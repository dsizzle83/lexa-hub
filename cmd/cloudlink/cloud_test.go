package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// genSelfSigned writes a self-signed ECDSA P-256 cert + key PEM pair into dir
// and returns their paths. The cert doubles as its own CA (IsCA), so the same
// PEM can serve as cloud_ca in tests.
func genSelfSigned(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "cloudlink-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("createcert: %v", err)
	}
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	writePEM(t, certPath, "CERTIFICATE", der)
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshalkey: %v", err)
	}
	writePEM(t, keyPath, "PRIVATE KEY", keyDER)
	return certPath, keyPath
}

func writePEM(t *testing.T, path, typ string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestBuildTLSConfig_OK(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := genSelfSigned(t, dir)
	cfg := &Config{CloudCA: certPath, CloudCert: certPath, CloudKey: keyPath}

	tlsCfg, err := buildTLSConfig(cfg)
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if tlsCfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %d, want TLS12 (%d)", tlsCfg.MinVersion, tls.VersionTLS12)
	}
	if len(tlsCfg.Certificates) != 1 {
		t.Errorf("Certificates = %d, want 1 (device identity)", len(tlsCfg.Certificates))
	}
	if tlsCfg.RootCAs == nil {
		t.Error("RootCAs is nil, want the pinned cloud CA pool")
	}
}

func TestBuildTLSConfig_Errors(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := genSelfSigned(t, dir)

	t.Run("missing ca", func(t *testing.T) {
		cfg := &Config{CloudCA: filepath.Join(dir, "nope.pem"), CloudCert: certPath, CloudKey: keyPath}
		if _, err := buildTLSConfig(cfg); err == nil {
			t.Error("expected error for missing cloud_ca")
		}
	})
	t.Run("garbage ca", func(t *testing.T) {
		bad := filepath.Join(dir, "bad-ca.pem")
		if err := os.WriteFile(bad, []byte("not a pem"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg := &Config{CloudCA: bad, CloudCert: certPath, CloudKey: keyPath}
		if _, err := buildTLSConfig(cfg); err == nil {
			t.Error("expected error for unparsable cloud_ca")
		}
	})
	t.Run("mismatched cert/key", func(t *testing.T) {
		_, otherKey := genSelfSigned(t, t.TempDir())
		cfg := &Config{CloudCA: certPath, CloudCert: certPath, CloudKey: otherKey}
		if _, err := buildTLSConfig(cfg); err == nil {
			t.Error("expected error for cert/key mismatch")
		}
	})
}

func TestReadSerial(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "serial")
	if err := os.WriteFile(p, []byte("  DEV-12345\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := readSerial(p)
	if err != nil {
		t.Fatalf("readSerial: %v", err)
	}
	if s != "DEV-12345" {
		t.Errorf("serial = %q, want DEV-12345 (trimmed)", s)
	}

	if _, err := readSerial(filepath.Join(dir, "missing")); err == nil {
		t.Error("expected error for missing serial file")
	}

	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("   \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readSerial(empty); err == nil {
		t.Error("expected error for empty serial file")
	}
}

func TestNewCloudSession_DisabledIsStub(t *testing.T) {
	cloud, err := newCloudSession(&Config{Enabled: false}, newTestMetrics())
	if err != nil {
		t.Fatalf("newCloudSession(disabled): %v", err)
	}
	if cloud.Connected() {
		t.Error("disabled session reports Connected() = true")
	}
	if cloud.Serial() != "" {
		t.Errorf("disabled session Serial() = %q, want empty", cloud.Serial())
	}
	if err := cloud.PublishFrame("t", []byte("x"), time.Second); err == nil {
		t.Error("disabled session PublishFrame returned nil, want error")
	}
}

func TestNewCloudSession_EnabledFatalOnBadCert(t *testing.T) {
	dir := t.TempDir()
	serial := filepath.Join(dir, "serial")
	if err := os.WriteFile(serial, []byte("DEV-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Enabled:    true,
		Endpoint:   "ssl://localhost:8883",
		SerialFile: serial,
		CloudCA:    filepath.Join(dir, "nope.pem"), // missing → fatal (returned error)
		CloudCert:  filepath.Join(dir, "nope.pem"),
		CloudKey:   filepath.Join(dir, "nope.pem"),
	}
	if _, err := newCloudSession(cfg, newTestMetrics()); err == nil {
		t.Error("enabled newCloudSession with missing certs returned nil error, want fatal")
	}
}

func TestUplinkTopics(t *testing.T) {
	if got := telemetryTopic("SER"); got != "lexa/v1/SER/telemetry" {
		t.Errorf("telemetryTopic = %q", got)
	}
	if got := eventsTopic("SER"); got != "lexa/v1/SER/events" {
		t.Errorf("eventsTopic = %q", got)
	}
}
