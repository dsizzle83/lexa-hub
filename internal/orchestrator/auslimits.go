package orchestrator

// CSIP-AUS dynamic-envelope enforcement (WP-11): the opModGenLimW gross-
// generation and opModLoadLimW gross-load cascade rules plus their
// measured-effect convergence backstops. Everything in this file runs ONLY
// when DefaultOptimizer.EnforceAusLimits is true (hub.json
// `enforce_aus_limits`, default false) — with the flag off, plans are
// byte-identical to pre-WP-11 no matter what AUS limits GridState carries.
//
// The mirrored SHADOW copies live in orchestrator/constraint/genlimaus.go and
// loadlimaus.go (TASK-060/061 porting pattern) and run under
// constraint_shadow regardless of this flag. Per the repo's do-not-strip
// rule, BOTH copies must exist until the per-axis `active` flip retires the
// cascade side — never delete either.

import (
	"fmt"
	"math"
)

// applyAusGenerationLimitRule enforces the CSIP-AUS opModGenLimW GROSS
// generation cap (WP-11): gross generation = solar output PLUS battery
// discharge, capped at the connection point.
//
// DISAMBIGUATION: this is NOT applyGenLimitRule. That rule enforces
// opModMaxLimW — an absolute cap on inverter OUTPUT alone, which deliberately
// ignores the battery (deriveGridConstraints' note: battery absorption merely
// hides over-generation from the meter). The CSIP-AUS dynamic envelope caps
// everything the site's DER produces — inverter output and battery discharge
// alike — so this rule has two levers:
//
//  1. Battery-discharge participation cap: batteries may only discharge into
//     the headroom measured solar generation leaves under the cap
//     (dischargeCapW = max(0, cap − solar) — solar is free energy, so it is
//     never curtailed to make room for stored energy). Discharge already
//     committed this tick by earlier rules (fixed dispatch, plan-follow,
//     import-limit) is trimmed to that cap in command order, and the
//     REMAINING headroom is returned for Rule 5's maxDischargeW so autonomous
//     TOU discharge respects the cap too — the same threading the export
//     limit uses for its dischargeCapW.
//  2. Solar ceiling: whatever discharge remains committed shares the cap with
//     the inverters — ceiling = max(0, cap − committed discharge),
//     distributed by nameplate share and merged keep-the-tighter with any
//     ceiling the export or opModMaxLimW rules already issued (the same merge
//     applyGenLimitRule uses).
//
// The ceiling is absolute and re-issued every tick while the cap is active —
// the same sticky reasoning as applyGenLimitRule: the inverter clamps output
// to min(potential, ceiling), so re-issuing can never raise generation, and a
// conditional ceiling would be un-curtailed by the restore rule and oscillate
// across the cap at the tick period.
//
// Returns the (possibly updated) battery snapshot slice and the remaining
// discharge headroom for Rule 5 (NaN when no cap is active).
func (o *DefaultOptimizer) applyAusGenerationLimitRule(solar []SolarState, batteries []BatteryState, genLimitW float64, plan *Plan) ([]BatteryState, float64) {
	if math.IsNaN(genLimitW) {
		return batteries, math.NaN()
	}

	totalSolarW := 0.0
	for _, sol := range solar {
		if sol.Connected && sol.PowerW > 0 {
			totalSolarW += sol.PowerW
		}
	}

	// Lever 1 — battery-discharge participation cap. Trim discharge already
	// committed by earlier rules (fixed dispatch, plan-follow, import-limit)
	// to the headroom measured solar leaves under the cap, in command order.
	dischargeCapW := math.Max(0, genLimitW-totalSolarW)
	remainingW := dischargeCapW
	for i := range plan.BatteryCommands {
		sp := plan.BatteryCommands[i].SetpointW
		if math.IsNaN(sp) || sp <= 0 {
			continue
		}
		allowed := math.Min(sp, remainingW)
		if allowed < sp {
			name := plan.BatteryCommands[i].Name
			plan.BatteryCommands[i].SetpointW = allowed
			for j := range batteries {
				if batteries[j].Name == name {
					batteries[j].PowerW = allowed
				}
			}
			plan.AddDecision("csip-aus/gen-limit",
				fmt.Sprintf("gross generation cap %.0fW: solar %.0fW leaves %.0fW discharge headroom; trimming battery %s discharge %.0fW → %.0fW",
					genLimitW, totalSolarW, dischargeCapW, name, sp, allowed),
				fmt.Sprintf("battery %s → %.0fW", name, allowed))
		}
		remainingW -= allowed
	}
	// Total discharge still committed after the trim: every positive command
	// consumed exactly its final setpoint from the cap.
	committedDischargeW := dischargeCapW - remainingW

	// Lever 2 — solar ceiling on the share of the cap discharge is not using.
	totalNameplateW := 0.0
	for _, sol := range solar {
		if sol.Connected {
			totalNameplateW += sol.MaxW
		}
	}
	if totalNameplateW > 0 {
		ceilingW := math.Max(0, genLimitW-committedDischargeW)
		curtailed := 0
		for _, sol := range solar {
			if !sol.Connected {
				continue
			}
			curtailTo := ceilingW * (sol.MaxW / totalNameplateW)
			if i := solarCommandIndex(plan.SolarCommands, sol.Name); i >= 0 {
				// Already curtailed (export or opModMaxLimW rule): keep the tighter cap.
				if math.IsNaN(plan.SolarCommands[i].CurtailToW) || curtailTo < plan.SolarCommands[i].CurtailToW {
					plan.SolarCommands[i].CurtailToW = curtailTo
				}
			} else {
				plan.SolarCommands = append(plan.SolarCommands, SolarCommand{Name: sol.Name, CurtailToW: curtailTo})
			}
			curtailed++
		}
		plan.AddDecision("csip-aus/gen-limit",
			fmt.Sprintf("gross generation cap %.0fW: ceiling %.0fW solar + %.0fW committed battery discharge (held continuously)",
				genLimitW, ceilingW, committedDischargeW),
			fmt.Sprintf("ceiling %d inverters to ≤ %.0fW total", curtailed, ceilingW))
	}

	return batteries, remainingW
}

// checkAusGenerationConvergence verifies the site actually honoured the
// CSIP-AUS gross-generation cap applyAusGenerationLimitRule just commanded —
// the measured-effect half of closed-loop actuation, mirroring
// checkGenLimitConvergence (its template) in session tracking, leaky counter,
// and escalation latency, but over GROSS generation.
func (o *DefaultOptimizer) checkAusGenerationConvergence(solar []SolarState, batteries []BatteryState, netW, genLimitW float64, plan *Plan) {
	if math.IsNaN(genLimitW) {
		o.ausGenGuard = ausGenGuard{activeLimitW: math.NaN()} // cap cleared — reset
		return
	}
	// Reset the breach counter only when the cap changes MEANINGFULLY —
	// tolerance-band session tracking, verbatim from checkGenLimitConvergence:
	// the decoded cap can vary by a hair tick-to-tick (the watts→ActivePower
	// value×10^mult round-trip through the bus), and a bit-exact reset would
	// zero the counter every tick so a sustained breach never escalates.
	if math.IsNaN(o.ausGenGuard.activeLimitW) || math.Abs(genLimitW-o.ausGenGuard.activeLimitW) > complianceBreachW {
		o.ausGenGuard = ausGenGuard{activeLimitW: genLimitW} // new cap session — reset
	} else {
		o.ausGenGuard.activeLimitW = genLimitW // same session; track sub-threshold drift
	}

	// Measured GROSS generation: solar output plus battery discharge — the
	// quantity opModGenLimW caps. Both terms are the devices' self-reports.
	measuredGrossW := 0.0
	for _, sol := range solar {
		if sol.Connected && sol.PowerW > 0 {
			measuredGrossW += sol.PowerW
		}
	}
	for _, b := range batteries {
		if b.Connected && b.PowerW > 0 {
			measuredGrossW += b.PowerW
		}
	}

	// Independent GROSS-generation floor from the grid meter — ADAPTED from
	// checkGenLimitConvergence's meter floor (a HARD preserve: a device that
	// echoes the commanded limit back while still generating is caught only
	// by this). The original floor is `gen ≥ export − batteryDischarge`
	// because opModMaxLimW caps inverter output ALONE and the export may be
	// battery-fed. Here battery discharge is INSIDE the capped quantity, so
	// the subtraction drops out: from the site energy balance,
	//   grossGen = export + load + evse + batteryCharge − import,
	// and load/evse/batteryCharge are all ≥ 0, so grossGen ≥ export − import
	// = −netW regardless of what any device self-reports. A solar inverter or
	// battery that echoes a compliant power while the meter shows the site
	// exporting over the cap is still caught.
	if !math.IsNaN(netW) {
		if floor := -netW; floor > measuredGrossW {
			measuredGrossW = floor
		}
	}

	// Leaky counter, verbatim from checkGenLimitConvergence: a single
	// under-cap sample decrements rather than zeroing a climbing breach, so a
	// sustained miss with occasional noise still escalates while genuine
	// convergence drains the counter within a few ticks.
	threshold := o.scaleTicks(ausGenBreachTicks)
	if measuredGrossW > genLimitW+complianceBreachW {
		if o.ausGenGuard.overCount < threshold {
			o.ausGenGuard.overCount++ // cap at the threshold so it drains fast on recovery
		}
	} else if o.ausGenGuard.overCount > 0 {
		o.ausGenGuard.overCount--
	}

	if o.ausGenGuard.overCount >= threshold {
		o.recordBreach(plan, &ComplianceBreach{
			LimitType:  "generation-aus",
			LimitW:     genLimitW,
			MeasuredW:  measuredGrossW,
			ShortfallW: measuredGrossW - genLimitW,
			Reason:     "gross generation (solar + battery discharge) remains above the CSIP-AUS generation cap after curtailment was commanded — a device is not honouring the command",
		})
		plan.AddDecision("csip-aus/gen-limit",
			fmt.Sprintf("gross generation %.0fW still over cap %.0fW after %d ticks (~%.0fs)",
				measuredGrossW, genLimitW, o.ausGenGuard.overCount, float64(o.ausGenGuard.overCount)*o.tickSeconds()),
			"reporting CannotComply — site not converging to the CSIP-AUS generation cap")
	}
}

// applyAusLoadLimitRule enforces the CSIP-AUS opModLoadLimW GROSS load cap
// (WP-11): gross load = home consumption + EV charging + battery charging,
// capped at the connection point. The load-side twin of
// applyAusGenerationLimitRule.
//
// Measurement comes from the site energy balance: rearranging
// solar + batteryDischarge + import = home + evse + batteryCharge + export,
//
//	grossLoad = solar + batteryDischarge + netW
//
// (all three terms measured), so the rule needs the grid meter. A meter-blind
// tick (netW NaN) re-issues the sticky EV ceiling — a cap is conservative, so
// holding the last enforcement is the fail-closed choice — but takes no new
// decision; the convergence counter NaN-holds too (checkAusLoadConvergence).
//
// Levers, in order:
//
//  1. Battery-charge cap: charging is load the hub itself added, so it is
//     shed first (single Modbus write — the same battery-first reasoning as
//     the export rule). Commanded charge (export-absorb, plan-follow,
//     self-consumption) is trimmed so total charge fits the headroom the cap
//     leaves over the measured non-battery load. A trimmed export-absorb is
//     self-correcting: the export rule's battStallTicks discredit sees the
//     missing absorption and curtails solar instead — most-restrictive wins.
//  2. EVSE curtail: a sticky per-episode current ceiling on the first active
//     charging session (the same single-EV pattern the export rule uses). It
//     engages when gross load exceeds the hard cap, tightens immediately,
//     relaxes only after ExportRelaxCycles safe ticks and by at most
//     ausLoadEVMaxRelaxA per tick (the import rule's ratchet, direction
//     inverted), and once engaged is re-issued every tick until the cap
//     clears — at the EVSE's rating it is a harmless keep-tighter no-op, the
//     export ceiling's "sticky, clamped at nameplate" rule.
//
// Battery DISCHARGE is deliberately NOT a lever: discharging reduces grid
// import, not site consumption — a load cap constrains what the site
// consumes; import relief is opModImpLimW's job (applyImportLimitRule).
//
// Runs AFTER the economics rules — see the placement comment in Optimize.
func (o *DefaultOptimizer) applyAusLoadLimitRule(solar []SolarState, batteries []BatteryState, evses []EVSEState, loadLimitW, netW float64, plan *Plan) {
	if math.IsNaN(loadLimitW) {
		o.ausLoadGuard = ausLoadGuard{activeLimitW: math.NaN(), evLimitA: math.NaN()}
		return
	}

	// Session management: a MEANINGFUL cap change restarts the whole guard
	// (single reset domain, including breachTicks — the same wholesale reset
	// applyImportLimitRule uses); sub-threshold decode drift is tracked.
	if math.IsNaN(o.ausLoadGuard.activeLimitW) || math.Abs(loadLimitW-o.ausLoadGuard.activeLimitW) > complianceBreachW {
		o.ausLoadGuard = ausLoadGuard{activeLimitW: loadLimitW, evLimitA: math.NaN()}
	} else {
		o.ausLoadGuard.activeLimitW = loadLimitW
	}

	// Meter-blind: no fresh evidence. Hold the sticky EV ceiling (fail-closed)
	// and take no new decision this tick.
	if math.IsNaN(netW) {
		if !math.IsNaN(o.ausLoadGuard.evLimitA) {
			o.mergeAusEVLimit(evses, o.ausLoadGuard.evLimitA, loadLimitW, plan)
		}
		return
	}

	solarW := 0.0
	for _, sol := range solar {
		if sol.Connected && sol.PowerW > 0 {
			solarW += sol.PowerW
		}
	}
	battDischargeW, battChargeW := 0.0, 0.0
	for _, b := range batteries {
		if !b.Connected {
			continue
		}
		if b.PowerW > 0 {
			battDischargeW += b.PowerW
		} else {
			battChargeW += -b.PowerW
		}
	}
	grossLoadW := math.Max(0, solarW+battDischargeW+netW)

	// Hysteresis: count safe ticks against the HARD cap (the EV relax gate),
	// mirroring the import rule's safeCount.
	if grossLoadW <= loadLimitW {
		o.ausLoadGuard.safeCount++
	} else {
		o.ausLoadGuard.safeCount = 0
	}

	margin := o.ImportMarginFrac
	if margin <= 0 {
		margin = 0.20
	}
	conservativeW := loadLimitW * (1 - margin)

	// Lever 1 — battery-charge cap. The measured non-battery load (home + EV)
	// is grossLoad minus the measured battery charge; whatever headroom the
	// conservative target leaves over it is all the charge the site may carry.
	// A commanded charge that FITS the remaining headroom is kept; one that
	// EXCEEDS it is NEUTRALISED to idle (0 W), not trimmed to the boundary —
	// deliberately matching (a) applyImportLimitRule's charge-neutralisation
	// precedent ("stop any commanded battery charging while defending the
	// cap") and (b) the constraint arbiter's cross-tier semantics, where an
	// economics proposal outside a compliance bound is DISCARDED and the
	// projection idles (TestResolve_EconomicsCannotWidenSafetyInterval) — so
	// the cascade and its shadow mirror resolve the same value. The shed
	// charge re-proposes itself next tick from measured state and settles
	// inside the headroom naturally.
	nonBattLoadW := math.Max(0, grossLoadW-battChargeW)
	allowedChargeW := math.Max(0, conservativeW-nonBattLoadW)
	remainingChargeW := allowedChargeW
	chargeAfterW := 0.0
	for i := range plan.BatteryCommands {
		sp := plan.BatteryCommands[i].SetpointW
		if math.IsNaN(sp) || sp >= 0 {
			continue
		}
		charge := -sp
		if charge <= remainingChargeW+1 { // fits (1 W float-noise band)
			remainingChargeW -= charge
			chargeAfterW += charge
			continue
		}
		name := plan.BatteryCommands[i].Name
		plan.BatteryCommands[i].SetpointW = 0
		plan.AddDecision("csip-aus/load-limit",
			fmt.Sprintf("gross load cap %.0fW (target ≤%.0fW): measured non-battery load %.0fW leaves only %.0fW charge headroom; halting battery %s charge (was %.0fW)",
				loadLimitW, conservativeW, nonBattLoadW, allowedChargeW, name, charge),
			fmt.Sprintf("battery %s → 0W", name))
	}

	// Lever 2 — EV curtail (sticky). Engage on a hard-cap breach; once
	// engaged, stay engaged until the cap clears.
	if math.IsNaN(o.ausLoadGuard.evLimitA) && grossLoadW <= loadLimitW {
		return
	}

	// First connected active session — the export rule's single-EV pattern.
	var ev *EVSEState
	for i := range evses {
		if evses[i].Connected && evses[i].SessionActive {
			ev = &evses[i]
			break
		}
	}
	if ev == nil {
		o.ausLoadGuard.evLimitA = math.NaN() // no session — lever released
		return
	}
	voltage := ev.VoltageV
	if voltage <= 0 {
		voltage = 230.0
	}
	const minChargeA = 6.0 // IEC 61851-1 minimum AC charge current

	evMeasuredW := 0.0
	for _, e := range evses {
		if e.Connected && e.SessionActive {
			evMeasuredW += e.PowerW
		}
	}
	homeW := math.Max(0, nonBattLoadW-evMeasuredW)
	evAllowanceW := math.Max(0, conservativeW-homeW-chargeAfterW)
	targetA := math.Min(evAllowanceW/voltage, ev.MaxCurrentA)
	if targetA < minChargeA {
		targetA = 0 // below the IEC floor → suspend
	}

	relaxCycles := o.ExportRelaxCycles
	if relaxCycles <= 0 {
		relaxCycles = 5
	}
	newA := targetA
	if prior := o.ausLoadGuard.evLimitA; !math.IsNaN(prior) && targetA > prior {
		// Relax only after sustained compliance, one bounded step at a time —
		// the import rule's ramp gate, direction inverted (tightening is
		// always immediate: defend the cap fast, give current back slowly).
		if o.ausLoadGuard.safeCount < relaxCycles {
			newA = prior
		} else {
			newA = math.Min(targetA, prior+ausLoadEVMaxRelaxA)
			o.ausLoadGuard.safeCount = 0 // restart hold window after each relax step
		}
		// Never command inside (0, minChargeA): snap up to the IEC floor when
		// the target supports it, else stay suspended.
		if newA > 0 && newA < minChargeA {
			if targetA >= minChargeA {
				newA = minChargeA
			} else {
				newA = 0
			}
		}
	}
	o.ausLoadGuard.evLimitA = newA
	o.mergeAusEVLimit(evses, newA, loadLimitW, plan)
}

// ausLoadEVMaxRelaxA bounds how fast the AUS load rule's sticky EV ceiling may
// relax per tick once the site has held under the cap for the relax window —
// matched to the export rule's maxTightenA so give-back is no faster than the
// export controller's own ramp.
const ausLoadEVMaxRelaxA = 2.0

// mergeAusEVLimit applies the sticky AUS-load EV ceiling to the first active
// session's command: keep-the-tighter with whatever an earlier rule (plan
// follow, export absorb, EV charging) already commanded, or append when the
// session has no command yet. Mirrors applyGenLimitRule's keep-tighter merge
// on the solar axis.
func (o *DefaultOptimizer) mergeAusEVLimit(evses []EVSEState, limitA, loadLimitW float64, plan *Plan) {
	for i := range evses {
		ev := &evses[i]
		if !ev.Connected || !ev.SessionActive {
			continue
		}
		if j := evseCommandIndex(plan.EVSECommands, ev.StationID, ev.ConnectorID); j >= 0 {
			if limitA < plan.EVSECommands[j].MaxCurrentA {
				plan.AddDecision("csip-aus/load-limit",
					fmt.Sprintf("gross load cap %.0fW: EVSE %s limited %.1fA → %.1fA (sticky ceiling)",
						loadLimitW, ev.StationID, plan.EVSECommands[j].MaxCurrentA, limitA),
					fmt.Sprintf("EVSE %s → %.1fA", ev.StationID, limitA))
				plan.EVSECommands[j].MaxCurrentA = limitA
			}
		} else {
			plan.EVSECommands = append(plan.EVSECommands, EVSECommand{
				StationID:   ev.StationID,
				ConnectorID: ev.ConnectorID,
				MaxCurrentA: limitA,
			})
			plan.AddDecision("csip-aus/load-limit",
				fmt.Sprintf("gross load cap %.0fW: EVSE %s held at %.1fA (sticky ceiling)",
					loadLimitW, ev.StationID, limitA),
				fmt.Sprintf("EVSE %s → %.1fA", ev.StationID, limitA))
		}
		return // single-EV pattern: only the first active session is managed
	}
}

// checkAusLoadConvergence is the measured-effect backstop for the CSIP-AUS
// gross-load cap, mirroring checkImportConvergence (its template) — the same
// NaN-HOLD semantics, leaky counter, and plan.Breach==nil gate. Unlike a
// generation cap (always satisfiable by curtailing PV), a load cap can be
// genuinely unmeetable — the home load alone can exceed it and the hub has no
// lever on home load — so CannotComply is the correct outcome.
func (o *DefaultOptimizer) checkAusLoadConvergence(solar []SolarState, batteries []BatteryState, netW, loadLimitW float64, plan *Plan) {
	if math.IsNaN(loadLimitW) {
		o.ausLoadGuard.breachTicks = 0 // cap cleared — reset
		return
	}
	// A meter-blind tick (netW NaN) is evidence of nothing: neither breach
	// nor compliance. HOLD the counter rather than resetting it, or a single
	// blind tick inside a sustained breach silently restarts the whole
	// escalation and the CannotComply loses the race against the constraint
	// window — verbatim NaN-hold semantics from checkImportConvergence
	// (HARD preserve; QA 2026-07-01: battery-soc-refuse).
	if math.IsNaN(netW) {
		return
	}

	solarW := 0.0
	for _, sol := range solar {
		if sol.Connected && sol.PowerW > 0 {
			solarW += sol.PowerW
		}
	}
	battDischargeW := 0.0
	for _, b := range batteries {
		if b.Connected && b.PowerW > 0 {
			battDischargeW += b.PowerW
		}
	}
	grossLoadW := math.Max(0, solarW+battDischargeW+netW)

	// Leaky counter, matching checkImportConvergence: a single under-cap
	// sample decrements instead of zeroing, so a sustained breach with
	// occasional noise still escalates while genuine convergence drains the
	// counter within a few ticks.
	threshold := o.scaleTicks(ausLoadBreachTicks)
	if grossLoadW > loadLimitW+complianceBreachW {
		if o.ausLoadGuard.breachTicks < threshold {
			o.ausLoadGuard.breachTicks++ // cap at the threshold so it drains fast on recovery
		}
	} else if o.ausLoadGuard.breachTicks > 0 {
		o.ausLoadGuard.breachTicks--
	}
	if o.ausLoadGuard.breachTicks >= threshold && plan.Breach == nil {
		o.recordBreach(plan, &ComplianceBreach{
			LimitType:  "load-aus",
			LimitW:     loadLimitW,
			MeasuredW:  grossLoadW,
			ShortfallW: grossLoadW - loadLimitW,
			Reason:     "gross load remains over the CSIP-AUS load cap after charge-shed and EV curtailment — the remaining load is not sheddable or a device is not honouring its command",
		})
		plan.AddDecision("csip-aus/load-limit",
			fmt.Sprintf("gross load %.0fW still over cap %.0fW after %d ticks (~%.0fs)",
				grossLoadW, loadLimitW, o.ausLoadGuard.breachTicks, float64(o.ausLoadGuard.breachTicks)*o.tickSeconds()),
			"reporting CannotComply — site not converging to the CSIP-AUS load cap")
	}
}

// evseCommandIndex returns the index of the command for the station/connector,
// or −1 if absent. The index-returning sibling of hasEVSECommand, mirroring
// batteryCommandIndex/solarCommandIndex.
func evseCommandIndex(cmds []EVSECommand, stationID string, connectorID int) int {
	for i := range cmds {
		if cmds[i].StationID == stationID && cmds[i].ConnectorID == connectorID {
			return i
		}
	}
	return -1
}
