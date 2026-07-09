package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newTestClient builds a *client pointed at server with a plain (non-TLS)
// transport — used by every subcommand test that doesn't specifically
// exercise the TLS/fingerprint trust path (trust_test.go covers that
// separately). server.URL is already "http://127.0.0.1:port".
func newTestClient(t *testing.T, server *httptest.Server, token string) *client {
	t.Helper()
	return newClient(server.URL, token, "", nil)
}

// writeCertPEM writes der as a PEM-encoded CERTIFICATE block to a temp file
// under t.TempDir() and returns its path.
func writeCertPEM(t *testing.T, der []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cert.pem")
	data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write cert fixture: %v", err)
	}
	return path
}

// fingerprintOfDER returns the lowercase-hex sha256 of der — the same
// computation fingerprintFromCertFile and cmd/api/tlscert.go's
// fingerprintOf perform, used here to compute an EXPECTED value
// independently in tests.
func fingerprintOfDER(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// genSelfSignedCert mints a fresh, distinct self-signed ECDSA P-256 leaf for
// cn, valid for loopback serving (SAN 127.0.0.1). Used where a test needs
// TWO genuinely different server certificates — httptest.NewTLSServer alone
// isn't enough for that: every instance in a process defaults to the SAME
// hardcoded net/http/internal test certificate, so two httptest.NewTLSServer
// servers are NOT a valid "different cert" fixture on their own.
func genSelfSignedCert(t *testing.T, cn string) tls.Certificate {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		t.Fatalf("generate serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
}

// newTLSServerWithCert starts an httptest server presenting cert (rather
// than the shared default test cert every bare httptest.NewTLSServer uses).
func newTLSServerWithCert(t *testing.T, cert tls.Certificate) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	return srv
}
