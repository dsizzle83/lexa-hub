package main

import (
	"encoding/json"
	"testing"
	"time"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/journal"
	"lexa-hub/internal/orchestrator"
)

// ---------------------------------------------------------------------
// Fakes (mode.go seams). Reuse intent_test.go's fakeHubMQTTClient / ptr.
// ---------------------------------------------------------------------

// sentinelPlan tags a Plan with a unique marker rule so a delegation test can
// prove WHICH author (or safety evaluator) produced the returned plan — the
// "a safety test that still passes with the guard unwired is not a test"
// standard: a bare "did EvaluateSafety return a plan" assertion would pass even
// if gateway mode wrongly routed safety through the gateway stack, so we make
// the plan's PROVENANCE observable.
func sentinelPlan(marker string) orchestrator.Plan {
	p := orchestrator.Plan{Timestamp: time.Unix(1_700_000_000, 0)}
	p.AddDecision(marker, "", "")
	return p
}

func planMarker(p orchestrator.Plan) string {
	if len(p.Decisions) == 0 {
		return ""
	}
	return p.Decisions[0].Rule
}

// fakeOptimizer returns a fixed sentinel plan and counts Optimize calls.
type fakeOptimizer struct {
	marker string
	calls  int
}

func (f *fakeOptimizer) Optimize(orchestrator.SystemState) orchestrator.Plan {
	f.calls++
	return sentinelPlan(f.marker)
}

// fakeSafety returns a fixed sentinel Safety plan from EvaluateSafety and counts
// calls — the injected stand-in for the raw *DefaultOptimizer.
type fakeSafety struct {
	marker string
	calls  int
}

func (f *fakeSafety) EvaluateSafety(orchestrator.SystemState) orchestrator.Plan {
	f.calls++
	p := sentinelPlan(f.marker)
	p.Safety = true
	return p
}

// fakeWaker records Wake() calls (the engineWaker seam).
type fakeWaker struct{ wakes int }

func (f *fakeWaker) Wake() { f.wakes++ }

// testModeFixture bundles a modeManager with its fakes for assertions.
type testModeFixture struct {
	m         *modeManager
	optAuthor *fakeOptimizer
	gwAuthor  *fakeOptimizer
	safety    *fakeSafety
	mc        *fakeHubMQTTClient
	waker     *fakeWaker
}

// newTestModeFixture wires a modeManager to fakes with a deterministic clock.
// jw may be nil (journaling disabled, hub.json default) or a real writer on a
// t.TempDir() when a test inspects journaled events.
func newTestModeFixture(t *testing.T, initialMode string, jw *journal.Writer) *testModeFixture {
	t.Helper()
	optAuthor := &fakeOptimizer{marker: "optimizer-author"}
	gwAuthor := &fakeOptimizer{marker: "gateway-author"}
	safety := &fakeSafety{marker: "legacy-safety"}
	mc := &fakeHubMQTTClient{}
	waker := &fakeWaker{}
	m := newModeManager(initialMode, optAuthor, gwAuthor, safety, jw, mc, nil)
	m.setEngine(waker)
	fixedNow := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return fixedNow }
	return &testModeFixture{m: m, optAuthor: optAuthor, gwAuthor: gwAuthor, safety: safety, mc: mc, waker: waker}
}

// ---------------------------------------------------------------------
// THE load-bearing invariant: Tier-1 safety is mode-invariant.
// ---------------------------------------------------------------------

// TestModeManager_EvaluateSafety_ModeInvariant is the mutation-check: in BOTH
// modes, EvaluateSafety must return the INJECTED safety evaluator's sentinel
// plan — never the gateway author's. The gateway author's Optimize must not be
// called at all during a safety pass. If the delegation were wrongly wired
// through the gateway stack in gateway mode (the exact foot-gun this unit
// exists to prevent), the returned marker would not be the safety sentinel and
// this test would fail.
func TestModeManager_EvaluateSafety_ModeInvariant(t *testing.T) {
	for _, mode := range []string{"optimizer", "gateway"} {
		t.Run(mode, func(t *testing.T) {
			f := newTestModeFixture(t, mode, nil)
			plan := f.m.EvaluateSafety(orchestrator.SystemState{})
			if got := planMarker(plan); got != "legacy-safety" {
				t.Fatalf("mode %q: EvaluateSafety marker = %q, want the legacy safety sentinel — Tier-1 must be mode-invariant", mode, got)
			}
			if !plan.Safety {
				t.Errorf("mode %q: returned plan must have Safety=true", mode)
			}
			if f.safety.calls != 1 {
				t.Errorf("mode %q: safety evaluator called %d times, want 1", mode, f.safety.calls)
			}
			if f.gwAuthor.calls != 0 {
				t.Errorf("mode %q: gateway author Optimize called %d times during a safety pass, want 0", mode, f.gwAuthor.calls)
			}
			if f.optAuthor.calls != 0 {
				t.Errorf("mode %q: optimizer author Optimize called %d times during a safety pass, want 0", mode, f.optAuthor.calls)
			}
		})
	}
}

// TestNewModeManager_NilSafetyPanics pins the construct-time assert: a nil
// safety evaluator is a programming error (the protective reflex would have no
// author), so construction must panic rather than defer the failure to the
// first safety tick.
func TestNewModeManager_NilSafetyPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("newModeManager with nil safety must panic")
		}
	}()
	newModeManager("optimizer", &fakeOptimizer{}, &fakeOptimizer{}, nil, nil, &fakeHubMQTTClient{}, nil)
}

// ---------------------------------------------------------------------
// Optimize delegation table.
// ---------------------------------------------------------------------

func TestModeManager_Optimize_Delegation(t *testing.T) {
	cases := []struct {
		mode       string
		wantMarker string
	}{
		{"optimizer", "optimizer-author"},
		{"gateway", "gateway-author"},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			f := newTestModeFixture(t, tc.mode, nil)
			plan := f.m.Optimize(orchestrator.SystemState{})
			if got := planMarker(plan); got != tc.wantMarker {
				t.Fatalf("mode %q: Optimize marker = %q, want %q", tc.mode, got, tc.wantMarker)
			}
			// The OTHER author must not be invoked.
			if tc.mode == "optimizer" && f.gwAuthor.calls != 0 {
				t.Errorf("optimizer mode called the gateway author %d times", f.gwAuthor.calls)
			}
			if tc.mode == "gateway" && f.optAuthor.calls != 0 {
				t.Errorf("gateway mode called the optimizer author %d times", f.optAuthor.calls)
			}
		})
	}
}

// ---------------------------------------------------------------------
// request(): duplicate vs applied, journal-before-flip, ModeStatus shape, Wake.
// ---------------------------------------------------------------------

// TestModeManager_Request_DuplicateIsNoop: requesting the mode already in effect
// is a "duplicate" with no side effects — no flip, no journal, no publish, no
// Wake.
func TestModeManager_Request_DuplicateIsNoop(t *testing.T) {
	dir := t.TempDir()
	jw, err := journal.Open(journal.Config{Dir: dir})
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	defer jw.Close()

	f := newTestModeFixture(t, "optimizer", jw)
	outcome, _ := f.m.request("optimizer", bus.IntentMeta{ID: "d1"})
	if outcome != "duplicate" {
		t.Fatalf("outcome = %q, want duplicate", outcome)
	}
	if f.m.Mode() != "optimizer" {
		t.Errorf("mode = %q, want unchanged optimizer", f.m.Mode())
	}
	if len(f.mc.publishes) != 0 {
		t.Errorf("publishes = %d, want 0 (a duplicate must not publish ModeStatus)", len(f.mc.publishes))
	}
	if f.waker.wakes != 0 {
		t.Errorf("wakes = %d, want 0", f.waker.wakes)
	}
	if err := jw.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := len(journalEventsByType(t, dir)[journal.TypeModeChange]); got != 0 {
		t.Errorf("mode_change events = %d, want 0 for a duplicate", got)
	}
}

// TestModeManager_Request_AppliedFlipsAndAnnounces covers the whole applied
// path: mode flips, mode_change is journaled with the PRE-flip From (proving it
// captured the transition endpoints correctly), a retained ModeStatus {v:1} is
// published on lexa/hub/mode with the requesting actor/intent, and eng.Wake is
// poked.
func TestModeManager_Request_AppliedFlipsAndAnnounces(t *testing.T) {
	dir := t.TempDir()
	jw, err := journal.Open(journal.Config{Dir: dir})
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	defer jw.Close()

	f := newTestModeFixture(t, "optimizer", jw)
	meta := bus.IntentMeta{ID: "m1", Actor: "user@example.com", Origin: "app"}
	outcome, detail := f.m.request("gateway", meta)
	if outcome != "applied" || detail != "" {
		t.Fatalf("outcome/detail = %q/%q, want applied/\"\"", outcome, detail)
	}
	if f.m.Mode() != "gateway" {
		t.Fatalf("mode = %q, want gateway after applied request", f.m.Mode())
	}
	if f.waker.wakes != 1 {
		t.Errorf("wakes = %d, want 1 (the new author must run on the next tick)", f.waker.wakes)
	}

	// Journal: exactly one mode_change, endpoints old→new.
	if err := jw.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	evs := journalEventsByType(t, dir)[journal.TypeModeChange]
	if len(evs) != 1 {
		t.Fatalf("mode_change events = %d, want 1", len(evs))
	}
	var mc journal.ModeChange
	if err := json.Unmarshal(evs[0].Data, &mc); err != nil {
		t.Fatalf("unmarshal ModeChange: %v", err)
	}
	if mc.From != "optimizer" || mc.To != "gateway" {
		t.Errorf("mode_change From/To = %q/%q, want optimizer/gateway (must record the pre-flip From)", mc.From, mc.To)
	}
	if mc.Actor != "user@example.com" || mc.Origin != "app" || mc.IntentID != "m1" {
		t.Errorf("mode_change actor/origin/intent = %q/%q/%q, want the request meta", mc.Actor, mc.Origin, mc.IntentID)
	}

	// Retained ModeStatus {v:1} on lexa/hub/mode.
	if len(f.mc.publishes) != 1 {
		t.Fatalf("publishes = %d, want 1 (the retained ModeStatus)", len(f.mc.publishes))
	}
	pub := f.mc.publishes[0]
	if pub.topic != bus.TopicHubMode {
		t.Errorf("topic = %q, want %q", pub.topic, bus.TopicHubMode)
	}
	if !pub.retained {
		t.Errorf("ModeStatus must be retained (the hub's own restart re-seed)")
	}
	var st bus.ModeStatus
	if err := json.Unmarshal(pub.payload, &st); err != nil {
		t.Fatalf("unmarshal ModeStatus: %v", err)
	}
	if st.V != bus.ModeStatusV {
		t.Errorf("ModeStatus v = %d, want %d", st.V, bus.ModeStatusV)
	}
	if bus.ModeStatusV != 1 {
		t.Errorf("ModeStatusV = %d, want 1 (schema {v:1})", bus.ModeStatusV)
	}
	if st.Mode != "gateway" || st.Actor != "user@example.com" || st.IntentID != "m1" {
		t.Errorf("ModeStatus = %+v, want mode=gateway actor=user@example.com intent_id=m1", st)
	}
	if st.Since == 0 || st.Ts == 0 {
		t.Errorf("ModeStatus Since/Ts must be stamped, got %d/%d", st.Since, st.Ts)
	}
}

// ---------------------------------------------------------------------
// Boot re-seed precedence: retained ▸ cfg ▸ default; post-Start ignored.
// ---------------------------------------------------------------------

func TestModeManager_BootReseed_Precedence(t *testing.T) {
	// (a) retained value overrides the config-derived starting mode.
	t.Run("retained_beats_cfg", func(t *testing.T) {
		f := newTestModeFixture(t, "optimizer", nil) // cfg says optimizer
		f.m.onModeStatus(bus.ModeStatus{Envelope: bus.Envelope{V: bus.ModeStatusV}, Mode: "gateway"})
		if f.m.Mode() != "gateway" {
			t.Errorf("mode = %q, want gateway (retained beats cfg)", f.m.Mode())
		}
		// Boot adoption is SILENT: no journal (nil jw here), no publish, no Wake.
		if len(f.mc.publishes) != 0 {
			t.Errorf("boot re-seed published %d messages, want 0 (silent adoption)", len(f.mc.publishes))
		}
		if f.waker.wakes != 0 {
			t.Errorf("boot re-seed called Wake %d times, want 0 (engine not started)", f.waker.wakes)
		}
	})

	// (b) with no retained message, the config-derived mode stands (beats the
	// "optimizer" fallback default).
	t.Run("cfg_beats_default", func(t *testing.T) {
		f := newTestModeFixture(t, "gateway", nil)
		if f.m.Mode() != "gateway" {
			t.Errorf("mode = %q, want gateway (cfg value, no retained message)", f.m.Mode())
		}
	})

	// (c) a corrupt/empty retained value during boot is ignored (keeps cfg).
	t.Run("corrupt_retained_ignored", func(t *testing.T) {
		f := newTestModeFixture(t, "optimizer", nil)
		f.m.onModeStatus(bus.ModeStatus{Mode: "bogus"})
		f.m.onModeStatus(bus.ModeStatus{Mode: ""})
		if f.m.Mode() != "optimizer" {
			t.Errorf("mode = %q, want optimizer (corrupt retained value must not flip)", f.m.Mode())
		}
	})

	// (d) after SealBoot, later messages on lexa/hub/mode are IGNORED — the hub
	// is the sole writer; a forged/echoed status must not flip the mode.
	t.Run("post_seal_ignored", func(t *testing.T) {
		f := newTestModeFixture(t, "optimizer", nil)
		f.m.SealBoot()
		f.m.onModeStatus(bus.ModeStatus{Mode: "gateway"})
		if f.m.Mode() != "optimizer" {
			t.Errorf("mode = %q, want optimizer (post-seal message must be ignored)", f.m.Mode())
		}
	})
}

// ---------------------------------------------------------------------
// Config: mode validation + gateway block defaults.
// ---------------------------------------------------------------------

func TestLoadConfig_ModeValidation(t *testing.T) {
	cases := []struct {
		name    string
		json    string
		wantErr bool
		wantVal string
	}{
		{"absent_defaults_optimizer", `{"mqtt_broker":"tcp://localhost:1883"}`, false, "optimizer"},
		{"explicit_optimizer", `{"mode":"optimizer"}`, false, "optimizer"},
		{"explicit_gateway", `{"mode":"gateway"}`, false, "gateway"},
		{"unknown_is_fatal", `{"mode":"turbo"}`, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempConfig(t, tc.json)
			cfg, err := loadConfig(path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("loadConfig(%s) = nil error, want a fatal error on unknown mode", tc.json)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if cfg.Mode != tc.wantVal {
				t.Errorf("cfg.Mode = %q, want %q", cfg.Mode, tc.wantVal)
			}
		})
	}
}

func TestConfig_GatewayPolicy_Defaults(t *testing.T) {
	// Absent block ⇒ the constraint defaults (scheduled 23→7, 32 A).
	c := &Config{}
	p := c.GatewayPolicy()
	if p.Mode != "scheduled" || p.WindowStartHH != 23 || p.WindowEndHH != 7 || p.FullCurrentA != 32 {
		t.Errorf("absent-block GatewayPolicy = %+v, want scheduled 23→7 32A", p)
	}
}

func TestConfig_GatewayPolicy_ExplicitBlockParses(t *testing.T) {
	path := writeTempConfig(t, `{
		"mode": "gateway",
		"gateway": {"evse_policy":"full","evse_window":{"start_hh":1,"end_hh":5},"evse_full_a":16}
	}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	p := cfg.GatewayPolicy()
	if p.Mode != "full" || p.WindowStartHH != 1 || p.WindowEndHH != 5 || p.FullCurrentA != 16 {
		t.Errorf("GatewayPolicy = %+v, want full 1→5 16A", p)
	}
}
