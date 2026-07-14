// Untagged (runs in the plain `go test ./...` suite): the PUT verb's
// CGo-free seams — the request builder, the injection guard it shares with
// every other verb, and putResult's status contract — mirror the coverage
// parsing_test.go gives GET/POST. The one fetcher-level test at the bottom
// reuses client_timeout_test.go's ephemeral-identity helper, so it too
// needs no CSIP server and no checked-in keys.
package tlsclient

import (
	"context"
	"errors"
	"strings"
	"testing"

	"lexa-hub/internal/wolfssl"
)

// === PUT request building ==================================================

func TestBuildPutRequest_Format(t *testing.T) {
	body := []byte("<DERCapability/>")
	req := string(buildPutRequest("/edev/1/dercap", "192.168.0.188:11111", body, "application/sep+xml"))

	for _, want := range []string{
		"PUT /edev/1/dercap HTTP/1.1\r\n",
		"Host: 192.168.0.188:11111\r\n",
		"Content-Type: application/sep+xml\r\n",
		"Content-Length: 16\r\n",
		"Connection: keep-alive\r\n",
		"\r\n\r\n", // header terminator
	} {
		if !strings.Contains(req, want) {
			t.Errorf("request missing %q\nfull request:\n%s", want, req)
		}
	}
	if !strings.HasSuffix(req, "<DERCapability/>") {
		t.Errorf("request does not end with the body:\n%s", req)
	}
}

// TestBuildPutRequest_MirrorsPost pins D3's "buildPutRequest mirrors
// buildPostRequest": byte-identical output except the verb, so the two can
// never drift on framing.
func TestBuildPutRequest_MirrorsPost(t *testing.T) {
	body := []byte("<DERSettings/>")
	put := string(buildPutRequest("/edev/1/derg", "host:11111", body, "application/sep+xml"))
	post := string(buildPostRequest("/edev/1/derg", "host:11111", body, "application/sep+xml"))

	if !strings.HasPrefix(put, "PUT ") {
		t.Fatalf("PUT request line wrong: %q", put)
	}
	if got, want := strings.TrimPrefix(put, "PUT"), strings.TrimPrefix(post, "POST"); got != want {
		t.Errorf("PUT and POST requests differ beyond the verb:\nPUT:  %q\nPOST: %q", put, post)
	}
}

func TestBuildPutRequest_EmptyBody(t *testing.T) {
	req := string(buildPutRequest("/edev/1/ders", "host:11111", nil, "application/sep+xml"))
	if !strings.Contains(req, "Content-Length: 0\r\n") {
		t.Errorf("empty-body PUT missing Content-Length: 0:\n%s", req)
	}
}

// === Injection guard (path/host) ===========================================

// TestValidateRequestParam_HostilePathAndHost pins the CRLF/space guard the
// Client verb methods (Get/Post/Put alike) apply to the request path — and
// documents that the same function rejects hostile host values, should a
// host ever come from anywhere but the local config. A CR, LF, or space in
// either value would let a hostile server (via a redirect Location) or a
// misconfigured deployment smuggle extra headers or a second request into
// the wire bytes.
func TestValidateRequestParam_HostilePathAndHost(t *testing.T) {
	hostile := []struct {
		name  string
		value string
	}{
		{"CRLF header injection", "/edev/1\r\nX-Evil: 1"},
		{"bare CR", "/edev/1\r"},
		{"bare LF", "/edev/1\n"},
		{"space request smuggling", "/edev/1 HTTP/1.1"},
		{"host with CRLF", "host:11111\r\nX-Evil: 1"},
		{"host with space", "host:11111 evil"},
	}
	for _, tc := range hostile {
		t.Run(tc.name, func(t *testing.T) {
			for _, param := range []string{"path", "host"} {
				if err := validateRequestParam(tc.value, param); err == nil {
					t.Errorf("validateRequestParam(%q, %q) = nil, want error", tc.value, param)
				}
			}
		})
	}

	for _, clean := range []string{"/edev/1/dercap", "/derp/0/derc?l=10", "192.168.0.188:11111"} {
		if err := validateRequestParam(clean, "path"); err != nil {
			t.Errorf("validateRequestParam(%q) = %v, want nil", clean, err)
		}
	}
}

// === putResult: the status contract ========================================

func TestPutResult_Statuses(t *testing.T) {
	// D3: success is 200/201/204, no Location dependency.
	for _, status := range []int{200, 201, 204} {
		body, err := putResult("/edev/1/dercap", &HTTPResponse{StatusCode: status, Body: []byte("ok")})
		if err != nil {
			t.Errorf("status %d: putResult error = %v, want nil", status, err)
		}
		if string(body) != "ok" {
			t.Errorf("status %d: body = %q, want %q", status, body, "ok")
		}
	}

	for _, status := range []int{301, 302, 400, 403, 404, 405, 500} {
		_, err := putResult("/edev/1/dercap", &HTTPResponse{StatusCode: status})
		if err == nil {
			t.Fatalf("status %d: putResult error = nil, want error", status)
		}
		if !strings.Contains(err.Error(), "PUT /edev/1/dercap") {
			t.Errorf("status %d: error = %v, want it to name the PUT and path", status, err)
		}
	}
}

// === Fetcher-level: ctx preflight ==========================================

// TestWolfSSLFetcher_PutContext_CtxPreflight_NoDial mirrors the Get/
// PostContext preflight tests (fetcher_test.go, integration-tagged) for the
// new PutContext, but untagged: like client_timeout_test.go it needs only an
// ephemeral identity, and the "never dials" proof is a config pointing at a
// closed port. Same contract — a canceled ctx is honored after the mutex,
// before any dial or write.
func TestWolfSSLFetcher_PutContext_CtxPreflight_NoDial(t *testing.T) {
	wolfInitOnce.Do(wolfssl.Init)

	certPath, keyPath := ephemeralIdentity(t, t.TempDir())
	fetcher, err := NewWolfSSLFetcher(Config{
		ServerAddr:     "127.0.0.1:1",
		CACertPath:     certPath,
		ClientCertPath: certPath,
		ClientKeyPath:  keyPath,
	})
	if err != nil {
		t.Fatalf("NewWolfSSLFetcher: %v", err)
	}
	defer fetcher.Free()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already Done before PutContext is ever called

	_, err = fetcher.PutContext(ctx, "/edev/1/dercap", []byte("<x/>"), "application/sep+xml")
	if err == nil {
		t.Fatal("PutContext with an already-canceled ctx returned nil error, want context.Canceled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("PutContext error = %v, want errors.Is(err, context.Canceled)", err)
	}
	if fetcher.client.ssl != nil {
		t.Error("PutContext dialed (ssl session non-nil) despite an already-canceled ctx")
	}
}
