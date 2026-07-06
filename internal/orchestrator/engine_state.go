package orchestrator

import (
	"fmt"
	"log"
	"sync/atomic"
)

// engineCmd mutates engineState. It is applied exactly once, on the control
// goroutine (Engine.run), in the order it was enqueued — the single-writer
// discipline behind every field below (05 §4: "One writer per state struct;
// snapshot reads. Engine mutex consolidation, TASK-067, is the model.").
type engineCmd func(*engineState)

// engineState is the Engine's mutable state. It used to be five separate
// mutex-guarded groups (actuMu, planInMu, dailyPlanMu, planMu — csipMu was
// deleted whole by TASK-012, its domain already empty). Every field below
// names exactly one writer goroutine in its doc comment; everyone else reads
// a lock-free atomic snapshot. There is no closure-held control-path state
// left to promote here — cmd/hub's former activeBreachMRID/dedupeResets
// closures were already replaced by the named breachEpisodes component in
// cmd/hub/breach.go (TASK-031/032); this struct is the engine-side sibling.
type engineState struct {
	// Actuators, keyed by device name. Written ONLY by Register*Actuator,
	// which must complete before Start() — Engine.started enforces this
	// with a panic (an init-time impossibility per 05 §3). That makes the
	// maps immutable for the rest of the Engine's life, so executePlan (and
	// any other reader) can read them directly with no lock and no
	// snapshot-copy.
	battActuators  map[string]BatteryActuator
	solarActuators map[string]SolarActuator
	evseActuators  map[string]EVSEActuator

	// planIn holds the planner inputs last set via SetDERConstraints /
	// SetPrices. Single writer: the control goroutine (run(), applying
	// commands enqueued through Engine.cmdCh via drainCmds). Reader:
	// plannerLoop's replan(), via planInSnapshot()'s atomic load — no lock.
	planIn atomic.Pointer[plannerInput]

	// dailyPlan is the most recent planner output. Single writer: the
	// planner goroutine (replan()). Reader: the control goroutine (tick()),
	// via an atomic load — no lock.
	dailyPlan atomic.Pointer[DailyPlan]

	// lastPlan is the most recently computed economic Plan, exposed via
	// Engine.LastPlan(). Single writer: the control goroutine (tick()).
	// Read by any external caller (e.g. cmd/hub's /status handler) via an
	// atomic load — no lock.
	lastPlan atomic.Pointer[Plan]

	// lastSolarPeakKw is a running high-water estimate of the clear-sky PV
	// peak, used to seed the diurnal solar forecast after dark. Single
	// writer AND sole reader: the planner goroutine (buildPlannerParams) —
	// unchanged from before, just given a documented home alongside the
	// rest of the planner-owned state instead of living loose on Engine.
	lastSolarPeakKw float64
}

// newEngineState builds an engineState with its actuator registries ready
// for Register*Actuator calls (which must all happen before Start).
func newEngineState() *engineState {
	return &engineState{
		battActuators:  make(map[string]BatteryActuator),
		solarActuators: make(map[string]SolarActuator),
		evseActuators:  make(map[string]EVSEActuator),
	}
}

// planInSnapshot returns the current planner-input snapshot, defaulting to
// the zero value before the first SetDERConstraints/SetPrices call — the
// same default the old planInMu-guarded zero-value field gave.
func (s *engineState) planInSnapshot() plannerInput {
	if p := s.planIn.Load(); p != nil {
		return *p
	}
	return plannerInput{}
}

// setPlanIn applies fn to a copy of the current planner-input snapshot and
// publishes the result atomically. Must only be called from the control
// goroutine (i.e. from inside an engineCmd applied by drainCmds) — that's
// what makes this read-modify-write race-free even though
// SetDERConstraints/SetPrices themselves are callable concurrently from any
// goroutine (they just enqueue; they never call this directly).
func (s *engineState) setPlanIn(fn func(*plannerInput)) {
	next := s.planInSnapshot()
	fn(&next)
	s.planIn.Store(&next)
}

// ── Engine methods that mutate/gate engineState ────────────────────────────
//
// These live here, next to engineState, rather than in engine.go's control
// flow (run/tick/safetyTick/replan).

// enqueue hands cmd to the control goroutine to apply against e.state.
// Non-blocking: a full cmdCh means the control goroutine is not keeping up
// (shouldn't happen at any real tick cadence — the channel is drained
// immediately before every tick), so the command is dropped and counted
// rather than blocking the caller. A blocking enqueue from an MQTT callback
// is exactly the C08 wedge shape this task exists to close off.
func (e *Engine) enqueue(cmd engineCmd) {
	select {
	case e.cmdCh <- cmd:
	default:
		n := e.cmdDropped.Add(1)
		log.Printf("[orchestrator] command channel full; dropping mutation (dropped=%d total)", n)
	}
}

// drainCmds applies every currently queued command, in order, without
// blocking. Called only from the control goroutine (Start, before either
// goroutine launches; and run, immediately before every tick/safetyTick/
// forced Wake tick) so a mutator's effect is visible no later than the next
// tick.
func (e *Engine) drainCmds() {
	for {
		select {
		case cmd := <-e.cmdCh:
			cmd(e.state)
		default:
			return
		}
	}
}

// RegisterBatteryActuator wires an actuator for the battery device called
// name. Must be called before Start — see mustNotBeStarted's doc.
func (e *Engine) RegisterBatteryActuator(name string, a BatteryActuator) {
	e.mustNotBeStarted("RegisterBatteryActuator")
	e.state.battActuators[name] = a
}

// RegisterSolarActuator wires an actuator for the solar device called name.
// Must be called before Start — see mustNotBeStarted's doc.
func (e *Engine) RegisterSolarActuator(name string, a SolarActuator) {
	e.mustNotBeStarted("RegisterSolarActuator")
	e.state.solarActuators[name] = a
}

// RegisterEVSEActuator wires an actuator for the EVSE station called id.
// Must be called before Start — see mustNotBeStarted's doc.
func (e *Engine) RegisterEVSEActuator(stationID string, a EVSEActuator) {
	e.mustNotBeStarted("RegisterEVSEActuator")
	e.state.evseActuators[stationID] = a
}

// mustNotBeStarted panics if the Engine has already been Start()ed. Panicking
// here is an init-time impossibility (05 §3): every production caller
// (cmd/hub/main.go) registers all actuators before calling Start, and making
// the registries immutable afterwards is what lets executePlan read them with
// no lock. A previous version of this Engine allowed post-Start
// (hot-plug-style) registration under actuMu; no caller used it, and it was
// the one thing standing in the way of dropping actuMu entirely — see
// TASK-067.
func (e *Engine) mustNotBeStarted(who string) {
	if e.started.Load() {
		panic(fmt.Sprintf("orchestrator: %s called after Start; actuators must be registered before Start", who))
	}
}
