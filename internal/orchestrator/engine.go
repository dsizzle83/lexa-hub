package orchestrator

import (
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"lexa-hub/internal/csip/discovery"
	"lexa-hub/internal/csip/scheduler"
)

// Engine is the central orchestrator.  It runs a continuous control loop that:
//  1. Reads the current SystemState via its reader
//  2. Passes it to the Optimizer
//  3. Executes the resulting Plan via the registered actuators
//
// The Engine is safe for concurrent use: SetCSIPPrograms may be called from
// any goroutine while the engine is running.
type Engine struct {
	reader    SystemReader
	optimizer Optimizer
	interval  time.Duration

	// Actuators — keyed by device name.  Protected by actuMu so Register* can
	// be called after Start (e.g. hot-plug EVSE).
	actuMu         sync.RWMutex
	battActuators  map[string]BatteryActuator
	solarActuators map[string]SolarActuator
	evseActuators  map[string]EVSEActuator

	// CSIP state — updated via SetCSIPPrograms, read in the control loop.
	csipMu      sync.RWMutex
	programs    []discovery.ProgramState
	clockOffset int64
	sched       *scheduler.Scheduler

	// Last plan — updated after every tick; readable from any goroutine.
	planMu   sync.RWMutex
	lastPlan Plan

	// Debug mode: print full decision trace on every tick.
	Debug bool

	stop        chan struct{}
	done        chan struct{}
	urgentWake  chan struct{} // poked by SetCSIPPrograms on OpModConnect=false
}

// Config groups optional Engine tunables.
type Config struct {
	// Interval is how often the optimization loop runs.  Default: 15s.
	Interval time.Duration
	// Debug enables step-by-step decision tracing.
	Debug bool
}

// New creates an Engine.  Call RegisterBatteryActuator, RegisterSolarActuator,
// and RegisterEVSEActuator to wire up devices, then Start.
func New(reader SystemReader, optimizer Optimizer, cfg Config) *Engine {
	if cfg.Interval <= 0 {
		cfg.Interval = 15 * time.Second
	}
	return &Engine{
		reader:         reader,
		optimizer:      optimizer,
		interval:       cfg.Interval,
		battActuators:  make(map[string]BatteryActuator),
		solarActuators: make(map[string]SolarActuator),
		evseActuators:  make(map[string]EVSEActuator),
		sched:          scheduler.New(),
		Debug:          cfg.Debug,
		stop:           make(chan struct{}),
		done:           make(chan struct{}),
		urgentWake:     make(chan struct{}, 1),
	}
}

// RegisterBatteryActuator wires an actuator for the battery device called name.
// Safe to call after Start.
func (e *Engine) RegisterBatteryActuator(name string, a BatteryActuator) {
	e.actuMu.Lock()
	e.battActuators[name] = a
	e.actuMu.Unlock()
}

// RegisterSolarActuator wires an actuator for the solar device called name.
// Safe to call after Start.
func (e *Engine) RegisterSolarActuator(name string, a SolarActuator) {
	e.actuMu.Lock()
	e.solarActuators[name] = a
	e.actuMu.Unlock()
}

// RegisterEVSEActuator wires an actuator for the EVSE station called id.
// Safe to call after Start.
func (e *Engine) RegisterEVSEActuator(stationID string, a EVSEActuator) {
	e.actuMu.Lock()
	e.evseActuators[stationID] = a
	e.actuMu.Unlock()
}

// SetCSIPPrograms updates the CSIP program list used by the control loop.
// Call this from the northbound discovery goroutine whenever programs change.
// Safe for concurrent use.
//
// If any program contains an OpModConnect=false control, the engine wakes
// immediately rather than waiting for the next ticker interval.
func (e *Engine) SetCSIPPrograms(programs []discovery.ProgramState, clockOffset int64) {
	e.csipMu.Lock()
	e.programs = programs
	e.clockOffset = clockOffset
	e.csipMu.Unlock()

	if hasDisconnectControl(programs) {
		select {
		case e.urgentWake <- struct{}{}:
		default: // already pending; don't block
		}
	}
}

// Start launches the control loop goroutine.  Pair with Stop.
func (e *Engine) Start() {
	go e.run()
}

// Stop signals the control loop to exit and waits for it to finish.
func (e *Engine) Stop() {
	close(e.stop)
	<-e.done
}

// run is the main control loop.
func (e *Engine) run() {
	defer close(e.done)

	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()

	// Evaluate immediately so devices get their first control without waiting.
	e.tick()

	for {
		select {
		case <-e.stop:
			return
		case <-e.urgentWake:
			e.tick()
			ticker.Reset(e.interval) // skip the tick that would fire after the forced one
		case <-ticker.C:
			e.tick()
		}
	}
}

// tick is one optimization cycle.
func (e *Engine) tick() {
	// 1. Read system state.
	state, err := e.reader.ReadSystemState()
	if err != nil {
		log.Printf("[orchestrator] read state error: %v", err)
		return
	}

	// 2. Inject CSIP active control.
	e.csipMu.RLock()
	programs := e.programs
	clockOffset := e.clockOffset
	e.csipMu.RUnlock()

	state.ClockOffset = clockOffset
	if len(programs) > 0 {
		serverNow := scheduler.ServerNow(clockOffset)
		active := e.sched.Evaluate(programs, serverNow)
		state.CSIPControl = FromActiveControl(active)
	}

	// 3. Optimize.
	plan := e.optimizer.Optimize(state)

	// 4. Execute plan.
	e.executePlan(plan)

	// 5. Store plan for external inspection (e.g. /status endpoint).
	e.planMu.Lock()
	e.lastPlan = plan
	e.planMu.Unlock()

	// 6. Log decisions.
	if e.Debug || len(plan.Decisions) > 0 {
		e.logPlan(state, plan)
	}
}

// LastPlan returns a snapshot of the most recently computed Plan.
// Safe for concurrent use from any goroutine.
func (e *Engine) LastPlan() Plan {
	e.planMu.RLock()
	defer e.planMu.RUnlock()
	return e.lastPlan
}

// executePlan fans out the plan's commands to the registered actuators.
func (e *Engine) executePlan(plan Plan) {
	// Snapshot actuator maps under read lock so hardware calls run lock-free.
	e.actuMu.RLock()
	batt := make(map[string]BatteryActuator, len(e.battActuators))
	for k, v := range e.battActuators {
		batt[k] = v
	}
	sol := make(map[string]SolarActuator, len(e.solarActuators))
	for k, v := range e.solarActuators {
		sol[k] = v
	}
	evse := make(map[string]EVSEActuator, len(e.evseActuators))
	for k, v := range e.evseActuators {
		evse[k] = v
	}
	e.actuMu.RUnlock()

	for _, cmd := range plan.BatteryCommands {
		a, ok := batt[cmd.Name]
		if !ok {
			log.Printf("[orchestrator] no battery actuator for %q", cmd.Name)
			continue
		}
		if err := a.ApplyBatteryCommand(cmd); err != nil {
			log.Printf("[orchestrator] battery %s command error: %v", cmd.Name, err)
		}
	}

	for _, cmd := range plan.SolarCommands {
		a, ok := sol[cmd.Name]
		if !ok {
			log.Printf("[orchestrator] no solar actuator for %q", cmd.Name)
			continue
		}
		if err := a.ApplySolarCommand(cmd); err != nil {
			log.Printf("[orchestrator] solar %s command error: %v", cmd.Name, err)
		}
	}

	for _, cmd := range plan.EVSECommands {
		a, ok := evse[cmd.StationID]
		if !ok {
			a, ok = evse["*"] // wildcard fallback for single-EVSE setups
		}
		if !ok {
			log.Printf("[orchestrator] no EVSE actuator for %q", cmd.StationID)
			continue
		}
		if err := a.ApplyEVSECommand(cmd); err != nil {
			log.Printf("[orchestrator] EVSE %s command error: %v", cmd.StationID, err)
		}
	}
}

// hasDisconnectControl returns true if any program in the list contains an
// OpModConnect=false control (event or default).  Used to decide whether to
// wake the engine loop immediately instead of waiting for the next ticker.
func hasDisconnectControl(programs []discovery.ProgramState) bool {
	for _, ps := range programs {
		if ps.DefaultControl != nil {
			if c := ps.DefaultControl.DERControlBase.OpModConnect; c != nil && !*c {
				return true
			}
		}
		if ps.Controls != nil {
			for _, ev := range ps.Controls.DERControl {
				if c := ev.DERControlBase.OpModConnect; c != nil && !*c {
					return true
				}
			}
		}
	}
	return false
}

// logPlan emits a structured summary of the plan to the standard logger.
func (e *Engine) logPlan(state SystemState, plan Plan) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[orchestrator] tick: solar=%.0fW battery=%.0fW evse=%.0fW",
		state.TotalSolarW(), state.TotalBatteryW(), state.TotalEVSEW())
	if !math.IsNaN(state.Grid.NetW) {
		fmt.Fprintf(&sb, " grid=%.0fW", state.Grid.NetW)
	}
	if load := state.InferredLoadW(); !math.IsNaN(load) {
		fmt.Fprintf(&sb, " load=%.0fW", load)
	}
	if state.CSIPControl != nil {
		fmt.Fprintf(&sb, " csip=%s(%s)", state.CSIPControl.Source, state.CSIPControl.MRID)
	}

	if len(plan.Decisions) == 0 {
		sb.WriteString(" → no action")
	} else {
		for _, d := range plan.Decisions {
			fmt.Fprintf(&sb, "\n  [%s] %s → %s", d.Rule, d.Reason, d.Impact)
		}
	}
	log.Print(sb.String())
}
