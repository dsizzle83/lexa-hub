package orchestrator_test

import (
	"sync/atomic"
	"testing"
	"time"

	"lexa-hub/internal/northbound/discovery"
	"lexa-hub/internal/northbound/model"
	"lexa-hub/internal/orchestrator"
)

// ── Mocks ─────────────────────────────────────────────────────────────────────

type mockReader struct {
	state orchestrator.SystemState
	err   error
	calls atomic.Int32
}

func (m *mockReader) ReadSystemState() (orchestrator.SystemState, error) {
	m.calls.Add(1)
	return m.state, m.err
}

type mockOptimizer struct {
	plan  orchestrator.Plan
	calls atomic.Int32
}

func (m *mockOptimizer) Optimize(_ orchestrator.SystemState) orchestrator.Plan {
	m.calls.Add(1)
	return m.plan
}

type mockBatteryActuator struct {
	commands []orchestrator.BatteryCommand
	err      error
	calls    atomic.Int32
}

func (m *mockBatteryActuator) ApplyBatteryCommand(cmd orchestrator.BatteryCommand) error {
	m.commands = append(m.commands, cmd)
	m.calls.Add(1)
	return m.err
}

type mockSolarActuator struct {
	commands []orchestrator.SolarCommand
	calls    atomic.Int32
}

func (m *mockSolarActuator) ApplySolarCommand(cmd orchestrator.SolarCommand) error {
	m.commands = append(m.commands, cmd)
	m.calls.Add(1)
	return nil
}

type mockEVSEActuator struct {
	commands []orchestrator.EVSECommand
	calls    atomic.Int32
}

func (m *mockEVSEActuator) ApplyEVSECommand(cmd orchestrator.EVSECommand) error {
	m.commands = append(m.commands, cmd)
	m.calls.Add(1)
	return nil
}

// safetyMockOptimizer implements both Optimizer and SafetyEvaluator so the engine
// wires the fast protection loop.
type safetyMockOptimizer struct {
	optimizeCalls atomic.Int32
	safetyCalls   atomic.Int32
}

func (m *safetyMockOptimizer) Optimize(_ orchestrator.SystemState) orchestrator.Plan {
	m.optimizeCalls.Add(1)
	return orchestrator.Plan{}
}

func (m *safetyMockOptimizer) EvaluateSafety(_ orchestrator.SystemState) orchestrator.Plan {
	m.safetyCalls.Add(1)
	return orchestrator.Plan{}
}

// safetyMockReader implements both SystemReader and SafetyReader.
type safetyMockReader struct {
	full   atomic.Int32
	safety atomic.Int32
}

func (m *safetyMockReader) ReadSystemState() (orchestrator.SystemState, error) {
	m.full.Add(1)
	return orchestrator.SystemState{Timestamp: time.Now()}, nil
}

func (m *safetyMockReader) ReadSafetyState() (orchestrator.SystemState, error) {
	m.safety.Add(1)
	return orchestrator.SystemState{Timestamp: time.Now()}, nil
}

// TestEngine_FastProtectionLoop verifies that when the optimizer implements
// SafetyEvaluator and the reader implements SafetyReader, the engine runs the fast
// safety pass more often than the economic pass (ADR-0001 two-loop hierarchy).
func TestEngine_FastProtectionLoop(t *testing.T) {
	opt := &safetyMockOptimizer{}
	reader := &safetyMockReader{}
	eng := orchestrator.New(reader, opt, orchestrator.Config{
		Interval:       100 * time.Millisecond,
		SafetyInterval: 20 * time.Millisecond, // econEvery = 5
	})
	eng.Start()
	time.Sleep(260 * time.Millisecond)
	eng.Stop()

	safety := opt.safetyCalls.Load()
	econ := opt.optimizeCalls.Load()
	if safety <= econ {
		t.Errorf("expected more fast safety passes than economic; safety=%d economic=%d", safety, econ)
	}
	if econ < 2 {
		t.Errorf("expected the economic pass to keep running; economic=%d", econ)
	}
	if reader.safety.Load() == 0 {
		t.Error("fast loop never called ReadSafetyState")
	}
}

// TestEngine_NoFastLoopWithoutInterfaces verifies the engine degrades to a single
// economic ticker when the optimizer/reader do not implement the safety
// interfaces (the mockOptimizer/mockReader case).
func TestEngine_NoFastLoopWithoutInterfaces(t *testing.T) {
	reader := &mockReader{state: state0()}
	opt := &mockOptimizer{}
	eng := orchestrator.New(reader, opt, orchestrator.Config{
		Interval:       30 * time.Millisecond,
		SafetyInterval: 5 * time.Millisecond, // ignored: mocks lack the interfaces
	})
	eng.Start()
	time.Sleep(120 * time.Millisecond)
	eng.Stop()
	// With no fast loop, every tick is an economic tick: calls track the 30ms
	// cadence (~4), nowhere near the 5ms fast cadence (~24).
	if c := opt.calls.Load(); c > 10 {
		t.Errorf("economic optimizer ran %d times — fast loop should be disabled without the interfaces", c)
	}
}

// ── Lifecycle ─────────────────────────────────────────────────────────────────

func TestEngine_StartStop(t *testing.T) {
	reader := &mockReader{state: state0()}
	opt := &mockOptimizer{}
	eng := orchestrator.New(reader, opt, orchestrator.Config{Interval: time.Second})
	eng.Start()

	done := make(chan struct{})
	go func() { eng.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop blocked")
	}
}

// TestEngine_CallsOptimizerOnTick verifies the engine calls the optimizer at
// least once immediately on Start and again on each tick.
func TestEngine_CallsOptimizerOnTick(t *testing.T) {
	reader := &mockReader{state: state0()}
	opt := &mockOptimizer{}
	const interval = 20 * time.Millisecond
	eng := orchestrator.New(reader, opt, orchestrator.Config{Interval: interval})
	eng.Start()
	time.Sleep(4 * interval)
	eng.Stop()

	if opt.calls.Load() < 3 {
		t.Errorf("optimizer called %d times; expected ≥ 3 over 4 intervals",
			opt.calls.Load())
	}
}

// TestEngine_ExecutesBatteryCommand checks that commands in the plan reach
// the registered actuator.
func TestEngine_ExecutesBatteryCommand(t *testing.T) {
	reader := &mockReader{state: state0()}
	sp := -3000.0
	opt := &mockOptimizer{plan: orchestrator.Plan{
		BatteryCommands: []orchestrator.BatteryCommand{
			{Name: "bat-0", SetpointW: sp},
		},
	}}
	bat := &mockBatteryActuator{}

	eng := orchestrator.New(reader, opt, orchestrator.Config{Interval: 10 * time.Second})
	eng.RegisterBatteryActuator("bat-0", bat)
	eng.Start()
	time.Sleep(30 * time.Millisecond) // one immediate tick
	eng.Stop()

	if bat.calls.Load() == 0 {
		t.Fatal("battery actuator was not called")
	}
	if bat.commands[0].SetpointW != sp {
		t.Errorf("setpoint = %.0f, want %.0f", bat.commands[0].SetpointW, sp)
	}
}

func TestEngine_ExecutesSolarCommand(t *testing.T) {
	reader := &mockReader{state: state0()}
	opt := &mockOptimizer{plan: orchestrator.Plan{
		SolarCommands: []orchestrator.SolarCommand{
			{Name: "pv-0", CurtailToW: 3000},
		},
	}}
	sol := &mockSolarActuator{}

	eng := orchestrator.New(reader, opt, orchestrator.Config{Interval: 10 * time.Second})
	eng.RegisterSolarActuator("pv-0", sol)
	eng.Start()
	time.Sleep(30 * time.Millisecond)
	eng.Stop()

	if sol.calls.Load() == 0 {
		t.Fatal("solar actuator was not called")
	}
}

func TestEngine_ExecutesEVSECommand(t *testing.T) {
	reader := &mockReader{state: state0()}
	opt := &mockOptimizer{plan: orchestrator.Plan{
		EVSECommands: []orchestrator.EVSECommand{
			{StationID: "cs-001", ConnectorID: 1, MaxCurrentA: 16},
		},
	}}
	evseAct := &mockEVSEActuator{}

	eng := orchestrator.New(reader, opt, orchestrator.Config{Interval: 10 * time.Second})
	eng.RegisterEVSEActuator("cs-001", evseAct)
	eng.Start()
	time.Sleep(30 * time.Millisecond)
	eng.Stop()

	if evseAct.calls.Load() == 0 {
		t.Fatal("EVSE actuator was not called")
	}
	if evseAct.commands[0].MaxCurrentA != 16 {
		t.Errorf("MaxCurrentA = %.1f, want 16", evseAct.commands[0].MaxCurrentA)
	}
}

// TestEngine_MissingActuator_DoesNotPanic ensures the engine logs and continues
// when a plan references a device with no registered actuator.
func TestEngine_MissingActuator_DoesNotPanic(t *testing.T) {
	reader := &mockReader{state: state0()}
	opt := &mockOptimizer{plan: orchestrator.Plan{
		BatteryCommands: []orchestrator.BatteryCommand{
			{Name: "nonexistent", SetpointW: -1000},
		},
	}}

	eng := orchestrator.New(reader, opt, orchestrator.Config{Interval: 10 * time.Second})
	// No actuator registered for "nonexistent".
	eng.Start()
	time.Sleep(30 * time.Millisecond)
	eng.Stop() // should not panic
}

// TestEngine_SetCSIPPrograms_InjectedIntoState checks that CSIP programs are
// evaluated and the result is available in the state passed to the optimizer.
func TestEngine_SetCSIPPrograms_InjectedIntoState(t *testing.T) {
	var capturedState orchestrator.SystemState
	reader := &mockReader{state: state0()}
	opt := &captureStateOptimizer{}

	eng := orchestrator.New(reader, opt, orchestrator.Config{Interval: 10 * time.Second})

	now := time.Now().Unix()
	tr := true
	programs := []discovery.ProgramState{
		{
			Program: model.DERProgram{MRID: "prog-1", Primacy: 1},
			DefaultControl: &model.DefaultDERControl{
				MRID:           "ddc-1",
				DERControlBase: model.DERControlBase{OpModConnect: &tr},
			},
			Controls: &model.DERControlList{
				DERControl: []model.DERControl{
					{
						MRID:         "ctrl-1",
						CreationTime: now - 100,
						Interval: model.DateTimeInterval{
							Start:    now - 60,
							Duration: 3600,
						},
						DERControlBase: model.DERControlBase{OpModConnect: &tr},
					},
				},
			},
		},
	}

	eng.SetCSIPPrograms(programs, 0)
	eng.Start()
	time.Sleep(30 * time.Millisecond)
	eng.Stop()

	capturedState = opt.lastState
	if capturedState.CSIPControl == nil {
		t.Fatal("expected CSIPControl to be set from programs")
	}
	if capturedState.CSIPControl.Source != "event" && capturedState.CSIPControl.Source != "default" {
		t.Errorf("unexpected source: %q", capturedState.CSIPControl.Source)
	}
}

// ── Concurrent safety ─────────────────────────────────────────────────────────

func TestEngine_SetCSIPPrograms_ConcurrentSafe(t *testing.T) {
	reader := &mockReader{state: state0()}
	opt := &mockOptimizer{}
	eng := orchestrator.New(reader, opt, orchestrator.Config{Interval: 5 * time.Millisecond})
	eng.Start()

	// Hammer SetCSIPPrograms from multiple goroutines while the engine ticks.
	done := make(chan struct{})
	for i := 0; i < 5; i++ {
		go func() {
			for j := 0; j < 20; j++ {
				eng.SetCSIPPrograms(nil, int64(j))
				time.Sleep(time.Millisecond)
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 5; i++ {
		<-done
	}
	eng.Stop()
}

// TestEngine_PreservesReaderClockOffset is a regression test for the bug where
// tick() overwrote the reader-supplied CSIP clock offset with the engine's
// unused zero. Only the SetCSIPPrograms path owns the offset; a bus-driven
// deployment (no programs) carries the real offset in the state it reads, and
// the optimizer must see it so serverNow = Timestamp+ClockOffset follows utility
// (or replayed) time instead of collapsing to local time and killing TOU
// peak-shaving.
func TestEngine_PreservesReaderClockOffset(t *testing.T) {
	st := state0()
	st.Timestamp = time.Now()
	const offset = int64(7 * 3600) // e.g. a replay clock warp
	st.ClockOffset = offset
	reader := &mockReader{state: st}
	opt := &captureStateOptimizer{}

	eng := orchestrator.New(reader, opt, orchestrator.Config{Interval: 10 * time.Second})
	// No SetCSIPPrograms call: programs are empty, so the reader's offset must
	// survive into the optimizer untouched.
	eng.Start()
	time.Sleep(30 * time.Millisecond)
	eng.Stop()

	if got := opt.lastState.ClockOffset; got != offset {
		t.Fatalf("optimizer saw ClockOffset=%d, want %d — reader offset was clobbered", got, offset)
	}
}

// ── captureStateOptimizer ─────────────────────────────────────────────────────

type captureStateOptimizer struct {
	lastState orchestrator.SystemState
}

func (c *captureStateOptimizer) Optimize(state orchestrator.SystemState) orchestrator.Plan {
	c.lastState = state
	return orchestrator.Plan{}
}
