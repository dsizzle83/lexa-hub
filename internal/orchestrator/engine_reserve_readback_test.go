package orchestrator

import (
	"io"
	"log"
	"math"
	"sync"
	"testing"
	"time"
)

// White-box tests for Engine.EffectiveReservePct (GAP-8 read-back). They reuse
// intentEngine/battState/buildAfter and concReader/concOptimizer from
// engine_intents_test.go / engine_concurrency_test.go (same package), mirroring
// the ForecastSource read-back tests' style.

// TestEffectiveReservePct_SentinelThenClamp pins the three read-back cases the
// GAP-8 brief calls out: -1 before the first plan, a raise reflected verbatim,
// and a lowball clamped UP to the floor.
func TestEffectiveReservePct_SentinelThenClamp(t *testing.T) {
	cfg := PlannerCfg{TerminalReservePct: 20} // 20% floor
	state := battState()

	// Before any plan: the -1 "no plan yet" sentinel.
	e0 := intentEngine(cfg)
	if got := e0.EffectiveReservePct(); got != -1 {
		t.Fatalf("EffectiveReservePct() before first plan = %v, want -1 sentinel", got)
	}

	// No intent: the plan resolves to the cfg floor (20).
	base := intentEngine(cfg)
	base.buildAfter(state)
	if got := base.EffectiveReservePct(); math.Abs(got-20) > 1e-9 {
		t.Errorf("no-intent EffectiveReservePct() = %v, want 20 (cfg floor)", got)
	}

	// Raise to 40: reflected verbatim (max(40,20)=40).
	hi := intentEngine(cfg)
	hi.SetBackupReserve(40)
	hi.buildAfter(state)
	if got := hi.EffectiveReservePct(); math.Abs(got-40) > 1e-9 {
		t.Errorf("raised EffectiveReservePct() = %v, want 40", got)
	}

	// Lowball 5: clamped UP to the floor (max(5,20)=20).
	lo := intentEngine(cfg)
	lo.SetBackupReserve(5)
	lo.buildAfter(state)
	if got := lo.EffectiveReservePct(); math.Abs(got-20) > 1e-9 {
		t.Errorf("lowball EffectiveReservePct() = %v, want 20 (clamped up to floor)", got)
	}
}

// TestEffectiveReservePct_DefaultFloorAndNoBattery verifies the floor defaults
// to 20 when cfg is unset, and that the effective pct is stored even with no
// connected battery (the reserve overlay is a no-op on TerminalSocKwh then, but
// the read-back value must still reflect the floor in force).
func TestEffectiveReservePct_DefaultFloorAndNoBattery(t *testing.T) {
	e := intentEngine(PlannerCfg{}) // TerminalReservePct 0 → derived floor 20
	stateNoBatt := SystemState{Timestamp: fixedNow, Grid: NewGridState()}
	e.buildAfter(stateNoBatt)
	if got := e.EffectiveReservePct(); math.Abs(got-20) > 1e-9 {
		t.Errorf("no-battery EffectiveReservePct() = %v, want 20 (derived floor)", got)
	}

	// A raise still reflects even without a battery to apply it to.
	e2 := intentEngine(PlannerCfg{})
	e2.SetBackupReserve(55)
	e2.buildAfter(stateNoBatt)
	if got := e2.EffectiveReservePct(); math.Abs(got-55) > 1e-9 {
		t.Errorf("no-battery raised EffectiveReservePct() = %v, want 55", got)
	}
}

// TestEffectiveReservePct_ConcurrentRead runs EffectiveReservePct readers
// against a ticking, re-planning engine while SetBackupReserve mutates it — the
// 05 §4 concurrency requirement for the new atomic accessor, under -race.
func TestEffectiveReservePct_ConcurrentRead(t *testing.T) {
	prev := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(prev)

	eng := New(concReader{}, &concOptimizer{}, Config{Interval: 5 * time.Millisecond})
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
		id := w
		spawn(func(i int) { eng.SetBackupReserve(float64((id + i) % 100)) })
		spawn(func(i int) {
			if got := eng.EffectiveReservePct(); math.IsNaN(got) {
				t.Errorf("EffectiveReservePct() returned NaN")
			}
		})
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("EffectiveReservePct hammer goroutines never finished — possible deadlock")
	}
	eng.Stop()
}
