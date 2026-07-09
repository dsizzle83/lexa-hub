package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestEnsureServerCert_StableFingerprintAcrossCalls pins the TOFU contract:
// once cert.pem/key.pem exist, every subsequent call loads them unchanged.
func TestEnsureServerCert_StableFingerprintAcrossCalls(t *testing.T) {
	dir := t.TempDir()

	cert1, fp1, err := ensureServerCert(dir)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if len(cert1.Certificate) == 0 {
		t.Fatal("first call: empty certificate chain")
	}

	cert2, fp2, err := ensureServerCert(dir)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if len(cert2.Certificate) == 0 {
		t.Fatal("second call: empty certificate chain")
	}

	if fp1 != fp2 {
		t.Fatalf("fingerprint changed across calls: %s != %s", fp1, fp2)
	}
	if !bytes.Equal(cert1.Certificate[0], cert2.Certificate[0]) {
		t.Fatal("leaf DER bytes differ across calls — second call did not load the persisted file")
	}
}

func TestEnsureServerCert_KeyFilePerms0600(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := ensureServerCert(dir); err != nil {
		t.Fatalf("ensureServerCert: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, keyFileName))
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file perm = %o, want 0600", perm)
	}
}

func TestEnsureServerCert_CertFilePerms0644(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := ensureServerCert(dir); err != nil {
		t.Fatalf("ensureServerCert: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, certFileName))
	if err != nil {
		t.Fatalf("stat cert file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Fatalf("cert file perm = %o, want 0644", perm)
	}
}

func TestEnsureServerCert_EmptyDirErrors(t *testing.T) {
	if _, _, err := ensureServerCert(""); err == nil {
		t.Fatal("ensureServerCert(\"\"): want error, got nil")
	}
}

// TestEnsureServerCertFor_CNAndSANIncludeSerial pins CN "lexa-<serial>" and
// the "<serial>.local" DNS SAN, with the serial resolved from an explicit
// serial file (the config-key override path main() uses).
func TestEnsureServerCertFor_CNAndSANIncludeSerial(t *testing.T) {
	dir := t.TempDir()
	serialFile := filepath.Join(t.TempDir(), "serial")
	if err := os.WriteFile(serialFile, []byte("SN-42\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cert, _, err := ensureServerCertFor(dir, serialFile)
	if err != nil {
		t.Fatalf("ensureServerCertFor: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	if want := "lexa-SN-42"; leaf.Subject.CommonName != want {
		t.Fatalf("CN = %q, want %q", leaf.Subject.CommonName, want)
	}
	var sawSerialSAN bool
	for _, dn := range leaf.DNSNames {
		if dn == "SN-42.local" {
			sawSerialSAN = true
		}
	}
	if !sawSerialSAN {
		t.Fatalf("DNSNames %v missing %q", leaf.DNSNames, "SN-42.local")
	}
}

func TestResolveSerial_ReadsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serial")
	if err := os.WriteFile(path, []byte("  ABC-123  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := resolveSerial(path); got != "ABC-123" {
		t.Fatalf("resolveSerial = %q, want %q", got, "ABC-123")
	}
}

func TestResolveSerial_FallsBackToHostnameWhenFileMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	got := resolveSerial(missing)
	host, err := os.Hostname()
	if err != nil {
		t.Skipf("os.Hostname unavailable in this environment: %v", err)
	}
	if got != host {
		t.Fatalf("resolveSerial fallback = %q, want hostname %q", got, host)
	}
}

func TestResolveSerial_FallsBackWhenFileEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serial")
	if err := os.WriteFile(path, []byte("   \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := resolveSerial(path)
	host, _ := os.Hostname()
	if got != host {
		t.Fatalf("resolveSerial with empty file = %q, want hostname fallback %q", got, host)
	}
}

func TestFingerprint_IsSHA256HexOfLeafDER(t *testing.T) {
	dir := t.TempDir()
	cert, fp, err := ensureServerCert(dir)
	if err != nil {
		t.Fatalf("ensureServerCert: %v", err)
	}
	sum := sha256.Sum256(cert.Certificate[0])
	want := hex.EncodeToString(sum[:])
	if fp != want {
		t.Fatalf("fingerprint = %s, want %s", fp, want)
	}
	if len(fp) != 64 { // 32 bytes, hex-encoded, no separators
		t.Fatalf("fingerprint length = %d, want 64 (plain lowercase hex, no colons)", len(fp))
	}
}

// TestTLSServer_EndToEnd_FingerprintPinned exercises the actual HTTPS path:
// an httptest server using the generated cert, and a client that pins the
// fingerprint via VerifyPeerCertificate (InsecureSkipVerify + a callback)
// instead of chain validation — the same trust model DEVICE_ROADMAP.md
// §4.1 describes for a TOFU-pinning consumer.
func TestTLSServer_EndToEnd_FingerprintPinned(t *testing.T) {
	dir := t.TempDir()
	cert, wantFP, err := ensureServerCert(dir)
	if err != nil {
		t.Fatalf("ensureServerCert: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthzHandler)

	ts := httptest.NewUnstartedServer(mux)
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	ts.StartTLS()
	defer ts.Close()

	var sawFP string
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, // fingerprint pinning below stands in for chain trust
				VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
					if len(rawCerts) == 0 {
						return errors.New("no certs presented")
					}
					sum := sha256.Sum256(rawCerts[0])
					sawFP = hex.EncodeToString(sum[:])
					return nil
				},
			},
		},
	}

	resp, err := client.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET over TLS: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if sawFP != wantFP {
		t.Fatalf("presented fingerprint = %s, want the pinned %s", sawFP, wantFP)
	}
}
