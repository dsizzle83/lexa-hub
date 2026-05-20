//go:build integration

package tlsclient

import (
	"strings"
	"testing"
)

// === Happy path ===========================================================

// TestClient_HappyPath is the end-to-end smoke test from the client's
// perspective. It validates the full pipeline: TCP dial, TLS handshake,
// HTTP request, response parse, XML unmarshal. If this test passes,
// the client product is functional against a known-good server.
func TestClient_HappyPath(t *testing.T) {
	addr := startInProcessServer(t)

	client, err := New(goodClientConfig(addr))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer client.Free()

	if err := client.Dial(); err != nil {
		t.Fatalf("Dial: %v", err)
	}

	if got := client.Version(); got != "TLSv1.2" {
		t.Errorf("Version() = %q, want TLSv1.2", got)
	}
	if got := client.Cipher(); got != "ECDHE-ECDSA-AES128-CCM-8" {
		t.Errorf("Cipher() = %q, want ECDHE-ECDSA-AES128-CCM-8", got)
	}

	dcap, err := client.FetchDCAP()
	if err != nil {
		t.Fatalf("FetchDCAP: %v", err)
	}

	if dcap.Href != "/dcap" {
		t.Errorf("DCAP.Href = %q, want /dcap", dcap.Href)
	}
	if dcap.EndDeviceListLink == nil || dcap.EndDeviceListLink.Href != "/edev" {
		t.Errorf("EndDeviceListLink wrong: %+v", dcap.EndDeviceListLink)
	}
	if dcap.TimeLink == nil || dcap.TimeLink.Href != "/tm" {
		t.Errorf("TimeLink wrong: %+v", dcap.TimeLink)
	}
}

// TestClient_GenericGet exercises the lower-level Get method directly,
// useful when you want the raw bytes for debugging or want to test
// against an endpoint without a parser yet.
func TestClient_GenericGet(t *testing.T) {
	addr := startInProcessServer(t)

	client, err := New(goodClientConfig(addr))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer client.Free()

	if err := client.Dial(); err != nil {
		t.Fatalf("Dial: %v", err)
	}

	raw, err := client.Get("/dcap")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	for _, want := range []string{
		"HTTP/1.1 200 OK",
		"Content-Type: application/sep+xml",
		"<DeviceCapability",
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("response missing %q\nraw:\n%s", want, raw)
		}
	}
}

// TestClient_404 exercises the client's behavior when the server
// returns a 404. The current FetchDCAP doesn't try other paths, so
// we use Get directly here.
func TestClient_404(t *testing.T) {
	addr := startInProcessServer(t)

	client, err := New(goodClientConfig(addr))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer client.Free()

	if err := client.Dial(); err != nil {
		t.Fatalf("Dial: %v", err)
	}

	raw, err := client.Get("/this-resource-does-not-exist")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !strings.Contains(string(raw), "404 Not Found") {
		t.Errorf("expected 404 in response, got:\n%s", raw)
	}
}

// === Cipher conformance ===================================================

// TestClient_CipherIsCSIPCompliant is the client-side equivalent of
// the server's cipher conformance test. The client must negotiate
// exactly the CSIP-mandated cipher and nothing else. This is the
// single highest-value regression test for CSIP compliance from the
// product side.
func TestClient_CipherIsCSIPCompliant(t *testing.T) {
	addr := startInProcessServer(t)

	client, err := New(goodClientConfig(addr))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer client.Free()

	if err := client.Dial(); err != nil {
		t.Fatalf("Dial: %v", err)
	}

	if got := client.Cipher(); got != "ECDHE-ECDSA-AES128-CCM-8" {
		t.Errorf("client negotiated cipher = %q, want ECDHE-ECDSA-AES128-CCM-8 (CSIP §5.2.1.1)", got)
	}
	if got := client.Version(); got != "TLSv1.2" {
		t.Errorf("client negotiated version = %q, want TLSv1.2 (CSIP §5.2.1.1)", got)
	}
}

// === Negative cases =======================================================

// TestClient_RejectsServerWithWrongCA verifies that if the client is
// configured with a CA that does NOT sign the server's cert, the
// handshake fails. This proves the client is doing real cert
// verification, not just blindly trusting whatever the server presents.
func TestClient_RejectsServerWithWrongCA(t *testing.T) {
	addr := startInProcessServer(t)

	cfg := goodClientConfig(addr)
	cfg.CACertPath = testdataPath("certs/wrong-ca-cert.pem")

	client, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer client.Free()

	if err := client.Dial(); err == nil {
		t.Fatal("expected Dial to fail with wrong CA, but it succeeded")
	} else {
		t.Logf("(expected) Dial rejected: %v", err)
	}
}

// TestClient_RejectsWrongCipher verifies that if the client tries to
// negotiate a non-CSIP cipher, the handshake fails because the server
// only accepts CCM_8. This is the symmetric counterpart to the
// server-side "wrong cipher" test — both sides agree on enforcement.
func TestClient_RejectsWrongCipher(t *testing.T) {
	addr := startInProcessServer(t)

	cfg := goodClientConfig(addr)
	cfg.CipherList = "ECDHE-ECDSA-AES128-GCM-SHA256"

	client, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer client.Free()

	if err := client.Dial(); err == nil {
		t.Fatal("expected Dial to fail with non-CSIP cipher, but it succeeded")
	} else {
		t.Logf("(expected) Dial rejected: %v", err)
	}
}

// TestClient_DialReuseAfterClose verifies that the client lifecycle
// supports reconnection after Close. This matters because real DER
// devices need to handle server restarts and network blips by
// reconnecting without recreating the cert state from scratch.
func TestClient_DialReuseAfterClose(t *testing.T) {
	addr := startInProcessServer(t)

	client, err := New(goodClientConfig(addr))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer client.Free()

	// First dial
	if err := client.Dial(); err != nil {
		t.Fatalf("first Dial: %v", err)
	}
	if _, err := client.FetchDCAP(); err != nil {
		t.Fatalf("first FetchDCAP: %v", err)
	}
	client.Close()

	// Second dial — same client, same ctx, new connection
	if err := client.Dial(); err != nil {
		t.Fatalf("second Dial: %v", err)
	}
	if _, err := client.FetchDCAP(); err != nil {
		t.Fatalf("second FetchDCAP: %v", err)
	}
	client.Close()
}
