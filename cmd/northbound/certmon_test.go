package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// fakeClient is a minimal mqtt.Client stand-in for tests that need
// Monitor.CheckOnce/Run to actually call mqttutil.PublishJSONRetained
// without a real broker — mirrors internal/northbound/publish/publish_test.go's
// fakeClient (same rationale: publishJSONInner calls client.Publish directly,
// which panics on a nil interface value, so a real fake — not nil — is
// required wherever CheckOnce runs).
type fakeClient struct {
	mu        sync.Mutex
	publishes int
}

func (f *fakeClient) IsConnected() bool      { return true }
func (f *fakeClient) IsConnectionOpen() bool { return true }
func (f *fakeClient) Connect() mqtt.Token    { panic("not implemented") }
func (f *fakeClient) Disconnect(quiesce uint) {
}
func (f *fakeClient) Publish(topic string, qos byte, retained bool, payload interface{}) mqtt.Token {
	f.mu.Lock()
	f.publishes++
	f.mu.Unlock()
	return &doneToken{}
}
func (f *fakeClient) Subscribe(topic string, qos byte, callback mqtt.MessageHandler) mqtt.Token {
	panic("not implemented")
}
func (f *fakeClient) SubscribeMultiple(filters map[string]byte, callback mqtt.MessageHandler) mqtt.Token {
	panic("not implemented")
}
func (f *fakeClient) Unsubscribe(topics ...string) mqtt.Token { panic("not implemented") }
func (f *fakeClient) AddRoute(topic string, callback mqtt.MessageHandler) {
	panic("not implemented")
}
func (f *fakeClient) OptionsReader() mqtt.ClientOptionsReader {
	panic("not implemented")
}

type doneToken struct{}

func (t *doneToken) Wait() bool                       { return true }
func (t *doneToken) WaitTimeout(d time.Duration) bool { return true }
func (t *doneToken) Done() <-chan struct{}            { c := make(chan struct{}); close(c); return c }
func (t *doneToken) Error() error                     { return nil }

// writeTestCertFile generates a fresh self-signed ECDSA P-256 certificate
// valid from notBefore to notAfter, PEM-encodes it, writes it to a temp file,
// and returns the path. No key material is ever checked in — every fixture
// here is generated in-process (task's "common mistakes": never commit test
// keys or a short-lived bench cert).
func writeTestCertFile(t *testing.T, notBefore, notAfter time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generate serial: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "certmon-test"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	path := filepath.Join(t.TempDir(), "cert.pem")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("pem encode: %v", err)
	}
	return path
}

// writeChainFile writes leaf then a second (intermediate-shaped) certificate
// into ONE PEM file, to exercise inspectCertFile's "take the leaf, which is
// the first block" rule on a genuine chain file.
func writeChainFile(t *testing.T, leafNotAfter, secondNotAfter time.Time) string {
	t.Helper()
	leafPath := writeTestCertFile(t, time.Now().Add(-time.Hour), leafNotAfter)
	secondPath := writeTestCertFile(t, time.Now().Add(-time.Hour), secondNotAfter)
	leaf, err := os.ReadFile(leafPath)
	if err != nil {
		t.Fatalf("read leaf: %v", err)
	}
	second, err := os.ReadFile(secondPath)
	if err != nil {
		t.Fatalf("read second: %v", err)
	}
	path := filepath.Join(t.TempDir(), "chain.pem")
	if err := os.WriteFile(path, append(append([]byte{}, leaf...), second...), 0o644); err != nil {
		t.Fatalf("write chain: %v", err)
	}
	return path
}

func TestInspectCertFile_Valid(t *testing.T) {
	notAfter := time.Now().Add(365 * 24 * time.Hour).Truncate(time.Second)
	path := writeTestCertFile(t, time.Now().Add(-time.Hour), notAfter)
	info, err := inspectCertFile(path)
	if err != nil {
		t.Fatalf("inspectCertFile: %v", err)
	}
	if !info.NotAfter.Equal(notAfter) {
		t.Errorf("NotAfter = %v, want %v", info.NotAfter, notAfter)
	}
}

func TestInspectCertFile_Expired(t *testing.T) {
	notAfter := time.Now().Add(-48 * time.Hour)
	path := writeTestCertFile(t, time.Now().Add(-72*time.Hour), notAfter)
	info, err := inspectCertFile(path)
	if err != nil {
		t.Fatalf("inspectCertFile: %v", err)
	}
	if !info.NotAfter.Before(time.Now()) {
		t.Errorf("expired cert's NotAfter %v should be before now", info.NotAfter)
	}
}

func TestInspectCertFile_Chain_TakesLeaf(t *testing.T) {
	leafNotAfter := time.Now().Add(10 * 24 * time.Hour).Truncate(time.Second)
	secondNotAfter := time.Now().Add(3650 * 24 * time.Hour).Truncate(time.Second)
	path := writeChainFile(t, leafNotAfter, secondNotAfter)
	info, err := inspectCertFile(path)
	if err != nil {
		t.Fatalf("inspectCertFile: %v", err)
	}
	if !info.NotAfter.Equal(leafNotAfter) {
		t.Errorf("chain file: NotAfter = %v, want the LEAF's %v (not the second block's %v)",
			info.NotAfter, leafNotAfter, secondNotAfter)
	}
}

func TestInspectCertFile_Garbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "garbage.pem")
	if err := os.WriteFile(path, []byte("this is not PEM data at all\n"), 0o644); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	if _, err := inspectCertFile(path); err == nil {
		t.Fatal("inspectCertFile on garbage data: want error, got nil")
	}
}

func TestInspectCertFile_MissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.pem")
	if _, err := inspectCertFile(path); err == nil {
		t.Fatal("inspectCertFile on a missing file: want error, got nil")
	}
}

func TestInspectCertFile_MalformedBlock(t *testing.T) {
	// A well-formed PEM envelope around bytes that are not a valid DER
	// certificate — exercises the x509.ParseCertificate error path
	// distinctly from "no PEM block at all".
	path := filepath.Join(t.TempDir(), "malformed.pem")
	block := &pem.Block{Type: "CERTIFICATE", Bytes: []byte("not a real certificate DER")}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if err := pem.Encode(f, block); err != nil {
		t.Fatalf("pem encode: %v", err)
	}
	if _, err := inspectCertFile(path); err == nil {
		t.Fatal("inspectCertFile on a malformed DER block: want error, got nil")
	}
}

// TestDaysUntil_Boundaries pins the <=30d WARN / <=0d ERROR boundary
// semantics daysUntil must produce for classify to key off directly (task
// step 1's "table tests" + step 2's exact threshold language).
func TestDaysUntil_Boundaries(t *testing.T) {
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		notAfter time.Time
		want     int
	}{
		{"exactly 30 days remaining", now.Add(30 * 24 * time.Hour), 30},
		{"30 days + 1 hour remaining rounds up to 31", now.Add(30*24*time.Hour + time.Hour), 31},
		{"29 days 23 hours remaining rounds up to 30", now.Add(29*24*time.Hour + 23*time.Hour), 30},
		{"12 hours remaining rounds up to 1 (not yet expired)", now.Add(12 * time.Hour), 1},
		{"exactly at expiry (0 remaining) is 0 (expired)", now, 0},
		{"1 hour past expiry", now.Add(-time.Hour), -1},
		{"48 hours past expiry", now.Add(-48 * time.Hour), -2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := daysUntil(c.notAfter, now); got != c.want {
				t.Errorf("daysUntil(%v, now) = %d, want %d", c.notAfter, got, c.want)
			}
		})
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		daysLeft, warnDays int
		want               certLevel
	}{
		{31, 30, certLevelOK},
		{30, 30, certLevelWarn},
		{1, 30, certLevelWarn},
		{0, 30, certLevelError},
		{-5, 30, certLevelError},
		{45, 30, certLevelOK},
	}
	for _, c := range cases {
		if got := classify(c.daysLeft, c.warnDays); got != c.want {
			t.Errorf("classify(%d, %d) = %v, want %v", c.daysLeft, c.warnDays, got, c.want)
		}
	}
}

func TestMonitor_CheckOnce_Valid(t *testing.T) {
	clientPath := writeTestCertFile(t, time.Now().Add(-time.Hour), time.Now().Add(365*24*time.Hour))
	caPath := writeTestCertFile(t, time.Now().Add(-time.Hour), time.Now().Add(3650*24*time.Hour))

	m := NewMonitor(&fakeClient{}, clientPath, caPath, 30, nil)
	status := m.CheckOnce()

	if status.ClientErr != "" || status.CAErr != "" {
		t.Fatalf("unexpected errors: client=%q ca=%q", status.ClientErr, status.CAErr)
	}
	if status.DaysLeft < 300 {
		t.Errorf("DaysLeft = %d, want a large positive value for a 1-year cert", status.DaysLeft)
	}
	if status.V != 1 {
		t.Errorf("Envelope.V = %d, want 1 (bus.CertStatusV)", status.V)
	}
}

func TestMonitor_CheckOnce_ExpiringSoon(t *testing.T) {
	// 10 days left on the client cert, CA cert healthy — DaysLeft (the
	// binding constraint) must reflect the SOONER of the two.
	clientPath := writeTestCertFile(t, time.Now().Add(-24*time.Hour), time.Now().Add(10*24*time.Hour))
	caPath := writeTestCertFile(t, time.Now().Add(-24*time.Hour), time.Now().Add(3650*24*time.Hour))

	m := NewMonitor(&fakeClient{}, clientPath, caPath, 30, nil)
	status := m.CheckOnce()

	if status.DaysLeft > 11 || status.DaysLeft < 9 {
		t.Errorf("DaysLeft = %d, want ~10 (the client cert's remaining days, the binding constraint)", status.DaysLeft)
	}
	if classify(status.DaysLeft, 30) != certLevelWarn {
		t.Errorf("classify(%d, 30) = %v, want certLevelWarn", status.DaysLeft, classify(status.DaysLeft, 30))
	}
}

func TestMonitor_CheckOnce_Expired(t *testing.T) {
	clientPath := writeTestCertFile(t, time.Now().Add(-72*time.Hour), time.Now().Add(-24*time.Hour))
	caPath := writeTestCertFile(t, time.Now().Add(-72*time.Hour), time.Now().Add(3650*24*time.Hour))

	m := NewMonitor(&fakeClient{}, clientPath, caPath, 30, nil)
	status := m.CheckOnce()

	if status.DaysLeft >= 0 {
		t.Errorf("DaysLeft = %d, want negative (client cert expired)", status.DaysLeft)
	}
	if classify(status.DaysLeft, 30) != certLevelError {
		t.Errorf("classify(%d, 30) = %v, want certLevelError", status.DaysLeft, classify(status.DaysLeft, 30))
	}
}

func TestMonitor_CheckOnce_MissingFile_ReportsErrorNotCrash(t *testing.T) {
	caPath := writeTestCertFile(t, time.Now().Add(-time.Hour), time.Now().Add(365*24*time.Hour))
	missing := filepath.Join(t.TempDir(), "no-such-cert.pem")

	m := NewMonitor(&fakeClient{}, missing, caPath, 30, nil)
	status := m.CheckOnce() // must not panic

	if status.ClientErr == "" {
		t.Error("ClientErr should be populated for a missing client cert file (fail-closed reporting)")
	}
	if status.CAErr != "" {
		t.Errorf("CAErr should be empty (CA cert is valid), got %q", status.CAErr)
	}
	// Only the CA cert was successfully inspected; DaysLeft falls back to it.
	if status.DaysLeft < 300 {
		t.Errorf("DaysLeft = %d, want the CA cert's large remaining days when the client cert failed", status.DaysLeft)
	}
}

// TestMonitor_Run_DailyTicker verifies the ticker edge directly against
// Monitor.Run: an immediate startup check, then a re-check on every
// subsequent tick, until ctx is cancelled — the "daily re-check is the
// point" requirement (task's common mistakes list: alerting once at startup
// only misses a cert that crosses the threshold mid-run). Uses a short
// injected interval (Run's second parameter) instead of the real 24h
// production value so the test completes in milliseconds; a wrapped
// mqtt.Client-free Monitor (mc: nil) is fine here because
// mqttutil.PublishJSONRetainedtolerates being handed work — the assertion
// below only cares that CheckOnce ran repeatedly, via the count.
func TestMonitor_Run_DailyTicker(t *testing.T) {
	clientPath := writeTestCertFile(t, time.Now().Add(-time.Hour), time.Now().Add(365*24*time.Hour))
	caPath := writeTestCertFile(t, time.Now().Add(-time.Hour), time.Now().Add(3650*24*time.Hour))

	m := NewMonitor(&fakeClient{}, clientPath, caPath, 30, nil)
	// countingClock stands in for m.now: each read increments a counter, so
	// the test can tell how many times CheckOnce actually ran without
	// depending on wall-clock timing races.
	var checks int
	var mu sync.Mutex
	m.now = func() time.Time {
		mu.Lock()
		checks++
		mu.Unlock()
		return time.Now()
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(ctx, 10*time.Millisecond)
	}()

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := checks
		mu.Unlock()
		if n >= 3 {
			break
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("timed out waiting for >=3 checks, got %d", n)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done // Run must actually return once ctx is cancelled — owned-goroutine shutdown (05 §4).
}
