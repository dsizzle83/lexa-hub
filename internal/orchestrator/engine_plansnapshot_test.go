package orchestrator

import (
	"io"
	"log"
	"math"
	"sync"
	"testing"
	"time"
)

// White-box tests for Engine.DailyPlanSnapshot (GAP-7). They live in package
// orchestrator (like engine_intents_test.go / engine_reserve_readback_test.go)
// so they can drive replan() directly and read e.state. battState/fixedNow are
// reused from engine_intents_test.go (same package).

// snapReader is a fixed-state SystemReader: replan() reads the same battery
// state (fixedNow timestamp, one connected 10 kWh battery) on every call, so the
// captured forecast/SOC are deterministic.
type snapReader struct{ state SystemState }

func (r snapReader) ReadSystemState() (SystemState, error) { return r.state, nil }

// snapBattState is battState with a finite grid reading so InferredLoadW (hence
// LoadForecastKw) is finite: a NaN load makes every DP transition cost NaN → an
// all-NaN plan (existing planner behaviour, orthogonal to this capture), which
// is not what these tests exercise.
func snapBattState() SystemState {
	st := battState()
	st.Grid.NetW = 500 // finite site draw → feasible DP
	return st
}

// TestDailyPlanSnapshot_SentinelThenCapture pins the three brief requirements:
// nil before the first plan; after a plan it returns the 288 slots + the forecast
// the plan used (an external forecast, resampled 1:1) + the per-slot SOC.
func TestDailyPlanSnapshot_SentinelThenCapture(t *testing.T) {
	e := New(snapReader{state: snapBattState()}, &concOptimizer{}, Config{Interval: time.Hour})

	// Before any plan: the "no plan yet" sentinel.
	if snap := e.DailyPlanSnapshot(); snap.Plan != nil {
		t.Fatalf("DailyPlanSnapshot before first plan: Plan = %+v, want nil sentinel", snap.Plan)
	}

	// Feed a known external forecast aligned to the plan window so the captured
	// ForecastKw is deterministic (bypasses the diurnal high-water path). fixedNow
	// is on a 5-min boundary, so ws == fixedNow.Unix() and the resample offset is 0.
	ws := fixedNow.Unix() - fixedNow.Unix()%planStepSec
	forecast := make([]float64, planSteps)
	for i := range forecast {
		forecast[i] = float64(i) * 0.01 // 0..2.87 kW ramp, all finite ≥ 0
	}
	e.SetSolarForecast(ExternalForecast{StepKw: forecast, WindowStart: ws, ReceivedUnix: fixedNow.Unix()})
	e.drainCmds()
	e.replan()

	snap := e.DailyPlanSnapshot()
	if snap.Plan == nil {
		t.Fatal("DailyPlanSnapshot after replan: Plan is nil")
	}
	if len(snap.Plan.Intervals) != planSteps {
		t.Fatalf("plan slots = %d, want %d", len(snap.Plan.Intervals), planSteps)
	}

	// The captured forecast is exactly the forecast the plan was built against.
	if len(snap.ForecastKw) != planSteps {
		t.Fatalf("ForecastKw len = %d, want %d", len(snap.ForecastKw), planSteps)
	}
	for i := range forecast {
		if math.Abs(snap.ForecastKw[i]-forecast[i]) > 1e-9 {
			t.Fatalf("ForecastKw[%d] = %v, want %v (the forecast the plan used)", i, snap.ForecastKw[i], forecast[i])
		}
	}

	// Capacity + voltage captured for the SOC/EV-power projection.
	if math.Abs(snap.BattCapKwh-10) > 1e-9 {
		t.Errorf("BattCapKwh = %v, want 10", snap.BattCapKwh)
	}

	// Per-slot SOC: slot 0 begins at the initial SOC (50% of 10 kWh = 5 kWh),
	// snapped to the SOC grid, and every slot is finite and inside [0, capacity].
	if got := snap.Plan.Intervals[0].SocKwh; math.IsNaN(got) || math.Abs(got-5) > 0.5 {
		t.Errorf("Intervals[0].SocKwh = %v, want ≈5 (initial SOC)", got)
	}
	for i, iv := range snap.Plan.Intervals {
		if math.IsNaN(iv.SocKwh) {
			t.Fatalf("Intervals[%d].SocKwh is NaN despite a modelled battery", i)
		}
		if iv.SocKwh < -1e-6 || iv.SocKwh > 10+1e-6 {
			t.Errorf("Intervals[%d].SocKwh = %v out of [0,10]", i, iv.SocKwh)
		}
	}
}

// TestDailyPlanSnapshot_NoBattery verifies the no-battery case: a plan is still
// captured, but BattCapKwh is 0 and per-slot SocKwh is NaN (so the schedule
// builder emits an empty battery series).
func TestDailyPlanSnapshot_NoBattery(t *testing.T) {
	state := SystemState{Timestamp: fixedNow, Grid: NewGridState()} // no batteries
	e := New(snapReader{state: state}, &concOptimizer{}, Config{Interval: time.Hour})
	e.replan()

	snap := e.DailyPlanSnapshot()
	if snap.Plan == nil {
		t.Fatal("Plan is nil after replan")
	}
	if snap.BattCapKwh != 0 {
		t.Errorf("BattCapKwh = %v, want 0 with no battery", snap.BattCapKwh)
	}
	if got := snap.Plan.Intervals[0].SocKwh; !math.IsNaN(got) {
		t.Errorf("Intervals[0].SocKwh = %v, want NaN with no battery", got)
	}
}

// TestDailyPlanSnapshot_ConcurrentRead runs DailyPlanSnapshot readers against a
// ticking, re-planning engine while SetSolarForecast mutates it — the 05 §4
// concurrency requirement for the new atomic accessor, under -race.
func TestDailyPlanSnapshot_ConcurrentRead(t *testing.T) {
	prev := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(prev)

	eng := New(snapReader{state: snapBattState()}, &concOptimizer{}, Config{Interval: 5 * time.Millisecond})
	eng.Start()

	const workers = 4
	const iterations = 200
	var wg sync.WaitGroup
	spawn := func(f func(i int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				f(i)
			}
		}()
	}
	for w := 0; w < workers; w++ {
		spawn(func(i int) {
			f := make([]float64, planSteps)
			for k := range f {
				f[k] = float64((i + k) % 5)
			}
			eng.SetSolarForecast(ExternalForecast{StepKw: f, WindowStart: fixedNow.Unix(), ReceivedUnix: fixedNow.Unix()})
		})
		spawn(func(int) {
			snap := eng.DailyPlanSnapshot()
			// Read through the pointer fields to make -race catch any torn read.
			if snap.Plan != nil {
				_ = snap.Plan.Intervals[0].SocKwh
				_ = len(snap.ForecastKw)
			}
		})
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("DailyPlanSnapshot hammer goroutines never finished — possible deadlock")
	}
	eng.Stop()
}
