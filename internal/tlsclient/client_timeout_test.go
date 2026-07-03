package tlsclient

// Untagged (runs in the plain `go test ./...` suite, unlike the integration-
// tagged files): it needs no CSIP server and no checked-in keys — it generates
// an ephemeral self-signed ECDSA cert and talks to a silent TCP listener.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"lexa-hub/internal/wolfssl"
)

// wolfSSL_Init is process-global C state — exactly once per process.
var wolfInitOnce sync.Once

// ephemeralIdentity writes a self-signed ECDSA P-256 cert + key to dir and
// returns their paths. Good enough for wolfSSL to load an identity; no
// handshake ever completes in these tests.
func ephemeralIdentity(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "tlsclient-timeout-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certPath, keyPath
}

// A server that accepts TCP and then never speaks (wedged head-end, black-
// holing middlebox) must fail the Dial within the configured ReadTimeout —
// wolfSSL's blocking read on the handshake would otherwise hang the caller's
// goroutine forever, which in the northbound stalls every future discovery
// walk (QA 2026-07-02: northbound-hang).
func TestClient_ReadTimeoutBoundsStalledServer(t *testing.T) {
	wolfInitOnce.Do(wolfssl.Init)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer lis.Close()
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			defer conn.Close() // accept, read nothing, say nothing
		}
	}()

	certPath, keyPath := ephemeralIdentity(t, t.TempDir())
	cfg := Config{
		ServerAddr:     lis.Addr().String(),
		CACertPath:     certPath,
		ClientCertPath: certPath,
		ClientKeyPath:  keyPath,
		ReadTimeout:    1 * time.Second,
	}

	client, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer client.Free()

	start := time.Now()
	err = client.Dial()
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Dial against a silent server should fail")
	}
	if elapsed > 5*time.Second {
		t.Errorf("Dial took %v against a silent server — ReadTimeout (1s) did not bound the handshake read", elapsed)
	}
	t.Logf("Dial failed as expected after %v: %v", elapsed, err)
}
