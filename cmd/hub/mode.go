package main

// mode.go is Unit 3.4 (docs/DEVICE_ROADMAP.md §3.5): the modeManager, the
// runtime switch between the hub's two plan authors —
//
//   - "optimizer": the legacy DefaultOptimizer cascade (the TASK-059 shadow
//     wrapper when constraint_shadow is on) — the cost-optimal author that has
//     always run.
//   - "gateway":   a constraint.Stack whose economics slot is CSIPPassthrough
//     (Unit 3.5) — a pure CSIP gateway that forwards the utility's active
//     control (or restore/idle defaults) with no economic opinion, narrowed by
//     the SAME shadow-validated compliance constraints (Export/GenLimit/
//     ImportLimit) that run in the shadow stack today.
//
// THE load-bearing invariant (ADR-0001; ecosystem roadmap §14): Tier-1 safety
// evaluation is MODE-INVARIANT. EvaluateSafety ALWAYS delegates to the legacy
// *DefaultOptimizer, regardless of which author Optimize() routes to. A
// protection relay is not policy: gateway mode must NOT hand the fast protective
// reflex to the gateway stack's own (still shadow-only, un-flipped) BatterySafety
// constraint. Get this wrong and gateway mode silently disables the reflex that
// force-disconnects an inverting/reserve-violating pack — mode_test.go's
// mutation-check pins it (a fake safety evaluator's sentinel plan must still
// surface in gateway mode).
//
// Zero engine changes: the engine already takes any orchestrator.Optimizer and
// type-asserts it to orchestrator.SafetyEvaluator to wire the fast loop
// (engine.go:199). modeManager satisfies BOTH, so passing it to orchestrator.New
// as the optimizer argument (main.go) is the ONLY structural touch — it wraps
// exactly what was passed to New before. SystemState already carries CSIPControl;
// the mode is only which author reads it.
//
// AxisConnect fan-out (Unit 3.5 finding, CLOSED by Unit 3.6): the gateway
// stack's AxisConnect demands now fan out to the OWNING class's command —
// stack.go's emitCommands routes a resolved Desired.Connect onto
// SolarCommand.Connect / EVSECommand.Connect / BatteryCommand.Connect by the
// value axis the same device carries. cmd/hub's solar and EVSE actuators fold
// that Connect into the retained desired doc (desired.go). CAVEAT the desired
// DOC carries Connect for all three classes, but EXECUTION of cease-to-energize
// is complete only for battery today (its reconciler writes OpModConnect); the
// lexa-modbus solar reconciler (solarFieldsToControl) and the lexa-ocpp EVSE
// reconciler (applyActionLocked) still drop the Connect field on the write path
// — a reconciler-side follow-up (see the Unit 3.6 report). Gateway-mode
// solar/EVSE curtailment (ceiling / current, incl. 0 A suspend) is fully
// functional; hard disconnect for those two classes is doc-complete,
// execution-pending.
//
// Gateway mode × constraint_shadow (soak note — where an operator will look):
// the two are orthogonal wirings of the SAME shadow-validated constraint code.
// In OPTIMIZER mode with constraint_shadow on, the shadow wrapper runs the
// candidate Stack observe-only and diffs it against the legacy cascade (the P5
// gate). In GATEWAY mode, modeManager.Optimize routes to the gateway Stack
// directly — modeMgr.optimizer (the shadow wrapper) is NOT called at all, so
// the economic shadow diff IDLES for the duration of gateway mode (its
// divergence counters simply stop advancing; a soak reading zero divergences
// while the hub is in gateway mode is expected, not a passing economic gate).
// Tier-1 safety still routes to the legacy evaluator either way (below).

import (
	"log/slog"
	"sync/atomic"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/journal"
	"lexa-hub/internal/metrics"
	"lexa-hub/internal/mqttutil"
	"lexa-hub/internal/orchestrator"
)

// engineWaker is the subset of *orchestrator.Engine the mode manager needs to
// poke on a flip: just Wake(), so the new author runs on the very next tick
// instead of waiting out the economic interval. An interface (not the concrete
// *Engine) so mode_test.go can substitute a recording fake — the same
// testability reason intent.go isolates hubEngine. *orchestrator.Engine
// satisfies it structurally, with no orchestrator-package change.
type engineWaker interface {
	Wake()
}

// modeManager is the runtime switch between the optimizer and gateway plan
// authors (see the file doc). It implements BOTH orchestrator.Optimizer (Optimize
// routes by mode) and orchestrator.SafetyEvaluator (EvaluateSafety is
// mode-invariant), so it drops straight into orchestrator.New in place of the
// bare optimizer.
type modeManager struct {
	// mode holds the current author, "optimizer" | "gateway", as a string in an
	// atomic.Value: written by request()/onModeStatus (rare, off the control
	// goroutine), read lock-free by Optimize on every tick.
	mode atomic.Value

	optimizer orchestrator.Optimizer       // optimizer-mode author (shadow-wrapped when constraint_shadow)
	gateway   orchestrator.Optimizer       // gateway-mode author (constraint.Stack + CSIPPassthrough)
	safety    orchestrator.SafetyEvaluator // ALWAYS the raw *DefaultOptimizer — Tier-1 is mode-invariant

	jw           *journal.Writer
	mc           mqtt.Client
	gatewayGauge *metrics.Gauge // lexa_hub_mode_gateway (0/1); nil-safe
	eng          engineWaker    // set post-construction via setEngine, for Wake()

	// bootSealed gates onModeStatus. While false (during boot, before
	// eng.Start()) the FIRST retained lexa/hub/mode message re-seeds the
	// persisted mode; once sealed (immediately after Start) later messages on
	// that topic are ignored — the hub is the SOLE writer of its own mode, so a
	// forged/broker-echoed status must never flip it. Only the intent path
	// (request) flips the mode after boot.
	bootSealed atomic.Bool

	now func() time.Time // seam for tests; time.Now in production
}

// compile-time proof modeManager is a drop-in for orchestrator.New's optimizer
// argument AND the fast-loop SafetyEvaluator the engine type-asserts it to.
var (
	_ orchestrator.Optimizer       = (*modeManager)(nil)
	_ orchestrator.SafetyEvaluator = (*modeManager)(nil)
)

// newModeManager builds the manager. initialMode is the config-derived starting
// author (cfg.Mode, already validated ∈ {optimizer, gateway} by loadConfig); the
// boot re-seed (onModeStatus) may override it from the retained lexa/hub/mode
// topic before SealBoot. safety MUST be non-nil (the raw *DefaultOptimizer in
// production): the whole point of the manager is that the protective reflex never
// routes through the gateway author, so a nil safety is a programming error, not
// a runtime fallback.
func newModeManager(initialMode string, optimizer, gateway orchestrator.Optimizer, safety orchestrator.SafetyEvaluator, jw *journal.Writer, mc mqtt.Client, gatewayGauge *metrics.Gauge) *modeManager {
	if safety == nil {
		panic("newModeManager: safety evaluator must not be nil (Tier-1 is mode-invariant)")
	}
	if initialMode != "optimizer" && initialMode != "gateway" {
		initialMode = "optimizer"
	}
	m := &modeManager{
		optimizer:    optimizer,
		gateway:      gateway,
		safety:       safety,
		jw:           jw,
		mc:           mc,
		gatewayGauge: gatewayGauge,
		now:          time.Now,
	}
	m.mode.Store(initialMode)
	m.setGatewayGauge(initialMode) // boot: gauge reflects the starting author
	return m
}

// setEngine wires the engine post-construction (the engine takes the manager as
// its optimizer argument, so the manager cannot receive the engine at
// construction). Called once in main.go before eng.Start().
func (m *modeManager) setEngine(e engineWaker) { m.eng = e }

// Mode returns the current plan author, defaulting to "optimizer" if somehow
// unset (belt-and-suspenders — the constructor always stores a value).
func (m *modeManager) Mode() string {
	if v, ok := m.mode.Load().(string); ok {
		return v
	}
	return "optimizer"
}

// Optimize implements orchestrator.Optimizer, delegating to the current author.
// Called on the engine's single control goroutine every economic tick.
func (m *modeManager) Optimize(state orchestrator.SystemState) orchestrator.Plan {
	if m.Mode() == "gateway" {
		return m.gateway.Optimize(state)
	}
	return m.optimizer.Optimize(state)
}

// EvaluateSafety implements orchestrator.SafetyEvaluator and is DELIBERATELY
// mode-invariant: it ALWAYS delegates to the legacy *DefaultOptimizer, never the
// gateway author. Tier-1 is a protection relay (ADR-0001; ecosystem roadmap §14),
// not policy — the fast protective reflex (force-disconnect an inverting or
// reserve-violating pack) must be identical in both modes. The gateway stack's
// own BatterySafety constraint is still shadow-only (un-flipped), so routing
// safety through it would be premature; the raw legacy path stays authoritative.
// mode_test.go's mutation-check pins this with a sentinel plan.
func (m *modeManager) EvaluateSafety(state orchestrator.SystemState) orchestrator.Plan {
	return m.safety.EvaluateSafety(state)
}

// request performs a mode transition on behalf of a lexa/intent/mode adoption
// (routed through intentAdopter.adopt, so ID-dedupe + IntentResult + the generic
// intent_applied/rejected journal already wrap this call). mode is pre-validated
// ∈ {optimizer, gateway} by intentAdopter.applyMode.
//
// Ordering (spec §3.5): a same-mode request is a "duplicate" no-op; otherwise the
// mode_change is journaled BEFORE the flip (the journal anchors which author was
// live at every instant — a flip-then-journal ordering could attribute a tick to
// the wrong author across a crash), then mode.Store, then eng.Wake() (the new
// author runs on the next tick without waiting out the interval), then the
// retained lexa/hub/mode status is published (the hub's own restart re-seed).
//
// No bespoke transition machinery is needed (spec §3.5): the gateway stack's
// first pass emits an explicit demand for EVERY device (restore ceiling / idle
// setpoint / policy EVSE ceiling), so optimizer-authored holds are released by
// explicit desired-doc writes and the actuators' content-dedupe absorbs the
// no-ops; the reverse direction re-seeds engine state exactly like a restart (the
// compliance constraints kept running throughout, so convergence counters were
// never suspended, and retained reconcile reports re-seed breach state).
func (m *modeManager) request(mode string, meta bus.IntentMeta) (outcome, detail string) {
	from := m.Mode()
	if from == mode {
		return "duplicate", ""
	}
	if m.jw != nil {
		if ev, err := journal.NewModeChangeEvent("hub", journal.NewModeChange(from, mode, meta.Actor, meta.Origin, meta.ID)); err == nil {
			_ = m.jw.Append(ev)
		}
	}
	m.mode.Store(mode)
	m.setGatewayGauge(mode)
	if m.eng != nil {
		m.eng.Wake()
	}
	m.publishModeStatus(mode, meta)
	slog.Info("hub mode changed", "from", from, "to", mode, "actor", meta.Actor, "origin", meta.Origin, "intent_id", meta.ID)
	return "applied", ""
}

// publishModeStatus writes the retained lexa/hub/mode authoritative state (the
// hub's own restart re-seed source, subscribe-own-retained like breach
// snapshots). Since and Ts are both the flip time.
func (m *modeManager) publishModeStatus(mode string, meta bus.IntentMeta) {
	now := m.now().Unix()
	st := bus.ModeStatus{
		Envelope: bus.Envelope{V: bus.ModeStatusV},
		Mode:     mode,
		Since:    now,
		Actor:    meta.Actor,
		IntentID: meta.ID,
		Ts:       now,
	}
	if err := mqttutil.PublishJSONRetained(m.mc, bus.TopicHubMode, st); err != nil {
		slog.Warn("lexa-hub: publish mode status", "mode", mode, "err", err)
	}
}

// onModeStatus is the BOOT RE-SEED handler for the retained lexa/hub/mode topic,
// subscribed before eng.Start(). While boot is unsealed, the FIRST retained
// message adopts the persisted mode SILENTLY — mode.Store only, no journal, no
// republish, no Wake (the engine is not running yet, and this IS the persisted
// state, not a new transition). Precedence: this retained value ▸ cfg.Mode ▸
// "optimizer" — a stored retained mode overrides the config-derived starting
// mode. Once SealBoot has run (immediately after Start), later messages are
// IGNORED: the hub is the sole writer of its own mode, and only the intent path
// (request) flips it — a forged or broker-echoed status must never move it.
func (m *modeManager) onModeStatus(msg bus.ModeStatus) {
	if m.bootSealed.Load() {
		return
	}
	if msg.Mode != "optimizer" && msg.Mode != "gateway" {
		return // corrupt/empty retained value: keep the config-derived mode
	}
	m.mode.Store(msg.Mode)
	m.setGatewayGauge(msg.Mode)
}

// SealBoot closes the boot re-seed window (called once, immediately after
// eng.Start()). After it, onModeStatus ignores every lexa/hub/mode message.
//
// It also publishes the retained ModeStatus for the mode the hub actually
// booted into (integration-day finding, 2026-07-10): before this, the
// retained doc only ever appeared after the FIRST mode flip, so a fresh
// deploy showed `/status` "mode: unknown" (and lexactl "mode: unknown")
// despite the optimizer author running fine. Publishing here — after the
// re-seed window closed, so the value is final — makes the retained topic
// authoritative from boot. Re-publishing the same value a re-seed just
// adopted is harmless (retained overwrite, identical content); Actor/
// IntentID are empty for a boot publish (no intent caused it).
func (m *modeManager) SealBoot() {
	m.bootSealed.Store(true)
	m.publishModeStatus(m.Mode(), bus.IntentMeta{})
}

// setGatewayGauge mirrors the current mode onto lexa_hub_mode_gateway (1 in
// gateway mode, 0 otherwise). Nil-safe: the gauge is absent in unit tests.
func (m *modeManager) setGatewayGauge(mode string) {
	if m.gatewayGauge == nil {
		return
	}
	if mode == "gateway" {
		m.gatewayGauge.Set(1)
	} else {
		m.gatewayGauge.Set(0)
	}
}
