package orchestrator

import (
	"fmt"
	"log"
	"math"
	"sync/atomic"
	"time"

	"lexa-hub/internal/utilitytime"
	model "lexa-proto/csipmodel"
)

// exportGuard carries state across ticks for the conservative export-limit rule.
type exportGuard struct {
	evSetpointA     float64 // last EV current limit issued; NaN until first command
	evCmdW          float64 // last EV power commanded (current × voltage at command time); NaN = none
	batteryAbsorbW  float64 // last battery absorption (positive watts) commanded; NaN = none
	safeCount       int     // consecutive ticks where actual export ≤ conservative target
	activeLimitW    float64 // limit value when guard was reset; NaN = no active limit
	filteredExportW float64 // low-pass-filtered actual export, used by the controller
	solarCeilingW   float64 // sticky generation ceiling commanded; NaN = uncurtailed
	battStallTicks  int     // consecutive ticks battery was commanded to absorb but didn't
}

// importGuard carries state across ticks for the conservative import-limit rule.
// Mirrors exportGuard: without sticky state the rule fires only when import
// strictly exceeds the limit, then applyRestoreRule idles the battery on the
// next tick, the import jumps back over the limit, and the system oscillates
// at the tick period.  Holding the prior discharge command between ticks
// (with a relax-cycle ramp-down) settles the controller at a steady operating
// point just under the limit.
type importGuard struct {
	dischargeW   float64 // last battery discharge commanded (positive watts); NaN = none
	safeCount    int     // consecutive ticks where importW ≤ hard limit (battery ramp-down gate)
	evSafeCount  int     // consecutive ticks where 0 ≤ importW ≤ hard limit (EV resume gate)
	activeLimitW float64 // limit value when guard was reset; NaN = no active limit
	breachTicks  int     // consecutive ticks measured import stayed over the cap (convergence check)
}

// genGuard carries state across ticks for the generation-limit rule's
// closed-loop convergence check. Curtailing an inverter to a generation cap is
// a fire-and-forget Modbus write; a device can ACK the write but lag or reject
// the actual output change. Without verifying the measured effect the hub would
// assert a compliance the meter contradicts. overCount counts consecutive ticks
// the measured generation has stayed over the cap *after* curtailment was
// commanded, so a sustained non-convergence is told apart from the normal
// one-or-two-tick ramp-down.
type genGuard struct {
	activeLimitW float64 // cap value when guard was reset; NaN = no active cap
	overCount    int     // consecutive ticks measured generation stayed over the cap
}

// ausGenGuard carries per-cap-session state for the CSIP-AUS gross-generation
// rule's closed-loop convergence check (WP-11). Mirrors genGuard field-for-field
// — same tolerance-band session tracking, same leaky counter — but over GROSS
// generation (solar + battery discharge), the quantity opModGenLimW caps.
type ausGenGuard struct {
	activeLimitW float64 // cap value when guard was reset; NaN = no active cap
	overCount    int     // consecutive ticks measured GROSS generation stayed over the cap
}

// ausLoadGuard carries per-cap-session state for the CSIP-AUS gross-load rule
// (WP-11). Mirrors importGuard's shape: without sticky state the EV lever would
// fire only when gross load strictly exceeds the cap, the EV-charging rule
// would restore full current the next tick, and the system would oscillate at
// the tick period (the exact failure importGuard's doc describes). The whole
// guard — including breachTicks — resets together on a MEANINGFUL cap-value
// change, the same single reset domain applyImportLimitRule uses.
type ausLoadGuard struct {
	activeLimitW float64 // limit value when guard was reset; NaN = no active limit
	evLimitA     float64 // sticky EV current ceiling (A); NaN = EV lever not engaged
	safeCount    int     // consecutive ticks gross load ≤ hard cap (EV relax gate)
	breachTicks  int     // leaky convergence counter with NaN-hold semantics
}

// genBreachTicks is how many ticks measured generation may stay over the cap
// (after curtailment is commanded) before the hub reports a CannotComply. Long
// enough to ride out a normal inverter ramp-down (1–2 ticks, further softened by
// the leaky counter below), short enough that the breach is detected AND the
// downstream alert→northbound→gridsim CannotComply chain completes inside a tight
// compliance window — at 3 (≈9 s) detection no longer races the ~20 s window the
// way 5 (≈15 s) did, which was the reject-write/enable-gate-curtail flakiness.
const genBreachTicks = 3

// battConvergeFrac / battBreachTicks gate the export rule's closed-loop
// battery-absorption check: measured absorption must reach at least
// battConvergeFrac of the commanded charge, else after battBreachTicks ticks the
// commanded (phantom) absorption is no longer credited and the inverter is
// curtailed instead (audit: battery-charge-disabled). A normal charge ramp closes
// the gap within a tick or two (further softened by the leaky counter), so these
// ride it out. battBreachTicks is 3 (≈9 s) so the curtail engages inside a tight
// export-cap window rather than racing it (the battery-charge-disabled flakiness,
// the export-lever twin of the gen-limit genBreachTicks fix).
const (
	battConvergeFrac = 0.5
	battBreachTicks  = 3
)

// ausGenBreachTicks / ausLoadBreachTicks are how many ticks measured GROSS
// generation / GROSS load may stay over the CSIP-AUS cap (WP-11) before the
// hub reports a CannotComply. Same value and leaky accumulation as their
// direct templates (genBreachTicks / importBreachTicks): long enough to ride
// out a normal inverter ramp-down or EV soft-stop, short enough that the
// detect→alert→northbound CannotComply chain completes inside a tight
// compliance window. Scaled by scaleTicks like every other tick-denominated
// threshold so the wall-clock latency is cadence-invariant.
const (
	ausGenBreachTicks  = 3
	ausLoadBreachTicks = 3
)

// DefaultOptimizer is a rule-based + heuristic optimizer.
//
// Priority order:
//
//  1. Safety        — CSIP disconnect overrides everything
//  2. Fixed dispatch — meet an explicit grid export request (OpModFixedW)
//  3. Export limit  — absorb excess into EVSEs, then battery, then curtail solar
//  4. Self-use      — route solar surplus to battery
//  5. TOU peak      — discharge battery during expensive grid hours
//  6. EV charging   — allocate remaining budget to EVSEs
type DefaultOptimizer struct {
	// CostModel is optional; when non-nil it drives TOU peak discharge.
	CostModel *TOUCostModel

	// Debug enables per-rule logging.
	Debug bool

	// SOCReserve is the minimum SOC [0,100] kept for demand-response.  Default 20%.
	SOCReserve float64

	// SOCFullThreshold is the SOC above which charging stops.  Default 95%.
	SOCFullThreshold float64

	// ExcessSolarThreshold is the minimum surplus watts before routing to battery.
	// Avoids constant tiny adjustments.  Default 100 W.
	ExcessSolarThreshold float64

	// ExportMarginFrac is the safety margin applied to the export limit.
	// The optimizer targets limit×(1−margin) rather than the hard limit.
	// Default 0.15 (operate at 85 % of the limit).
	ExportMarginFrac float64

	// ExportRelaxCycles is the number of consecutive ticks where actual export
	// stays at or below the conservative target before the EV setpoint is
	// allowed to relax.  Default 5.
	ExportRelaxCycles int

	// ImportMarginFrac is the safety margin applied to the import limit.
	// The optimizer targets limit×(1−margin) so the battery sits comfortably
	// inside the import window rather than chattering across the boundary.
	// Default 0.20.
	ImportMarginFrac float64

	// EVImportCooldownCycles is the number of consecutive ticks where actual
	// grid import is positive and under the hard limit before EV charging is
	// re-allowed after an import-limit event.  Negative grid (site exporting
	// due to battery transient) resets the count, preventing the EV from
	// resuming during the over-discharge settling period.
	//
	// This is tick-denominated: size it from the engine interval to a
	// wall-clock target of ~1 min (cmd/hub derives it as 60s/interval).
	// Default 20, which is ≈ 1 min only at a 3 s demo tick.
	EVImportCooldownCycles int

	// expGuard holds per-limit-session state for the export-limit rule.
	expGuard exportGuard

	// expOverTicks is the export-cap convergence counter: ticks the MEASURED
	// export has stayed over the active export limit. Deliberately NOT a field
	// of expGuard — that struct is the ceiling *controller's* state and resets
	// on every cap VALUE change, but a rewritten cap value is a new controller
	// session with the SAME compliance obligation. control-churn rewrites the
	// cap every ~12 s alternating 0 W / 500 W (a step far wider than any noise
	// tolerance band), so a counter sharing the controller's reset cadence
	// would be wiped before ever reaching its threshold — structurally unable
	// to fire for the exact fault it exists to catch. Session-scoped instead:
	// reset only when the export limit clears entirely (checkExportLimitConvergence).
	expOverTicks int

	// expZeroLeverTicks debounces the export rule's OWN "last lever exhausted"
	// breach (audit D3): the feed-forward ceiling can drive the solar ceiling to
	// ~0 in a single tick against a 1-2-tick-lagged meter, so an undebounced
	// zero-lever emitter posts a spurious CannotComply on any brief meter-noise
	// excursion under a tight cap. Same leaky accumulation and session scope as
	// expOverTicks (a transient blip decays; a genuine no-lever episode escalates).
	expZeroLeverTicks int

	// impGuard holds per-limit-session state for the import-limit rule.
	impGuard importGuard

	// genGuard holds per-cap-session state for the generation-limit rule's
	// measured-effect convergence check.
	genGuard genGuard

	// EnforceAusLimits gates the WP-11 CSIP-AUS dynamic-envelope cascade rules
	// (applyAusGenerationLimitRule / applyAusLoadLimitRule + their convergence
	// backstops) — hub.json's `enforce_aus_limits`, default false. The limits
	// themselves are ALWAYS adopted into GridState (see GridState.GenLimitW);
	// with the flag off, Optimize's output is byte-identical to pre-WP-11
	// regardless of what limits are present. Set once at construction from
	// cmd/hub; never mutated while the engine runs.
	EnforceAusLimits bool

	// ausGenGuard / ausLoadGuard hold per-cap-session state for the CSIP-AUS
	// gross-generation / gross-load rules (WP-11). Untouched while
	// EnforceAusLimits is false.
	ausGenGuard  ausGenGuard
	ausLoadGuard ausLoadGuard

	// battDrainTicks counts, per battery, consecutive ticks the pack was measured
	// discharging at/below its SOC reserve — a state no rule commands, so a
	// sustained count means the device is inverting/ignoring its setpoint
	// (audit: battery-wrong-sign). Drives the checkBatterySafety disconnect.
	battDrainTicks map[string]int

	// battWrongDirTicks counts, per battery, consecutive ticks where the hub
	// commanded the pack to charge (negative setpoint) but it measured as
	// discharging. This catches a sign-flipped battery regardless of SOC level —
	// the SOC-reserve check alone misses it when SoC is high (audit: battery-wrong-sign).
	battWrongDirTicks map[string]int

	// battReserveHold latches per-pack discharge suppression at the SOC-reserve
	// floor (audit B-1 / GAP-08). It is the SINGLE owner of the reserve-floor
	// decision, consulted by every discharge author via dischargeBlocked. Enter
	// is immediate when measured SOC ≤ SOCReserve (protect fast); release requires
	// SOC ≥ SOCReserve+reserveReleaseMarginPct sustained for reserveReleaseTicks
	// ticks (release slow). Without this hysteresis every author gated discharge
	// on the INSTANTANEOUS SOC ≤ reserve, so ±1pt BMS telemetry dither at the
	// reserve line re-authorized full discharge on every above-line tick — a
	// multi-kW↔0 W command square wave that walked the pack below reserve at ~50%
	// duty. Owned and advanced by updateReserveHolds at the top of each Optimize.
	battReserveHold map[string]bool

	// battReserveRecoverTicks counts, per held pack, consecutive ticks measured
	// SOC has stayed at/above the release threshold (reserve+margin). Any tick
	// below it resets the run to 0, so a value dithering in the margin band never
	// releases the hold.
	battReserveRecoverTicks map[string]int

	// tickInterval is the engine cadence. When > 0 it scales the tick-denominated
	// breach/debounce thresholds so their WALL-CLOCK meaning is constant across
	// cadences (fast 3 s vs stock 15 s) — the product ships in stock timing but is
	// QA'd in fast timing, so a raw tick count means the shipped safety/CannotComply
	// latency is not the one that was validated. 0 = use the raw constants (unit
	// tests and the historical 3 s fast tick they were calibrated at). Set via
	// SetTickInterval from cmd/hub.
	tickInterval time.Duration

	// lastBattCmd records the setpoint (W; <0 = charge) most recently commanded to
	// each battery by Optimize, so the fast protection loop's EvaluateSafety — which
	// runs between economic ticks without a fresh plan — can tell whether a measured
	// discharge contradicts a commanded charge (audit: battery-wrong-sign). Written
	// by Optimize and read by EvaluateSafety on the same control goroutine, so it
	// needs no lock.
	lastBattCmd map[string]float64

	// costModelOverride, when set via SwapCostModel (tariff-intent path,
	// TASK-094/§3.4), atomically replaces the CostModel field for all INTERNAL
	// reads (which go through costModel()). nil = no override = use the
	// constructor-time CostModel field. An atomic.Pointer so a tariff swap on
	// any goroutine is race-free against Optimize reading it on the control
	// goroutine — no lock is added, and the exported CostModel field stays
	// construction-time-only (its own reads are the accessor's nil-override
	// fallback). Zero value (nil) means behaviour is bit-identical to reading
	// CostModel directly, so pre-swap construction and existing tests are
	// unaffected.
	costModelOverride atomic.Pointer[TOUCostModel]
}

// tunedTickInterval is the engine cadence the *BreachTicks constants were
// calibrated at (the FAST replay/QA tick). scaleTicks is a no-op at this cadence,
// so unit tests that construct the optimizer without an interval see the exact
// legacy tick counts.
const tunedTickInterval = 3 * time.Second

// SetTickInterval tells the optimizer the engine cadence so it can hold the
// wall-clock meaning of the breach/debounce thresholds constant across cadences.
// Safe to call once at construction, before Start.
func (o *DefaultOptimizer) SetTickInterval(d time.Duration) { o.tickInterval = d }

// SwapCostModel atomically replaces the TOU cost model that the reactive
// peak-discharge rule (Rule 5) consults, without adding a lock to the control
// loop. It is the optimizer half of the tariff-intent path (TASK-094/§3.4): a
// compiled TariffSpec model is installed here while the planner half rides
// Engine.SetFallbackTOU. Passing nil REVERTS to the constructor-time CostModel
// field — a tariff-intent clear falls back to the shipped DefaultTOUCostModel
// (or to nil/disabled if the field was never set). Safe to call from any
// goroutine while Optimize runs on the control goroutine.
func (o *DefaultOptimizer) SwapCostModel(m *TOUCostModel) { o.costModelOverride.Store(m) }

// costModel returns the effective TOU model for INTERNAL reads: the swapped
// override when set, else the constructor-time CostModel field (which may
// itself be nil, meaning TOU peak-shift is disabled). Callers must read it once
// into a local — the override can change between calls, so re-reading would
// risk a nil-deref if a concurrent SwapCostModel(nil) landed mid-sequence.
func (o *DefaultOptimizer) costModel() *TOUCostModel {
	if m := o.costModelOverride.Load(); m != nil {
		return m
	}
	return o.CostModel
}

// scaleTicks converts a threshold expressed in tuned-cadence ticks into the
// equivalent tick count at the configured engine cadence, preserving the
// wall-clock duration the constant encodes. A floor of 2 keeps the single-glitch
// tolerance the leaky counters rely on even when one stock tick already exceeds
// the tuned hold. Returns ticks unchanged when no interval is configured (tests)
// or the cadence matches the tuned one (fast mode).
func (o *DefaultOptimizer) scaleTicks(ticks int) int {
	if o.tickInterval <= 0 || o.tickInterval == tunedTickInterval {
		return ticks
	}
	hold := time.Duration(ticks) * tunedTickInterval
	n := int(math.Round(hold.Seconds() / o.tickInterval.Seconds()))
	if n < 2 {
		n = 2
	}
	return n
}

// scaleRateWPerTick converts a physical ramp rate — expressed as watts moved
// in one tuned-cadence tick (tunedTickInterval) — into the equivalent
// watts-per-tick at the configured engine cadence, preserving the WALL-CLOCK
// rate (W/s) the constant encodes. This is the rate-domain twin of scaleTicks
// (which preserves a wall-clock DURATION for a tick-COUNT threshold instead):
// a "W per tick" constant is not cadence-invariant by itself — the same
// physical ramp expressed as a fixed watts-per-tick allowance describes a 5x
// faster real-world ramp at the FAST 3 s tick than at the STOCK 15 s tick
// unless it is rescaled by tick length (audit: STOCK QA malform-huge-
// activepower/wan-outage-hold — the export ceiling's slew limit took ~5x
// longer, in wall-clock seconds, to re-tighten after a relax/disturbance at
// STOCK than at FAST purely from this, not from any northbound/reconciler
// staleness path).
//
// This mirrors internal/orchestrator/constraint/export.go's ceilingSlewW /
// orchestrator.InverterPlant.MaxRampDownWPerS·MaxRampUpWPerS (TASK-057/064,
// AD-007), which already made the NEW constraint-stack candidate path
// cadence-correct; the legacy cascade here (applyExportLimitRule) was left on
// the raw tuned-tick constants during that migration ("preserve-first ...
// Until then the constants in optimizer.go remain the single source of
// truth", plantmodel.go's package doc) and never received the matching fix —
// the exact gap plantwiring_test.go's "STOCK spot-check at the wave gate"
// comment flagged as outstanding. Returns wPerTickAtTuned unchanged when no
// interval is configured (tests) or the cadence matches the tuned one (fast
// mode) — bit-identical to the pre-fix raw constant in both those cases.
func (o *DefaultOptimizer) scaleRateWPerTick(wPerTickAtTuned float64) float64 {
	if o.tickInterval <= 0 || o.tickInterval == tunedTickInterval {
		return wPerTickAtTuned
	}
	return wPerTickAtTuned / tunedTickInterval.Seconds() * o.tickInterval.Seconds()
}

// tickSeconds returns the wall-clock length of one engine tick for latency
// telemetry, defaulting to the tuned cadence when unset.
func (o *DefaultOptimizer) tickSeconds() float64 {
	if o.tickInterval <= 0 {
		return tunedTickInterval.Seconds()
	}
	return o.tickInterval.Seconds()
}

// NewDefaultOptimizer returns an optimizer with sensible defaults.
func NewDefaultOptimizer() *DefaultOptimizer {
	return &DefaultOptimizer{
		SOCReserve:             20.0,
		SOCFullThreshold:       95.0,
		ExcessSolarThreshold:   100.0,
		ExportMarginFrac:       0.20,
		ExportRelaxCycles:      5,
		ImportMarginFrac:       0.20,
		EVImportCooldownCycles: 20,
		expGuard: exportGuard{
			evSetpointA:     math.NaN(),
			evCmdW:          math.NaN(),
			batteryAbsorbW:  math.NaN(),
			activeLimitW:    math.NaN(),
			filteredExportW: math.NaN(),
			solarCeilingW:   math.NaN(),
		},
		impGuard: importGuard{
			dischargeW:   math.NaN(),
			activeLimitW: math.NaN(),
		},
		genGuard:                genGuard{activeLimitW: math.NaN()},
		ausGenGuard:             ausGenGuard{activeLimitW: math.NaN()},
		ausLoadGuard:            ausLoadGuard{activeLimitW: math.NaN(), evLimitA: math.NaN()},
		battDrainTicks:          make(map[string]int),
		battWrongDirTicks:       make(map[string]int),
		battReserveHold:         make(map[string]bool),
		battReserveRecoverTicks: make(map[string]int),
		lastBattCmd:             make(map[string]float64),
	}
}

// reserveReleaseMarginPct is the SOC margin above SOCReserve (percentage points)
// a held pack must recover to before its reserve hold releases. Set well above
// realistic BMS quantization/telemetry noise (±1pt) so a value dithering at the
// reserve line can never flip the latch. See battReserveHold.
const reserveReleaseMarginPct = 2.0

// reserveReleaseTicks is how many CONSECUTIVE ticks a held pack must read at/above
// (SOCReserve+reserveReleaseMarginPct) before its hold releases — scaled to
// wall-clock via scaleTicks like every other debounce threshold.
const reserveReleaseTicks = 3

// updateReserveHolds advances the per-pack reserve latch (battReserveHold) from
// THIS tick's measured SOC, before any discharge author runs. The asymmetry is
// deliberate — enter immediately to protect the reserve, release only after a
// sustained recovery above a noise margin — so telemetry dither at the reserve
// line cannot chatter the discharge command (audit B-1 / GAP-08).
func (o *DefaultOptimizer) updateReserveHolds(batteries []BatteryState) {
	if o.battReserveHold == nil {
		o.battReserveHold = make(map[string]bool)
		o.battReserveRecoverTicks = make(map[string]int)
	}
	release := o.scaleTicks(reserveReleaseTicks)
	seen := make(map[string]bool, len(batteries))
	for _, b := range batteries {
		seen[b.Name] = true
		if !b.Connected {
			// A disconnected pack cannot discharge, and the immediate-entry rule
			// re-arms the hold on the reconnect tick (updateReserveHolds runs
			// before the authors) if SOC is still low — so dropping the latch
			// here is safe and lets a pack that recharged while dark re-evaluate
			// from live SOC. Matches battDrainTicks' !Connected handling.
			delete(o.battReserveHold, b.Name)
			delete(o.battReserveRecoverTicks, b.Name)
			continue
		}
		if math.IsNaN(b.SOC) {
			// Unknown SOC: RETAIN an existing hold (fail-safe — never discharge
			// into an unknown reserve) but do not newly arm one (no evidence of a
			// low reserve) and do not count toward recovery.
			o.battReserveRecoverTicks[b.Name] = 0
			continue
		}
		if b.SOC <= o.SOCReserve {
			o.battReserveHold[b.Name] = true
			o.battReserveRecoverTicks[b.Name] = 0
			continue
		}
		// SOC above the reserve line.
		if !o.battReserveHold[b.Name] {
			continue // not held — a pack that never touched reserve is never blocked
		}
		if b.SOC >= o.SOCReserve+reserveReleaseMarginPct {
			o.battReserveRecoverTicks[b.Name]++
			if o.battReserveRecoverTicks[b.Name] >= release {
				delete(o.battReserveHold, b.Name)
				delete(o.battReserveRecoverTicks, b.Name)
			}
		} else {
			// Above reserve but inside the release-margin (dither) band: hold, and
			// reset the recovery run so the release needs an UNBROKEN sustained run.
			o.battReserveRecoverTicks[b.Name] = 0
		}
	}
	// Prune packs that vanished from the state.
	for name := range o.battReserveHold {
		if !seen[name] {
			delete(o.battReserveHold, name)
			delete(o.battReserveRecoverTicks, name)
		}
	}
}

// dischargeBlocked reports whether the named pack's reserve latch currently
// suppresses discharge. It is the single reserve-floor gate every discharge
// author consults. The instantaneous SOC ≤ reserve fold-in is belt-and-suspenders:
// updateReserveHolds runs first so a sub-reserve pack is already latched, but the
// reserve floor is safety-critical enough to double-check the live sample.
func (o *DefaultOptimizer) dischargeBlocked(name string, soc float64) bool {
	if o.battReserveHold[name] {
		return true
	}
	return !math.IsNaN(soc) && soc <= o.SOCReserve
}

// gridConstraints holds effective export/import/max limits after applying CSIP
// overrides on top of grid-reported values.  NaN means unconstrained.
type gridConstraints struct {
	exportLimitW float64
	importLimitW float64
	maxLimitW    float64

	// CSIP-AUS dynamic-envelope axes (WP-11). Unlike the three above they
	// have no DERControlBase override leg: opModGenLimW/opModLoadLimW are
	// EXTENDED controls that reach the optimizer pre-distilled on GridState
	// (cmd/hub adopts bus.ActiveControl.gen_lim_w/load_lim_w into
	// Grid.GenLimitW/LoadLimitW), so deriveGridConstraints copies them
	// through unchanged. Enforcement is gated by EnforceAusLimits.
	genLimitW  float64
	loadLimitW float64
}

// Optimize evaluates all rules against state and returns a Plan.
func (o *DefaultOptimizer) Optimize(state SystemState) Plan {
	now := state.Timestamp
	if now.IsZero() {
		now = time.Now()
	}
	plan := Plan{Timestamp: now}

	// Rule 1: CSIP disconnect — highest priority, always early-return.
	if csipDisconnectRule(state.CSIPControl, state, &plan) {
		return plan
	}

	limits := deriveGridConstraints(state.Grid, state.CSIPControl)
	solarW, batteryW, evseW, surplusW := computePowerBalance(state)
	homeLoadW := state.InferredLoadW()

	if o.Debug {
		log.Printf("[optimizer] solarW=%.0f batteryW=%.0f evseW=%.0f homeLoadW=%.0f surplusW=%.0f gridNetW=%.0f",
			solarW, batteryW, evseW, homeLoadW, surplusW, state.Grid.NetW)
	}

	// Thread a mutable copy of battery states through rules so each rule sees
	// PowerW updated by prior rules (reflects already-committed setpoints).
	batteries := make([]BatteryState, len(state.Batteries))
	copy(batteries, state.Batteries)

	// Advance the per-pack reserve latch from this tick's measured SOC BEFORE any
	// discharge author runs, and expose it as `blocked` — the single hysteretic
	// reserve-floor gate every author consults in place of the old instantaneous
	// SOC ≤ reserve check (audit B-1 / GAP-08). Enter is immediate, release is
	// sustained, so dither at the reserve line cannot chatter the discharge command.
	o.updateReserveHolds(state.Batteries)
	blocked := func(b BatteryState) bool { return o.dischargeBlocked(b.Name, b.SOC) }

	// Rule 2: CSIP fixed dispatch — discharge battery to meet explicit grid export request.
	batteries = applyFixedDispatchRule(state.CSIPControl, batteries, solarW, homeLoadW, o.SOCReserve, blocked, &plan)

	// Rule 2.5: Follow the 24-hour cost-optimal plan.
	// Fires only when a plan exists and CSIP has not already mandated fixed dispatch.
	// Sets battery setpoints and EV current limits from the plan; downstream limit
	// rules (3 & 3.5) still run to enforce live CSIP constraints.
	//
	// An active export limit must ALSO cap the plan's discharge guidance: the
	// export-limit rule (Rule 3) absorbs surplus into battery-charge/EV and
	// curtails solar, but it does NOT reduce a plan-commanded battery *discharge*
	// (it models discharge as an immovable export source). So a stale 24h-plan
	// discharge target — one built before the OpenADR/CSIP export cap arrived,
	// since the planner never sees the reactive export limit — would export past
	// the cap for a whole replan interval (mayhem: openadr-limit-adopt /
	// openadr-csip-precedence). Cap it here at the source, with an ABSOLUTE bound
	// (conservative limit minus the non-battery base export) rather than the
	// autonomous TOU rule's headroom bound: applyPlanRule re-issues the command
	// every tick, so a headroom cap — which shrinks once the meter reflects the
	// discharge — would oscillate. See planExportDischargeCapW.
	planDischargeCapW := o.planExportDischargeCapW(limits, state)
	planFollowed := false
	if state.CSIPControl == nil || state.CSIPControl.Base.OpModFixedW == nil {
		batteries, surplusW, planFollowed = applyPlanRule(state.DailyPlanTarget, batteries, state.EVSEs, o.SOCReserve, o.SOCFullThreshold, surplusW, planDischargeCapW, blocked, &plan)
	}

	// Rule 3: Export/import limit — absorb excess into EVSEs, battery, then curtail solar.
	// Always runs (CSIP compliance cannot be skipped).
	batteries, surplusW = o.applyExportLimitRule(state.Solar, state.EVSEs, evseW, limits, state.Grid.NetW, o.SOCFullThreshold, surplusW, batteries, &plan)
	// Closed-loop check: verify measured export actually converged to the cap.
	o.checkExportLimitConvergence(limits.exportLimitW, state.Grid.NetW, &plan)

	// Rule 3.1: Generation limit — curtail inverters so total output ≤ MaxLimW.
	// Always runs (CSIP compliance cannot be skipped).
	applyGenLimitRule(state.Solar, limits.maxLimitW, &plan)
	// TASK-032: the explicit uncurtail on the cap-release edge (formerly
	// restoreOnGenLimitClear) was deleted — applyRestoreRule (end of pass) emits
	// the same NaN restore for any inverter left uncommanded once the cap clears,
	// and the retained desired doc carries it to a dark inverter on reconnect.
	// Closed-loop check: verify the inverters actually converged to the cap.
	o.checkGenLimitConvergence(state.Solar, state.Batteries, state.Grid.NetW, limits.maxLimitW, &plan)

	// Rule 3.5: Import limit enforcement — discharge battery to reduce grid import.
	batteries = o.applyImportLimitRule(batteries, limits, state.Grid.NetW, o.SOCReserve, &plan)
	// Closed-loop check: verify measured import actually converged to the cap.
	o.checkImportConvergence(limits.importLimitW, state.Grid.NetW, &plan)

	// Rule 3.6 (WP-11, gated on `enforce_aus_limits`): CSIP-AUS gross-
	// GENERATION cap (opModGenLimW — NOT opModMaxLimW; see the rule's doc for
	// the disambiguation). PLACEMENT: with the other CSIP compliance rules
	// (AD-007: compliance > economics), and deliberately LAST of them —
	// after the export (3), opModMaxLimW (3.1), and import (3.5) rules — so
	// that (a) its keep-the-tighter solar-ceiling merge sees the ceilings
	// those rules already issued, and (b) its battery-discharge participation
	// cap sees the discharge they already committed. It runs BEFORE the
	// economics rules (4/5/6) and bounds their autonomous TOU discharge via
	// ausGenTOUCapW (threaded into Rule 5's maxDischargeW exactly the way the
	// export limit threads dischargeCapW). Flag off ⇒ nothing here runs and
	// ausGenTOUCapW stays NaN ⇒ plans byte-identical to pre-WP-11.
	ausGenTOUCapW := math.NaN()
	if o.EnforceAusLimits {
		batteries, ausGenTOUCapW = o.applyAusGenerationLimitRule(state.Solar, batteries, limits.genLimitW, &plan)
		// Closed-loop check: verify measured GROSS generation converged.
		o.checkAusGenerationConvergence(state.Solar, state.Batteries, state.Grid.NetW, limits.genLimitW, &plan)
	}

	if !planFollowed {
		// Rule 4: Self-consumption — route solar surplus to battery.
		batteries, surplusW = applySelfConsumptionRule(batteries, surplusW, o.ExcessSolarThreshold, o.SOCFullThreshold, &plan)

		// Rule 5: TOU peak discharge.
		// CSIP dispatch (OpModFixedW) is handled in Rule 2; this rule covers autonomous peak shifting.
		// serverNow source only (AD-004, TASK-036): utilitytime.ServerNowAt is the
		// same now.Unix()+state.ClockOffset arithmetic, single-owned. orchestrator
		// stays I/O-free — this is a pure function call, no Clock/wall-time read.
		serverNow := time.Unix(utilitytime.ServerNowAt(now, state.ClockOffset), 0)
		// cm is read once (not re-fetched per use) so a concurrent SwapCostModel
		// can't null it out between the nil-check and the method calls.
		cm := o.costModel()
		isPeak := cm != nil && cm.IsPeakHour(serverNow)
		peakReason := ""
		if isPeak {
			peakReason = fmt.Sprintf("peak TOU hour (rate=%.3f/kWh)", cm.CurrentRate(serverNow))
		}
		// An active export limit caps the discharge: the export-limit rule
		// only corrects on the *next* tick, so an uncapped MaxDischargeW
		// command could overshoot the CSIP limit for a full interval.
		dischargeCapW := math.NaN()
		if !math.IsNaN(limits.exportLimitW) {
			margin := o.ExportMarginFrac
			if margin <= 0 {
				margin = 0.20
			}
			exportNowW := 0.0
			if !math.IsNaN(state.Grid.NetW) {
				exportNowW = math.Max(0, -state.Grid.NetW)
			} else {
				exportNowW = math.Max(0, surplusW)
			}
			dischargeCapW = math.Max(0, limits.exportLimitW*(1-margin)-exportNowW)
		}
		// WP-11: the CSIP-AUS gross-generation cap bounds autonomous TOU
		// discharge too — battery discharge is INSIDE the capped quantity.
		// NaN when the flag is off or no gen cap is active (nanMin no-op).
		dischargeCapW = nanMin(dischargeCapW, ausGenTOUCapW)
		batteries, surplusW = applyDemandResponseRule(batteries, surplusW, o.SOCReserve, false, isPeak, peakReason, dischargeCapW, blocked, &plan)

		// Rule 6: EV charging allocation.
		cooldown := o.EVImportCooldownCycles
		if cooldown <= 0 {
			cooldown = 20
		}
		evImportSuppressed := !math.IsNaN(limits.importLimitW) && o.impGuard.evSafeCount < cooldown
		applyEVChargingRule(state.EVSEs, limits, state.Grid.NetW, solarW, surplusW, evImportSuppressed, &plan)
	}

	// Rule 3.7 (WP-11, gated on `enforce_aus_limits`): CSIP-AUS gross-LOAD cap
	// (opModLoadLimW) + its convergence backstop. PLACEMENT: this compliance
	// rule deliberately runs AFTER the economics rules (4/5/6) — the one
	// deviation from the pre-economics slot the other compliance rules occupy
	// — because a load cap must NARROW whatever charge/EV draw economics just
	// proposed, and this cascade's rules coordinate by command OWNERSHIP
	// (hasBatteryCommand/hasEVSECommand skips), which lets a rule own an axis
	// but not narrow a later rule's write. Pre-economics placement would leave
	// self-consumption (4) and EV charging (6) free to re-add the very load
	// this rule shed — a tick-period oscillation, the exact failure
	// importGuard's stickiness exists to kill. Running after economics and
	// before applyRestoreRule/checkBatterySafety, it clamps the FINAL
	// commanded charge/EV draw the same way checkBatterySafety (the other
	// post-pass) overrides final commands: a higher tier narrowing a lower
	// tier's output, which is AD-007's priority order expressed in cascade
	// form. See applyAusLoadLimitRule's doc for the lever details.
	if o.EnforceAusLimits {
		o.applyAusLoadLimitRule(state.Solar, state.Batteries, state.EVSEs, limits.loadLimitW, state.Grid.NetW, &plan)
		// Closed-loop check: verify measured GROSS load converged to the cap.
		o.checkAusLoadConvergence(state.Solar, state.Batteries, state.Grid.NetW, limits.loadLimitW, &plan)
	}

	// Final: restore unconstrained devices so prior setpoints don't persist.
	// While an export or generation cap is active the cap rules own the solar
	// setpoints, so a dark inverter must keep its held curtailment; once both
	// clear, the restore is queued even for dark inverters so the southbound
	// delivers it on reconnect (see applyRestoreRule). The WP-11 AUS gross-
	// generation cap is a solar cap too (its ceiling must survive a dark
	// inverter the same way), so it joins the guard when enforcement is on.
	solarCapActive := !math.IsNaN(limits.exportLimitW) || !math.IsNaN(limits.maxLimitW) ||
		(o.EnforceAusLimits && !math.IsNaN(limits.genLimitW))
	applyRestoreRule(state.Solar, batteries, o.SOCReserve, solarCapActive, &plan)

	// Safety backstop: force-disconnect any pack that is draining itself below the
	// SOC reserve against its command (a device inverting/ignoring its setpoint).
	o.checkBatterySafety(state.Batteries, &plan)

	// Stamp the active control's mRID onto any breach so the northbound service
	// can address the CannotComply Response to the right event.
	if plan.Breach != nil && state.CSIPControl != nil {
		plan.Breach.MRID = state.CSIPControl.MRID
	}

	// WS-4.3 (V1.0 punch list): stamp the same active control's mRID onto
	// every per-device command this tick produced (including checkBatterySafety's
	// forced disconnects above, since that runs inside Optimize() too) — a
	// small, safe, post-hoc blanket stamp mirroring the Breach.MRID technique
	// just above, rather than threading MRID through every individual rule
	// function that builds a command. state.CSIPControl == nil is exactly
	// "no real CSIP control active" (cmd/hub/state.go's busToCSIPControl),
	// in which case MRID correctly stays "". The separate fast-protection-loop
	// path (EvaluateSafety/EvaluateFast) does NOT go through Optimize() and is
	// deliberately NOT stamped here — see EvaluateSafety's own doc.
	if state.CSIPControl != nil {
		for i := range plan.BatteryCommands {
			plan.BatteryCommands[i].MRID = state.CSIPControl.MRID
		}
		for i := range plan.SolarCommands {
			plan.SolarCommands[i].MRID = state.CSIPControl.MRID
		}
		for i := range plan.EVSECommands {
			plan.EVSECommands[i].MRID = state.CSIPControl.MRID
		}
	}

	// Record commanded battery setpoints so the fast protection loop's
	// EvaluateSafety can infer charge-vs-discharge intent between economic ticks.
	if o.lastBattCmd == nil {
		o.lastBattCmd = make(map[string]float64)
	}
	for _, c := range plan.BatteryCommands {
		if !math.IsNaN(c.SetpointW) {
			o.lastBattCmd[c.Name] = c.SetpointW
		}
	}

	return plan
}

// ── Rule functions ─────────────────────────────────────────────────────────────

// csipDisconnectRule ceases to energize all DERs when the utility sends
// OpModConnect=false: batteries are disconnected, solar is curtailed to zero
// output, and EVSE sessions are suspended.  Returns true when Optimize should
// return immediately.
//
// Curtailing solar matters for compliance: cease-to-energize applies to the
// DER as a whole, and a PV inverter exporting through a disconnect order is a
// direct CSIP/IEEE 1547 violation.  EVSEs are load rather than DER, but
// suspending charging during a grid event is the safe choice — and it also
// prevents a session that starts mid-event from ramping unsupervised.
func csipDisconnectRule(cc *CSIPControlState, state SystemState, plan *Plan) bool {
	if cc == nil || cc.Base.OpModConnect == nil || *cc.Base.OpModConnect {
		return false
	}
	f := false
	for _, b := range state.Batteries {
		plan.BatteryCommands = append(plan.BatteryCommands, BatteryCommand{
			Name:    b.Name,
			Connect: &f,
		})
	}
	curtailed := 0
	for _, sol := range state.Solar {
		if !sol.Connected {
			continue
		}
		plan.SolarCommands = append(plan.SolarCommands, SolarCommand{
			Name:       sol.Name,
			CurtailToW: 0,
		})
		curtailed++
	}
	suspended := 0
	for _, ev := range state.EVSEs {
		if !ev.Connected {
			continue
		}
		plan.EVSECommands = append(plan.EVSECommands, EVSECommand{
			StationID:   ev.StationID,
			ConnectorID: ev.ConnectorID,
			MaxCurrentA: 0,
		})
		suspended++
	}
	plan.AddDecision("csip/disconnect",
		"OpModConnect=false received from utility",
		fmt.Sprintf("disconnecting %d batteries, curtailing %d solar, suspending %d EVSEs",
			len(state.Batteries), curtailed, suspended))
	return true
}

// deriveGridConstraints returns the tightest of CSIP and grid-reported limits.
// NaN in any field means no constraint for that direction.
func deriveGridConstraints(grid GridState, cc *CSIPControlState) gridConstraints {
	c := gridConstraints{
		exportLimitW: grid.ExportLimitW,
		importLimitW: grid.ImportLimitW,
		maxLimitW:    grid.MaxLimitW,
		genLimitW:    grid.GenLimitW,
		loadLimitW:   grid.LoadLimitW,
	}
	if cc != nil {
		if lim := cc.Base.OpModExpLimW; lim != nil {
			c.exportLimitW = nanMin(c.exportLimitW, apW(lim))
		}
		if lim := cc.Base.OpModMaxLimW; lim != nil {
			c.maxLimitW = nanMin(c.maxLimitW, apW(lim))
		}
		if lim := cc.Base.OpModImpLimW; lim != nil {
			c.importLimitW = nanMin(c.importLimitW, apW(lim))
		}
	}
	// MaxLimW (absolute generation cap) is enforced by curtailing the inverter
	// output (applyGenLimitRule), NOT by folding it into the export limit: a
	// generation cap limits what the DER produces, and battery absorption keeps
	// the meter export low while generation stays over the cap — a violation.
	return c
}

// computePowerBalance returns the site-level power flows and solar surplus.
//
// Sign conventions (throughout the optimizer):
//
//	solarW   >= 0            (generation)
//	batteryW > 0 discharge, < 0 charge
//	evseW    >= 0            (consumption)
//	Grid.NetW > 0 import from grid, < 0 export
//
// surplusW > 0 means solar exceeds home load and is available for battery or grid.
// When no grid meter is present (NetW=NaN) surplusW equals solarW.
func computePowerBalance(state SystemState) (solarW, batteryW, evseW, surplusW float64) {
	solarW = state.TotalSolarW()
	batteryW = state.TotalBatteryW()
	evseW = state.TotalEVSEW()
	if !math.IsNaN(state.Grid.NetW) {
		// surplusW = solar above home load = export available for battery/grid.
		// Grid.NetW < 0 means exporting; evseW is already on the site bus.
		surplusW = -state.Grid.NetW - evseW
	} else {
		surplusW = solarW
	}
	return
}

// planExportDischargeCapW returns the maximum AGGREGATE battery discharge (W)
// the plan may command without driving meter export past the active export
// limit, or NaN when no export limit is active (⇒ no cap, byte-identical to
// pre-fix plans). Unlike the autonomous TOU rule's headroom cap (which relies
// on "no command ⇒ hold last setpoint" to stay stable), this is an ABSOLUTE
// bound so applyPlanRule can re-issue a stable command every tick:
//
//	cap = conservativeLimit − baseExport
//	baseExport = measuredExport − measuredBatteryDischarge   (solar − load − EV)
//
// i.e. the export attributable to everything BUT the battery. Discharging up to
// `cap` on top of that base lands the meter exactly at the conservative limit;
// the measured-discharge subtraction is what makes the value stable tick-to-tick
// (the discharge already flowing does not eat its own headroom). Charging plans
// (setW < 0) are never capped — they reduce export.
func (o *DefaultOptimizer) planExportDischargeCapW(limits gridConstraints, state SystemState) float64 {
	if math.IsNaN(limits.exportLimitW) {
		return math.NaN()
	}
	margin := o.ExportMarginFrac
	if margin <= 0 {
		margin = 0.20
	}
	conservativeW := limits.exportLimitW * (1 - margin)
	// baseExport = meter export attributable to everything BUT the battery
	// (solar − load − EV) = SIGNED net export minus the SIGNED battery power now
	// flowing. Subtracting the signed power (discharge positive, CHARGE negative)
	// is what makes this "everything but the battery": a charging pack draws from
	// the site, so its negative power must be added back out of the base, not
	// treated as immovable load. Summing only positive (discharge) power inflated
	// the cap by the full charge magnitude at every charge→discharge plan flip
	// (audit EXPCAP-1), briefly letting the plan discharge export past the cap.
	// Signed net export (not floored at 0) also credits a net-IMPORTING site's
	// local load: under a 0 W export cap a pack may still discharge up to the load
	// (offsetting import, exporting nothing). Without a grid meter we cannot credit
	// load, so fall back to a base of 0 ⇒ cap = conservative limit, a pure-discharge
	// bound that can never itself exceed the cap.
	baseExportW := 0.0
	if !math.IsNaN(state.Grid.NetW) {
		signedNetExportW := -state.Grid.NetW
		measuredBatterySignedW := 0.0
		for _, b := range state.Batteries {
			if b.Connected && !math.IsNaN(b.PowerW) {
				measuredBatterySignedW += b.PowerW
			}
		}
		baseExportW = signedNetExportW - measuredBatterySignedW
	}
	return math.Max(0, conservativeW-baseExportW)
}

// applyPlanRule applies the battery setpoint and EV current limit from the
// 24-hour cost-optimal plan.  Returns updated batteries, updated surplusW, and
// true when the plan was applied (suppressing the reactive self-consumption,
// TOU, and EV charging rules downstream).
//
// The plan setpoint is a guidance value; the export/import limit rules still
// run after this to enforce live CSIP compliance. planDischargeCapW (NaN ⇒
// none) additionally bounds the AGGREGATE discharge this rule commands so a
// stale plan cannot export past an active limit for a whole interval — see
// planExportDischargeCapW.
func applyPlanRule(target *PlanTarget, batteries []BatteryState, evses []EVSEState, socReserve, socFull, surplusW, planDischargeCapW float64, dischargeBlocked func(BatteryState) bool, plan *Plan) ([]BatteryState, float64, bool) {
	if target == nil {
		return batteries, surplusW, false
	}
	setW := target.BattSetpointW
	if math.IsNaN(setW) {
		return batteries, surplusW, false
	}

	// Clamp a planned DISCHARGE (setW > 0) to the export-limit headroom before
	// distributing it across packs — the aggregate discharge is proportional to
	// setW, so bounding setW bounds the total the reconciler will actuate. A
	// charge plan (setW < 0) is left untouched.
	if setW > 0 && !math.IsNaN(planDischargeCapW) && setW > planDischargeCapW {
		plan.AddDecision("plan/export-cap",
			fmt.Sprintf("plan discharge %.0fW would export past the active limit", setW),
			fmt.Sprintf("capped to %.0fW (export headroom)", planDischargeCapW))
		setW = planDischargeCapW
	}

	// Distribute the planned setpoint across connected batteries proportionally
	// to their combined charge+discharge power rating (or equally when the
	// rating is unknown).
	totalCap := 0.0
	for _, b := range batteries {
		if b.Connected {
			cap := b.MaxDischargeW + b.MaxChargeW
			if cap <= 0 {
				cap = 1
			}
			totalCap += cap
		}
	}
	if totalCap == 0 {
		return batteries, surplusW, false
	}

	for i, b := range batteries {
		if !b.Connected {
			continue
		}
		cap := b.MaxDischargeW + b.MaxChargeW
		if cap <= 0 {
			cap = 1
		}
		share := setW * cap / totalCap
		// Clamp to device limits.
		share = math.Max(-b.MaxChargeW, math.Min(b.MaxDischargeW, share))
		// Live-SOC safety clamp: the plan setpoint is computed from a forecast
		// SOC trajectory and can lag reality, so never discharge while the reserve
		// latch holds or charge above the full threshold based on the measured
		// SOC. Without this, a stale plan can drive a battery flat (and the device
		// would report phantom power it can't deliver). The discharge side is the
		// hysteretic latch (dischargeBlocked), not a bare SOC ≤ reserve, so dither
		// at the reserve line cannot re-authorize discharge (audit B-1).
		if share > 0 && dischargeBlocked(b) {
			share = 0
		} else if share < 0 && !math.IsNaN(b.SOC) && b.SOC >= socFull {
			share = 0
		}
		batteries[i].PowerW = share
		plan.BatteryCommands = append(plan.BatteryCommands, BatteryCommand{
			Name:      b.Name,
			SetpointW: share,
		})
	}

	// Set EV current from plan; only override EVSEs with active sessions.
	// An idle charger gets no command — when a session starts, this rule
	// applies the plan target on the next tick.
	evCmds := 0
	for _, ev := range evses {
		if ev.Connected && ev.SessionActive {
			cmd := EVSECommand{
				StationID:   ev.StationID,
				ConnectorID: ev.ConnectorID,
				MaxCurrentA: target.EVMaxCurrentA,
			}
			// D8/WP-14: a genuine discharge target (positive W, battery
			// convention) from an ev_storage-enabled plan switches the
			// command to setpoint mode. target.EVSetpointW can only be
			// positive when PlannerParams.EVStorage was true when this
			// plan was built (planner.go's PlanInterval.EVSetpointW doc),
			// so this is unreachable — and MaxCurrentA is used exactly as
			// before — whenever the flag is off. MaxCurrentA is zeroed
			// here to avoid publishing a stale ceiling value alongside a
			// setpoint the desired-doc/OCPP-bridge layer will treat as
			// authoritative instead (EVSECommand.SetpointW's doc: "nil ⇒
			// ceiling mode; non-nil ⇒ MaxCurrentA ignored downstream").
			if !math.IsNaN(target.EVSetpointW) && target.EVSetpointW > 0 {
				w := target.EVSetpointW
				cmd.SetpointW = &w
				cmd.MaxCurrentA = 0
			}
			plan.EVSECommands = append(plan.EVSECommands, cmd)
			evCmds++
		}
	}

	plan.AddDecision("plan/follow",
		fmt.Sprintf("following 24h plan: battery=%.0fW ev=%.1fA", setW, target.EVMaxCurrentA),
		fmt.Sprintf("set %d batteries, %d EVSEs", len(plan.BatteryCommands), evCmds))

	// Zero surplusW so self-consumption and TOU rules don't fire after us.
	return batteries, 0, true
}

// applyFixedDispatchRule discharges batteries to meet an explicit grid export
// request (CSIP OpModFixedW).  Solar is credited first; batteries cover the
// shortfall up to SOC reserve.
func applyFixedDispatchRule(cc *CSIPControlState, batteries []BatteryState, solarW, homeLoadW, socReserve float64, dischargeBlocked func(BatteryState) bool, plan *Plan) []BatteryState {
	if cc == nil || cc.Base.OpModFixedW == nil {
		return batteries
	}
	targetW := apW(cc.Base.OpModFixedW)

	// How much solar output is already available for grid export?
	var availableW float64
	if !math.IsNaN(homeLoadW) {
		availableW = math.Max(0, solarW-homeLoadW)
	} else {
		availableW = solarW // no grid meter — assume all solar can export
	}

	if availableW >= targetW {
		plan.AddDecision("csip/fixed-dispatch",
			fmt.Sprintf("solar provides %.0fW, covering grid request of %.0fW", availableW, targetW),
			"no battery discharge needed")
		return batteries
	}

	shortfallW := targetW - availableW
	for i, b := range batteries {
		if !b.Connected || !b.Energized {
			continue
		}
		if dischargeBlocked(b) {
			plan.AddDecision("csip/fixed-dispatch",
				fmt.Sprintf("battery %s SOC=%.1f%% at/near reserve minimum (hold latched)", b.Name, b.SOC),
				"skip discharge — protecting reserve")
			continue
		}
		if hasBatteryCommand(plan.BatteryCommands, b.Name) {
			continue
		}
		available := b.AvailableDischargeW()
		if available < 50 {
			continue
		}
		dispatchW := math.Min(available, shortfallW)
		newSetpoint := b.PowerW + dispatchW
		plan.BatteryCommands = append(plan.BatteryCommands, BatteryCommand{
			Name:      b.Name,
			SetpointW: newSetpoint,
		})
		plan.AddDecision("csip/fixed-dispatch",
			fmt.Sprintf("grid requests %.0fW; solar covers %.0fW; battery %s dispatches %.0fW",
				targetW, availableW, b.Name, dispatchW),
			fmt.Sprintf("battery %s setpoint → %.0fW", b.Name, newSetpoint))
		batteries[i].PowerW = newSetpoint
		shortfallW -= dispatchW
		if shortfallW <= 1 {
			break
		}
	}
	return batteries
}

// applyExportLimitRule enforces the CSIP/grid export limit conservatively.
//
// Dispatch priority: battery first (absorbs bulk of excess up to rated charge
// power), then EV (absorbs remainder with hysteretic setpoint), then solar
// curtailment as last resort.  Battery-first matches the scenario narrative and
// avoids a round-trip lag: batteries respond in one Modbus write whereas the EV
// ramps over several OCPP MeterValues intervals.
func (o *DefaultOptimizer) applyExportLimitRule(
	solar []SolarState, evses []EVSEState, evseW float64,
	limits gridConstraints, netW, socFull, surplusW float64,
	batteries []BatteryState, plan *Plan,
) ([]BatteryState, float64) {
	if math.IsNaN(limits.exportLimitW) {
		o.expGuard = exportGuard{evSetpointA: math.NaN(), evCmdW: math.NaN(), batteryAbsorbW: math.NaN(), activeLimitW: math.NaN(), filteredExportW: math.NaN(), solarCeilingW: math.NaN()}
		o.expZeroLeverTicks = 0 // cap cleared — no-lever compliance session over (like expOverTicks)
		return batteries, surplusW
	}

	// New limit value → start the guard fresh.
	//
	// NOTE (audit D1, DEFERRED to bench validation): preserving the ceiling
	// integrator across a cap-VALUE change (control-churn) to avoid the onset
	// one-step-correction overshoot was tried and reverted — under a slow/frozen
	// meter it lets the integrator wind the ceiling DOWN below the physically-
	// correct feed-forward value (this full reset re-anchors it each rewrite), a
	// countervailing over-curtailment the verifier flagged as needing a hub-journal
	// capture to weigh against the overshoot. The primary control-churn fix is the
	// release-side debounce (D2); this stays a full reset until bench evidence.
	if limits.exportLimitW != o.expGuard.activeLimitW {
		o.expGuard = exportGuard{evSetpointA: math.NaN(), evCmdW: math.NaN(), batteryAbsorbW: math.NaN(), activeLimitW: limits.exportLimitW, filteredExportW: math.NaN(), solarCeilingW: math.NaN()}
	}

	margin := o.ExportMarginFrac
	if margin <= 0 {
		margin = 0.20
	}
	relaxCycles := o.ExportRelaxCycles
	if relaxCycles <= 0 {
		relaxCycles = 5
	}
	conservativeW := limits.exportLimitW * (1.0 - margin)

	// ── Inputs ────────────────────────────────────────────────────────────────
	// Signed net export at the meter: positive = exporting, negative = importing.
	signedNetExportW := math.NaN()
	if !math.IsNaN(netW) {
		signedNetExportW = -netW
	} else {
		signedNetExportW = 0
		for _, sol := range solar {
			signedNetExportW += sol.PowerW
		}
		for _, b := range batteries {
			signedNetExportW += math.Max(0, b.PowerW)
		}
		signedNetExportW -= evseW
	}
	actualExportW := math.Max(0, signedNetExportW)

	// Low-pass filter the measured export.  The meter and OCPP MeterValues update
	// on different cadences (5 s vs 10 s) and the Modbus battery poll is offset
	// from both; an unfiltered controller bites itself on every desync.
	// alpha = 0.4 → ~63 % settled in 2 ticks, ~95 % in 5 ticks.
	const filterAlpha = 0.4
	if math.IsNaN(o.expGuard.filteredExportW) {
		o.expGuard.filteredExportW = actualExportW
	} else {
		o.expGuard.filteredExportW = filterAlpha*actualExportW + (1-filterAlpha)*o.expGuard.filteredExportW
	}
	filteredExportW := o.expGuard.filteredExportW

	if filteredExportW <= conservativeW {
		o.expGuard.safeCount++
	} else {
		o.expGuard.safeCount = 0
	}

	// Measured battery absorption *before* we issue any commands this tick.
	measuredBatteryAbsorbW := 0.0
	for _, b := range batteries {
		if b.Connected && b.PowerW < 0 {
			measuredBatteryAbsorbW += -b.PowerW
		}
	}

	// Detect an active EV early so we can (a) drop stale evCmdW state if the
	// session has ended and (b) decide which value to use in the conservation
	// identity below.  The full EV control block re-uses this pointer.
	var ev *EVSEState
	for i := range evses {
		if evses[i].Connected && evses[i].SessionActive &&
			!hasEVSECommand(plan.EVSECommands, evses[i].StationID, evses[i].ConnectorID) {
			ev = &evses[i]
			break
		}
	}
	if ev == nil {
		o.expGuard.evSetpointA = math.NaN()
		o.expGuard.evCmdW = math.NaN()
	}

	// Conservation identity for unconstrained export (= solar − home_load):
	//   signedNetExportW + batteryAbsorbW + evW
	// All three terms must reflect the same instant.  In practice the SunSpec
	// meter and battery poll at ~1 s but OCPP MeterValues lag ~10 s — so right
	// after we command a new EV current, signedNetExportW already shows the new
	// draw while measured evseW still reports the old current.  That mismatch
	// inflates unconstrainedExportW, the pre-flight check below thinks the hard
	// limit will be breached, and ratchets the EV up by 15-20 A — driving the
	// site from a steady export into a multi-kW import.
	//
	// Once we have prior commanded values, devices have settled to them by the
	// next 15 s tick.  Use the commands so the three terms are consistent.
	// On the first tick of an episode (no prior commands), fall back to the
	// measured values, which are mutually consistent in pre-event steady state.
	identityBattW := measuredBatteryAbsorbW
	if !math.IsNaN(o.expGuard.batteryAbsorbW) {
		identityBattW = o.expGuard.batteryAbsorbW
	}
	identityEvW := evseW
	if !math.IsNaN(o.expGuard.evCmdW) {
		identityEvW = o.expGuard.evCmdW
	}
	unconstrainedExportW := signedNetExportW + identityBattW + identityEvW

	// Hard cap: solar − home_load can never exceed total solar generation.
	// Defends against any residual measurement skew slipping past the
	// commanded-value substitution above.
	totalSolarW := 0.0
	for _, sol := range solar {
		if sol.Connected {
			totalSolarW += sol.PowerW
		}
	}
	if unconstrainedExportW > totalSolarW {
		unconstrainedExportW = totalSolarW
	}

	// ── Battery: command this tick's absorption with SOC-taper handoff ───────
	// The battery is the workhorse and runs at its taper-adjusted max charge
	// power.  The EV (below) is computed against the battery's PREDICTED
	// next-tick contribution, not its current one, so the EV ramps up
	// *before* the battery ramps down.  Net effect: smooth handoff with no
	// momentary spike on either device.
	const (
		socTaperStart = 80.0 // begin SOC-driven battery taper here
	)
	// socStepEstimate is how much SOC is expected to climb per optimizer
	// tick when the battery is charging at its full MaxChargeW.  Calibrated
	// for the 20× demo (10 kWh / 5 kW pack, 3 s tick ≈ 0.83 %); at the 15 s
	// production tick the true value is ~0.2 %, so this overestimates and
	// the EV pre-positions slightly early.  That errs conservative (EV
	// absorbs sooner than strictly needed) and self-corrects on the next
	// tick, so a constant in the right ballpark is acceptable here.
	const socStepEstimate = 1.0

	batteryAbsorbW := 0.0          // commanded absorption this tick (positive watts)
	predictedBatteryAbsorbW := 0.0 // expected absorption next tick (positive watts)
	for i, b := range batteries {
		if !b.Connected || hasBatteryCommand(plan.BatteryCommands, b.Name) {
			continue
		}
		if !math.IsNaN(b.SOC) && b.SOC >= socFull {
			continue
		}
		if b.MaxChargeW < 50 {
			continue
		}
		taperFactor := func(soc float64) float64 {
			if math.IsNaN(soc) || soc <= socTaperStart {
				return 1.0
			}
			if soc >= socFull || socFull <= socTaperStart {
				return 0.0
			}
			return math.Max(0, (socFull-soc)/(socFull-socTaperStart))
		}
		effectiveMaxNow := b.MaxChargeW * taperFactor(b.SOC)
		nextSOC := b.SOC + socStepEstimate
		effectiveMaxNext := b.MaxChargeW * taperFactor(nextSOC)

		need := math.Max(0, unconstrainedExportW-conservativeW)
		absorb := math.Min(effectiveMaxNow, need)
		predictedNext := math.Min(effectiveMaxNext, need)

		// Ratchet against transient meter noise.  Taper-driven drops bypass
		// the ratchet — they are real, monotonic, and the EV is being
		// pre-positioned to compensate.
		if !math.IsNaN(o.expGuard.batteryAbsorbW) && o.expGuard.batteryAbsorbW > absorb {
			if absorb < effectiveMaxNow {
				if o.expGuard.safeCount < relaxCycles {
					absorb = math.Min(o.expGuard.batteryAbsorbW, effectiveMaxNow)
				} else {
					absorb = math.Min((absorb+o.expGuard.batteryAbsorbW)/2, effectiveMaxNow)
				}
			}
		}

		if absorb < 50 {
			continue
		}
		setpoint := -absorb
		plan.BatteryCommands = append(plan.BatteryCommands, BatteryCommand{
			Name:      b.Name,
			SetpointW: setpoint,
		})
		plan.AddDecision("csip/export-limit",
			fmt.Sprintf("export limit %.0fW (target ≤%.0fW); unconstrained %.0fW; battery %s absorbs %.0fW (next %.0fW)",
				limits.exportLimitW, conservativeW, unconstrainedExportW, b.Name, absorb, predictedNext),
			fmt.Sprintf("battery %s → %.0fW", b.Name, setpoint))
		batteries[i].PowerW = setpoint
		batteryAbsorbW += absorb
		predictedBatteryAbsorbW += predictedNext
		surplusW -= absorb
	}
	if batteryAbsorbW > 0 {
		o.expGuard.batteryAbsorbW = batteryAbsorbW
	} else {
		// No battery command this tick (battery full, too small, or no need):
		// the restore rule will idle it, so clear the guard.  Holding a stale
		// value here would re-inflate unconstrainedExportW on the next tick
		// with absorption that no longer exists.
		o.expGuard.batteryAbsorbW = math.NaN()
	}

	// ── Closed-loop battery-absorption convergence ───────────────────────────
	// The feed-forward ceiling below credits the battery's PREDICTED absorption to
	// keep the inverter uncurtailed while the pack soaks up the surplus.  If a
	// commanded charge never materialises at the meter — a pack that ACKs the
	// setpoint but refuses to charge (audit: battery-charge-disabled) — that credit
	// is fictitious and the site over-exports with no correction, because the
	// controller believes the surplus is being absorbed.
	//
	// Detect a sustained gap between the commanded absorption and what the meter
	// actually shows the battery drawing, and once it persists, credit only the
	// MEASURED draw.  The feed-forward then curtails the inverter to hold the cap.
	// PV curtailment is always available for an export cap, so this is full
	// compliance — not a CannotComply.  A normal charge ramp closes the gap within
	// a tick or two (measured rises to the setpoint), so battBreachTicks rides it
	// out; only a battery that will not absorb trips the discredit.
	// Leaky counter (mirrors checkGenLimitConvergence): a single noisy tick where
	// measured absorption momentarily catches up must not zero a climbing stall and
	// force the whole battBreachTicks count to restart, or the curtail never engages
	// before the breach window ends. Decrement instead, capped at the threshold so
	// the credit is restored quickly once the pack genuinely starts absorbing.
	battStallThreshold := o.scaleTicks(battBreachTicks)
	if batteryAbsorbW > complianceBreachW && measuredBatteryAbsorbW < batteryAbsorbW*battConvergeFrac {
		if o.expGuard.battStallTicks < battStallThreshold {
			o.expGuard.battStallTicks++
		}
	} else if o.expGuard.battStallTicks > 0 {
		o.expGuard.battStallTicks--
	}
	if o.expGuard.battStallTicks >= battStallThreshold {
		predictedBatteryAbsorbW = measuredBatteryAbsorbW
		plan.AddDecision("csip/export-limit",
			fmt.Sprintf("battery commanded %.0fW absorb but measured only %.0fW for %d ticks (~%.0fs)",
				batteryAbsorbW, measuredBatteryAbsorbW, o.expGuard.battStallTicks, float64(o.expGuard.battStallTicks)*o.tickSeconds()),
			"battery not absorbing — curtailing solar to hold the export cap instead")
	}

	// ── EV: trim the residual with a filtered P-controller ───────────────────
	// `ev` was located earlier so the conservation identity could detect a
	// stale session.
	if ev != nil {
		voltage := ev.VoltageV
		if voltage <= 0 {
			voltage = 230.0
		}
		const (
			minChargeA  = 6.0 // IEC 61851-1 minimum AC charge current
			deadbandA   = 0.5 // hold the setpoint within 0.5 A of target
			maxTightenA = 2.0 // ~460 W/tick — matched to typical battery taper rate
			maxRelaxA   = 1.0 // half-rate when backing off
		)

		// EV target is computed against the battery's PREDICTED next-tick
		// absorption, not the current one.  This pre-positions the EV so
		// that *when* the battery's taper actually reduces its charge on
		// the next tick, the EV is already absorbing the corresponding
		// extra surplus — no transient over-export and no transient slam.
		residualNeed := unconstrainedExportW - predictedBatteryAbsorbW - conservativeW
		targetA := math.Min(math.Max(residualNeed/voltage, minChargeA), ev.MaxCurrentA)

		var newCurrentA float64
		var reason string

		// Always start at the IEC minimum on the first tick of an episode.
		// The slew bounds the ramp from there, so the EV cannot slam on at
		// session start no matter what the steady-state target works out to.
		if math.IsNaN(o.expGuard.evSetpointA) {
			newCurrentA = minChargeA
			reason = fmt.Sprintf(
				"first tick of export-limit episode; soft-start EV at %.1fA (steady-state target %.1fA)",
				newCurrentA, targetA)
		} else {
			diffA := targetA - o.expGuard.evSetpointA
			switch {
			case math.Abs(diffA) < deadbandA:
				newCurrentA = o.expGuard.evSetpointA
				reason = fmt.Sprintf(
					"holding EV at %.1fA (target %.1fA, battery now %.0fW → next %.0fW)",
					newCurrentA, targetA, batteryAbsorbW, predictedBatteryAbsorbW)
			case diffA > 0:
				step := math.Min(diffA, maxTightenA)
				newCurrentA = o.expGuard.evSetpointA + step
				reason = fmt.Sprintf(
					"target %.1fA (battery next %.0fW); ramp EV up by %.1fA",
					targetA, predictedBatteryAbsorbW, step)
			default:
				if o.expGuard.safeCount < relaxCycles {
					newCurrentA = o.expGuard.evSetpointA
					reason = fmt.Sprintf(
						"target %.1fA below %.1fA but only %d/%d safe cycles; hold",
						targetA, o.expGuard.evSetpointA, o.expGuard.safeCount, relaxCycles)
				} else {
					step := math.Max(diffA, -maxRelaxA)
					newCurrentA = math.Max(o.expGuard.evSetpointA+step, minChargeA)
					o.expGuard.safeCount = 0
					reason = fmt.Sprintf(
						"target %.1fA below %.1fA for ≥%d cycles; ramp EV down by %.1fA",
						targetA, o.expGuard.evSetpointA, relaxCycles, -step)
				}
			}
		}

		// Pre-flight: validate the (battery, EV) command pair against the
		// hard export limit before committing.  Using the conservation
		// identity, the export with these commands is
		//   predicted_export = unconstrained − battery_now − ev_command
		// If that would exceed the hard limit, tighten the EV further
		// (within its rating).  Anything still over the limit falls through
		// to the solar-curtailment branch below.
		predictedExportW := unconstrainedExportW - batteryAbsorbW - newCurrentA*voltage
		if predictedExportW > limits.exportLimitW {
			excessW := predictedExportW - limits.exportLimitW
			boost := math.Min(excessW/voltage, ev.MaxCurrentA-newCurrentA)
			if boost > 0 {
				newCurrentA += boost
				reason += fmt.Sprintf("; pre-flight: +%.1fA to stay under hard limit %.0fW",
					boost, limits.exportLimitW)
			}
		}

		plan.EVSECommands = append(plan.EVSECommands, EVSECommand{
			StationID:   ev.StationID,
			ConnectorID: ev.ConnectorID,
			MaxCurrentA: newCurrentA,
		})
		plan.AddDecision("csip/export-limit", reason,
			fmt.Sprintf("EVSE %s → %.1fA", ev.StationID, newCurrentA))
		o.expGuard.evSetpointA = newCurrentA
		o.expGuard.evCmdW = newCurrentA * voltage
		surplusW -= newCurrentA * voltage
	}

	// ── Solar curtailment: sticky integrating controller on MEASURED export ──
	// Curtailment is the only remedy a full battery / full EV can't undermine,
	// so it is driven purely by the measured export — never by commanded EV
	// absorption a full or throttled EV may not actually draw.  Crediting
	// commanded-but-undrawn EV power is what let a plugged-in-but-full EV mask an
	// over-export and defeat curtailment entirely.
	//
	// The commanded generation ceiling is held in the guard and re-issued every
	// tick while curtailment is active, so the restore rule can't un-curtail it
	// between ticks.  Each tick the ceiling is set to the value that lands
	// measured export on the conservative target:
	//
	//	ceiling = currentGeneration + (target − effectiveExport)
	//
	// It reads the RAW measured export (actualExportW), not the low-passed value:
	// the filter's lag under the bench's measurement delay made the controller
	// under-curtail (settling well above the cap) on a falling load.  Raw export
	// and the directly-read generation come from the same state snapshot, so the
	// ceiling converges in ~1 tick. effectiveExport credits only the *additional*
	// battery absorption commanded this tick (a reliable single Modbus write); the
	// EV is never credited here — crediting commanded-but-undrawn EV power is what
	// let a full EV mask an over-export.
	newBatteryAbsorbW := math.Max(0, batteryAbsorbW-measuredBatteryAbsorbW)
	effectiveExportW := actualExportW - newBatteryAbsorbW

	totalNameplateW := 0.0
	for _, sol := range solar {
		if sol.Connected {
			totalNameplateW += sol.MaxW
		}
	}

	// Ceiling controller.
	//
	// First tick of an episode (no prior ceiling): the generation read and the
	// export are both still in pre-curtailment steady state, so they are mutually
	// consistent — take a full one-step correction to the target for immediate
	// compliance:  ceiling = generation + (target − export) = target + load.
	//
	// Subsequent ticks: integrate from our own PREVIOUSLY COMMANDED ceiling, not
	// the current generation read, with gain < 1.  The bench's grid meter derives
	// net from an independent, lagged solar fetch, so the hub's directly-read
	// generation drops to a new ceiling several seconds before the meter reflects
	// it; a generation-anchored loop subtracts the still-high (stale) export from
	// the already-lowered generation, drives the ceiling negative, and collapses
	// output to zero.  Anchoring on our own last command — which the meter
	// eventually catches up to — and damping keeps it stable.
	const ceilGain = 0.5
	prevCeilingW := o.expGuard.solarCeilingW
	var desiredCeilingW float64
	if math.IsNaN(prevCeilingW) {
		desiredCeilingW = totalSolarW + (conservativeW - effectiveExportW)
	} else {
		desiredCeilingW = prevCeilingW + ceilGain*(conservativeW-effectiveExportW)
	}
	if desiredCeilingW < 0 {
		desiredCeilingW = 0
	}

	// Slew-limit the FEEDBACK ceiling change to damp hunting under the bench
	// meter's ~1-tick lag.  After we curtail, the linked metersim keeps reporting
	// the old (higher) export for a tick, so the raw error stays large and the
	// integrator would slam the ceiling to 0 W (over-curtail) and then, once the
	// meter catches up and momentarily under-reports, fling it back up into a
	// re-violation — the 5.0→0→climb→over hunt observed on tight caps (seed 99
	// day-0, 1.5 kW cap).  Tightening is allowed faster than relaxing: defend the
	// cap quickly, give generation back slowly.  Skipped on the first tick of an
	// episode (NaN prev) so onset still takes the full one-step correction.
	// The feed-forward term below is applied AFTER this and deliberately bypasses
	// the down-slew (see there).
	if !math.IsNaN(prevCeilingW) {
		// maxDropW/maxRiseW are a PHYSICAL ramp rate, not a fixed per-tick
		// allowance: scaleRateWPerTick keeps the wall-clock rate (500 W/s
		// tighten, ~166.7 W/s relax) constant across engine cadences instead
		// of silently slowing 5x at STOCK's 15 s tick vs the tuned 3 s tick.
		// Bit-identical to the historical 1500/500 W/tick constants at the
		// tuned (FAST) cadence — see scaleRateWPerTick's doc.
		maxDropW := o.scaleRateWPerTick(benchCeilingDropWPerTick) // tighten ≤1.5 kW/tick @ tunedTickInterval
		maxRiseW := o.scaleRateWPerTick(benchCeilingRiseWPerTick) // relax ≤0.5 kW/tick @ tunedTickInterval
		if desiredCeilingW < prevCeilingW-maxDropW {
			desiredCeilingW = prevCeilingW - maxDropW
		} else if desiredCeilingW > prevCeilingW+maxRiseW {
			desiredCeilingW = prevCeilingW + maxRiseW
		}
		if desiredCeilingW < 0 {
			desiredCeilingW = 0
		}
	}

	// Feed-forward proactive curtailment. The feedback controller above only
	// reacts to export it has already measured, so when the battery stops
	// absorbing — whether tapering near full SOC or fully saturated — generation
	// overshoots the cap and the slew-limited feedback chases it down a tick or
	// more late (the sustained midday export violations in the 92-day replay,
	// worst at the instant the pack hits SOCFullThreshold). Lead the curtailment:
	// bound the ceiling by the sinks that will actually exist next tick — home
	// load + the battery's PREDICTED absorption + commanded EV draw + the
	// conservative export headroom.
	//
	// Always applied, with NO gate: when the battery is ramping up its absorption
	// the predicted term is large so this evaluates to ≈ nameplate and never
	// bites (the tuned charging path is unchanged); it only tightens when the
	// battery won't absorb the surplus — including when it is already full, where
	// the battery loop has set the commanded absorption to 0 (the case a
	// "predicted < commanded" gate used to miss entirely). It also BYPASSES the
	// down-slew above: a saturation/taper-driven drop is deterministic, not
	// measured-feedback noise, so it is safe to apply in one tick — the same
	// reasoning the battery-absorb ratchet uses to let taper-driven drops through.
	homeSinkW := totalSolarW - actualExportW - measuredBatteryAbsorbW - evseW
	if homeSinkW < 0 {
		homeSinkW = 0
	}
	evCmdW := 0.0
	if !math.IsNaN(o.expGuard.evCmdW) {
		evCmdW = o.expGuard.evCmdW
	}
	feedForwardCeilingW := homeSinkW + predictedBatteryAbsorbW + evCmdW + conservativeW
	if feedForwardCeilingW < desiredCeilingW {
		desiredCeilingW = math.Max(0, feedForwardCeilingW)
	}

	// Sticky ceiling, never released to free-running mid-episode.  When the loop
	// computes a ceiling at or above nameplate it means no real curtailment is
	// needed this tick — but we CLAMP to nameplate and stay engaged rather than
	// releasing the guard (NaN).  Releasing dropped the inverter back to
	// free-running nameplate, and because the battery credit can hold
	// effectiveExport at/under the target while the pack absorbs, the re-engage
	// test then kept computing a ceiling ≥ nameplate and never re-curtailed — so
	// the inverter ran free at nameplate and the site over-exported by 1-2 kW for
	// the whole episode (the sustained midday violations in the 92-day replay).
	// Staying engaged means the NEXT tick integrates from nameplate against fresh
	// measured export and curtails immediately if still over cap.  A ceiling at
	// nameplate is a harmless no-op (the inverter clamps to min(potential,
	// ceiling)), so battery-first is preserved: when the battery absorbs the whole
	// surplus the ceiling sits at nameplate and solar runs full.  The guard is
	// reset to NaN only when the export limit itself clears (top of this func).
	if totalNameplateW > 0 {
		if desiredCeilingW > totalNameplateW {
			desiredCeilingW = totalNameplateW
		}
		o.expGuard.solarCeilingW = desiredCeilingW
		for _, sol := range solar {
			if !sol.Connected {
				continue
			}
			// Absolute per-inverter ceiling (share of nameplate).  Commanding an
			// absolute ceiling lets the inverter both RAISE and lower output; a
			// fraction of *current* output is a one-way ratchet that collapses
			// generation to zero and can never recover.
			curtailTo := desiredCeilingW * (sol.MaxW / totalNameplateW)
			plan.SolarCommands = append(plan.SolarCommands, SolarCommand{
				Name:       sol.Name,
				CurtailToW: curtailTo,
			})
			plan.AddDecision("csip/export-limit",
				fmt.Sprintf("holding generation ≤ %.0fW to keep export ≤ %.0fW (measured %.0fW)",
					desiredCeilingW, limits.exportLimitW, actualExportW),
				fmt.Sprintf("solar %s %.0fW → %.0fW", sol.Name, sol.PowerW, curtailTo))
		}
	}

	// Compliance breach: generation is curtailed essentially to zero yet the
	// site still exports over the hard limit (e.g. an islanded surplus with no
	// load and a full battery). Curtailing to zero is the last lever, so report
	// it upstream rather than silently missing the cap.
	//
	// DEBOUNCED (audit D3): the feed-forward ceiling term can drive desiredCeilingW
	// to ~0 in ONE tick while the meter still lags 1-2 ticks behind, so an
	// undebounced check posts a spurious CannotComply on any brief meter-noise
	// excursion under a tight (e.g. 0 W) cap — where the converged ceiling sits
	// permanently adjacent to the trip. Accumulate the no-lever condition with the
	// same leaky counter as expOverTicks: a 1-2-tick transient decays; a genuine
	// no-lever episode escalates within exportBreachTicks. recordBreach keeps the
	// worst, so this never double-reports with checkExportLimitConvergence.
	threshold := o.scaleTicks(exportBreachTicks)
	noLever := actualExportW > limits.exportLimitW+complianceBreachW && desiredCeilingW <= complianceBreachW
	if noLever {
		if o.expZeroLeverTicks < threshold {
			o.expZeroLeverTicks++
		}
	} else if o.expZeroLeverTicks > 0 {
		o.expZeroLeverTicks--
	}
	if o.expZeroLeverTicks >= threshold {
		o.recordBreach(plan, &ComplianceBreach{
			LimitType:  "export",
			LimitW:     limits.exportLimitW,
			MeasuredW:  actualExportW,
			ShortfallW: actualExportW - limits.exportLimitW,
			Reason:     "generation curtailed to minimum; battery and EV cannot absorb the surplus",
		})
	}

	return batteries, surplusW
}

// exportBreachTicks is how many ticks measured export may stay over the cap
// before the hub reports a CannotComply. Same value and leaky accumulation as
// genBreachTicks/importBreachTicks: long enough to ride out the ceiling
// controller's normal 1–2-tick convergence (one-step correction + meter lag),
// short enough that the detect→alert→northbound→gridsim CannotComply chain
// completes inside a tight cap window (the 21–49 s overshoots in QA 2026-07-03
// control-churn/clock-jitter would be detected ~9 s in).
const exportBreachTicks = 3

// checkExportLimitConvergence is the measured-effect backstop for an export cap.
// applyExportLimitRule commands battery absorption, EV draw, and a solar ceiling
// — and cross-checks each lever — but its only breach report fires when the
// ceiling is already at essentially zero and the site is STILL over the cap (no
// lever left). A site that never converges to a ceiling commanded WITH room to
// spare (a device that ACKs but ignores, or control churn that keeps the loop
// re-starting) stayed over the cap indefinitely with no CannotComply (QA
// 2026-07-03: control-churn, clock-jitter — 6/21 FAILs). Mirror
// checkGenLimitConvergence/checkImportConvergence: compare MEASURED export from
// the grid meter against the CSIP-visible limit (not the commanded ceiling — the
// grid server cares about the constraint, not our controller internals) and post
// a CannotComply after exportBreachTicks sustained over-cap ticks.
//
// The counter (expOverTicks) is session-scoped: it resets when the export limit
// clears entirely, NOT on cap value changes — see the field comment for why a
// controller-cadence or tolerance-band reset can never fire under control-churn.
// Skipping the per-value reset is also behaviourally identical to gen-limit's
// new-cap reset whenever the site was compliant under the outgoing cap (the
// counter sits at zero, so a mid-episode tighten still gets the full ramp
// allowance); the only divergence is a site already in violation across a
// rewrite, which keeps escalating — exactly the churn fault. Clock-offset
// changes never touch the counter: the reset keys off limits.exportLimitW
// alone, and the scheduler's fail-closed hold plus the hub's expiry-confirm
// debounce keep the limit from flapping to NaN under spec-legal jitter.
func (o *DefaultOptimizer) checkExportLimitConvergence(exportLimitW, netW float64, plan *Plan) {
	if math.IsNaN(exportLimitW) {
		o.expOverTicks = 0 // cap cleared — compliance session over
		return
	}
	// A meter-blind tick (netW NaN) is evidence of nothing: neither breach nor
	// compliance. Hold the counter, matching checkImportConvergence, so a blind
	// tick inside a sustained breach doesn't restart the escalation.
	if math.IsNaN(netW) {
		return
	}
	exportW := math.Max(0, -netW)
	// Leaky counter, matching the gen/import checks: an under-cap sample (meter
	// blip, momentary convergence) decrements instead of zeroing, so a sustained
	// breach with occasional noise still escalates while genuine convergence
	// drains the counter within a few ticks.
	threshold := o.scaleTicks(exportBreachTicks)
	if exportW > exportLimitW+complianceBreachW {
		if o.expOverTicks < threshold {
			o.expOverTicks++ // cap at the threshold so it drains fast on recovery
		}
	} else if o.expOverTicks > 0 {
		o.expOverTicks--
	}
	if o.expOverTicks >= threshold && plan.Breach == nil {
		o.recordBreach(plan, &ComplianceBreach{
			LimitType:  "export",
			LimitW:     exportLimitW,
			MeasuredW:  exportW,
			ShortfallW: exportW - exportLimitW,
			Reason:     "export remains over the cap after correction was commanded — the site is not converging to the limit",
		})
		plan.AddDecision("csip/export-limit",
			fmt.Sprintf("export %.0fW still over cap %.0fW after %d ticks (~%.0fs)",
				exportW, exportLimitW, o.expOverTicks, float64(o.expOverTicks)*o.tickSeconds()),
			"reporting CannotComply — site not converging to the export limit")
	}
}

// applyGenLimitRule enforces an absolute generation cap (CSIP OpModMaxLimW) by
// curtailing the inverters so total solar output stays at or below the limit.
//
// DISAMBIGUATION (WP-11): this rule enforces opModMaxLimW — a cap on inverter
// OUTPUT alone, which is why its only lever is the solar ceiling and battery
// discharge never enters its arithmetic. The CSIP-AUS opModGenLimW GROSS
// generation cap (solar output PLUS battery discharge) is a different axis
// with a different rule: applyAusGenerationLimitRule (auslimits.go).
//
// A generation cap limits inverter OUTPUT; only curtailing the inverter can
// satisfy it (battery absorption merely hides the over-generation behind a
// lower meter export). Runs every tick — CSIP compliance cannot be skipped —
// and reconciles with any curtailment the export-limit rule already issued by
// keeping the tighter of the two.
//
// The ceiling is an ABSOLUTE value (maxLimitW, distributed by nameplate) and is
// re-issued every tick while the cap is active — even when the live reading is
// already within the cap.  It must be sticky: if we only curtailed when the
// reading exceeds the cap, the restore rule would un-curtail the inverter on the
// next tick (reading now at the cap), generation would jump back to full
// nameplate, and output would oscillate across the cap every tick — ~50% of
// gen-limit ticks violating.  The inverter clamps output to min(potential,
// ceiling), so commanding the ceiling can never push generation up; when
// potential is below the cap the command is a harmless no-op.
func applyGenLimitRule(solar []SolarState, maxLimitW float64, plan *Plan) {
	if math.IsNaN(maxLimitW) {
		return
	}
	totalNameplateW := 0.0
	for _, sol := range solar {
		if sol.Connected {
			totalNameplateW += sol.MaxW
		}
	}
	if totalNameplateW <= 0 {
		return
	}

	curtailed := 0
	for _, sol := range solar {
		if !sol.Connected {
			continue
		}
		curtailTo := maxLimitW * (sol.MaxW / totalNameplateW)
		if i := solarCommandIndex(plan.SolarCommands, sol.Name); i >= 0 {
			// Already curtailed (e.g. for an export limit): keep the tighter cap.
			if math.IsNaN(plan.SolarCommands[i].CurtailToW) || curtailTo < plan.SolarCommands[i].CurtailToW {
				plan.SolarCommands[i].CurtailToW = curtailTo
			}
		} else {
			plan.SolarCommands = append(plan.SolarCommands, SolarCommand{Name: sol.Name, CurtailToW: curtailTo})
		}
		curtailed++
	}
	plan.AddDecision("csip/gen-limit",
		fmt.Sprintf("generation cap %.0fW (held continuously)", maxLimitW),
		fmt.Sprintf("ceiling %d inverters to ≤ %.0fW total", curtailed, maxLimitW))
}

// checkGenLimitConvergence verifies the inverters actually honoured the
// generation cap that applyGenLimitRule just commanded. Commanding the ceiling
// is the only lever for a generation limit, so if the MEASURED output stays
// over the cap for genBreachTicks consecutive ticks, the command is not taking
// effect at the device — it ACKed the Modbus write but did not act on it, or
// rejected it. Rather than asserting a compliance the meter contradicts, record
// a breach so the hub POSTs a 2030.5 CannotComply. This is the measured-effect
// half of closed-loop actuation: applyGenLimitRule issues the command, this
// confirms the device converged.
//
// A short overage is normal (the inverter ramps down over a tick or two), so the
// breach is gated on a sustained miss. The guard resets when the cap changes or
// clears.
func (o *DefaultOptimizer) checkGenLimitConvergence(solar []SolarState, batteries []BatteryState, netW, maxLimitW float64, plan *Plan) {
	if math.IsNaN(maxLimitW) {
		o.genGuard = genGuard{activeLimitW: math.NaN()} // cap cleared — reset
		return
	}
	// Reset the breach counter only when the cap changes MEANINGFULLY. The decoded
	// cap can vary by a hair tick-to-tick (the watts→ActivePower value×10^mult
	// round-trip through the bus), and resetting on a bit-exact inequality zeroed
	// overCount every tick so a sustained breach never reached genBreachTicks —
	// part of the reject-write/enable-gate-curtail nondeterminism. Track the cap
	// session by a tolerance band and follow minor drift instead.
	if math.IsNaN(o.genGuard.activeLimitW) || math.Abs(maxLimitW-o.genGuard.activeLimitW) > complianceBreachW {
		o.genGuard = genGuard{activeLimitW: maxLimitW} // new cap session — reset
	} else {
		o.genGuard.activeLimitW = maxLimitW // same session; track sub-threshold drift
	}

	measuredGenW := 0.0
	for _, sol := range solar {
		if sol.Connected && sol.PowerW > 0 {
			measuredGenW += sol.PowerW
		}
	}

	// Independent generation floor from the grid meter. The inverter's
	// self-reported power can be corrupted by the same fault that ignores the
	// curtailment — a device that echoes the commanded limit back while still
	// generating full output (audit: enable-gate-curtail) reports a compliant
	// power even though it is not. The meter is independent: from the site energy
	// balance, generation = export + load + evse + batteryCharge − batteryDischarge,
	// and load/evse/batteryCharge are all ≥ 0, so generation ≥ export −
	// batteryDischarge regardless of what the inverter claims. Use this lower bound
	// so an echoed-but-ignored limit is still caught.
	if !math.IsNaN(netW) {
		exportW := math.Max(0, -netW)
		batteryDischargeW := 0.0
		for _, b := range batteries {
			if b.Connected && b.PowerW > 0 {
				batteryDischargeW += b.PowerW
			}
		}
		if floor := exportW - batteryDischargeW; floor > measuredGenW {
			measuredGenW = floor
		}
	}

	// Leaky counter, not a hard consecutive run: a single sub-threshold sample —
	// an HIL meter blip, a momentary inverter dip — must not zero a climbing breach
	// and force the whole genBreachTicks count to restart (the marginal-timing half
	// of the reject-write/enable-gate-curtail nondeterminism, where the breach
	// window barely affords genBreachTicks consecutive ticks). Decrement on an
	// under-cap tick so a sustained breach with occasional noise still escalates,
	// while a genuine convergence still drains the counter to zero within a few ticks.
	threshold := o.scaleTicks(genBreachTicks)
	if measuredGenW > maxLimitW+complianceBreachW {
		if o.genGuard.overCount < threshold {
			o.genGuard.overCount++ // cap at the threshold so it drains fast on recovery
		}
	} else if o.genGuard.overCount > 0 {
		o.genGuard.overCount--
	}

	if o.Debug {
		log.Printf("[optimizer] gen-converge: cap=%.0f measuredGen=%.0f overCount=%d/%d",
			maxLimitW, measuredGenW, o.genGuard.overCount, threshold)
	}

	if o.genGuard.overCount >= threshold {
		o.recordBreach(plan, &ComplianceBreach{
			LimitType:  "generation",
			LimitW:     maxLimitW,
			MeasuredW:  measuredGenW,
			ShortfallW: measuredGenW - maxLimitW,
			Reason:     "inverter output remains above the generation cap after curtailment was commanded — the device is not honouring the command",
		})
		plan.AddDecision("csip/gen-limit",
			fmt.Sprintf("generation %.0fW still over cap %.0fW after %d ticks (~%.0fs)",
				measuredGenW, maxLimitW, o.genGuard.overCount, float64(o.genGuard.overCount)*o.tickSeconds()),
			"reporting CannotComply — inverter not converging to the commanded ceiling")
	}
}

// importBreachTicks is how many ticks measured grid import may stay over the
// cap before the hub reports a CannotComply. Mirrors genBreachTicks — same
// value, same leaky accumulation: long enough to ride out a battery discharge
// ramp / EV soft-stop, short enough to surface a load the hub cannot actually
// shed — a charger that ACKs a current-limit profile or suspend but keeps
// drawing (audit: ev-accept-but-ignore). Was 5; at 5 the CannotComply lost a
// 40–50% timing race against the scenario hold window on the HIL bench
// (QA 2026-07-01: battery-soc-refuse), and an unmeetable import cap deserves
// the same escalation latency as an unmeetable generation cap.
const importBreachTicks = 3

// checkImportConvergence is the measured-effect backstop for an import cap.
// applyImportLimitRule (battery discharge) and applyEVChargingRule (EV suspend)
// issue commands and the EV rule trusts the OCPP "Accepted"; if the load ignores
// the command the meter stays over the cap with no admission. When MEASURED
// import stays over the cap for importBreachTicks consecutive ticks and no rule
// already recorded a breach this tick, post a CannotComply so the grid server
// learns the cap is unmet rather than the hub asserting a compliance the meter
// contradicts. Unlike an export cap (always satisfiable by curtailing PV), an
// import cap can be genuinely unmeetable, so CannotComply is the correct outcome.
func (o *DefaultOptimizer) checkImportConvergence(importLimitW, netW float64, plan *Plan) {
	if math.IsNaN(importLimitW) {
		o.impGuard.breachTicks = 0 // cap cleared — reset
		return
	}
	// A meter-blind tick (netW NaN — e.g. the worldMoving gate excluded a frozen
	// meter) is evidence of nothing: neither breach nor compliance. Hold the
	// counter rather than resetting it, or a single blind tick inside a sustained
	// breach silently restarts the whole escalation and the CannotComply loses the
	// race against the constraint window (QA 2026-07-01: battery-soc-refuse).
	if math.IsNaN(netW) {
		return
	}
	importW := math.Max(0, netW)
	// Leaky counter, matching checkGenLimitConvergence: a single under-cap sample
	// (meter blip, momentary load dip) decrements instead of zeroing, so a
	// sustained breach with occasional noise still escalates while genuine
	// convergence drains the counter within a few ticks.
	threshold := o.scaleTicks(importBreachTicks)
	if importW > importLimitW+complianceBreachW {
		if o.impGuard.breachTicks < threshold {
			o.impGuard.breachTicks++ // cap at the threshold so it drains fast on recovery
		}
	} else if o.impGuard.breachTicks > 0 {
		o.impGuard.breachTicks--
	}
	if o.impGuard.breachTicks >= o.scaleTicks(importBreachTicks) && plan.Breach == nil {
		o.recordBreach(plan, &ComplianceBreach{
			LimitType:  "import",
			LimitW:     importLimitW,
			MeasuredW:  importW,
			ShortfallW: importW - importLimitW,
			Reason:     "import remains over the cap after all levers — a load is not honouring the hub's curtailment/suspend command",
		})
		plan.AddDecision("csip/import-limit",
			fmt.Sprintf("import %.0fW still over cap %.0fW after %d ticks (~%.0fs)",
				importW, importLimitW, o.impGuard.breachTicks, float64(o.impGuard.breachTicks)*o.tickSeconds()),
			"reporting CannotComply — load not responding to curtailment")
	}
}

// batteryReserveDrainTicks is how many consecutive ticks a pack may read as
// discharging at/below its SOC reserve before the hub force-disconnects it. No
// rule ever commands a discharge below the reserve, so a sustained reserve-drain
// means the device is inverting or ignoring its setpoint (audit:
// battery-wrong-sign — a commanded charge executed as a discharge). Confirm over
// a few ticks to ride out a single telemetry glitch.
const batteryReserveDrainTicks = 3

// checkBatterySafety is a measured-effect safety backstop on battery direction.
// checkBatterySafety detects two fault modes and force-disconnects the pack:
//
//  1. Reserve drain: the rules never discharge at/below SOC reserve, so measured
//     discharge there for batteryReserveDrainTicks ticks means the pack is not
//     honouring its command. Trusting the command would walk the battery to empty.
//
//  2. Wrong direction: when the hub commanded charge (negative setpoint) but the
//     pack measures as discharging for batteryReserveDrainTicks ticks, the sign
//     convention is inverted — the pack will discharge regardless of SOC level.
//     Disconnecting is safer than letting it drain uncontrolled (audit: battery-wrong-sign).
//
// Runs after applyRestoreRule so it overrides the idle/reconnect that rule issues.
// criticalBatteryInversion reports the unambiguous, act-now battery fault: a pack
// commanded to charge but measured discharging (>complianceBreachW) while at/near
// its SOC reserve (≤ reserve+5%). No correct command produces this, and at a full
// discharge the pack crosses the reserve floor in seconds — so it warrants an
// immediate disconnect with no debounce, on whichever loop observes it first
// (audit: battery-wrong-sign).
func criticalBatteryInversion(powerW, soc, socReserve float64, chargeCommanded bool) bool {
	return chargeCommanded && powerW > complianceBreachW &&
		!math.IsNaN(soc) && soc <= socReserve+5
}

// chargeCommandedFor reports whether the pack is currently commanded to charge —
// from this tick's plan if present (economic tick), else from the last commanded
// setpoint (fast protection tick, which has no fresh plan).
func (o *DefaultOptimizer) chargeCommandedFor(name string, plan *Plan) bool {
	if i := batteryCommandIndex(plan.BatteryCommands, name); i >= 0 {
		return !math.IsNaN(plan.BatteryCommands[i].SetpointW) && plan.BatteryCommands[i].SetpointW < 0
	}
	if o.lastBattCmd != nil {
		if sp, ok := o.lastBattCmd[name]; ok {
			return sp < 0
		}
	}
	return false
}

// EvaluateSafety is the Tier-1 fast protection pass (ADR-0001). It runs between
// economic ticks on a short cadence and issues ONLY the immediate, unambiguous
// protective disconnects (critical sign-inversion at reserve), so a mis-wired pack
// is ceased in ~1 tick instead of waiting a full economic interval — which at the
// 15 s stock cadence is far too slow for a reserve-floor emergency. It is
// deliberately stateless (no debounce counters): the debounced reserve-drain /
// wrong-direction paths remain in checkBatterySafety on the economic tick. Runs on
// the same goroutine as Optimize, so it needs no lock.
func (o *DefaultOptimizer) EvaluateSafety(state SystemState) Plan {
	plan := Plan{Timestamp: state.Timestamp, Safety: true}
	empty := &Plan{} // no fresh economic plan on the fast tick
	for _, b := range state.Batteries {
		if !b.Connected {
			continue
		}
		chargeCommanded := o.chargeCommandedFor(b.Name, empty)
		if !criticalBatteryInversion(b.PowerW, b.SOC, o.SOCReserve, chargeCommanded) {
			continue
		}
		f := false
		plan.BatteryCommands = append(plan.BatteryCommands, BatteryCommand{
			Name: b.Name, SetpointW: 0, Connect: &f,
		})
		plan.AddDecision("safety/battery-direction-fast",
			fmt.Sprintf("battery %s commanded to charge but discharging %.0fW at SOC %.1f%% ≤ reserve+5%% — fast-loop immediate disconnect",
				b.Name, b.PowerW, b.SOC),
			"force-disconnecting the pack (Tier-1 fast protection loop)")
	}
	return plan
}

func (o *DefaultOptimizer) checkBatterySafety(batteries []BatteryState, plan *Plan) {
	if o.battDrainTicks == nil {
		o.battDrainTicks = make(map[string]int)
	}
	if o.battWrongDirTicks == nil {
		o.battWrongDirTicks = make(map[string]int)
	}
	for _, b := range batteries {
		if !b.Connected || math.IsNaN(b.SOC) {
			delete(o.battDrainTicks, b.Name)
			delete(o.battWrongDirTicks, b.Name)
			continue
		}

		// Check 1: reserve drain. Gate on the LATCHED reserve state, not the
		// instantaneous SOC: telemetry that dithers ±1pt across the reserve line
		// would otherwise flip this gate every tick and the counter could never
		// accumulate to the trip (audit B-2). The latch (updateReserveHolds, run
		// earlier in Optimize) is hold-biased, so a pack MEASURED discharging
		// throughout a reserve episode accumulates cleanly toward the disconnect.
		// The instantaneous check is folded in so the drain guard still works on
		// the fast/economic paths that call checkBatterySafety without a prior
		// updateReserveHolds. Reset (not decay) on a non-draining sample keeps the
		// single-glitch tolerance and lets a legitimate ramp-down clear before the
		// threshold — the latch, not decay, is what makes the trip reachable here.
		atReserve := o.battReserveHold[b.Name] || b.SOC <= o.SOCReserve
		dischargingAtReserve := b.PowerW > complianceBreachW && atReserve
		if dischargingAtReserve {
			o.battDrainTicks[b.Name]++
		} else {
			o.battDrainTicks[b.Name] = 0
		}

		// Check 2: wrong direction — commanded charge but measuring discharge.
		// Look at this tick's command to infer intent; measured PowerW reflects
		// the previous tick's state (one-tick lag is intentional: we want to see
		// whether the pack responded to the command, not whether we issued it).
		chargeCommanded := o.chargeCommandedFor(b.Name, plan)
		wrongDirection := chargeCommanded && b.PowerW > complianceBreachW
		if wrongDirection {
			o.battWrongDirTicks[b.Name]++
		} else {
			o.battWrongDirTicks[b.Name] = 0
		}

		// Critical fast path (audit: battery-wrong-sign): a pack commanded to
		// charge but measured discharging while already at/near its SOC reserve is
		// unambiguously inverting its setpoint AND about to sail through the reserve
		// floor — there is no benign explanation, so disconnect THIS tick without
		// spending the debounce budget. At a 4800 W discharge the pack crosses the
		// reserve in a few seconds, faster than the multi-tick debounce can react;
		// the tick paths below still cover the non-critical cases (wrong direction
		// at high SOC, slow reserve drain) where riding out a single telemetry
		// glitch is worth the small delay.
		threshold := o.scaleTicks(batteryReserveDrainTicks)
		criticalTrip := criticalBatteryInversion(b.PowerW, b.SOC, o.SOCReserve, chargeCommanded)
		drainTrip := o.battDrainTicks[b.Name] >= threshold
		wrongDirTrip := o.battWrongDirTicks[b.Name] >= threshold
		if !criticalTrip && !drainTrip && !wrongDirTrip {
			continue
		}

		// Force-disconnect, overriding any prior command (e.g. the restore idle).
		disconnect := false
		if i := batteryCommandIndex(plan.BatteryCommands, b.Name); i >= 0 {
			plan.BatteryCommands[i].SetpointW = 0
			plan.BatteryCommands[i].Connect = &disconnect
		} else {
			plan.BatteryCommands = append(plan.BatteryCommands, BatteryCommand{
				Name:      b.Name,
				SetpointW: 0,
				Connect:   &disconnect,
			})
		}
		reason := fmt.Sprintf("battery %s discharging %.0fW at SOC %.1f%% ≤ reserve %.0f%% for %d ticks (~%.0fs)",
			b.Name, b.PowerW, b.SOC, o.SOCReserve, o.battDrainTicks[b.Name], float64(o.battDrainTicks[b.Name])*o.tickSeconds())
		switch {
		case criticalTrip && !wrongDirTrip:
			reason = fmt.Sprintf("battery %s commanded to charge but discharging %.0fW at SOC %.1f%% ≤ reserve+5%% — critical sign inversion, immediate disconnect (no debounce)",
				b.Name, b.PowerW, b.SOC)
		case wrongDirTrip:
			reason = fmt.Sprintf("battery %s commanded to charge but discharging %.0fW (SOC %.1f%%) for %d ticks (~%.0fs) — sign inversion",
				b.Name, b.PowerW, b.SOC, o.battWrongDirTicks[b.Name], float64(o.battWrongDirTicks[b.Name])*o.tickSeconds())
		}
		plan.AddDecision("safety/battery-direction", reason,
			"force-disconnecting the pack — it is not honouring its charge command")
	}
}

// applySelfConsumptionRule routes solar surplus into connected batteries.
// Returns updated battery states and updated surplusW.
//
// When a battery is already charging and its current rate already covers the
// measured surplus (e.g. because the grid meter lags), the rule re-issues the
// current setpoint ("maintain") rather than escalating it each tick.  This
// prevents a runaway charge ramp when the meter reading is stale.
func applySelfConsumptionRule(batteries []BatteryState, surplusW, excessThreshold, socFull float64, plan *Plan) ([]BatteryState, float64) {
	for i, b := range batteries {
		if !b.Connected || !b.Energized {
			continue
		}
		if hasBatteryCommand(plan.BatteryCommands, b.Name) {
			continue
		}
		if !math.IsNaN(b.SOC) && b.SOC >= socFull {
			if surplusW > excessThreshold {
				plan.AddDecision("self-consumption",
					fmt.Sprintf("battery %s SOC=%.1f%% >= full threshold %.1f%%",
						b.Name, b.SOC, socFull),
					"skip charging — battery full")
			}
			continue
		}

		// How much is the battery already absorbing?
		alreadyAbsorbingW := 0.0
		if b.PowerW < 0 {
			alreadyAbsorbingW = -b.PowerW
		}

		// Additional surplus beyond what this battery is already absorbing.
		additionalSurplus := math.Max(0, surplusW-alreadyAbsorbingW)

		if additionalSurplus < excessThreshold {
			// Battery is already covering the surplus; re-issue current setpoint to
			// prevent the restore rule from clearing it, but do not escalate.
			if alreadyAbsorbingW > 0 && surplusW > 0 {
				plan.BatteryCommands = append(plan.BatteryCommands, BatteryCommand{
					Name:      b.Name,
					SetpointW: b.PowerW,
				})
				plan.AddDecision("self-consumption",
					fmt.Sprintf("%.0fW surplus absorbed by %.0fW charge; maintaining battery %s", surplusW, alreadyAbsorbingW, b.Name),
					fmt.Sprintf("battery %s holds %.0fW", b.Name, b.PowerW))
				batteries[i].PowerW = b.PowerW
				surplusW -= alreadyAbsorbingW
			}
			continue
		}

		// Absorb the additional surplus beyond the current charge rate.
		headroom := b.AvailableChargeW()
		absorb := math.Min(headroom, additionalSurplus)
		if absorb < 50 {
			// Battery at capacity — hold current rate so restore rule doesn't idle it.
			if alreadyAbsorbingW > 0 && surplusW > 0 {
				plan.BatteryCommands = append(plan.BatteryCommands, BatteryCommand{
					Name:      b.Name,
					SetpointW: b.PowerW,
				})
				plan.AddDecision("self-consumption",
					fmt.Sprintf("battery %s at capacity (%.0fW); holding while surplus %.0fW remains",
						b.Name, alreadyAbsorbingW, surplusW),
					fmt.Sprintf("battery %s holds %.0fW", b.Name, b.PowerW))
				surplusW -= alreadyAbsorbingW
				batteries[i].PowerW = b.PowerW
			}
			continue
		}
		newSetpoint := b.PowerW - absorb
		plan.BatteryCommands = append(plan.BatteryCommands, BatteryCommand{
			Name:      b.Name,
			SetpointW: newSetpoint,
		})
		plan.AddDecision("self-consumption",
			fmt.Sprintf("%.0fW solar surplus → charging battery %s", surplusW, b.Name),
			fmt.Sprintf("battery %s setpoint %.0fW", b.Name, newSetpoint))
		surplusW -= absorb + alreadyAbsorbingW
		batteries[i].PowerW = newSetpoint
	}
	return batteries, surplusW
}

// applyDemandResponseRule discharges batteries during DR events or TOU peak hours.
// Returns updated battery states and updated surplusW (discharge adds to surplus).
//
// maxDischargeW caps the total discharge commanded across all batteries so
// the rule cannot push site export over an active CSIP export limit while
// waiting for the export-limit rule's next-tick correction.  NaN = uncapped.
func applyDemandResponseRule(batteries []BatteryState, surplusW, socReserve float64, isDR, isPeak bool, peakReason string, maxDischargeW float64, dischargeBlocked func(BatteryState) bool, plan *Plan) ([]BatteryState, float64) {
	if !isDR && !isPeak {
		return batteries, surplusW
	}
	reason := "demand-response event active"
	if peakReason != "" {
		reason = peakReason
	}
	capped := !math.IsNaN(maxDischargeW)
	remainingW := maxDischargeW
	for i, b := range batteries {
		if !b.Connected || !b.Energized {
			continue
		}
		if dischargeBlocked(b) {
			plan.AddDecision("demand-response",
				fmt.Sprintf("battery %s SOC=%.1f%% at/near reserve minimum (hold latched)", b.Name, b.SOC),
				"skip discharge — protecting reserve")
			continue
		}
		available := b.AvailableDischargeW()
		if available < 50 {
			continue
		}
		setpoint := b.MaxDischargeW
		if capped {
			if remainingW < 50 {
				plan.AddDecision("demand-response",
					fmt.Sprintf("battery %s discharge withheld: export-limit headroom exhausted", b.Name),
					"skip discharge — protecting export limit")
				continue
			}
			setpoint = math.Min(setpoint, remainingW)
		}
		if !hasBatteryCommand(plan.BatteryCommands, b.Name) {
			plan.BatteryCommands = append(plan.BatteryCommands, BatteryCommand{
				Name:      b.Name,
				SetpointW: setpoint,
			})
			plan.AddDecision("demand-response",
				reason,
				fmt.Sprintf("discharging battery %s at %.0fW", b.Name, setpoint))
			surplusW += setpoint - b.PowerW
			batteries[i].PowerW = setpoint
			if capped {
				remainingW -= setpoint
			}
		}
	}
	return batteries, surplusW
}

// applyEVChargingRule distributes the available power budget across connected EVSEs.
//
// When an export limit is active and there is solar surplus but below the IEC 61851
// minimum 6 A, the rule supplements from grid to reach 6 A (provided import headroom
// allows), rather than suspending the session entirely.
//
// evImportSuppressed gates EV resumption while the import guard is cooling down:
// the EV must not charge until the site has demonstrated N consecutive ticks of
// stable positive import under the cap, preventing it from surging during the
// battery over-discharge transient.
func applyEVChargingRule(evses []EVSEState, limits gridConstraints, netW, solarW, surplusW float64, evImportSuppressed bool, plan *Plan) {
	const minChargeA = 6.0 // IEC 61851-1 minimum AC charge current

	for _, evse := range evses {
		if !evse.Connected || !evse.SessionActive {
			continue
		}
		// Skip EVSEs already commanded (e.g. by export-limit rule).
		if hasEVSECommand(plan.EVSECommands, evse.StationID, evse.ConnectorID) {
			continue
		}

		// Hold EV at zero while the import guard is cooling down.
		if evImportSuppressed {
			plan.EVSECommands = append(plan.EVSECommands, EVSECommand{
				StationID:   evse.StationID,
				ConnectorID: evse.ConnectorID,
				MaxCurrentA: 0,
			})
			plan.AddDecision("import-limit",
				fmt.Sprintf("EVSE %s suspended: import guard cooling down (need stable import ticks)",
					evse.StationID),
				"EVSE suspended during import-limit cooldown")
			continue
		}

		voltage := evse.VoltageV
		if voltage <= 0 {
			voltage = 230.0
		}
		maxPowerW := evse.MaxCurrentA * voltage
		minChargeW := minChargeA * voltage

		// Suspend if grid import is already at or above the limit.
		if !math.IsNaN(limits.importLimitW) && !math.IsNaN(netW) && netW >= limits.importLimitW {
			plan.EVSECommands = append(plan.EVSECommands, EVSECommand{
				StationID:   evse.StationID,
				ConnectorID: evse.ConnectorID,
				MaxCurrentA: 0,
			})
			plan.AddDecision("import-limit",
				fmt.Sprintf("grid import %.0fW at/above limit %.0fW; suspending EVSE %s",
					netW, limits.importLimitW, evse.StationID),
				"EVSE session suspended")
			continue
		}

		// No grid constraint active.  Default to full rate, but cap to the
		// available solar surplus (after this tick's battery command) when
		// solar is producing — otherwise we'd be importing from the grid to
		// charge the EV, defeating behind-the-meter PV.  Matters most during
		// the discovery gap: when a new export-limit event has been published
		// but the hub hasn't fetched it yet, the EV would otherwise slam to
		// full and create a several-second 3 kW import.
		//
		// surplusW is already net of measured EV consumption (see
		// computePowerBalance); add evse.PowerW back to size the new EV
		// command from the unconsumed budget.
		if math.IsNaN(limits.exportLimitW) && math.IsNaN(limits.importLimitW) {
			targetA := evse.MaxCurrentA
			reason := fmt.Sprintf("no grid constraint; charging EVSE %s at full %.1fA",
				evse.StationID, evse.MaxCurrentA)
			if solarW > 0 {
				evBudgetW := surplusW + evse.PowerW
				budgetA := evBudgetW / voltage
				if budgetA < targetA {
					targetA = math.Max(minChargeA, budgetA)
					reason = fmt.Sprintf("no grid constraint but solar budget %.0fW < EV max %.0fW; throttling EVSE %s to %.1fA to avoid grid import",
						evBudgetW, evse.MaxCurrentA*voltage, evse.StationID, targetA)
				}
			}
			plan.EVSECommands = append(plan.EVSECommands, EVSECommand{
				StationID:   evse.StationID,
				ConnectorID: evse.ConnectorID,
				MaxCurrentA: targetA,
			})
			plan.AddDecision("ev-charging", reason,
				fmt.Sprintf("EVSE %s at %.1fA", evse.StationID, targetA))
			continue
		}

		// Export limit active but site is currently importing (not exporting).
		// The export-limit rule found no excess to manage, so charge at full rate.
		// The export-limit rule re-engages automatically once export exceeds the limit.
		if !math.IsNaN(limits.exportLimitW) && !math.IsNaN(netW) && netW >= 0 {
			plan.EVSECommands = append(plan.EVSECommands, EVSECommand{
				StationID:   evse.StationID,
				ConnectorID: evse.ConnectorID,
				MaxCurrentA: evse.MaxCurrentA,
			})
			plan.AddDecision("ev-charging",
				fmt.Sprintf("export limit %.0fW active but site importing %.0fW; EVSE %s at full %.1fA",
					limits.exportLimitW, netW, evse.StationID, evse.MaxCurrentA),
				fmt.Sprintf("EVSE %s at %.1fA", evse.StationID, evse.MaxCurrentA))
			continue
		}

		if solarW > 0 && surplusW < maxPowerW {
			budgetW := math.Max(0, surplusW)

			// When an export limit is active and there is solar surplus but below minimum
			// charge rate, supplement from grid rather than suspending.
			if !math.IsNaN(limits.exportLimitW) && budgetW > 0 && budgetW < minChargeW {
				supplementW := minChargeW - budgetW
				importHeadroom := math.Inf(1) // unconstrained unless import limit set
				if !math.IsNaN(limits.importLimitW) && !math.IsNaN(netW) {
					importHeadroom = limits.importLimitW - netW
				}
				if supplementW <= importHeadroom {
					plan.EVSECommands = append(plan.EVSECommands, EVSECommand{
						StationID:   evse.StationID,
						ConnectorID: evse.ConnectorID,
						MaxCurrentA: minChargeA,
					})
					plan.AddDecision("ev-charging",
						fmt.Sprintf("%.0fW solar + %.0fW grid supplement → EVSE %s at %.0fA minimum",
							budgetW, supplementW, evse.StationID, minChargeA),
						fmt.Sprintf("EVSE %s at %.0fA", evse.StationID, minChargeA))
					continue
				}
				// Import limit would be violated; suspend.
				plan.EVSECommands = append(plan.EVSECommands, EVSECommand{
					StationID:   evse.StationID,
					ConnectorID: evse.ConnectorID,
					MaxCurrentA: 0,
				})
				plan.AddDecision("ev-charging",
					fmt.Sprintf("%.0fW solar insufficient and import limit prevents supplement; suspending EVSE %s",
						surplusW, evse.StationID),
					"EVSE suspended")
				continue
			}

			limitA := budgetW / voltage
			if limitA < minChargeA {
				plan.EVSECommands = append(plan.EVSECommands, EVSECommand{
					StationID:   evse.StationID,
					ConnectorID: evse.ConnectorID,
					MaxCurrentA: 0,
				})
				plan.AddDecision("ev-charging",
					fmt.Sprintf("insufficient solar surplus (%.0fW < min %.0fW); suspending EVSE %s",
						surplusW, minChargeW, evse.StationID),
					"EVSE suspended to minimise grid import")
			} else {
				limitA = math.Min(limitA, evse.MaxCurrentA)
				plan.EVSECommands = append(plan.EVSECommands, EVSECommand{
					StationID:   evse.StationID,
					ConnectorID: evse.ConnectorID,
					MaxCurrentA: limitA,
				})
				plan.AddDecision("ev-charging",
					fmt.Sprintf("solar surplus %.0fW → throttling EVSE %s to %.1fA",
						surplusW, evse.StationID, limitA),
					fmt.Sprintf("EVSE %s limited to %.1fA", evse.StationID, limitA))
				surplusW -= limitA * voltage
			}
		} else {
			plan.EVSECommands = append(plan.EVSECommands, EVSECommand{
				StationID:   evse.StationID,
				ConnectorID: evse.ConnectorID,
				MaxCurrentA: evse.MaxCurrentA,
			})
			plan.AddDecision("ev-charging",
				fmt.Sprintf("sufficient power available; charging EVSE %s at full %.1fA",
					evse.StationID, evse.MaxCurrentA),
				fmt.Sprintf("EVSE %s at %.1fA", evse.StationID, evse.MaxCurrentA))
		}
	}
}

// applyImportLimitRule discharges batteries to defend the CSIP import limit.
// Stateful: it ratchets discharge up immediately when import exceeds the hard
// limit, holds the commanded discharge across ticks (preventing
// applyRestoreRule from idling the battery), and ramps down only after the
// import has stayed inside the limit for ExportRelaxCycles consecutive ticks.
// Without this stickiness the system oscillates at the tick period as
// described in the demo S2 (import 1 kW → discharge 500 W → import 500 W →
// restore idles → import 1 kW → ...).
func (o *DefaultOptimizer) applyImportLimitRule(batteries []BatteryState, limits gridConstraints, netW, socReserve float64, plan *Plan) []BatteryState {
	if math.IsNaN(limits.importLimitW) {
		o.impGuard = importGuard{dischargeW: math.NaN(), activeLimitW: math.NaN()}
		return batteries
	}

	// New limit value → restart the guard fresh. "New" means a MEANINGFUL change:
	// the decoded cap can vary by a hair tick-to-tick (the watts→ActivePower
	// value×10^mult round-trip through the bus), and restarting on a bit-exact
	// inequality wipes the guard — including breachTicks — every tick, so a
	// sustained import breach never escalates to CannotComply (same failure the
	// gen guard fixed in checkGenLimitConvergence; QA 2026-07-01:
	// battery-soc-refuse). Track the cap session by a tolerance band and follow
	// minor drift instead.
	if math.IsNaN(o.impGuard.activeLimitW) || math.Abs(limits.importLimitW-o.impGuard.activeLimitW) > complianceBreachW {
		o.impGuard = importGuard{dischargeW: math.NaN(), activeLimitW: limits.importLimitW}
		// A limit that arrives while the site is already compliant must not
		// suspend the EV: the cooldown gate exists for recovery after a
		// violation, not for limit arrival.  Seed the resume gate as satisfied.
		if !math.IsNaN(netW) && netW >= 0 && netW <= limits.importLimitW {
			cooldown := o.EVImportCooldownCycles
			if cooldown <= 0 {
				cooldown = 20
			}
			o.impGuard.evSafeCount = cooldown
		}
	} else {
		o.impGuard.activeLimitW = limits.importLimitW // same session; track sub-threshold drift
	}

	importW := 0.0
	if !math.IsNaN(netW) {
		importW = math.Max(0, netW) // positive netW = importing from grid
	}

	// Measured battery discharge before this tick's commands.  Used as the
	// first-tick fallback for the conservation identity below.
	measuredDischargeW := 0.0
	for _, b := range batteries {
		if b.Connected && b.PowerW > 0 {
			measuredDischargeW += b.PowerW
		}
	}

	// Conservation identity: the meter import already reflects whatever the
	// battery is currently discharging.  So the unconstrained import — what
	// the meter would show with the battery idle — is importW + measured discharge.
	// We intentionally use the measured (not commanded) value: if Modbus readings
	// are stale across consecutive engine ticks, substituting the prior commanded
	// value compounds it each tick (unconstrained grows without bound), causing
	// runaway over-discharge followed by oscillation.
	unconstrainedImportW := importW + measuredDischargeW

	margin := o.ImportMarginFrac
	if margin <= 0 {
		margin = 0.20
	}
	relaxCycles := o.ExportRelaxCycles
	if relaxCycles <= 0 {
		relaxCycles = 5
	}
	conservativeLimitW := limits.importLimitW * (1.0 - margin)

	// Hysteresis: count safe ticks against the hard limit, not the
	// conservative one, so we don't refuse to relax when the controller is
	// sitting steady at the conservative target.
	if importW <= limits.importLimitW {
		o.impGuard.safeCount++
	} else {
		o.impGuard.safeCount = 0
	}

	// evSafeCount gates EV resumption: only increments when the site is
	// actually importing (positive netW) and under the cap.  Negative netW
	// (export due to battery over-discharge) resets it so the EV cannot
	// resume during the over-discharge settling transient.
	if !math.IsNaN(netW) && netW >= 0 && netW <= limits.importLimitW {
		o.impGuard.evSafeCount++
	} else {
		o.impGuard.evSafeCount = 0
	}

	// Target discharge brings unconstrained import down to the conservative limit.
	targetDischargeW := math.Max(0, unconstrainedImportW-conservativeLimitW)

	// Slew: ratchet up immediately (defend the limit fast), ramp down only
	// after safeCount accumulates so we don't chatter across the boundary.
	commandedDischargeW := targetDischargeW
	if !math.IsNaN(o.impGuard.dischargeW) {
		prior := o.impGuard.dischargeW
		if targetDischargeW < prior {
			if o.impGuard.safeCount < relaxCycles {
				commandedDischargeW = prior // hold
			} else {
				const maxRelaxW = 250.0
				commandedDischargeW = math.Max(targetDischargeW, prior-maxRelaxW)
				o.impGuard.safeCount = 0 // restart hold window after each ramp-down step
			}
		}
	}

	if commandedDischargeW < 50 {
		// Nothing to defend; let restore idle the battery and clear guard so
		// a fresh episode starts cleanly on the next over-limit event.
		o.impGuard.dischargeW = math.NaN()
		return batteries
	}

	result := make([]BatteryState, len(batteries))
	copy(result, batteries)

	// Stop any commanded battery CHARGING while defending the cap.  Charging
	// draws from the grid, so a cost-plan charge during an import breach directly
	// causes the violation — and a battery too drained to discharge (below the
	// SOC reserve) can still at least stop charging.  Neutralise negative
	// setpoints to idle before assigning discharge.
	for i := range plan.BatteryCommands {
		if plan.BatteryCommands[i].SetpointW < 0 {
			name := plan.BatteryCommands[i].Name
			plan.BatteryCommands[i].SetpointW = 0
			for j := range result {
				if result[j].Name == name {
					result[j].PowerW = 0
				}
			}
			plan.AddDecision("csip/import-limit",
				fmt.Sprintf("import %.0fW over limit %.0fW; halting battery %s charge",
					importW, limits.importLimitW, name),
				fmt.Sprintf("%s charge → 0W (was draining grid into the cap)", name))
		}
	}

	// Discharge already committed by prior rules (e.g. the 24-hour cost plan).
	// The import-limit rule must be able to RAISE these setpoints — defending a
	// CSIP import cap overrides the cost-optimal dispatch — so account for what
	// is already committed and add only the shortfall.  Previously the loop
	// SKIPPED any battery the plan had commanded (hasBatteryCommand), so a soft
	// plan setpoint (e.g. 249 W at 66 % SOC) was left in place while grid import
	// breached the cap; the rule could never discharge harder to hold the limit.
	committedDischargeW := 0.0
	for _, c := range plan.BatteryCommands {
		if c.SetpointW > 0 {
			committedDischargeW += c.SetpointW
		}
	}
	remaining := math.Max(0, commandedDischargeW-committedDischargeW)
	totalCommanded := committedDischargeW

	for i, b := range result {
		if remaining < 1 {
			break
		}
		if !b.Connected {
			continue
		}
		if o.dischargeBlocked(b.Name, b.SOC) {
			continue
		}
		// AvailableDischargeW is the headroom from the current setpoint (which a
		// prior rule may already have raised) up to MaxDischargeW.
		add := math.Min(b.AvailableDischargeW(), remaining)
		if add <= 0 {
			continue
		}
		base := 0.0
		if j := batteryCommandIndex(plan.BatteryCommands, b.Name); j >= 0 && plan.BatteryCommands[j].SetpointW > 0 {
			base = plan.BatteryCommands[j].SetpointW
		}
		newSetpoint := base + add
		if j := batteryCommandIndex(plan.BatteryCommands, b.Name); j >= 0 {
			plan.BatteryCommands[j].SetpointW = newSetpoint
		} else {
			plan.BatteryCommands = append(plan.BatteryCommands, BatteryCommand{
				Name:      b.Name,
				SetpointW: newSetpoint,
			})
		}
		result[i].PowerW = newSetpoint
		plan.AddDecision("csip/import-limit",
			fmt.Sprintf("import %.0fW vs limit %.0fW (target ≤%.0fW); unconstrained %.0fW; %s discharges %.0fW",
				importW, limits.importLimitW, conservativeLimitW, unconstrainedImportW, b.Name, newSetpoint),
			fmt.Sprintf("%s → %.0fW discharge", b.Name, newSetpoint))
		remaining -= add
		totalCommanded += add
	}

	if totalCommanded > 0 {
		o.impGuard.dischargeW = totalCommanded
	} else {
		// No battery could actually discharge (all at reserve, etc.).  Clear
		// guard so we don't carry a phantom setpoint.
		o.impGuard.dischargeW = math.NaN()
	}

	// Compliance breach: the site is over the hard import limit and the battery
	// has no discharge headroom left to close the gap (all packs at/below the
	// SOC reserve, or already maxed). This is a physically unavoidable miss —
	// with no stored energy the hub cannot offset the load — but the grid server
	// must be told the cap is not being met. `remaining` is the discharge we
	// still needed but could not command.
	if importW > limits.importLimitW && remaining > complianceBreachW {
		o.recordBreach(plan, &ComplianceBreach{
			LimitType:  "import",
			LimitW:     limits.importLimitW,
			MeasuredW:  importW,
			ShortfallW: importW - limits.importLimitW,
			Reason:     batteryHeadroomReason(result, socReserve),
		})
	}
	return result
}

// complianceBreachW is the discharge/curtailment shortfall (W) below which a
// limit miss is treated as measurement noise rather than a reportable breach.
const complianceBreachW = 100.0

// recordBreach attaches a breach to the plan, keeping the worst (largest
// shortfall) when more than one rule reports one in the same tick.
func (o *DefaultOptimizer) recordBreach(plan *Plan, b *ComplianceBreach) {
	if plan.Breach == nil || b.ShortfallW > plan.Breach.ShortfallW {
		plan.Breach = b
	}
}

// batteryHeadroomReason describes why the battery could not discharge further.
func batteryHeadroomReason(batteries []BatteryState, socReserve float64) string {
	lowest := math.NaN()
	for _, b := range batteries {
		if !b.Connected {
			continue
		}
		if math.IsNaN(lowest) || b.SOC < lowest {
			lowest = b.SOC
		}
	}
	if !math.IsNaN(lowest) && lowest <= socReserve {
		return fmt.Sprintf("battery at SOC reserve (%.0f%% ≤ %.0f%%); no discharge headroom", lowest, socReserve)
	}
	return "battery discharge headroom exhausted (at MaxDischargeW)"
}

// applyRestoreRule is the STANDING-INTENT source (ledger L1): it emits restore
// commands for devices that received no command this tick, so the hub's desired
// state is always complete. Solar is restored to full output (NaN = nameplate
// max); battery is idled (0 W) and reconnected so a prior disconnect does not
// persist. These commands feed the desired-doc publisher (cmd/hub/desired.go),
// which serializes them into the retained lexa/desired/* documents the
// reconciler executes — TASK-032 deleted the per-tick QoS 1 command spam this
// rule used to drive through the legacy actuators, but the RULE itself (the
// intent) stays: it is what makes "idle enforces the reserve at any SoC" and
// "restore is an explicit write" true on the wire.
//
// When no solar cap is active, the restore is emitted for DISCONNECTED inverters
// too: the retained desired doc holds it and the reconciler reasserts it on
// reconnect, so a release that happens while the inverter is dark is delivered on
// its return instead of leaving it latched at the stale ceiling. Gating it on
// Connected meant an inverter dark on the ticks after the cap cleared kept the
// old ceiling as its desired state and returned still clamped, forever (QA
// 2026-07-03: curtailment-release, clock-jump-forward, release-while-rebooting —
// 11/21 FAILs).
//
// While a solar cap IS active, a dark inverter is deliberately left uncommanded:
// the cap rules only command connected inverters, so the retained desired doc
// still holds the live curtailment — queuing a restore here would overwrite it
// and a reconnecting inverter would snap to full nameplate under an active cap
// until the next tick re-curtails.
//
// Batteries keep the Connected gate deliberately: the orchestrator re-commands
// packs every engine tick once they are connected (this rule idles any uncommanded
// pack), so a reconnecting battery is reconciled within one tick — and the
// reconciler's reassert-on-reconnect covers a pack that WAS commanded while dark.
// Queuing idle commands for dark packs would add nothing but per-tick MQTT noise.
func applyRestoreRule(solar []SolarState, batteries []BatteryState, socReserve float64, solarCapActive bool, plan *Plan) {
	for _, sol := range solar {
		if hasSolarCommand(plan.SolarCommands, sol.Name) {
			continue
		}
		if !sol.Connected && solarCapActive {
			continue // dark under an active cap: keep the held curtailment as desired state
		}
		plan.SolarCommands = append(plan.SolarCommands, SolarCommand{
			Name:       sol.Name,
			CurtailToW: math.NaN(), // NaN → restore to full nameplate output
		})
	}
	reconnect := true
	for _, b := range batteries {
		if b.Connected && !hasBatteryCommand(plan.BatteryCommands, b.Name) && b.MaxDischargeW > 0 {
			// Always idle an uncommanded battery to 0 W — regardless of SOC.
			//
			// This previously only fired when SOC > socReserve, which silently
			// drained the pack to empty: once SOC fell to the reserve, every
			// discharge rule correctly STOPPED issuing a discharge (they skip at
			// the reserve), so no command was sent — and the device kept running
			// the last discharge setpoint latched in its Modbus registers.  In the
			// 92-day replay this sailed the battery straight through the 20%
			// reserve to 0% during peak (e.g. 42%→0% in ~3 h), defeating the whole
			// point of the reserve and leaving nothing for evening import caps or
			// emergencies.  Idling to 0 W can never breach the reserve — it is the
			// command that ENFORCES it — so it must be sent at any SOC.
			plan.BatteryCommands = append(plan.BatteryCommands, BatteryCommand{
				Name:      b.Name,
				SetpointW: 0,          // idle: clear any stale (e.g. discharge) setpoint
				Connect:   &reconnect, // re-assert Conn=1 each tick
			})
		}
	}
}

// ── helpers ────────────────────────────────────────────────────────────────────

func apW(ap *model.ActivePower) float64 {
	return float64(ap.Value) * math.Pow10(int(ap.Multiplier))
}

func nanMin(a, b float64) float64 {
	if math.IsNaN(a) {
		return b
	}
	if math.IsNaN(b) {
		return a
	}
	return math.Min(a, b)
}

func hasBatteryCommand(cmds []BatteryCommand, name string) bool {
	return batteryCommandIndex(cmds, name) >= 0
}

// batteryCommandIndex returns the index of the command for name, or −1 if absent.
func batteryCommandIndex(cmds []BatteryCommand, name string) int {
	for i := range cmds {
		if cmds[i].Name == name {
			return i
		}
	}
	return -1
}

func hasSolarCommand(cmds []SolarCommand, name string) bool {
	return solarCommandIndex(cmds, name) >= 0
}

// solarCommandIndex returns the index of the command for name, or −1 if absent.
func solarCommandIndex(cmds []SolarCommand, name string) int {
	for i := range cmds {
		if cmds[i].Name == name {
			return i
		}
	}
	return -1
}

func hasEVSECommand(cmds []EVSECommand, stationID string, connectorID int) bool {
	for _, c := range cmds {
		if c.StationID == stationID && c.ConnectorID == connectorID {
			return true
		}
	}
	return false
}
