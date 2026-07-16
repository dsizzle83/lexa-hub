package constraint

import (
	"math"
	"testing"

	"lexa-hub/internal/orchestrator"
	model "lexa-proto/csipmodel"
)

// noLeverExportState: PV over a 0 W export cap with the battery FULL (cannot
// absorb) and no load or EV, so the ceiling controller curtails solar to ~0 in
// one tick while the meter still shows export — the zero-lever condition, and
// the shape a brief meter-noise blip fakes for a tick or two.
func noLeverExportState(capW int16) orchestrator.SystemState {
	return orchestrator.SystemState{
		Solar:     []orchestrator.SolarState{{Name: "pv", PowerW: 4500, MaxW: 5000, Connected: true, Energized: true}},
		Batteries: []orchestrator.BatteryState{{Name: "bat", PowerW: 0, SOC: 100, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true}},
		Grid:      orchestrator.GridState{NetW: -4500, ExportLimitW: math.NaN(), ImportLimitW: math.NaN(), MaxLimitW: math.NaN()},
		CSIPControl: &orchestrator.CSIPControlState{
			Source: "event", MRID: "zl-mrid",
			Base: model.DERControlBase{OpModExpLimW: &model.ActivePower{Value: capW, Multiplier: 0}},
		},
	}
}

// TestExportConstraint_ZeroLeverBreachDebounced is the constraint-side twin of
// the legacy audit-D3 fix: the zero-lever breach must not fire on the first
// no-lever tick (that was the spurious-CannotComply bug), but a sustained
// episode still escalates — keeping the candidate stack in shadow parity with
// the debounced legacy path.
func TestExportConstraint_ZeroLeverBreachDebounced(t *testing.T) {
	c, s := newExportPair()
	firstBreach := -1
	var last *orchestrator.ComplianceBreach
	for i := 0; i < 6; i++ {
		_, breach := c.Evaluate(benchInput(noLeverExportState(0)), s)
		if breach != nil && firstBreach < 0 {
			firstBreach = i
		}
		last = breach
	}
	if firstBreach < 0 {
		t.Fatal("a sustained no-lever export episode must escalate to a CannotComply")
	}
	if firstBreach == 0 {
		t.Fatal("the zero-lever breach fired on the FIRST tick — the constraint mirror is still single-sample (audit D3)")
	}
	if last == nil || last.Reason != "generation curtailed to minimum; battery and EV cannot absorb the surplus" {
		t.Fatalf("expected the zero-lever breach reason, got %+v", last)
	}
}

// A 1-2-tick transient must not breach (the meter-noise case).
func TestExportConstraint_ZeroLeverTransientNoBreach(t *testing.T) {
	c, s := newExportPair()
	for i := 0; i < 2; i++ {
		if _, breach := c.Evaluate(benchInput(noLeverExportState(0)), s); breach != nil {
			t.Fatalf("tick %d: a %d-tick no-lever transient must not breach", i, i+1)
		}
	}
	// Relief under the same cap so the counter drains (not a cap clear).
	relief := noLeverExportState(0)
	relief.Solar[0].PowerW = 50
	relief.Grid.NetW = -50
	for i := 0; i < 3; i++ {
		if _, breach := c.Evaluate(benchInput(relief), s); breach != nil {
			t.Fatalf("relief tick %d: counter must drain to no breach, got %+v", i, breach)
		}
	}
}
