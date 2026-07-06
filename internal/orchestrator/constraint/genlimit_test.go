package constraint

import (
	"math"
	"testing"

	"lexa-hub/internal/orchestrator"
	model "lexa-proto/csipmodel"
)

// Constraint-package twins of the generation-limit suite in internal/orchestrator
// (convergence_test.go:103-134). Each drives GenLimitConstraint.Evaluate through
// the same tick sequence the legacy test drives applyGenLimitRule /
// checkGenLimitConvergence through and asserts the SAME behavioral outcome, so the
// ported controller is pinned against the same contract before the flip. White-box
// (package constraint) so the session and the meter floor can be inspected directly.

func newGenPair() (*GenLimitConstraint, *Session) {
	return NewGenLimitConstraint(), NewSession("gen", 0)
}

func genLimControl(w int16) *orchestrator.CSIPControlState {
	return &orchestrator.CSIPControlState{
		Source: "event", MRID: "gen-mrid",
		Base: model.DERControlBase{OpModMaxLimW: &model.ActivePower{Value: w, Multiplier: 0}},
	}
}

// ── ceiling emission ──────────────────────────────────────────────────────────

// A gen cap must emit an absolute per-inverter ceiling distributed by nameplate
// share (sticky, re-issued every tick).
func TestGenConstraint_EmitsNameplateDistributedCeiling(t *testing.T) {
	c, s := newGenPair()
	st := orchestrator.SystemState{
		Solar: []orchestrator.SolarState{
			{Name: "pvA", PowerW: 3000, MaxW: 6000, Connected: true, Energized: true},
			{Name: "pvB", PowerW: 1000, MaxW: 2000, Connected: true, Energized: true},
		},
		Grid:        orchestrator.GridState{NetW: -3000, ExportLimitW: math.NaN(), ImportLimitW: math.NaN(), MaxLimitW: math.NaN()},
		CSIPControl: genLimControl(3000),
	}
	demands, _ := c.Evaluate(benchInput(st), s)
	// 3000 W cap over 8000 W total nameplate: pvA (6000/8000)=2250, pvB=750.
	if got := ceilingOf(demands, "pvA"); math.Abs(got-2250) > 1 {
		t.Errorf("pvA ceiling = %.0f, want 2250 (nameplate share)", got)
	}
	if got := ceilingOf(demands, "pvB"); math.Abs(got-750) > 1 {
		t.Errorf("pvB ceiling = %.0f, want 750 (nameplate share)", got)
	}
}

// No cap → no opinion (empty demands, no breach), so the candidate-scoped shadow
// diff stays inert on unmigrated ticks.
func TestGenConstraint_NoCapNoDemands(t *testing.T) {
	c, s := newGenPair()
	st := orchestrator.SystemState{
		Solar: []orchestrator.SolarState{{Name: "pv", PowerW: 4000, MaxW: 5000, Connected: true, Energized: true}},
		Grid:  orchestrator.GridState{NetW: -3000, ExportLimitW: math.NaN(), ImportLimitW: math.NaN(), MaxLimitW: math.NaN()},
	}
	demands, breach := c.Evaluate(benchInput(st), s)
	if len(demands) != 0 || breach != nil {
		t.Errorf("no-cap tick emitted %d demands / breach=%v; want none", len(demands), breach)
	}
}

// ── meter-independent floor (HARD preserve + mutation guard) ──────────────────

// Twin of TestCheckGenConvergence_CaughtByMeterFloorDespiteEchoedReport: an
// inverter echoes the gen cap back (self-reports a compliant 1000 W) but the meter
// shows the site exporting 4550 W with no battery discharge — generation is really
// ≥ 4550 W, over the 1000 W cap. Only the independent meter floor catches it.
//
// THIS IS THE RECORDED MUTATION GUARD for the meter-independent floor: delete the
// floor block in checkConvergence (genlimit.go) and measuredGenW stays at the
// echoed 1000 W (under the cap), overCount never climbs, and this test goes red —
// proving the floor is load-bearing (verified by hand-mutation, TASK-061 report).
func TestGenConstraint_CaughtByMeterFloorDespiteEchoedReport(t *testing.T) {
	c, s := newGenPair()
	var breach *orchestrator.ComplianceBreach
	// Inverter self-reports a compliant 1000 W; meter shows 4550 W export, no battery.
	st := orchestrator.SystemState{
		Solar:       []orchestrator.SolarState{{Name: "pv", PowerW: 1000, MaxW: 5000, Connected: true, Energized: true}},
		Grid:        orchestrator.GridState{NetW: -4550, ExportLimitW: math.NaN(), ImportLimitW: math.NaN(), MaxLimitW: math.NaN()},
		CSIPControl: genLimControl(1000),
	}
	// checkConvergence's window is 3 at bench defaults; drive that many ticks.
	for i := 0; i < 3; i++ {
		_, breach = c.Evaluate(benchInput(st), s)
	}
	if breach == nil || breach.LimitType != "generation" {
		t.Fatalf("expected a generation breach from the meter floor, got %+v", breach)
	}
	if breach.MRID != "gen-mrid" {
		t.Errorf("breach MRID = %q, want gen-mrid", breach.MRID)
	}
}

// Twin of TestCheckGenConvergence_BatteryDischargeDoesNotFalseTrip: battery
// discharge legitimately raises export without raising generation, so the meter
// floor must net it out (generation ≥ export − batteryDischarge) and NOT false-trip.
func TestGenConstraint_BatteryDischargeDoesNotFalseTrip(t *testing.T) {
	c, s := newGenPair()
	st := orchestrator.SystemState{
		Solar: []orchestrator.SolarState{{Name: "pv", PowerW: 800, MaxW: 5000, Connected: true, Energized: true}},
		Batteries: []orchestrator.BatteryState{
			{Name: "bat", PowerW: 2000, Connected: true, Energized: true}, // discharging
		},
		Grid:        orchestrator.GridState{NetW: -2000, ExportLimitW: math.NaN(), ImportLimitW: math.NaN(), MaxLimitW: math.NaN()},
		CSIPControl: genLimControl(1000),
	}
	for i := 0; i < 5; i++ {
		if _, breach := c.Evaluate(benchInput(st), s); breach != nil {
			t.Fatalf("tick %d: false gen breach while battery supplies the export: %+v", i, breach)
		}
	}
}

// ── leaky counter + tolerance-band session reset ──────────────────────────────

// The counter is leaky: a single sub-threshold sample decrements a climbing breach
// rather than zeroing it, so a sustained miss with one compliant blip still escalates.
func TestGenConstraint_LeakyCounterToleratesBlip(t *testing.T) {
	c, s := newGenPair()
	over := orchestrator.SystemState{
		Solar:       []orchestrator.SolarState{{Name: "pv", PowerW: 4000, MaxW: 5000, Connected: true, Energized: true}},
		Grid:        orchestrator.GridState{NetW: -4000, ExportLimitW: math.NaN(), ImportLimitW: math.NaN(), MaxLimitW: math.NaN()},
		CSIPControl: genLimControl(1000),
	}
	under := over
	under.Solar = []orchestrator.SolarState{{Name: "pv", PowerW: 900, MaxW: 5000, Connected: true, Energized: true}}
	under.Grid = orchestrator.GridState{NetW: -900, ExportLimitW: math.NaN(), ImportLimitW: math.NaN(), MaxLimitW: math.NaN()}

	// over, over, blip(under), over, over — leaky counter must still escalate.
	seq := []orchestrator.SystemState{over, over, under, over, over}
	var breach *orchestrator.ComplianceBreach
	for _, st := range seq {
		_, breach = c.Evaluate(benchInput(st), s)
	}
	if breach == nil {
		t.Error("a sustained gen breach with one compliant blip must still escalate (leaky counter)")
	}
}

// A meaningful cap change (> complianceBreachW) resets overCount; sub-threshold
// decode drift within a session does NOT (tolerance band).
func TestGenConstraint_CapDriftKeepsSession_MeaningfulChangeResets(t *testing.T) {
	c, s := newGenPair()
	over := func(capW float64) orchestrator.SystemState {
		return orchestrator.SystemState{
			Solar: []orchestrator.SolarState{{Name: "pv", PowerW: 4000, MaxW: 5000, Connected: true, Energized: true}},
			Grid:  orchestrator.GridState{NetW: -4000, ExportLimitW: math.NaN(), ImportLimitW: math.NaN(), MaxLimitW: math.NaN()},
			CSIPControl: &orchestrator.CSIPControlState{
				Source: "event", MRID: "gen-mrid",
				Base: model.DERControlBase{OpModMaxLimW: &model.ActivePower{Value: 0, Multiplier: 0}},
			},
		}
	}
	// Use raw floats to exercise sub-threshold drift the int16 CSIP path can't carry.
	st := over(0)
	st.Grid.MaxLimitW = 1000
	st.CSIPControl = nil
	c.Evaluate(benchInput(st), s)
	c.Evaluate(benchInput(st), s)
	afterTwo := c.sess.overCount
	if afterTwo == 0 {
		t.Fatal("expected overCount to climb under a sustained over-cap")
	}
	// Sub-threshold drift (1000 → 1000.4): same session, counter survives.
	drift := st
	drift.Grid.MaxLimitW = 1000.4
	c.Evaluate(benchInput(drift), s)
	if c.sess.overCount < afterTwo {
		t.Errorf("sub-threshold cap drift restarted the session: overCount %d < %d", c.sess.overCount, afterTwo)
	}
	if c.sess.activeLimitW != 1000.4 {
		t.Errorf("session did not follow drift: activeLimitW=%v, want 1000.4", c.sess.activeLimitW)
	}
	// A meaningful change (1000.4 → 5000) resets the session.
	meaningful := st
	meaningful.Grid.MaxLimitW = 5000
	c.Evaluate(benchInput(meaningful), s)
	if c.sess.overCount != 0 {
		t.Errorf("a meaningful cap change must reset overCount: got %d, want 0", c.sess.overCount)
	}
}

// Cap clearing to NaN resets the whole session.
func TestGenConstraint_ClearsOnNoCap(t *testing.T) {
	c, s := newGenPair()
	st := orchestrator.SystemState{
		Solar:       []orchestrator.SolarState{{Name: "pv", PowerW: 4000, MaxW: 5000, Connected: true, Energized: true}},
		Grid:        orchestrator.GridState{NetW: -4000, ExportLimitW: math.NaN(), ImportLimitW: math.NaN(), MaxLimitW: math.NaN()},
		CSIPControl: genLimControl(1000),
	}
	c.Evaluate(benchInput(st), s)
	c.Evaluate(benchInput(st), s)
	// Clear the cap.
	clear := st
	clear.CSIPControl = nil
	demands, breach := c.Evaluate(benchInput(clear), s)
	if len(demands) != 0 || breach != nil {
		t.Errorf("cleared cap emitted %d demands / breach=%v; want none", len(demands), breach)
	}
	if !math.IsNaN(c.sess.activeLimitW) || c.sess.overCount != 0 {
		t.Errorf("session not cleared: activeLimitW=%v overCount=%d", c.sess.activeLimitW, c.sess.overCount)
	}
}

// ── adaptive detection window ─────────────────────────────────────────────────

// The gen-breach window is derived from plant physics, not the fixed constant.
// Bench defaults (control 3 s + meter 5 s over a 3 s tick) yield 3 — bit-identical
// to genBreachTicks — but a slower plant grows the window (the M2 fix).
func TestGenConstraint_AdaptiveDetectionWindow(t *testing.T) {
	c, _ := newGenPair()
	st := orchestrator.SystemState{
		Solar: []orchestrator.SolarState{{Name: "pv", PowerW: 4000, MaxW: 5000, Connected: true, Energized: true}},
		Grid:  orchestrator.GridState{NetW: -4000, ExportLimitW: math.NaN(), ImportLimitW: math.NaN(), MaxLimitW: math.NaN()},
	}
	if got := c.detectionWindowTicks(benchInput(st)); got != 3 {
		t.Errorf("bench-default gen window = %d ticks, want 3 (parity with genBreachTicks)", got)
	}
	slow := benchInput(st)
	slow.Plant.Inverters["pv"] = orchestrator.InverterPlant{ControlLatencyS: 12}
	slow.Plant.Meter = orchestrator.MeterPlant{MeterLagS: 5}
	if got := c.detectionWindowTicks(slow); got <= 3 {
		t.Errorf("slow-plant gen window = %d ticks, want > 3", got)
	}
}
