package orchestrator

import (
	"math"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────
// Volumetric TOU delivery ($/kWh) + fixed daily charge.
//
// Delivery is an orthogonal adder folded into the ALL-IN import price
// (planStepImportAllIn) at both the forward DP objective and the backtrack
// MarginalCost mirror; it is import-only (never charged on export). The fixed
// daily charge is a flat, dispatch-independent cost added ONCE to
// DailyPlan.TotalCost — never to any per-slot MarginalCost or setpoint.
//
// These tests share the package-level planner harness (plannerTestBase,
// withLocalZone, plannerTestNow) from planner_test.go. Delivery is evaluated at
// local wall-clock time, so the reshaping test pins time.Local to UTC (the
// window built from UTC step hours then lines up slot-for-slot). This package's
// tests never call t.Parallel(), so repointing time.Local is safe.
// ─────────────────────────────────────────────────────────────────────────

// importEnergyKwh sums the imported energy (kWh) over steps [lo,hi): the
// positive part of ExpectedGridW × the step duration. Export (negative grid) is
// ignored.
func importEnergyKwh(plan *DailyPlan, lo, hi int) float64 {
	var e float64
	for i := lo; i < hi && i < planSteps; i++ {
		if g := plan.Intervals[i].ExpectedGridW; g > 0 {
			e += g / 1000 * planStepHours
		}
	}
	return e
}

// sameSetpoint compares two battery setpoints treating NaN (no-battery sentinel)
// as equal to NaN.
func sameSetpoint(a, b float64) bool {
	if math.IsNaN(a) && math.IsNaN(b) {
		return true
	}
	return a == b
}

// TestPlan_DeliveryTOU_ReshapesDispatchAndRaisesCost drives the DP with a FLAT,
// cheap supply price all day but a HIGH volumetric delivery charge only during
// 16:00–21:00. Supply alone gives no arbitrage signal; the delivery adder makes
// the all-in import price 1.10 in the window vs 0.10 elsewhere, so the DP must
// charge the battery off-window and discharge it during the window — cutting
// grid import in the window relative to the no-delivery baseline — and the
// all-in TotalCost must be higher.
func TestPlan_DeliveryTOU_ReshapesDispatchAndRaisesCost(t *testing.T) {
	// Delivery is evaluated at local time; pin it so the window aligns with the
	// UTC-anchored plan window.
	withLocalZone(t, time.UTC)

	base := plannerTestBase()
	base.SOCStepKwh = 0.1 // fine enough that 3 kW moves SOC within a 5-min step
	base.BattCapacityKwh = 3
	base.BattMaxChargeKw = 3
	base.BattMaxDischargeKw = 3
	base.InitialBattSocKwh = 0.3 // min SOC (10% of 3 kWh)
	base.LoadForecastKw = 1.0

	// Flat, cheap SUPPLY price all day — no arbitrage signal from supply alone.
	base.FallbackTOU = nil
	base.ImportPricePerKwh = make([]float64, planSteps)
	base.ExportPricePerKwh = make([]float64, planSteps)
	for i := 0; i < planSteps; i++ {
		base.ImportPricePerKwh[i] = 0.10
	}

	// Baseline: no delivery charge (nil DeliveryTOU).
	planNoDeliv := NewDailyPlanner().Plan(base)

	// With a high delivery charge only during 16:00–21:00.
	withDeliv := base
	withDeliv.DeliveryTOU = NewTOUCostModel([]TOUPeriod{
		{StartHour: 16, EndHour: 21, RatePerKwh: 1.00, Label: "peak-delivery"},
	}, 0.0, 0.30) // defaultRate 0 ⇒ no delivery outside the window
	planDeliv := NewDailyPlanner().Plan(withDeliv)

	winLo := int((16 * 3600) / planStepSec)
	winHi := int((21 * 3600) / planStepSec)

	impNoDeliv := importEnergyKwh(planNoDeliv, winLo, winHi)
	impDeliv := importEnergyKwh(planDeliv, winLo, winHi)

	if impDeliv >= impNoDeliv {
		t.Errorf("delivery charge should REDUCE window import: with delivery=%.3f kWh, without=%.3f kWh",
			impDeliv, impNoDeliv)
	}

	// The battery must actually discharge (positive setpoint) inside the
	// high-delivery window — proves the reshaping is real, not degenerate.
	dischargedInWindow := false
	for i := winLo; i < winHi; i++ {
		if s := planDeliv.Intervals[i].BattSetpointW; !math.IsNaN(s) && s > 100 {
			dischargedInWindow = true
			break
		}
	}
	if !dischargedInWindow {
		t.Error("expected the battery to discharge during the high-delivery window")
	}

	// You pay delivery on every net imported kWh the battery cannot displace, so
	// the all-in TotalCost must exceed the no-delivery baseline.
	if planDeliv.TotalCost <= planNoDeliv.TotalCost {
		t.Errorf("TotalCost with delivery (%.4f) should exceed without delivery (%.4f)",
			planDeliv.TotalCost, planNoDeliv.TotalCost)
	}
}

// TestPlan_DeliveryTOU_ImportOnly_ExportSlotsUnaffected proves delivery is
// applied to imported energy only. With no battery and no EV the dispatch is
// fully determined (grid = load − solar each step), identical with or without a
// delivery charge — so a flat all-day delivery adder must leave every EXPORTING
// slot's MarginalCost bit-for-bit unchanged while still moving import slots.
func TestPlan_DeliveryTOU_ImportOnly_ExportSlotsUnaffected(t *testing.T) {
	base := plannerTestBase()
	// No battery, no EV → no dispatch choices; grid is deterministic per step.
	base.LoadForecastKw = 0.5

	// Midday solar surplus (10:00–15:00) that exports well past the 0.5 kW load.
	base.SolarForecastKw = make([]float64, planSteps)
	ws := base.WindowStart - (base.WindowStart % planStepSec)
	for i := range base.SolarForecastKw {
		if h := time.Unix(ws+int64(i)*planStepSec, 0).UTC().Hour(); h >= 10 && h < 15 {
			base.SolarForecastKw[i] = 5.0
		}
	}
	base.FallbackTOU = nil
	base.ImportPricePerKwh = make([]float64, planSteps)
	base.ExportPricePerKwh = make([]float64, planSteps)
	for i := 0; i < planSteps; i++ {
		base.ImportPricePerKwh[i] = 0.10
		base.ExportPricePerKwh[i] = 0.05
	}

	planNil := NewDailyPlanner().Plan(base)

	withDeliv := base
	// Flat, all-day delivery adder (no periods → defaultRate everywhere). Local
	// time is irrelevant here, so no zone pinning is needed.
	withDeliv.DeliveryTOU = NewTOUCostModel(nil, 0.50, 0.30)
	planDeliv := NewDailyPlanner().Plan(withDeliv)

	sawExport := false
	sawImportChanged := false
	for i := 0; i < planSteps; i++ {
		ivN := planNil.Intervals[i]
		ivD := planDeliv.Intervals[i]
		if ivN.ExpectedGridW < 0 { // exporting slot
			sawExport = true
			if ivD.MarginalCost != ivN.MarginalCost {
				t.Errorf("step %d exports (grid=%.0fW) but MarginalCost changed with delivery: %.6f vs %.6f",
					i, ivN.ExpectedGridW, ivD.MarginalCost, ivN.MarginalCost)
			}
		}
		if ivN.ExpectedGridW > 0 && ivD.MarginalCost != ivN.MarginalCost { // importing slot
			sawImportChanged = true
		}
	}
	if !sawExport {
		t.Fatal("scenario produced no exporting slots — cannot test import-only delivery")
	}
	if !sawImportChanged {
		t.Error("delivery charge never changed any import slot's MarginalCost — delivery not taking effect")
	}
}

// TestPlan_FixedDailyCharge_OnlyInTotalCost pins that a fixed daily charge
// raises DailyPlan.TotalCost by exactly that amount and touches nothing else:
// every per-slot MarginalCost and BattSetpointW is identical to the zero-charge
// plan (a constant cannot change marginal dispatch).
func TestPlan_FixedDailyCharge_OnlyInTotalCost(t *testing.T) {
	base := plannerTestBase()
	base.SOCStepKwh = 0.1
	base.BattCapacityKwh = 3
	base.BattMaxChargeKw = 3
	base.BattMaxDischargeKw = 3
	base.InitialBattSocKwh = 0.3
	base.LoadForecastKw = 1.0

	// A real TOU signal so the plan produces non-trivial setpoints to compare.
	base.FallbackTOU = nil
	base.ImportPricePerKwh = make([]float64, planSteps)
	base.ExportPricePerKwh = make([]float64, planSteps)
	ws := base.WindowStart - (base.WindowStart % planStepSec)
	for i := 0; i < planSteps; i++ {
		if h := time.Unix(ws+int64(i)*planStepSec, 0).UTC().Hour(); h >= 16 && h < 21 {
			base.ImportPricePerKwh[i] = 1.00
		} else {
			base.ImportPricePerKwh[i] = 0.10
		}
	}

	planZero := NewDailyPlanner().Plan(base)

	const fixed = 5.0
	withFixed := base
	withFixed.FixedDailyCharge = fixed
	planFixed := NewDailyPlanner().Plan(withFixed)

	// TotalCost rises by exactly the fixed charge.
	if diff := planFixed.TotalCost - planZero.TotalCost; math.Abs(diff-fixed) > 1e-9 {
		t.Errorf("TotalCost delta = %.9f, want %.9f (the fixed daily charge)", diff, fixed)
	}

	// Every per-slot MarginalCost and BattSetpointW must be identical.
	for i := 0; i < planSteps; i++ {
		z := planZero.Intervals[i]
		f := planFixed.Intervals[i]
		if z.MarginalCost != f.MarginalCost {
			t.Errorf("step %d: MarginalCost changed with fixed charge: %.9f vs %.9f",
				i, f.MarginalCost, z.MarginalCost)
		}
		if !sameSetpoint(z.BattSetpointW, f.BattSetpointW) {
			t.Errorf("step %d: BattSetpointW changed with fixed charge: %v vs %v",
				i, f.BattSetpointW, z.BattSetpointW)
		}
	}
}
