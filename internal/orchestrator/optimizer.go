package orchestrator

import (
	"fmt"
	"log"
	"math"
	"time"

	"lexa-hub/internal/csip/model"
)

// exportGuard carries state across ticks for the conservative export-limit rule.
type exportGuard struct {
	evSetpointA     float64 // last EV current limit issued; NaN until first command
	evCmdW          float64 // last EV power commanded (current × voltage at command time); NaN = none
	batteryAbsorbW  float64 // last battery absorption (positive watts) commanded; NaN = none
	safeCount       int     // consecutive ticks where actual export ≤ conservative target
	activeLimitW    float64 // limit value when guard was reset; NaN = no active limit
	filteredExportW float64 // low-pass-filtered actual export, used by the controller
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
}

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
	// Default 20 (≈ 1 min at a 3 s engine tick).
	EVImportCooldownCycles int

	// NowFunc returns the current time.  Nil means time.Now.
	// Override in tests to inject a deterministic clock.
	NowFunc func() time.Time

	// expGuard holds per-limit-session state for the export-limit rule.
	expGuard exportGuard

	// impGuard holds per-limit-session state for the import-limit rule.
	impGuard importGuard
}

// NewDefaultOptimizer returns an optimizer with sensible defaults.
func NewDefaultOptimizer() *DefaultOptimizer {
	return &DefaultOptimizer{
		SOCReserve:           20.0,
		SOCFullThreshold:     95.0,
		ExcessSolarThreshold: 100.0,
		ExportMarginFrac:     0.20,
		ExportRelaxCycles:      5,
		ImportMarginFrac:       0.20,
		EVImportCooldownCycles: 20,
		expGuard: exportGuard{
			evSetpointA:     math.NaN(),
			evCmdW:          math.NaN(),
			batteryAbsorbW:  math.NaN(),
			activeLimitW:    math.NaN(),
			filteredExportW: math.NaN(),
		},
		impGuard: importGuard{
			dischargeW:   math.NaN(),
			activeLimitW: math.NaN(),
		},
	}
}

func (o *DefaultOptimizer) now() time.Time {
	if o.NowFunc != nil {
		return o.NowFunc()
	}
	return time.Now()
}

// gridConstraints holds effective export/import/max limits after applying CSIP
// overrides on top of grid-reported values.  NaN means unconstrained.
type gridConstraints struct {
	exportLimitW float64
	importLimitW float64
	maxLimitW    float64
}

// Optimize evaluates all rules against state and returns a Plan.
func (o *DefaultOptimizer) Optimize(state SystemState) Plan {
	plan := Plan{Timestamp: o.now()}

	// Rule 1: CSIP disconnect — highest priority, always early-return.
	if csipDisconnectRule(state.CSIPControl, state.Batteries, &plan) {
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

	// Rule 2: CSIP fixed dispatch — discharge battery to meet explicit grid export request.
	batteries = applyFixedDispatchRule(state.CSIPControl, batteries, solarW, homeLoadW, o.SOCReserve, &plan)

	// Rule 3: Export/import limit — absorb excess into EVSEs, battery, then curtail solar.
	batteries, surplusW = o.applyExportLimitRule(state.Solar, state.EVSEs, evseW, limits, state.Grid.NetW, o.SOCFullThreshold, surplusW, batteries, &plan)

	// Rule 3.5: Import limit enforcement — discharge battery to reduce grid import.
	batteries = o.applyImportLimitRule(batteries, limits, state.Grid.NetW, o.SOCReserve, &plan)

	// Rule 4: Self-consumption — route solar surplus to battery.
	batteries, surplusW = applySelfConsumptionRule(batteries, surplusW, o.ExcessSolarThreshold, o.SOCFullThreshold, &plan)

	// Rule 5: TOU peak discharge.
	// CSIP dispatch (OpModFixedW) is handled in Rule 2; this rule covers autonomous peak shifting.
	serverNow := time.Unix(o.now().Unix()+state.ClockOffset, 0)
	isPeak := o.CostModel != nil && o.CostModel.IsPeakHour(serverNow)
	peakReason := ""
	if isPeak {
		peakReason = fmt.Sprintf("peak TOU hour (rate=%.3f/kWh)", o.CostModel.CurrentRate(serverNow))
	}
	batteries, surplusW = applyDemandResponseRule(batteries, surplusW, o.SOCReserve, false, isPeak, peakReason, &plan)

	// Rule 6: EV charging allocation.
	// Suppress EV while the import guard hasn't accumulated enough consecutive
	// ticks of stable positive import — prevents EV from resuming during the
	// battery over-discharge transient that briefly makes the site look like
	// it has surplus (grid.NetW < 0).
	cooldown := o.EVImportCooldownCycles
	if cooldown <= 0 {
		cooldown = 20
	}
	evImportSuppressed := !math.IsNaN(limits.importLimitW) && o.impGuard.evSafeCount < cooldown
	applyEVChargingRule(state.EVSEs, limits, state.Grid.NetW, solarW, surplusW, evImportSuppressed, &plan)

	// Final: restore unconstrained devices so prior setpoints don't persist.
	applyRestoreRule(state.Solar, batteries, o.SOCReserve, &plan)

	return plan
}

// ── Rule functions ─────────────────────────────────────────────────────────────

// csipDisconnectRule issues Connect=false commands when the utility sends
// OpModConnect=false.  Returns true when Optimize should return immediately.
func csipDisconnectRule(cc *CSIPControlState, batteries []BatteryState, plan *Plan) bool {
	if cc == nil || cc.Base.OpModConnect == nil || *cc.Base.OpModConnect {
		return false
	}
	f := false
	for _, b := range batteries {
		plan.BatteryCommands = append(plan.BatteryCommands, BatteryCommand{
			Name:    b.Name,
			Connect: &f,
		})
	}
	plan.AddDecision("csip/disconnect",
		"OpModConnect=false received from utility",
		fmt.Sprintf("disconnecting %d batteries", len(batteries)))
	return true
}

// deriveGridConstraints returns the tightest of CSIP and grid-reported limits.
// NaN in any field means no constraint for that direction.
func deriveGridConstraints(grid GridState, cc *CSIPControlState) gridConstraints {
	c := gridConstraints{
		exportLimitW: grid.ExportLimitW,
		importLimitW: grid.ImportLimitW,
		maxLimitW:    grid.MaxLimitW,
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
	// MaxLimW is an absolute generation cap that also constrains exports.
	if !math.IsNaN(c.maxLimitW) {
		c.exportLimitW = nanMin(c.exportLimitW, c.maxLimitW)
	}
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

// applyFixedDispatchRule discharges batteries to meet an explicit grid export
// request (CSIP OpModFixedW).  Solar is credited first; batteries cover the
// shortfall up to SOC reserve.
func applyFixedDispatchRule(cc *CSIPControlState, batteries []BatteryState, solarW, homeLoadW, socReserve float64, plan *Plan) []BatteryState {
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
		if !math.IsNaN(b.SOC) && b.SOC <= socReserve {
			plan.AddDecision("csip/fixed-dispatch",
				fmt.Sprintf("battery %s SOC=%.1f%% at reserve minimum", b.Name, b.SOC),
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
		o.expGuard = exportGuard{evSetpointA: math.NaN(), evCmdW: math.NaN(), batteryAbsorbW: math.NaN(), activeLimitW: math.NaN(), filteredExportW: math.NaN()}
		return batteries, surplusW
	}

	// New limit value → start the guard fresh.
	if limits.exportLimitW != o.expGuard.activeLimitW {
		o.expGuard = exportGuard{evSetpointA: math.NaN(), evCmdW: math.NaN(), batteryAbsorbW: math.NaN(), activeLimitW: limits.exportLimitW, filteredExportW: math.NaN()}
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
	// tick when the battery is charging at its full MaxChargeW.  At 20× demo
	// speed with a 10 kWh / 5 kW pack and a 3 s tick, that's ≈ 0.83 %.  This
	// only needs to be in the right ballpark; it gates the size of the EV
	// feed-forward step.
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

	// ── Solar curtailment: last resort, only when the limit is still exceeded ─
	// Curtailment is a hard-fault safety net, not a control variable, so it
	// reads the unfiltered measured export.  EV-driven absorption uses the
	// commanded setpoint (predicted steady-state) so we don't double-curtail
	// while the OCPP MeterValues / Modbus polls are still catching up.
	commandedEvW := evseW
	if ev != nil && !math.IsNaN(o.expGuard.evSetpointA) {
		voltage := ev.VoltageV
		if voltage <= 0 {
			voltage = 230.0
		}
		commandedEvW = o.expGuard.evSetpointA * voltage
	}
	finalExcessW := math.Max(0, unconstrainedExportW-conservativeW-batteryAbsorbW-commandedEvW)
	if finalExcessW > 1 {
		if totalSolarW > 0 {
			fraction := math.Min(1.0, finalExcessW/totalSolarW)
			for _, sol := range solar {
				if !sol.Connected {
					continue
				}
				curtailTo := sol.PowerW * (1 - fraction)
				plan.SolarCommands = append(plan.SolarCommands, SolarCommand{
					Name:       sol.Name,
					CurtailToW: curtailTo,
				})
				plan.AddDecision("csip/export-limit",
					fmt.Sprintf("curtailing solar %s to %.0fW (hard limit %.0fW still exceeded)",
						sol.Name, curtailTo, limits.exportLimitW),
					fmt.Sprintf("solar %s %.0fW → %.0fW", sol.Name, sol.PowerW, curtailTo))
			}
		}
	}

	return batteries, surplusW
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
func applyDemandResponseRule(batteries []BatteryState, surplusW, socReserve float64, isDR, isPeak bool, peakReason string, plan *Plan) ([]BatteryState, float64) {
	if !isDR && !isPeak {
		return batteries, surplusW
	}
	reason := "demand-response event active"
	if peakReason != "" {
		reason = peakReason
	}
	for i, b := range batteries {
		if !b.Connected || !b.Energized {
			continue
		}
		if !math.IsNaN(b.SOC) && b.SOC <= socReserve {
			plan.AddDecision("demand-response",
				fmt.Sprintf("battery %s SOC=%.1f%% at reserve minimum", b.Name, b.SOC),
				"skip discharge — protecting reserve")
			continue
		}
		available := b.AvailableDischargeW()
		if available < 50 {
			continue
		}
		if !hasBatteryCommand(plan.BatteryCommands, b.Name) {
			plan.BatteryCommands = append(plan.BatteryCommands, BatteryCommand{
				Name:      b.Name,
				SetpointW: b.MaxDischargeW,
			})
			plan.AddDecision("demand-response",
				reason,
				fmt.Sprintf("discharging battery %s at %.0fW", b.Name, b.MaxDischargeW))
			batteries[i].PowerW = b.MaxDischargeW
			surplusW += available
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

	// New limit value → restart the guard fresh.
	if limits.importLimitW != o.impGuard.activeLimitW {
		o.impGuard = importGuard{dischargeW: math.NaN(), activeLimitW: limits.importLimitW}
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
	remaining := commandedDischargeW
	totalCommanded := 0.0

	for i, b := range result {
		if remaining < 1 {
			break
		}
		if !b.Connected || hasBatteryCommand(plan.BatteryCommands, b.Name) {
			continue
		}
		if !math.IsNaN(b.SOC) && b.SOC <= socReserve {
			continue
		}
		discharge := math.Min(b.AvailableDischargeW(), remaining)
		if discharge <= 0 {
			continue
		}
		result[i].PowerW = discharge
		plan.BatteryCommands = append(plan.BatteryCommands, BatteryCommand{
			Name:      b.Name,
			SetpointW: discharge,
		})
		plan.AddDecision("csip/import-limit",
			fmt.Sprintf("import %.0fW vs limit %.0fW (target ≤%.0fW); unconstrained %.0fW; %s discharges %.0fW",
				importW, limits.importLimitW, conservativeLimitW, unconstrainedImportW, b.Name, discharge),
			fmt.Sprintf("%s → %.0fW discharge", b.Name, discharge))
		remaining -= discharge
		totalCommanded += discharge
	}

	if totalCommanded > 0 {
		o.impGuard.dischargeW = totalCommanded
	} else {
		// No battery could actually discharge (all at reserve, etc.).  Clear
		// guard so we don't carry a phantom setpoint.
		o.impGuard.dischargeW = math.NaN()
	}
	return result
}

// applyRestoreRule sends restore commands for devices that received no command this
// tick so that prior setpoints don't latch in Modbus registers.
// Solar is restored to full output (NaN = nameplate max).
// Battery is idled (0 W) and reconnected so a prior disconnect does not persist.
func applyRestoreRule(solar []SolarState, batteries []BatteryState, socReserve float64, plan *Plan) {
	for _, sol := range solar {
		if sol.Connected && !hasSolarCommand(plan.SolarCommands, sol.Name) {
			plan.SolarCommands = append(plan.SolarCommands, SolarCommand{
				Name:       sol.Name,
				CurtailToW: math.NaN(), // NaN → restore to full nameplate output
			})
		}
	}
	reconnect := true
	for _, b := range batteries {
		if b.Connected && !hasBatteryCommand(plan.BatteryCommands, b.Name) && b.MaxDischargeW > 0 {
			if math.IsNaN(b.SOC) || b.SOC > socReserve {
				plan.BatteryCommands = append(plan.BatteryCommands, BatteryCommand{
					Name:      b.Name,
					SetpointW: 0,          // idle: clear any stale setpoint
					Connect:   &reconnect, // re-assert Conn=1 each tick
				})
			}
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
	for _, c := range cmds {
		if c.Name == name {
			return true
		}
	}
	return false
}

func hasSolarCommand(cmds []SolarCommand, name string) bool {
	for _, c := range cmds {
		if c.Name == name {
			return true
		}
	}
	return false
}

func hasEVSECommand(cmds []EVSECommand, stationID string, connectorID int) bool {
	for _, c := range cmds {
		if c.StationID == stationID && c.ConnectorID == connectorID {
			return true
		}
	}
	return false
}
