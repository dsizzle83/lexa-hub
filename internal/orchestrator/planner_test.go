package orchestrator

import (
	"math"
	"testing"
	"time"
)

// plannerTestNow is a fixed test timestamp: Monday 2026-05-18 00:00 UTC.
var plannerTestNow = time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC)

func plannerTestBase() PlannerParams {
	return PlannerParams{
		WindowStart:    plannerTestNow.Unix(),
		SOCStepKwh:     1.0, // coarser than default for speed
		BattEfficiency: 0.96,
		EVEfficiency:   0.95,
		EVVoltageV:     240,
		FallbackTOU:    DefaultTOUCostModel(),
	}
}

// ── Discretisation helpers ─────────────────────────────────────────────────────

func TestDiscretizeLevels_BasicRange(t *testing.T) {
	levels := discretizeLevels(0, 10, 2.5)
	// Expect: 0, 2.5, 5, 7.5, 10
	if len(levels) < 2 {
		t.Fatalf("expected at least 2 levels, got %d", len(levels))
	}
	if levels[0] != 0 {
		t.Errorf("first level = %.1f, want 0", levels[0])
	}
	if levels[len(levels)-1] != 10 {
		t.Errorf("last level = %.1f, want 10", levels[len(levels)-1])
	}
}

func TestDiscretizePowers_IncludesZero(t *testing.T) {
	powers := discretizePowers(-5, 5, 1)
	hasZero := false
	for _, p := range powers {
		if p == 0 {
			hasZero = true
		}
	}
	if !hasZero {
		t.Errorf("discretizePowers did not include 0: %v", powers)
	}
	if powers[0] != -5 || powers[len(powers)-1] != 5 {
		t.Errorf("unexpected endpoints: %v", powers)
	}
}

func TestDiscretizeEVCurrents_IEC61851(t *testing.T) {
	currents := discretizeEVCurrents(32)
	if currents[0] != 0 {
		t.Errorf("first current = %.0f, want 0", currents[0])
	}
	// Must include 6 A (IEC 61851 minimum charging current)
	has6 := false
	for _, a := range currents {
		if a == 6 {
			has6 = true
		}
	}
	if !has6 {
		t.Errorf("missing 6A: %v", currents)
	}
	// Must not exceed maxA
	for _, a := range currents {
		if a > 32.5 {
			t.Errorf("current %.0fA exceeds max 32A", a)
		}
	}
}

// ── Plan: no assets ────────────────────────────────────────────────────────────

func TestPlan_NoAssets_AllNaN(t *testing.T) {
	p := plannerTestBase()
	// No battery, no EV
	plan := NewDailyPlanner().Plan(p)

	if plan == nil {
		t.Fatal("Plan returned nil")
	}
	if plan.WindowStart >= plan.WindowEnd {
		t.Errorf("invalid window: start=%d end=%d", plan.WindowStart, plan.WindowEnd)
	}
	if len(plan.Intervals) != planSteps {
		t.Errorf("expected %d intervals, got %d", planSteps, len(plan.Intervals))
	}
	for i, iv := range plan.Intervals {
		if !math.IsNaN(iv.BattSetpointW) {
			t.Errorf("interval %d: BattSetpointW = %.1f, want NaN", i, iv.BattSetpointW)
		}
		if iv.EVMaxCurrentA != 0 {
			t.Errorf("interval %d: EVMaxCurrentA = %.1f, want 0", i, iv.EVMaxCurrentA)
		}
	}
}

// ── Plan: battery only, TOU pricing ───────────────────────────────────────────

func TestPlan_Battery_ChargesAtNight_DischargesdAtPeak(t *testing.T) {
	p := plannerTestBase()
	// Use 0.1 kWh step so 3 kW power moves ~2.4 levels per 5-min step.
	p.SOCStepKwh = 0.1
	p.BattCapacityKwh = 3
	p.BattMaxChargeKw = 3
	p.BattMaxDischargeKw = 3
	p.InitialBattSocKwh = 0.3 // start at min SOC (10% of 3 kWh)
	p.LoadForecastKw = 1.0

	// Strong TOU signal: $0.10 off-peak (00-16h), $1.00 peak (16-21h).
	p.FallbackTOU = nil
	p.ImportPricePerKwh = make([]float64, planSteps)
	p.ExportPricePerKwh = make([]float64, planSteps)
	ws := p.WindowStart - (p.WindowStart % planStepSec)
	for i := 0; i < planSteps; i++ {
		stepT := time.Unix(ws+int64(i)*planStepSec, 0).UTC()
		if stepT.Hour() >= 16 && stepT.Hour() < 21 {
			p.ImportPricePerKwh[i] = 1.00
		} else {
			p.ImportPricePerKwh[i] = 0.10
		}
	}

	plan := NewDailyPlanner().Plan(p)

	// During peak (e.g. 18:00 = step 216), battery should be discharging (positive setpoint),
	// providing energy charged earlier at off-peak rates.
	step6pm := int((18 * 3600) / planStepSec)
	iv6pm := plan.Intervals[step6pm]
	if !math.IsNaN(iv6pm.BattSetpointW) && iv6pm.BattSetpointW < -100 {
		t.Errorf("at 18:00, expected battery discharging (≥0W), got %.0fW", iv6pm.BattSetpointW)
	}

	// Total cost must be finite and positive.
	if math.IsInf(plan.TotalCost, 0) || math.IsNaN(plan.TotalCost) {
		t.Errorf("TotalCost = %v, want finite", plan.TotalCost)
	}

	// Verify that the plan actually uses the battery (some non-zero setpoints exist).
	hasCharge, hasDischarge := false, false
	for _, iv := range plan.Intervals {
		if !math.IsNaN(iv.BattSetpointW) {
			if iv.BattSetpointW < -100 {
				hasCharge = true
			}
			if iv.BattSetpointW > 100 {
				hasDischarge = true
			}
		}
	}
	if !hasCharge {
		t.Error("expected at least one charging interval")
	}
	if !hasDischarge {
		t.Error("expected at least one discharging interval")
	}
}

// ── Plan: battery + export limit ──────────────────────────────────────────────

func TestPlan_ExportLimit_Respected(t *testing.T) {
	p := plannerTestBase()
	// Fine SOC step so the battery can actually represent the sub-kWh-per-step
	// charging needed to absorb export. (At the coarse 1 kWh default a 5-min
	// charge rounds to zero SOC change; before the energy-accumulation fix that
	// rounding was hidden as phantom absorption — an infinite sink with frozen
	// SOC — which let an all-day 5 kW surplus be "held" by a 10 kWh battery.)
	p.SOCStepKwh = 0.1
	p.BattCapacityKwh = 10
	p.BattMaxChargeKw = 5
	p.BattMaxDischargeKw = 5
	p.InitialBattSocKwh = 2 // room to absorb the midday surplus
	p.LoadForecastKw = 0.5

	// A bounded 2-hour midday solar burst (12:00–14:00) that would export ~4.5 kW
	// uncontrolled. The battery must charge to hold export under the 1 kW cap.
	// It's bounded because a 10 kWh pack can absorb a 2-hour burst but NOT an
	// all-day one — the planner has no solar-curtailment lever (that's the
	// optimizer's job), so a sustained over-cap surplus is genuinely infeasible.
	p.SolarForecastKw = make([]float64, planSteps)
	ws := p.WindowStart - (p.WindowStart % planStepSec)
	for i := range p.SolarForecastKw {
		if h := time.Unix(ws+int64(i)*planStepSec, 0).UTC().Hour(); h >= 12 && h < 14 {
			p.SolarForecastKw[i] = 5.0
		}
	}

	// Constrain export to 1 kW for all steps.
	p.DERConstraints = make([]StepConstraint, planSteps)
	for i := range p.DERConstraints {
		p.DERConstraints[i] = StepConstraint{
			ExpLimW: 1000,
			ImpLimW: math.NaN(),
			MaxLimW: math.NaN(),
			FixedW:  math.NaN(),
		}
	}

	plan := NewDailyPlanner().Plan(p)

	// Export must never exceed 1 kW, and the battery must actually charge during
	// the burst (proves it's a feasible plan, not the degenerate infeasible one).
	charged := false
	for i, iv := range plan.Intervals {
		if iv.ExpectedGridW < -(1000 + 50) { // allow 50W tolerance for discretisation
			t.Errorf("step %d: export %.0fW exceeds limit of 1000W (gridW=%.0f)", i, -iv.ExpectedGridW, iv.ExpectedGridW)
		}
		if !math.IsNaN(iv.BattSetpointW) && iv.BattSetpointW < -100 {
			charged = true
		}
	}
	if !charged {
		t.Error("battery never charged — plan is infeasible/degenerate, not actually holding the cap")
	}
}

// ── Plan: EV departure constraint ─────────────────────────────────────────────

func TestPlan_EV_MeetsTargetByDeparture(t *testing.T) {
	p := plannerTestBase()
	// Use 0.25 kWh step: 7.2 kW × (5/60) × 0.95 ≈ 0.57 kWh/step → ~2 levels per step.
	p.SOCStepKwh = 0.25

	// Small EV (15 kWh) needing to go from 5 to 12 kWh in 6 hours.
	p.EVCapacityKwh = 15
	p.EVMaxChargeKw = 7.2
	p.InitialEVSocKwh = 5 // 33%
	p.EVTargetSocKwh = 12 // 80%
	p.EVVoltageV = 240
	p.LoadForecastKw = 0.5

	// Departure in 6 hours (72 steps).
	p.EVDepartureUnix = plannerTestNow.Add(6 * time.Hour).Unix()

	// Flat cheap rate — EV must charge to meet target.
	p.FallbackTOU = nil
	p.ImportPricePerKwh = make([]float64, planSteps)
	p.ExportPricePerKwh = make([]float64, planSteps)
	for i := range p.ImportPricePerKwh {
		p.ImportPricePerKwh[i] = 0.12
	}

	plan := NewDailyPlanner().Plan(p)

	// EV must charge at some point before departure.
	deptStep := int((6 * 3600) / planStepSec)
	var maxEVA float64
	for i := 0; i < deptStep; i++ {
		if plan.Intervals[i].EVMaxCurrentA > maxEVA {
			maxEVA = plan.Intervals[i].EVMaxCurrentA
		}
	}
	if maxEVA < 5 {
		t.Errorf("EV never charged meaningfully before departure (max=%.1fA)", maxEVA)
	}

	// Total cost must be finite.
	if math.IsInf(plan.TotalCost, 0) || math.IsNaN(plan.TotalCost) {
		t.Errorf("TotalCost = %v, want finite", plan.TotalCost)
	}
}

// ── CurrentTarget ─────────────────────────────────────────────────────────────

func TestDailyPlan_CurrentTarget_InsideWindow(t *testing.T) {
	plan := &DailyPlan{
		WindowStart: plannerTestNow.Unix(),
		WindowEnd:   plannerTestNow.Unix() + int64(planSteps)*planStepSec,
	}
	// Step 0: 00:00, battery = 1000W, EV = 10A
	plan.Intervals[0] = PlanInterval{
		Start:         plan.WindowStart,
		BattSetpointW: 1000,
		EVMaxCurrentA: 10,
	}

	target := plan.CurrentTarget(plannerTestNow)
	if target == nil {
		t.Fatal("expected non-nil target inside window")
	}
	if target.BattSetpointW != 1000 {
		t.Errorf("BattSetpointW = %.0f, want 1000", target.BattSetpointW)
	}
	if target.EVMaxCurrentA != 10 {
		t.Errorf("EVMaxCurrentA = %.1f, want 10", target.EVMaxCurrentA)
	}
}

func TestDailyPlan_CurrentTarget_OutsideWindow(t *testing.T) {
	plan := &DailyPlan{
		WindowStart: plannerTestNow.Unix(),
		WindowEnd:   plannerTestNow.Unix() + int64(planSteps)*planStepSec,
	}

	tAfter := plannerTestNow.Add(25 * time.Hour)
	if plan.CurrentTarget(tAfter) != nil {
		t.Error("expected nil target after window end")
	}
	tBefore := plannerTestNow.Add(-1 * time.Hour)
	if plan.CurrentTarget(tBefore) != nil {
		t.Error("expected nil target before window start")
	}
}

func TestDailyPlan_CurrentTarget_Nil(t *testing.T) {
	var plan *DailyPlan
	if plan.CurrentTarget(plannerTestNow) != nil {
		t.Error("nil plan should return nil target")
	}
}

// ── Window snapping ────────────────────────────────────────────────────────────

func TestPlan_WindowSnappedTo5Min(t *testing.T) {
	p := plannerTestBase()
	p.WindowStart = plannerTestNow.Unix() + 37 // not aligned

	plan := NewDailyPlanner().Plan(p)

	if plan.WindowStart%planStepSec != 0 {
		t.Errorf("WindowStart %d not aligned to %ds", plan.WindowStart, planStepSec)
	}
	if plan.WindowEnd != plan.WindowStart+int64(planSteps)*planStepSec {
		t.Errorf("WindowEnd mismatch: %d", plan.WindowEnd)
	}
	if len(plan.Intervals) != planSteps {
		t.Errorf("expected %d intervals, got %d", planSteps, len(plan.Intervals))
	}
}

// ── Disconnect constraint ──────────────────────────────────────────────────────

func TestPlan_Disconnect_ForcesZero(t *testing.T) {
	p := plannerTestBase()
	p.BattCapacityKwh = 10
	p.BattMaxChargeKw = 5
	p.BattMaxDischargeKw = 5
	p.InitialBattSocKwh = 9 // nearly full — would normally discharge
	p.LoadForecastKw = 2.0

	// Disconnect all steps
	p.DERConstraints = make([]StepConstraint, planSteps)
	for i := range p.DERConstraints {
		p.DERConstraints[i] = StepConstraint{
			Disconnect: true,
			ExpLimW:    math.NaN(),
			ImpLimW:    math.NaN(),
			MaxLimW:    math.NaN(),
			FixedW:     math.NaN(),
		}
	}

	plan := NewDailyPlanner().Plan(p)

	for i, iv := range plan.Intervals {
		if !math.IsNaN(iv.BattSetpointW) && math.Abs(iv.BattSetpointW) > 1 {
			t.Errorf("step %d: battery setpoint = %.0fW despite Disconnect=true", i, iv.BattSetpointW)
		}
	}
}

// sumBattSetpointW totals the per-step battery setpoints (+discharge, −charge),
// a proxy for net energy moved over the window.
func sumBattSetpointW(plan *DailyPlan) float64 {
	var s float64
	for _, iv := range plan.Intervals {
		if !math.IsNaN(iv.BattSetpointW) {
			s += iv.BattSetpointW
		}
	}
	return s
}

// TestPlan_TerminalReserve_AllowsNetDischarge checks that lowering the terminal
// SOC target below the initial SOC lets the planner net-discharge across an
// expensive evening instead of being pinned at its starting charge.
func TestPlan_TerminalReserve_AllowsNetDischarge(t *testing.T) {
	base := plannerTestBase()
	base.SOCStepKwh = 0.1 // fine enough that 3 kW moves SOC within a 5-min step
	base.BattCapacityKwh = 3
	base.BattMaxChargeKw = 3
	base.BattMaxDischargeKw = 3
	base.InitialBattSocKwh = 2.7 // 90% — nearly full
	base.LoadForecastKw = 1.0
	// Strong peak so discharging stored energy in the evening clearly pays.
	base.FallbackTOU = nil
	base.ImportPricePerKwh = make([]float64, planSteps)
	base.ExportPricePerKwh = make([]float64, planSteps)
	ws := base.WindowStart - (base.WindowStart % planStepSec)
	for i := 0; i < planSteps; i++ {
		h := time.Unix(ws+int64(i)*planStepSec, 0).UTC().Hour()
		if h >= 16 && h < 21 {
			base.ImportPricePerKwh[i] = 1.00
		} else {
			base.ImportPricePerKwh[i] = 0.10
		}
	}

	// Default terminal target (== initial SOC): no net daily discharge.
	planDefault := NewDailyPlanner().Plan(base)

	// Reserve floor at 20% of capacity: net discharge down to 0.6 kWh allowed.
	aggr := base
	aggr.TerminalSocKwh = 0.6
	planAggr := NewDailyPlanner().Plan(aggr)

	netDefault := sumBattSetpointW(planDefault)
	netAggr := sumBattSetpointW(planAggr)

	if netAggr <= netDefault {
		t.Errorf("reserve plan should net-discharge more: netAggr=%.0fW netDefault=%.0fW", netAggr, netDefault)
	}
	if netAggr <= 0 {
		t.Errorf("reserve plan should be net-discharging over the window, got Σsetpoint=%.0fW", netAggr)
	}
}

func TestClearSkyShape(t *testing.T) {
	if got := clearSkyShape(3); got != 0 {
		t.Errorf("pre-dawn shape(3) = %v, want 0", got)
	}
	if got := clearSkyShape(22); got != 0 {
		t.Errorf("post-dusk shape(22) = %v, want 0", got)
	}
	if got := clearSkyShape(solarSunriseHr); got != 0 {
		t.Errorf("sunrise shape = %v, want 0", got)
	}
	// Solar noon is the midpoint of [sunrise,sunset] = 13:00 → peak of 1.
	if got := clearSkyShape(13); math.Abs(got-1) > 1e-9 {
		t.Errorf("solar-noon shape(13) = %v, want 1", got)
	}
	if got := clearSkyShape(12); got <= 0.9 || got > 1 {
		t.Errorf("midday shape(12) = %v, want ~0.97", got)
	}
}

func TestDiurnalSolarForecast(t *testing.T) {
	if diurnalSolarForecast(0, 0) != nil {
		t.Error("peakKw<=0 should yield a nil forecast")
	}
	// Base at local midnight so step indices map cleanly to local hours.
	base := time.Date(2026, 5, 18, 0, 0, 0, 0, time.Local).Unix()
	const peak = 7.0
	fc := diurnalSolarForecast(base, peak)
	if len(fc) != planSteps {
		t.Fatalf("len = %d, want %d", len(fc), planSteps)
	}
	stepsPerHour := int(3600 / planStepSec)
	if v := fc[3*stepsPerHour]; v != 0 { // 03:00
		t.Errorf("03:00 forecast = %v, want 0 (dark)", v)
	}
	if v := fc[22*stepsPerHour]; v != 0 { // 22:00
		t.Errorf("22:00 forecast = %v, want 0 (dark)", v)
	}
	if v := fc[13*stepsPerHour]; math.Abs(v-peak) > 1e-9 { // 13:00 solar noon
		t.Errorf("solar-noon forecast = %v, want %v", v, peak)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// TASK-079 (GAP-05): localHourOf / price-shaping DST coverage.
//
// localHourOf and planStepImportPrice/planStepExportPrice all read
// time.Unix(unix, 0).Local() — the PROCESS's configured zone, exactly like
// the optimizer's serverNow rendering (CLAUDE.md, AD-004). To pin this
// deterministically against an explicit zone (never the test runner's own,
// which varies between a dev laptop and a UTC CI box), these tests
// temporarily repoint the package-level time.Local variable at
// America/Los_Angeles. This is a test-only technique — no production API
// change — and is safe here because this package's tests never call
// t.Parallel() (verified: no other test in this package does, and Plan()/
// the helpers under test spawn no goroutines that could race on time.Local
// concurrently with a test's own goroutine).
// ─────────────────────────────────────────────────────────────────────────

// dstForwardYear/dstBackYear/normalYear and friends mirror the same 2026
// America/Los_Angeles dates used in costmodel_test.go (that file is package
// orchestrator_test, this one is package orchestrator — the constants can't
// be shared, so they're duplicated here deliberately).
const (
	dstForwardYear, dstForwardMonth, dstForwardDay = 2026, time.March, 8
	dstBackYear, dstBackMonth, dstBackDay          = 2026, time.November, 1
	normalYear, normalMonth, normalDay             = 2026, time.June, 15
)

// withLocalZone temporarily repoints time.Local for the duration of the
// test (restored via t.Cleanup). Must not be used from a t.Parallel() test.
func withLocalZone(t *testing.T, loc *time.Location) {
	t.Helper()
	old := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = old })
}

func laLocationForPlanner(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("LoadLocation(America/Los_Angeles): %v (tzdata missing on this runner?)", err)
	}
	return loc
}

// TestLocalHourOf_AcrossDSTForward pins localHourOf's behavior across the
// spring-forward gap (2026-03-08, 02:00 PST -> 03:00 PDT): it must never
// report an hour-of-day in [2,3) (that local hour does not exist), and must
// advance monotonically with real elapsed time up to and through the gap.
func TestLocalHourOf_AcrossDSTForward(t *testing.T) {
	loc := laLocationForPlanner(t)
	withLocalZone(t, loc)

	anchor := time.Date(dstForwardYear, dstForwardMonth, dstForwardDay, 1, 0, 0, 0, loc)
	prev := localHourOf(anchor.Unix())
	if prev < 1 || prev >= 2 {
		t.Fatalf("localHourOf(01:00) = %v, want in [1,2)", prev)
	}
	sawInGap := false
	for i := 1; i <= 4*60; i++ { // 01:00 -> 05:00 real elapsed minutes, 1-min resolution
		ti := anchor.Add(time.Duration(i) * time.Minute)
		h := localHourOf(ti.Unix())
		if h >= 2 && h < 3 {
			sawInGap = true
		}
		// Monotonic non-decreasing except for the single expected jump
		// across the gap (from ~1.98 straight to 3.0) and the ordinary
		// hour-24 wrap (not reached in this 4h window).
		if h < prev-1e-9 && !(prev > 20 && h < 4) { // guard against a midnight wrap this window never reaches
			t.Errorf("t=%s: localHourOf went backwards (%v -> %v)", ti.Format(time.RFC3339), prev, h)
		}
		prev = h
	}
	if sawInGap {
		t.Error("localHourOf reported an hour inside [2,3) on the spring-forward gap day — that local hour does not exist")
	}
	if prev < 4 {
		t.Errorf("after the gap, localHourOf should have advanced past hour 4, got %v", prev)
	}
}

// TestLocalHourOf_AcrossDSTBack pins the fall-back fold (2026-11-01, 02:00
// PDT -> 01:00 PST): the repeated local hour 01:00-01:59 renders as the same
// fractional-hour RANGE twice (a real, expected non-monotonicity — this is
// documented, not a bug: the wall clock genuinely reads 01:00-01:59 twice),
// and the eventual advance past hour 2 must still happen exactly once.
func TestLocalHourOf_AcrossDSTBack(t *testing.T) {
	loc := laLocationForPlanner(t)
	withLocalZone(t, loc)

	anchor := time.Date(dstBackYear, dstBackMonth, dstBackDay, 1, 0, 0, 0, loc)
	first := localHourOf(anchor.Unix())
	if first < 1 || first >= 2 {
		t.Fatalf("localHourOf(01:00 first pass) = %v, want in [1,2)", first)
	}

	sawFold := false
	sawHour2 := false
	prev := first
	for i := 1; i <= 3*60; i++ { // 01:00 -> 04:00 real elapsed minutes
		ti := anchor.Add(time.Duration(i) * time.Minute)
		h := localHourOf(ti.Unix())
		if h < prev-1e-9 { // fractional hour went backwards -> the fold repeating 01:xx
			sawFold = true
		}
		if h >= 2 && h < 3 {
			sawHour2 = true
		}
		prev = h
	}
	if !sawFold {
		t.Error("never observed localHourOf go backwards (the repeated 01:xx range) across the fall-back fold")
	}
	if !sawHour2 {
		t.Error("never observed localHourOf reach hour 2 after the fold")
	}
}

// TestPlanStepPrice_ContinuityAcrossDSTTransitions walks a full planSteps
// (24h, 5-min resolution) planning horizon starting at local midnight on
// each of a normal day and both 2026 DST transition days, using
// planStepImportPrice with FallbackTOU=DefaultTOUCostModel() (no explicit
// ImportPricePerKwh slice, matching production's fallback path — see
// planner.go:631). It asserts the number of rate CHANGES across the day
// equals exactly 3 (the default schedule's 07:00/16:00/21:00 edges) on every
// day, including the transition days: no double-priced or skipped step
// introduced by the gap/fold, since all three tariff edges sit far from the
// 02:00 DST transition.
func TestPlanStepPrice_ContinuityAcrossDSTTransitions(t *testing.T) {
	loc := laLocationForPlanner(t)
	withLocalZone(t, loc)

	cases := []struct {
		name string
		y    int
		mo   time.Month
		d    int
	}{
		{"normal", normalYear, normalMonth, normalDay},
		{"dst-forward", dstForwardYear, dstForwardMonth, dstForwardDay},
		{"dst-back", dstBackYear, dstBackMonth, dstBackDay},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ws := time.Date(c.y, c.mo, c.d, 0, 0, 0, 0, loc).Unix()
			p := plannerTestBase()
			p.WindowStart = ws

			var changes int
			var rates []float64
			prevRate := planStepImportPrice(p, 0, ws)
			rates = append(rates, prevRate)
			for step := 1; step < planSteps; step++ {
				stepT := ws + int64(step)*planStepSec
				rate := planStepImportPrice(p, step, stepT)
				rates = append(rates, rate)
				if rate != prevRate {
					changes++
				}
				prevRate = rate
			}
			if changes != 3 {
				t.Errorf("%s: saw %d rate changes across the 24h plan horizon, want exactly 3 (07:00/16:00/21:00 edges); rates=%v", c.name, changes, rates)
			}
			// Every rate must be one of the DefaultTOUCostModel period rates
			// (or its default) — no NaN/zero/garbage value from a
			// mis-rendered instant near the transition.
			valid := map[float64]bool{0.10: true, 0.18: true, 0.38: true}
			for i, r := range rates {
				if !valid[r] {
					t.Errorf("%s: step %d rate=%v is not a recognized DefaultTOUCostModel rate", c.name, i, r)
				}
			}
		})
	}
}

// TestPlanStepExportPrice_NoImplicitTZDependency confirms
// planStepExportPrice (which only reads p.ExportPricePerKwh, never a
// FallbackTOU) is unaffected by the DST-day/zone concerns above — it takes
// no local-time-dependent path at all. Documented here so a future change
// that adds a zone-dependent fallback to export pricing gets a test to
// extend.
func TestPlanStepExportPrice_NoImplicitTZDependency(t *testing.T) {
	p := plannerTestBase()
	p.ExportPricePerKwh = nil
	if got := planStepExportPrice(p, 0, 0); got != 0 {
		t.Errorf("planStepExportPrice with nil ExportPricePerKwh = %v, want 0 (no TOU fallback exists for export)", got)
	}
}
