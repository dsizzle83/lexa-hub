package main

// WP-9 tests: D7 arbitration table, per-archetype doc golden fixtures,
// rvrt_tms clamp, heartbeat/dedupe/harvest-rollback (mirroring
// desired_test.go's actuator tests), the flag-off absence property, and the
// ignored-mode alarm edge semantics.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/metrics"
)

const advFixedNow = int64(1_750_000_000)

// advTestEntries is the resolved curve content fixture: volt-var + volt-watt
// + freq-watt + two LVRT trip curves.
func advTestEntries() []bus.CurveSetEntry {
	return []bus.CurveSetEntry{
		{Mode: bus.CurveModeVoltVar, CurveType: 0, YRefType: 3,
			Points: []bus.CurvePoint{{X: 9200, Y: 3000}, {X: 10800, Y: -3000}}},
		{Mode: bus.CurveModeVoltWatt, CurveType: 3,
			Points: []bus.CurvePoint{{X: 10600, Y: 10000}, {X: 11000, Y: 2000}}},
		{Mode: bus.CurveModeFreqWatt, CurveType: 1,
			Points: []bus.CurvePoint{{X: 60200, Y: 10000}}},
		{Mode: bus.CurveModeLVRTMustTrip, CurveType: 9,
			Points: []bus.CurvePoint{{X: 5000, Y: 200}}},
		{Mode: bus.CurveModeLVRTMayTrip, CurveType: 7,
			Points: []bus.CurvePoint{{X: 7000, Y: 100}}},
	}
}

// advTestControl is a default-sourced control commanding: the curve modes
// above (via CurveSetID), a fixed-var (which volt-var must out-arbitrate),
// energize, the ramp gradients, and a 600 s ValidUntil (rvrt source).
func advTestControl(entries []bus.CurveSetEntry) bus.ActiveControl {
	fv := -25.0
	e := true
	g1, g2 := 0.3, 0.1
	return bus.ActiveControl{
		Envelope:     bus.Envelope{V: bus.ActiveControlV},
		Source:       "default",
		MRID:         "DEF-1",
		FixedVarPct:  &fv,
		Energize:     &e,
		SetGradW:     &g1,
		SetSoftGradW: &g2,
		ValidUntil:   advFixedNow + 600,
		CurveSetID:   bus.CurveSetContentHash(entries),
		Ts:           advFixedNow,
	}
}

func advTestCurves(entries []bus.CurveSetEntry) bus.CurveSet {
	return bus.CurveSet{
		Envelope: bus.Envelope{V: bus.CurveSetV},
		SetID:    bus.CurveSetContentHash(entries),
		MRID:     "DEF-1",
		Curves:   entries,
		Ts:       advFixedNow,
	}
}

func advTestSchedule() bus.DERScheduleMsg {
	return bus.DERScheduleMsg{
		Envelope: bus.Envelope{V: bus.DERScheduleV},
		Slots: []bus.DERScheduleSlot{{
			Start: advFixedNow, End: advFixedNow + 3600, Source: "default", MRID: "DEF-1",
			FreqDroop: &bus.FreqDroopMsg{DBuf: 36, DF: 3000, DP: 500, OpenLoopTms: 500, TResponse: 1000},
		}},
	}
}

func newTestAdvAuthor(t *testing.T, mc *fakeHubMQTTClient, devices []DeviceConfig, reg *metrics.Registry) *advAuthor {
	t.Helper()
	cfg := &Config{AdvancedDER: "on", Devices: devices}
	a := maybeNewAdvAuthor(mc, cfg,
		reg.Counter("lexa_hub_ignored_modes_total"),
		reg.Counter("lexa_hub_desired_adv_publishes_total"),
		reg.Counter("lexa_hub_desired_publish_failures_total"))
	if a == nil {
		t.Fatal("maybeNewAdvAuthor returned nil with advanced_der on")
	}
	a.now = func() time.Time { return time.Unix(advFixedNow, 0) }
	return a
}

// ─── D7 arbitration table ────────────────────────────────────────────────────

func rc(kind string, event bool) reactiveCandidate {
	return reactiveCandidate{kind: kind, eventSourced: event, mode: bus.AdvReactiveMode{Kind: kind}}
}

// TestArbitrateReactive_D7Table pins the D7 ordering: event-sourced over
// default-sourced, then voltVar > wattVar > fixedVar > fixedPF; every loser
// is returned as dropped.
func TestArbitrateReactive_D7Table(t *testing.T) {
	cases := []struct {
		name        string
		cands       []reactiveCandidate
		wantKind    string
		wantDropped []string
	}{
		{"none", nil, "", nil},
		{"single", []reactiveCandidate{rc(bus.AdvReactiveFixedPF, false)}, bus.AdvReactiveFixedPF, nil},
		{"voltvar-over-wattvar", []reactiveCandidate{rc(bus.AdvReactiveWattVar, false), rc(bus.AdvReactiveVoltVar, false)},
			bus.AdvReactiveVoltVar, []string{bus.AdvReactiveWattVar}},
		{"wattvar-over-fixedvar", []reactiveCandidate{rc(bus.AdvReactiveFixedVar, true), rc(bus.AdvReactiveWattVar, true)},
			bus.AdvReactiveWattVar, []string{bus.AdvReactiveFixedVar}},
		{"fixedvar-over-fixedpf", []reactiveCandidate{rc(bus.AdvReactiveFixedPF, false), rc(bus.AdvReactiveFixedVar, false)},
			bus.AdvReactiveFixedVar, []string{bus.AdvReactiveFixedPF}},
		// Event-sourced beats default-sourced even against a dynamically
		// higher-ranked mode (D7: source tier first, dynamic rank second).
		{"event-fixedpf-over-default-voltvar",
			[]reactiveCandidate{rc(bus.AdvReactiveVoltVar, false), rc(bus.AdvReactiveFixedPF, true)},
			bus.AdvReactiveFixedPF, []string{bus.AdvReactiveVoltVar}},
		{"all-four-same-source",
			[]reactiveCandidate{
				rc(bus.AdvReactiveFixedPF, true), rc(bus.AdvReactiveFixedVar, true),
				rc(bus.AdvReactiveWattVar, true), rc(bus.AdvReactiveVoltVar, true),
			},
			bus.AdvReactiveVoltVar,
			[]string{bus.AdvReactiveFixedPF, bus.AdvReactiveFixedVar, bus.AdvReactiveWattVar}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			winner, dropped := arbitrateReactive(tc.cands)
			if tc.wantKind == "" {
				if winner != nil {
					t.Fatalf("winner = %+v, want nil", winner)
				}
				return
			}
			if winner == nil || winner.kind != tc.wantKind {
				t.Fatalf("winner = %+v, want kind %q", winner, tc.wantKind)
			}
			var got []string
			for _, d := range dropped {
				got = append(got, d.kind)
			}
			if !reflect.DeepEqual(got, tc.wantDropped) {
				t.Fatalf("dropped = %v, want %v", got, tc.wantDropped)
			}
		})
	}
}

// TestBuildSiteAxes_MultiReactiveConflict: a control carrying volt-var +
// watt-var(watt-PF) curves + fixed-var + BOTH fixed-PF halves resolves to
// volt-var with every loser alarmed — including the absorb half's
// single-slot drop — and the overlays/trips pass through untouched.
func TestBuildSiteAxes_MultiReactiveConflict(t *testing.T) {
	entries := advTestEntries()
	entries = append(entries, bus.CurveSetEntry{
		Mode: bus.CurveModeWattPF, CurveType: 2,
		Points: []bus.CurvePoint{{X: 2000, Y: 9500}, {X: 10000, Y: 9000}},
	})
	ctrl := advTestControl(entries)
	ctrl.Source = "event"
	ctrl.MRID = "EVT-9"
	ctrl.FixedPFInject = &bus.FixedPF{PF: 0.95, OverExcited: true}
	ctrl.FixedPFAbsorb = &bus.FixedPF{PF: 0.9, OverExcited: false}

	byMode := map[string]bus.CurveSetEntry{}
	for _, e := range entries {
		byMode[e.Mode] = e
	}
	site, ignored := buildSiteAxes(&ctrl, byMode, nil, advFixedNow)

	if site.reactive == nil || site.reactive.Kind != bus.AdvReactiveVoltVar {
		t.Fatalf("reactive = %+v, want volt_var winner", site.reactive)
	}
	if site.voltWatt == nil || site.freqWatt == nil || site.trips == nil || site.trips.LV == nil {
		t.Fatalf("overlays/trips must pass through: volt_watt=%v freq_watt=%v trips=%+v",
			site.voltWatt, site.freqWatt, site.trips)
	}
	gotModes := map[string]bool{}
	for _, ig := range ignored {
		if ig.Scope != "site" || ig.MRID != "EVT-9" {
			t.Errorf("ignored entry scope/mrid = %q/%q, want site/EVT-9", ig.Scope, ig.MRID)
		}
		gotModes[ig.Mode] = true
	}
	for _, want := range []string{bus.AdvReactiveWattVar, bus.AdvReactiveFixedVar, bus.AdvReactiveFixedPF, "fixed_pf_absorb"} {
		if !gotModes[want] {
			t.Errorf("dropped mode %q not alarmed; got %v", want, gotModes)
		}
	}
	if len(ignored) != 4 {
		t.Errorf("ignored count = %d, want 4 (%v)", len(ignored), gotModes)
	}
	if site.source != "csip-event" {
		t.Errorf("source = %q, want csip-event", site.source)
	}
}

// ─── Golden docs per device archetype ────────────────────────────────────────

// TestAdvAuthor_GoldenDocs feeds one full (schedule, curves, control) input
// set to an author over the three archetypes — 7xx inverter (full axes),
// legacy-12x battery (reduced), unknown-capability device (empty + alarmed)
// — and pins each published doc byte-for-byte plus the ignored-mode count.
func TestAdvAuthor_GoldenDocs(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	reg := metrics.New()
	a := newTestAdvAuthor(t, mc, []DeviceConfig{
		{Name: "inv7", Role: "inverter", DERGen: "7xx"},
		{Name: "bat12", Role: "battery", DERGen: "12x"},
		{Name: "unk", Role: "inverter"}, // no der_gen: unknown capability
		{Name: "meter-0", Role: "meter"},
	}, reg)

	entries := advTestEntries()
	a.OnSchedule(advTestSchedule())
	a.OnCurves(advTestCurves(entries))
	if len(mc.publishes) != 0 {
		t.Fatalf("published before any control adopted: %d", len(mc.publishes))
	}
	a.OnControl(advTestControl(entries))

	if len(mc.publishes) != 3 {
		t.Fatalf("got %d publishes, want 3 (one per inverter/battery; none for the meter)", len(mc.publishes))
	}

	e := true
	g1, g2 := 0.3, 0.1
	rvrt := int64(600) // ValidUntil−serverNow, inside the clamp band
	droop := &bus.AdvFreqDroop{
		DbOfHz: float64(36) / 1000,
		DbUfHz: float64(36) / 1000,
		KOf:    (float64(3000) / 1000) / advNominalHz,
		KUf:    (float64(3000) / 1000) / advNominalHz,
		OlrtS:  float64(500) / 100,
	}
	trips := &bus.AdvTrips{LV: &bus.AdvTripSet{
		Curves: []bus.AdvTripCurve{
			{Kind: bus.AdvTripMustTrip, Curve: *advCurveFromEntry(entries[3])},
			{Kind: bus.AdvTripMayTrip, Curve: *advCurveFromEntry(entries[4])},
		},
		Hash: bus.CurveSetContentHash([]bus.CurveSetEntry{entries[3], entries[4]}),
	}}
	base := bus.DesiredAdvanced{
		Envelope: bus.Envelope{V: bus.DesiredAdvancedV},
		Source:   "csip-default",
		MRID:     "DEF-1",
		IssuedAt: advFixedNow,
		Seq:      0,
	}

	want := map[string]bus.DesiredAdvanced{}
	// 7xx inverter: FULL axes.
	full := base
	full.DeviceClass, full.DeviceID = bus.DesiredClassSolar, "inv7"
	full.ReactiveMode = &bus.AdvReactiveMode{Kind: bus.AdvReactiveVoltVar, Curve: advCurveFromEntry(entries[0])}
	full.VoltWatt = advCurveFromEntry(entries[1])
	full.FreqWatt = advCurveFromEntry(entries[2])
	full.FreqDroop = droop
	full.Trips = trips
	full.Energize = &e
	full.SetGradW, full.SetSoftGradW = &g1, &g2
	full.RvrtTmsS = &rvrt
	want[bus.DesiredAdvTopic("inv7")] = full
	// legacy-12x battery: curve axes only — no droop/energize/ramp/rvrt.
	reduced := base
	reduced.DeviceClass, reduced.DeviceID = bus.DesiredClassBattery, "bat12"
	reduced.ReactiveMode = &bus.AdvReactiveMode{Kind: bus.AdvReactiveVoltVar, Curve: advCurveFromEntry(entries[0])}
	reduced.VoltWatt = advCurveFromEntry(entries[1])
	reduced.FreqWatt = advCurveFromEntry(entries[2])
	reduced.Trips = trips
	want[bus.DesiredAdvTopic("bat12")] = reduced
	// unknown capability: every axis an explicit null / omitted scalar.
	empty := base
	empty.DeviceClass, empty.DeviceID = bus.DesiredClassSolar, "unk"
	want[bus.DesiredAdvTopic("unk")] = empty

	for _, p := range mc.publishes {
		w, ok := want[p.topic]
		if !ok {
			t.Errorf("unexpected publish topic %q", p.topic)
			continue
		}
		wantJSON, err := json.Marshal(w)
		if err != nil {
			t.Fatalf("marshal want: %v", err)
		}
		if string(p.payload) != string(wantJSON) {
			t.Errorf("golden mismatch on %s:\n got  %s\n want %s", p.topic, p.payload, wantJSON)
		}
		if !p.retained {
			t.Errorf("%s: desired docs must be retained", p.topic)
		}
		delete(want, p.topic)
	}
	if len(want) != 0 {
		t.Errorf("missing publishes for %v", want)
	}

	// Ignored-mode count: 1 site (fixed_var lost arbitration) + 4 for bat12
	// (droop/energize/ramp/rvrt unsupported) + 8 for unk (every commanded
	// axis) = 13, counted once per episode.
	if f := reg.Format(); !strings.Contains(f, "lexa_hub_ignored_modes_total 13") {
		t.Errorf("ignored-modes counter: want 13, metrics:\n%s", f)
	}
	// Edge semantics: a repeat evaluation of the SAME standing control must
	// not re-count.
	a.Evaluate()
	if f := reg.Format(); !strings.Contains(f, "lexa_hub_ignored_modes_total 13") {
		t.Errorf("ignored-modes counter re-counted on unchanged control:\n%s", f)
	}
}

// ─── rvrt_tms clamp ──────────────────────────────────────────────────────────

func TestComputeRvrtTmsS_ClampTable(t *testing.T) {
	cases := []struct {
		name       string
		validUntil int64
		want       *int64
	}{
		{"no-valid-until", 0, nil},
		{"below-floor", advFixedNow + 30, i64ptr(60)},
		{"already-expired", advFixedNow - 100, i64ptr(60)},
		{"in-band", advFixedNow + 600, i64ptr(600)},
		{"at-floor", advFixedNow + 60, i64ptr(60)},
		{"at-ceiling", advFixedNow + 24*3600, i64ptr(24 * 3600)},
		{"above-ceiling", advFixedNow + 100_000, i64ptr(24 * 3600)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeRvrtTmsS(tc.validUntil, advFixedNow)
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("got %d, want nil", *got)
			case tc.want != nil && (got == nil || *got != *tc.want):
				t.Fatalf("got %v, want %d", got, *tc.want)
			}
		})
	}
}

func i64ptr(v int64) *int64 { return &v }

// ─── Heartbeat / dedupe / harvest-rollback (desired_test.go mirrors) ─────────

// TestAdvAuthor_ContentChangeDedupeAndHeartbeat: unchanged content publishes
// nothing (the retained doc is standing intent), the WS-2 heartbeat re-stamps
// it after desiredHeartbeatInterval, a genuine change republishes at once,
// and a ticking rvrt countdown alone never counts as a content change.
func TestAdvAuthor_ContentChangeDedupeAndHeartbeat(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	reg := metrics.New()
	a := newTestAdvAuthor(t, mc, []DeviceConfig{{Name: "inv7", Role: "inverter", DERGen: "7xx"}}, reg)
	now := advFixedNow
	a.now = func() time.Time { return time.Unix(now, 0) }

	ctrl := advTestControl(nil) // no curves: CurveSetID "", scalars only
	ctrl.ValidUntil = advFixedNow + 7200
	a.OnControl(ctrl)
	if len(mc.publishes) != 1 {
		t.Fatalf("first control: got %d publishes, want 1", len(mc.publishes))
	}

	// Repeat evaluation, 60 s later: rvrt countdown moved (7200→7140) but the
	// content key excludes it — no republish before the heartbeat is due.
	now += 60
	a.Evaluate()
	if len(mc.publishes) != 1 {
		t.Fatalf("rvrt countdown alone republished: got %d publishes, want 1", len(mc.publishes))
	}

	// Past the heartbeat interval: unchanged content republishes verbatim
	// with fresh IssuedAt/Seq (and a fresh rvrt).
	now = advFixedNow + int64(desiredHeartbeatInterval/time.Second) + 1
	a.Evaluate()
	if len(mc.publishes) != 2 {
		t.Fatalf("heartbeat: got %d publishes, want 2", len(mc.publishes))
	}
	var d0, d1 bus.DesiredAdvanced
	if err := json.Unmarshal(mc.publishes[0].payload, &d0); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(mc.publishes[1].payload, &d1); err != nil {
		t.Fatal(err)
	}
	if d1.Seq != d0.Seq+1 || d1.IssuedAt <= d0.IssuedAt {
		t.Errorf("heartbeat stamps: seq %d→%d issued_at %d→%d", d0.Seq, d1.Seq, d0.IssuedAt, d1.IssuedAt)
	}
	d0.IssuedAt, d1.IssuedAt, d0.Seq, d1.Seq = 0, 0, 0, 0
	d0.RvrtTmsS, d1.RvrtTmsS = nil, nil
	if !reflect.DeepEqual(d0, d1) {
		t.Errorf("heartbeat changed content:\n first %+v\n heart %+v", d0, d1)
	}

	// A genuine content change republishes immediately.
	fv := -50.0
	ctrl.FixedVarPct = &fv
	a.OnControl(ctrl)
	if len(mc.publishes) != 3 {
		t.Fatalf("changed control: got %d publishes, want 3", len(mc.publishes))
	}
}

// TestAdvAuthor_HarvestRollback mirrors the actuator async-failure tests: a
// publish that fails (immediately, or resolved later while in flight) rolls
// the dedupe baseline back so the identical content republishes on the next
// evaluation, and counts a failure.
func TestAdvAuthor_HarvestRollback(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	reg := metrics.New()
	a := newTestAdvAuthor(t, mc, []DeviceConfig{{Name: "bat", Role: "battery", DERGen: "7xx"}}, reg)

	// (a) Already-resolved failure: the opportunistic post-fire harvest rolls
	// back immediately; the next evaluation retries the identical content.
	mc.failNext = true
	a.OnControl(advTestControl(nil))
	if len(mc.publishes) != 1 {
		t.Fatalf("failed publish still fires once: got %d", len(mc.publishes))
	}
	a.Evaluate()
	if len(mc.publishes) != 2 {
		t.Fatalf("rolled-back content must retry: got %d publishes, want 2", len(mc.publishes))
	}
	if string(mc.publishes[0].payload) != string(mc.publishes[1].payload) {
		t.Errorf("retry payload differs from original:\n %s\n %s", mc.publishes[0].payload, mc.publishes[1].payload)
	}
	if f := reg.Format(); !strings.Contains(f, "lexa_hub_desired_publish_failures_total 1") {
		t.Errorf("async failure not counted:\n%s", f)
	}

	// (b) Genuinely in-flight publish (latent token), resolved to an error
	// later: harvested on a subsequent evaluation, rolled back, retried.
	lt := newLatentToken()
	fv := -60.0
	ctrl := advTestControl(nil)
	ctrl.FixedVarPct = &fv
	mc.nextToken = lt
	a.OnControl(ctrl) // fires publish #3, stays pending
	if len(mc.publishes) != 3 {
		t.Fatalf("got %d publishes, want 3", len(mc.publishes))
	}
	a.Evaluate() // still in flight within budget: identical content suppressed
	if len(mc.publishes) != 3 {
		t.Fatalf("in-flight identical content must not double-publish: got %d", len(mc.publishes))
	}
	lt.resolve(errAdvTest)
	a.Evaluate() // harvest error → rollback → retry
	if len(mc.publishes) != 4 {
		t.Fatalf("resolved-error publish must retry: got %d publishes, want 4", len(mc.publishes))
	}
	if f := reg.Format(); !strings.Contains(f, "lexa_hub_desired_publish_failures_total 2") {
		t.Errorf("second async failure not counted:\n%s", f)
	}
}

var errAdvTest = &advTestError{}

type advTestError struct{}

func (*advTestError) Error() string { return "no ack (test)" }

// TestAdvAuthor_CurveSyncHold: a control naming a curve_set_id whose curves
// doc has not (yet) arrived publishes NOTHING (holding beats commanding a
// release the control never asked for); the matching curves doc unblocks it.
func TestAdvAuthor_CurveSyncHold(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	reg := metrics.New()
	a := newTestAdvAuthor(t, mc, []DeviceConfig{{Name: "inv7", Role: "inverter", DERGen: "7xx"}}, reg)

	entries := advTestEntries()
	a.OnControl(advTestControl(entries)) // CurveSetID set, curves missing
	if len(mc.publishes) != 0 {
		t.Fatalf("held evaluation published: %d", len(mc.publishes))
	}
	a.OnCurves(advTestCurves(entries)) // in sync now
	if len(mc.publishes) != 1 {
		t.Fatalf("got %d publishes after curve sync, want 1", len(mc.publishes))
	}
}

// ─── Flag-off absence + config validation ────────────────────────────────────

// TestAdvancedDERFlagOff_NoAuthorNoPublishes pins the releasability property:
// with advanced_der off (the default), the author is not even constructed —
// main.go's nil-gated hooks mean zero DesiredAdvanced publishes, byte-zero
// behavior change.
func TestAdvancedDERFlagOff_NoAuthorNoPublishes(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	cfg := &Config{AdvancedDER: "off", Devices: []DeviceConfig{{Name: "inv7", Role: "inverter", DERGen: "7xx"}}}
	adv := maybeNewAdvAuthor(mc, cfg, nil, nil, nil)
	if adv != nil {
		t.Fatal("author constructed with advanced_der off")
	}
	// Mirror main.go's gates exactly: every feed is `if adv != nil`-guarded.
	if adv != nil {
		adv.OnControl(advTestControl(nil))
		adv.OnCurves(advTestCurves(advTestEntries()))
		adv.OnSchedule(advTestSchedule())
	}
	if len(mc.publishes) != 0 {
		t.Fatalf("flag off must publish nothing, got %d", len(mc.publishes))
	}
}

func writeAdvTestConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hub.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestLoadConfig_AdvancedDERValidation: empty defaults to "off"; unknown
// values for advanced_der or der_gen are fatal config errors (never a silent
// fallback), mirroring the mode/reconciler validation discipline.
func TestLoadConfig_AdvancedDERValidation(t *testing.T) {
	cfg, err := loadConfig(writeAdvTestConfig(t, `{}`))
	if err != nil {
		t.Fatalf("empty config: %v", err)
	}
	if cfg.AdvancedDER != "off" {
		t.Errorf("default advanced_der = %q, want off", cfg.AdvancedDER)
	}

	if _, err := loadConfig(writeAdvTestConfig(t, `{"advanced_der":"shadow"}`)); err == nil {
		t.Error("unknown advanced_der accepted, want fatal config error")
	}
	if _, err := loadConfig(writeAdvTestConfig(t,
		`{"devices":[{"name":"inv","role":"inverter","der_gen":"70x"}]}`)); err == nil {
		t.Error("unknown der_gen accepted, want fatal config error")
	}
	cfg, err = loadConfig(writeAdvTestConfig(t,
		`{"advanced_der":"on","devices":[{"name":"inv","role":"inverter","der_gen":"7xx"},{"name":"b","role":"battery","der_gen":"12x"}]}`))
	if err != nil {
		t.Fatalf("valid advanced_der config rejected: %v", err)
	}
	if cfg.AdvancedDER != "on" || cfg.Devices[0].DERGen != "7xx" || cfg.Devices[1].DERGen != "12x" {
		t.Errorf("config not carried: %+v", cfg)
	}
}

// TestAdvAuthor_NoControlNoPublish: an author that has never adopted a
// control publishes nothing (no opinion), even across evaluations.
func TestAdvAuthor_NoControlNoPublish(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	a := newTestAdvAuthor(t, mc, []DeviceConfig{{Name: "inv7", Role: "inverter", DERGen: "7xx"}}, metrics.New())
	a.OnCurves(advTestCurves(advTestEntries()))
	a.OnSchedule(advTestSchedule())
	a.Evaluate()
	if len(mc.publishes) != 0 {
		t.Fatalf("published without a control: %d", len(mc.publishes))
	}
}

// TestAdvAuthor_NoneControlReleasesAllAxes: a Source "none" control (no
// active programs) publishes the all-null release doc — a superseded
// control's curves must not linger as standing intent.
func TestAdvAuthor_NoneControlReleasesAllAxes(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	a := newTestAdvAuthor(t, mc, []DeviceConfig{{Name: "inv7", Role: "inverter", DERGen: "7xx"}}, metrics.New())

	entries := advTestEntries()
	a.OnCurves(advTestCurves(entries))
	a.OnControl(advTestControl(entries))
	if len(mc.publishes) != 1 {
		t.Fatalf("got %d publishes, want 1", len(mc.publishes))
	}
	a.OnControl(bus.ActiveControl{Envelope: bus.Envelope{V: bus.ActiveControlV}, Source: "none", Ts: advFixedNow})
	if len(mc.publishes) != 2 {
		t.Fatalf("release doc not published: got %d, want 2", len(mc.publishes))
	}
	var doc bus.DesiredAdvanced
	if err := json.Unmarshal(mc.publishes[1].payload, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Source != "none" || doc.ReactiveMode != nil || doc.VoltWatt != nil ||
		doc.FreqWatt != nil || doc.FreqDroop != nil || doc.Trips != nil ||
		doc.Energize != nil || doc.RvrtTmsS != nil {
		t.Errorf("release doc must be all-null: %s", mc.publishes[1].payload)
	}
	for _, key := range []string{`"reactive_mode":null`, `"volt_watt":null`, `"freq_watt":null`, `"freq_droop":null`, `"trips":null`} {
		if !strings.Contains(string(mc.publishes[1].payload), key) {
			t.Errorf("release doc missing explicit null %s: %s", key, mc.publishes[1].payload)
		}
	}
}
