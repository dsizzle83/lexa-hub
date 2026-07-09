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
// SetDERConstraints/SetPrices and the intent setters (SetEVGoal/
// SetBackupReserve/SetSolarForecast/SetLoadProfile/SetFallbackTOU), read by
// replan(). A nil pointer / empty slice means "not set" — buildPlannerParams
// falls back to config/diurnal defaults for each unset field independently.
type plannerInput struct {
	derConstraints []StepConstraint
	importPrices   []float64 // per planSteps step; nil = use FallbackTOU
	exportPrices   []float64

	// Intent-fed inputs (Unit 3.1 / DEVICE_ROADMAP §3.2). Each is nil/empty
	// until the matching setter is called; buildPlannerParams overlays them on
	// top of the config-derived params.
	evGoal        *EVGoal           // app/cloud EV charging goal; overrides cfg EV target/departure
	reservePct    *float64          // user backup-reserve floor (percent of capacity); RAISE-only
	solarForecast *ExternalForecast // external solar forecast; wins over the diurnal curve when fresh
	loadProfileKw []float64         // per-step site load (kW) on the 288 grid; empty = scalar load
	fallbackTOU   *TOUCostModel     // tariff-intent TOU model; used when CSIP price slices are nil
}

// EVGoal is an externally-supplied EV charging goal (app or cloud), overlaid
// on the config-derived EV parameters in buildPlannerParams. Energy terms are
// kWh so the planner stays unit-simple (PlannerParams already works in kWh).
type EVGoal struct {
	// TargetSocKwh is the required EV pack energy at departure (kWh).
	TargetSocKwh float64
	// DepartureUnix is the departure instant (Unix seconds, server/tariff zone).
	DepartureUnix int64
	// InitialSocKwh is the user's stated pack energy at plug-in (kWh). Negative
	// means "not stated" — the live EVSE SOC (if any) is used instead.
	InitialSocKwh float64
}

// ExternalForecast is a solar-generation forecast supplied from outside the box
// (a weather model, via lexa/intent/solarforecast). StepKw is on the planner's
// 5-min grid starting at WindowStart.
type ExternalForecast struct {
	// StepKw is the per-step generation forecast (kW), indexed from WindowStart
	// on the 5-min grid (<=288 useful entries; extra are ignored, short is
	// zero-filled by resampleForecast).
	StepKw []float64
	// WindowStart is the Unix second the StepKw series begins at (5-min aligned).
	WindowStart int64
	// ReceivedUnix is the ARRIVAL time of this forecast on THIS box (Unix s),
	// not the weather model's own timestamp. Staleness is judged against the
	// local monotone-ish wall clock so a warped/stepped utility clock can never
	// make a fresh forecast look stale or vice-versa (same clock-warp-safe
	// stance as lexa-api's plan heartbeat).
	ReceivedUnix int64
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

	// forecastSource records which solar-forecast path buildPlannerParams last
	// took: forecastExternal (a fresh external forecast was resampled onto the
	// plan grid) or forecastDiurnal (the clear-sky fallback ran — no forecast,
	// or a stale one). Written by the planner goroutine (buildPlannerParams,
	// which replan() calls single-threaded), read by any caller via
	// ForecastSource(). Holds a string; Load()==nil ⇒ "" before the first plan.
	forecastSource atomic.Value
	// forecastAgeSecs is the age (seconds) of the external solar forecast at the
	// last buildPlannerParams evaluation: now-ReceivedUnix when an external
	// forecast was present (fresh OR stale), else -1. Same writer/reader split
	// as forecastSource; initialised to -1 in New.
	forecastAgeSecs atomic.Int64
}

// Solar-forecast source labels stored in Engine.forecastSource and reported by
// ForecastSource(). cmd/hub's planObserver stamps these onto the plan log.
const (
	forecastExternal = "external"
	forecastDiurnal  = "diurnal"
)

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
	// No external forecast has been evaluated yet: report age -1 until the first
	// plan that sees one (ForecastSource() stays "" until the first plan runs).
	e.forecastAgeSecs.Store(-1)
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

// Intent setters (Unit 3.1 / DEVICE_ROADMAP §3.2). Each is a byte-for-byte copy
// of the SetPrices idiom: enqueue a closure onto cmdCh (cap 16, drop-and-count
// on overflow), which the control goroutine applies via setPlanIn's atomic
// read-modify-write, then poke the planner. Latency contract, unchanged from
// SetPrices/SetDERConstraints: the mutation takes effect no later than the next
// tick/safetyTick/forced-Wake tick, because drainCmds runs immediately before
// every one of them. All are safe for concurrent use from any goroutine.

// SetEVGoal overlays an externally-supplied EV charging goal (app/cloud) on top
// of the config-derived EV target/departure. Triggers an immediate re-plan.
func (e *Engine) SetEVGoal(g EVGoal) {
	e.enqueue(func(s *engineState) {
		s.setPlanIn(func(in *plannerInput) { in.evGoal = &g })
	})
	e.signalReplan()
}

// SetBackupReserve sets the user backup-reserve floor (percent of battery
// capacity). It can only RAISE the reserve above the configured safety floor —
// see buildPlannerParams — never lower it. Triggers an immediate re-plan.
func (e *Engine) SetBackupReserve(pct float64) {
	e.enqueue(func(s *engineState) {
		s.setPlanIn(func(in *plannerInput) { in.reservePct = &pct })
	})
	e.signalReplan()
}

// SetSolarForecast supplies an external solar-generation forecast that wins over
// the diurnal clear-sky curve while it is fresh (buildPlannerParams' age gate).
// Triggers an immediate re-plan.
//
// Unlike SetPrices — which stores the caller's slices directly, since the CSIP
// converter builds a fresh slice per pricing update — this copies f.StepKw
// defensively: the retained lexa/intent/solarforecast handler may reuse its
// decode buffer across broker redeliveries, so a shared slice could be mutated
// under the planner. The copy is <=288 float64 and off the tick path.
func (e *Engine) SetSolarForecast(f ExternalForecast) {
	f.StepKw = append([]float64(nil), f.StepKw...)
	e.enqueue(func(s *engineState) {
		s.setPlanIn(func(in *plannerInput) { in.solarForecast = &f })
	})
	e.signalReplan()
}

// SetLoadProfile supplies a per-step site-load forecast (kW) on the 288-slot
// 5-min grid; empty restores the scalar LoadForecastKw. Triggers an immediate
// re-plan. Copies stepKw defensively for the same retained-handler reason as
// SetSolarForecast.
func (e *Engine) SetLoadProfile(stepKw []float64) {
	cp := append([]float64(nil), stepKw...)
	e.enqueue(func(s *engineState) {
		s.setPlanIn(func(in *plannerInput) { in.loadProfileKw = cp })
	})
	e.signalReplan()
}

// SetFallbackTOU supplies a tariff-intent TOU model, used when CSIP price slices
// are absent (utility pricing still wins by the planner's nil-slice fallback
// rule). The *TOUCostModel is compiled fresh per tariff intent, so the pointer
// is stored as-is (no copy needed). Triggers an immediate re-plan.
func (e *Engine) SetFallbackTOU(m *TOUCostModel) {
	e.enqueue(func(s *engineState) {
		s.setPlanIn(func(in *plannerInput) { in.fallbackTOU = m })
	})
	e.signalReplan()
}

// ForecastSource reports which solar-forecast path the most recent plan used:
// "external" (a fresh external forecast was resampled onto the plan grid) or
// "diurnal" (the clear-sky fallback ran — no forecast, or a stale one). Empty
// before the first plan. Safe for concurrent use from any goroutine.
func (e *Engine) ForecastSource() string {
	if v, ok := e.forecastSource.Load().(string); ok {
		return v
	}
	return ""
}

// ForecastAgeSeconds reports the age (seconds) of the external solar forecast at
// the most recent plan evaluation, or -1 when no external forecast was in effect
// (including before the first plan). A large value alongside
// ForecastSource()=="diurnal" means a stale forecast was rejected in favour of
// the fallback. Safe for concurrent use from any goroutine.
func (e *Engine) ForecastAgeSeconds() int64 {
	return e.forecastAgeSecs.Load()
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
	//
	// The high-water OBSERVATION runs UNCONDITIONALLY — above the external-
	// forecast gate below — because it is an independent sensor-derived estimate
	// the diurnal fallback needs kept warm: if an external forecast arrives fresh
	// today but goes stale tomorrow, the fallback must still have a peak to shape.
	// Only the diurnal ASSIGNMENT to p.SolarForecastKw is gated (Unit 3.1 §3.3).
	if curKw := state.TotalSolarW() / 1000; curKw > 0 {
		if shape := clearSkyShape(localHourOf(now.Unix())); shape > 0.15 {
			if est := curKw / shape; est > e.state.lastSolarPeakKw {
				e.state.lastSolarPeakKw = est
			}
		}
	}
	// Solar forecast gate (§11 staleness rule): a FRESH external forecast wins;
	// otherwise fall back to the clear-sky diurnal curve (the block below is the
	// pre-existing forecast code, unchanged, just moved into the else arm).
	const maxForecastAgeS = 12 * 3600 // §11 staleness rule; config-overridable later
	if fc := inp.solarForecast; fc != nil && now.Unix()-fc.ReceivedUnix <= maxForecastAgeS {
		p.SolarForecastKw = resampleForecast(fc, p.WindowStart)
		e.forecastSource.Store(forecastExternal)
		e.forecastAgeSecs.Store(now.Unix() - fc.ReceivedUnix)
	} else {
		peakKw := e.state.lastSolarPeakKw
		if cfg.SolarPeakKw > peakKw {
			peakKw = cfg.SolarPeakKw
		}
		p.SolarForecastKw = diurnalSolarForecast(p.WindowStart, peakKw)
		e.forecastSource.Store(forecastDiurnal)
		if fc := inp.solarForecast; fc != nil {
			// A stale external forecast exists: the fallback ran, but record its
			// age so ForecastAgeSeconds()/the plan log surface the staleness.
			e.forecastAgeSecs.Store(now.Unix() - fc.ReceivedUnix)
		} else {
			e.forecastAgeSecs.Store(-1) // no external forecast at all
		}
	}

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
		// EV goal override (Unit 3.1 §3.3): an app/cloud goal wins over the
		// config-derived target/departure just set. A stated initial SOC seeds
		// the energy integration; <0 leaves the live-EVSE SOC (above) in place.
		if g := inp.evGoal; g != nil {
			p.EVTargetSocKwh = g.TargetSocKwh
			p.EVDepartureUnix = g.DepartureUnix
			if g.InitialSocKwh >= 0 {
				p.InitialEVSocKwh = g.InitialSocKwh
			}
		}
	}

	// Reserve + load profile + fallback TOU overlays (Unit 3.1 §3.3), next to
	// the TerminalReservePct derivation above. p.BattCapacityKwh was set by the
	// battery loop (0 when no connected battery, making the reserve a no-op).
	if r := inp.reservePct; r != nil {
		// Intents may only RAISE the reserve floor, never lower it — a safety
		// invariant. We clamp against the CFG-DERIVED floor (the same 20%
		// default the battery loop applies), not the raw cfg field, so a lowball
		// intent can never drop TerminalSocKwh below the floor the loop set.
		// Planner economics only: the optimizer's SOCReserve safety checks and
		// the Tier-0 interlock floor are untouched.
		floorPct := cfg.TerminalReservePct
		if floorPct <= 0 {
			floorPct = 20
		}
		pct := math.Max(*r, floorPct)
		p.TerminalSocKwh = p.BattCapacityKwh * pct / 100
		p.MinBattSocKwh = math.Max(p.MinBattSocKwh, p.BattCapacityKwh*pct/100)
	}
	if lp := inp.loadProfileKw; len(lp) > 0 {
		p.LoadProfileKw = lp
	}
	if inp.fallbackTOU != nil {
		p.FallbackTOU = inp.fallbackTOU
	}

	return p
}

// resampleForecast shifts an external solar forecast's per-step kW series onto
// the plan window's 5-min grid. fc.StepKw is indexed from fc.WindowStart; align
// it to windowStart by a whole-step offset ((windowStart-fc.WindowStart)/
// planStepSec). A forecast that STARTS in the future (fc.WindowStart >
// windowStart) yields a negative offset, which is deliberately NOT clamped:
// the leading plan steps (before the forecast's own start) zero-fill via the
// src<0 branch below, and the series lands at its temporally correct steps.
// Start-aligning instead (clamping the offset to 0) would shift the whole
// solar curve EARLIER by the gap — wrong peak timing, mispricing every
// battery/EV decision the DP makes off it (principal review finding, unit 3.1).
// Out-of-range steps are zero-filled — the same rule planStepSolar applies to a
// short SolarForecastKw. Each value is clamped >= 0, and any non-finite entry
// maps to 0: the bus layer already rejects NaN/Inf upstream (GAP-09), so this
// is pure defense-in-depth against a forecast that slipped through.
func resampleForecast(fc *ExternalForecast, windowStart int64) []float64 {
	out := make([]float64, planSteps)
	if fc == nil {
		return out
	}
	offset := (windowStart - fc.WindowStart) / planStepSec
	for t := 0; t < planSteps; t++ {
		src := int64(t) + offset
		if src < 0 || src >= int64(len(fc.StepKw)) {
			continue // zero-fill out-of-range
		}
		v := fc.StepKw[src]
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			continue // clamp negatives / non-finite to zero (defense-in-depth)
		}
		out[t] = v
	}
	return out
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
