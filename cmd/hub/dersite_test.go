package main

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/metrics"
	model "lexa-proto/csipmodel"
)

// dersiteTestConfig is the canonical WP-4 test site: one 10 kW inverter, two
// batteries (5 kW / 3 kW), a meter, one EVSE station.
func dersiteTestConfig() *Config {
	return &Config{
		Devices: []DeviceConfig{
			{Name: "inverter-0", Role: "inverter", MaxW: 10000},
			{Name: "battery-0", Role: "battery", MaxW: 5000},
			{Name: "battery-1", Role: "battery", MaxW: 3000},
			{Name: "meter-0", Role: "meter"},
		},
		Stations: []StationConfig{{ID: "cs-001", MaxCurrentA: 32}},
	}
}

func newTestAggregator(t *testing.T, cfg *Config) (*dersiteAggregator, *fakeHubMQTTClient, *fakeClock) {
	t.Helper()
	mc := &fakeHubMQTTClient{}
	reg := metrics.New()
	a := newDersiteAggregator(mc, cfg, reg.Counter("lexa_hub_dersite_publishes_total"))
	clk := &fakeClock{t: time.Unix(1752480000, 0)}
	a.now = clk.now
	return a, mc, clk
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func fptr(v float64) *float64 { return &v }

func feedBattery(a *dersiteAggregator, name string, socPct, capWh, chargeW, dischargeW float64) {
	a.OnBattMetrics(bus.BattMetrics{
		Device:        name,
		SOC:           fptr(socPct),
		CapacityWh:    fptr(capWh),
		MaxChargeW:    fptr(chargeW),
		MaxDischargeW: fptr(dischargeW),
	})
}

// TestDersiteRatingsSums pins the D2 ratings arithmetic: inverter nameplate
// plus battery discharge in rtg_max_w; battery rate/capacity sums; live
// BattMetrics preferred over the config nameplate.
func TestDersiteRatingsSums(t *testing.T) {
	a, _, clk := newTestAggregator(t, dersiteTestConfig())
	feedBattery(a, "battery-0", 50, 10000, 5000, 5000)
	feedBattery(a, "battery-1", 50, 6000, 2500, 3000) // metrics charge rate < config MaxW

	a.mu.Lock()
	rep := a.buildReportLocked(clk.now())
	a.mu.Unlock()

	if rep.RtgMaxW != 10000+5000+3000 {
		t.Errorf("rtg_max_w = %v, want 18000 (inverter + Σ battery discharge)", rep.RtgMaxW)
	}
	if rep.RtgMaxChargeRateW != 5000+2500 {
		t.Errorf("rtg_max_charge_rate_w = %v, want 7500 (live metrics preferred)", rep.RtgMaxChargeRateW)
	}
	if rep.RtgMaxDischargeRateW != 5000+3000 {
		t.Errorf("rtg_max_discharge_rate_w = %v, want 8000", rep.RtgMaxDischargeRateW)
	}
	if rep.RtgMaxWh != 16000 {
		t.Errorf("rtg_max_wh = %v, want 16000", rep.RtgMaxWh)
	}
}

// TestDersiteRatingsFallBackToConfig pins the boot case: no BattMetrics seen
// yet, ratings come from the config nameplates (and plant capacity when
// present) so the capability PUT has stable data from the first publish.
func TestDersiteRatingsFallBackToConfig(t *testing.T) {
	cfg := dersiteTestConfig()
	cfg.Devices[1].BatteryPlant.CapacityKWh = 10 // battery-0 plant block
	a, _, clk := newTestAggregator(t, cfg)

	a.mu.Lock()
	rep := a.buildReportLocked(clk.now())
	a.mu.Unlock()

	if rep.RtgMaxChargeRateW != 5000+3000 {
		t.Errorf("rtg_max_charge_rate_w = %v, want 8000 (config max_w fallback)", rep.RtgMaxChargeRateW)
	}
	if rep.RtgMaxWh != 10000 {
		t.Errorf("rtg_max_wh = %v, want 10000 (plant capacity, battery-1 unknown)", rep.RtgMaxWh)
	}
}

// TestDersiteOmissionNotFabrication pins G27 on the aggregate: with no VA/
// Var rating source anywhere, the report's VA/Var fields stay nil — the
// wire carries no key (bus-level test) and the aggregator never invents one.
func TestDersiteOmissionNotFabrication(t *testing.T) {
	a, _, clk := newTestAggregator(t, dersiteTestConfig())
	feedBattery(a, "battery-0", 50, 10000, 5000, 5000)

	a.mu.Lock()
	rep := a.buildReportLocked(clk.now())
	a.mu.Unlock()

	if rep.RtgMaxVA != nil || rep.RtgMaxVar != nil || rep.SetMaxVA != nil || rep.SetMaxVar != nil {
		t.Errorf("VA/Var populated with no device data: %+v", rep)
	}
	if rep.Status.SocPct == nil {
		t.Fatal("SoC nil despite battery metrics")
	}
	// No fresh measurements at all: availability must not fabricate.
	if rep.Avail == nil {
		// Batteries with SOC+capacity DO make availability derivable.
		t.Fatal("avail nil despite derivable battery energy")
	}
	// But a site with nothing reporting derives nothing.
	b, _, clk2 := newTestAggregator(t, dersiteTestConfig())
	b.mu.Lock()
	rep2 := b.buildReportLocked(clk2.now())
	b.mu.Unlock()
	if rep2.Avail != nil {
		t.Errorf("avail fabricated with zero device data: %+v", rep2.Avail)
	}
	if rep2.Status.SocPct != nil {
		t.Errorf("SoC fabricated with zero battery data: %v", *rep2.Status.SocPct)
	}
}

// TestDersiteSocCapacityWeighting pins D2's capacity-weighted aggregate SoC
// over multiple packs, and the equal-weight fallback when a pack's capacity
// is unknown.
func TestDersiteSocCapacityWeighting(t *testing.T) {
	a, _, clk := newTestAggregator(t, dersiteTestConfig())
	// 10 kWh @ 80% and 6 kWh @ 20% → (8000+1200)/16000 = 57.5%
	feedBattery(a, "battery-0", 80, 10000, 5000, 5000)
	feedBattery(a, "battery-1", 20, 6000, 2500, 3000)

	a.mu.Lock()
	rep := a.buildReportLocked(clk.now())
	a.mu.Unlock()
	if rep.Status.SocPct == nil || *rep.Status.SocPct != 57.5 {
		t.Fatalf("capacity-weighted SoC = %v, want 57.5", rep.Status.SocPct)
	}

	// battery-1's capacity unknown → deterministic equal-weight fallback.
	b, _, clk2 := newTestAggregator(t, dersiteTestConfig())
	feedBattery(b, "battery-0", 80, 10000, 5000, 5000)
	b.OnBattMetrics(bus.BattMetrics{Device: "battery-1", SOC: fptr(20)})
	b.mu.Lock()
	rep2 := b.buildReportLocked(clk2.now())
	b.mu.Unlock()
	if rep2.Status.SocPct == nil || *rep2.Status.SocPct != 50 {
		t.Fatalf("equal-weight fallback SoC = %v, want 50", rep2.Status.SocPct)
	}
}

// TestDersiteSetLEQRtgInvariant pins CORE-014's ≤-by-construction settings:
// with a site policy tighter than the ratings, every set_* equals the
// policy; without one, set_* == rtg_* — and in both cases set ≤ rtg holds
// for every field.
func TestDersiteSetLEQRtgInvariant(t *testing.T) {
	a, _, clk := newTestAggregator(t, dersiteTestConfig())
	feedBattery(a, "battery-0", 50, 10000, 5000, 5000)
	feedBattery(a, "battery-1", 50, 6000, 2500, 3000)
	a.policy = dersitePolicy{
		MaxW:              fptr(12000),
		MaxChargeRateW:    fptr(99999), // looser than the rating — must NOT raise it
		MaxDischargeRateW: fptr(6000),
		MaxWh:             fptr(15000),
	}

	a.mu.Lock()
	rep := a.buildReportLocked(clk.now())
	a.mu.Unlock()

	if rep.SetMaxW != 12000 {
		t.Errorf("set_max_w = %v, want 12000 (policy clamp)", rep.SetMaxW)
	}
	if rep.SetMaxChargeRateW != rep.RtgMaxChargeRateW {
		t.Errorf("set_max_charge_rate_w = %v, want rating %v (policy looser)", rep.SetMaxChargeRateW, rep.RtgMaxChargeRateW)
	}
	if rep.SetMaxDischargeRateW != 6000 {
		t.Errorf("set_max_discharge_rate_w = %v, want 6000", rep.SetMaxDischargeRateW)
	}
	if rep.SetMaxWh != 15000 {
		t.Errorf("set_max_wh = %v, want 15000", rep.SetMaxWh)
	}
	for _, pair := range [][2]float64{
		{rep.SetMaxW, rep.RtgMaxW},
		{rep.SetMaxChargeRateW, rep.RtgMaxChargeRateW},
		{rep.SetMaxDischargeRateW, rep.RtgMaxDischargeRateW},
		{rep.SetMaxWh, rep.RtgMaxWh},
	} {
		if pair[0] > pair[1] {
			t.Errorf("set %v > rtg %v — CORE-014 invariant broken", pair[0], pair[1])
		}
	}
}

// TestDersiteTruthMask pins the D2 truth-mask derivation: exactly the five
// end-to-end-enforced scalar axes today, advanced bits OFF, and the
// data-driven table flipping bits without mask arithmetic.
func TestDersiteTruthMask(t *testing.T) {
	got := hubEnforcedModes().mask()
	want := model.ModeConnect | model.ModeMaxLimW | model.ModeFixedW | model.ModeExpLimW | model.ModeImpLimW
	if got != want {
		t.Fatalf("truth mask = %#x, want %#x", got, want)
	}
	advBits := model.ModeFixedVar | model.ModeFixedPFAbsorb | model.ModeFixedPFInject |
		model.ModeVoltVar | model.ModeFreqWatt | model.ModeVoltWatt | model.ModeFreqDroop
	if got&advBits != 0 {
		t.Fatalf("advanced bits set before WP-9/10: %#x", got&advBits)
	}

	// The WP-9/10 flip is one field: FixedPF covers both PF bits.
	e := hubEnforcedModes()
	e.FixedPF = true
	if m := e.mask(); m != want|model.ModeFixedPFAbsorb|model.ModeFixedPFInject {
		t.Fatalf("FixedPF flip mask = %#x", m)
	}
}

// TestDersiteDERTypeDerivation pins deriveDERType's D2 mix table and the
// config override.
func TestDersiteDERTypeDerivation(t *testing.T) {
	mix := func(roles ...string) []DeviceConfig {
		var out []DeviceConfig
		for i, r := range roles {
			out = append(out, DeviceConfig{Name: string(rune('a' + i)), Role: r})
		}
		return out
	}
	cases := []struct {
		name     string
		devices  []DeviceConfig
		override uint8
		want     uint8
	}{
		{"pv+storage", mix("inverter", "battery"), 0, bus.DERTypeStorage},
		{"storage only", mix("battery"), 0, bus.DERTypeStorage},
		{"pv only", mix("inverter", "meter"), 0, bus.DERTypePV},
		{"neither", mix("meter"), 0, bus.DERTypeVirtualOrMixed},
		{"override wins", mix("inverter", "battery"), 86, 86},
	}
	for _, tc := range cases {
		if got := deriveDERType(tc.devices, tc.override); got != tc.want {
			t.Errorf("%s: deriveDERType = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestDersiteStatusAndAlarms pins the aggregate status derivation: gen
// connect from fresh generation data, storage mode from summed battery sign,
// alarm categories through the shared 701→Table 14 mapping, and the breach
// level as EMERGENCY_REMOTE.
func TestDersiteStatusAndAlarms(t *testing.T) {
	a, _, clk := newTestAggregator(t, dersiteTestConfig())

	w := 4000.0
	alarm := alrm701OverFrequency | alrm701GroundFault // one mapped, one deliberately unmapped
	a.OnMeasurement(bus.Measurement{Device: "inverter-0", W: &w, AlarmBits: &alarm})
	chg := -2000.0
	a.OnMeasurement(bus.Measurement{Device: "battery-0", W: &chg})

	a.mu.Lock()
	st := a.buildStatusLocked(clk.now())
	a.mu.Unlock()

	if st.GenConnectStatus != model.GenConnectConnected {
		t.Errorf("gen_connect_status = %d, want connected", st.GenConnectStatus)
	}
	if st.OperationalMode != model.OpStatusOperating {
		t.Errorf("operational_mode = %d, want operating", st.OperationalMode)
	}
	if st.StorageMode == nil || *st.StorageMode != model.StorageCharging {
		t.Errorf("storage_mode = %v, want charging (net battery W < 0)", st.StorageMode)
	}
	if st.AlarmBits != bus.DERAlarmOverFrequency {
		t.Errorf("alarm_bits = %#x, want over-frequency only (ground fault unmapped)", st.AlarmBits)
	}

	a.SetBreachActive(true)
	a.mu.Lock()
	st = a.buildStatusLocked(clk.now())
	a.mu.Unlock()
	if st.AlarmBits&bus.DERAlarmEmergencyRemote == 0 {
		t.Error("breach episode not mirrored as EMERGENCY_REMOTE category")
	}

	// Staleness: a source silent past measStaleAfter no longer counts.
	clk.advance(measStaleAfter + time.Second)
	a.SetBreachActive(false)
	a.mu.Lock()
	st = a.buildStatusLocked(clk.now())
	a.mu.Unlock()
	if st.GenConnectStatus != model.GenConnectAvailable {
		t.Errorf("stale sources still read connected: %d", st.GenConnectStatus)
	}
	if st.StorageMode != nil {
		t.Errorf("storage_mode fabricated from stale data: %v", *st.StorageMode)
	}
}

// TestDersitePublishChangeDetectAndHeartbeat mirrors desired_test.go's
// dedupe/heartbeat pinning for the retained dersite doc: first evaluation
// publishes immediately; unchanged content publishes nothing until the
// heartbeat; changed content republishes only after the 60 s min interval;
// the heartbeat re-stamp carries an identical ContentHash.
func TestDersitePublishChangeDetectAndHeartbeat(t *testing.T) {
	a, mc, clk := newTestAggregator(t, dersiteTestConfig())
	feedBattery(a, "battery-0", 50, 10000, 5000, 5000)

	a.publishIfDue()
	if len(mc.publishes) != 1 {
		t.Fatalf("first eval: %d publishes, want 1 (immediate initial publish)", len(mc.publishes))
	}
	pub := mc.publishes[0]
	if pub.topic != bus.TopicHubDERSite || !pub.retained || pub.qos != 1 {
		t.Fatalf("publish shape: topic=%s retained=%v qos=%d", pub.topic, pub.retained, pub.qos)
	}
	var first bus.DERSiteReport
	if err := json.Unmarshal(pub.payload, &first); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if first.V != bus.DERSiteReportV {
		t.Fatalf("published v=%d, want %d", first.V, bus.DERSiteReportV)
	}

	// Unchanged content on the next eval: nothing. (The battery feed is
	// refreshed on each advance, as a live poller would — otherwise the
	// metrics age past measStaleAfter and the availability block dropping
	// out IS a legitimate content change.)
	clk.advance(dersiteEvalInterval)
	feedBattery(a, "battery-0", 50, 10000, 5000, 5000)
	a.publishIfDue()
	if len(mc.publishes) != 1 {
		t.Fatalf("unchanged content republished: %d", len(mc.publishes))
	}

	// Content change inside the 60 s window (SoC 50 → 80 at T0+6s): held...
	feedBattery(a, "battery-0", 80, 10000, 5000, 5000)
	clk.advance(time.Second)
	a.publishIfDue()
	if len(mc.publishes) != 1 {
		t.Fatalf("changed content republished inside min interval: %d", len(mc.publishes))
	}
	// ...and released once dersiteMinRepublish has elapsed since the last
	// publish.
	clk.advance(dersiteMinRepublish)
	feedBattery(a, "battery-0", 80, 10000, 5000, 5000)
	a.publishIfDue()
	if len(mc.publishes) != 2 {
		t.Fatalf("changed content not republished after min interval: %d", len(mc.publishes))
	}
	var second bus.DERSiteReport
	if err := json.Unmarshal(mc.publishes[1].payload, &second); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if second.Status.SocPct == nil || *second.Status.SocPct != 80 {
		t.Fatalf("republished SoC = %v, want 80", second.Status.SocPct)
	}
	// SoC is live status, not capability content: the cap-scoped hash must
	// NOT have moved (G29 — northbound must not re-PUT nameplate data).
	if second.ContentHash != first.ContentHash {
		t.Fatalf("cap-scoped ContentHash moved on a status-only change: %s → %s", first.ContentHash, second.ContentHash)
	}

	// Heartbeat: unchanged content re-published with a fresh Ts once
	// dersiteHeartbeat elapses (feed refreshed so content genuinely is
	// unchanged — this exercises the heartbeat branch, not change-detect).
	clk.advance(dersiteHeartbeat)
	feedBattery(a, "battery-0", 80, 10000, 5000, 5000)
	a.publishIfDue()
	if len(mc.publishes) != 3 {
		t.Fatalf("heartbeat republish missing: %d", len(mc.publishes))
	}
	var hb bus.DERSiteReport
	if err := json.Unmarshal(mc.publishes[2].payload, &hb); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if hb.ContentHash != second.ContentHash {
		t.Fatalf("heartbeat changed ContentHash: %s → %s", second.ContentHash, hb.ContentHash)
	}
	if hb.Ts <= second.Ts {
		t.Fatalf("heartbeat Ts not refreshed: %d then %d", second.Ts, hb.Ts)
	}
}

// TestDersitePublishFailureRollsBack pins the TASK-046 rollback contract: a
// publish whose token resolves to an error must roll the change-detect
// baseline back so the identical content is retried on a later evaluation.
func TestDersitePublishFailureRollsBack(t *testing.T) {
	a, mc, clk := newTestAggregator(t, dersiteTestConfig())
	mc.failNext = true

	a.publishIfDue()
	if len(mc.publishes) != 1 {
		t.Fatalf("publish not fired: %d", len(mc.publishes))
	}
	// The opportunistic harvest saw the failure and rolled back: the next
	// evaluation re-fires the same content immediately (first-publish path).
	clk.advance(dersiteEvalInterval)
	a.publishIfDue()
	if len(mc.publishes) != 2 {
		t.Fatalf("failed publish not retried: %d", len(mc.publishes))
	}
}

// TestDersiteCapHashScope pins which fields move the capability-scoped hash:
// ratings/settings/modes/der_type do; status, availability, and Ts do not.
func TestDersiteCapHashScope(t *testing.T) {
	base := bus.DERSiteReport{RtgMaxW: 1000, SetMaxW: 1000, ModesSupported: 7, DERType: 83}
	h := dersiteCapHash(base)

	statusChanged := base
	soc := 42.0
	statusChanged.Status.SocPct = &soc
	statusChanged.Ts = 999
	statusChanged.Avail = &bus.DERSiteAvailability{EstimatedWAvailW: fptr(100)}
	if dersiteCapHash(statusChanged) != h {
		t.Error("status/avail/Ts change moved the cap-scoped hash")
	}

	ratingChanged := base
	ratingChanged.RtgMaxW = 2000
	if dersiteCapHash(ratingChanged) == h {
		t.Error("rating change did not move the cap-scoped hash")
	}
	if math.Abs(float64(len(h))-16) > 0 {
		t.Errorf("hash length %d, want 16", len(h))
	}
}
