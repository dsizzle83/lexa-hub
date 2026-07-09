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

// TestEmitCommands_ConnectFanOutPerClass pins the Unit 3.6 AxisConnect fan-out:
// a resolved Desired.Connect is routed onto the SAME class its value axis names
// (solar/EVSE/battery), never spuriously onto a battery command for a solar or
// EVSE device — closing the 3.4/3.5-documented gap. It also proves nil-safety
// (a nil Connect leaves the class command's Connect nil) and that the standalone
// bare-connect battery path (the Tier-1 safety force-disconnect) is unchanged.
func TestEmitCommands_ConnectFanOutPerClass(t *testing.T) {
	solarCeil := func(max float64) map[Axis]Interval {
		return map[Axis]Interval{AxisSolarCeilingW: {Min: nan(), Max: max}}
	}
	evseCur := func(max float64) map[Axis]Interval {
		return map[Axis]Interval{AxisEVSECurrentA: {Min: nan(), Max: max}}
	}
	battSet := func(w float64) map[Axis]Interval {
		return map[Axis]Interval{AxisBatterySetpointW: {Min: w, Max: w}}
	}

	tests := []struct {
		name    string
		desired map[string]Desired
		check   func(t *testing.T, plan orchestrator.Plan)
	}{
		{
			name: "solar ceiling + disconnect → SolarCommand.Connect, no battery command",
			desired: map[string]Desired{
				"inv1": {Device: "inv1", Bounds: solarCeil(3000), Connect: boolPtr(false)},
			},
			check: func(t *testing.T, plan orchestrator.Plan) {
				if len(plan.SolarCommands) != 1 || plan.SolarCommands[0].Connect == nil || *plan.SolarCommands[0].Connect {
					t.Fatalf("want SolarCommand.Connect=false, got %+v", plan.SolarCommands)
				}
				if plan.SolarCommands[0].CurtailToW != 3000 {
					t.Fatalf("solar ceiling lost: %+v", plan.SolarCommands)
				}
				if len(plan.BatteryCommands) != 0 {
					t.Fatalf("solar device must not emit a battery command: %+v", plan.BatteryCommands)
				}
			},
		},
		{
			name: "evse current + disconnect → EVSECommand.Connect, no battery command",
			desired: map[string]Desired{
				"cs-1": {Device: "cs-1", Bounds: evseCur(16), Connect: boolPtr(false)},
			},
			check: func(t *testing.T, plan orchestrator.Plan) {
				if len(plan.EVSECommands) != 1 || plan.EVSECommands[0].Connect == nil || *plan.EVSECommands[0].Connect {
					t.Fatalf("want EVSECommand.Connect=false, got %+v", plan.EVSECommands)
				}
				if plan.EVSECommands[0].MaxCurrentA != 16 || plan.EVSECommands[0].StationID != "cs-1" {
					t.Fatalf("evse limit/station lost: %+v", plan.EVSECommands)
				}
				if len(plan.BatteryCommands) != 0 {
					t.Fatalf("evse device must not emit a battery command: %+v", plan.BatteryCommands)
				}
			},
		},
		{
			name: "battery setpoint + disconnect → BatteryCommand carries both",
			desired: map[string]Desired{
				"bat1": {Device: "bat1", Bounds: battSet(-1500), Connect: boolPtr(false)},
			},
			check: func(t *testing.T, plan orchestrator.Plan) {
				if len(plan.BatteryCommands) != 1 {
					t.Fatalf("want 1 battery command, got %+v", plan.BatteryCommands)
				}
				c := plan.BatteryCommands[0]
				if c.SetpointW != -1500 || c.Connect == nil || *c.Connect {
					t.Fatalf("want battery {setpoint -1500, connect false}, got %+v", c)
				}
			},
		},
		{
			name: "bare connect (no value axis) → battery force-disconnect path unchanged",
			desired: map[string]Desired{
				"bat1": {Device: "bat1", Bounds: map[Axis]Interval{}, Connect: boolPtr(false)},
			},
			check: func(t *testing.T, plan orchestrator.Plan) {
				if len(plan.SolarCommands) != 0 || len(plan.EVSECommands) != 0 {
					t.Fatalf("bare connect must not touch solar/evse: %+v", plan)
				}
				if len(plan.BatteryCommands) != 1 || plan.BatteryCommands[0].Connect == nil || *plan.BatteryCommands[0].Connect {
					t.Fatalf("bare connect must emit a battery disconnect: %+v", plan.BatteryCommands)
				}
				if !math.IsNaN(plan.BatteryCommands[0].SetpointW) {
					t.Fatalf("bare connect battery setpoint must stay NaN (leave unchanged): %+v", plan.BatteryCommands[0])
				}
			},
		},
		{
			name: "nil connect is class-safe: solar ceiling only, no battery command",
			desired: map[string]Desired{
				"inv1": {Device: "inv1", Bounds: solarCeil(2000), Connect: nil},
			},
			check: func(t *testing.T, plan orchestrator.Plan) {
				if len(plan.SolarCommands) != 1 || plan.SolarCommands[0].Connect != nil {
					t.Fatalf("nil connect must leave SolarCommand.Connect nil: %+v", plan.SolarCommands)
				}
				if len(plan.BatteryCommands) != 0 {
					t.Fatalf("nil-connect solar must not emit a battery command: %+v", plan.BatteryCommands)
				}
			},
		},
		{
			name: "nil connect is class-safe: evse current only, no battery command",
			desired: map[string]Desired{
				"cs-1": {Device: "cs-1", Bounds: evseCur(24), Connect: nil},
			},
			check: func(t *testing.T, plan orchestrator.Plan) {
				if len(plan.EVSECommands) != 1 || plan.EVSECommands[0].Connect != nil {
					t.Fatalf("nil connect must leave EVSECommand.Connect nil: %+v", plan.EVSECommands)
				}
				if len(plan.BatteryCommands) != 0 {
					t.Fatalf("nil-connect evse must not emit a battery command: %+v", plan.BatteryCommands)
				}
			},
		},
		{
			name: "connect true fans onto solar and evse",
			desired: map[string]Desired{
				"inv1": {Device: "inv1", Bounds: solarCeil(4000), Connect: boolPtr(true)},
				"cs-1": {Device: "cs-1", Bounds: evseCur(32), Connect: boolPtr(true)},
			},
			check: func(t *testing.T, plan orchestrator.Plan) {
				if len(plan.SolarCommands) != 1 || plan.SolarCommands[0].Connect == nil || !*plan.SolarCommands[0].Connect {
					t.Fatalf("want SolarCommand.Connect=true, got %+v", plan.SolarCommands)
				}
				if len(plan.EVSECommands) != 1 || plan.EVSECommands[0].Connect == nil || !*plan.EVSECommands[0].Connect {
					t.Fatalf("want EVSECommand.Connect=true, got %+v", plan.EVSECommands)
				}
				if len(plan.BatteryCommands) != 0 {
					t.Fatalf("no battery device present: %+v", plan.BatteryCommands)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var plan orchestrator.Plan
			emitCommands(&plan, tt.desired)
			tt.check(t, plan)
		})
	}
}
