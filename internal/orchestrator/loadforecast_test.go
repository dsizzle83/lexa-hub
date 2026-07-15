package orchestrator

import (
	"math"
	"testing"
	"time"
)

// TestDiurnalLoadForecast_ShapeAndScale pins the synthesized residential load
// curve: nil when disabled, a 288-slot positive curve whose mean equals the
// configured average, with a real (non-flat) daily peak.
func TestDiurnalLoadForecast_ShapeAndScale(t *testing.T) {
	if got := diurnalLoadForecast(1_752_000_000, 0); got != nil {
		t.Fatalf("avgKw<=0 must yield nil (no synthesis), got len %d", len(got))
	}
	const avg = 2.0
	f := diurnalLoadForecast(1_752_000_000, avg)
	if len(f) != planSteps {
		t.Fatalf("len = %d, want %d", len(f), planSteps)
	}
	sum, min, max := 0.0, math.Inf(1), 0.0
	for _, v := range f {
		if v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			t.Fatalf("non-positive/non-finite load slot: %v", v)
		}
		sum += v
		min = math.Min(min, v)
		max = math.Max(max, v)
	}
	if mean := sum / float64(planSteps); math.Abs(mean-avg) > 1e-9 {
		t.Errorf("mean = %v, want %v (curve is scaled so its mean is the configured average)", mean, avg)
	}
	if max <= min*1.5 {
		t.Errorf("curve too flat: min=%v max=%v (want a real evening peak to shave)", min, max)
	}
}

// TestPlan_SynthesizedLoad_UtilizesBattery is the regression for the
// "battery not utilized" report. On a solar-masked / baseload-free bench the
// flat-scalar inferred load is ~0, so the DP correctly leaves the battery idle
// (charging costs money; discharging into a zero export price / zero load earns
// nothing). A synthesized residential load + TOU gives the battery a reason to
// peak-shave — charge cheap, discharge into the expensive evening — so it must
// both charge and discharge. This pins that the load forecast unlocks battery
// cycling.
func TestPlan_SynthesizedLoad_UtilizesBattery(t *testing.T) {
	ws := time.Date(2026, 7, 15, 0, 0, 0, 0, time.Local).Unix()
	base := PlannerParams{
		Now:                time.Unix(ws, 0),
		WindowStart:        ws,
		BattCapacityKwh:    10,
		BattMaxChargeKw:    5,
		BattMaxDischargeKw: 5,
		InitialBattSocKwh:  5,
		TerminalSocKwh:     5, // == initial: no net-discharge freebie, isolate arbitrage
		SolarForecastKw:    diurnalSolarForecast(ws, 6),
		FallbackTOU:        DefaultTOUCostModel(),
		SOCStepKwh:         0.5,
	}
	pl := NewDailyPlanner()

	activeSlots := func(dp *DailyPlan) (charge, discharge int) {
		for _, iv := range dp.Intervals {
			switch {
			case iv.BattSetpointW < -500:
				charge++
			case iv.BattSetpointW > 500:
				discharge++
			}
		}
		return
	}

	zl := base // zero-load baseline (the pre-fix, solar-masked-bench behaviour)
	zl.LoadForecastKw = 0
	zc, zd := activeSlots(pl.Plan(zl))

	wl := base // synthesized residential load
	wl.LoadProfileKw = diurnalLoadForecast(ws, 2)
	wc, wd := activeSlots(pl.Plan(wl))

	if wc == 0 || wd == 0 {
		t.Errorf("with a synthesized load the battery must both charge and discharge; got charge=%d discharge=%d slots", wc, wd)
	}
	if wc+wd <= zc+zd {
		t.Errorf("synthesized load should increase battery activity vs zero load: zero-load=%d active slots, with-load=%d", zc+zd, wc+wd)
	}
}

// TestPlan_TotalCost_ReflectsActualPlan is the regression for the "total_cost: 0
// on a plan that imports" blemish the load forecast surfaced live on the bench.
// An EV target that cannot be met by departure forces the DP's terminal
// selection into its best-effort fallback (dp[best] pinned to 0 by the
// "no feasible path" guard), yet the plan still serves the house load and
// charges what it can — so its real daily cost is > 0. TotalCost must report
// THAT (the sum of the planned marginals + the fixed charge), never the
// misleading forward-pass 0.
func TestPlan_TotalCost_ReflectsActualPlan(t *testing.T) {
	ws := time.Date(2026, 7, 15, 0, 0, 0, 0, time.Local).Unix()
	const fixed = 0.35
	p := PlannerParams{
		Now: time.Unix(ws, 0), WindowStart: ws,
		BattCapacityKwh: 10, BattMaxChargeKw: 5, BattMaxDischargeKw: 5, InitialBattSocKwh: 5,
		EVCapacityKwh: 60, EVMaxChargeKw: 7.68, EVVoltageV: 240,
		InitialEVSocKwh: 24, EVTargetSocKwh: 48, EVDepartureUnix: ws + 3600, // 24 kWh needed in 1 h @ 7.68 kW ⇒ infeasible
		SolarForecastKw: diurnalSolarForecast(ws, 6),
		LoadProfileKw:   diurnalLoadForecast(ws, 2),
		FallbackTOU:     DefaultTOUCostModel(),
		SOCStepKwh:      0.5, FixedDailyCharge: fixed,
	}
	plan := NewDailyPlanner().Plan(p)

	var sum float64
	for i := range plan.Intervals {
		sum += plan.Intervals[i].MarginalCost
	}
	if math.Abs(plan.TotalCost-(sum+fixed)) > 1e-6 {
		t.Errorf("TotalCost = %.6f, want sum(marginals)+fixed = %.6f", plan.TotalCost, sum+fixed)
	}
	if plan.TotalCost <= fixed {
		t.Errorf("TotalCost = %.6f must exceed the fixed charge alone (%.2f): the plan serves real load and imports", plan.TotalCost, fixed)
	}
}
