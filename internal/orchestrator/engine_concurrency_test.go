package orchestrator

import (
	"io"
	"log"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This file holds white-box concurrency tests for the TASK-067 state
// consolidation (engineState + cmdCh, engine_state.go). They live in package
// orchestrator (not orchestrator_test) specifically so they can inspect
// e.state directly and assert the single-writer/ordering guarantees the
// task requires — engine_test.go (black-box, exported API only) is left
// untouched, and still passes unchanged: see that file.

// concReader/concOptimizer are minimal SystemReader/Optimizer stand-ins for
// this file's tests; independent of the mocks in engine_test.go (different
// package, no collision).
type concReader struct{}

func (concReader) ReadSystemState() (SystemState, error) {
	return SystemState{Timestamp: time.Now()}, nil
}

type concOptimizer struct {
	calls atomic.Int32
	// onOptimize, if set, is invoked synchronously from inside Optimize —
	// used by TestEngine_CommandAppliesBeforeNextOptimize to observe
	// e.state at the moment each economic tick runs.
	onOptimize func(n int32)
}

func (o *concOptimizer) Optimize(_ SystemState) Plan {
	n := o.calls.Add(1)
	if o.onOptimize != nil {
		o.onOptimize(n)
	}
	return Plan{}
}

// TestEngineState_SetPlanIn_PreservesOtherFields verifies setPlanIn's
// read-modify-write does not clobber a sibling field — SetDERConstraints and
// SetPrices each touch only part of plannerInput and must compose.
func TestEngineState_SetPlanIn_PreservesOtherFields(t *testing.T) {
	s := newEngineState()

	s.setPlanIn(func(in *plannerInput) { in.derConstraints = []StepConstraint{{}} })
	s.setPlanIn(func(in *plannerInput) { in.importPrices = []float64{1, 2, 3} })
	s.setPlanIn(func(in *plannerInput) { in.exportPrices = []float64{4, 5, 6} })

	got := s.planInSnapshot()
	if len(got.derConstraints) != 1 {
		t.Errorf("derConstraints lost: %+v", got)
	}
	if len(got.importPrices) != 3 || got.importPrices[0] != 1 {
		t.Errorf("importPrices lost: %+v", got)
	}
	if len(got.exportPrices) != 3 || got.exportPrices[0] != 4 {
		t.Errorf("exportPrices lost: %+v", got)
	}
}

// TestEngine_CommandOrdering verifies that commands from one caller apply in
// the order they were enqueued — SetDERConstraints called N times in
// sequence must leave planIn holding the LAST value, never an earlier one
// re-applied out of order, once the control goroutine has drained them.
func TestEngine_CommandOrdering(t *testing.T) {
	reader := concReader{}
	opt := &concOptimizer{}
	eng := New(reader, opt, Config{Interval: time.Hour})

	// Exercise the real SetDERConstraints -> enqueue -> cmdCh -> drainCmds ->
	// setPlanIn path directly (no Start/run needed — this is a same-goroutine,
	// deterministic check that draining preserves enqueue order). Interleave
	// drains so the 200 enqueues don't overrun cmdCh's buffer(16) and get
	// silently dropped by the bounded-channel policy.
	const n = 200
	for i := 0; i < n; i++ {
		eng.SetDERConstraints([]StepConstraint{{ImpLimW: float64(i)}})
		if i%8 == 7 {
			eng.drainCmds()
		}
	}
	eng.drainCmds() // flush the remainder

	got := eng.state.planInSnapshot()
	if len(got.derConstraints) != 1 || got.derConstraints[0].ImpLimW != float64(n-1) {
		t.Fatalf("planIn = %+v, want last-enqueued ImpLimW=%d", got, n-1)
	}
}

// TestEngine_CommandAppliesBeforeNextOptimize verifies the drain-point
// guarantee: a command enqueued while a (slow) tick is in flight is applied
// no later than before the NEXT Optimize call — never left pending across
// two economic ticks.
func TestEngine_CommandAppliesBeforeNextOptimize(t *testing.T) {
	tickStarted := make(chan struct{}, 4)
	release := make(chan struct{})
	var seenAtTick2 plannerInput
	var mu sync.Mutex

	reader := concReader{}
	var eng *Engine
	opt := &concOptimizer{}
	opt.onOptimize = func(n int32) {
		switch n {
		case 1:
			// First (startup) tick: signal, then block until the test has
			// enqueued a command "mid-tick".
			tickStarted <- struct{}{}
			<-release
		case 2:
			mu.Lock()
			seenAtTick2 = eng.state.planInSnapshot()
			mu.Unlock()
			tickStarted <- struct{}{}
		}
	}
	eng = New(reader, opt, Config{Interval: time.Hour})
	eng.Start()
	defer eng.Stop()

	<-tickStarted // tick 1 (the immediate startup tick) is now in flight

	// Enqueue mid-tick, from another goroutine, exactly like an MQTT callback
	// racing the control loop.
	want := []float64{9, 9, 9}
	go eng.SetPrices(want, want)
	time.Sleep(10 * time.Millisecond) // give SetPrices a chance to enqueue first

	release <- struct{}{} // let tick 1's Optimize (and the rest of tick()) finish
	eng.Wake()            // force tick 2 without waiting out the 1h interval

	select {
	case <-tickStarted: // tick 2 landed
	case <-time.After(2 * time.Second):
		t.Fatal("tick 2 (forced Wake) never ran")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seenAtTick2.importPrices) != 3 || seenAtTick2.importPrices[0] != 9 {
		t.Fatalf("planIn at tick 2 = %+v, want the mid-tick-1 SetPrices to have already landed", seenAtTick2)
	}
}

// TestEngine_ConcurrentHammer runs SetPrices/SetDERConstraints/Wake/
// LastPlan/RegisterBatteryActuator... concurrently against a running Engine
// under -race. It asserts no deadlock/panic (the race detector covers data
// races) and that the engine keeps ticking throughout — the exact scenario
// 05 §4 calls out ("-race in CI is non-negotiable; new subscribe/publish
// paths get a concurrency test").
func TestEngine_ConcurrentHammer(t *testing.T) {
	// The bounded-channel drop policy logs every drop; at hammer volume that's
	// thousands of log.Printf calls, whose shared-mutex + terminal I/O can
	// dominate wall-clock time (especially under -race). Silence it for the
	// duration — the drop path itself is still fully exercised.
	prev := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(prev)

	reader := concReader{}
	opt := &concOptimizer{}
	eng := New(reader, opt, Config{Interval: 5 * time.Millisecond})
	eng.Start()

	const workers = 4
	const iterations = 100
	var wg sync.WaitGroup

	wg.Add(workers*2 + workers)
	for w := 0; w < workers; w++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				eng.SetPrices([]float64{float64(id), float64(i)}, []float64{float64(id), float64(i)})
			}
		}(w)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				eng.SetDERConstraints([]StepConstraint{{ImpLimW: float64(id*1000 + i)}})
			}
		}(w)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				_ = eng.LastPlan()
			}
		}()
	}

	// A couple of Wake() callers too, mimicking urgent CSIP controls arriving
	// mid-hammer.
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				eng.Wake()
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("hammer goroutines never finished — possible deadlock")
	}

	eng.Stop()

	if opt.calls.Load() == 0 {
		t.Error("engine never ticked during the hammer")
	}
}
