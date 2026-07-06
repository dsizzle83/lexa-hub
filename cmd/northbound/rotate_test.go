package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lexa-hub/internal/metrics"
	"lexa-hub/internal/tlsclient"
)

// counterValue extracts a counter's exposed value from the registry's
// Prometheus text-exposition Format() output — internal/metrics.Counter's
// underlying value() accessor is unexported and internal to that package
// (see metrics_test.go), so this package (an external caller, like a real
// scrape target) reads it the same way Prometheus itself would.
func counterValue(t *testing.T, reg *metrics.Registry, name string) string {
	t.Helper()
	for _, line := range strings.Split(reg.Format(), "\n") {
		if strings.HasPrefix(line, name+" ") {
			return strings.TrimPrefix(line, name+" ")
		}
	}
	return "<absent>"
}

// fakeReloader is a scripted double for reloader, recording every Reload
// call's cfg so tests can assert exactly which fetchers were rotated, in
// what order, with what cert paths — without a live wolfSSL session (the
// real Reload mechanics are covered by internal/tlsclient's reload_test.go
// / reload_integration_test.go).
type fakeReloader struct {
	mu    sync.Mutex
	err   error // returned by every Reload call, if non-nil
	calls []tlsclient.Config
}

func (f *fakeReloader) Reload(cfg tlsclient.Config, probePath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, cfg)
	return f.err
}

func (f *fakeReloader) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// newRotateTestCert writes a fresh self-signed test cert (via
// writeTestCertFile, shared with certmon_test.go) and returns both its path
// and derived LFDI, so tests can set up "own" vs "staged" identities without
// duplicating cert-generation code.
func newRotateTestCert(t *testing.T) (path, lfdi string) {
	t.Helper()
	path = writeTestCertFile(t, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
	l, err := lfdiFromCert(path)
	if err != nil {
		t.Fatalf("lfdiFromCert(%s): %v", path, err)
	}
	return path, l
}

// writeSentinel JSON-encodes req to path.
func writeSentinel(t *testing.T, path string, req rotateRequest) {
	t.Helper()
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal sentinel: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
}

// globExactlyOne returns the single path matching pattern, failing the test
// if there isn't exactly one — used to find the renamed (consumed) sentinel
// without hardcoding its timestamp suffix.
func globExactlyOne(t *testing.T, pattern string) string {
	t.Helper()
	matches, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatalf("glob %s: %v", pattern, err)
	}
	if len(matches) != 1 {
		t.Fatalf("glob %s: want exactly 1 match, got %v", pattern, matches)
	}
	return matches[0]
}

func newTestRotationController(t *testing.T, ownLFDI string, disc, resp, fr *fakeReloader, onCommit func(), reg *metrics.Registry) (*RotationController, string) {
	t.Helper()
	sentinelPath := filepath.Join(t.TempDir(), "rotate.request")
	rc := NewRotationController(sentinelPath, tlsclient.Config{ServerAddr: "example.invalid:443"}, ownLFDI, disc, resp, fr, onCommit, reg)
	return rc, sentinelPath
}

// TestRotationController_CheckOnce_NoSentinel_NoOp verifies the steady
// state — no file present — touches nothing: no Reload calls, no commit.
func TestRotationController_CheckOnce_NoSentinel_NoOp(t *testing.T) {
	disc, resp, fr := &fakeReloader{}, &fakeReloader{}, &fakeReloader{}
	committed := false
	rc, _ := newTestRotationController(t, "AAAA", disc, resp, fr, func() { committed = true }, nil)

	rc.checkOnce()

	if disc.callCount() != 0 || resp.callCount() != 0 || fr.callCount() != 0 {
		t.Fatalf("expected no Reload calls with no sentinel present, got disc=%d resp=%d fr=%d",
			disc.callCount(), resp.callCount(), fr.callCount())
	}
	if committed {
		t.Error("onCommit must not fire with no sentinel present")
	}
}

// TestRotationController_CheckOnce_Success_RotatesAllThreeAndCommits is the
// happy path: a sentinel whose staged cert's LFDI matches ownLFDI rotates
// all three fetchers with the SAME new cert/key paths, fires onCommit
// exactly once, and consumes the sentinel (renamed with a "done" suffix so
// a later poll cannot reprocess it).
func TestRotationController_CheckOnce_Success_RotatesAllThreeAndCommits(t *testing.T) {
	stagedCertPath, ownLFDI := newRotateTestCert(t)
	stagedKeyPath := filepath.Join(t.TempDir(), "irrelevant-key.pem") // Reload is faked; key content unused

	disc, resp, fr := &fakeReloader{}, &fakeReloader{}, &fakeReloader{}
	commits := 0
	reg := metrics.New()
	rc, sentinelPath := newTestRotationController(t, ownLFDI, disc, resp, fr, func() { commits++ }, reg)

	writeSentinel(t, sentinelPath, rotateRequest{ClientCert: stagedCertPath, ClientKey: stagedKeyPath})

	rc.checkOnce()

	for name, f := range map[string]*fakeReloader{"discovery": disc, "response": resp, "flow-reservation": fr} {
		if f.callCount() != 1 {
			t.Errorf("%s: Reload called %d times, want 1", name, f.callCount())
			continue
		}
		got := f.calls[0]
		if got.ClientCertPath != stagedCertPath || got.ClientKeyPath != stagedKeyPath {
			t.Errorf("%s: Reload cfg = %+v, want ClientCertPath=%s ClientKeyPath=%s", name, got, stagedCertPath, stagedKeyPath)
		}
		if got.ServerAddr != "example.invalid:443" {
			t.Errorf("%s: Reload cfg.ServerAddr = %q, want the base cfg's ServerAddr preserved", name, got.ServerAddr)
		}
	}
	if commits != 1 {
		t.Errorf("onCommit fired %d times, want exactly 1", commits)
	}
	if _, err := os.Stat(sentinelPath); !os.IsNotExist(err) {
		t.Errorf("sentinel still present at original path after a successful rotation: %v", err)
	}
	_ = globExactlyOne(t, sentinelPath+".done-*")

	// A second poll with the sentinel already consumed must be a pure no-op
	// — the request is not reprocessed.
	rc.checkOnce()
	if disc.callCount() != 1 {
		t.Errorf("second checkOnce reprocessed the consumed sentinel: discovery Reload called %d times, want 1", disc.callCount())
	}
}

// TestRotationController_CheckOnce_LFDIMismatch_Refuses is the acceptance
// criterion "LFDI-mismatch refusal tested": a staged cert whose derived
// LFDI differs from ownLFDI must be refused WITHOUT attempting any Reload
// — a new device identity is re-enrollment, not rotation (task's "common
// mistakes to avoid").
func TestRotationController_CheckOnce_LFDIMismatch_Refuses(t *testing.T) {
	ownCertPath, ownLFDI := newRotateTestCert(t)
	_ = ownCertPath
	mismatchedCertPath, mismatchedLFDI := newRotateTestCert(t)
	if mismatchedLFDI == ownLFDI {
		t.Fatal("test setup: two independently generated certs produced the same LFDI (serial collision?)")
	}

	disc, resp, fr := &fakeReloader{}, &fakeReloader{}, &fakeReloader{}
	commits := 0
	reg := metrics.New()
	rc, sentinelPath := newTestRotationController(t, ownLFDI, disc, resp, fr, func() { commits++ }, reg)

	writeSentinel(t, sentinelPath, rotateRequest{ClientCert: mismatchedCertPath, ClientKey: "unused-key.pem"})

	rc.checkOnce()

	if disc.callCount() != 0 || resp.callCount() != 0 || fr.callCount() != 0 {
		t.Fatalf("LFDI mismatch must refuse before any Reload: disc=%d resp=%d fr=%d",
			disc.callCount(), resp.callCount(), fr.callCount())
	}
	if commits != 0 {
		t.Error("onCommit must not fire on a refused rotation")
	}
	if got := counterValue(t, reg, "lexa_nb_cert_rotation_refusals_total"); got != "1" {
		t.Errorf("refusals counter = %s, want 1", got)
	}
	_ = globExactlyOne(t, sentinelPath+".rejected-*")
}

// TestRotationController_CheckOnce_MalformedJSON_Refuses verifies a
// sentinel that isn't valid JSON is refused the same way as an LFDI
// mismatch — rejected, no Reload attempted, nothing left to reprocess.
func TestRotationController_CheckOnce_MalformedJSON_Refuses(t *testing.T) {
	_, ownLFDI := newRotateTestCert(t)
	disc, resp, fr := &fakeReloader{}, &fakeReloader{}, &fakeReloader{}
	rc, sentinelPath := newTestRotationController(t, ownLFDI, disc, resp, fr, nil, nil)

	if err := os.WriteFile(sentinelPath, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	rc.checkOnce()

	if disc.callCount() != 0 {
		t.Fatalf("malformed sentinel must refuse before any Reload, got %d calls", disc.callCount())
	}
	_ = globExactlyOne(t, sentinelPath+".rejected-*")
}

// TestRotationController_CheckOnce_MissingFields_Refuses verifies a
// syntactically valid sentinel missing client_cert/client_key is refused.
func TestRotationController_CheckOnce_MissingFields_Refuses(t *testing.T) {
	_, ownLFDI := newRotateTestCert(t)
	disc, resp, fr := &fakeReloader{}, &fakeReloader{}, &fakeReloader{}
	rc, sentinelPath := newTestRotationController(t, ownLFDI, disc, resp, fr, nil, nil)

	writeSentinel(t, sentinelPath, rotateRequest{})

	rc.checkOnce()

	if disc.callCount() != 0 {
		t.Fatalf("sentinel missing client_cert/client_key must refuse before any Reload, got %d calls", disc.callCount())
	}
	_ = globExactlyOne(t, sentinelPath+".rejected-*")
}

// TestRotationController_CheckOnce_StagedCertUnreadable_Refuses verifies a
// sentinel pointing at a nonexistent staged cert file is refused (fails to
// derive an LFDI at all) rather than panicking or silently proceeding.
func TestRotationController_CheckOnce_StagedCertUnreadable_Refuses(t *testing.T) {
	_, ownLFDI := newRotateTestCert(t)
	disc, resp, fr := &fakeReloader{}, &fakeReloader{}, &fakeReloader{}
	rc, sentinelPath := newTestRotationController(t, ownLFDI, disc, resp, fr, nil, nil)

	writeSentinel(t, sentinelPath, rotateRequest{ClientCert: "/nonexistent/staged-cert.pem", ClientKey: "/nonexistent/staged-key.pem"})

	rc.checkOnce()

	if disc.callCount() != 0 {
		t.Fatalf("unreadable staged cert must refuse before any Reload, got %d calls", disc.callCount())
	}
	_ = globExactlyOne(t, sentinelPath+".rejected-*")
}

// TestRotationController_CheckOnce_PartialFailure_MarksFailedNotDone
// verifies the code-review-checklist requirement "probe failure leaves old
// path fully functional", from the orchestration side: when one fetcher's
// Reload fails, checkOnce still attempts the OTHER two (each swaps at its
// own safe point — task's "common mistakes"), never calls onCommit (the
// rotation is not fully committed), and marks the sentinel "failed" rather
// than "done" so the operator script/runbook can tell a partial rotation
// from a clean one.
func TestRotationController_CheckOnce_PartialFailure_MarksFailedNotDone(t *testing.T) {
	stagedCertPath, ownLFDI := newRotateTestCert(t)

	disc, resp, fr := &fakeReloader{}, &fakeReloader{err: errFakeReloadFailure}, &fakeReloader{}
	commits := 0
	reg := metrics.New()
	rc, sentinelPath := newTestRotationController(t, ownLFDI, disc, resp, fr, func() { commits++ }, reg)

	writeSentinel(t, sentinelPath, rotateRequest{ClientCert: stagedCertPath, ClientKey: "unused-key.pem"})

	rc.checkOnce()

	if disc.callCount() != 1 {
		t.Errorf("discovery: Reload called %d times, want 1 (must still be attempted)", disc.callCount())
	}
	if resp.callCount() != 1 {
		t.Errorf("response: Reload called %d times, want 1", resp.callCount())
	}
	if fr.callCount() != 1 {
		t.Errorf("flow-reservation: Reload called %d times, want 1 (must still be attempted after response's failure)", fr.callCount())
	}
	if commits != 0 {
		t.Error("onCommit must not fire on a partial rotation")
	}
	if got := counterValue(t, reg, "lexa_nb_cert_rotations_total"); got != "2" {
		t.Errorf("rotations counter = %s, want 2 (discovery + flow-reservation committed)", got)
	}
	if got := counterValue(t, reg, "lexa_nb_cert_rotation_failures_total"); got != "1" {
		t.Errorf("failures counter = %s, want 1 (response)", got)
	}
	_ = globExactlyOne(t, sentinelPath+".failed-*")
}

// errFakeReloadFailure is a sentinel error fakeReloader returns to simulate
// a probe-then-commit failure (e.g. the new session's probe 403'd or timed
// out) without needing a real wolfSSL session.
var errFakeReloadFailure = &fakeReloadFailure{}

type fakeReloadFailure struct{}

func (*fakeReloadFailure) Error() string { return "fake: reload probe failed" }
