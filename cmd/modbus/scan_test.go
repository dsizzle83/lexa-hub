package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/journal"
	"lexa-hub/internal/metrics"
	"lexa-proto/sunspec"
)

// ── fake mqtt.Client (Publish-recording only; mirrors heal_stale_test.go's
// fakeHealMQTTClient pattern, which is this package's existing precedent for
// a hand-rolled mqtt.Client double) ──────────────────────────────────────────

type fakeScanPublish struct {
	topic    string
	qos      byte
	retained bool
	payload  []byte
}

// fakeScanMQTTClient records every Publish call. Guarded by a mutex because
// TestHandleRequest_ConcurrentRefused drives it from two goroutines at once.
// Every other mqtt.Client method panics if called — nothing in scan.go's
// tested paths needs Connect/SubscribeMultiple/Unsubscribe/AddRoute/
// OptionsReader, so a test that accidentally exercises one of those fails
// loudly instead of silently no-opping.
type fakeScanMQTTClient struct {
	mu        sync.Mutex
	publishes []fakeScanPublish
}

func (f *fakeScanMQTTClient) Publish(topic string, qos byte, retained bool, payload interface{}) mqtt.Token {
	var b []byte
	switch p := payload.(type) {
	case []byte:
		b = p
	case string:
		b = []byte(p)
	}
	f.mu.Lock()
	f.publishes = append(f.publishes, fakeScanPublish{topic: topic, qos: qos, retained: retained, payload: b})
	f.mu.Unlock()
	return fakeScanDoneToken{}
}

func (f *fakeScanMQTTClient) snapshot() []fakeScanPublish {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeScanPublish, len(f.publishes))
	copy(out, f.publishes)
	return out
}

func (f *fakeScanMQTTClient) IsConnected() bool      { return true }
func (f *fakeScanMQTTClient) IsConnectionOpen() bool { return true }
func (f *fakeScanMQTTClient) Connect() mqtt.Token    { panic("not implemented") }
func (f *fakeScanMQTTClient) Disconnect(uint)        {}

func (f *fakeScanMQTTClient) Subscribe(topic string, qos byte, handler mqtt.MessageHandler) mqtt.Token {
	return fakeScanDoneToken{}
}
func (f *fakeScanMQTTClient) SubscribeMultiple(map[string]byte, mqtt.MessageHandler) mqtt.Token {
	panic("not implemented")
}
func (f *fakeScanMQTTClient) Unsubscribe(topics ...string) mqtt.Token { panic("not implemented") }
func (f *fakeScanMQTTClient) AddRoute(string, mqtt.MessageHandler)    { panic("not implemented") }
func (f *fakeScanMQTTClient) OptionsReader() mqtt.ClientOptionsReader { panic("not implemented") }

type fakeScanDoneToken struct{}

func (fakeScanDoneToken) Wait() bool                     { return true }
func (fakeScanDoneToken) WaitTimeout(time.Duration) bool { return true }
func (fakeScanDoneToken) Done() <-chan struct{}          { c := make(chan struct{}); close(c); return c }
func (fakeScanDoneToken) Error() error                   { return nil }

// scanStatusPubs decodes every bus.ScanStatus published on bus.TopicScanStatus
// matching id and phase, in publish order.
func scanStatusPubs(f *fakeScanMQTTClient, id, phase string) []bus.ScanStatus {
	var out []bus.ScanStatus
	for _, p := range f.snapshot() {
		if p.topic != bus.TopicScanStatus {
			continue
		}
		var st bus.ScanStatus
		if err := json.Unmarshal(p.payload, &st); err != nil {
			continue
		}
		if st.ID == id && (phase == "" || st.Phase == phase) {
			out = append(out, st)
		}
	}
	return out
}

// waitUntil polls cond every 2ms up to a 2s deadline; fails the test on
// timeout. Used only to synchronize with a background goroutine's completion
// (TestHandleRequest_ConcurrentRefused) — not a substitute for real
// synchronization where a channel is available.
func waitUntil(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(msg)
}

// withFakeSweeps overrides the sweepTCP/sweepRTU seams for the duration of a
// test, restoring the real lexa-proto primitives on cleanup. A nil argument
// leaves that seam untouched.
func withFakeSweeps(t *testing.T,
	tcp func(ctx context.Context, cidr string, port int, unitIDs []uint8, timeout time.Duration, hit func(sunspec.SweepHit)) error,
	rtu func(ctx context.Context, dev string, bauds []int, unitIDs []uint8, timeout time.Duration, hit func(sunspec.SweepHit)) error,
) {
	t.Helper()
	origTCP, origRTU := sweepTCP, sweepRTU
	if tcp != nil {
		sweepTCP = tcp
	}
	if rtu != nil {
		sweepRTU = rtu
	}
	t.Cleanup(func() { sweepTCP, sweepRTU = origTCP, origRTU })
}

// noHitsTCP/noHitsRTU are the default "nothing found" fakes for tests that
// only care about arguments passed in or don't touch RTU at all.
func noHitsTCP(ctx context.Context, cidr string, port int, unitIDs []uint8, timeout time.Duration, hit func(sunspec.SweepHit)) error {
	return nil
}
func noHitsRTU(ctx context.Context, dev string, bauds []int, unitIDs []uint8, timeout time.Duration, hit func(sunspec.SweepHit)) error {
	return nil
}

// ── 1. Arming rule ───────────────────────────────────────────────────────

func TestScanArmed(t *testing.T) {
	cases := []struct {
		name       string
		cfg        *Config
		wantArmed  bool
		wantReason string // substring, only checked when !wantArmed
	}{
		{
			name:      "uncommissioned (no devices) runs",
			cfg:       &Config{},
			wantArmed: true,
		},
		{
			name: "battery device configured + reconciler active is refused",
			cfg: &Config{
				Devices:    []DeviceConfig{{Name: "b0", Role: "battery"}},
				Reconciler: map[string]string{"battery": "active"},
			},
			wantArmed:  false,
			wantReason: "battery",
		},
		{
			name: "battery device configured + reconciler off runs (all-off)",
			cfg: &Config{
				Devices:    []DeviceConfig{{Name: "b0", Role: "battery"}},
				Reconciler: map[string]string{"battery": "off"},
			},
			wantArmed: true,
		},
		{
			name: "inverter device configured + solar shadow (not off) is refused",
			cfg: &Config{
				Devices:    []DeviceConfig{{Name: "inv0", Role: "inverter"}},
				Reconciler: map[string]string{"solar": "shadow"},
			},
			wantArmed:  false,
			wantReason: "solar",
		},
		{
			name: "battery+inverter configured, both reconcilers off runs (all-off)",
			cfg: &Config{
				Devices: []DeviceConfig{
					{Name: "b0", Role: "battery"},
					{Name: "inv0", Role: "inverter"},
				},
				Reconciler: map[string]string{"battery": "off", "solar": "off"},
			},
			wantArmed: true,
		},
		{
			name: "battery off but solar active (mixed) is refused",
			cfg: &Config{
				Devices: []DeviceConfig{
					{Name: "b0", Role: "battery"},
					{Name: "inv0", Role: "inverter"},
				},
				Reconciler: map[string]string{"battery": "off", "solar": "active"},
			},
			wantArmed:  false,
			wantReason: "solar",
		},
		{
			// Meters have no reconciler concept at all (reconcilerClassByRole
			// in config.go has no "meter" entry), so a meter-only fleet is
			// vacuously "every present class is off" — the v1 rule arms even
			// though the meter is still being live-polled. Documented as a
			// known v1 gap in scan.go's package doc; pinned here so a future
			// v2 armed-pause handshake change shows up as an intentional
			// test update, not a silent regression.
			name: "meter-only devices runs (documented v1 gap)",
			cfg: &Config{
				Devices: []DeviceConfig{{Name: "m0", Role: "meter"}},
			},
			wantArmed: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			armed, reason := scanArmed(c.cfg)
			if armed != c.wantArmed {
				t.Fatalf("scanArmed() = (%v, %q), want armed=%v", armed, reason, c.wantArmed)
			}
			if !c.wantArmed && !strings.Contains(reason, c.wantReason) {
				t.Errorf("refusal reason %q does not contain %q", reason, c.wantReason)
			}
		})
	}
}

// ── 2. Classification ────────────────────────────────────────────────────

func TestClassifyHit(t *testing.T) {
	cases := []struct {
		name   string
		models []uint16
		want   string
	}{
		{"802 (Li-Ion battery detail) alone", []uint16{802}, "battery"},
		{"801 (battery base) alone", []uint16{801}, "battery"},
		{"802 present alongside DER control models 703/704 — battery wins", []uint16{1, 802, 703, 704}, "battery"},
		{"201 (single-phase meter) alone", []uint16{201}, "meter"},
		{"203 (three-phase meter) alongside legacy inverter AC 101 — meter wins", []uint16{203, 101}, "meter"},
		{"103 (legacy three-phase inverter AC)", []uint16{1, 103}, "inverter"},
		{"701+704 (IEEE 1547-2018 DER measure+control)", []uint16{1, 701, 704}, "inverter"},
		{"120/121/123 legacy companion models alone (no primary signal)", []uint16{120, 121, 123}, "unknown-sunspec"},
		{"empty model list (marker matched, scan/identify failed)", nil, "unknown-sunspec"},
		{"unrecognized model id", []uint16{999}, "unknown-sunspec"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyHit(c.models); got != c.want {
				t.Errorf("classifyHit(%v) = %q, want %q", c.models, got, c.want)
			}
		})
	}
}

// ── 3. Local /24 derivation ──────────────────────────────────────────────

func addrIPNet(ip net.IP, ones int) net.Addr {
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(ones, 32)}
}

func TestDeriveLocalCIDR(t *testing.T) {
	t.Run("first non-loopback IPv4 interface", func(t *testing.T) {
		orig := interfaceAddrs
		interfaceAddrs = func() ([]net.Addr, error) {
			return []net.Addr{
				addrIPNet(net.IPv4(127, 0, 0, 1), 8),
				addrIPNet(net.IPv4(192, 168, 1, 42), 24),
			}, nil
		}
		t.Cleanup(func() { interfaceAddrs = orig })

		got, err := deriveLocalCIDR()
		if err != nil {
			t.Fatalf("deriveLocalCIDR() error: %v", err)
		}
		if got != "192.168.1.0/24" {
			t.Errorf("deriveLocalCIDR() = %q, want 192.168.1.0/24", got)
		}
	})

	t.Run("loopback-only host errors", func(t *testing.T) {
		orig := interfaceAddrs
		interfaceAddrs = func() ([]net.Addr, error) {
			return []net.Addr{addrIPNet(net.IPv4(127, 0, 0, 1), 8)}, nil
		}
		t.Cleanup(func() { interfaceAddrs = orig })

		if _, err := deriveLocalCIDR(); err == nil {
			t.Fatal("deriveLocalCIDR() = nil error, want an error on a loopback-only host")
		}
	})

	t.Run("interface enumeration error propagates", func(t *testing.T) {
		orig := interfaceAddrs
		wantErr := errTestSentinel
		interfaceAddrs = func() ([]net.Addr, error) { return nil, wantErr }
		t.Cleanup(func() { interfaceAddrs = orig })

		if _, err := deriveLocalCIDR(); err == nil {
			t.Fatal("deriveLocalCIDR() = nil error, want the enumeration error propagated")
		}
	})
}

var errTestSentinel = &testSentinelErr{}

type testSentinelErr struct{}

func (*testSentinelErr) Error() string { return "sentinel interface enumeration failure" }

// TestRunScan_ErroredCIDRDerivationRefuses covers the caller side: runScan
// turns a deriveLocalCIDR failure into a refused ScanStatus (never a panic,
// never a silent sweep of 127.0.0.0/24) when the request leaves TCPCidr empty.
func TestRunScan_ErroredCIDRDerivationRefuses(t *testing.T) {
	orig := interfaceAddrs
	interfaceAddrs = func() ([]net.Addr, error) {
		return []net.Addr{addrIPNet(net.IPv4(127, 0, 0, 1), 8)}, nil
	}
	t.Cleanup(func() { interfaceAddrs = orig })
	withFakeSweeps(t, func(ctx context.Context, cidr string, port int, unitIDs []uint8, timeout time.Duration, hit func(sunspec.SweepHit)) error {
		t.Fatal("sweepTCP must not be called when CIDR derivation fails")
		return nil
	}, nil)

	fc := &fakeScanMQTTClient{}
	mreg := metrics.New()
	sc := newScanController(fc, &Config{}, nil, mreg)
	sc.runScan(bus.ScanRequest{ID: "scan-nocidr"}) // TCPCidr empty ⇒ derive, which fails

	pubs := scanStatusPubs(fc, "scan-nocidr", "refused")
	if len(pubs) != 1 {
		t.Fatalf("got %d refused status lines, want 1", len(pubs))
	}
	// A CIDR-derivation failure never reaches sweepTCP, so it must count as
	// a refusal ONLY — runsCtr/refusedCtr stay mutually exclusive per attempt
	// (runsCtr increments only once a sweep is actually about to start).
	out := mreg.Format()
	if !strings.Contains(out, "lexa_mb_scan_refused_total 1") {
		t.Errorf("refused counter not incremented:\n%s", out)
	}
	if !strings.Contains(out, "lexa_mb_scan_runs_total 0") {
		t.Errorf("runs counter must stay at 0 when CIDR derivation fails before any sweep:\n%s", out)
	}
}

// ── 4. Defaults: TCP unit IDs, RTU passthrough, port, bauds ──────────────

func TestRunScan_TCPDefaultsToBusContractUnitIDs(t *testing.T) {
	var gotCIDR string
	var gotPort int
	var gotUnitIDs []uint8
	withFakeSweeps(t, func(ctx context.Context, cidr string, port int, unitIDs []uint8, timeout time.Duration, hit func(sunspec.SweepHit)) error {
		gotCIDR, gotPort, gotUnitIDs = cidr, port, unitIDs
		return nil
	}, nil)

	fc := &fakeScanMQTTClient{}
	sc := newScanController(fc, &Config{}, nil, metrics.New())
	sc.runScan(bus.ScanRequest{ID: "scan-defaults", TCPCidr: "10.0.0.0/24"})

	if gotCIDR != "10.0.0.0/24" {
		t.Errorf("cidr = %q, want the explicitly-supplied 10.0.0.0/24", gotCIDR)
	}
	if gotPort != scanDefaultTCPPort {
		t.Errorf("port = %d, want default %d", gotPort, scanDefaultTCPPort)
	}
	// Proto's OWN empty-unitIDs default for SweepTCP is just {1} — the
	// controller must pass the WIDER bus-contract default explicitly
	// (docs/extension/00_PROGRESS.md 5.1 review ruling).
	if !reflect.DeepEqual(gotUnitIDs, scanDefaultTCPUnitIDs) {
		t.Errorf("tcp unitIDs = %v, want the bus-contract default %v", gotUnitIDs, scanDefaultTCPUnitIDs)
	}
}

func TestRunScan_TCPExplicitUnitIDsHonored(t *testing.T) {
	var gotUnitIDs []uint8
	withFakeSweeps(t, func(ctx context.Context, cidr string, port int, unitIDs []uint8, timeout time.Duration, hit func(sunspec.SweepHit)) error {
		gotUnitIDs = unitIDs
		return nil
	}, nil)

	fc := &fakeScanMQTTClient{}
	sc := newScanController(fc, &Config{}, nil, metrics.New())
	explicit := []uint8{5, 7}
	sc.runScan(bus.ScanRequest{ID: "scan-explicit", TCPCidr: "10.0.0.0/24", UnitIDs: explicit})

	if !reflect.DeepEqual(gotUnitIDs, explicit) {
		t.Errorf("tcp unitIDs = %v, want the request's explicit %v (no override)", gotUnitIDs, explicit)
	}
}

func TestRunScan_RTUUnitIDsPassthroughEmpty(t *testing.T) {
	var rtuCalled bool
	var gotRTUUnitIDs []uint8
	var gotBauds []int
	withFakeSweeps(t, noHitsTCP, func(ctx context.Context, dev string, bauds []int, unitIDs []uint8, timeout time.Duration, hit func(sunspec.SweepHit)) error {
		rtuCalled = true
		gotRTUUnitIDs = unitIDs
		gotBauds = bauds
		return nil
	})

	fc := &fakeScanMQTTClient{}
	sc := newScanController(fc, &Config{}, nil, metrics.New())
	sc.runScan(bus.ScanRequest{ID: "scan-rtu", TCPCidr: "10.0.0.0/24", RTUDev: "/dev/ttyUSB0"})

	if !rtuCalled {
		t.Fatal("expected sweepRTU to run when RTUDev is set")
	}
	if gotRTUUnitIDs != nil {
		t.Errorf("rtu unitIDs = %v, want nil (untouched passthrough — sweepRTU applies its OWN 1..247 default)", gotRTUUnitIDs)
	}
	if !reflect.DeepEqual(gotBauds, scanDefaultBauds) {
		t.Errorf("bauds = %v, want default %v", gotBauds, scanDefaultBauds)
	}
}

func TestRunScan_RTUSkippedWhenDevEmpty(t *testing.T) {
	var rtuCalled bool
	withFakeSweeps(t, noHitsTCP, func(ctx context.Context, dev string, bauds []int, unitIDs []uint8, timeout time.Duration, hit func(sunspec.SweepHit)) error {
		rtuCalled = true
		return nil
	})

	fc := &fakeScanMQTTClient{}
	sc := newScanController(fc, &Config{}, nil, metrics.New())
	sc.runScan(bus.ScanRequest{ID: "scan-nortu", TCPCidr: "10.0.0.0/24"}) // RTUDev empty

	if rtuCalled {
		t.Error("sweepRTU must not run when RTUDev is empty")
	}
}

// ── 5. Progress rate-limit (fake clock) ──────────────────────────────────

func TestScanProgress_RateLimit(t *testing.T) {
	fc := &fakeScanMQTTClient{}
	sc := newScanController(fc, &Config{}, nil, metrics.New())
	clock := time.Unix(1700000000, 0)
	sc.now = func() time.Time { return clock }

	p := newScanProgress(sc, "scan-rl", "tcp")
	p.observe(1) // first observation always publishes
	p.observe(2) // <1s later — rate-limited, no publish
	clock = clock.Add(1500 * time.Millisecond)
	p.observe(3)   // ≥1s since last publish — publishes
	p.flushFinal() // always publishes, regardless of timing

	pubs := scanStatusPubs(fc, "scan-rl", "tcp")
	if len(pubs) != 3 {
		t.Fatalf("got %d tcp status publishes, want 3 (first observe, post-rate-limit observe, flushFinal)", len(pubs))
	}
	if pubs[0].Found != 1 {
		t.Errorf("publish[0].Found = %d, want 1", pubs[0].Found)
	}
	if pubs[1].Found != 3 {
		t.Errorf("publish[1].Found = %d, want 3 (observe(2) at n=2 must have been suppressed by the rate limit)", pubs[1].Found)
	}
	if pubs[2].Found != 3 {
		t.Errorf("publish[2].Found = %d, want 3 (flushFinal repeats the last observed count)", pubs[2].Found)
	}
}

// ── 6. Concurrency: one scan at a time ────────────────────────────────────

func TestHandleRequest_ConcurrentRefused(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	withFakeSweeps(t, func(ctx context.Context, cidr string, port int, unitIDs []uint8, timeout time.Duration, hit func(sunspec.SweepHit)) error {
		close(started)
		<-release
		return nil
	}, nil)

	fc := &fakeScanMQTTClient{}
	sc := newScanController(fc, &Config{}, nil, metrics.New())

	go sc.handleRequest(bus.ScanRequest{ID: "scan-1", TCPCidr: "10.0.0.0/24"})
	<-started // scan-1 is now inside sweepTCP, holding sc.running

	// scan-2 arrives while scan-1 is still running: must be refused
	// immediately (synchronously), not queued behind scan-1.
	sc.handleRequest(bus.ScanRequest{ID: "scan-2", TCPCidr: "10.0.0.0/24"})

	close(release)
	waitUntil(t, func() bool { return !sc.running.Load() }, "scan-1 never finished")

	refused := scanStatusPubs(fc, "scan-2", "refused")
	if len(refused) != 1 {
		t.Fatalf("got %d refused status lines for scan-2, want 1", len(refused))
	}
	if refused[0].Detail != "scan already running" {
		t.Errorf("refusal detail = %q, want %q", refused[0].Detail, "scan already running")
	}
	// scan-1 must have completed normally (not itself refused).
	if got := scanStatusPubs(fc, "scan-1", "refused"); len(got) != 0 {
		t.Errorf("scan-1 was refused (%v), want it to have run to completion", got)
	}
	if got := scanStatusPubs(fc, "scan-1", "done"); len(got) != 1 {
		t.Errorf("scan-1 done status lines = %d, want 1", len(got))
	}
}

// ── 7. Refusal end-to-end: unarmed request never touches the sweep seams ──

func TestHandleRequest_RefusedWhenNotArmed(t *testing.T) {
	withFakeSweeps(t, func(ctx context.Context, cidr string, port int, unitIDs []uint8, timeout time.Duration, hit func(sunspec.SweepHit)) error {
		t.Fatal("sweepTCP must not be called when the scan is refused by the arming rule")
		return nil
	}, nil)

	fc := &fakeScanMQTTClient{}
	mreg := metrics.New()
	cfg := &Config{
		Devices:    []DeviceConfig{{Name: "b0", Role: "battery"}},
		Reconciler: map[string]string{"battery": "active"},
	}
	sc := newScanController(fc, cfg, nil, mreg)

	sc.handleRequest(bus.ScanRequest{ID: "scan-refused"})

	pubs := scanStatusPubs(fc, "scan-refused", "refused")
	if len(pubs) != 1 {
		t.Fatalf("got %d refused status lines, want 1", len(pubs))
	}
	if pubs[0].Detail == "" {
		t.Error("expected a non-empty refusal reason")
	}

	out := mreg.Format()
	if !strings.Contains(out, "lexa_mb_scan_refused_total 1") {
		t.Errorf("refused counter not incremented:\n%s", out)
	}
	if !strings.Contains(out, "lexa_mb_scan_runs_total 0") {
		t.Errorf("runs counter must stay at 0 on a refusal:\n%s", out)
	}
}

// ── 8. Result doc shape + full run (metrics, journal) ─────────────────────

func TestRunScan_EndToEnd(t *testing.T) {
	batteryHit := sunspec.SweepHit{
		URL: "tcp://10.0.0.5:502", UnitID: 1,
		Blocks: []sunspec.Block{{ModelID: 1}, {ModelID: 802}},
		Common: &sunspec.Common{Manufacturer: "Acme", Model: "BAT-1", Serial: "SN1", Version: "1.0"},
	}
	meterHit := sunspec.SweepHit{
		URL: "tcp://10.0.0.6:502", UnitID: 1,
		Blocks: []sunspec.Block{{ModelID: 203}},
	}
	withFakeSweeps(t, func(ctx context.Context, cidr string, port int, unitIDs []uint8, timeout time.Duration, hit func(sunspec.SweepHit)) error {
		hit(batteryHit)
		hit(meterHit)
		return nil
	}, nil)

	dir := t.TempDir()
	jw, err := journal.Open(journal.Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer jw.Close()

	fc := &fakeScanMQTTClient{}
	mreg := metrics.New()
	sc := newScanController(fc, &Config{}, jw, mreg)

	sc.runScan(bus.ScanRequest{ID: "scan-e2e", TCPCidr: "10.0.0.0/24"})
	if err := jw.Flush(); err != nil {
		t.Fatal(err)
	}

	// Final retained ScanResult.
	pubs := fc.snapshot()
	var resultPub *fakeScanPublish
	for i := range pubs {
		if pubs[i].topic == bus.TopicScanResult {
			resultPub = &pubs[i]
		}
	}
	if resultPub == nil {
		t.Fatal("no publish on bus.TopicScanResult")
	}
	if !resultPub.retained {
		t.Error("ScanResult must be published retained")
	}
	if !strings.Contains(string(resultPub.payload), `"v":1`) {
		t.Errorf("ScanResult payload missing schema version v:1: %s", resultPub.payload)
	}
	var result bus.ScanResult
	if err := json.Unmarshal(resultPub.payload, &result); err != nil {
		t.Fatalf("unmarshal ScanResult: %v", err)
	}
	if len(result.Devices) != 2 {
		t.Fatalf("got %d devices, want 2", len(result.Devices))
	}
	classes := map[string]bus.ScanHit{}
	for _, d := range result.Devices {
		classes[d.Class] = d
	}
	bat, ok := classes["battery"]
	if !ok {
		t.Fatal("no battery-classified hit in result")
	}
	if bat.Manufacturer != "Acme" || bat.Model != "BAT-1" || bat.Serial != "SN1" || bat.FwVersion != "1.0" {
		t.Errorf("battery hit identity fields = %+v, want Acme/BAT-1/SN1/1.0", bat)
	}
	if _, ok := classes["meter"]; !ok {
		t.Fatal("no meter-classified hit in result")
	}

	// Terminal "done" status.
	done := scanStatusPubs(fc, "scan-e2e", "done")
	if len(done) != 1 || done[0].Found != 2 {
		t.Fatalf("done status = %+v, want exactly one with Found=2", done)
	}
	// Identify phase between sweep and result.
	if identify := scanStatusPubs(fc, "scan-e2e", "identify"); len(identify) != 1 {
		t.Errorf("identify status lines = %d, want 1", len(identify))
	}

	// Metrics.
	out := mreg.Format()
	if !strings.Contains(out, "lexa_mb_scan_runs_total 1") {
		t.Errorf("runs counter not incremented:\n%s", out)
	}
	if !strings.Contains(out, "lexa_mb_scan_devices_found 2") {
		t.Errorf("devices_found gauge not set to 2:\n%s", out)
	}

	// Journal.
	data, err := os.ReadFile(filepath.Join(dir, journal.DefaultName))
	if err != nil {
		t.Fatal(err)
	}
	line := string(data)
	if !strings.Contains(line, `"type":"scan_run"`) {
		t.Errorf("journal missing scan_run event: %s", line)
	}
	if !strings.Contains(line, `"phase":"done"`) || !strings.Contains(line, `"devices_found":2`) {
		t.Errorf("journal scan_run payload missing phase=done/devices_found=2: %s", line)
	}
}

// TestScanController_JournalRun_NilWriterNoop covers the no-journal-configured
// default: journalRun must not panic when sc.jw is nil.
func TestScanController_JournalRun_NilWriterNoop(t *testing.T) {
	sc := newScanController(&fakeScanMQTTClient{}, &Config{}, nil, metrics.New())
	sc.journalRun("scan-noop", "done", 1, "")
}

// TestScanController_JournalRun_Refused pins the exact refused payload shape
// (docs/extension/00_PROGRESS.md 1.1+1.2 review ruling: ScanRun is
// {ID, Phase, DevicesFound, Detail} via NewScanRun/NewScanRunEvent).
func TestScanController_JournalRun_Refused(t *testing.T) {
	dir := t.TempDir()
	jw, err := journal.Open(journal.Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer jw.Close()

	sc := newScanController(&fakeScanMQTTClient{}, &Config{}, jw, metrics.New())
	sc.journalRun("scan-refused-1", "refused", 0, "scan already running")
	if err := jw.Flush(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, journal.DefaultName))
	if err != nil {
		t.Fatal(err)
	}
	line := string(data)
	for _, want := range []string{
		`"type":"scan_run"`,
		`"svc":"modbus"`,
		`"id":"scan-refused-1"`,
		`"phase":"refused"`,
		`"detail":"scan already running"`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("journal line missing %q: %s", want, line)
		}
	}
	// devices_found is 0 with omitempty — must NOT appear on a refusal.
	if strings.Contains(line, `"devices_found"`) {
		t.Errorf("refused scan_run must omit devices_found (omitempty, zero value): %s", line)
	}
}
