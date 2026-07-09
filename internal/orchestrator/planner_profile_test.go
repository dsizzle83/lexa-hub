package orchestrator

import (
	"math"
	"testing"
)

// White-box tests for the per-step load profile (Unit 3.1 §3.3): the
// planStepLoad accessor and its consumption by the DP. planStepLoad mirrors
// planStepSolar but falls back to the SCALAR LoadForecastKw (not zero) for
// empty/out-of-range steps, so the DP's behaviour is byte-identical to the old
// `p.LoadForecastKw` read whenever no profile is set.

func TestPlanStepLoad_Table(t *testing.T) {
	full := make([]float64, planSteps)
	for i := range full {
		full[i] = 1 + float64(i)*0.01
	}

	cases := []struct {
		name    string
		profile []float64
		scalar  float64
		idx     int
		want    float64
	}{
		{"empty-profile-uses-scalar", nil, 1.5, 0, 1.5},
		{"empty-profile-uses-scalar-high-idx", nil, 1.5, 100, 1.5},
		{"full-profile-in-range", full, 9.9, 5, full[5]},
		{"full-profile-in-range-last", full, 9.9, planSteps - 1, full[planSteps-1]},
		{"short-profile-in-range", []float64{2, 3}, 1.5, 1, 3},
		{"short-profile-out-of-range-falls-back", []float64{2, 3}, 1.5, 2, 1.5},
		{"negative-index-falls-back", nil, 1.5, -1, 1.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := PlannerParams{LoadForecastKw: tc.scalar, LoadProfileKw: tc.profile}
			if got := planStepLoad(p, tc.idx); got != tc.want {
				t.Errorf("planStepLoad(idx=%d) = %v, want %v", tc.idx, got, tc.want)
			}
		})
	}
}

// TestPlan_LoadProfile_Consumed proves the DP actually reads the per-step
// profile: with no battery and no EV, ExpectedGridW for each interval collapses
// to load(kW)*1000, so a profile value at step 0 (and the scalar fallback at
// step 1, where the short profile does not cover) show up directly in the plan
// intervals. Asserts on plan intervals, not decision strings.
func TestPlan_LoadProfile_Consumed(t *testing.T) {
	const scalar = 1.0
	// A one-element profile: step 0 is governed by the profile (5 kW), every
	// later step falls back to the scalar (1 kW) — exercises both paths.
	profile := []float64{5.0}

	base := plannerTestBase() // no battery, no EV, no solar
	base.LoadForecastKw = scalar

	// Scalar-only reference run.
	ref := base
	ref.LoadProfileKw = nil
	refPlan := NewDailyPlanner().Plan(ref)
	if got := refPlan.Intervals[0].ExpectedGridW; math.Abs(got-scalar*1000) > 1 {
		t.Fatalf("scalar run: Intervals[0].ExpectedGridW = %.1f, want ~%.1f", got, scalar*1000)
	}

	// Profile run.
	prof := base
	prof.LoadProfileKw = profile
	profPlan := NewDailyPlanner().Plan(prof)

	if got := profPlan.Intervals[0].ExpectedGridW; math.Abs(got-profile[0]*1000) > 1 {
		t.Errorf("profile run: Intervals[0].ExpectedGridW = %.1f, want ~%.1f (profile consumed)",
			got, profile[0]*1000)
	}
	if got := profPlan.Intervals[1].ExpectedGridW; math.Abs(got-scalar*1000) > 1 {
		t.Errorf("profile run: Intervals[1].ExpectedGridW = %.1f, want ~%.1f (scalar fallback out of profile range)",
			got, scalar*1000)
	}
}
