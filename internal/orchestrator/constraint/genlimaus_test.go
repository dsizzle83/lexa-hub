package constraint

import (
	"math"
	"testing"
	"time"

	"lexa-hub/internal/orchestrator"
)

// Constraint-package twins of the WP-11 gross-generation suite in
// internal/orchestrator/auslimits_test.go. Each drives
// AusGenLimitConstraint.Evaluate through the same tick sequences the cascade
// tests drive applyAusGenerationLimitRule/checkAusGenerationConvergence
// through and asserts the SAME behavioural outcome, pinning the mirror against
// the cascade's contract before any flip (the TASK-061 pairing discipline —
// do not strip either suite).

func newAusGenPair() (*AusGenLimitConstraint, *Session) {
	return NewAusGenLimitConstraint(), NewSession("gen-aus", 0)
}

// ausGrid returns a GridState with the AUS caps set and everything else NaN.
func ausGrid(netW, genLimW, loadLimW float64) orchestrator.GridState {
	g := orchestrator.NewGridState()
	g.NetW = netW
	g.GenLimitW = genLimW
	g.LoadLimitW = loadLimW
	return g
}

// batteryCeilingOf returns the emitted battery-setpoint UPPER bound (NaN if none).
func batteryCeilingOf(demands []Demand, name string) float64 {
	for _, d := range demands {
		if d.Axis == AxisBatterySetpointW && d.Device == name && d.Source == "gen-aus" {
			return d.Max
		}
	}
	return math.NaN()
}

// A gross-generation cap emits the full cap as a nameplate-distributed solar
// ceiling plus a battery-discharge participation ceiling (cap − measured solar).
func TestAusGenConstraint_EmitsCeilingAndDischargeCap(t *testing.T) {
	c, s := newAusGenPair()
	st := orchestrator.SystemState{
		Solar: []orchestrator.SolarState{
			{Name: "pvA", PowerW: 3000, MaxW: 6000, Connected: true, Energized: true},
			{Name: "pvB", PowerW: 1000, MaxW: 2000, Connected: true, Energized: true},
		},
		Batteries: []orchestrator.BatteryState{
			{Name: "bat", PowerW: 0, SOC: 60, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true},
		},
		Grid: ausGrid(-3000, 5000, math.NaN()),
	}
	demands, breach := c.Evaluate(benchInput(st), s)
	if breach != nil {
		t.Fatalf("unexpected breach: %+v", breach)
	}
	// Solar ceilings: full 5000 W cap by nameplate share (6000/8000, 2000/8000).
	if got := ceilingOf(demands, "pvA"); math.Abs(got-3750) > 1 {
		t.Errorf("pvA ceiling = %.0f, want 3750", got)
	}
	if got := ceilingOf(demands, "pvB"); math.Abs(got-1250) > 1 {
		t.Errorf("pvB ceiling = %.0f, want 1250", got)
	}
	// Battery participation cap: cap 5000 − measured solar 4000 = 1000.
	if got := batteryCeilingOf(demands, "bat"); math.Abs(got-1000) > 1 {
		t.Errorf("battery discharge ceiling = %.0f, want 1000", got)
	}
}

// Solar at/over the cap leaves ZERO discharge headroom — the participation cap
// must clamp to 0, never negative.
func TestAusGenConstraint_SolarSaturatedZeroDischargeHeadroom(t *testing.T) {
	c, s := newAusGenPair()
	st := orchestrator.SystemState{
		Solar: []orchestrator.SolarState{{Name: "pv", PowerW: 4000, MaxW: 5000, Connected: true, Energized: true}},
		Batteries: []orchestrator.BatteryState{
			{Name: "bat", PowerW: 0, SOC: 60, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true},
		},
		Grid: ausGrid(-3000, 3000, math.NaN()),
	}
	demands, _ := c.Evaluate(benchInput(st), s)
	if got := batteryCeilingOf(demands, "bat"); got != 0 {
		t.Errorf("battery discharge ceiling = %v, want 0 (solar saturates the gross cap)", got)
	}
}

// No cap → no opinion (empty demands, no breach) so the candidate-scoped
// shadow diff stays inert.
func TestAusGenConstraint_NoCapNoDemands(t *testing.T) {
	c, s := newAusGenPair()
	st := orchestrator.SystemState{
		Solar: []orchestrator.SolarState{{Name: "pv", PowerW: 4000, MaxW: 5000, Connected: true, Energized: true}},
		Grid:  ausGrid(-3000, math.NaN(), math.NaN()),
	}
	demands, breach := c.Evaluate(benchInput(st), s)
	if len(demands) != 0 || breach != nil {
		t.Fatalf("no cap must yield no opinion, got demands=%+v breach=%+v", demands, breach)
	}
}

// Battery discharge counts toward the GROSS cap: solar alone under the cap but
// solar+discharge over it must breach "generation-aus" — the distinction from
// the "gen" (opModMaxLimW) constraint, whose floor nets discharge out.
func TestAusGenConstraint_BatteryDischargeCountsTowardCap(t *testing.T) {
	c, s := newAusGenPair()
	st := orchestrator.SystemState{
		Solar: []orchestrator.SolarState{{Name: "pv", PowerW: 800, MaxW: 5000, Connected: true, Energized: true}},
		Batteries: []orchestrator.BatteryState{
			{Name: "bat", PowerW: 2000, SOC: 60, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true},
		},
		Grid: ausGrid(-2000, 1000, math.NaN()),
	}
	in := benchInput(st)
	threshold := c.detectionWindowTicks(in)
	var breach *orchestrator.ComplianceBreach
	for i := 0; i < threshold; i++ {
		_, breach = c.Evaluate(in, s)
	}
	if breach == nil || breach.LimitType != "generation-aus" {
		t.Fatalf("expected generation-aus breach (gross gen includes discharge), got %+v", breach)
	}
}

// The ADAPTED meter floor (grossGen ≥ −netW, no discharge subtraction) catches
// devices that echo compliant self-reports while the site exports over the cap.
func TestAusGenConstraint_MeterFloorCatchesEchoedReports(t *testing.T) {
	c, s := newAusGenPair()
	st := orchestrator.SystemState{
		Solar: []orchestrator.SolarState{{Name: "pv", PowerW: 500, MaxW: 5000, Connected: true, Energized: true}},
		Batteries: []orchestrator.BatteryState{
			{Name: "bat", PowerW: 0, SOC: 60, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true},
		},
		Grid: ausGrid(-4500, 1000, math.NaN()),
	}
	in := benchInput(st)
	threshold := c.detectionWindowTicks(in)
	var breach *orchestrator.ComplianceBreach
	for i := 0; i < threshold; i++ {
		_, breach = c.Evaluate(in, s)
	}
	if breach == nil || breach.LimitType != "generation-aus" {
		t.Fatalf("expected the meter floor to catch echoed reports, got %+v", breach)
	}
}

// A single under-cap blip decrements (leaky) rather than resetting the counter.
func TestAusGenConstraint_LeakyCounterToleratesBlip(t *testing.T) {
	c, s := newAusGenPair()
	mk := func(genW float64) Input {
		return benchInput(orchestrator.SystemState{
			Solar: []orchestrator.SolarState{{Name: "pv", PowerW: genW, MaxW: 5000, Connected: true, Energized: true}},
			Grid:  ausGrid(math.NaN(), 1000, math.NaN()),
		})
	}
	breached := false
	for _, genW := range []float64{3000, 3000, 500, 3000, 3000, 3000} {
		if _, b := c.Evaluate(mk(genW), s); b != nil {
			breached = true
		}
	}
	if !breached {
		t.Fatal("sustained breach with a single blip never escalated — counter must be leaky")
	}
}

// A sub-tolerance cap drift keeps the session (counter climbs); a meaningful
// change resets it — the tolerance-band session rule.
func TestAusGenConstraint_CapDriftKeepsSession_MeaningfulChangeResets(t *testing.T) {
	c, s := newAusGenPair()
	mk := func(capW float64) Input {
		return benchInput(orchestrator.SystemState{
			Solar: []orchestrator.SolarState{{Name: "pv", PowerW: 3000, MaxW: 5000, Connected: true, Energized: true}},
			Grid:  ausGrid(math.NaN(), capW, math.NaN()),
		})
	}
	c.Evaluate(mk(1000), s)
	c.Evaluate(mk(1050), s) // sub-tolerance drift (≤ 100 W band): session holds
	if c.sess.overCount != 2 {
		t.Fatalf("overCount = %d after drift, want 2 (session held)", c.sess.overCount)
	}
	c.Evaluate(mk(2000), s) // meaningful change: fresh session
	if c.sess.overCount != 1 {
		t.Fatalf("overCount = %d after meaningful change, want 1 (session reset)", c.sess.overCount)
	}
}

// Cap cleared (NaN) clears the whole session.
func TestAusGenConstraint_ClearsOnNoCap(t *testing.T) {
	c, s := newAusGenPair()
	over := benchInput(orchestrator.SystemState{
		Solar: []orchestrator.SolarState{{Name: "pv", PowerW: 3000, MaxW: 5000, Connected: true, Energized: true}},
		Grid:  ausGrid(math.NaN(), 1000, math.NaN()),
	})
	c.Evaluate(over, s)
	c.Evaluate(over, s)
	clear := benchInput(orchestrator.SystemState{
		Solar: []orchestrator.SolarState{{Name: "pv", PowerW: 3000, MaxW: 5000, Connected: true, Energized: true}},
		Grid:  ausGrid(math.NaN(), math.NaN(), math.NaN()),
	})
	c.Evaluate(clear, s)
	if c.sess.overCount != 0 || !math.IsNaN(c.sess.activeLimitW) {
		t.Fatalf("session must clear on no cap: %+v", c.sess)
	}
}

// The breach carries the active control's MRID (stamped in Evaluate).
func TestAusGenConstraint_BreachCarriesMRID(t *testing.T) {
	c, s := newAusGenPair()
	st := orchestrator.SystemState{
		Timestamp:   time.Unix(1700000000, 0),
		Solar:       []orchestrator.SolarState{{Name: "pv", PowerW: 3000, MaxW: 5000, Connected: true, Energized: true}},
		Grid:        ausGrid(math.NaN(), 1000, math.NaN()),
		CSIPControl: &orchestrator.CSIPControlState{Source: "event", MRID: "aus-gen-mrid"},
	}
	in := benchInput(st)
	threshold := c.detectionWindowTicks(in)
	var breach *orchestrator.ComplianceBreach
	for i := 0; i < threshold; i++ {
		_, breach = c.Evaluate(in, s)
	}
	if breach == nil || breach.MRID != "aus-gen-mrid" {
		t.Fatalf("breach must carry the active MRID, got %+v", breach)
	}
}
