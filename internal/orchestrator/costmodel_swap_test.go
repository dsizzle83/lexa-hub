package orchestrator

// costmodel_swap_test.go covers the optimizer half of the tariff-intent path
// (TASK-094/§3.4): DefaultOptimizer.SwapCostModel + the costModel() accessor,
// and the CSIP-wins precedence the planner already enforces. White-box (package
// orchestrator) so it can reach the unexported costModel() accessor and
// planStepImportPrice — the exported Optimize path is exercised too.

import (
	"sync"
	"testing"
	"time"
)

// flatModel is a cost model whose single all-day period sits below its own peak
// threshold, so IsPeakHour is false at EVERY hour — the "no autonomous
// peak-discharge" baseline the swap tests toggle away from and back to.
func flatModel() *TOUCostModel {
	return NewTOUCostModel(
		[]TOUPeriod{{StartHour: 0, EndHour: 24, RatePerKwh: 0.10, Label: "flat"}},
		0.10, // default rate
		1.00, // peak threshold — 0.10 never reaches it, so nothing is "peak"
	)
}

// hasBattDischarge reports whether the plan commands any battery to discharge
// (positive setpoint = discharge, the peak-shift signal).
func hasBattDischarge(p Plan) bool {
	for _, c := range p.BatteryCommands {
		if c.SetpointW > 0 {
			return true
		}
	}
	return false
}

// swapState builds the same minimal peak-hour state the existing TOU optimizer
// tests use: one healthy battery at 80 % SOC, no plan target, clock at 17:00
// local (inside DefaultTOUCostModel's 16:00–21:00 peak window).
func swapState() SystemState {
	return SystemState{
		Timestamp: time.Date(2025, 1, 15, 17, 0, 0, 0, time.Local),
		Grid:      NewGridState(),
		Batteries: []BatteryState{ruleBat("bat-0", 0, 80, 5000)},
	}
}

// TestCostModel_AccessorOverrideAndRevert pins the costModel() contract
// directly: override wins when set, nil reverts to the constructor-time
// CostModel field.
func TestCostModel_AccessorOverrideAndRevert(t *testing.T) {
	opt := NewDefaultOptimizer()
	base := DefaultTOUCostModel()
	opt.CostModel = base

	if got := opt.costModel(); got != base {
		t.Fatalf("no override: costModel() = %p, want the constructor CostModel %p", got, base)
	}

	over := flatModel()
	opt.SwapCostModel(over)
	if got := opt.costModel(); got != over {
		t.Fatalf("after swap: costModel() = %p, want the swapped model %p", got, over)
	}

	opt.SwapCostModel(nil)
	if got := opt.costModel(); got != base {
		t.Fatalf("after nil swap: costModel() = %p, want revert to CostModel %p", got, base)
	}
}

// TestSwapCostModel_ChangesPeakDischarge is the end-to-end behavior test: with
// a flat (never-peak) model installed, Rule 5 does not discharge at 17:00;
// swapping in DefaultTOUCostModel (17:00 is peak) makes it discharge; a nil
// swap reverts to the flat model and the discharge stops again — proving the
// swap, and the nil-revert, both flow through Optimize.
func TestSwapCostModel_ChangesPeakDischarge(t *testing.T) {
	opt := NewDefaultOptimizer()
	opt.CostModel = flatModel() // constructor-time model: never peak

	if hasBattDischarge(opt.Optimize(swapState())) {
		t.Fatal("baseline flat model: expected no peak discharge at 17:00")
	}

	opt.SwapCostModel(DefaultTOUCostModel()) // 17:00 is squarely inside 16–21 peak
	if !hasBattDischarge(opt.Optimize(swapState())) {
		t.Fatal("after swap to peak model: expected battery discharge at 17:00")
	}

	opt.SwapCostModel(nil) // revert to the constructor flat model
	if hasBattDischarge(opt.Optimize(swapState())) {
		t.Fatal("after nil swap (revert to flat): expected no peak discharge at 17:00")
	}
}

// TestSwapCostModel_ConcurrentWithOptimize_Race hammers SwapCostModel from
// several goroutines while a single goroutine runs Optimize in a loop — the
// production shape (one control-loop writer, tariff swaps arriving from the
// intent-adoption goroutine). Under -race this pins that the atomic.Pointer
// override needs no lock around Optimize's CostModel reads.
func TestSwapCostModel_ConcurrentWithOptimize_Race(t *testing.T) {
	opt := NewDefaultOptimizer()
	opt.CostModel = DefaultTOUCostModel()
	state := swapState()

	models := []*TOUCostModel{DefaultTOUCostModel(), flatModel(), nil}

	const iters = 2000
	var wg sync.WaitGroup

	// Single Optimize goroutine — the control-loop single-writer invariant
	// (only the override is shared; the guard maps stay single-goroutine).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			_ = opt.Optimize(state)
		}
	}()

	// Multiple swappers race the atomic override (concurrent Stores are safe).
	for s := 0; s < 3; s++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				opt.SwapCostModel(models[i%len(models)])
			}
		}()
	}

	wg.Wait()
}

// TestFallbackTOU_CSIPImportPricesWin pins the precedence chain at the unit
// level from the tariff perspective (§3.4 "CSIP-published pricing still wins"):
// a non-nil ImportPricePerKwh slice (CSIP SetPrices) overrides FallbackTOU for
// the steps it covers; steps beyond the slice fall through to FallbackTOU.
// Constructs PlannerParams directly — no MQTT, no engine.
func TestFallbackTOU_CSIPImportPricesWin(t *testing.T) {
	fb := DefaultTOUCostModel()
	// 02:00 local is off-peak (0.10) under FallbackTOU, distinct from the CSIP
	// slice value below — so a match on step 0 genuinely proves precedence.
	off := time.Date(2025, 1, 15, 2, 0, 0, 0, time.Local).Unix()
	fbRate := fb.CurrentRate(time.Unix(off, 0).Local())
	if fbRate == 0.99 {
		t.Fatal("test setup: FallbackTOU off-peak rate must differ from the CSIP price 0.99 to prove precedence")
	}

	p := PlannerParams{
		ImportPricePerKwh: []float64{0.99}, // CSIP-published rate, covers step 0 only
		FallbackTOU:       fb,
	}

	if got := planStepImportPrice(p, 0, off); got != 0.99 {
		t.Fatalf("step 0: CSIP price slice must win over FallbackTOU, got %v want 0.99", got)
	}
	if got := planStepImportPrice(p, 1, off); got != fbRate {
		t.Fatalf("step 1 (beyond the slice) must fall through to FallbackTOU, got %v want %v", got, fbRate)
	}
}
