package constraint

import (
	"math"
	"testing"
	"time"

	"lexa-hub/internal/orchestrator"
)

// Acceptance: the Stack is a drop-in orchestrator.Optimizer.
var _ orchestrator.Optimizer = (*Stack)(nil)

type fakeConstraint struct {
	name    string
	tier    Tier
	demands []Demand
	breach  *orchestrator.ComplianceBreach
	seen    *int // incremented per Evaluate, to prove session/loop wiring
}

func (f fakeConstraint) Name() string { return f.name }
func (f fakeConstraint) Tier() Tier   { return f.tier }
func (f fakeConstraint) Evaluate(in Input, s *Session) ([]Demand, *orchestrator.ComplianceBreach) {
	if f.seen != nil {
		*f.seen++
	}
	return f.demands, f.breach
}

func TestStack_ZeroConstraintsEmptyPlan(t *testing.T) {
	ts := time.Unix(1000, 0)
	plan := NewStack(Plant{}, 0).Optimize(orchestrator.SystemState{Timestamp: ts})
	if !plan.Timestamp.Equal(ts) {
		t.Errorf("timestamp=%v want %v", plan.Timestamp, ts)
	}
	if len(plan.BatteryCommands)+len(plan.SolarCommands)+len(plan.EVSECommands) != 0 {
		t.Errorf("empty stack emitted commands: %+v", plan)
	}
	if plan.Breach != nil || len(plan.Decisions) != 0 {
		t.Errorf("empty stack emitted breach/decisions: %+v", plan)
	}
}

func TestStack_EmitsCommandsPerAxis(t *testing.T) {
	c := true
	cs := []Constraint{
		fakeConstraint{name: "solar", tier: TierCompliance, demands: []Demand{
			CeilingDemand("inv1", AxisSolarCeilingW, 3000, TierCompliance, "solar"),
		}},
		fakeConstraint{name: "batt", tier: TierEconomics, demands: []Demand{
			PointDemand("bat1", AxisBatterySetpointW, -1500, TierEconomics, "batt"),
			{Device: "bat1", Axis: AxisConnect, Connect: &c, Tier: TierEconomics, Source: "batt"},
		}},
		fakeConstraint{name: "ev", tier: TierCompliance, demands: []Demand{
			CeilingDemand("evse1", AxisEVSECurrentA, 16, TierCompliance, "ev"),
		}},
	}
	plan := NewStack(Plant{}, 0, cs...).Optimize(orchestrator.SystemState{Timestamp: time.Unix(1, 0)})

	if len(plan.SolarCommands) != 1 || plan.SolarCommands[0].Name != "inv1" || plan.SolarCommands[0].CurtailToW != 3000 {
		t.Errorf("solar commands wrong: %+v", plan.SolarCommands)
	}
	if len(plan.BatteryCommands) != 1 || plan.BatteryCommands[0].SetpointW != -1500 ||
		plan.BatteryCommands[0].Connect == nil || *plan.BatteryCommands[0].Connect != true {
		t.Errorf("battery commands wrong: %+v", plan.BatteryCommands)
	}
	if len(plan.EVSECommands) != 1 || plan.EVSECommands[0].StationID != "evse1" || plan.EVSECommands[0].MaxCurrentA != 16 {
		t.Errorf("evse commands wrong: %+v", plan.EVSECommands)
	}
}

func TestStack_SolarCeilingNaNRestores(t *testing.T) {
	// An unbounded ceiling (NaN) becomes a restore command (CurtailToW NaN).
	cs := []Constraint{fakeConstraint{name: "s", tier: TierEconomics, demands: []Demand{
		{Device: "inv1", Axis: AxisSolarCeilingW, Min: nan(), Max: nan(), Tier: TierEconomics, Source: "s"},
	}}}
	plan := NewStack(Plant{}, 0, cs...).Optimize(orchestrator.SystemState{Timestamp: time.Unix(1, 0)})
	if len(plan.SolarCommands) != 1 || !math.IsNaN(plan.SolarCommands[0].CurtailToW) {
		t.Errorf("want NaN restore, got %+v", plan.SolarCommands)
	}
}

func TestStack_WorstBreachWins(t *testing.T) {
	small := &orchestrator.ComplianceBreach{LimitType: "export", ShortfallW: 200}
	big := &orchestrator.ComplianceBreach{LimitType: "import", ShortfallW: 900}
	cs := []Constraint{
		fakeConstraint{name: "a", tier: TierCompliance, breach: small},
		fakeConstraint{name: "b", tier: TierCompliance, breach: big},
	}
	plan := NewStack(Plant{}, 0, cs...).Optimize(orchestrator.SystemState{Timestamp: time.Unix(1, 0)})
	if plan.Breach == nil || plan.Breach.ShortfallW != 900 {
		t.Fatalf("breach=%+v want worst shortfall 900", plan.Breach)
	}
}

func TestStack_ConflictEmitsDecision(t *testing.T) {
	cs := []Constraint{fakeConstraint{name: "c", tier: TierCompliance, demands: []Demand{
		{Device: "inv1", Axis: AxisSolarCeilingW, Min: nan(), Max: 1000, Tier: TierCompliance, Source: "a"},
		{Device: "inv1", Axis: AxisSolarCeilingW, Min: 2000, Max: nan(), Tier: TierCompliance, Source: "b"},
	}}}
	plan := NewStack(Plant{}, 0, cs...).Optimize(orchestrator.SystemState{Timestamp: time.Unix(1, 0)})
	if len(plan.Decisions) != 1 || plan.Decisions[0].Rule != "constraint-arbiter" {
		t.Fatalf("want 1 arbiter decision, got %+v", plan.Decisions)
	}
}

func TestStack_SessionPersistsAcrossTicks(t *testing.T) {
	seen := 0
	stack := NewStack(Plant{}, 15*time.Second, fakeConstraint{name: "s", tier: TierCompliance, seen: &seen})
	// The same Session instance must be reused tick over tick.
	first := stack.sessions["s"]
	stack.Optimize(orchestrator.SystemState{Timestamp: time.Unix(1, 0)})
	stack.Optimize(orchestrator.SystemState{Timestamp: time.Unix(2, 0)})
	if seen != 2 {
		t.Errorf("Evaluate called %d times want 2", seen)
	}
	if stack.sessions["s"] != first {
		t.Errorf("session replaced between ticks")
	}
	// Cadence propagated to the session so ScaleTicks holds wall-clock meaning.
	if first.ScaleTicks(20) != 4 {
		t.Errorf("session cadence not propagated: ScaleTicks(20)=%d want 4", first.ScaleTicks(20))
	}
}

func TestStack_ZeroTimestampFallsBack(t *testing.T) {
	plan := NewStack(Plant{}, 0).Optimize(orchestrator.SystemState{})
	if plan.Timestamp.IsZero() {
		t.Errorf("zero-timestamp state should fall back to now()")
	}
}
