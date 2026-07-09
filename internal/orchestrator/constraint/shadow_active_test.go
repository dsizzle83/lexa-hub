package constraint

import (
	"math"
	"reflect"
	"testing"

	"lexa-hub/internal/orchestrator"
	model "lexa-proto/csipmodel"
)

// FIX-F (TASK-060 §4 / TASK-061 §4): active-mode composition tests. These
// prove the Wrapper's per-axis single-author rule — when a constraint is
// "active", the axes it wins this tick come from the CANDIDATE plan and
// legacy's own write to those same axes is DROPPED (legacyOverrideDropped),
// while every axis no active constraint touched stays legacy-authored and
// keeps being shadow-diffed exactly as before FIX-F.

// ── (a) one-author-per-axis ──────────────────────────────────────────────────

// THE named case (TASK-060 launch brief): applyRestoreRule idling a battery
// the ExportConstraint told to absorb must NOT leak through once export is
// active. Legacy stands in for the restore-rule bug directly — a battery
// re-idled to {0, connect=true} while the (real) ExportConstraint, driven by
// an active export cap with solar surplus, computes a genuine negative
// (absorb) setpoint for the SAME battery this tick.
func TestWrapper_ActiveComposition_RestoreRuleVsExportBatteryAbsorbDoesNotLeak(t *testing.T) {
	st := orchestrator.SystemState{
		Timestamp:   offPeakTime(),
		Solar:       []orchestrator.SolarState{{Name: "pv", PowerW: 6000, MaxW: 8000, Connected: true, Energized: true}},
		Batteries:   []orchestrator.BatteryState{{Name: "bat", PowerW: 0, SOC: 50, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true}},
		Grid:        orchestrator.GridState{NetW: -5500, ImportLimitW: math.NaN(), ExportLimitW: math.NaN(), MaxLimitW: math.NaN()},
		CSIPControl: expLimControl(1000),
	}
	candStack := NewStack(benchInput(st).Plant, 0, NewExportConstraint())

	// Legacy simulates the restore-rule leak: no legacy rule committed a
	// command for "bat" this tick, so applyRestoreRule idled it (SetpointW:0,
	// Connect:&true — optimizer.go:2229-2233), even though the export cap is
	// active and the pack has charge headroom.
	legacy := &stubOptimizer{plan: orchestrator.Plan{
		BatteryCommands: []orchestrator.BatteryCommand{{Name: "bat", SetpointW: 0, Connect: boolPtr(true)}},
	}}
	w := Wrap(legacy, candStack, Options{Now: newFixedClock().now, ActiveConstraints: map[string]bool{"export": true}})

	got := w.Optimize(st)

	var bat *orchestrator.BatteryCommand
	for i := range got.BatteryCommands {
		if got.BatteryCommands[i].Name == "bat" {
			bat = &got.BatteryCommands[i]
		}
	}
	if bat == nil {
		t.Fatal("composed plan has no command for bat")
	}
	if bat.SetpointW >= 0 {
		t.Fatalf("composed battery setpoint = %.0f, want a negative (absorb) value from ExportConstraint — legacy's restore-idle 0 leaked through", bat.SetpointW)
	}
	// Connect is NOT an axis ExportConstraint touches, so it must pass through
	// legacy's restore-rule value unchanged — composition is per-AXIS, not
	// per-command.
	if bat.Connect == nil || *bat.Connect != true {
		t.Fatalf("connect = %v, want legacy's true unchanged (export does not author the connect axis)", bat.Connect)
	}

	dropped := w.LegacyOverrideDropped()
	if dropped[AxisBatterySetpointW.String()] != 1 {
		t.Fatalf("legacyOverrideDropped[battery-setpoint-w] = %d, want 1 (the restore rule's idle write must be counted as dropped)", dropped[AxisBatterySetpointW.String()])
	}
	if dropped[AxisConnect.String()] != 0 {
		t.Fatalf("legacyOverrideDropped[connect] = %d, want 0 (connect was never active-owned)", dropped[AxisConnect.String()])
	}
}

// Solar-ceiling composition mechanism, isolated from ExportConstraint's own
// algorithm via the shadow_test.go ceilConstraint stand-in.
func TestWrapper_ActiveComposition_SolarCeilingOverwritesLegacyAndCountsDrop(t *testing.T) {
	legacy := &stubOptimizer{plan: orchestrator.Plan{
		SolarCommands: []orchestrator.SolarCommand{{Name: "s0", CurtailToW: 5000}},
	}}
	cand := NewStack(Plant{}, 0, ceilConstraint{name: "export", ceil: 1200})
	w := Wrap(legacy, cand, Options{Now: newFixedClock().now, ActiveConstraints: map[string]bool{"export": true}})

	got := w.Optimize(orchestrator.SystemState{
		Solar: []orchestrator.SolarState{{Name: "s0", Connected: true, PowerW: 5000, MaxW: 8000}},
	})
	if len(got.SolarCommands) != 1 || got.SolarCommands[0].CurtailToW != 1200 {
		t.Fatalf("composed solar = %+v, want candidate's 1200W ceiling", got.SolarCommands)
	}
	if n := w.LegacyOverrideDropped()[AxisSolarCeilingW.String()]; n != 1 {
		t.Fatalf("legacyOverrideDropped[solar-ceiling-w] = %d, want 1", n)
	}
}

// A constraint constructed into the candidate Stack but left in "shadow"
// (not in ActiveConstraints) must NOT compose — its axis stays legacy's, and
// the disagreement keeps being shadow-diffed. "Axes owned by non-active
// constraints keep coming from legacy" (deliverable 3).
func TestWrapper_ActiveComposition_NonActiveConstraintAxisStaysLegacyAndKeepsDiverging(t *testing.T) {
	legacy := &stubOptimizer{plan: orchestrator.Plan{
		SolarCommands: []orchestrator.SolarCommand{{Name: "s0", CurtailToW: 5000}},
	}}
	// "gen" is constructed (so it can express a shadow opinion) but is NOT in
	// ActiveConstraints — only "export" (which stays silent: no fakeConstraint
	// named "export" is wired) is active this tick.
	cand := NewStack(Plant{}, 0, ceilConstraint{name: "gen", ceil: 900})
	w := Wrap(legacy, cand, Options{
		Now:               newFixedClock().now,
		ActiveConstraints: map[string]bool{"export": true},
		OnDiverge:         (&collector{}).sink,
	})

	got := w.Optimize(orchestrator.SystemState{
		Solar: []orchestrator.SolarState{{Name: "s0", Connected: true, PowerW: 5000, MaxW: 8000}},
	})
	if len(got.SolarCommands) != 1 || got.SolarCommands[0].CurtailToW != 5000 {
		t.Fatalf("composed solar = %+v, want legacy's 5000W unchanged (gen is shadow, not active)", got.SolarCommands)
	}
	if n := w.LegacyOverrideDropped()[AxisSolarCeilingW.String()]; n != 0 {
		t.Fatalf("legacyOverrideDropped = %d, want 0 (no active constraint touched this axis)", n)
	}
	if w.Divergences() != 1 {
		t.Fatalf("divergences = %d, want 1 — gen's shadow opinion vs legacy must still be diffed (divergence diffing continues for axes still in shadow)", w.Divergences())
	}
}

// ── (b) TASK-061 §4: export+gen+import all active ───────────────────────────

// The shared battery-setpoint axis under a contradictory simultaneous
// import+export cap resolves to EXACTLY ONE author and EXACTLY ONE composed
// command — never a double-write, whichever way the arbiter's tier fold
// breaks the tie (importgen_arbitration_test.go's
// TestImportGen_SimultaneousCapArbitration pins the same non-parity-with-
// legacy gap at the arbiter level; this pins it through composition).
func TestWrapper_ActiveComposition_SharedBatteryAxisSingleAuthorUnderContradictoryCaps(t *testing.T) {
	st := orchestrator.SystemState{
		Solar: []orchestrator.SolarState{{Name: "pv", PowerW: 5000, MaxW: 5000, Connected: true, Energized: true}},
		Batteries: []orchestrator.BatteryState{{
			Name: "bat", PowerW: 0, SOC: 60, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true,
		}},
		Grid: orchestrator.GridState{NetW: -3000, ExportLimitW: 1000, ImportLimitW: 500, MaxLimitW: math.NaN()},
	}
	// Legacy stands in for the cascade's import-wins-and-neutralise shape: a
	// single discharge command distinct from whatever the candidate resolves.
	legacy := &stubOptimizer{plan: orchestrator.Plan{
		BatteryCommands: []orchestrator.BatteryCommand{{Name: "bat", SetpointW: 300}},
	}}
	cand := fullStack() // export + gen + import, per importgen_arbitration_test.go
	active := map[string]bool{"export": true, "gen": true, "import": true}
	w := Wrap(legacy, cand, Options{Now: newFixedClock().now, ActiveConstraints: active})

	got := w.Optimize(st)

	var batCmds int
	var setpoint float64
	for _, bc := range got.BatteryCommands {
		if bc.Name == "bat" {
			batCmds++
			setpoint = bc.SetpointW
		}
	}
	if batCmds != 1 {
		t.Fatalf("composed plan has %d commands for the shared battery axis, want exactly 1 (single-author)", batCmds)
	}
	author, ok := cand.AxisAuthors()[axisKey("bat", AxisBatterySetpointW)]
	if !ok || !active[author] {
		t.Fatalf("battery-setpoint-w author = %q (ok=%v), want exactly one ACTIVE constraint credited", author, ok)
	}
	if setpoint == 300 {
		t.Fatalf("composed setpoint is still legacy's 300W — the active constraint's write did not compose in")
	}
	if n := w.LegacyOverrideDropped()[AxisBatterySetpointW.String()]; n != 1 {
		t.Fatalf("legacyOverrideDropped[battery-setpoint-w] = %d, want 1", n)
	}
}

// On an IN-FAMILY scenario (a single, non-contradictory cap — the gen-only
// fixture from TestImportGen_GenCapUnaffectedByIdleSiblings, where export and
// import both express no opinion and only gen's absolute, unratcheted
// nameplate-share rule fires), the composed output must MATCH the legacy
// cascade's own output — the TASK-061 §4 acceptance requirement, distinct
// from the contradictory-cap test above which only pins single-authorship,
// not legacy parity (a documented, accepted gap).
func TestWrapper_ActiveComposition_MatchesLegacyCascadeOnInFamilyScenario(t *testing.T) {
	st := orchestrator.SystemState{
		Timestamp: offPeakTime(),
		Solar:     []orchestrator.SolarState{{Name: "pv", PowerW: 4000, MaxW: 5000, Connected: true, Energized: true}},
		Grid:      orchestrator.GridState{NetW: -3800, ExportLimitW: math.NaN(), ImportLimitW: math.NaN(), MaxLimitW: math.NaN()},
		CSIPControl: &orchestrator.CSIPControlState{
			Source: "event", MRID: "gen-only",
			Base: model.DERControlBase{OpModMaxLimW: &model.ActivePower{Value: 2000, Multiplier: 0}},
		},
	}
	refLegacyPlan := benchLegacy().Optimize(st)
	wantCeil := solarCeil(refLegacyPlan, "pv")
	if math.IsNaN(wantCeil) {
		t.Fatal("fixture sanity: the reference legacy cascade did not curtail — fixture is broken")
	}

	active := map[string]bool{"export": true, "gen": true, "import": true}
	w := Wrap(benchLegacy(), fullStack(), Options{Now: newFixedClock().now, ActiveConstraints: active})
	got := w.Optimize(st)

	gotCeil := solarCeil(got, "pv")
	tol := DefaultTolerances()
	if !tol.wattsAgree(wantCeil, gotCeil) {
		t.Fatalf("composed ceiling = %.0fW, legacy cascade = %.0fW (outside the shadow tolerance band) — active composition must match the legacy cascade on this in-family scenario", gotCeil, wantCeil)
	}
	if n := w.LegacyOverrideDropped()[AxisSolarCeilingW.String()]; n != 1 {
		t.Errorf("legacyOverrideDropped[solar-ceiling-w] = %d, want 1 (legacy's own gen-limit write must have been dropped in favour of the active one)", n)
	}
}

// ── (c) back-compat: config-path construction is bit-identical to pre-FIX-F ──

// The FIX-F config path's back-compat guarantee (cmd/hub/config.go
// ResolveConstraintModes: constraint_modes absent + constraint_shadow=true
// resolves every key to "shadow") never adds anything to ActiveConstraints —
// only an explicit "active" mode does (cmd/hub/main.go's per-constraint
// wiring). Reusing TestWrapper_BatterySetpointAndConnect's exact fixture
// through a Wrapper built with that (empty) active set must reproduce that
// pre-FIX-F test's exact outcome byte-for-byte.
func TestWrapper_BackCompat_AllShadowModesEqualsPreFixFWrapper(t *testing.T) {
	allShadowActiveSet := map[string]bool{} // what "every key -> shadow" resolves to

	col := &collector{}
	legacy := &stubOptimizer{plan: orchestrator.Plan{BatteryCommands: []orchestrator.BatteryCommand{
		{Name: "b0", SetpointW: 1000, Connect: boolPtr(true)},
	}}}
	cand := &stubOptimizer{plan: orchestrator.Plan{BatteryCommands: []orchestrator.BatteryCommand{
		{Name: "b0", SetpointW: 1100, Connect: boolPtr(false)},
	}}}
	w := Wrap(legacy, cand, Options{Now: newFixedClock().now, OnDiverge: col.sink, ActiveConstraints: allShadowActiveSet})
	got := w.Optimize(orchestrator.SystemState{})

	if !reflect.DeepEqual(got, legacy.plan) {
		t.Fatalf("config-path wrapper returned %+v, want the legacy plan unmodified %+v", got, legacy.plan)
	}
	if w.Divergences() != 1 || len(col.recs) != 1 {
		t.Fatalf("want 1 divergent tick exactly like the pre-FIX-F test, got count=%d recs=%d", w.Divergences(), len(col.recs))
	}
	axes := col.recs[0].Axes
	if len(axes) != 1 || axes[0].Axis != AxisConnect.String() || axes[0].Author != "" {
		t.Fatalf("want a single, unattributed connect divergence (stub candidate exposes no authorship), got %+v", axes)
	}
}

// Companion to TestWrapper_EmptyCandidateNeverDiverges: an empty active set
// against a real (empty) Stack candidate must still never diverge and never
// compose anything, across many ticks.
func TestWrapper_BackCompat_EmptyActiveSetNeverComposes(t *testing.T) {
	legacy := &stubOptimizer{plan: orchestrator.Plan{
		BatteryCommands: []orchestrator.BatteryCommand{{Name: "b0", SetpointW: 4000, Connect: boolPtr(false)}},
		SolarCommands:   []orchestrator.SolarCommand{{Name: "s0", CurtailToW: 1000}},
	}}
	stack := NewStack(Plant{}, 3000000000) // 3s tick, zero constraints
	col := &collector{}
	clk := newFixedClock()
	w := Wrap(legacy, stack, Options{Now: clk.now, OnDiverge: col.sink, ActiveConstraints: map[string]bool{}})

	for i := 0; i < 20; i++ {
		got := w.Optimize(orchestrator.SystemState{})
		if !reflect.DeepEqual(got, legacy.plan) {
			t.Fatalf("tick %d: composed plan = %+v, want legacy's unmodified", i, got)
		}
	}
	if w.Divergences() != 0 || len(w.LegacyOverrideDropped()) != 0 {
		t.Fatalf("empty active set against empty stack must never diverge or drop, got divergences=%d dropped=%v", w.Divergences(), w.LegacyOverrideDropped())
	}
}

// ── (d) panic latch fail-safes composition ───────────────────────────────────

// A candidate panic in active mode must fall back to the PURE legacy plan on
// every axis (not a partially-composed one) and flag ActiveFallbacks — the
// WS-5.1 latch extended to composition (deliverable 3's "the panic latch must
// also cover composition").
func TestWrapper_ActiveComposition_PanicFallsBackToPureLegacyAndFlagsMetric(t *testing.T) {
	legacy := &stubOptimizer{plan: orchestrator.Plan{
		BatteryCommands: []orchestrator.BatteryCommand{{Name: "bat", SetpointW: 500}},
		SolarCommands:   []orchestrator.SolarCommand{{Name: "pv", CurtailToW: 3000}},
	}}
	cand := &panicOptimizer{}
	var gotPanic any
	w := Wrap(legacy, cand, Options{
		Now:               newFixedClock().now,
		ActiveConstraints: map[string]bool{"export": true},
		OnPanic:           func(r any, _ []byte) { gotPanic = r },
	})

	got := w.Optimize(orchestrator.SystemState{}) // must not panic
	if !reflect.DeepEqual(got, legacy.plan) {
		t.Fatalf("composed plan after candidate panic = %+v, want the pure legacy plan %+v", got, legacy.plan)
	}
	if gotPanic == nil || w.Panics() != 1 || !w.Latched() {
		t.Fatalf("panic not latched: onPanic=%v panics=%d latched=%v", gotPanic, w.Panics(), w.Latched())
	}
	if w.ActiveFallbacks() != 1 {
		t.Fatalf("ActiveFallbacks = %d, want 1 (the panicking tick itself)", w.ActiveFallbacks())
	}

	// Subsequent ticks: latched, candidate never consulted again, every tick
	// still falls back to legacy and keeps counting.
	got2 := w.Optimize(orchestrator.SystemState{})
	if !reflect.DeepEqual(got2, legacy.plan) {
		t.Fatalf("post-latch composed plan = %+v, want pure legacy", got2)
	}
	if cand.calls != 1 {
		t.Fatalf("latched candidate consulted again: calls=%d", cand.calls)
	}
	if w.ActiveFallbacks() != 2 {
		t.Fatalf("ActiveFallbacks = %d, want 2 (a latched tick counts every time, not just the first)", w.ActiveFallbacks())
	}
	if n := len(w.LegacyOverrideDropped()); n != 0 {
		t.Errorf("legacyOverrideDropped should be empty (nothing was ever composed), got %v", w.LegacyOverrideDropped())
	}
}

// A candidate that is ALREADY latched (from an earlier shadow-mode panic,
// say) must never even be called once active mode is configured — every tick
// falls back immediately and ActiveFallbacks tracks it from the first active
// tick onward.
func TestWrapper_ActiveComposition_AlreadyLatchedNeverCallsCandidate(t *testing.T) {
	legacy := &stubOptimizer{plan: orchestrator.Plan{SolarCommands: []orchestrator.SolarCommand{{Name: "pv", CurtailToW: 3000}}}}
	cand := &panicOptimizer{}
	w := Wrap(legacy, cand, Options{Now: newFixedClock().now, ActiveConstraints: map[string]bool{"export": true}})

	// First tick trips the latch.
	w.Optimize(orchestrator.SystemState{})
	if !w.Latched() {
		t.Fatal("setup: expected the first tick to latch")
	}
	// Second and third ticks must not call the candidate again, and must keep
	// returning pure legacy while counting the fallback.
	for i := 0; i < 2; i++ {
		got := w.Optimize(orchestrator.SystemState{})
		if !reflect.DeepEqual(got, legacy.plan) {
			t.Fatalf("tick %d: got %+v, want pure legacy", i, got)
		}
	}
	if cand.calls != 1 {
		t.Fatalf("candidate called %d times, want 1 (only the tripping tick)", cand.calls)
	}
	if w.ActiveFallbacks() != 3 {
		t.Fatalf("ActiveFallbacks = %d, want 3 (one per tick since latching)", w.ActiveFallbacks())
	}
}
