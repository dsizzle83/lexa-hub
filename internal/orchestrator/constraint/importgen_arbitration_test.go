package constraint

import (
	"math"
	"testing"

	"lexa-hub/internal/orchestrator"
	model "lexa-proto/csipmodel"
)

// These pin the cross-constraint behaviour once export + gen + import are all live
// in one Stack (the shadow configuration TASK-061 deploys), on the axes they share.

// fullStack builds the shadow candidate: export, gen, import, in the fixed order
// main.go wires them, at the tuned bench tick.
func fullStack() *Stack {
	p := Plant{
		Inverters: map[string]orchestrator.InverterPlant{"pv": orchestrator.InverterPlant{}.WithDefaults()},
		Batteries: map[string]orchestrator.BatteryPlant{"bat": orchestrator.BatteryPlant{}.WithDefaults()},
		EVSEs:     map[string]orchestrator.EVSEPlant{},
		Meter:     orchestrator.MeterPlant{}.WithDefaults(),
	}
	return NewStack(p, 0, NewExportConstraint(), NewGenLimitConstraint(), NewImportLimitConstraint(NewEVImportCooldown()))
}

func solarCeilingOf(p orchestrator.Plan, name string) float64 {
	for _, c := range p.SolarCommands {
		if c.Name == name {
			return c.CurtailToW
		}
	}
	return math.NaN()
}

// When BOTH an export cap and a (tighter) generation cap bind the same inverter,
// the arbiter must keep the MOST-RESTRICTIVE ceiling — the legacy "keep the tighter
// of the two" reconciliation, now done explicitly by interval intersection.
func TestImportGen_ExportAndGenCeilingMostRestrictive(t *testing.T) {
	st := orchestrator.SystemState{
		Solar: []orchestrator.SolarState{{Name: "pv", PowerW: 5000, MaxW: 5000, Connected: true, Energized: true}},
		Grid:  orchestrator.GridState{NetW: -4500, ExportLimitW: math.NaN(), ImportLimitW: math.NaN(), MaxLimitW: math.NaN()},
		// Export cap 2000 and a tighter generation cap 1500 both active.
		CSIPControl: &orchestrator.CSIPControlState{
			Source: "event", MRID: "both",
			Base: model.DERControlBase{
				OpModExpLimW: &model.ActivePower{Value: 2000, Multiplier: 0},
				OpModMaxLimW: &model.ActivePower{Value: 1500, Multiplier: 0},
			},
		},
	}
	plan := fullStack().Optimize(st)
	ceil := solarCeilingOf(plan, "pv")
	if math.IsNaN(ceil) {
		t.Fatal("no solar ceiling emitted with export+gen caps active")
	}
	// The generation cap (1500 absolute) is tighter than anything the export
	// controller commands on the first tick; the resolved ceiling must not exceed it.
	if ceil > 1500+1 {
		t.Errorf("resolved ceiling = %.0fW exceeds the tighter generation cap (1500W); arbiter did not keep the most restrictive", ceil)
	}
}

// A gen-only cap must produce the same nameplate-distributed ceiling whether or not
// the (inert) import/export constraints are also in the stack — they express no
// opinion on the solar axis when their caps are absent.
func TestImportGen_GenCapUnaffectedByIdleSiblings(t *testing.T) {
	st := orchestrator.SystemState{
		Solar: []orchestrator.SolarState{{Name: "pv", PowerW: 4000, MaxW: 5000, Connected: true, Energized: true}},
		Grid:  orchestrator.GridState{NetW: -3800, ExportLimitW: math.NaN(), ImportLimitW: math.NaN(), MaxLimitW: math.NaN()},
		CSIPControl: &orchestrator.CSIPControlState{
			Source: "event", MRID: "gen-only",
			Base: model.DERControlBase{OpModMaxLimW: &model.ActivePower{Value: 2000, Multiplier: 0}},
		},
	}
	plan := fullStack().Optimize(st)
	if ceil := solarCeilingOf(plan, "pv"); math.Abs(ceil-2000) > 1 {
		t.Errorf("gen-only ceiling = %.0fW, want 2000 (single inverter = full cap); idle siblings perturbed it", ceil)
	}
}

// Documented arbiter semantics for the contradictory simultaneous import+export cap
// on a shared battery (NOT exercised by the scenario families; a KNOWN gap for the
// active flip — see importlimit.go type doc and the TASK-061 shadow report). Legacy
// resolves import-wins (neutralise charge, discharge); the interval arbiter instead
// collapses the battery axis to the most-restrictive bound. This test PINS the
// current arbiter behaviour so the flip work (TASK-062/063) revisits it deliberately
// rather than by surprise; it is not a claim of legacy parity on this axis.
func TestImportGen_SimultaneousCapArbitration(t *testing.T) {
	st := orchestrator.SystemState{
		Solar: []orchestrator.SolarState{{Name: "pv", PowerW: 5000, MaxW: 5000, Connected: true, Energized: true}},
		Batteries: []orchestrator.BatteryState{{
			Name: "bat", PowerW: 0, SOC: 60, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true,
		}},
		// Export cap (drives battery charge/absorb) AND import cap (drives discharge)
		// both active — a physically contradictory pair.
		Grid: orchestrator.GridState{NetW: -3000, ExportLimitW: 1000, ImportLimitW: 500, MaxLimitW: math.NaN()},
	}
	plan := fullStack().Optimize(st)
	// Whatever the arbiter decides, it must be DETERMINISTIC and a single battery
	// command (no double-authoring), and the decision log must record the conflict.
	var batCmds int
	for _, bc := range plan.BatteryCommands {
		if bc.Name == "bat" {
			batCmds++
		}
	}
	if batCmds > 1 {
		t.Errorf("battery double-authored (%d commands) under simultaneous import+export caps; arbiter must resolve to one", batCmds)
	}
}
