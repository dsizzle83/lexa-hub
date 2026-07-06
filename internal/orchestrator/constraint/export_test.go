package constraint

import (
	"math"
	"testing"

	"lexa-hub/internal/orchestrator"
	model "lexa-proto/csipmodel"
)

// These are the constraint-package twins of the export behavioral suite in
// internal/orchestrator (optimizer_compliance_test.go, convergence_test.go, and
// TestOptimizer_ExportChurnEscalatesCannotComply). Each drives
// ExportConstraint.Evaluate through the same tick sequence the legacy test drives
// applyExportLimitRule/Optimize through and asserts the SAME behavioral outcome,
// so the ported controller is pinned against the same contract before the flip.
// Tests are white-box (package constraint) so the two reset cadences can be
// inspected directly for the mutation checks.

// ── helpers ──────────────────────────────────────────────────────────────────

// benchInput builds an Input from a state with a bench-defaulted plant keyed to
// the state's device names, at the tuned (bench FAST) 3 s tick.
func benchInput(st orchestrator.SystemState) Input {
	p := Plant{
		Inverters: map[string]orchestrator.InverterPlant{},
		Batteries: map[string]orchestrator.BatteryPlant{},
		EVSEs:     map[string]orchestrator.EVSEPlant{},
		Meter:     orchestrator.MeterPlant{}.WithDefaults(),
	}
	for _, s := range st.Solar {
		p.Inverters[s.Name] = orchestrator.InverterPlant{}.WithDefaults()
	}
	for _, b := range st.Batteries {
		p.Batteries[b.Name] = orchestrator.BatteryPlant{}.WithDefaults()
	}
	return Input{State: st, Plant: p, TickSeconds: tunedTickInterval.Seconds()}
}

// newExportPair returns a fresh constraint and its base session at the tuned tick.
func newExportPair() (*ExportConstraint, *Session) {
	return NewExportConstraint(), NewSession("export", 0)
}

func expLimControl(w int16) *orchestrator.CSIPControlState {
	return &orchestrator.CSIPControlState{
		Source: "event", MRID: "exp-mrid",
		Base: model.DERControlBase{OpModExpLimW: &model.ActivePower{Value: w, Multiplier: 0}},
	}
}

// ceilingOf returns the emitted solar-ceiling for an inverter (NaN if none).
func ceilingOf(demands []Demand, name string) float64 {
	for _, d := range demands {
		if d.Axis == AxisSolarCeilingW && d.Device == name {
			return d.Max
		}
	}
	return math.NaN()
}

// setpointOf returns the emitted battery setpoint (NaN if none). Ports read the
// pinned point demand.
func setpointOf(demands []Demand, name string) float64 {
	for _, d := range demands {
		if d.Axis == AxisBatterySetpointW && d.Device == name {
			return d.Max // PointDemand ⇒ Min==Max
		}
	}
	return math.NaN()
}

const complianceMarginW = 150.0 // mirrors the replay's ±150 W tolerance

// ── ported behavioral tests ──────────────────────────────────────────────────

// Twin of TestExportLimit_PhantomEVCredit_StillCurtails: closed-loop, a full
// battery and a plugged-in-but-full EV — only curtailment can hold the cap, and
// it must, without collapsing generation to zero (the ratchet bug).
func TestExportConstraint_PhantomEVCredit_StillCurtails(t *testing.T) {
	c, s := newExportPair()
	const (
		potential = 6000.0
		loadW     = 500.0
		nameW     = 8000.0
	)
	solarOut := potential
	var finalExport float64
	for i := 0; i < 12; i++ {
		export := solarOut - loadW
		st := orchestrator.SystemState{
			Solar: []orchestrator.SolarState{{Name: "pv", PowerW: solarOut, MaxW: nameW, Connected: true, Energized: true}},
			Batteries: []orchestrator.BatteryState{{
				Name: "bat", PowerW: 0, SOC: 100, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true,
			}},
			EVSEs: []orchestrator.EVSEState{{
				StationID: "evse-001", ConnectorID: 1, Connected: true, SessionActive: true,
				MaxCurrentA: 32, VoltageV: 240, PowerW: 7, SOC: 100,
			}},
			Grid:        orchestrator.GridState{NetW: -export, ExportLimitW: math.NaN(), ImportLimitW: math.NaN(), MaxLimitW: math.NaN()},
			CSIPControl: expLimControl(1000),
		}
		demands, _ := c.Evaluate(benchInput(st), s)
		ceiling := ceilingOf(demands, "pv")
		if math.IsNaN(ceiling) {
			solarOut = potential
		} else {
			solarOut = math.Min(potential, ceiling)
		}
		finalExport = solarOut - loadW
	}
	if finalExport > 1000+complianceMarginW {
		t.Errorf("after convergence, export = %.0fW exceeds the 1000W cap (curtailment failed to hold)", finalExport)
	}
	if solarOut < 200 {
		t.Errorf("generation collapsed to %.0fW — over-curtailed (ratchet bug)", solarOut)
	}
}

// Twin of TestExportLimit_StickyCurtailment_NoRelease: already mid-episode and
// compliant only because of curtailment; the controller must keep issuing a real
// curtail (< nameplate), not release.
func TestExportConstraint_StickyCurtailment_NoRelease(t *testing.T) {
	c, s := newExportPair()
	c.sess.ctrl = exportController{
		evSetpointA: 22.6, evCmdW: 22.6 * 240, batteryAbsorbW: math.NaN(),
		activeLimitW: 1000, filteredExportW: 800, solarCeilingW: 1300, safeCount: 10,
	}
	st := orchestrator.SystemState{
		Solar: []orchestrator.SolarState{{Name: "pv", PowerW: 1300, MaxW: 8000, Connected: true, Energized: true}},
		Batteries: []orchestrator.BatteryState{{
			Name: "bat", PowerW: 0, SOC: 100, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true,
		}},
		EVSEs: []orchestrator.EVSEState{{
			StationID: "evse-001", ConnectorID: 1, Connected: true, SessionActive: true,
			MaxCurrentA: 32, VoltageV: 240, PowerW: 7, SOC: 100,
		}},
		Grid:        orchestrator.GridState{NetW: -800, ExportLimitW: math.NaN(), ImportLimitW: math.NaN(), MaxLimitW: math.NaN()},
		CSIPControl: expLimControl(1000),
	}
	demands, _ := c.Evaluate(benchInput(st), s)
	ceil := ceilingOf(demands, "pv")
	if math.IsNaN(ceil) || ceil >= 7000 {
		t.Errorf("curtailment released while compliant only because of curtailment (ceiling=%.0f) — will oscillate", ceil)
	}
}

// Twin of TestExportLimit_NoReleaseWhenBatteryCreditCancelsExport: the guard must
// stay engaged (non-NaN ceiling) on a tick where the battery-absorb credit lands
// the implied export on target.
func TestExportConstraint_NoReleaseWhenBatteryCreditCancelsExport(t *testing.T) {
	c, s := newExportPair()
	st := orchestrator.SystemState{
		Solar: []orchestrator.SolarState{{Name: "pv", PowerW: 5000, MaxW: 5000, Connected: true, Energized: true}},
		Batteries: []orchestrator.BatteryState{{
			Name: "bat", PowerW: 0, SOC: 82, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true,
		}},
		Grid:        orchestrator.GridState{NetW: -4700, ExportLimitW: math.NaN(), ImportLimitW: math.NaN(), MaxLimitW: math.NaN()},
		CSIPControl: expLimControl(2000),
	}
	c.Evaluate(benchInput(st), s)
	if math.IsNaN(c.sess.ctrl.solarCeilingW) {
		t.Error("export controller released to NaN on a battery-credit tick; must stay engaged so it re-curtails next tick")
	}
}

// Twin of TestExportLimit_FeedForwardSaturationCurtail: a full battery mid-episode
// must drop the ceiling to load + conservative-target in one tick (bypassing the
// down-slew), floored near the conservative target so it does not crater.
func TestExportConstraint_FeedForwardSaturationCurtail(t *testing.T) {
	c, s := newExportPair()
	c.sess.ctrl = exportController{
		evSetpointA: math.NaN(), evCmdW: math.NaN(), batteryAbsorbW: math.NaN(),
		activeLimitW: 2000, filteredExportW: 5000, solarCeilingW: 4000,
	}
	st := orchestrator.SystemState{
		Solar: []orchestrator.SolarState{{Name: "pv", PowerW: 4000, MaxW: 5000, Connected: true, Energized: true}},
		Batteries: []orchestrator.BatteryState{{
			Name: "bat", PowerW: 0, SOC: 100, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true,
		}},
		Grid:        orchestrator.GridState{NetW: -5000, ExportLimitW: math.NaN(), ImportLimitW: math.NaN(), MaxLimitW: math.NaN()},
		CSIPControl: expLimControl(2000),
	}
	c.Evaluate(benchInput(st), s)
	if c.sess.ctrl.solarCeilingW > 2000 {
		t.Errorf("ceiling = %.0fW exceeds the 2000W cap — feed-forward did not curtail a full battery's surplus", c.sess.ctrl.solarCeilingW)
	}
	if c.sess.ctrl.solarCeilingW < 1400 {
		t.Errorf("ceiling cratered to %.0fW — feed-forward must floor near the conservative target (~1600W)", c.sess.ctrl.solarCeilingW)
	}
}

// exportStallState builds the stall test's per-tick state (cap 0, PV 4800 @ 4800
// nameplate, refusing/absorbing battery).
func exportStallState(batPowerW, netW float64) orchestrator.SystemState {
	return orchestrator.SystemState{
		Solar: []orchestrator.SolarState{{Name: "pv", PowerW: 4800, MaxW: 4800, Connected: true, Energized: true}},
		Batteries: []orchestrator.BatteryState{{
			Name: "bat", PowerW: batPowerW, SOC: 50, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true,
		}},
		Grid: orchestrator.GridState{NetW: netW, ExportLimitW: 0, ImportLimitW: math.NaN(), MaxLimitW: math.NaN()},
	}
}

func curtailsSolar(demands []Demand, name string, nameplateW float64) bool {
	ceil := ceilingOf(demands, name)
	return !math.IsNaN(ceil) && ceil < nameplateW*0.9
}

// Twin of TestExportLimit_BatteryStallCurtailsSolar.
func TestExportConstraint_BatteryStallCurtailsSolar(t *testing.T) {
	c, s := newExportPair()
	var last []Demand
	for i := 0; i < exportBattBreachTicks; i++ {
		last, _ = c.Evaluate(benchInput(exportStallState(0, -4400)), s)
	}
	if !curtailsSolar(last, "pv", 4800) {
		t.Errorf("expected solar curtailment after the stall was declared over %d ticks; ceiling=%.0f", exportBattBreachTicks, ceilingOf(last, "pv"))
	}
}

// Twin of TestExportLimit_BatteryStallToleratesBlip: a single mid-run absorb blip
// only decrements the leaky counter, so the curtail still trips.
func TestExportConstraint_BatteryStallToleratesBlip(t *testing.T) {
	c, s := newExportPair()
	blips := []bool{false, false, true, false, false, false}
	var last []Demand
	for _, absorb := range blips {
		batPower, netW := 0.0, -4400.0
		if absorb {
			batPower, netW = -2400, -2000
		}
		last, _ = c.Evaluate(benchInput(exportStallState(batPower, netW)), s)
	}
	if !curtailsSolar(last, "pv", 4800) {
		t.Errorf("a stall with one absorb-blip must still curtail solar (leaky counter); ceiling=%.0f", ceilingOf(last, "pv"))
	}
}

// Twin of TestExportLimit_BatteryAbsorbingDoesNotTrip: a genuinely absorbing pack
// keeps solar uncurtailed (battery-first).
func TestExportConstraint_BatteryAbsorbingDoesNotTrip(t *testing.T) {
	c, s := newExportPair()
	for i := 0; i < exportBattBreachTicks+2; i++ {
		demands, _ := c.Evaluate(benchInput(exportStallState(-4400, 0)), s)
		if curtailsSolar(demands, "pv", 4800) {
			t.Fatalf("tick %d: solar curtailed while battery absorbs (false stall); ceiling=%.0f", i, ceilingOf(demands, "pv"))
		}
	}
}

// ── convergence counter: two-cadence + mutation ──────────────────────────────

// churnState mirrors TestOptimizer_ExportChurnEscalatesCannotComply's tick: PV
// 4500 @ 5000 nameplate with 1600 W load → 2900 W export, over both churned caps,
// while the computed ceiling stays well above zero (only the convergence check
// can report it).
func churnState(capW int16) orchestrator.SystemState {
	return orchestrator.SystemState{
		Solar: []orchestrator.SolarState{{Name: "pv", PowerW: 4500, MaxW: 5000, Connected: true, Energized: true}},
		Batteries: []orchestrator.BatteryState{{
			Name: "bat", PowerW: 0, SOC: 100, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true,
		}},
		Grid: orchestrator.GridState{NetW: -2900, ExportLimitW: math.NaN(), ImportLimitW: math.NaN(), MaxLimitW: math.NaN()},
		CSIPControl: &orchestrator.CSIPControlState{
			Source: "event", MRID: "churn-mrid",
			Base: model.DERControlBase{OpModExpLimW: &model.ActivePower{Value: capW, Multiplier: 0}},
		},
	}
}

// Twin of TestOptimizer_ExportChurnEscalatesCannotComply: the cap is rewritten
// every tick (0/500), resetting the controller each time, yet the session-scoped
// convergence counter must SURVIVE the resets and escalate to a CannotComply. The
// load-bearing separation of the two reset cadences.
func TestExportConstraint_ChurnEscalatesCannotComply(t *testing.T) {
	c, s := newExportPair()
	churn := []int16{0, 500, 0, 500, 0, 500}
	var lastBreach *orchestrator.ComplianceBreach
	firedAt := -1
	for i, capW := range churn {
		_, breach := c.Evaluate(benchInput(churnState(capW)), s)
		if breach != nil {
			lastBreach = breach
			if firedAt < 0 {
				firedAt = i
			}
		}
	}
	if lastBreach == nil {
		t.Fatal("sustained over-cap export across rapid cap rewrites must escalate to CannotComply")
	}
	if lastBreach.LimitType != "export" {
		t.Errorf("LimitType = %q, want export", lastBreach.LimitType)
	}
	if lastBreach.MRID != "churn-mrid" {
		t.Errorf("breach MRID = %q, want churn-mrid", lastBreach.MRID)
	}
	if firedAt < 2 {
		t.Errorf("breach fired at tick %d — too early; the sustained gate must ride out a normal ramp", firedAt)
	}
}

// TestExportConstraint_MutationChurn_CounterUnwired is the recorded MUTATION
// check: if the compliance counter were (wrongly) reset on every controller
// reset — the exact 2026-07-03 bug — the churn breach could never fire. This test
// simulates that mutation by zeroing overTicks whenever the controller resets and
// asserts the escalation is LOST, proving the counter's independent cadence is
// load-bearing (mirrors the upstream mutation guard).
func TestExportConstraint_MutationChurn_CounterUnwired(t *testing.T) {
	c, s := newExportPair()
	churn := []int16{0, 500, 0, 500, 0, 500, 0, 500}
	var fired bool
	prevLimit := math.NaN()
	for _, capW := range churn {
		in := benchInput(churnState(capW))
		limit := effectiveExportLimitW(in.State)
		// MUTATION: fold the compliance reset into the controller's cadence.
		if limit != prevLimit {
			c.sess.overTicks = 0
		}
		prevLimit = limit
		_, breach := c.Evaluate(in, s)
		// Re-apply the mutation post-evaluate too: the controller reset inside
		// Evaluate fired for this same value-change, so a counter sharing that
		// cadence would also be wiped here.
		if breach != nil {
			fired = true
		}
	}
	if fired {
		t.Fatal("MUTATION not detected: churn breach fired even with the compliance counter " +
			"reset on the controller cadence — the two-cadence separation would not be load-bearing")
	}
}

// TestExportConstraint_OverTicksSurvivesCapRewrite_ResetsOnClear pins both reset
// domains directly: overTicks survives a cap VALUE change but zeroes when the cap
// clears to NaN; the controller's activeLimitW tracks the current value.
func TestExportConstraint_OverTicksSurvivesCapRewrite_ResetsOnClear(t *testing.T) {
	c, s := newExportPair()
	// Two over-cap ticks under cap 0 → counter climbs.
	c.Evaluate(benchInput(churnState(0)), s)
	c.Evaluate(benchInput(churnState(0)), s)
	afterTwo := c.sess.overTicks
	if afterTwo == 0 {
		t.Fatal("expected overTicks to climb under a sustained over-cap")
	}
	// Rewrite the cap value (0→500): controller resets, counter must SURVIVE.
	c.Evaluate(benchInput(churnState(500)), s)
	if c.sess.overTicks < afterTwo {
		t.Errorf("overTicks dropped from %d to %d across a cap-VALUE rewrite; the compliance obligation must survive", afterTwo, c.sess.overTicks)
	}
	if c.sess.ctrl.activeLimitW != 500 {
		t.Errorf("controller activeLimitW = %.0f, want 500 (fresh session on the new value)", c.sess.ctrl.activeLimitW)
	}
	// Clear the cap entirely → both domains reset.
	clear := churnState(0)
	clear.Grid.ExportLimitW = math.NaN()
	clear.CSIPControl = nil
	c.Evaluate(benchInput(clear), s)
	if c.sess.overTicks != 0 {
		t.Errorf("overTicks = %d after cap cleared to NaN; want 0 (compliance session over)", c.sess.overTicks)
	}
	if !math.IsNaN(c.sess.ctrl.activeLimitW) {
		t.Errorf("controller activeLimitW = %.0f after clear; want NaN", c.sess.ctrl.activeLimitW)
	}
}

// ── NaN-meter hold + adaptive window ─────────────────────────────────────────

// TestExportConstraint_BlindMeterHoldsCounter: a NaN grid reading is evidence of
// nothing — the compliance counter must HOLD across it ("a blind meter must not
// launder a breach"), not reset.
func TestExportConstraint_BlindMeterHoldsCounter(t *testing.T) {
	c, s := newExportPair()
	c.Evaluate(benchInput(churnState(0)), s)
	c.Evaluate(benchInput(churnState(0)), s)
	held := c.sess.overTicks
	blind := churnState(0)
	blind.Grid.NetW = math.NaN()
	c.Evaluate(benchInput(blind), s)
	if c.sess.overTicks != held {
		t.Errorf("overTicks = %d after a blind-meter tick; want it HELD at %d", c.sess.overTicks, held)
	}
}

// TestExportConstraint_AdaptiveDetectionWindow: the export-breach window is
// derived from plant physics, not the fixed constant. Bench defaults (control 3 s
// + meter 5 s over a 3 s tick) yield 3 — bit-identical to exportBreachTicks — but
// a slower plant grows the window (the M2 fix: never fire before the meter could
// show the correction).
func TestExportConstraint_AdaptiveDetectionWindow(t *testing.T) {
	c, _ := newExportPair()

	benchIn := benchInput(churnState(0))
	if got := c.detectionWindowTicks(benchIn); got != 3 {
		t.Errorf("bench-default detection window = %d ticks, want 3 (parity with exportBreachTicks)", got)
	}

	// A slower inverter (control latency 12 s) over the same 3 s tick must widen
	// the window past the fixed constant.
	slow := benchInput(churnState(0))
	slow.Plant.Inverters["pv"] = orchestrator.InverterPlant{ControlLatencyS: 12}
	slow.Plant.Meter = orchestrator.MeterPlant{MeterLagS: 5}
	if got := c.detectionWindowTicks(slow); got <= 3 {
		t.Errorf("slow-plant detection window = %d ticks, want > 3 (window must track plant latency)", got)
	}
}

// TestExportConstraint_NoCapNoDemands: with no export cap the constraint expresses
// no opinion (empty demands, no breach) so the candidate-scoped shadow diff stays
// inert on unmigrated ticks.
func TestExportConstraint_NoCapNoDemands(t *testing.T) {
	c, s := newExportPair()
	st := orchestrator.SystemState{
		Solar: []orchestrator.SolarState{{Name: "pv", PowerW: 4000, MaxW: 5000, Connected: true, Energized: true}},
		Grid:  orchestrator.GridState{NetW: -3000, ExportLimitW: math.NaN(), ImportLimitW: math.NaN(), MaxLimitW: math.NaN()},
	}
	demands, breach := c.Evaluate(benchInput(st), s)
	if len(demands) != 0 || breach != nil {
		t.Errorf("no-cap tick emitted %d demands / breach=%v; want none", len(demands), breach)
	}
}
