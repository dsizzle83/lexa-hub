package constraint

import (
	"math"
	"testing"

	"lexa-hub/internal/orchestrator"
)

// Constraint-package twins of the WP-11 gross-load suite in
// internal/orchestrator/auslimits_test.go (TASK-061 pairing discipline — do
// not strip either suite).

func newAusLoadPair() (*AusLoadLimitConstraint, *Session) {
	return NewAusLoadLimitConstraint(), NewSession("load-aus", 0)
}

// chargeFloorOf returns the emitted battery-setpoint LOWER bound (NaN if none).
func chargeFloorOf(demands []Demand, name string) float64 {
	for _, d := range demands {
		if d.Axis == AxisBatterySetpointW && d.Device == name && d.Source == "load-aus" {
			return d.Min
		}
	}
	return math.NaN()
}

// (evCeilingOf — the AxisEVSECurrentA reader — is shared from
// passthrough_test.go.)

// A gross-load cap bounds battery charge from below: charge may not exceed the
// headroom the conservative target leaves over the measured non-battery load.
func TestAusLoadConstraint_EmitsChargeFloor(t *testing.T) {
	c, s := newAusLoadPair()
	// Home 2000 + battery charge 3000 → gross 5000 (netW carries it all: solar
	// 0, discharge 0 → grossLoad = netW = 5000). Cap 4000, conservative 3200,
	// non-battery load 2000 → allowed charge 1200.
	st := orchestrator.SystemState{
		Batteries: []orchestrator.BatteryState{
			{Name: "bat", PowerW: -3000, SOC: 50, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true},
		},
		Grid: ausGrid(5000, math.NaN(), 4000),
	}
	demands, breach := c.Evaluate(benchInput(st), s)
	if breach != nil {
		t.Fatalf("unexpected breach: %+v", breach)
	}
	if got := chargeFloorOf(demands, "bat"); math.Abs(got-(-1200)) > 1 {
		t.Errorf("charge floor = %.0f, want −1200", got)
	}
}

// No cap → no opinion.
func TestAusLoadConstraint_NoCapNoDemands(t *testing.T) {
	c, s := newAusLoadPair()
	st := orchestrator.SystemState{
		Batteries: []orchestrator.BatteryState{
			{Name: "bat", PowerW: -3000, SOC: 50, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true},
		},
		Grid: ausGrid(5000, math.NaN(), math.NaN()),
	}
	demands, breach := c.Evaluate(benchInput(st), s)
	if len(demands) != 0 || breach != nil {
		t.Fatalf("no cap must yield no opinion, got demands=%+v breach=%+v", demands, breach)
	}
}

// The EV lever engages on a hard-cap breach, stays sticky on compliant ticks,
// and relaxes only after the relax window — mirroring the cascade guard.
func TestAusLoadConstraint_EVCurtailSticky(t *testing.T) {
	c, s := newAusLoadPair()
	ev := orchestrator.EVSEState{StationID: "cs1", ConnectorID: 1, Connected: true, SessionActive: true,
		MaxCurrentA: 32, VoltageV: 230, PowerW: 7360}
	mk := func(netW float64, evPowerW float64) Input {
		e := ev
		e.PowerW = evPowerW
		return benchInput(orchestrator.SystemState{
			EVSEs: []orchestrator.EVSEState{e},
			Grid:  ausGrid(netW, math.NaN(), 4000),
		})
	}
	// Tick 1: home 1000 + EV 7360 = 8360 over the 4000 cap → engage.
	demands, _ := c.Evaluate(mk(8360, 7360), s)
	first := evCeilingOf(demands, "cs1#1")
	// conservative 3200 − home 1000 → allowance 2200 W ≈ 9.6 A.
	if math.IsNaN(first) || first > 10 || first < 6 {
		t.Fatalf("first EV ceiling = %v, want ≈9.6 A", first)
	}
	// Tick 2: EV compliant at the ceiling, gross under cap — the ceiling must
	// be RE-EMITTED (sticky), not withdrawn.
	demands, _ = c.Evaluate(mk(1000+first*230, first*230), s)
	held := evCeilingOf(demands, "cs1#1")
	if math.IsNaN(held) || held >= 32 {
		t.Fatalf("tick 2: sticky EV ceiling withdrawn/released: %v", held)
	}
	// Relax is gated on the relax window and bounded per tick.
	var after float64
	for i := 0; i < ausLoadRelaxCycle+2; i++ {
		demands, _ = c.Evaluate(mk(500, 0), s)
		after = evCeilingOf(demands, "cs1#1")
	}
	if after < held {
		t.Fatalf("relax lowered the ceiling (%.1f < %.1f)", after, held)
	}
	if after > held+float64(ausLoadRelaxCycle+2)*ausLoadEVMaxRelax {
		t.Fatalf("relax exceeded the per-tick bound: %.1f from %.1f", after, held)
	}
}

// A meter-blind tick re-emits the sticky EV ceiling (fail-closed) and holds
// every counter.
func TestAusLoadConstraint_MeterBlindHoldsSticky(t *testing.T) {
	c, s := newAusLoadPair()
	ev := orchestrator.EVSEState{StationID: "cs1", ConnectorID: 1, Connected: true, SessionActive: true,
		MaxCurrentA: 32, VoltageV: 230, PowerW: 7360}
	engagedIn := benchInput(orchestrator.SystemState{
		EVSEs: []orchestrator.EVSEState{ev},
		Grid:  ausGrid(8360, math.NaN(), 4000),
	})
	c.Evaluate(engagedIn, s)
	engaged := c.sess.evLimitA
	if math.IsNaN(engaged) {
		t.Fatal("EV lever did not engage")
	}
	blind := benchInput(orchestrator.SystemState{
		EVSEs: []orchestrator.EVSEState{ev},
		Grid:  ausGrid(math.NaN(), math.NaN(), 4000),
	})
	demands, breach := c.Evaluate(blind, s)
	if breach != nil {
		t.Fatalf("meter-blind tick must never breach: %+v", breach)
	}
	if got := evCeilingOf(demands, "cs1#1"); got != engaged {
		t.Fatalf("meter-blind tick must re-emit the sticky ceiling %v, got %v", engaged, got)
	}
	if c.sess.evLimitA != engaged {
		t.Fatalf("meter-blind tick changed the sticky limit: %v → %v", engaged, c.sess.evLimitA)
	}
}

// Unsheddable load over the cap escalates to a "load-aus" breach after the
// detection window; a meter-blind tick mid-run HOLDS the counter (NaN-hold).
func TestAusLoadConstraint_BreachAfterSustained_NaNHolds(t *testing.T) {
	c, s := newAusLoadPair()
	over := benchInput(orchestrator.SystemState{Grid: ausGrid(6000, math.NaN(), 4000)})
	blind := benchInput(orchestrator.SystemState{Grid: ausGrid(math.NaN(), math.NaN(), 4000)})
	threshold := c.detectionWindowTicks(over)

	var breach *orchestrator.ComplianceBreach
	for i := 0; i < threshold-1; i++ {
		_, breach = c.Evaluate(over, s)
	}
	if breach != nil {
		t.Fatalf("breached before the detection window: %+v", breach)
	}
	if _, b := c.Evaluate(blind, s); b != nil {
		t.Fatalf("meter-blind tick must not breach: %+v", b)
	}
	_, breach = c.Evaluate(over, s) // counter held through the blind tick
	if breach == nil || breach.LimitType != "load-aus" {
		t.Fatalf("expected load-aus breach after the held counter reached the window, got %+v", breach)
	}
}

// A meaningful cap change resets the whole session (single reset domain),
// while sub-tolerance drift holds it.
func TestAusLoadConstraint_CapDriftKeepsSession_MeaningfulChangeResets(t *testing.T) {
	c, s := newAusLoadPair()
	mk := func(capW float64) Input {
		return benchInput(orchestrator.SystemState{Grid: ausGrid(6000, math.NaN(), capW)})
	}
	c.Evaluate(mk(4000), s)
	c.Evaluate(mk(4050), s) // sub-tolerance drift: session holds
	if c.sess.breachTicks != 2 {
		t.Fatalf("breachTicks = %d after drift, want 2 (session held)", c.sess.breachTicks)
	}
	c.Evaluate(mk(2000), s) // meaningful change: fresh session
	if c.sess.breachTicks != 1 {
		t.Fatalf("breachTicks = %d after meaningful change, want 1 (session reset)", c.sess.breachTicks)
	}
}

// Cap cleared clears the whole session including the sticky EV limit.
func TestAusLoadConstraint_ClearsOnNoCap(t *testing.T) {
	c, s := newAusLoadPair()
	ev := orchestrator.EVSEState{StationID: "cs1", ConnectorID: 1, Connected: true, SessionActive: true,
		MaxCurrentA: 32, VoltageV: 230, PowerW: 7360}
	c.Evaluate(benchInput(orchestrator.SystemState{
		EVSEs: []orchestrator.EVSEState{ev},
		Grid:  ausGrid(8360, math.NaN(), 4000),
	}), s)
	if math.IsNaN(c.sess.evLimitA) {
		t.Fatal("EV lever did not engage")
	}
	demands, breach := c.Evaluate(benchInput(orchestrator.SystemState{
		EVSEs: []orchestrator.EVSEState{ev},
		Grid:  ausGrid(8360, math.NaN(), math.NaN()),
	}), s)
	if len(demands) != 0 || breach != nil {
		t.Fatalf("cleared cap must yield no opinion, got demands=%+v breach=%+v", demands, breach)
	}
	if !math.IsNaN(c.sess.evLimitA) || c.sess.breachTicks != 0 {
		t.Fatalf("session must clear on no cap: %+v", c.sess)
	}
}
