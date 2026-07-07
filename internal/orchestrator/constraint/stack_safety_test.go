package constraint

import (
	"math"
	"testing"

	"lexa-hub/internal/orchestrator"
)

// These pin the TASK-063 safety wiring the Stack adds on top of TASK-062's
// BatterySafetyConstraint: the post-arbitration ordering (closing the ≤1-tick
// wrong-direction lag) and the RecordCommands call site feeding the fast loop.

// safetyStack builds a minimal stack — economics (to author a charge) + battery
// safety (post-arbitration) — at the tuned tick.
func safetyStack() *Stack {
	return NewStack(Plant{Meter: orchestrator.MeterPlant{}.WithDefaults()}, 0,
		NewBatterySafetyConstraint(benchSOCReserve),
		NewEconomicsConstraint(orchestrator.DefaultTOUCostModel(),
			benchSOCReserve, benchSOCFull, benchExcessSolar, benchExportMargin, benchEVCooldown))
}

// A pack commanded to CHARGE this tick (economics self-consumption resolves a
// negative setpoint) but MEASURED discharging hard at/near its reserve is a
// critical sign inversion. Because battery safety runs POST-arbitration, it reads
// THIS tick's resolved charge command and trips on the SAME tick — the lag TASK-062
// left (a pre-arbitration Evaluate reads last-committed, which is empty on tick 1
// and would NOT trip) is closed.
func TestStack_BatterySafetyPostArbitrationClosesLag(t *testing.T) {
	st := orchestrator.SystemState{
		Timestamp: offPeakTime(),
		Solar:     []orchestrator.SolarState{{Name: "pv", PowerW: 6000, MaxW: 8000, Connected: true, Energized: true}},
		// Measured discharging 4000 W at SOC 22% (≤ reserve+5) while economics will
		// command a charge — the unambiguous inversion.
		Batteries: []orchestrator.BatteryState{{Name: "bat", PowerW: 4000, SOC: 22, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true}},
		Grid:      orchestrator.GridState{NetW: -5000, ImportLimitW: math.NaN(), ExportLimitW: math.NaN(), MaxLimitW: math.NaN()},
	}
	plan := safetyStack().Optimize(st)

	i := -1
	for idx, c := range plan.BatteryCommands {
		if c.Name == "bat" {
			i = idx
		}
	}
	if i < 0 {
		t.Fatal("no battery command emitted")
	}
	cmd := plan.BatteryCommands[i]
	if cmd.Connect == nil || *cmd.Connect != false || cmd.SetpointW != 0 {
		t.Fatalf("battery cmd = %+v, want force-disconnect {0, false} on the SAME tick (post-arbitration safety)", cmd)
	}
}

// A benign charge on tick 1 (measured charging, high SOC) must NOT trip — and it
// records the commanded charge so the fast protection loop can catch a later
// inversion between economic ticks. This proves the RecordCommands call site.
func TestStack_RecordCommandsFeedsFastLoop(t *testing.T) {
	stack := safetyStack()

	// Tick 1: economics charges the pack; measured charging at high SOC → no trip.
	econTick := orchestrator.SystemState{
		Timestamp: offPeakTime(),
		Solar:     []orchestrator.SolarState{{Name: "pv", PowerW: 6000, MaxW: 8000, Connected: true, Energized: true}},
		Batteries: []orchestrator.BatteryState{{Name: "bat", PowerW: -1000, SOC: 60, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true}},
		Grid:      orchestrator.GridState{NetW: -5000, ImportLimitW: math.NaN(), ExportLimitW: math.NaN(), MaxLimitW: math.NaN()},
	}
	p1 := stack.Optimize(econTick)
	for _, c := range p1.BatteryCommands {
		if c.Name == "bat" && c.Connect != nil {
			t.Fatalf("tick 1 unexpectedly disconnected: %+v", c)
		}
		if c.Name == "bat" && !(c.SetpointW < 0) {
			t.Fatalf("tick 1 battery = %+v, want a charge (negative) setpoint recorded", c)
		}
	}

	// Fast protection tick BETWEEN economic ticks (no fresh plan): the pack now
	// reads as discharging hard at reserve. EvaluateSafety must trip using the
	// charge intent RecordCommands stored on tick 1.
	fastTick := orchestrator.SystemState{
		Timestamp: offPeakTime(),
		Batteries: []orchestrator.BatteryState{{Name: "bat", PowerW: 4500, SOC: 21, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true}},
		Grid:      orchestrator.GridState{NetW: math.NaN()},
	}
	sp := stack.EvaluateSafety(fastTick)
	if !sp.Safety {
		t.Fatal("EvaluateSafety plan not marked Safety")
	}
	if len(sp.BatteryCommands) != 1 || sp.BatteryCommands[0].Connect == nil || *sp.BatteryCommands[0].Connect != false {
		t.Fatalf("fast loop did not disconnect using the recorded charge intent: %+v", sp.BatteryCommands)
	}
}

// With no fast-safety constraint wired, the Stack's EvaluateSafety is an inert
// Safety plan (the engine fast loop is a no-op) — the SafetyEvaluator contract.
func TestStack_EvaluateSafetyInertWithoutSafetyConstraint(t *testing.T) {
	stack := NewStack(Plant{}, 0, NewExportConstraint())
	sp := stack.EvaluateSafety(orchestrator.SystemState{Batteries: []orchestrator.BatteryState{{Name: "bat", Connected: true, SOC: 10, PowerW: 5000}}})
	if !sp.Safety || len(sp.BatteryCommands) != 0 {
		t.Fatalf("inert safety plan expected, got %+v", sp)
	}
}
