package constraint

import (
	"math"
	"testing"

	"lexa-hub/internal/orchestrator"
	model "lexa-proto/csipmodel"
)

// Constraint-package twins of the import-limit suite in internal/orchestrator
// (convergence_test.go:236-361, optimizer_compliance_test.go:122-323). Each drives
// ImportLimitConstraint.Evaluate through the same tick sequence and asserts the SAME
// behavioral outcome. White-box so the single-domain session and the NaN-hold
// counter can be inspected directly for the mutation checks.

func newImportPair() (*ImportLimitConstraint, *Session) {
	return NewImportLimitConstraint(NewEVImportCooldown()), NewSession("import", 0)
}

func impLimControl(w int16) *orchestrator.CSIPControlState {
	return &orchestrator.CSIPControlState{
		Source: "event", MRID: "imp-mrid",
		Base: model.DERControlBase{OpModImpLimW: &model.ActivePower{Value: w, Multiplier: 0}},
	}
}

// dischargeOf returns the emitted positive battery setpoint (NaN if none).
func dischargeOf(demands []Demand, name string) float64 {
	return setpointOf(demands, name)
}

// ── discharge controller ──────────────────────────────────────────────────────

// Twin of TestImportLimit_RaisesPlanBatterySetpoint: import 2300 W over a 1600 W
// cap with 66% SOC available — the battery must discharge well above the soft plan
// setpoint to defend the cap (target ≈ unconstrained − conservative ≈ 1300 W).
func TestImportConstraint_DischargesToDefendCap(t *testing.T) {
	c, s := newImportPair()
	st := orchestrator.SystemState{
		Batteries: []orchestrator.BatteryState{{
			Name: "bat", PowerW: 0, SOC: 66, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true,
		}},
		Grid:        orchestrator.GridState{NetW: 2300, ExportLimitW: math.NaN(), ImportLimitW: math.NaN(), MaxLimitW: math.NaN()},
		CSIPControl: impLimControl(1600),
	}
	demands, _ := c.Evaluate(benchInput(st), s)
	setpoint := dischargeOf(demands, "bat")
	if math.IsNaN(setpoint) {
		t.Fatal("no battery discharge demand issued")
	}
	if setpoint < 1000 {
		t.Errorf("battery discharge = %.0fW; want > 1000W to defend the 1600W import cap", setpoint)
	}
}

// No cap → no opinion.
func TestImportConstraint_NoCapNoDemands(t *testing.T) {
	c, s := newImportPair()
	st := orchestrator.SystemState{
		Batteries: []orchestrator.BatteryState{{Name: "bat", PowerW: 0, SOC: 66, MaxDischargeW: 5000, Connected: true, Energized: true}},
		Grid:      orchestrator.GridState{NetW: 2300, ExportLimitW: math.NaN(), ImportLimitW: math.NaN(), MaxLimitW: math.NaN()},
	}
	demands, breach := c.Evaluate(benchInput(st), s)
	if len(demands) != 0 || breach != nil {
		t.Errorf("no-cap tick emitted %d demands / breach=%v; want none", len(demands), breach)
	}
}

// Sticky discharge: once ratcheted up, the controller holds the discharge across a
// dip inside relaxCycles rather than releasing (anti-oscillation).
func TestImportConstraint_StickyDischarge_HoldsThroughDip(t *testing.T) {
	c, s := newImportPair()
	over := orchestrator.SystemState{
		Batteries:   []orchestrator.BatteryState{{Name: "bat", PowerW: 0, SOC: 80, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true}},
		Grid:        orchestrator.GridState{NetW: 2500, ExportLimitW: math.NaN(), ImportLimitW: math.NaN(), MaxLimitW: math.NaN()},
		CSIPControl: impLimControl(1000),
	}
	demands, _ := c.Evaluate(benchInput(over), s)
	ratcheted := dischargeOf(demands, "bat")
	if math.IsNaN(ratcheted) || ratcheted < 1000 {
		t.Fatalf("expected a ratcheted discharge to defend the cap, got %.0f", ratcheted)
	}
	// Next tick the battery is now discharging (measured), import momentarily lands
	// under the cap — but safeCount < relaxCycles, so the discharge must HOLD.
	dip := over
	dip.Batteries = []orchestrator.BatteryState{{Name: "bat", PowerW: ratcheted, SOC: 80, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true}}
	dip.Grid.NetW = 800 // under cap thanks to the discharge
	demands, _ = c.Evaluate(benchInput(dip), s)
	held := dischargeOf(demands, "bat")
	if math.IsNaN(held) || held < ratcheted-1 {
		t.Errorf("discharge released to %.0f on a single under-cap dip; must stay sticky near %.0f", held, ratcheted)
	}
}

// ── battery-headroom breach (twin of TestImportLimit_BatteryFloored_ReportsBreach) ─

// Battery at its SOC reserve (no discharge headroom) and import over the cap: the
// cap is physically unmeetable, so a CannotComply breach must fire immediately.
func TestImportConstraint_BatteryFloored_ReportsBreach(t *testing.T) {
	c, s := newImportPair()
	st := orchestrator.SystemState{
		Solar: []orchestrator.SolarState{{Name: "pv", PowerW: 0, MaxW: 8000, Connected: true, Energized: true}},
		Batteries: []orchestrator.BatteryState{{
			Name: "bat", PowerW: 0, SOC: 20, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true,
		}},
		Grid:        orchestrator.GridState{NetW: 2500, ExportLimitW: math.NaN(), ImportLimitW: math.NaN(), MaxLimitW: math.NaN()},
		CSIPControl: impLimControl(1700),
	}
	_, breach := c.Evaluate(benchInput(st), s)
	if breach == nil {
		t.Fatal("expected an import breach (cap unmeetable with battery at reserve), got none")
	}
	if breach.LimitType != "import" {
		t.Errorf("breach LimitType = %q, want import", breach.LimitType)
	}
	if breach.MRID != "imp-mrid" {
		t.Errorf("breach MRID = %q, want imp-mrid", breach.MRID)
	}
	if breach.ShortfallW <= 0 {
		t.Errorf("breach ShortfallW = %.0f, want > 0", breach.ShortfallW)
	}
}

// Twin of TestImportLimit_BatteryHasHeadroom_NoBreach: with charge available the
// rule discharges to hold the cap, so no breach.
func TestImportConstraint_BatteryHasHeadroom_NoBreach(t *testing.T) {
	c, s := newImportPair()
	st := orchestrator.SystemState{
		Solar: []orchestrator.SolarState{{Name: "pv", PowerW: 0, MaxW: 8000, Connected: true, Energized: true}},
		Batteries: []orchestrator.BatteryState{{
			Name: "bat", PowerW: 0, SOC: 80, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true,
		}},
		Grid:        orchestrator.GridState{NetW: 2500, ExportLimitW: math.NaN(), ImportLimitW: math.NaN(), MaxLimitW: math.NaN()},
		CSIPControl: impLimControl(1700),
	}
	if _, breach := c.Evaluate(benchInput(st), s); breach != nil {
		t.Errorf("unexpected breach: %+v (battery had discharge headroom)", breach)
	}
}

// ── convergence counter: leaky + NaN-HOLD + mutation ──────────────────────────

// importOverCapState isolates the convergence backstop from applyImportControl's
// own headroom breach. Cap 1000 W; the battery has plenty of headroom (SOC 80) but
// its measured PowerW stays 0 — i.e. the device ACKs the small commanded discharge
// (~630 W) yet never actually draws down, so `remaining` collapses to 0 (no
// headroom breach) while the METER stays over the cap. That is exactly the
// "load/device is not honouring the command" case checkImportConvergence exists to
// catch, and it lets these tests exercise the leaky/NaN-hold counter in isolation,
// the way the legacy convergence_test.go calls checkImportConvergence directly.
func importOverCapState(netW float64) orchestrator.SystemState {
	return orchestrator.SystemState{
		Batteries: []orchestrator.BatteryState{{
			Name: "bat", PowerW: 0, SOC: 80, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true,
		}},
		Grid:        orchestrator.GridState{NetW: netW, ExportLimitW: math.NaN(), ImportLimitW: 1000, MaxLimitW: math.NaN()},
		CSIPControl: impLimControl(1000),
	}
}

// Twin of TestCheckImportConvergence_NaNTickHoldsCounter: a meter-blind tick (netW
// NaN) HOLDS the counter — evidence of neither breach nor compliance.
//
// THIS IS THE RECORDED MUTATION GUARD for NaN-hold: replace the `if IsNaN(netW) {
// return nil }` early-return in checkConvergence with a drain, and the blind tick
// resets/decrements the counter so the next over-cap tick no longer completes the
// escalation, and this test goes red (verified by hand-mutation, TASK-061 report).
func TestImportConstraint_NaNTickHoldsCounter(t *testing.T) {
	c, s := newImportPair()
	// Threshold is 3 at bench defaults; climb to threshold−1, blind tick, then over.
	c.Evaluate(benchInput(importOverCapState(1430)), s)
	c.Evaluate(benchInput(importOverCapState(1430)), s)
	held := c.sess.breachTicks
	if held == 0 {
		t.Fatal("expected breachTicks to climb under a sustained over-cap")
	}
	c.Evaluate(benchInput(importOverCapState(math.NaN())), s) // blind — must HOLD
	if c.sess.breachTicks != held {
		t.Fatalf("breachTicks = %d after a blind tick; want it HELD at %d (NaN-hold)", c.sess.breachTicks, held)
	}
	_, breach := c.Evaluate(benchInput(importOverCapState(1430)), s) // completes the count
	if breach == nil {
		t.Error("a meter-blind tick must hold the breach counter, not reset it — escalation lost")
	}
}

// Twin of TestCheckImportConvergence_CompliantTickDrainsOneCount: the counter is
// leaky — a compliant blip drains one count, so a sustained breach with one blip
// still escalates, but a lone blip cannot restart a climbing breach from scratch.
func TestImportConstraint_LeakyCounterDrainsOneCount(t *testing.T) {
	c, s := newImportPair()
	c.Evaluate(benchInput(importOverCapState(1430)), s)
	c.Evaluate(benchInput(importOverCapState(1430)), s) // breachTicks = 2 (threshold−1)
	c.Evaluate(benchInput(importOverCapState(0)), s)    // compliant → drains to 1
	_, breach := c.Evaluate(benchInput(importOverCapState(1430)), s)
	if breach != nil {
		t.Error("breach fired before the leaky counter re-reached the threshold")
	}
	_, breach = c.Evaluate(benchInput(importOverCapState(1430)), s)
	if breach == nil {
		t.Error("a sustained breach with one compliant blip must still escalate")
	}
}

// Twin of TestApplyImportLimitRule_CapDriftKeepsGuardSession: sub-threshold decode
// drift keeps the session (breachTicks survives); a meaningful change resets it.
func TestImportConstraint_CapDriftKeepsSession_MeaningfulChangeResets(t *testing.T) {
	c, s := newImportPair()
	// Battery with headroom (SOC 80) but measured PowerW 0: over-cap import climbs
	// the convergence counter without tripping the headroom breach, so breachTicks
	// accumulates cleanly. Cap 3000, importing 4000.
	over := func(cap float64) orchestrator.SystemState {
		return orchestrator.SystemState{
			Batteries: []orchestrator.BatteryState{{Name: "bat", PowerW: 0, SOC: 80, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true}},
			Grid:      orchestrator.GridState{NetW: 4000, ExportLimitW: math.NaN(), ImportLimitW: cap, MaxLimitW: math.NaN()},
		}
	}
	c.Evaluate(benchInput(over(3000)), s) // breachTicks 0→1
	c.Evaluate(benchInput(over(3000)), s) // breachTicks 1→2
	if c.sess.breachTicks != 2 {
		t.Fatalf("setup: breachTicks=%d, want 2", c.sess.breachTicks)
	}
	// Sub-threshold drift (3000 → 3000.4): same session — breachTicks must SURVIVE
	// (not reset to 0), so this over-cap tick advances it to 3 rather than 1.
	c.Evaluate(benchInput(over(3000.4)), s)
	if c.sess.breachTicks != 3 {
		t.Errorf("sub-threshold cap drift restarted the session: breachTicks=%d, want 3 (2 survived + 1)", c.sess.breachTicks)
	}
	if c.sess.activeLimitW != 3000.4 {
		t.Errorf("session did not follow drift: activeLimitW=%v, want 3000.4", c.sess.activeLimitW)
	}
	// A genuinely new cap (→ 5000, now compliant at import 4000) resets the session.
	c.Evaluate(benchInput(over(5000)), s)
	if c.sess.breachTicks != 0 {
		t.Errorf("a meaningful cap change must reset the session: breachTicks=%d, want 0", c.sess.breachTicks)
	}
}

// Cap clearing to NaN resets the whole session.
func TestImportConstraint_ClearsOnNoCap(t *testing.T) {
	c, s := newImportPair()
	c.Evaluate(benchInput(importOverCapState(1430)), s)
	c.Evaluate(benchInput(importOverCapState(1430)), s)
	clear := importOverCapState(1430)
	clear.Grid.ImportLimitW = math.NaN()
	clear.CSIPControl = nil
	demands, breach := c.Evaluate(benchInput(clear), s)
	if len(demands) != 0 || breach != nil {
		t.Errorf("cleared cap emitted %d demands / breach=%v; want none", len(demands), breach)
	}
	if !math.IsNaN(c.sess.activeLimitW) || c.sess.breachTicks != 0 || !math.IsNaN(c.sess.dischargeW) {
		t.Errorf("session not cleared: %+v", c.sess)
	}
}

// ── adaptive detection window (battery-lever) ─────────────────────────────────

// The import-breach window is derived from the battery control latency + meter lag.
// Bench defaults yield 3 — bit-identical to importBreachTicks — but a slower battery
// grows the window.
func TestImportConstraint_AdaptiveDetectionWindow(t *testing.T) {
	c, _ := newImportPair()
	st := orchestrator.SystemState{
		Batteries: []orchestrator.BatteryState{{Name: "bat", PowerW: 0, SOC: 50, MaxDischargeW: 5000, Connected: true, Energized: true}},
		Grid:      orchestrator.GridState{NetW: 1430, ExportLimitW: math.NaN(), ImportLimitW: 0, MaxLimitW: math.NaN()},
	}
	if got := c.detectionWindowTicks(benchInput(st)); got != 3 {
		t.Errorf("bench-default import window = %d ticks, want 3 (parity with importBreachTicks)", got)
	}
	slow := benchInput(st)
	slow.Plant.Batteries["bat"] = orchestrator.BatteryPlant{ControlLatencyS: 12}
	slow.Plant.Meter = orchestrator.MeterPlant{MeterLagS: 5}
	if got := c.detectionWindowTicks(slow); got <= 3 {
		t.Errorf("slow-plant import window = %d ticks, want > 3", got)
	}
}
