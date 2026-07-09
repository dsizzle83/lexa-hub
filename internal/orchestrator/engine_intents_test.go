package orchestrator

import (
	"io"
	"log"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"
)

// White-box tests for the Unit 3.1 intent setters + planner-input gates
// (DEVICE_ROADMAP §3.2–3.3). They live in package orchestrator (like
// engine_concurrency_test.go) so they can drive the enqueue→drainCmds→
// planInSnapshot→buildPlannerParams path directly and inspect e.state /
// e.forecast* — mirroring TestEngine_CommandOrdering's same-goroutine,
// deterministic style. concReader/concOptimizer are reused from
// engine_concurrency_test.go (same package).

// intentEngine builds an unstarted Engine with the given planner config. The
// setters and buildPlannerParams are exercised on the test goroutine (no
// Start), so there is no planner goroutine to race e.state.lastSolarPeakKw.
func intentEngine(cfg PlannerCfg) *Engine {
	return New(concReader{}, &concOptimizer{}, Config{Interval: time.Hour, Planner: cfg})
}

// fixedNow is a deterministic build timestamp: forecast staleness is judged as
// state.Timestamp.Unix()-ReceivedUnix (ClockOffset 0), so pinning it makes the
// age assertions exact.
var fixedNow = time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)

// battState returns one connected 10 kWh battery so buildPlannerParams sets
// p.BattCapacityKwh (the reserve overlay is a no-op without a battery).
func battState() SystemState {
	b := NewBatteryState("bat-0")
	b.Connected = true
	b.CapacityWh = 10000 // 10 kWh
	b.MaxChargeW = 5000
	b.MaxDischargeW = 5000
	b.SOC = 50
	return SystemState{Timestamp: fixedNow, Batteries: []BatteryState{b}, Grid: NewGridState()}
}

// buildAfter enqueues via setter(s), drains, and builds params for state.
func (e *Engine) buildAfter(state SystemState) PlannerParams {
	e.drainCmds()
	return e.buildPlannerParams(state, e.state.planInSnapshot())
}

// ── EV goal ─────────────────────────────────────────────────────────────────

func TestSetEVGoal_OverridesConfig(t *testing.T) {
	cfg := PlannerCfg{
		EVCapacityKwh:  60,
		EVMaxChargeKw:  11,
		EVTargetSocPct: 80, // config target: 48 kWh
		EVDepartureHH:  7,
	}
	// A live EVSE session so config derives InitialEVSocKwh = 25%*60 = 15 kWh.
	ev := EVSEState{StationID: "cs-1", ConnectorID: 1, Connected: true, SessionActive: true, SOC: 25, MaxCurrentA: 32, VoltageV: 240}
	state := battState()
	state.EVSEs = []EVSEState{ev}

	// Baseline: no goal → config-derived target/initial.
	base := intentEngine(cfg).buildAfter(state)
	if math.Abs(base.EVTargetSocKwh-48) > 1e-9 {
		t.Fatalf("config baseline EVTargetSocKwh = %v, want 48", base.EVTargetSocKwh)
	}
	if math.Abs(base.InitialEVSocKwh-15) > 1e-9 {
		t.Fatalf("config baseline InitialEVSocKwh = %v, want 15", base.InitialEVSocKwh)
	}

	// Goal with a stated initial SOC wins on all three fields.
	e := intentEngine(cfg)
	e.SetEVGoal(EVGoal{TargetSocKwh: 55, DepartureUnix: 1234567890, InitialSocKwh: 30})
	got := e.buildAfter(state)
	if math.Abs(got.EVTargetSocKwh-55) > 1e-9 {
		t.Errorf("EVTargetSocKwh = %v, want 55 (goal wins over config)", got.EVTargetSocKwh)
	}
	if got.EVDepartureUnix != 1234567890 {
		t.Errorf("EVDepartureUnix = %d, want 1234567890 (goal wins)", got.EVDepartureUnix)
	}
	if math.Abs(got.InitialEVSocKwh-30) > 1e-9 {
		t.Errorf("InitialEVSocKwh = %v, want 30 (stated initial seeds integration)", got.InitialEVSocKwh)
	}

	// Goal with InitialSocKwh < 0 keeps the live-EVSE initial (15 kWh).
	e2 := intentEngine(cfg)
	e2.SetEVGoal(EVGoal{TargetSocKwh: 55, DepartureUnix: 1234567890, InitialSocKwh: -1})
	got2 := e2.buildAfter(state)
	if math.Abs(got2.InitialEVSocKwh-15) > 1e-9 {
		t.Errorf("InitialEVSocKwh = %v, want 15 (negative initial leaves live SOC)", got2.InitialEVSocKwh)
	}
}

// ── Backup reserve: clamps UP only ────────────────────────────────────────────

func TestSetBackupReserve_RaiseOnly(t *testing.T) {
	cfg := PlannerCfg{TerminalReservePct: 20} // 20% floor of 10 kWh = 2 kWh
	state := battState()

	// Baseline (no intent): battery loop sets TerminalSocKwh to the 20% floor.
	base := intentEngine(cfg).buildAfter(state)
	if math.Abs(base.TerminalSocKwh-2.0) > 1e-9 {
		t.Fatalf("baseline TerminalSocKwh = %v, want 2.0 (20%% of 10 kWh)", base.TerminalSocKwh)
	}

	// Lowball intent (5%) must NOT lower the floor: max(5,20)=20 → 2 kWh.
	lo := intentEngine(cfg)
	lo.SetBackupReserve(5)
	gotLo := lo.buildAfter(state)
	if math.Abs(gotLo.TerminalSocKwh-2.0) > 1e-9 {
		t.Errorf("lowball TerminalSocKwh = %v, want 2.0 (floor wins, never lower)", gotLo.TerminalSocKwh)
	}

	// Higher intent (40%) raises: max(40,20)=40 → 4 kWh, and lifts MinBattSocKwh.
	hi := intentEngine(cfg)
	hi.SetBackupReserve(40)
	gotHi := hi.buildAfter(state)
	if math.Abs(gotHi.TerminalSocKwh-4.0) > 1e-9 {
		t.Errorf("raised TerminalSocKwh = %v, want 4.0 (intent raises floor)", gotHi.TerminalSocKwh)
	}
	if math.Abs(gotHi.MinBattSocKwh-4.0) > 1e-9 {
		t.Errorf("raised MinBattSocKwh = %v, want 4.0 (reserve lifts the operating floor)", gotHi.MinBattSocKwh)
	}
}

// TestSetBackupReserve_DefaultFloorWhenCfgUnset verifies the cfg-DERIVED floor
// (defaulted to 20 when TerminalReservePct<=0) is what a lowball intent clamps
// against — not the raw 0 cfg field, which would let the intent drop below the
// floor the battery loop applied.
func TestSetBackupReserve_DefaultFloorWhenCfgUnset(t *testing.T) {
	cfg := PlannerCfg{} // TerminalReservePct 0 → effective floor 20%
	state := battState()

	e := intentEngine(cfg)
	e.SetBackupReserve(5) // max(5, 20) = 20 → 2 kWh
	got := e.buildAfter(state)
	if math.Abs(got.TerminalSocKwh-2.0) > 1e-9 {
		t.Errorf("TerminalSocKwh = %v, want 2.0 (clamped to derived 20%% floor, not raw cfg 0)", got.TerminalSocKwh)
	}
}

// ── Solar forecast gate ───────────────────────────────────────────────────────

func TestSetSolarForecast_FreshUsesExternal(t *testing.T) {
	state := battState()
	ws := state.Timestamp.Unix() // ClockOffset 0 ⇒ p.WindowStart == this
	step := make([]float64, planSteps)
	for i := range step {
		step[i] = 2 + float64(i)*0.001
	}

	e := intentEngine(PlannerCfg{})
	e.SetSolarForecast(ExternalForecast{StepKw: step, WindowStart: ws, ReceivedUnix: ws - 60}) // age 60s
	got := e.buildAfter(state)

	if src := e.ForecastSource(); src != "external" {
		t.Errorf("ForecastSource() = %q, want external", src)
	}
	if age := e.ForecastAgeSeconds(); age != 60 {
		t.Errorf("ForecastAgeSeconds() = %d, want 60", age)
	}
	if len(got.SolarForecastKw) != planSteps {
		t.Fatalf("SolarForecastKw len = %d, want %d", len(got.SolarForecastKw), planSteps)
	}
	if math.Abs(got.SolarForecastKw[0]-step[0]) > 1e-9 || math.Abs(got.SolarForecastKw[10]-step[10]) > 1e-9 {
		t.Errorf("external forecast not resampled onto the grid: got[0]=%v want %v; got[10]=%v want %v",
			got.SolarForecastKw[0], step[0], got.SolarForecastKw[10], step[10])
	}
}

func TestSetSolarForecast_StaleFallsBackToDiurnal(t *testing.T) {
	state := battState()
	ws := state.Timestamp.Unix()
	step := make([]float64, planSteps)
	for i := range step {
		step[i] = 3.0
	}
	const staleAge = 13 * 3600 // > 12h gate

	cfg := PlannerCfg{SolarPeakKw: 5} // gives the diurnal fallback a peak to shape
	e := intentEngine(cfg)
	e.SetSolarForecast(ExternalForecast{StepKw: step, WindowStart: ws, ReceivedUnix: ws - staleAge})
	got := e.buildAfter(state)

	if src := e.ForecastSource(); src != "diurnal" {
		t.Errorf("ForecastSource() = %q, want diurnal (stale forecast rejected)", src)
	}
	if age := e.ForecastAgeSeconds(); age != staleAge {
		t.Errorf("ForecastAgeSeconds() = %d, want %d (staleness still reported)", age, staleAge)
	}
	// The fallback ran with the config peak, NOT the external series.
	want := diurnalSolarForecast(got.WindowStart, 5)
	if !reflect.DeepEqual(got.SolarForecastKw, want) {
		t.Errorf("SolarForecastKw is not the diurnal fallback curve")
	}
}

// TestForecastSource_EmptyBeforeFirstPlan pins the "empty before first plan"
// contract and the -1 initial age.
func TestForecastSource_EmptyBeforeFirstPlan(t *testing.T) {
	e := intentEngine(PlannerCfg{})
	if src := e.ForecastSource(); src != "" {
		t.Errorf("ForecastSource() before first plan = %q, want empty", src)
	}
	if age := e.ForecastAgeSeconds(); age != -1 {
		t.Errorf("ForecastAgeSeconds() before first plan = %d, want -1", age)
	}
	// A plan with no external forecast keeps age at -1 and reports diurnal.
	e.buildAfter(battState())
	if src := e.ForecastSource(); src != "diurnal" {
		t.Errorf("ForecastSource() after diurnal plan = %q, want diurnal", src)
	}
	if age := e.ForecastAgeSeconds(); age != -1 {
		t.Errorf("ForecastAgeSeconds() with no external forecast = %d, want -1", age)
	}
}

// ── resampleForecast ─────────────────────────────────────────────────────────

func TestResampleForecast_Table(t *testing.T) {
	const ws = int64(1_000_000)
	cases := []struct {
		name        string
		fcWindow    int64
		step        []float64
		checkIdx    []int
		checkWant   []float64
		wantLenFull bool
	}{
		{
			name: "aligned-offset-0", fcWindow: ws, step: []float64{1, 2, 3},
			checkIdx: []int{0, 1, 2, 3}, checkWant: []float64{1, 2, 3, 0}, // step 3 zero-filled
		},
		{
			name: "positive-offset-shifts-back", fcWindow: ws - 2*planStepSec, step: []float64{10, 11, 12, 13, 14},
			checkIdx: []int{0, 1, 2, 3}, checkWant: []float64{12, 13, 14, 0}, // offset 2
		},
		{
			// A forecast starting 3 steps in the FUTURE lands at its temporally
			// correct steps (3..5); the leading steps zero-fill. Start-aligning
			// (the old clamp-to-0 behavior) would have shifted the curve 3 steps
			// early — wrong solar peak timing (principal review finding, unit 3.1).
			name: "future-start-time-aligned-leading-zero-fill", fcWindow: ws + 3*planStepSec, step: []float64{7, 8, 9},
			checkIdx: []int{0, 1, 2, 3, 4, 5, 6}, checkWant: []float64{0, 0, 0, 7, 8, 9, 0},
		},
		{
			name: "negative-values-clamped", fcWindow: ws, step: []float64{1, -5, 3},
			checkIdx: []int{0, 1, 2}, checkWant: []float64{1, 0, 3},
		},
		{
			name: "non-finite-clamped", fcWindow: ws, step: []float64{math.NaN(), math.Inf(1), 2, math.Inf(-1)},
			checkIdx: []int{0, 1, 2, 3}, checkWant: []float64{0, 0, 2, 0},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &ExternalForecast{StepKw: tc.step, WindowStart: tc.fcWindow}
			out := resampleForecast(fc, ws)
			if len(out) != planSteps {
				t.Fatalf("len(out) = %d, want %d", len(out), planSteps)
			}
			for k, idx := range tc.checkIdx {
				if math.Abs(out[idx]-tc.checkWant[k]) > 1e-9 {
					t.Errorf("out[%d] = %v, want %v", idx, out[idx], tc.checkWant[k])
				}
			}
		})
	}

	// nil forecast → all-zero, full length (defensive).
	if out := resampleForecast(nil, ws); len(out) != planSteps {
		t.Fatalf("nil forecast len = %d, want %d", len(out), planSteps)
	}
}

// ── Load profile readthrough ─────────────────────────────────────────────────

func TestSetLoadProfile_ReadsThroughAndCopies(t *testing.T) {
	state := battState()
	profile := []float64{5, 6, 7}

	e := intentEngine(PlannerCfg{})
	e.SetLoadProfile(profile)
	// Mutate the caller's slice AFTER the setter: the defensive copy must make
	// the stored profile immune (retained-handler buffer-reuse contract).
	profile[0] = 999

	got := e.buildAfter(state)
	if len(got.LoadProfileKw) != 3 {
		t.Fatalf("LoadProfileKw len = %d, want 3", len(got.LoadProfileKw))
	}
	if got.LoadProfileKw[0] != 5 {
		t.Errorf("LoadProfileKw[0] = %v, want 5 (defensive copy immune to caller mutation)", got.LoadProfileKw[0])
	}
	if v := planStepLoad(got, 1); v != 6 {
		t.Errorf("planStepLoad(1) = %v, want 6", v)
	}
	// Out of profile range → scalar fallback. With no grid meter the derived
	// LoadForecastKw is NaN, so compare NaN-aware: the point is that the
	// out-of-range step returns exactly the scalar, whatever its value.
	sameFloat := func(a, b float64) bool { return a == b || (math.IsNaN(a) && math.IsNaN(b)) }
	if v := planStepLoad(got, 50); !sameFloat(v, got.LoadForecastKw) {
		t.Errorf("planStepLoad(50) = %v, want scalar %v", v, got.LoadForecastKw)
	}

	// Empty profile restores the scalar path.
	e.SetLoadProfile(nil)
	got2 := e.buildAfter(state)
	if len(got2.LoadProfileKw) != 0 {
		t.Errorf("empty SetLoadProfile left LoadProfileKw = %v, want empty", got2.LoadProfileKw)
	}
}

// ── Fallback TOU ─────────────────────────────────────────────────────────────

func TestSetFallbackTOU_LandsInParams(t *testing.T) {
	state := battState()
	m := DefaultTOUCostModel()

	e := intentEngine(PlannerCfg{})
	e.SetFallbackTOU(m)
	got := e.buildAfter(state)
	if got.FallbackTOU != m {
		t.Errorf("FallbackTOU = %p, want the intent model %p (wins over the default)", got.FallbackTOU, m)
	}
}

// ── Concurrency ──────────────────────────────────────────────────────────────

// TestEngine_IntentSetters_ConcurrentHammer runs all five intent setters from
// many goroutines against a ticking (and re-planning) engine under -race,
// alongside readers of ForecastSource/ForecastAgeSeconds/LastPlan — the 05 §4
// concurrency-test requirement for new mutator paths. Mirrors
// TestEngine_ConcurrentHammer.
func TestEngine_IntentSetters_ConcurrentHammer(t *testing.T) {
	prev := log.Writer()
	log.SetOutput(io.Discard) // the bounded-channel drop path logs per drop; silence it
	defer log.SetOutput(prev)

	eng := New(concReader{}, &concOptimizer{}, Config{Interval: 5 * time.Millisecond})
	eng.Start()

	const workers = 4
	const iterations = 100
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
		id := w
		spawn(func(i int) {
			eng.SetEVGoal(EVGoal{TargetSocKwh: float64(id*10 + i), DepartureUnix: int64(i), InitialSocKwh: float64(i - 1)})
		})
		spawn(func(i int) { eng.SetBackupReserve(float64((id + i) % 100)) })
		spawn(func(i int) {
			eng.SetSolarForecast(ExternalForecast{StepKw: []float64{float64(id), float64(i)}, WindowStart: int64(i), ReceivedUnix: time.Now().Unix()})
		})
		spawn(func(i int) { eng.SetLoadProfile([]float64{float64(id), float64(i)}) })
		spawn(func(i int) { eng.SetFallbackTOU(DefaultTOUCostModel()) })
		spawn(func(i int) { _ = eng.ForecastSource(); _ = eng.ForecastAgeSeconds(); _ = eng.LastPlan() })
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("intent-setter hammer goroutines never finished — possible deadlock")
	}
	eng.Stop()
}
