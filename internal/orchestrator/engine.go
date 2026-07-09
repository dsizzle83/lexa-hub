package orchestrator

import (
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// plannerInput holds the external inputs consumed by the daily planner.
// Published via Engine.state.planIn (see engine_state.go) — set by
// SetDERConstraints/SetPrices, read by replan().
type plannerInput struct {
	derConstraints []StepConstraint
	importPrices   []float64 // per planSteps step; nil = use FallbackTOU
	exportPrices   []float64
}

// Engine is the central orchestrator.  It runs a continuous control loop that:
//  1. Reads the current SystemState via its reader
//  2. Passes it to the Optimizer
//  3. Executes the resulting Plan via the registered actuators
//
// The Engine is safe for concurrent use: SetDERConstraints and SetPrices may be
// called from any goroutine while the engine is running.
//
// State model (05 §4, TASK-067): all mutable Engine state lives in one
// engineState struct (engine_state.go) with a single designated writer per
// field and lock-free atomic snapshot reads — see that file's doc comments
// for the writer/reader map. Mutators that need to touch engineState from an
// arbitrary caller goroutine (SetDERConstraints, SetPrices) enqueue a command
// on cmdCh instead of locking; the control goroutine (run) is the only thing
// that ever applies a command, which is what makes the read-modify-write in
// engineState.setPlanIn race-free. A command takes effect no later than the
// next tick (drainCmds runs immediately before every tick/safetyTick/forced
// Wake tick) — the same "at most one tick" latency the old RLock-at-tick-time
// scheme gave.
type Engine struct {
	reader    SystemReader
	optimizer Optimizer
	interval  time.Duration

	// Fast protection loop (ADR-0001, Tier 1). Non-nil only when the optimizer and
	// reader implement the optional SafetyEvaluator / SafetyReader interfaces.
	safety         SafetyEvaluator
	safetyReader   SafetyReader
	safetyInterval time.Duration

	// state holds every mutable field the control/planner goroutines and
	// external callers share. See engine_state.go.
	state *engineState

	// started guards the actuator registries: Register*Actuator panics once
	// this is true (registration is a before-Start-only, init-time
	// operation — 05 §3). Set once, in Start(), before either goroutine
	// launches.
	started atomic.Bool

	// cmdCh carries mutations to be applied to state from the control
	// goroutine (drainCmds). Buffered so SetDERConstraints/SetPrices never
	// block their caller; a full channel drops the command and counts it
	// (05 §4 bounded-channel policy) rather than stalling an MQTT callback.
	cmdCh      chan engineCmd
	cmdDropped atomic.Uint64

	// Daily planner — produces 24-hour cost-optimal dispatch plans.
	planner    *DailyPlanner
	plannerCfg PlannerCfg

	plannerWake chan struct{} // buffered(1): signals the planner goroutine

	// planObserver, when non-nil, is called with every Plan after it is computed.
	planObserver func(Plan)

	// Debug mode: print full decision trace on every tick.
	Debug bool

	stop        chan struct{}
	stopOnce    sync.Once // makes Stop idempotent
	done        chan struct{}
	plannerDone chan struct{} // closed when plannerLoop exits
	urgentWake  chan struct{} // poked by Wake on urgent controls (OpModConnect=false)
}

// Config groups optional Engine tunables.
type Config struct {
	// Interval is how often the optimization loop runs.  Default: 15s.
	Interval time.Duration
	// SafetyInterval is the cadence of the fast protection loop (ADR-0001). When
	// > 0 and < Interval, AND the optimizer implements SafetyEvaluator and the
	// reader implements SafetyReader, the Engine runs EvaluateSafety on this
	// cadence between economic ticks. 0 disables it (safety runs only on the
	// economic tick, inside Optimize).
	SafetyInterval time.Duration
	// Debug enables step-by-step decision tracing.
	Debug bool
	// Planner holds the daily-planner asset and scheduling configuration.
	Planner PlannerCfg
	// PlanObserver, when set, is called with every Plan right after it is
	// computed (before actuation). Used by cmd/hub to forward compliance
	// breaches to the northbound service. Must not block.
	PlanObserver func(Plan)
}

// New creates an Engine.  Call RegisterBatteryActuator, RegisterSolarActuator,
// and RegisterEVSEActuator to wire up devices, then Start.
func New(reader SystemReader, optimizer Optimizer, cfg Config) *Engine {
	if cfg.Interval <= 0 {
		cfg.Interval = 15 * time.Second
	}
	e := &Engine{
		reader:         reader,
		optimizer:      optimizer,
		interval:       cfg.Interval,
		safetyInterval: cfg.SafetyInterval,
		state:          newEngineState(),
		cmdCh:          make(chan engineCmd, 16),
		planner:        NewDailyPlanner(),
		plannerCfg:     cfg.Planner,
		plannerWake:    make(chan struct{}, 1),
		planObserver:   cfg.PlanObserver,
		Debug:          cfg.Debug,
		stop:           make(chan struct{}),
		done:           make(chan struct{}),
		plannerDone:    make(chan struct{}),
		urgentWake:     make(chan struct{}, 1),
	}
	// Wire the fast protection loop only if BOTH the optimizer and reader support
	// it; otherwise safety runs on the economic tick (inside Optimize) as before.
	if se, ok := optimizer.(SafetyEvaluator); ok {
		e.safety = se
	}
	if sr, ok := reader.(SafetyReader); ok {
		e.safetyReader = sr
	}
	return e
}

// SetDERConstraints stores per-step DER operating constraints derived from the
// northbound 24-hour schedule.  Triggers an immediate re-plan.
// Safe for concurrent use.
func (e *Engine) SetDERConstraints(constraints []StepConstraint) {
	e.enqueue(func(s *engineState) {
		s.setPlanIn(func(in *plannerInput) { in.derConstraints = constraints })
	})
	e.signalReplan()
}

// SetPrices stores per-step electricity import and export prices ($/kWh).
// Both slices must have len == planSteps.  Triggers an immediate re-plan.
// Safe for concurrent use.
func (e *Engine) SetPrices(importPrices, exportPrices []float64) {
	e.enqueue(func(s *engineState) {
		s.setPlanIn(func(in *plannerInput) {
			in.importPrices = importPrices
			in.exportPrices = exportPrices
		})
	})
	e.signalReplan()
}

func (e *Engine) signalReplan() {
	select {
	case e.plannerWake <- struct{}{}:
	default:
	}
}

// enqueue, drainCmds, mustNotBeStarted, and Register*Actuator — the Engine
// methods that mutate or gate engineState — live in engine_state.go
// alongside the struct they operate on.

// Wake forces an immediate optimization tick instead of waiting for the next
// ticker interval.  Non-blocking and safe to call from any goroutine (e.g. an
// MQTT handler that just received an urgent CSIP control such as
// OpModConnect=false).
func (e *Engine) Wake() {
	select {
	case e.urgentWake <- struct{}{}:
	default: // wake already pending; don't block
	}
}

// Start launches the control loop goroutine and the daily planner goroutine.
// Pair with Stop.
func (e *Engine) Start() {
	e.started.Store(true)
	// Apply any commands enqueued before Start (e.g. a retained MQTT message
	// resolving SetPrices/SetDERConstraints between subscribe and Start) here,
	// on the calling goroutine, while nothing else can be draining cmdCh —
	// once run() is running it's the only drainer, by construction.
	e.drainCmds()
	go e.run()
	go e.plannerLoop()
}

// Stop signals both goroutines to exit and waits for them to finish.
// Safe to call more than once.
func (e *Engine) Stop() {
	e.stopOnce.Do(func() { close(e.stop) })
	<-e.done
	<-e.plannerDone
}

// run is the main control loop (ADR-0001 two-loop hierarchy).
//
// When a fast protection loop is wired (safety + safetyReader present and
// safetyInterval in (0, interval)), the loop ticks at safetyInterval: every tick
// runs the fast safety pass, and every econEvery-th tick ALSO runs the full
// economic pass. Both run on this one goroutine, so the optimizer needs no locking.
// Otherwise it degenerates to the original single economic ticker.
//
// This goroutine is also engineState's single writer: every mutation —
// registered actuators (fixed before Start), planner-input commands drained
// from cmdCh, and lastPlan — is written from here or from the one other
// long-lived goroutine (plannerLoop, for dailyPlan). See engine_state.go.
func (e *Engine) run() {
	defer close(e.done)

	base := e.interval
	econEvery := 1
	fastLoop := e.safety != nil && e.safetyReader != nil &&
		e.safetyInterval > 0 && e.safetyInterval < e.interval
	if fastLoop {
		base = e.safetyInterval
		econEvery = int(math.Round(float64(e.interval) / float64(e.safetyInterval)))
		if econEvery < 1 {
			econEvery = 1
		}
	}

	ticker := time.NewTicker(base)
	defer ticker.Stop()

	// Evaluate immediately so devices get their first control without waiting.
	e.drainCmds()
	e.tick()

	n := 0
	for {
		select {
		case <-e.stop:
			return
		case <-e.urgentWake:
			e.drainCmds()
			e.tick()
			ticker.Reset(base) // skip the tick that would fire after the forced one
			n = 0
		case <-ticker.C:
			e.drainCmds()
			n++
			if !fastLoop || n >= econEvery {
				n = 0
				e.tick()
			} else {
				e.safetyTick()
			}
		}
	}
}

// safetyTick is the fast protection pass: read the cheap safety snapshot, evaluate
// the immediate protective reflexes, and actuate only their commands. It never
// touches the economic plan, planner, or CSIP scheduler. Runs between economic
// ticks on the same goroutine as tick(). It never waits on cmdCh — the drain
// happens in run() before this is called — so the fast loop's cadence is never
// at the mercy of a mutator.
func (e *Engine) safetyTick() {
	state, err := e.safetyReader.ReadSafetyState()
	if err != nil {
		log.Printf("[orchestrator] safety: read state error: %v", err)
		return
	}
	plan := e.safety.EvaluateSafety(state)
	if len(plan.BatteryCommands) == 0 && len(plan.SolarCommands) == 0 && len(plan.EVSECommands) == 0 {
		return // nothing to protect against this tick
	}
	// Notify the observer: a fast-loop protective action (e.g. a wrong-sign
	// disconnect) must reach the bus plan log like any economic decision, or
	// the most safety-critical actions the hub takes are exactly the ones
	// /status never shows. Safety plans carry no Breach, so the observer's
	// breach-alert edge logic is untouched.
	if e.planObserver != nil {
		e.planObserver(plan)
	}
	e.executePlan(plan)
	if len(plan.Decisions) > 0 {
		e.logPlan(state, plan)
	}
}

// plannerLoop re-runs the daily planner whenever its inputs change or the
// replan cadence fires.  It runs as a separate goroutine so the DP does not
// block the 15-second control-loop tick. It is dailyPlan's single writer
// (engine_state.go) and planIn's only reader.
func (e *Engine) plannerLoop() {
	defer close(e.plannerDone)
	interval := time.Duration(e.plannerCfg.ReplanIntervalS) * time.Second
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Plan immediately on startup.
	e.replan()

	for {
		select {
		case <-e.stop:
			return
		case <-e.plannerWake:
			e.replan()
			ticker.Reset(interval)
		case <-ticker.C:
			e.replan()
		}
	}
}

// replan builds PlannerParams from current state and runs the DP.
func (e *Engine) replan() {
	state, err := e.reader.ReadSystemState()
	if err != nil {
		log.Printf("[orchestrator] planner: read state error: %v", err)
		return
	}

	inp := e.state.planInSnapshot()

	params := e.buildPlannerParams(state, inp)
	plan := e.planner.Plan(params)

	e.state.dailyPlan.Store(plan)

	log.Printf("[orchestrator] replan: window=%s–%s cost=%.3f slots=%d",
		time.Unix(plan.WindowStart, 0).Format("15:04"),
		time.Unix(plan.WindowEnd, 0).Format("15:04"),
		plan.TotalCost,
		planSteps)
}

// buildPlannerParams derives PlannerParams from SystemState and planner inputs.
func (e *Engine) buildPlannerParams(state SystemState, inp plannerInput) PlannerParams {
	cfg := e.plannerCfg
	now := state.Timestamp
	if now.IsZero() {
		now = time.Now()
	}
	// Plan in CSIP server time. The optimizer evaluates TOU at
	// Timestamp+ClockOffset and tick() queries CurrentTarget there too, so the
	// plan window must use the same clock or the planned intervals never line up
	// with the live tick (notably under a replay that warps the utility clock).
	now = now.Add(time.Duration(state.ClockOffset) * time.Second)

	p := PlannerParams{
		Now:               now,
		WindowStart:       now.Unix(),
		LoadForecastKw:    math.Max(0, state.InferredLoadW()-state.TotalEVSEW()) / 1000,
		DERConstraints:    inp.derConstraints,
		ImportPricePerKwh: inp.importPrices,
		ExportPricePerKwh: inp.exportPrices,
		SOCStepKwh:        cfg.SOCStepKwh,
	}

	// Solar forecast: a clear-sky diurnal bell curve scaled to a peak estimate,
	// rather than a flat hold of the current reading (which assumed constant sun
	// all day and zero sun for the whole horizon on any post-sunset replan).
	// Back out the clear-sky peak from the live generation and the time of day,
	// and keep a high-water mark so a replan after dark still forecasts tomorrow.
	if curKw := state.TotalSolarW() / 1000; curKw > 0 {
		if shape := clearSkyShape(localHourOf(now.Unix())); shape > 0.15 {
			if est := curKw / shape; est > e.state.lastSolarPeakKw {
				e.state.lastSolarPeakKw = est
			}
		}
	}
	peakKw := e.state.lastSolarPeakKw
	if cfg.SolarPeakKw > peakKw {
		peakKw = cfg.SolarPeakKw
	}
	p.SolarForecastKw = diurnalSolarForecast(p.WindowStart, peakKw)

	// Fallback TOU if no live pricing.
	if p.ImportPricePerKwh == nil {
		p.FallbackTOU = DefaultTOUCostModel()
	}

	// Battery: prefer live MQTT metrics, fall back to config.
	for _, b := range state.Batteries {
		if !b.Connected {
			continue
		}
		capKwh := cfg.BattCapacityKwh
		if !math.IsNaN(b.CapacityWh) && b.CapacityWh > 0 {
			capKwh = b.CapacityWh / 1000
		}
		maxCKw := cfg.BattMaxChargeKw
		if maxCKw == 0 {
			maxCKw = b.MaxChargeW / 1000
		}
		maxDKw := cfg.BattMaxDischargeKw
		if maxDKw == 0 {
			maxDKw = b.MaxDischargeW / 1000
		}
		initSocKwh := 0.0
		if !math.IsNaN(b.SOC) && capKwh > 0 {
			initSocKwh = b.SOC / 100 * capKwh
		}
		p.BattCapacityKwh = capKwh
		p.BattMaxChargeKw = maxCKw
		p.BattMaxDischargeKw = maxDKw
		p.BattEfficiency = cfg.BattEfficiency
		p.InitialBattSocKwh = initSocKwh
		// Allow net daily discharge down to a reserve floor (instead of pinning
		// the terminal SOC at the starting SOC), so the battery can shave the
		// evening peak and overnight import rather than hoarding charge.
		reservePct := cfg.TerminalReservePct
		if reservePct <= 0 {
			reservePct = 20
		}
		p.TerminalSocKwh = reservePct / 100 * capKwh
		break // use first connected battery
	}

	// EV: config capacity + live SOC from EVSE state.
	if cfg.EVCapacityKwh > 0 {
		p.EVCapacityKwh = cfg.EVCapacityKwh
		p.EVMaxChargeKw = cfg.EVMaxChargeKw
		p.EVEfficiency = cfg.EVEfficiency
		p.EVVoltageV = cfg.EVVoltageV
		if p.EVVoltageV == 0 {
			p.EVVoltageV = 240
		}
		// Pick live EV SOC if an active session is present.
		for _, ev := range state.EVSEs {
			if ev.SessionActive && !math.IsNaN(ev.SOC) {
				p.InitialEVSocKwh = ev.SOC / 100 * cfg.EVCapacityKwh
				if cfg.EVMaxChargeKw == 0 && ev.MaxCurrentA > 0 {
					p.EVMaxChargeKw = ev.MaxCurrentA * p.EVVoltageV / 1000
				}
			}
		}
		// Target SOC and departure time.
		if cfg.EVTargetSocPct > 0 {
			p.EVTargetSocKwh = cfg.EVTargetSocPct / 100 * cfg.EVCapacityKwh
			if cfg.EVDepartureHH > 0 || cfg.EVDepartureMM > 0 {
				p.EVDepartureUnix = nextDailyOccurrence(now, cfg.EVDepartureHH, cfg.EVDepartureMM).Unix()
			}
		}
	}

	return p
}

// nextDailyOccurrence returns the next occurrence of HH:MM local time at or after now.
func nextDailyOccurrence(now time.Time, hh, mm int) time.Time {
	loc := now.Location()
	candidate := time.Date(now.Year(), now.Month(), now.Day(), hh, mm, 0, 0, loc)
	if candidate.Before(now) {
		candidate = candidate.Add(24 * time.Hour)
	}
	return candidate
}

// tick is one optimization cycle.
func (e *Engine) tick() {
	// 1. Read system state.
	state, err := e.reader.ReadSystemState()
	if err != nil {
		log.Printf("[orchestrator] read state error: %v", err)
		return
	}

	// 2. Inject daily plan target for the current 5-min interval.
	//
	// The bus-driven reader (cmd/hub's MQTTSystemReader) is the sole source of
	// state.CSIPControl and state.ClockOffset: it resolves the active CSIP
	// control from the retained lexa/csip/control message and hands the engine a
	// state that already carries them. The engine never overwrites the reader's
	// offset — the optimizer's serverNow (= Timestamp+ClockOffset) must follow
	// utility (or replayed) time, or TOU evaluation / event timing collapse to
	// local time (e.g. a replay that warps the utility clock would no longer
	// shift the hub's peak window).
	//
	// Query the plan at CSIP server time so a clock offset selects the same
	// interval the planner built its window around (and the optimizer evaluates
	// TOU at). Without this the planned dispatch never lines up with the live
	// tick under a warped clock.
	dp := e.state.dailyPlan.Load()
	serverTime := state.Timestamp.Add(time.Duration(state.ClockOffset) * time.Second)
	state.DailyPlanTarget = dp.CurrentTarget(serverTime)

	// 3. Optimize.
	plan := e.optimizer.Optimize(state)

	// 4. Notify any observer (e.g. compliance-breach forwarding) before
	// actuation, then execute the plan.
	if e.planObserver != nil {
		e.planObserver(plan)
	}
	e.executePlan(plan)

	// 5. Store plan for external inspection (e.g. /status endpoint).
	e.state.lastPlan.Store(&plan)

	// 6. Log decisions.
	if e.Debug || len(plan.Decisions) > 0 {
		e.logPlan(state, plan)
	}
}

// LastPlan returns a snapshot of the most recently computed Plan.
// Safe for concurrent use from any goroutine.
func (e *Engine) LastPlan() Plan {
	if p := e.state.lastPlan.Load(); p != nil {
		return *p
	}
	return Plan{}
}

// CmdDropped reports the total number of engineCmds dropped by enqueue
// (engine_state.go) because cmdCh was full — a full channel means the
// control goroutine wasn't keeping up with SetDERConstraints/SetPrices/Wake
// callers, which today is log-only visibility (WS-9.3). This package stays
// decoupled from internal/metrics (matching internal/mqttutil's same
// stance); the caller with visibility into the metrics registry —
// cmd/hub/main.go — is expected to mirror this into a Counter via
// metrics.Registry.Collect, the same external-mirroring idiom already used
// for lexa_hub_tick_overruns_total. Safe for concurrent use from any
// goroutine.
func (e *Engine) CmdDropped() uint64 {
	return e.cmdDropped.Load()
}

// executePlan fans out the plan's commands to the registered actuators. The
// actuator maps are fixed before Start (RegisterXActuator panics afterwards —
// see mustNotBeStarted), so they can be read directly here with no lock and
// no snapshot-copy.
func (e *Engine) executePlan(plan Plan) {
	for _, cmd := range plan.BatteryCommands {
		a, ok := e.state.battActuators[cmd.Name]
		if !ok {
			log.Printf("[orchestrator] no battery actuator for %q", cmd.Name)
			continue
		}
		if err := a.ApplyBatteryCommand(cmd); err != nil {
			log.Printf("[orchestrator] battery %s command error: %v", cmd.Name, err)
		}
	}

	for _, cmd := range plan.SolarCommands {
		a, ok := e.state.solarActuators[cmd.Name]
		if !ok {
			log.Printf("[orchestrator] no solar actuator for %q", cmd.Name)
			continue
		}
		if err := a.ApplySolarCommand(cmd); err != nil {
			log.Printf("[orchestrator] solar %s command error: %v", cmd.Name, err)
		}
	}

	for _, cmd := range plan.EVSECommands {
		a, ok := e.state.evseActuators[cmd.StationID]
		if !ok {
			a, ok = e.state.evseActuators["*"] // wildcard fallback for single-EVSE setups
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
