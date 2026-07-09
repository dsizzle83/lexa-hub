package main

// certmon_test.go: unit 2.5/§2.7 — the cloud cert expiry monitor. Certs are
// generated in-process with controllable NotAfter (never checked in —
// cmd/northbound/certmon_test.go's discipline); assertions cover days-left,
// the two gauges, the binding-minimum rule, unreadable-file fail-closed
// reporting, and the CertDaysLeft → CloudlinkStatus overlay.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/metrics"
)

// writeCertExpiringAt writes a self-signed cert PEM whose NotAfter is the
// given instant and returns its path.
func writeCertExpiringAt(t *testing.T, dir, name string, notAfter time.Time) string {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "cloudlink-certmon-test"},
		NotBefore:             notAfter.Add(-365 * 24 * time.Hour),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("createcert: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// newTestCertMon builds a monitor over the given cert/CA paths with a frozen
// clock, returning the monitor and its registry for gauge assertions.
func newTestCertMon(t *testing.T, certPath, caPath string, warnDays int, now time.Time) (*cloudCertMon, *metrics.Registry) {
	t.Helper()
	reg := metrics.New()
	m := newCloudlinkMetrics(reg)
	cfg := &Config{CloudCert: certPath, CloudCA: caPath, CertExpiryWarnDays: warnDays}
	cm := newCloudCertMon(cfg, m)
	cm.now = func() time.Time { return now }
	return cm, reg
}

func TestCloudCertMon_Healthy(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1752000000, 0)
	cert := writeCertExpiringAt(t, dir, "cert.pem", now.Add(90*24*time.Hour))
	ca := writeCertExpiringAt(t, dir, "ca.pem", now.Add(120*24*time.Hour))

	cm, reg := newTestCertMon(t, cert, ca, 30, now)
	cm.CheckOnce()

	if got := cm.CloudDaysLeft(); got != 90 {
		t.Errorf("CloudDaysLeft = %d, want 90 (leaf is the binding cert)", got)
	}
	if got := metricValue(t, reg, "lexa_cloudlink_cert_expiring"); got != 0 {
		t.Errorf("cert_expiring = %v, want 0", got)
	}
	secs := metricValue(t, reg, "lexa_cloudlink_cert_expiry_seconds")
	want := (90 * 24 * time.Hour).Seconds()
	if secs < want-2 || secs > want+2 {
		t.Errorf("cert_expiry_seconds = %v, want ~%v", secs, want)
	}
}

func TestCloudCertMon_ExpiringSoon(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1752000000, 0)
	cert := writeCertExpiringAt(t, dir, "cert.pem", now.Add(10*24*time.Hour))
	ca := writeCertExpiringAt(t, dir, "ca.pem", now.Add(120*24*time.Hour))

	cm, reg := newTestCertMon(t, cert, ca, 30, now)
	cm.CheckOnce()

	if got := cm.CloudDaysLeft(); got != 10 {
		t.Errorf("CloudDaysLeft = %d, want 10", got)
	}
	if got := metricValue(t, reg, "lexa_cloudlink_cert_expiring"); got != 1 {
		t.Errorf("cert_expiring = %v, want 1 (inside the 30-day warn window)", got)
	}
}

func TestCloudCertMon_Expired(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1752000000, 0)
	cert := writeCertExpiringAt(t, dir, "cert.pem", now.Add(-48*time.Hour))
	ca := writeCertExpiringAt(t, dir, "ca.pem", now.Add(120*24*time.Hour))

	cm, reg := newTestCertMon(t, cert, ca, 30, now)
	cm.CheckOnce()

	if got := cm.CloudDaysLeft(); got > 0 {
		t.Errorf("CloudDaysLeft = %d, want <= 0 (expired)", got)
	}
	if got := metricValue(t, reg, "lexa_cloudlink_cert_expiring"); got != 1 {
		t.Errorf("cert_expiring = %v, want 1", got)
	}
	if secs := metricValue(t, reg, "lexa_cloudlink_cert_expiry_seconds"); secs >= 0 {
		t.Errorf("cert_expiry_seconds = %v, want negative once expired", secs)
	}
}

func TestCloudCertMon_UnreadableFailsClosed(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1752000000, 0)
	ca := writeCertExpiringAt(t, dir, "ca.pem", now.Add(120*24*time.Hour))

	t.Run("missing cert file", func(t *testing.T) {
		cm, reg := newTestCertMon(t, filepath.Join(dir, "nope.pem"), ca, 30, now)
		cm.CheckOnce() // must not panic
		// Binding falls back to the one readable cert (CA), but expiring=1
		// because an unreadable identity is itself an alarm condition.
		if got := cm.CloudDaysLeft(); got != 120 {
			t.Errorf("CloudDaysLeft = %d, want 120 (CA is the only readable cert)", got)
		}
		if got := metricValue(t, reg, "lexa_cloudlink_cert_expiring"); got != 1 {
			t.Errorf("cert_expiring = %v, want 1 (unreadable is an alarm)", got)
		}
	})

	t.Run("garbage PEM", func(t *testing.T) {
		bad := filepath.Join(dir, "garbage.pem")
		if err := os.WriteFile(bad, []byte("not a pem at all"), 0o644); err != nil {
			t.Fatal(err)
		}
		cm, reg := newTestCertMon(t, bad, ca, 30, now)
		cm.CheckOnce()
		if got := metricValue(t, reg, "lexa_cloudlink_cert_expiring"); got != 1 {
			t.Errorf("cert_expiring = %v, want 1", got)
		}
	})

	t.Run("both unreadable", func(t *testing.T) {
		cm, reg := newTestCertMon(t, filepath.Join(dir, "no1.pem"), filepath.Join(dir, "no2.pem"), 30, now)
		cm.CheckOnce()
		if got := cm.CloudDaysLeft(); got != 0 {
			t.Errorf("CloudDaysLeft = %d, want 0 (unknown state is worst case)", got)
		}
		if got := metricValue(t, reg, "lexa_cloudlink_cert_expiring"); got != 1 {
			t.Errorf("cert_expiring = %v, want 1", got)
		}
	})
}

// TestCloudCertMon_BindingMinimum pins the DaysLeft rule: whichever of
// cert/CA expires FIRST governs both the scalar and the seconds gauge.
func TestCloudCertMon_BindingMinimum(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1752000000, 0)
	cert := writeCertExpiringAt(t, dir, "cert.pem", now.Add(90*24*time.Hour))
	ca := writeCertExpiringAt(t, dir, "ca.pem", now.Add(10*24*time.Hour)) // CA is the binding cert

	cm, reg := newTestCertMon(t, cert, ca, 30, now)
	cm.CheckOnce()

	if got := cm.CloudDaysLeft(); got != 10 {
		t.Errorf("CloudDaysLeft = %d, want 10 (CA expires first)", got)
	}
	secs := metricValue(t, reg, "lexa_cloudlink_cert_expiry_seconds")
	want := (10 * 24 * time.Hour).Seconds()
	if secs < want-2 || secs > want+2 {
		t.Errorf("cert_expiry_seconds = %v, want ~%v (the CA's NotAfter, not the leaf's)", secs, want)
	}
	if got := metricValue(t, reg, "lexa_cloudlink_cert_expiring"); got != 1 {
		t.Errorf("cert_expiring = %v, want 1", got)
	}
}

func TestCloudCertMon_WarnDaysDefaultsWhenZero(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1752000000, 0)
	cert := writeCertExpiringAt(t, dir, "cert.pem", now.Add(20*24*time.Hour))
	ca := writeCertExpiringAt(t, dir, "ca.pem", now.Add(120*24*time.Hour))

	// warnDays 0 → defaultCertWarnDays (30): a 20-day cert is inside it.
	cm, reg := newTestCertMon(t, cert, ca, 0, now)
	cm.CheckOnce()
	if got := metricValue(t, reg, "lexa_cloudlink_cert_expiring"); got != 1 {
		t.Errorf("cert_expiring = %v, want 1 under the default 30-day window", got)
	}
}

// TestCloudCertMon_RunTicksAndStops proves Run does an immediate check plus
// periodic re-checks and exits on ctx cancel (short interval injected).
func TestCloudCertMon_RunTicksAndStops(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1752000000, 0)
	cert := writeCertExpiringAt(t, dir, "cert.pem", now.Add(90*24*time.Hour))
	ca := writeCertExpiringAt(t, dir, "ca.pem", now.Add(90*24*time.Hour))

	cm, _ := newTestCertMon(t, cert, ca, 30, now)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { cm.Run(ctx, 5*time.Millisecond); close(done) }()

	waitFor(t, "first check", func() bool { return cm.CloudDaysLeft() == 90 })
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit on ctx cancel")
	}
}

// TestStatusPublisher_CertDaysLeftOverlay wires a certDaysLeft func into
// statusPublisher and asserts the retained CloudlinkStatus carries it —
// §2.7's "no MQTT publish of its own; CloudlinkStatus carries it".
func TestStatusPublisher_CertDaysLeftOverlay(t *testing.T) {
	fc := &fakeBusClient{}
	cfg := &Config{Enabled: true, Endpoint: "ssl://x:8883", Uplink: UplinkConfig{HealthIntervalS: 3600}}
	reg := metrics.New()
	m := newCloudlinkMetrics(reg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		statusPublisher(ctx, fc, cfg, fakeSession{connected: true}, fakeSpool{}, func() int64 { return 111 }, func() int { return 7 }, m)
	}()

	waitFor(t, "status publish", func() bool { return len(fc.pubs()) >= 1 })

	pubs := fc.pubs()
	if pubs[0].topic != bus.TopicCloudlinkStatus || !pubs[0].retained {
		t.Fatalf("publish = %q retained=%v, want retained lexa/cloudlink/status", pubs[0].topic, pubs[0].retained)
	}
	var st bus.CloudlinkStatus
	if err := json.Unmarshal(pubs[0].payload, &st); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if st.CertDaysLeft != 7 {
		t.Errorf("CertDaysLeft = %d, want 7 (certmon overlay)", st.CertDaysLeft)
	}
	if st.LastUplinkTs != 111 {
		t.Errorf("LastUplinkTs = %d, want 111 (existing overlay unchanged)", st.LastUplinkTs)
	}
}

// TestStatusPublisher_NilCertDaysLeftIsZero pins the disabled path: a nil
// certDaysLeft func (local-only box, no monitor) leaves CertDaysLeft at its
// omitempty zero.
func TestStatusPublisher_NilCertDaysLeftIsZero(t *testing.T) {
	fc := &fakeBusClient{}
	cfg := &Config{Enabled: false, Uplink: UplinkConfig{HealthIntervalS: 3600}}
	reg := metrics.New()
	m := newCloudlinkMetrics(reg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		statusPublisher(ctx, fc, cfg, stubCloudSession{}, stubSpoolStats{}, nil, nil, m)
	}()

	waitFor(t, "status publish", func() bool { return len(fc.pubs()) >= 1 })
	var raw map[string]any
	if err := json.Unmarshal(fc.pubs()[0].payload, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["cert_days_left"]; ok {
		t.Error("cert_days_left present on a disabled box's status; want omitted (omitempty zero)")
	}
}
