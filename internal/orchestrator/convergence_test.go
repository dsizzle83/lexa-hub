package orchestrator

import (
	"math"
	"testing"
)

// hasCurtailingSolarCommand reports whether the plan curtails any inverter
// meaningfully below the given nameplate (a real curtailment, not a no-op
// ceiling at/above nameplate).
func hasCurtailingSolarCommand(p *Plan, nameplateW float64) bool {
	for _, c := range p.SolarCommands {
		if !math.IsNaN(c.CurtailToW) && c.CurtailToW < nameplateW*0.9 {
			return true
		}
	}
	return false
}

// A battery that ACKs a charge setpoint but never actually absorbs (PowerW stays
// at 0 every tick) must not keep the inverter uncurtailed: after battBreachTicks
// the export rule discredits the phantom absorption and curtails solar to hold
// the export cap. (audit: battery-charge-disabled)
func TestExportLimit_BatteryStallCurtailsSolar(t *testing.T) {
	o := NewDefaultOptimizer()
	limits := gridConstraints{exportLimitW: 0, importLimitW: math.NaN(), maxLimitW: math.NaN()}

	var last *Plan
	for i := 0; i < battBreachTicks; i++ {
		// Fresh state each tick: the pack refuses to charge, so its measured
		// PowerW is 0 even though the prior tick commanded a charge.
		bats := []BatteryState{ruleBat("bat", 0, 50, 5000)}
		sol := []SolarState{ruleSol("pv", 4800)}
		p := &Plan{}
		o.applyExportLimitRule(sol, nil, 0, limits, -4400, 95, 4400, bats, p)
		last = p
	}

	// Behavioral oracle: the phantom-absorption discredit is observable as a real
	// solar curtailment (feed-forward drops from crediting the commanded ~4400 W
	// absorb to crediting the measured 0 W), holding the export cap. We assert the
	// plan CURTAILS the inverter, not the wording of the decision.
	if !hasCurtailingSolarCommand(last, 4800) {
		t.Errorf("expected solar curtailment once the stall was declared after %d ticks; solarCommands=%+v", battBreachTicks, last.SolarCommands)
	}
}

// A refusing pack with a single mid-run blip (one tick where it momentarily
// absorbs) must still trip the stall guard — the leaky counter decrements on the
// blip instead of resetting, so the curtail is only delayed, not lost. (audit:
// battery-charge-disabled flakiness)
func TestExportLimit_BatteryStallToleratesBlip(t *testing.T) {
	o := NewDefaultOptimizer()
	limits := gridConstraints{exportLimitW: 0, importLimitW: math.NaN(), maxLimitW: math.NaN()}

	// refuse, refuse, blip(absorb), refuse, refuse, refuse — should still curtail.
	blips := []bool{false, false, true, false, false, false}
	var last *Plan
	for _, absorb := range blips {
		batPower, netW, surplus := 0.0, -4400.0, 4400.0
		if absorb { // the pack briefly charges ~half the surplus this tick
			batPower, netW, surplus = -2400, -2000, 2000
		}
		bats := []BatteryState{ruleBat("bat", batPower, 50, 5000)}
		sol := []SolarState{ruleSol("pv", 4800)}
		last = &Plan{}
		o.applyExportLimitRule(sol, nil, 0, limits, netW, 95, surplus, bats, last)
	}
	// Leaky counter: the single absorb-blip only decrements the stall count, so by
	// the final tick the discredit still trips and the plan curtails solar. Assert
	// the curtailment (the behavior), not the decision string.
	if !hasCurtailingSolarCommand(last, 4800) {
		t.Errorf("a stall with one absorb-blip must still curtail solar (leaky counter); solarCommands=%+v", last.SolarCommands)
	}
}

// A battery that DOES absorb the commanded charge (measured tracks command) must
// never trip the stall guard — solar stays uncurtailed (battery-first).
func TestExportLimit_BatteryAbsorbingDoesNotTrip(t *testing.T) {
	o := NewDefaultOptimizer()
	limits := gridConstraints{exportLimitW: 0, importLimitW: math.NaN(), maxLimitW: math.NaN()}

	for i := 0; i < battBreachTicks+2; i++ {
		// The pack is charging at ~4400 W (PowerW negative = charging), i.e. it is
		// honouring the command — measured absorption matches the surplus.
		bats := []BatteryState{ruleBat("bat", -4400, 50, 5000)}
		sol := []SolarState{ruleSol("pv", 4800)}
		p := &Plan{}
		// Meter shows ~0 export because the battery is soaking the surplus.
		o.applyExportLimitRule(sol, nil, 0, limits, 0, 95, 0, bats, p)
		// A genuinely absorbing pack keeps the feed-forward credit, so the inverter
		// is never curtailed. Assert the absence of a curtail command (the behavior),
		// not the absence of a decision string.
		if hasCurtailingSolarCommand(p, 4800) {
			t.Fatalf("tick %d: solar curtailed while battery is absorbing (false stall); solarCommands=%+v", i, p.SolarCommands)
		}
	}
}

// An inverter that echoes the gen-cap back (self-reported power looks compliant)
// but keeps generating must still be caught via the independent meter floor:
// generation ≥ grid_export − battery_discharge. (audit: enable-gate-curtail)
func TestCheckGenConvergence_CaughtByMeterFloorDespiteEchoedReport(t *testing.T) {
	o := NewDefaultOptimizer()
	// Inverter self-reports a compliant 1000 W (the echoed cap) but the meter
	// shows the site exporting 4550 W with no battery discharge — generation is
	// really ≥ 4550 W, over the 1000 W cap.
	solar := []SolarState{{Name: "pv", PowerW: 1000, MaxW: 5000, Connected: true, Energized: true}}
	var last *Plan
	for i := 0; i < genBreachTicks; i++ {
		p := &Plan{}
		o.checkGenLimitConvergence(solar, nil, -4550, 1000, p)
		last = p
	}
	if last.Breach == nil || last.Breach.LimitType != "generation" {
		t.Fatalf("expected a generation breach from the meter floor, got %+v", last.Breach)
	}
}

// Battery discharge legitimately raises export without raising generation, so the
// meter floor must net it out and NOT false-trip a gen breach.
func TestCheckGenConvergence_BatteryDischargeDoesNotFalseTrip(t *testing.T) {
	o := NewDefaultOptimizer()
	solar := []SolarState{{Name: "pv", PowerW: 800, MaxW: 5000, Connected: true, Energized: true}}
	bats := []BatteryState{{Name: "bat", PowerW: 2000, Connected: true, Energized: true}} // discharging
	for i := 0; i < genBreachTicks+2; i++ {
		p := &Plan{}
		// Export 2000 W is all from the battery; generation (800 W) is under the 1000 cap.
		o.checkGenLimitConvergence(solar, bats, -2000, 1000, p)
		if p.Breach != nil {
			t.Fatalf("tick %d: false gen breach while battery supplies the export: %+v", i, p.Breach)
		}
	}
}

// hasDisconnectCommand reports whether the plan force-disconnects the named pack.
func hasDisconnectCommand(p *Plan, name string) bool {
	for _, c := range p.BatteryCommands {
		if c.Name == name && c.Connect != nil && !*c.Connect {
			return true
		}
	}
	return false
}

// A pack discharging at/below its reserve (which no rule commands) is the device
// inverting its setpoint; after batteryReserveDrainTicks the hub disconnects it.
// (audit: battery-wrong-sign)
func TestCheckBatterySafety_DisconnectsReserveDrain(t *testing.T) {
	o := NewDefaultOptimizer() // SOCReserve = 20
	var last *Plan
	for i := 0; i < batteryReserveDrainTicks; i++ {
		// Pack discharging 4800 W at 10% SOC — below the 20% reserve.
		bats := []BatteryState{{Name: "bat", PowerW: 4800, SOC: 10, Connected: true, Energized: true}}
		p := &Plan{}
		o.checkBatterySafety(bats, p)
		last = p
	}
	if !hasDisconnectCommand(last, "bat") {
		t.Errorf("expected force-disconnect of reserve-draining pack; commands=%+v", last.BatteryCommands)
	}
}

// A pack discharging normally ABOVE its reserve must never be disconnected.
func TestCheckBatterySafety_AllowsDischargeAboveReserve(t *testing.T) {
	o := NewDefaultOptimizer()
	for i := 0; i < batteryReserveDrainTicks+2; i++ {
		bats := []BatteryState{{Name: "bat", PowerW: 4800, SOC: 60, Connected: true, Energized: true}}
		p := &Plan{}
		o.checkBatterySafety(bats, p)
		if hasDisconnectCommand(p, "bat") {
			t.Fatalf("tick %d: disconnected a pack discharging legally above reserve", i)
		}
	}
}

// A transient single-tick reserve reading must not trip the disconnect.
func TestCheckBatterySafety_RidesOutSingleTick(t *testing.T) {
	o := NewDefaultOptimizer()
	bats := []BatteryState{{Name: "bat", PowerW: 4800, SOC: 10, Connected: true, Energized: true}}
	p := &Plan{}
	o.checkBatterySafety(bats, p)
	if hasDisconnectCommand(p, "bat") {
		t.Error("disconnected on a single tick; should confirm over batteryReserveDrainTicks")
	}
}

// EvaluateSafety (fast protection loop) must disconnect a pack commanded to
// charge but measured discharging at/near its reserve — immediately, with no
// debounce and no economic plan present. (audit: battery-wrong-sign, ADR-0001)
func TestEvaluateSafety_DisconnectsSignInversionAtReserve(t *testing.T) {
	o := NewDefaultOptimizer() // SOCReserve = 20
	o.lastBattCmd["bat"] = -3000
	state := SystemState{Batteries: []BatteryState{
		{Name: "bat", PowerW: 4800, SOC: 12, Connected: true, Energized: true},
	}}
	plan := o.EvaluateSafety(state)
	if !hasDisconnectCommand(&plan, "bat") {
		t.Fatalf("expected fast-loop disconnect; commands=%+v", plan.BatteryCommands)
	}
}

func TestEvaluateSafety_NoDisconnectWhenCharging(t *testing.T) {
	o := NewDefaultOptimizer()
	o.lastBattCmd["bat"] = -3000
	state := SystemState{Batteries: []BatteryState{
		{Name: "bat", PowerW: -3000, SOC: 12, Connected: true, Energized: true},
	}}
	if plan := o.EvaluateSafety(state); hasDisconnectCommand(&plan, "bat") {
		t.Error("fast loop disconnected a correctly-charging pack")
	}
}

func TestEvaluateSafety_NoDisconnectAboveReserve(t *testing.T) {
	o := NewDefaultOptimizer()
	o.lastBattCmd["bat"] = -3000
	state := SystemState{Batteries: []BatteryState{
		{Name: "bat", PowerW: 4800, SOC: 80, Connected: true, Energized: true},
	}}
	if plan := o.EvaluateSafety(state); hasDisconnectCommand(&plan, "bat") {
		t.Error("fast loop disconnected far from reserve; should defer to the economic tick")
	}
}

func TestEvaluateSafety_NoDisconnectWhenDischargeCommanded(t *testing.T) {
	o := NewDefaultOptimizer()
	o.lastBattCmd["bat"] = 3000 // hub commanded DISCHARGE, not charge
	state := SystemState{Batteries: []BatteryState{
		{Name: "bat", PowerW: 4800, SOC: 12, Connected: true, Energized: true},
	}}
	if plan := o.EvaluateSafety(state); hasDisconnectCommand(&plan, "bat") {
		t.Error("fast loop disconnected a legitimately commanded discharge")
	}
}

func TestCheckImportConvergence_BreachAfterSustainedOverCap(t *testing.T) {
	o := NewDefaultOptimizer()
	var last *Plan
	for i := 0; i < importBreachTicks; i++ {
		p := &Plan{}
		o.checkImportConvergence(0, 1430, p) // cap 0 W, importing 1430 W
		if i < importBreachTicks-1 && p.Breach != nil {
			t.Fatalf("breach recorded too early at tick %d", i)
		}
		last = p
	}
	if last.Breach == nil {
		t.Fatal("expected a CannotComply breach after sustained import over cap")
	}
	if last.Breach.LimitType != "import" || last.Breach.MeasuredW != 1430 {
		t.Errorf("unexpected breach: %+v", last.Breach)
	}
}

// The counter is leaky, not hard-reset: a compliant tick drains one count, so
// sustained compliance drains to zero but a lone blip cannot restart a climbing
// breach from scratch (mirrors checkGenLimitConvergence).
func TestCheckImportConvergence_CompliantTickDrainsOneCount(t *testing.T) {
	o := NewDefaultOptimizer()
	for i := 0; i < importBreachTicks-1; i++ {
		o.checkImportConvergence(0, 1430, &Plan{})
	}
	o.checkImportConvergence(0, 0, &Plan{}) // compliant tick drains one count
	p := &Plan{}
	o.checkImportConvergence(0, 1430, p) // back over cap: count = threshold-1
	if p.Breach != nil {
		t.Error("breach fired before the leaky counter re-reached the threshold")
	}
	p = &Plan{}
	o.checkImportConvergence(0, 1430, p) // count reaches the threshold
	if p.Breach == nil {
		t.Error("a sustained breach with one compliant blip must still escalate")
	}
}

// Sustained compliance drains the counter fully — an old near-breach cannot
// trip a CannotComply long after the site converged.
func TestCheckImportConvergence_SustainedComplianceDrainsToZero(t *testing.T) {
	o := NewDefaultOptimizer()
	for i := 0; i < importBreachTicks-1; i++ {
		o.checkImportConvergence(0, 1430, &Plan{})
	}
	for i := 0; i < importBreachTicks-1; i++ {
		o.checkImportConvergence(0, 0, &Plan{}) // drain back to zero
	}
	p := &Plan{}
	o.checkImportConvergence(0, 1430, p) // one over-cap tick from a drained counter
	if p.Breach != nil {
		t.Error("breach recorded after sustained compliance should need a full re-escalation")
	}
}

// A meter-blind tick (netW NaN, e.g. worldMoving excluded a frozen meter) holds
// the counter: it is evidence of neither breach nor compliance, and must not
// stall the escalation race against the constraint window (QA 2026-07-01:
// battery-soc-refuse lost 50% of cycles to NaN-tick resets).
func TestCheckImportConvergence_NaNTickHoldsCounter(t *testing.T) {
	o := NewDefaultOptimizer()
	for i := 0; i < importBreachTicks-1; i++ {
		o.checkImportConvergence(0, 1430, &Plan{})
	}
	o.checkImportConvergence(0, math.NaN(), &Plan{}) // blind tick — counter held
	p := &Plan{}
	o.checkImportConvergence(0, 1430, p) // next over-cap tick completes the count
	if p.Breach == nil {
		t.Error("a meter-blind tick must hold the breach counter, not reset it")
	}
}

func TestCheckImportConvergence_ClearsOnNoCap(t *testing.T) {
	o := NewDefaultOptimizer()
	for i := 0; i < importBreachTicks-1; i++ {
		o.checkImportConvergence(0, 1430, &Plan{})
	}
	o.checkImportConvergence(math.NaN(), 1430, &Plan{}) // cap cleared → reset
	p := &Plan{}
	o.checkImportConvergence(0, 1430, p)
	if p.Breach != nil {
		t.Error("counter should reset when the import cap clears")
	}
}

// Sub-threshold drift in the decoded cap value (the watts→ActivePower
// value×10^mult round-trip can wobble tick-to-tick) must NOT restart the import
// guard: a bit-exact session comparison wiped breachTicks every tick, so a
// sustained breach never escalated to CannotComply (QA 2026-07-01:
// battery-soc-refuse; same bug class the gen guard fixed).
func TestApplyImportLimitRule_CapDriftKeepsGuardSession(t *testing.T) {
	o := NewDefaultOptimizer()
	limits := gridConstraints{importLimitW: 3000, exportLimitW: math.NaN(), maxLimitW: math.NaN()}
	o.applyImportLimitRule(nil, limits, 4000, 20, &Plan{}) // session starts
	o.impGuard.breachTicks = 2

	limits.importLimitW = 3000.4 // decode wobble, same session
	o.applyImportLimitRule(nil, limits, 4000, 20, &Plan{})
	if o.impGuard.breachTicks != 2 {
		t.Errorf("sub-threshold cap drift restarted the guard: breachTicks=%d, want 2", o.impGuard.breachTicks)
	}
	if o.impGuard.activeLimitW != 3000.4 {
		t.Errorf("guard did not follow drift: activeLimitW=%v, want 3000.4", o.impGuard.activeLimitW)
	}

	limits.importLimitW = 5000 // a genuinely new cap → fresh session
	o.applyImportLimitRule(nil, limits, 4000, 20, &Plan{})
	if o.impGuard.breachTicks != 0 {
		t.Errorf("a meaningful cap change must restart the guard: breachTicks=%d, want 0", o.impGuard.breachTicks)
	}
}

// An existing breach from another rule this tick is not overwritten.
func TestCheckImportConvergence_DefersToExistingBreach(t *testing.T) {
	o := NewDefaultOptimizer()
	for i := 0; i < importBreachTicks-1; i++ {
		o.checkImportConvergence(0, 1430, &Plan{})
	}
	p := &Plan{Breach: &ComplianceBreach{LimitType: "import", ShortfallW: 999, Reason: "preexisting"}}
	o.checkImportConvergence(0, 1430, p)
	if p.Breach.Reason != "preexisting" {
		t.Errorf("existing breach was overwritten: %+v", p.Breach)
	}
}

// ── checkExportLimitConvergence (closed-loop export cap) ──────────────────────

// TestCheckExportConvergence_BreachAfterSustainedOverCap is the regression for
// the silent-non-convergence cluster (QA 2026-07-03: control-churn,
// clock-jitter): measured export sustained over the cap — with the ceiling
// controller still holding levers, so the export rule's own zero-lever breach
// never fires — must escalate to a CannotComply after exportBreachTicks.
func TestCheckExportConvergence_BreachAfterSustainedOverCap(t *testing.T) {
	o := NewDefaultOptimizer()
	var last *Plan
	for i := 0; i < exportBreachTicks; i++ {
		p := &Plan{}
		o.checkExportLimitConvergence(500, -4400, p) // cap 500 W, exporting 4400 W
		if i < exportBreachTicks-1 && p.Breach != nil {
			t.Fatalf("breach recorded too early at tick %d", i)
		}
		last = p
	}
	if last.Breach == nil {
		t.Fatal("expected a CannotComply breach after sustained export over cap")
	}
	if last.Breach.LimitType != "export" || last.Breach.MeasuredW != 4400 {
		t.Errorf("unexpected breach: %+v", last.Breach)
	}
	if math.Abs(last.Breach.ShortfallW-3900) > 1 {
		t.Errorf("shortfall = %.0fW, want 3900W", last.Breach.ShortfallW)
	}
}

// TestCheckExportConvergence_SurvivesCapChurn is the test that would have caught
// the control-churn gap before it reached the bench: a rapid sequence of cap
// VALUE rewrites (delete-and-replace alternating 0 W / 500 W, the control-churn
// fault pattern) while the site stays in violation of every one of them must
// still accumulate the breach counter to threshold. The counter is
// session-scoped — a per-value or tolerance-band reset (the 0↔500 W step is far
// wider than any noise band) would wipe it on every rewrite and the CannotComply
// could structurally never fire.
func TestCheckExportConvergence_SurvivesCapChurn(t *testing.T) {
	o := NewDefaultOptimizer()
	churn := []float64{0, 500, 0, 500, 0, 500}
	var last *Plan
	fired := -1
	for i, cap := range churn {
		last = &Plan{}
		o.checkExportLimitConvergence(cap, -4400, last) // over EVERY cap in the churn
		if last.Breach != nil && fired < 0 {
			fired = i
		}
	}
	if last.Breach == nil {
		t.Fatal("a sustained violation across rapid cap rewrites must still escalate (session-scoped counter)")
	}
	if fired != exportBreachTicks-1 {
		t.Errorf("breach fired at churn tick %d, want %d (counter must survive every rewrite)", fired, exportBreachTicks-1)
	}
}

// A site that converges to the cap never breaches, no matter how long it's held.
func TestCheckExportConvergence_ConvergedNeverBreaches(t *testing.T) {
	o := NewDefaultOptimizer()
	for i := 0; i < exportBreachTicks+3; i++ {
		p := &Plan{}
		o.checkExportLimitConvergence(500, -450, p) // exporting 450 W under the 500 W cap
		if p.Breach != nil {
			t.Fatalf("converged export must not breach (tick %d)", i)
		}
	}
}

// The counter is leaky: one compliant tick drains a count instead of zeroing, so
// a sustained breach with a lone blip still escalates (mirrors gen/import).
func TestCheckExportConvergence_CompliantTickDrainsOneCount(t *testing.T) {
	o := NewDefaultOptimizer()
	for i := 0; i < exportBreachTicks-1; i++ {
		o.checkExportLimitConvergence(500, -4400, &Plan{})
	}
	o.checkExportLimitConvergence(500, -400, &Plan{}) // compliant blip drains one count
	p := &Plan{}
	o.checkExportLimitConvergence(500, -4400, p) // count = threshold-1
	if p.Breach != nil {
		t.Error("breach fired before the leaky counter re-reached the threshold")
	}
	p = &Plan{}
	o.checkExportLimitConvergence(500, -4400, p) // count reaches the threshold
	if p.Breach == nil {
		t.Error("a sustained breach with one compliant blip must still escalate")
	}
}

// A meter-blind tick (netW NaN) holds the counter — evidence of nothing.
func TestCheckExportConvergence_NaNTickHoldsCounter(t *testing.T) {
	o := NewDefaultOptimizer()
	for i := 0; i < exportBreachTicks-1; i++ {
		o.checkExportLimitConvergence(500, -4400, &Plan{})
	}
	o.checkExportLimitConvergence(500, math.NaN(), &Plan{}) // blind tick — counter held
	p := &Plan{}
	o.checkExportLimitConvergence(500, -4400, p) // next over-cap tick completes the count
	if p.Breach == nil {
		t.Error("a meter-blind tick must hold the breach counter, not reset it")
	}
}

// Clearing the export limit entirely ends the compliance session and resets the
// counter — the ONE reset the session-scoped counter has.
func TestCheckExportConvergence_ClearsOnNoCap(t *testing.T) {
	o := NewDefaultOptimizer()
	for i := 0; i < exportBreachTicks-1; i++ {
		o.checkExportLimitConvergence(500, -4400, &Plan{})
	}
	o.checkExportLimitConvergence(math.NaN(), -4400, &Plan{}) // cap cleared → reset
	p := &Plan{}
	o.checkExportLimitConvergence(500, -4400, p)
	if p.Breach != nil {
		t.Error("counter should reset when the export cap clears")
	}
}

// Grid import (netW > 0) is zero export — always compliant with an export cap.
func TestCheckExportConvergence_ImportIsCompliant(t *testing.T) {
	o := NewDefaultOptimizer()
	o.expOverTicks = 2 // mid-escalation
	p := &Plan{}
	o.checkExportLimitConvergence(500, 2000, p) // importing — drains the counter
	if p.Breach != nil {
		t.Error("an importing site cannot breach an export cap")
	}
	if o.expOverTicks != 1 {
		t.Errorf("import tick should drain one count, got %d", o.expOverTicks)
	}
}

// An existing breach from another rule this tick is not overwritten.
func TestCheckExportConvergence_DefersToExistingBreach(t *testing.T) {
	o := NewDefaultOptimizer()
	for i := 0; i < exportBreachTicks-1; i++ {
		o.checkExportLimitConvergence(500, -4400, &Plan{})
	}
	p := &Plan{Breach: &ComplianceBreach{LimitType: "export", ShortfallW: 9999, Reason: "preexisting"}}
	o.checkExportLimitConvergence(500, -4400, p)
	if p.Breach.Reason != "preexisting" {
		t.Errorf("existing breach was overwritten: %+v", p.Breach)
	}
}
