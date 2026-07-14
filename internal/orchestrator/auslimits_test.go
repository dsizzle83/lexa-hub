package orchestrator

// WP-11 cascade tests: the CSIP-AUS gross-generation (opModGenLimW) and
// gross-load (opModLoadLimW) rules plus their convergence backstops
// (auslimits.go), and the flag-off byte-identity pin. The mirrored SHADOW
// suite lives in orchestrator/constraint/{genlimaus,loadlimaus}_test.go —
// per the do-not-strip rule, both suites exist and neither replaces the other.

import (
	"math"
	"testing"
	"time"
)

// ausState builds a SystemState with the given AUS caps in GridState (NaN =
// absent) and everything else NaN-safe.
func ausState(ts time.Time, genLimW, loadLimW, netW float64) SystemState {
	g := NewGridState()
	g.NetW = netW
	g.GenLimitW = genLimW
	g.LoadLimitW = loadLimW
	return SystemState{Timestamp: ts, Grid: g}
}

// plansEqual compares two plans field-by-field with NaN-aware float equality
// (reflect.DeepEqual treats NaN != NaN, and plans legitimately carry NaN in
// CurtailToW/SetpointW). Decisions are compared by COUNT only, per the
// TASK-056 convention that decision wording is never load-bearing.
func plansEqual(a, b Plan) bool {
	feq := func(x, y float64) bool {
		return (math.IsNaN(x) && math.IsNaN(y)) || x == y
	}
	beq := func(x, y *bool) bool {
		if (x == nil) != (y == nil) {
			return false
		}
		return x == nil || *x == *y
	}
	if len(a.BatteryCommands) != len(b.BatteryCommands) ||
		len(a.SolarCommands) != len(b.SolarCommands) ||
		len(a.EVSECommands) != len(b.EVSECommands) ||
		len(a.Decisions) != len(b.Decisions) {
		return false
	}
	for i := range a.BatteryCommands {
		x, y := a.BatteryCommands[i], b.BatteryCommands[i]
		if x.Name != y.Name || !feq(x.SetpointW, y.SetpointW) || !beq(x.Connect, y.Connect) || x.MRID != y.MRID {
			return false
		}
	}
	for i := range a.SolarCommands {
		x, y := a.SolarCommands[i], b.SolarCommands[i]
		if x.Name != y.Name || !feq(x.CurtailToW, y.CurtailToW) || !beq(x.Connect, y.Connect) || x.MRID != y.MRID {
			return false
		}
	}
	for i := range a.EVSECommands {
		x, y := a.EVSECommands[i], b.EVSECommands[i]
		if x.StationID != y.StationID || x.ConnectorID != y.ConnectorID ||
			!feq(x.MaxCurrentA, y.MaxCurrentA) || !beq(x.Connect, y.Connect) || x.MRID != y.MRID {
			return false
		}
	}
	if (a.Breach == nil) != (b.Breach == nil) {
		return false
	}
	if a.Breach != nil && *a.Breach != *b.Breach {
		return false
	}
	return true
}

// ── flag-off byte-identity ────────────────────────────────────────────────────

// With enforce_aus_limits OFF (the shipped default), plans must be identical
// whether or not GridState carries AUS limits — adoption without enforcement
// changes nothing. This is the golden no-change pin every flag in this repo
// carries (the ev_storage/constraint_shadow "byte-identical plans" contract).
func TestAusLimits_FlagOffByteIdenticalPlans(t *testing.T) {
	mkOpt := func() *DefaultOptimizer {
		o := NewDefaultOptimizer()
		o.CostModel = DefaultTOUCostModel()
		return o
	}
	withCaps := mkOpt()
	without := mkOpt()

	peak := time.Date(2026, 7, 6, 17, 0, 0, 0, time.Local)   // 17:00 → TOU peak
	offPeak := time.Date(2026, 7, 6, 3, 0, 0, 0, time.Local) // 03:00 → off-peak
	midday := time.Date(2026, 7, 6, 12, 0, 0, 0, time.Local) // partial-peak
	scenarios := []func(genLimW, loadLimW float64) SystemState{
		// Solar surplus, battery charging headroom (self-consumption path).
		func(gen, load float64) SystemState {
			st := ausState(midday, gen, load, -3000)
			st.Solar = []SolarState{ruleSol("pv", 4000)}
			st.Batteries = []BatteryState{ruleBat("bat", 0, 50, 5000)}
			return st
		},
		// TOU peak discharge path.
		func(gen, load float64) SystemState {
			st := ausState(peak, gen, load, 500)
			st.Batteries = []BatteryState{ruleBat("bat", 0, 80, 4000)}
			return st
		},
		// EV charging with no other constraint.
		func(gen, load float64) SystemState {
			st := ausState(offPeak, gen, load, 2000)
			st.EVSEs = []EVSEState{ruleEVSE("cs1", true, 32, 230)}
			return st
		},
		// Export cap active alongside the (unenforced) AUS caps.
		func(gen, load float64) SystemState {
			st := ausState(midday, gen, load, -4000)
			st.Grid.ExportLimitW = 1000
			st.Solar = []SolarState{ruleSol("pv", 5000)}
			st.Batteries = []BatteryState{ruleBat("bat", 0, 96, 5000)}
			return st
		},
	}

	for i, mk := range scenarios {
		// Tight AUS caps that WOULD bite if enforcement leaked past the flag.
		pa := withCaps.Optimize(mk(500, 500))
		pb := without.Optimize(mk(math.NaN(), math.NaN()))
		if !plansEqual(pa, pb) {
			t.Fatalf("scenario %d: flag-off plans differ with AUS caps present:\nwith:    %+v\nwithout: %+v", i, pa, pb)
		}
	}
}

// ── gross-generation rule ─────────────────────────────────────────────────────

// A gross-generation cap must ceiling the inverters at the cap (nameplate-
// distributed) when no battery discharge is committed.
func TestAusGenRule_CurtailsSolarToGrossCap(t *testing.T) {
	o := NewDefaultOptimizer()
	sol := []SolarState{
		{Name: "pvA", PowerW: 3000, MaxW: 6000, Connected: true, Energized: true},
		{Name: "pvB", PowerW: 1000, MaxW: 2000, Connected: true, Energized: true},
	}
	p := &Plan{}
	_, touCap := o.applyAusGenerationLimitRule(sol, nil, 3000, p)
	// 3000 W cap over 8000 W nameplate: pvA 2250, pvB 750.
	if i := solarCommandIndex(p.SolarCommands, "pvA"); i < 0 || math.Abs(p.SolarCommands[i].CurtailToW-2250) > 1 {
		t.Errorf("pvA ceiling = %+v, want 2250", p.SolarCommands)
	}
	if i := solarCommandIndex(p.SolarCommands, "pvB"); i < 0 || math.Abs(p.SolarCommands[i].CurtailToW-750) > 1 {
		t.Errorf("pvB ceiling = %+v, want 750", p.SolarCommands)
	}
	// Measured solar 4000 ≥ cap 3000 → zero discharge headroom for Rule 5.
	if touCap != 0 {
		t.Errorf("TOU discharge cap = %v, want 0 (solar saturates the gross cap)", touCap)
	}
}

// Battery discharge committed by an earlier rule must be trimmed into the
// headroom measured solar leaves, and the solar ceiling must account for the
// discharge that survives.
func TestAusGenRule_TrimsCommittedDischarge(t *testing.T) {
	o := NewDefaultOptimizer()
	sol := []SolarState{{Name: "pv", PowerW: 2000, MaxW: 6000, Connected: true, Energized: true}}
	bats := []BatteryState{ruleBat("bat", 4000, 60, 5000)}
	p := &Plan{BatteryCommands: []BatteryCommand{{Name: "bat", SetpointW: 4000}}}
	bats, touCap := o.applyAusGenerationLimitRule(sol, bats, 5000, p)
	// Cap 5000, solar 2000 → discharge headroom 3000: the 4000 W command trims.
	if p.BatteryCommands[0].SetpointW != 3000 {
		t.Errorf("battery setpoint = %.0f, want 3000 (trimmed to participation cap)", p.BatteryCommands[0].SetpointW)
	}
	if bats[0].PowerW != 3000 {
		t.Errorf("threaded battery PowerW = %.0f, want 3000", bats[0].PowerW)
	}
	// All headroom consumed → nothing left for TOU.
	if touCap != 0 {
		t.Errorf("TOU discharge cap = %v, want 0", touCap)
	}
	// Solar ceiling = cap − surviving discharge = 2000.
	if i := solarCommandIndex(p.SolarCommands, "pv"); i < 0 || math.Abs(p.SolarCommands[i].CurtailToW-2000) > 1 {
		t.Errorf("solar ceiling = %+v, want 2000", p.SolarCommands)
	}
}

// The gen rule must keep the TIGHTER ceiling when the export/opModMaxLimW rules
// already curtailed an inverter (the applyGenLimitRule merge).
func TestAusGenRule_KeepsTighterExistingCurtailment(t *testing.T) {
	o := NewDefaultOptimizer()
	sol := []SolarState{{Name: "pv", PowerW: 4000, MaxW: 5000, Connected: true, Energized: true}}
	p := &Plan{SolarCommands: []SolarCommand{{Name: "pv", CurtailToW: 1200}}} // export rule was tighter
	o.applyAusGenerationLimitRule(sol, nil, 3000, p)
	if p.SolarCommands[0].CurtailToW != 1200 {
		t.Errorf("ceiling = %.0f, want the tighter pre-existing 1200", p.SolarCommands[0].CurtailToW)
	}
	// And the inverse: AUS tighter than the existing command wins.
	o2 := NewDefaultOptimizer()
	p2 := &Plan{SolarCommands: []SolarCommand{{Name: "pv", CurtailToW: 4500}}}
	o2.applyAusGenerationLimitRule(sol, nil, 3000, p2)
	if p2.SolarCommands[0].CurtailToW != 3000 {
		t.Errorf("ceiling = %.0f, want the tighter AUS 3000", p2.SolarCommands[0].CurtailToW)
	}
}

// Full-Optimize integration: with enforcement ON, autonomous TOU peak discharge
// must respect the gross-generation cap (cap − solar), not MaxDischargeW.
func TestAusGenRule_CapsTOUDischarge(t *testing.T) {
	o := NewDefaultOptimizer()
	o.CostModel = DefaultTOUCostModel()
	o.EnforceAusLimits = true
	st := ausState(time.Date(2026, 7, 6, 17, 0, 0, 0, time.Local), 3000, math.NaN(), 500)
	st.Solar = []SolarState{ruleSol("pv", 1000)}
	st.Batteries = []BatteryState{ruleBat("bat", 0, 80, 5000)}
	plan := o.Optimize(st)
	i := batteryCommandIndex(plan.BatteryCommands, "bat")
	if i < 0 {
		t.Fatalf("no battery command: %+v", plan.BatteryCommands)
	}
	// Gross cap 3000, solar 1000 → discharge may not exceed 2000.
	if sp := plan.BatteryCommands[i].SetpointW; math.IsNaN(sp) || sp > 2000+1 {
		t.Errorf("TOU discharge = %.0f, want ≤ 2000 (gross-generation cap)", sp)
	}
}

// ── gross-generation convergence ──────────────────────────────────────────────

// Battery discharge counts toward the GROSS cap: solar alone under the cap but
// solar+discharge over it must breach with LimitType "generation-aus" — the
// exact distinction from opModMaxLimW's solar-only check.
func TestAusGenConvergence_BatteryDischargeCountsTowardCap(t *testing.T) {
	o := NewDefaultOptimizer()
	sol := []SolarState{{Name: "pv", PowerW: 800, MaxW: 5000, Connected: true, Energized: true}}
	bats := []BatteryState{{Name: "bat", PowerW: 2000, Connected: true, Energized: true}}
	var last *Plan
	for i := 0; i < ausGenBreachTicks; i++ {
		p := &Plan{}
		// Gross = 800 + 2000 = 2800 over the 1000 W cap.
		o.checkAusGenerationConvergence(sol, bats, -2000, 1000, p)
		last = p
	}
	if last.Breach == nil || last.Breach.LimitType != "generation-aus" {
		t.Fatalf("expected a generation-aus breach (gross gen includes battery discharge), got %+v", last.Breach)
	}
	// Sanity: the legacy opModMaxLimW check would NOT breach here (gen 800 < 1000,
	// floor 2000−2000=0) — pin the distinction.
	o2 := NewDefaultOptimizer()
	for i := 0; i < genBreachTicks+2; i++ {
		p := &Plan{}
		o2.checkGenLimitConvergence(sol, bats, -2000, 1000, p)
		if p.Breach != nil {
			t.Fatalf("opModMaxLimW check must not breach on battery-fed export: %+v", p.Breach)
		}
	}
}

// The ADAPTED meter floor: devices that echo compliant self-reports while the
// meter shows the site exporting over the cap are still caught — grossGen ≥
// −netW needs no battery-discharge subtraction because discharge is inside the
// capped quantity.
func TestAusGenConvergence_MeterFloorCatchesEchoedReports(t *testing.T) {
	o := NewDefaultOptimizer()
	sol := []SolarState{{Name: "pv", PowerW: 500, MaxW: 5000, Connected: true, Energized: true}} // echoed
	bats := []BatteryState{{Name: "bat", PowerW: 0, Connected: true, Energized: true}}           // echoed idle
	var last *Plan
	for i := 0; i < ausGenBreachTicks; i++ {
		p := &Plan{}
		// Meter: exporting 4500 W → gross generation is really ≥ 4500, over 1000.
		o.checkAusGenerationConvergence(sol, bats, -4500, 1000, p)
		last = p
	}
	if last.Breach == nil || last.Breach.LimitType != "generation-aus" {
		t.Fatalf("expected the meter floor to catch the echoed reports, got %+v", last.Breach)
	}
}

// A single under-cap blip mid-run decrements (leaky) rather than resetting the
// counter, so a sustained breach still escalates.
func TestAusGenConvergence_LeakyCounterToleratesBlip(t *testing.T) {
	o := NewDefaultOptimizer()
	breached := false
	seq := []float64{3000, 3000, 500, 3000, 3000, 3000} // one compliant blip
	for _, gen := range seq {
		s := []SolarState{{Name: "pv", PowerW: gen, MaxW: 5000, Connected: true, Energized: true}}
		p := &Plan{}
		o.checkAusGenerationConvergence(s, nil, math.NaN(), 1000, p)
		if p.Breach != nil {
			breached = true
		}
	}
	if !breached {
		t.Fatal("sustained gross-gen breach with a single blip never escalated — counter must be leaky, not resetting")
	}
}

// Cap cleared (NaN) resets the whole session.
func TestAusGenConvergence_ClearsOnNoCap(t *testing.T) {
	o := NewDefaultOptimizer()
	sol := []SolarState{{Name: "pv", PowerW: 3000, MaxW: 5000, Connected: true, Energized: true}}
	for i := 0; i < ausGenBreachTicks-1; i++ {
		o.checkAusGenerationConvergence(sol, nil, math.NaN(), 1000, &Plan{})
	}
	o.checkAusGenerationConvergence(sol, nil, math.NaN(), math.NaN(), &Plan{}) // cap clears
	p := &Plan{}
	o.checkAusGenerationConvergence(sol, nil, math.NaN(), 1000, p) // fresh session, tick 1
	if p.Breach != nil {
		t.Fatalf("counter must reset when the cap clears; got breach %+v", p.Breach)
	}
}

// ── gross-load rule ───────────────────────────────────────────────────────────

// Commanded battery charge over the cap's headroom is NEUTRALISED to idle
// (never trimmed to the boundary) — the applyImportLimitRule precedent and
// the arbiter's discard-out-of-bounds semantics, so cascade and mirror agree.
// A charge that FITS the headroom is left alone.
func TestAusLoadRule_NeutralisesOversizedCharge(t *testing.T) {
	o := NewDefaultOptimizer()
	// Home load 2000, battery charging 3000 → gross 5000 over the 4000 cap.
	// solar 0, discharge 0 → grossLoad = netW = 5000.
	bats := []BatteryState{ruleBat("bat", -3000, 50, 5000)}
	p := &Plan{BatteryCommands: []BatteryCommand{{Name: "bat", SetpointW: -3000}}}
	o.applyAusLoadLimitRule(nil, bats, nil, 4000, 5000, p)
	// conservative = 3200; non-battery load = 5000−3000 = 2000 → allowed
	// charge 1200 < commanded 3000 → the charge is halted outright.
	if got := p.BatteryCommands[0].SetpointW; got != 0 {
		t.Errorf("battery setpoint = %.0f, want 0 (oversized charge neutralised)", got)
	}

	// And a charge inside the headroom is untouched.
	o2 := NewDefaultOptimizer()
	bats2 := []BatteryState{ruleBat("bat", -1000, 50, 5000)}
	// Home 2000 + charge 1000 → gross 3000 under the 4000 cap; conservative
	// 3200 − nonBatt 2000 = 1200 headroom ≥ 1000 commanded.
	p2 := &Plan{BatteryCommands: []BatteryCommand{{Name: "bat", SetpointW: -1000}}}
	o2.applyAusLoadLimitRule(nil, bats2, nil, 4000, 3000, p2)
	if got := p2.BatteryCommands[0].SetpointW; math.Abs(got-(-1000)) > 1 {
		t.Errorf("in-headroom charge = %.0f, want −1000 (untouched)", got)
	}
}

// The EV lever engages on a hard-cap breach, holds sticky across compliant
// ticks (no tick-period oscillation), and only relaxes after the relax window.
func TestAusLoadRule_EVCurtailStickyAcrossTicks(t *testing.T) {
	o := NewDefaultOptimizer()
	evs := []EVSEState{{StationID: "cs1", ConnectorID: 1, Connected: true, SessionActive: true,
		MaxCurrentA: 32, VoltageV: 230, PowerW: 7360}} // 32 A × 230 V

	// Tick 1: home 1000 + EV 7360 = 8360 gross, over the 4000 cap → engage.
	p1 := &Plan{EVSECommands: []EVSECommand{{StationID: "cs1", ConnectorID: 1, MaxCurrentA: 32}}}
	o.applyAusLoadLimitRule(nil, nil, evs, 4000, 8360, p1)
	if math.IsNaN(o.ausLoadGuard.evLimitA) {
		t.Fatal("EV lever did not engage on a hard-cap breach")
	}
	first := p1.EVSECommands[0].MaxCurrentA
	// conservative 3200 − home 1000 → allowance 2200 W ≈ 9.6 A.
	if first > 10 || first < 6 {
		t.Fatalf("first EV limit = %.1f A, want ≈9.6 A (allowance/voltage)", first)
	}

	// Tick 2: EV now compliant (draws at the limit), gross under cap — the
	// sticky ceiling must be RE-ISSUED, not released back to rule 6's full rate.
	evs[0].PowerW = first * 230
	p2 := &Plan{EVSECommands: []EVSECommand{{StationID: "cs1", ConnectorID: 1, MaxCurrentA: 32}}}
	o.applyAusLoadLimitRule(nil, nil, evs, 4000, 1000+first*230, p2)
	if p2.EVSECommands[0].MaxCurrentA >= 32 {
		t.Fatalf("tick 2: sticky EV ceiling released (%.1f A) — tick-period oscillation", p2.EVSECommands[0].MaxCurrentA)
	}
	held := p2.EVSECommands[0].MaxCurrentA

	// A relax step is gated on ExportRelaxCycles safe ticks and bounded per tick.
	var after float64
	for i := 0; i < o.ExportRelaxCycles+2; i++ {
		p := &Plan{EVSECommands: []EVSECommand{{StationID: "cs1", ConnectorID: 1, MaxCurrentA: 32}}}
		o.applyAusLoadLimitRule(nil, nil, evs, 4000, 500, p) // load well under cap now
		after = p.EVSECommands[0].MaxCurrentA
	}
	if after < held {
		t.Fatalf("relax lowered the limit (%.1f < %.1f)", after, held)
	}
	if after > held+float64(o.ExportRelaxCycles+2)*ausLoadEVMaxRelaxA {
		t.Fatalf("relax exceeded the per-tick bound: %.1f from %.1f", after, held)
	}
}

// A meter-blind tick must hold (re-issue) the sticky EV ceiling — fail-closed —
// and never relax or release it.
func TestAusLoadRule_MeterBlindHoldsStickyLimit(t *testing.T) {
	o := NewDefaultOptimizer()
	evs := []EVSEState{{StationID: "cs1", ConnectorID: 1, Connected: true, SessionActive: true,
		MaxCurrentA: 32, VoltageV: 230, PowerW: 7360}}
	p1 := &Plan{EVSECommands: []EVSECommand{{StationID: "cs1", ConnectorID: 1, MaxCurrentA: 32}}}
	o.applyAusLoadLimitRule(nil, nil, evs, 4000, 8360, p1)
	engaged := o.ausLoadGuard.evLimitA

	p2 := &Plan{EVSECommands: []EVSECommand{{StationID: "cs1", ConnectorID: 1, MaxCurrentA: 32}}}
	o.applyAusLoadLimitRule(nil, nil, evs, 4000, math.NaN(), p2)
	if o.ausLoadGuard.evLimitA != engaged {
		t.Fatalf("meter-blind tick changed the sticky limit: %v → %v", engaged, o.ausLoadGuard.evLimitA)
	}
	if p2.EVSECommands[0].MaxCurrentA != engaged {
		t.Fatalf("meter-blind tick did not re-issue the sticky ceiling: %+v", p2.EVSECommands)
	}
}

// ── gross-load convergence ────────────────────────────────────────────────────

// Unsheddable home load over the cap must escalate to a CannotComply with
// LimitType "load-aus" after the detection window.
func TestAusLoadConvergence_BreachAfterSustainedOverCap(t *testing.T) {
	o := NewDefaultOptimizer()
	var last *Plan
	for i := 0; i < ausLoadBreachTicks; i++ {
		p := &Plan{}
		// grossLoad = netW = 6000 over the 4000 cap; no battery, no EV to shed.
		o.checkAusLoadConvergence(nil, nil, 6000, 4000, p)
		last = p
	}
	if last.Breach == nil || last.Breach.LimitType != "load-aus" {
		t.Fatalf("expected a load-aus breach, got %+v", last.Breach)
	}
	if last.Breach.MeasuredW != 6000 || last.Breach.LimitW != 4000 {
		t.Fatalf("breach payload wrong: %+v", last.Breach)
	}
}

// A meter-blind tick HOLDS the counter (never resets) — the NaN-hold semantics
// mirrored from checkImportConvergence.
func TestAusLoadConvergence_NaNTickHoldsCounter(t *testing.T) {
	o := NewDefaultOptimizer()
	// Two over-cap ticks, one blind tick, one more over-cap tick → breach at
	// the threshold as if the blind tick never happened.
	o.checkAusLoadConvergence(nil, nil, 6000, 4000, &Plan{})
	o.checkAusLoadConvergence(nil, nil, 6000, 4000, &Plan{})
	o.checkAusLoadConvergence(nil, nil, math.NaN(), 4000, &Plan{}) // blind — hold
	p := &Plan{}
	o.checkAusLoadConvergence(nil, nil, 6000, 4000, p)
	if p.Breach == nil || p.Breach.LimitType != "load-aus" {
		t.Fatalf("blind tick must hold (not reset) the counter; got %+v", p.Breach)
	}
}

// Cap cleared resets the counter.
func TestAusLoadConvergence_ClearsOnNoCap(t *testing.T) {
	o := NewDefaultOptimizer()
	for i := 0; i < ausLoadBreachTicks-1; i++ {
		o.checkAusLoadConvergence(nil, nil, 6000, 4000, &Plan{})
	}
	o.checkAusLoadConvergence(nil, nil, 6000, math.NaN(), &Plan{}) // cap clears
	p := &Plan{}
	o.checkAusLoadConvergence(nil, nil, 6000, 4000, p)
	if p.Breach != nil {
		t.Fatalf("counter must reset when the cap clears; got %+v", p.Breach)
	}
}

// The convergence backstop defers to a breach another rule already recorded
// this tick (plan.Breach == nil gate, mirroring checkImportConvergence).
func TestAusLoadConvergence_DefersToExistingBreach(t *testing.T) {
	o := NewDefaultOptimizer()
	for i := 0; i < ausLoadBreachTicks; i++ {
		p := &Plan{Breach: &ComplianceBreach{LimitType: "import", ShortfallW: 9999}}
		o.checkAusLoadConvergence(nil, nil, 6000, 4000, p)
		if p.Breach.LimitType != "import" {
			t.Fatalf("load-aus backstop overwrote an existing breach: %+v", p.Breach)
		}
	}
}

// Full-Optimize breach emission: with enforcement ON and a sustained
// unsheddable gross-load breach, Optimize stamps the active control's MRID on
// the load-aus breach — the same post-hoc stamp every other breach gets, so
// the episode machinery (cmd/hub/breach.go) can address the CannotComply.
func TestAusLoad_OptimizeStampsBreachMRID(t *testing.T) {
	o := NewDefaultOptimizer()
	o.EnforceAusLimits = true
	var last Plan
	for i := 0; i < ausLoadBreachTicks; i++ {
		st := ausState(time.Date(2026, 7, 6, 3, 0, 0, 0, time.Local), math.NaN(), 4000, 6000)
		st.CSIPControl = &CSIPControlState{Source: "event", MRID: "aus-load-mrid"}
		last = o.Optimize(st)
	}
	if last.Breach == nil || last.Breach.LimitType != "load-aus" || last.Breach.MRID != "aus-load-mrid" {
		t.Fatalf("expected load-aus breach stamped with the active MRID, got %+v", last.Breach)
	}
}
