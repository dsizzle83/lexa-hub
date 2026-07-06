// Untagged (runs in the plain `go test ./...` suite, like
// client_timeout_test.go) — planReload's ordering is exercised here
// against a fake reloadable, so the RSK-07-critical sequencing (dial →
// probe → {swap-eligible | torn-down}) has fast, cgo-free coverage. The
// real wolfSSL Free-ordering (Close → FreeSSL → FreeCtx against a live
// in-process server) is proven by the `integration`-tagged tests in
// reload_integration_test.go (desktop amd64 sysroot, `make test-integration`
// equivalent: `go test -tags=integration`).
package tlsclient

import (
	"errors"
	"strings"
	"testing"
)

// fakeReloadable is a scripted double for reloadable, recording call order
// so tests can assert planReload never frees a session it just handed back
// as "the new active one", and always frees a session it is abandoning.
type fakeReloadable struct {
	dialErr error
	getResp []byte // raw HTTP response bytes returned by Get, when getErr is nil
	getErr  error

	calls []string
}

func (f *fakeReloadable) Dial() error {
	f.calls = append(f.calls, "dial")
	return f.dialErr
}

func (f *fakeReloadable) Get(path string) ([]byte, error) {
	f.calls = append(f.calls, "get:"+path)
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.getResp, nil
}

func (f *fakeReloadable) Free() {
	f.calls = append(f.calls, "free")
}

const rawOK200 = "HTTP/1.1 200 OK\r\nContent-Type: application/sep+xml\r\nContent-Length: 2\r\n\r\nOK"

func raw403() []byte {
	return []byte("HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
}

// TestPlanReload_Success verifies the happy path: dial, then probe, in that
// order, and — critically — Free is NEVER called on a session that
// succeeded. The caller (Reload) owns freeing it later, only after the old
// session has been retired; planReload calling Free on its own success
// would be exactly the kind of misordered free RSK-07 is about.
func TestPlanReload_Success(t *testing.T) {
	fc := &fakeReloadable{getResp: []byte(rawOK200)}

	if err := planReload(fc, "/dcap"); err != nil {
		t.Fatalf("planReload: %v", err)
	}

	wantOrder := []string{"dial", "get:/dcap"}
	if len(fc.calls) != len(wantOrder) {
		t.Fatalf("calls = %v, want exactly %v (no Free on success)", fc.calls, wantOrder)
	}
	for i, want := range wantOrder {
		if fc.calls[i] != want {
			t.Errorf("calls[%d] = %q, want %q", i, fc.calls[i], want)
		}
	}
}

// TestPlanReload_DialFailure verifies that a dial error on the new session
// (e.g. the new cert's CA chain doesn't match the server, or the network
// path is down) never calls Get, and tears the new session down with
// exactly one Free — nothing is left dangling for a future misordered free.
func TestPlanReload_DialFailure(t *testing.T) {
	fc := &fakeReloadable{dialErr: errors.New("handshake failed")}

	err := planReload(fc, "/dcap")
	if err == nil {
		t.Fatal("planReload: expected error on dial failure, got nil")
	}
	if !strings.Contains(err.Error(), "dial new session") {
		t.Errorf("error = %v, want it to mention dial failure", err)
	}

	wantOrder := []string{"dial", "free"}
	if len(fc.calls) != len(wantOrder) {
		t.Fatalf("calls = %v, want exactly %v (no Get after a dial failure)", fc.calls, wantOrder)
	}
	for i, want := range wantOrder {
		if fc.calls[i] != want {
			t.Errorf("calls[%d] = %q, want %q", i, fc.calls[i], want)
		}
	}
}

// TestPlanReload_ProbeTransportFailure verifies a transport-level probe
// error (e.g. the new session's socket died between Dial and Get — a
// wedged/dropped server) also tears the new session down with exactly one
// Free, dial having already succeeded.
func TestPlanReload_ProbeTransportFailure(t *testing.T) {
	fc := &fakeReloadable{getErr: errors.New("connection reset")}

	err := planReload(fc, "/dcap")
	if err == nil {
		t.Fatal("planReload: expected error on probe transport failure, got nil")
	}
	if !strings.Contains(err.Error(), "probe /dcap failed") {
		t.Errorf("error = %v, want it to mention the probe failure", err)
	}

	wantOrder := []string{"dial", "get:/dcap", "free"}
	if len(fc.calls) != len(wantOrder) {
		t.Fatalf("calls = %v, want exactly %v", fc.calls, wantOrder)
	}
}

// TestPlanReload_ProbeRejectedStatus is the case the Background section of
// TASK-073 calls out explicitly: a cert signed by the right CA but for the
// wrong/unregistered device still completes the TLS handshake (Dial
// succeeds) and only fails at the CSIP application layer (HTTP status,
// e.g. 403). A probe that only checked "Dial succeeded" or "Get returned no
// transport error" would miss this and wrongly commit the rotation onto a
// session the server doesn't actually accept for this identity. planReload
// must parse the response and require status 200, and must still Free the
// rejected session exactly once.
func TestPlanReload_ProbeRejectedStatus(t *testing.T) {
	fc := &fakeReloadable{getResp: raw403()}

	err := planReload(fc, "/dcap")
	if err == nil {
		t.Fatal("planReload: expected error on a non-200 probe response, got nil")
	}
	if !strings.Contains(err.Error(), "status 403") {
		t.Errorf("error = %v, want it to mention status 403", err)
	}

	wantOrder := []string{"dial", "get:/dcap", "free"}
	if len(fc.calls) != len(wantOrder) {
		t.Fatalf("calls = %v, want exactly %v", fc.calls, wantOrder)
	}
}

// TestPlanReload_ProbeMalformedResponse verifies a response that fails to
// parse as HTTP at all (no header/body separator) is treated the same as
// any other probe failure: torn down, old untouched, one Free.
func TestPlanReload_ProbeMalformedResponse(t *testing.T) {
	fc := &fakeReloadable{getResp: []byte("not an http response")}

	err := planReload(fc, "/dcap")
	if err == nil {
		t.Fatal("planReload: expected error on malformed probe response, got nil")
	}

	wantOrder := []string{"dial", "get:/dcap", "free"}
	if len(fc.calls) != len(wantOrder) {
		t.Fatalf("calls = %v, want exactly %v", fc.calls, wantOrder)
	}
}
