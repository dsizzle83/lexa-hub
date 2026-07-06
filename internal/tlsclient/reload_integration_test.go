//go:build integration

package tlsclient

// reload_integration_test.go proves TASK-073/RSK-07's real invariant against
// a LIVE wolfSSL client+server pair (not a fake): that WolfSSLFetcher.Reload
// never corrupts or crashes the process across a probe-success commit and a
// probe-rejected abort, including back-to-back repeats (a miniature version
// of the churn soak this task defers to a 24h bench run — see
// docs/CERT_ROTATION_RUNBOOK.md in this repo and csip-tls-test's
// docs/CERT_ROTATION_SOAK_RUNBOOK.md).
//
// This file is deliberately self-contained: it builds its own minimal
// wolfSSL test server directly on internal/wolfssl (the same package the
// product uses — RequireClientCert, LoadVerifyLocations, etc.) and
// generates its own CA-signed leaf certs at test time via crypto/x509,
// rather than reusing this package's existing startInProcessServer /
// goodClientConfig helpers referenced by client_test.go and fetcher_test.go.
// Those helpers are not defined anywhere in this tree — `go vet
// -tags=integration ./internal/tlsclient/...` fails with "undefined:
// startInProcessServer" — and this package's checked-in testdata/certs/
// contains no private keys (*-key.pem is gitignored repo-wide; see
// .gitignore) for any of the five checked-in cert files, so client_test.go
// and fetcher_test.go cannot currently build or run in a fresh checkout
// either. That gap predates TASK-073 (git blame: both files are unchanged
// since the initial commit) and is out of this task's scope to repair —
// flagged for the reviewer in TASK-073's final report rather than silently
// patched. This file does not depend on any of it.
import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"lexa-hub/internal/wolfssl"
)

// --- test PKI: a CA plus CA-signed leaf certs ------------------------------

type testCA struct {
	certPath string
	cert     *x509.Certificate
	key      *ecdsa.PrivateKey
}

func genTestCA(t *testing.T, dir string) testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "reload-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	certPath := filepath.Join(dir, "ca-cert.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write CA cert: %v", err)
	}
	return testCA{certPath: certPath, cert: cert, key: key}
}

// genLeaf issues a CA-signed leaf cert for cn (a server or client identity)
// and writes its cert+key PEMs to dir/<name>-{cert,key}.pem, returning the
// paths. ECDSA P-256, matching CSIP §5.2.1.1's ECDHE-ECDSA cipher family.
func genLeaf(t *testing.T, dir, name, cn string, ca testCA) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate %s key: %v", name, err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	if err != nil {
		t.Fatalf("generate %s serial: %v", name, err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"127.0.0.1"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("create %s cert: %v", name, err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal %s key: %v", name, err)
	}
	certPath = filepath.Join(dir, name+"-cert.pem")
	keyPath = filepath.Join(dir, name+"-key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write %s cert: %v", name, err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write %s key: %v", name, err)
	}
	return certPath, keyPath
}

// --- minimal mTLS test server -----------------------------------------------

// startReloadTestServer accepts mTLS connections, reads a single HTTP
// request per connection (ignored beyond framing), and responds 200 with a
// DeviceCapability body if the peer cert's CommonName matches allowedCN, or
// 403 otherwise — modeling the CSIP-layer identity check a real utility
// server performs (TASK-073 Background: a cert signed by the right CA but
// for the wrong/unregistered device passes the TLS handshake and only fails
// at the HTTP layer). Every connection closes after one response, so each
// WolfSSLFetcher.Get performs a fresh dial+request — sufficient for
// exercising Reload's dial+probe+swap+free sequence without needing
// keep-alive semantics from this test double.
func startReloadTestServer(t *testing.T, caCertPath, serverCertPath, serverKeyPath, allowedCN string) string {
	t.Helper()
	wolfInitOnce.Do(wolfssl.Init)

	ctx, err := wolfssl.NewServerCtx()
	if err != nil {
		t.Fatalf("NewServerCtx: %v", err)
	}
	if err := wolfssl.SetCipherList(ctx, DefaultCipherList); err != nil {
		t.Fatalf("SetCipherList: %v", err)
	}
	if err := wolfssl.LoadVerifyLocations(ctx, caCertPath); err != nil {
		t.Fatalf("LoadVerifyLocations: %v", err)
	}
	if err := wolfssl.UseCertFile(ctx, serverCertPath); err != nil {
		t.Fatalf("UseCertFile: %v", err)
	}
	if err := wolfssl.UseKeyFile(ctx, serverKeyPath); err != nil {
		t.Fatalf("UseKeyFile: %v", err)
	}
	wolfssl.RequireClientCert(ctx)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return // listener closed at test cleanup
			}
			go serveOneReloadTestConn(conn, ctx, allowedCN)
		}
	}()
	t.Cleanup(func() {
		lis.Close()
		wolfssl.FreeCtx(ctx)
	})
	return lis.Addr().String()
}

// serveOneReloadTestConn performs the server-side handshake on conn, checks
// the peer cert's CommonName against allowedCN, and writes exactly one
// HTTP response before closing — see startReloadTestServer's doc comment.
func serveOneReloadTestConn(conn net.Conn, ctx unsafe.Pointer, allowedCN string) {
	defer conn.Close()
	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		return
	}
	file, err := tcpConn.File()
	if err != nil {
		return
	}
	defer file.Close()

	ssl, err := wolfssl.NewSSL(ctx)
	if err != nil {
		return
	}
	defer wolfssl.FreeSSL(ssl)

	if err := wolfssl.SetFD(ssl, int(file.Fd())); err != nil {
		return
	}
	if err := wolfssl.Accept(ssl); err != nil {
		return // handshake rejected (e.g. untrusted CA) — nothing more to do
	}
	defer wolfssl.Shutdown(ssl)

	// Read the request off the wire (ignored beyond framing: this test
	// double only needs to know a request arrived before it replies).
	buf := make([]byte, 4096)
	total := 0
	for total < len(buf) {
		n, err := wolfssl.Read(ssl, buf[total:])
		total += n
		if err != nil || (total >= 4 && bytes.Contains(buf[:total], []byte("\r\n\r\n"))) {
			break
		}
		if n == 0 {
			break
		}
	}

	cn := ""
	if der := wolfssl.PeerCertificateDER(ssl); der != nil {
		if peerCert, err := x509.ParseCertificate(der); err == nil {
			cn = peerCert.Subject.CommonName
		}
	}

	var resp string
	if cn == allowedCN {
		body := `<?xml version="1.0"?><DeviceCapability xmlns="urn:ieee:std:2030.5:ns" href="/dcap"/>`
		resp = fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: application/sep+xml\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
			len(body), body)
	} else {
		resp = "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
	}
	_, _ = wolfssl.Write(ssl, []byte(resp))
}

// --- test config helper ------------------------------------------------

func reloadTestConfig(addr, caCertPath, clientCertPath, clientKeyPath string) Config {
	return Config{
		ServerAddr:     addr,
		CACertPath:     caCertPath,
		ClientCertPath: clientCertPath,
		ClientKeyPath:  clientKeyPath,
		DialTimeout:    5 * time.Second,
		ReadTimeout:    5 * time.Second,
	}
}

// --- tests -------------------------------------------------------------

// TestWolfSSLFetcher_Reload_Integration_CommitsAndFreesOld exercises the
// real, cgo-backed teardown/rebuild sequence across several back-to-back
// reloads — a miniature version of the reconnect-churn soak (TASK-073
// §8.6/RSK-07 deferred to a 24h bench run; see the runbooks). Each round
// reissues a NEW leaf cert for the SAME CN ("device-a"), mirroring the
// bench single-rotation drill's "same device, reissued cert" case (task
// step 5: `make gen-client-cert CN=<current>`). If Reload's Free ordering
// were wrong (e.g. FreeCtx before the last Get on the old session
// completed, or freeing the session still installed on f.client), this
// test would crash the process (segfault) or hang — a plain assertion
// failure cannot express "the process is still alive", so surviving to the
// final t.Logf is itself part of what this test proves.
func TestWolfSSLFetcher_Reload_Integration_CommitsAndFreesOld(t *testing.T) {
	dir := t.TempDir()
	ca := genTestCA(t, dir)

	const cn = "device-a"
	serverCertPath, serverKeyPath := genLeaf(t, dir, "server", "127.0.0.1", ca)
	addr := startReloadTestServer(t, ca.certPath, serverCertPath, serverKeyPath, cn)

	certPath0, keyPath0 := genLeaf(t, dir, "client-0", cn, ca)
	fetcher, err := NewWolfSSLFetcher(reloadTestConfig(addr, ca.certPath, certPath0, keyPath0))
	if err != nil {
		t.Fatalf("NewWolfSSLFetcher: %v", err)
	}
	defer fetcher.Free()

	if _, err := fetcher.Get("/dcap"); err != nil {
		t.Fatalf("baseline Get: %v", err)
	}

	const rounds = 5
	for i := 0; i < rounds; i++ {
		certPath, keyPath := genLeaf(t, dir, fmt.Sprintf("client-%d", i+1), cn, ca)
		newCfg := reloadTestConfig(addr, ca.certPath, certPath, keyPath)
		if err := fetcher.Reload(newCfg, "/dcap"); err != nil {
			t.Fatalf("round %d: Reload: %v", i, err)
		}
		// The reloaded session must be immediately usable — proving the
		// swap landed (f.client now points at the new, already-probed
		// session) and the old one's teardown didn't disturb it.
		if _, err := fetcher.Get("/dcap"); err != nil {
			t.Fatalf("round %d: Get after Reload: %v", i, err)
		}
	}
	t.Logf("completed %d reload rounds with no crash, no hang", rounds)
}

// TestWolfSSLFetcher_Reload_Integration_ProbeRejectedKeepsOldFunctional
// covers the RSK-07 "probe failure leaves old path fully functional"
// requirement (acceptance criteria; code review checklist) against a real
// wolfSSL session: the candidate cert is signed by the SAME trusted CA
// (the TLS handshake succeeds) but names a device the server does not
// recognize (403 at the HTTP/CSIP layer) — the exact case the task's
// Background section warns a TLS-handshake-only probe would miss. Reload
// must return an error, must not touch f's live session, and a subsequent
// Get on the original fetcher must still succeed.
func TestWolfSSLFetcher_Reload_Integration_ProbeRejectedKeepsOldFunctional(t *testing.T) {
	dir := t.TempDir()
	ca := genTestCA(t, dir)

	const goodCN = "device-a"
	serverCertPath, serverKeyPath := genLeaf(t, dir, "server", "127.0.0.1", ca)
	addr := startReloadTestServer(t, ca.certPath, serverCertPath, serverKeyPath, goodCN)

	goodCertPath, goodKeyPath := genLeaf(t, dir, "client-good", goodCN, ca)
	fetcher, err := NewWolfSSLFetcher(reloadTestConfig(addr, ca.certPath, goodCertPath, goodKeyPath))
	if err != nil {
		t.Fatalf("NewWolfSSLFetcher: %v", err)
	}
	defer fetcher.Free()

	if _, err := fetcher.Get("/dcap"); err != nil {
		t.Fatalf("baseline Get: %v", err)
	}

	// Same CA, wrong device — TLS handshake succeeds, CSIP layer 403s.
	wrongCertPath, wrongKeyPath := genLeaf(t, dir, "client-wrong-device", "device-x", ca)
	err = fetcher.Reload(reloadTestConfig(addr, ca.certPath, wrongCertPath, wrongKeyPath), "/dcap")
	if err == nil {
		t.Fatal("Reload: expected error for a wrong-device cert (403 at probe), got nil")
	}
	t.Logf("Reload correctly refused: %v", err)

	// Old session must still be fully functional after the aborted reload.
	if _, err := fetcher.Get("/dcap"); err != nil {
		t.Fatalf("Get after aborted Reload: expected old session to still work, got: %v", err)
	}
}
