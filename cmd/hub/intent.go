package main

// intent.go is the hub's intent-adoption layer (Unit 3.3,
// docs/DEVICE_ROADMAP.md §3.1): the single place lexa/intent/{kind} messages
// become engine state. intentAdopter.adopt is the funnel every per-kind
// mqttutil.Subscribe handler in main.go calls through — ID-dedupe, apply,
// journal, publish bus.IntentResult — for all seven intent kinds:
//
//	evgoal, reserve, tariff, solarforecast, loadprofile, chargenow, mode
//
// "mode" (Unit 3.4) routes through the SAME adopt funnel via applyMode (below),
// so it gets the identical ID-dedupe + IntentResult + generic intent_applied/
// rejected surface as every other kind — but its engine-state EFFECT (the mode
// flip, the mode_change journal written BEFORE the flip, the retained
// lexa/hub/mode status, and eng.Wake) lives in modeManager.request (mode.go),
// NOT here: this file adds no mode_change journal entry, and the mode subscribe
// block lives in main.go beside the other six. applyMode only validates the
// value and delegates.
//
// Binding contract (docs/extension/00_PROGRESS.md review log, unit 1.1+1.2
// finding (a)): journal constructors are bus-decoupled SCALAR-FIELD PAIRS —
// journal.NewIntentApplied(kind, id, outcome, detail, actor) +
// NewIntentAppliedEvent(svc, p), and the NewIntentRejected*/NewIntentRejectedEvent
// twins — NOT the flattened single-call sketch DEVICE_ROADMAP §3.1 shows.
// This file uses the real constructors.
//
// Boot re-seed (§3.1 note 3): every kind here except chargenow is retained,
// so a broker reconnect or a fresh hub process redelivers the current value
// of each on (re)subscribe. lastID starts empty on every process start, so
// the FIRST delivery of each retained kind after a restart always applies
// (adopt's ID-dedupe only fires on a SECOND delivery of the identical ID) —
// that first apply on a fresh process IS the re-seed; no separate bootstrap
// path is needed or present.
import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/journal"
	"lexa-hub/internal/metrics"
	"lexa-hub/internal/mqttutil"
	"lexa-hub/internal/orchestrator"
)

// forecastStaleAfter is the adoption-time staleness threshold for
// solarforecast intents (§3.1 "Staleness-at-adoption"): a retained forecast
// whose staleness input is older than this is still APPLIED — the engine's
// own age gate (buildPlannerParams' maxForecastAgeS, Unit 3.1, same 12h
// value) decides diurnal-fallback at planning time — but the adopter raises
// a rate-limited edge alarm here so a genuinely stale cloud forecast is
// visible in the logs at the moment it's adopted, not just inferable later
// from Engine.ForecastSource()=="diurnal".
const forecastStaleAfter = 12 * time.Hour

// hubEngine is the subset of *orchestrator.Engine's intent-setter surface the
// adopter needs. An interface — not the concrete type — so intent_test.go
// can substitute a recording fake instead of standing up a running
// orchestrator.Engine (which would require Start()ing goroutines just to
// observe a setter call); *orchestrator.Engine satisfies this structurally,
// with no change to internal/orchestrator. main.go wires the real Engine.
type hubEngine interface {
	SetEVGoal(orchestrator.EVGoal)
	SetBackupReserve(pct float64)
	SetSolarForecast(orchestrator.ExternalForecast)
	SetLoadProfile(stepKw []float64)
	SetFallbackTOU(m *orchestrator.TOUCostModel)
}

// costModelSwapper is *orchestrator.DefaultOptimizer's SwapCostModel method,
// isolated as an interface for the same testability reason as hubEngine.
type costModelSwapper interface {
	SwapCostModel(m *orchestrator.TOUCostModel)
}

// intentAdopter is the single owner of every lexa/intent/{kind} → engine-state
// transition except "mode" (Unit 3.4). One instance is constructed in main.go
// and handed to six explicit mqttutil.Subscribe closures — never a
// "lexa/intent/+" wildcard (topics.go's doc: TopicIntentResult shares the
// prefix and must not be self-subscribed).
type intentAdopter struct {
	mu sync.Mutex

	eng hubEngine
	opt costModelSwapper // tariff intent's second landing spot (§3.4)
	jw  *journal.Writer
	mc  mqtt.Client
	cfg *Config

	// modes routes "mode" intents (Unit 3.4): applyMode validates the value and
	// delegates the transition to modeManager.request (mode.go). Set by main.go
	// after both the adopter and the modeManager exist (the modeManager is
	// orchestrator.New's optimizer argument, built first; the adopter is built
	// after the engine). nil in the Unit 3.3 tests, which never route "mode".
	modes *modeManager

	// lastID dedupes retained redelivery per kind (IntentMeta.ID): a broker
	// reconnect or hub restart redelivers every retained intent topic, and
	// re-adopting the SAME id must not re-journal or re-apply (adopt's doc).
	// Updated unconditionally after every apply (even a rejected/expired
	// one), so a persistently-invalid retained intent doesn't re-reject and
	// re-journal on every reconnect either — only a genuinely NEW id does.
	lastID map[string]string

	// standingEVGoal is the last APPLIED evgoal intent (nil until the first
	// one lands). chargenow accelerates its departure and chargeNowRevert
	// restores exactly this value when the TTL elapses; a fresh evgoal
	// intent replaces it and cancels any pending revert — the new goal IS
	// the new standing state.
	standingEVGoal *orchestrator.EVGoal

	// chargeNowRevert is the pending timer that restores standingEVGoal at
	// TTL expiry; nil when no chargenow is in flight. chargeNowGen is a
	// generation counter bumped by every evgoal/chargenow apply — the
	// revert closure captures the generation it was scheduled under and
	// no-ops if superseded before it fires, closing the (Stop-vs-already-
	// fired) race between a late revert and a fresh intent landing in the
	// same instant.
	chargeNowRevert *time.Timer
	chargeNowGen    uint64

	// lastForecastStaleAlarm rate-limits the solarforecast stale-at-adoption
	// alarm (rewalkRateLimit's 10s-floor style, state.go) — a retained topic
	// redelivers on every broker reconnect, so without a limiter a flapping
	// connection would log-spam an already-known-stale forecast.
	lastForecastStaleAlarm time.Time

	// solarMaxWTotal/allMaxWTotal are the plausibility-ceiling inputs,
	// precomputed once from cfg.Devices at construction (§3.1): 1.5x the
	// summed nameplate of "inverter"-role (solar) devices for solarforecast,
	// 3x the summed nameplate of every device for loadprofile.
	solarMaxWTotal float64
	allMaxWTotal   float64

	appliedCtr  *metrics.Counter
	rejectedCtr *metrics.Counter

	now func() time.Time // seam for tests; time.Now in production
}

// newIntentAdopter constructs the adopter. eng/opt are interfaces (hubEngine/
// costModelSwapper above) so tests can substitute recording fakes; main.go
// passes the real *orchestrator.Engine and *orchestrator.DefaultOptimizer,
// which satisfy them structurally with no orchestrator-package change.
func newIntentAdopter(eng hubEngine, opt costModelSwapper, jw *journal.Writer, mc mqtt.Client, cfg *Config, appliedCtr, rejectedCtr *metrics.Counter) *intentAdopter {
	var solarMaxW, allMaxW float64
	for _, d := range cfg.Devices {
		allMaxW += d.MaxW
		if d.Role == "inverter" {
			solarMaxW += d.MaxW
		}
	}
	return &intentAdopter{
		eng:            eng,
		opt:            opt,
		jw:             jw,
		mc:             mc,
		cfg:            cfg,
		lastID:         make(map[string]string),
		solarMaxWTotal: solarMaxW,
		allMaxWTotal:   allMaxW,
		appliedCtr:     appliedCtr,
		rejectedCtr:    rejectedCtr,
		now:            time.Now,
	}
}

// adopt is the single funnel every kind handler in main.go calls through.
// meta.ID (when non-empty) dedupes retained redelivery: a repeat of the SAME
// ID never re-runs apply and never journals, but still gets an IntentResult
// reply ("duplicate") so a caller that republished after a missed result
// still hears back. Outcome classification for the journal (binding contract,
// see the package doc): "applied"/"clamped" → journal.NewIntentApplied(Event);
// anything else ("rejected"/"expired"/...) → journal.NewIntentRejected(Event).
func (a *intentAdopter) adopt(kind string, meta bus.IntentMeta, apply func() (outcome, detail string)) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if meta.ID != "" && a.lastID[kind] == meta.ID {
		a.publishResult(kind, meta.ID, "duplicate", "")
		return
	}

	outcome, detail := apply()
	a.lastID[kind] = meta.ID

	switch outcome {
	case "applied", "clamped":
		a.appliedCtr.Inc()
		if a.jw != nil {
			if ev, err := journal.NewIntentAppliedEvent("hub", journal.NewIntentApplied(kind, meta.ID, outcome, detail, meta.Actor)); err == nil {
				_ = a.jw.Append(ev)
			}
		}
	default:
		a.rejectedCtr.Inc()
		if a.jw != nil {
			if ev, err := journal.NewIntentRejectedEvent("hub", journal.NewIntentRejected(kind, meta.ID, outcome, detail, meta.Actor)); err == nil {
				_ = a.jw.Append(ev)
			}
		}
	}

	a.publishResult(kind, meta.ID, outcome, detail)
}

// publishResult publishes bus.IntentResult on lexa/intent/result, bounded at
// 1s (the emitAlerts pattern, main.go) — a result is rare (one per received
// intent) and edge-like, so a short synchronous bound is the right trade
// here too, not the async-and-harvest-next-tick shape the per-tick actuator
// publishes use.
func (a *intentAdopter) publishResult(kind, id, outcome, detail string) {
	res := bus.IntentResult{
		Envelope: bus.Envelope{V: bus.IntentResultV},
		ID:       id,
		Kind:     kind,
		Outcome:  outcome,
		Detail:   detail,
		Ts:       a.now().Unix(),
	}
	if err := mqttutil.PublishJSONTimeout(a.mc, bus.TopicIntentResult, false, res, 1*time.Second); err != nil {
		slog.Warn("lexa-hub: publish intent result", "kind", kind, "id", id, "outcome", outcome, "err", err)
	}
}

// applyEVGoal validates and applies an evgoal intent (lexa/intent/evgoal).
func (a *intentAdopter) applyEVGoal(msg bus.EVGoalIntent) (outcome, detail string) {
	if msg.TargetSocKwh == nil {
		return "rejected", "target_soc_kwh is required"
	}
	target := *msg.TargetSocKwh
	if target < 0 {
		return "rejected", "target_soc_kwh must be >= 0"
	}
	// PRINCIPAL RULING (this unit's brief, following the 3.1 review log's
	// open item): CapacityKwh is VALIDATION-ONLY in v1 — it bounds the
	// stated target against the user-stated vehicle pack size, but is never
	// plumbed to the engine. orchestrator.EVGoal has no capacity field, and
	// %→kWh resolution is the app/cloud layer's job (EVGoalIntent's doc
	// comment); the hub stays unit-simple (kWh in, kWh out). A future unit
	// that wants the hub itself to reason in percent-of-capacity would need
	// an additive EVGoal.CapacityKwh field plus a buildPlannerParams
	// insertion — out of scope here.
	if msg.CapacityKwh != nil && target > *msg.CapacityKwh {
		return "rejected", "target exceeds stated capacity"
	}
	if msg.DepartureUnix <= a.now().Unix() {
		return "expired", "departure_unix is not in the future"
	}

	initial := -1.0 // "not stated" — engine.go's buildPlannerParams then keeps whatever live-EVSE SOC it already picked up
	if msg.InitialSocKwh != nil {
		initial = *msg.InitialSocKwh
	}
	goal := orchestrator.EVGoal{
		TargetSocKwh:  target,
		DepartureUnix: msg.DepartureUnix,
		InitialSocKwh: initial,
	}
	a.eng.SetEVGoal(goal)

	// This intent IS the new standing goal: it replaces whatever chargenow
	// was accelerating toward and cancels any pending revert — a fresh
	// user/cloud-stated goal always wins over an in-flight
	// accelerate-then-restore cycle (§3.1 sketch: "a new evgoal cancels it
	// and becomes the standing goal").
	a.standingEVGoal = &goal
	a.chargeNowGen++
	if a.chargeNowRevert != nil {
		a.chargeNowRevert.Stop()
		a.chargeNowRevert = nil
	}
	return "applied", ""
}

// applyReserve validates and applies a reserve intent (lexa/intent/reserve).
func (a *intentAdopter) applyReserve(msg bus.BackupReserveIntent) (outcome, detail string) {
	if msg.ReservePct == nil {
		return "rejected", "reserve_pct is required"
	}
	pct := *msg.ReservePct
	if pct < 0 || pct > 100 {
		return "rejected", "reserve_pct must be within [0,100]"
	}
	a.eng.SetBackupReserve(pct)
	// The engine clamps UP to the configured safety floor internally
	// (buildPlannerParams — intents may only RAISE the reserve, never lower
	// it). This adopter has no way to observe whether that clamp fired (no
	// getter exposes the effective floor), so a raised-but-clamped reserve
	// still reports "applied" in v1 — an honest, documented limitation
	// rather than a guess at "clamped".
	return "applied", ""
}

// applyTariff validates and applies a tariff intent (lexa/intent/tariff):
// compiles the spec via compileTariff (cmd/hub/tariff.go, a pure function)
// and installs the result on BOTH landing spots — the planner's
// SetFallbackTOU (used only when CSIP SetPrices arrays are nil) and the
// reactive optimizer's SwapCostModel.
func (a *intentAdopter) applyTariff(msg bus.TariffIntent) (outcome, detail string) {
	m, err := compileTariff(msg.Tariff)
	if err != nil {
		return "rejected", err.Error()
	}
	a.eng.SetFallbackTOU(m)
	a.opt.SwapCostModel(m)
	return "applied", ""
}

// intentPlanStepSec is the planner's 5-minute grid step, matching the
// (function-local, same value) planStepSec constants in main.go — named
// separately at package scope here to avoid shadowing those.
const intentPlanStepSec = 5 * 60

// applySolarForecast validates and applies a solarforecast intent
// (lexa/intent/solarforecast).
func (a *intentAdopter) applySolarForecast(msg bus.SolarForecastIntent) (outcome, detail string) {
	if len(msg.StepKw) > 288 {
		return "rejected", fmt.Sprintf("step_kw has %d entries, want <= 288", len(msg.StepKw))
	}
	if msg.WindowStart%intentPlanStepSec != 0 {
		return "rejected", "window_start must be 5-minute aligned"
	}

	ceilingKw := 1.5 * a.solarMaxWTotal / 1000
	steps := append([]float64(nil), msg.StepKw...)
	clamped := 0
	for i, v := range steps {
		switch {
		case v < 0:
			steps[i] = 0
			clamped++
		case v > ceilingKw:
			steps[i] = ceilingKw
			clamped++
		}
	}

	now := a.now()
	// Staleness-at-adoption uses SourceTs ("when the weather model ran" —
	// SolarForecastIntent's own doc comment names this "the staleness
	// input"), not IntentMeta.IssuedAt (when the INTENT MESSAGE was
	// published/republished) — a retained forecast redelivered on a broker
	// reconnect keeps the same SourceTs, which is what actually answers "how
	// old is this forecast", whereas IssuedAt only says when this copy of it
	// was last sent. See this file's report/open-questions note if this
	// reading should be revisited.
	stale := msg.SourceTs > 0 && now.Sub(time.Unix(msg.SourceTs, 0)) > forecastStaleAfter
	if stale && now.Sub(a.lastForecastStaleAlarm) >= rewalkRateLimit {
		a.lastForecastStaleAlarm = now
		slog.Warn("lexa-hub: solarforecast intent adopted stale (age > 12h) — engine age gate governs planning-time fallback",
			"id", msg.ID, "source_ts", msg.SourceTs, "age", now.Sub(time.Unix(msg.SourceTs, 0)))
	}

	a.eng.SetSolarForecast(orchestrator.ExternalForecast{
		StepKw:       steps,
		WindowStart:  msg.WindowStart,
		ReceivedUnix: now.Unix(),
	})

	switch {
	case clamped > 0 && stale:
		return "clamped", fmt.Sprintf("%d step(s) clamped to plausibility ceiling; stale at adoption (age > 12h)", clamped)
	case clamped > 0:
		return "clamped", fmt.Sprintf("%d step(s) clamped to plausibility ceiling", clamped)
	case stale:
		return "applied", "stale at adoption (age > 12h); engine age gate governs fallback"
	default:
		return "applied", ""
	}
}

// applyLoadProfile validates and applies a loadprofile intent
// (lexa/intent/loadprofile). Same shape rules as solarforecast (len cap,
// clamp-and-count) except there is no WindowStart to align — LoadProfileIntent
// carries none (it rides the planner's existing window, same as
// SolarForecastKw's sibling scalar LoadForecastKw did before this).
func (a *intentAdopter) applyLoadProfile(msg bus.LoadProfileIntent) (outcome, detail string) {
	if len(msg.StepKw) > 288 {
		return "rejected", fmt.Sprintf("step_kw has %d entries, want <= 288", len(msg.StepKw))
	}

	// Generous plausibility ceiling (§3.1): 3x the summed nameplate of every
	// configured device, not just load-relevant ones — documented as
	// deliberately loose (this is a sanity bound against a garbled feed, not
	// a real site-load model).
	ceilingKw := 3 * a.allMaxWTotal / 1000
	steps := append([]float64(nil), msg.StepKw...)
	clamped := 0
	for i, v := range steps {
		switch {
		case v < 0:
			steps[i] = 0
			clamped++
		case v > ceilingKw:
			steps[i] = ceilingKw
			clamped++
		}
	}

	a.eng.SetLoadProfile(steps)
	if clamped > 0 {
		return "clamped", fmt.Sprintf("%d step(s) clamped to plausibility ceiling", clamped)
	}
	return "applied", ""
}

// applyChargeNow validates and applies a chargenow intent
// (lexa/intent/chargenow — the one EDGE kind, not retained).
//
// Honest v1 limitation (this unit's brief, principal ruling): without a
// previously-applied evgoal intent (standingEVGoal == nil), this adopter has
// no safe way to construct chargenow's accelerated EVGoal. The engine's own
// config-derived default (cfg.Planner's EVCapacityKwh/EVTargetSocPct/
// EVDepartureHH-MM) is computed by buildPlannerParams using an unexported
// helper (nextDailyOccurrence) and live EVSE-session state this package
// cannot read — reconstructing it here would risk silently diverging from
// the engine's own math, which is worse than refusing outright. So v1
// rejects chargenow whenever no standing goal exists yet, REGARDLESS of
// whether hub.json's planner block configures a default EV target: the
// caller's correct move is to post an evgoal intent first (establishing a
// standing goal with an explicit, already-validated departure time), then
// chargenow to accelerate it. This is a deliberate, documented v1 gap, not
// an oversight — flagged as an open question in this unit's report.
func (a *intentAdopter) applyChargeNow(msg bus.ChargeNowIntent) (outcome, detail string) {
	if msg.TTLS <= 0 {
		return "rejected", "ttl_s must be > 0"
	}
	now := a.now()
	if msg.IssuedAt+int64(msg.TTLS) < now.Unix() {
		return "expired", "issued_at + ttl_s is in the past"
	}
	if a.standingEVGoal == nil {
		return "rejected", "no EV goal to accelerate — set a target first"
	}

	ttl := time.Duration(msg.TTLS) * time.Second
	accelerated := orchestrator.EVGoal{
		TargetSocKwh:  a.standingEVGoal.TargetSocKwh,
		DepartureUnix: now.Add(ttl).Unix(),
		InitialSocKwh: a.standingEVGoal.InitialSocKwh,
	}
	a.eng.SetEVGoal(accelerated)

	standing := *a.standingEVGoal
	a.chargeNowGen++
	gen := a.chargeNowGen
	if a.chargeNowRevert != nil {
		a.chargeNowRevert.Stop() // a second chargenow resets the timer, per §3.1
	}
	a.chargeNowRevert = time.AfterFunc(ttl, func() { a.revertChargeNow(gen, standing) })

	return "applied", fmt.Sprintf("accelerated departure to +%ds; reverts to standing goal on expiry", msg.TTLS)
}

// revertChargeNow restores goal as the engine's active EV target once a
// chargenow's TTL elapses. Runs on time.AfterFunc's own goroutine (unlike the
// apply* methods above, which only ever run from inside adopt's already-held
// lock), so it takes a.mu itself. gen is checked against the CURRENT
// a.chargeNowGen so a revert superseded by a newer evgoal/chargenow before it
// fired is a silent no-op rather than clobbering whatever replaced it (the
// Stop()-vs-already-fired race Timer.Stop's doc warns about).
func (a *intentAdopter) revertChargeNow(gen uint64, goal orchestrator.EVGoal) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if gen != a.chargeNowGen {
		return
	}
	a.chargeNowRevert = nil
	a.eng.SetEVGoal(goal)
}

// applyMode validates a mode intent (lexa/intent/mode) and routes it to the
// modeManager (Unit 3.4, mode.go). This is the seventh kind's apply funnel — it
// reuses adopt()'s ID-dedupe + IntentResult + generic intent_applied/rejected
// surface like every other kind, but the engine-state effect (the mode flip,
// the mode_change journal written BEFORE the flip, the retained lexa/hub/mode
// status, and eng.Wake) lives entirely in modeManager.request, NOT here — this
// file adds no mode_change journal entry (see the package doc's split).
//
// A same-mode request returns request()'s "duplicate" (a no-op flip); an unknown
// value is "rejected" here before ever reaching the manager.
func (a *intentAdopter) applyMode(msg bus.ModeIntent) (outcome, detail string) {
	if msg.Mode != "optimizer" && msg.Mode != "gateway" {
		return "rejected", fmt.Sprintf("mode must be \"optimizer\" or \"gateway\", got %q", msg.Mode)
	}
	if a.modes == nil {
		// Defensive: main.go always sets a.modes before registering the mode
		// subscribe. A nil here means a mode intent reached an adopter never
		// wired for it (only reachable in a misconfigured test).
		return "rejected", "mode manager not wired"
	}
	return a.modes.request(msg.Mode, msg.IntentMeta)
}
