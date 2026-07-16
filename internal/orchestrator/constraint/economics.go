package constraint

import (
	"math"
	"time"

	"lexa-hub/internal/orchestrator"
	"lexa-hub/internal/utilitytime"
)

// EconomicsConstraint is the TierEconomics migration of the legacy cost-optimal
// dispatch rules (TASK-063, R4's second half). It ports, in ONE constraint at the
// lowest priority band, the four economic proposal sources the cascade scatters
// through DefaultOptimizer.Optimize:
//
//   - Rule 2   fixed dispatch    (applyFixedDispatchRule, optimizer.go:585-641)
//   - Rule 2.5 plan following    (applyPlanRule,          optimizer.go:503-580)
//   - Rule 4   self-consumption  (applySelfConsumptionRule, optimizer.go:1605-1680)
//   - Rule 5   TOU peak discharge(applyDemandResponseRule,  optimizer.go:1688-1738)
//   - Rule 6   EV charging alloc (applyEVChargingRule,      optimizer.go:1750-1919)
//
// # Economics PROPOSE, constraints DISPOSE (AD-007)
//
// Every setpoint this constraint emits is a PROPOSAL: a battery net-power point,
// an EV current point. They are PointDemands (Min==Max) at TierEconomics. The
// arbiter intersects them UNDER the safety and compliance tiers, so a proposal
// can never widen — or step outside — a bound a higher tier set (structural, per
// the tier-aware arbiter, TASK-063). A TOU discharge that would exceed an active
// import cap's battery-discharge point is clamped to that point; a self-consumption
// charge that contradicts a compliance discharge is clamped to the compliance
// value. Economics is physically unable to violate a cap — see
// TestEconomics_*ClampedBy* and economics_arbitration_test.go.
//
// # Internal precedence mirrors the legacy if-nesting
//
// The cascade encodes economic precedence as if-guards inside one function:
// fixed-dispatch and plan-following command a battery first, and Rules 4/5/6 skip
// a device already commanded (hasBatteryCommand / hasEVSECommand); Rules 4/5/6 are
// suppressed entirely when a plan was followed; plan-following is suppressed under
// CSIP OpModFixedW. That EXACT structure is reproduced here, INSIDE the one
// constraint (an ecoPlan tracks which devices this layer has already committed),
// so a full-stack shadow diff isolates tier-seam changes from intra-economics
// changes (task implementation strategy).
//
// # What is NOT reproduced (owned by the compliance interleaving → TASK-064)
//
// In the cascade the compliance rules (export/import/gen) run BETWEEN the economic
// rules and mutate the shared surplusW / battery PowerW / plan the later economic
// rules read. A below-compliance economics layer cannot see those mutations: it
// computes surplusW from raw state (computePowerBalance) and threads only its OWN
// prior sub-rule commands. When NO CSIP limit is active the compliance rules are
// no-ops, so economics is faithful to the cascade; when a cap IS active the
// surplus/headroom the cascade fed downstream differs, and economics diverges.
// That divergence is EXPECTED and is the characterized finding for TASK-064
// (constants→plant / shared-state owner) — it is not forced to bit-match here.
// The one genuinely cross-tier piece the rules need, the EV import-cooldown
// (evSafeCount, import-session-owned after TASK-061), is now the SHARED
// EVImportCooldown (TASK-064): the import constraint writes it, economics reads it,
// so the battery-empty-import-cap suspension (a HARD-preserve invariant) is
// preserved with ONE counter instead of a divergent economics-local copy.
type EconomicsConstraint struct {
	// cooldown is the SHARED EV-resume cooldown (TASK-064): written by the import
	// constraint (compliance tier, runs first), READ here to gate EV resumption.
	// nil ⇒ no import writer wired (standalone economics unit tests with no import
	// cap) ⇒ never suppressed, matching legacy where no import rule ran.
	cooldown *EVImportCooldown

	// costModel drives Rule 5 TOU peak detection. nil ⇒ TOU never fires (matching
	// DefaultOptimizer with a nil CostModel).
	costModel *orchestrator.TOUCostModel

	// Ported DefaultOptimizer config (the bench values; parameterisation is
	// TASK-064). NewEconomicsConstraint sets them from cmd/hub, mirroring the opt.*
	// assignments in main.go.
	socReserve             float64 // SOCReserve (default 20)
	socFull                float64 // SOCFullThreshold (default 95)
	excessSolarThreshold   float64 // ExcessSolarThreshold (default 100)
	exportMarginFrac       float64 // ExportMarginFrac (default 0.20) — caps TOU discharge
	evImportCooldownCycles int     // EVImportCooldownCycles (tick-denominated)
}

// compile-time proof EconomicsConstraint satisfies the Constraint interface.
var _ Constraint = (*EconomicsConstraint)(nil)

const (
	// ecoMinChargeA is the IEC 61851-1 minimum AC charge current (optimizer.go:1751).
	ecoMinChargeA = 6.0
	// ecoDefaultVoltage is the fallback EVSE supply voltage (optimizer.go:1777).
	ecoDefaultVoltage = 230.0
	// ecoMinBatteryW is the floor below which a battery command is not worth
	// emitting — the "< 50 W" guard the cascade uses throughout.
	ecoMinBatteryW = 50.0
)

// NewEconomicsConstraint builds the economics constraint with the bench-default
// DefaultOptimizer config. cmd/hub passes the same values it assigns to opt.*. cd is
// the shared EV-resume cooldown the import constraint writes and this constraint
// reads (TASK-064); nil is tolerated for standalone unit tests with no import cap.
func NewEconomicsConstraint(costModel *orchestrator.TOUCostModel, socReserve, socFull, excessSolar, exportMargin float64, evCooldownCycles int, cd *EVImportCooldown) *EconomicsConstraint {
	return &EconomicsConstraint{
		cooldown:               cd,
		costModel:              costModel,
		socReserve:             socReserve,
		socFull:                socFull,
		excessSolarThreshold:   excessSolar,
		exportMarginFrac:       exportMargin,
		evImportCooldownCycles: evCooldownCycles,
	}
}

// Name is the stable identity; it keys the Session and appears as Demand.Source.
func (c *EconomicsConstraint) Name() string { return "economics" }

// Tier places all four economic rules in the lowest band: they propose a working
// point inside whatever the safety and compliance tiers leave admissible.
func (c *EconomicsConstraint) Tier() Tier { return TierEconomics }

// Evaluate runs the four economic proposal sources in the legacy precedence order
// and emits their setpoints as TierEconomics PointDemands. It never returns a
// ComplianceBreach — economics does not own a CSIP limit; it only proposes.
func (c *EconomicsConstraint) Evaluate(in Input, s *Session) ([]Demand, *orchestrator.ComplianceBreach) {
	st := in.State

	// CSIP cease-to-energize (Rule 1) early-returns the whole cascade BEFORE any
	// economic rule runs (optimizer.go:266-268). Economics must not propose under a
	// disconnect order — the disconnect commands themselves are not an economics
	// concern (they stay legacy-authored until their own migration).
	if cc := st.CSIPControl; cc != nil && cc.Base.OpModConnect != nil && !*cc.Base.OpModConnect {
		return nil, nil
	}

	e := newEcoPlan(c.Name())

	// The shared reserve-floor gate for this tick (audit B-1) — the hysteretic
	// latch when the Stack wired it, else the instantaneous fallback for unit
	// tests. Threaded into every discharge sub-rule below, mirroring the legacy
	// cascade's `blocked` closure.
	blocked := reserveBlocker(in, c.socReserve)

	solarW := st.TotalSolarW()
	evseW := st.TotalEVSEW()
	surplusW := economicSurplusW(st, solarW, evseW)
	homeLoadW := st.InferredLoadW()

	// Thread a mutable battery copy so each sub-rule sees PowerW updated by the
	// prior sub-rules, exactly as Optimize threads `batteries` (optimizer.go:281-282).
	batteries := make([]orchestrator.BatteryState, len(st.Batteries))
	copy(batteries, st.Batteries)

	// Effective CSIP limits (compliance overrides folded onto grid-reported values),
	// reused from the compliance constraints' helpers.
	exportLimitW := effectiveExportLimitW(st)
	importLimitW := effectiveImportLimitW(st)

	// EV import cooldown: the import constraint (compliance tier) already advanced the
	// SHARED counter THIS tick before economics runs (Stack evaluates compliance
	// before economics), matching the legacy order where applyImportLimitRule advances
	// evSafeCount before applyEVChargingRule gates on it. Economics only READS it below
	// (TASK-064 single owner) — no local advance here.

	// Rule 2: CSIP fixed dispatch — discharge battery to meet an explicit export request.
	batteries = c.applyFixedDispatch(st.CSIPControl, batteries, solarW, homeLoadW, blocked, e)

	// Rule 2.5: follow the 24 h cost-optimal plan, unless CSIP mandates fixed dispatch.
	// An active export limit caps the plan's discharge guidance (audit EXPCAP-1/2):
	// the export constraint never reduces a plan-commanded discharge, so a stale
	// plan built before the cap arrived would export past it — the same
	// planExportDischargeCapW bound the legacy cascade applies at optimizer.go.
	planDischargeCapW := c.planExportDischargeCapW(st)
	planFollowed := false
	if st.CSIPControl == nil || st.CSIPControl.Base.OpModFixedW == nil {
		batteries, surplusW, planFollowed = c.applyPlan(st.DailyPlanTarget, batteries, st.EVSEs, surplusW, planDischargeCapW, blocked, e)
	}

	if !planFollowed {
		// Rule 4: self-consumption — route solar surplus into the battery.
		batteries, surplusW = c.applySelfConsumption(batteries, surplusW, e)

		// Rule 5: TOU peak discharge (autonomous peak shifting; CSIP fixed dispatch
		// is Rule 2). serverNow uses the tick's server time verbatim (AD-004): the
		// same now.Unix()+ClockOffset arithmetic, single-owned by utilitytime — a
		// pure call, no wall-clock read (HARD-preserve clock leg, TASK-063).
		batteries, surplusW = c.applyTOU(st, batteries, surplusW, exportLimitW, blocked, e)

		// Rule 6: EV charging allocation — remaining budget to EVSEs.
		c.applyEVCharging(st.EVSEs, exportLimitW, importLimitW, st.Grid.NetW, solarW, surplusW, e)
	}

	return e.demands, nil
}

// economicSurplusW reproduces computePowerBalance's surplus leg (optimizer.go:482-493):
// solar above home load, i.e. export available for battery/grid; solarW when no
// grid meter is present.
func economicSurplusW(st orchestrator.SystemState, solarW, evseW float64) float64 {
	if !math.IsNaN(st.Grid.NetW) {
		return -st.Grid.NetW - evseW
	}
	return solarW
}

// ── ecoPlan: the economics layer's own command-precedence tracker ───────────────

// ecoPlan accumulates this tick's economics demands and tracks which devices the
// layer has already committed, so a later sub-rule can honour the legacy
// hasBatteryCommand / hasEVSECommand "first writer wins" guards WITHIN economics.
type ecoPlan struct {
	demands   []Demand
	source    string
	batteries map[string]bool
	evses     map[string]bool
}

func newEcoPlan(source string) *ecoPlan {
	return &ecoPlan{source: source, batteries: map[string]bool{}, evses: map[string]bool{}}
}

// hasBattery reports whether an earlier economics sub-rule already commanded this
// pack (the intra-economics analogue of hasBatteryCommand, optimizer.go:2254).
func (e *ecoPlan) hasBattery(name string) bool { return e.batteries[name] }

// setBattery emits a battery setpoint proposal and marks the pack committed.
func (e *ecoPlan) setBattery(name string, setpointW float64) {
	e.demands = append(e.demands, PointDemand(name, AxisBatterySetpointW, setpointW, TierEconomics, e.source))
	e.batteries[name] = true
}

// hasEVSE reports whether an earlier economics sub-rule already commanded this
// connector (the analogue of hasEVSECommand, optimizer.go:2282).
func (e *ecoPlan) hasEVSE(station string, connector int) bool {
	return e.evses[evseKey(station, connector)]
}

// setEVSE emits an EV current proposal (keyed station#connector so the Stack can
// carry the OCPP connector, stack.go parseEVSEDevice) and marks it committed.
func (e *ecoPlan) setEVSE(station string, connector int, currentA float64) {
	key := evseKey(station, connector)
	e.demands = append(e.demands, PointDemand(key, AxisEVSECurrentA, currentA, TierEconomics, e.source))
	e.evses[key] = true
}

// ── Rule 2: CSIP fixed dispatch ─────────────────────────────────────────────────

// applyFixedDispatch ports applyFixedDispatchRule (optimizer.go:585-641): solar is
// credited toward the OpModFixedW export request first, batteries cover the
// shortfall up to their SOC reserve.
func (c *EconomicsConstraint) applyFixedDispatch(cc *orchestrator.CSIPControlState, batteries []orchestrator.BatteryState, solarW, homeLoadW float64, blocked func(orchestrator.BatteryState) bool, e *ecoPlan) []orchestrator.BatteryState {
	if cc == nil || cc.Base.OpModFixedW == nil {
		return batteries
	}
	targetW := apW(cc.Base.OpModFixedW)

	var availableW float64
	if !math.IsNaN(homeLoadW) {
		availableW = math.Max(0, solarW-homeLoadW)
	} else {
		availableW = solarW
	}
	if availableW >= targetW {
		return batteries // solar covers the request; no battery discharge needed
	}

	shortfallW := targetW - availableW
	for i, b := range batteries {
		if !b.Connected || !b.Energized {
			continue
		}
		if blocked(b) {
			continue // protect reserve (hysteretic latch)
		}
		if e.hasBattery(b.Name) {
			continue
		}
		available := b.AvailableDischargeW()
		if available < ecoMinBatteryW {
			continue
		}
		dispatchW := math.Min(available, shortfallW)
		newSetpoint := b.PowerW + dispatchW
		e.setBattery(b.Name, newSetpoint)
		batteries[i].PowerW = newSetpoint
		shortfallW -= dispatchW
		if shortfallW <= 1 {
			break
		}
	}
	return batteries
}

// planExportDischargeCapW mirrors DefaultOptimizer.planExportDischargeCapW
// (audit EXPCAP-1/2): the maximum AGGREGATE plan discharge that will not drive
// meter export past the active export limit, or NaN when none is active. Base
// export is signed net export minus the SIGNED battery power now flowing (a
// charging pack draws from the site, so its negative power is added back out of
// the base — not treated as immovable load). Kept identical to the legacy bound
// so the shadow diff stays clean.
func (c *EconomicsConstraint) planExportDischargeCapW(st orchestrator.SystemState) float64 {
	exportLimitW := effectiveExportLimitW(st)
	if math.IsNaN(exportLimitW) {
		return math.NaN()
	}
	margin := c.exportMarginFrac
	if margin <= 0 {
		margin = exportMarginFrac // package default (0.20)
	}
	conservativeW := exportLimitW * (1 - margin)
	baseExportW := 0.0
	if !math.IsNaN(st.Grid.NetW) {
		signedNetExportW := -st.Grid.NetW
		measuredBatterySignedW := 0.0
		for _, b := range st.Batteries {
			if b.Connected && !math.IsNaN(b.PowerW) {
				measuredBatterySignedW += b.PowerW
			}
		}
		baseExportW = signedNetExportW - measuredBatterySignedW
	}
	return math.Max(0, conservativeW-baseExportW)
}

// ── Rule 2.5: plan following ────────────────────────────────────────────────────

// applyPlan ports applyPlanRule (optimizer.go:503-580): distribute the DP planner's
// battery setpoint across connected packs proportionally to their power rating
// (with the live-SOC safety clamp), set EV current on active sessions, and zero
// surplusW so Rules 4/5 do not fire. Returns (batteries, surplusW, planFollowed).
func (c *EconomicsConstraint) applyPlan(target *orchestrator.PlanTarget, batteries []orchestrator.BatteryState, evses []orchestrator.EVSEState, surplusW, planDischargeCapW float64, blocked func(orchestrator.BatteryState) bool, e *ecoPlan) ([]orchestrator.BatteryState, float64, bool) {
	if target == nil || math.IsNaN(target.BattSetpointW) {
		return batteries, surplusW, false
	}
	setW := target.BattSetpointW

	// Clamp a planned DISCHARGE to the export-limit headroom before distributing
	// it across packs — mirrors optimizer.go's applyPlanRule (audit EXPCAP-2). A
	// charge plan (setW < 0) is never capped (it reduces export); NaN cap ⇒ none.
	if setW > 0 && !math.IsNaN(planDischargeCapW) && setW > planDischargeCapW {
		setW = planDischargeCapW
	}

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
		share = math.Max(-b.MaxChargeW, math.Min(b.MaxDischargeW, share))
		// Live-SOC safety clamp (optimizer.go): a stale plan setpoint must never
		// discharge while the reserve latch holds or charge above full on the
		// measured SOC. The discharge side is the hysteretic latch, not a bare
		// SOC ≤ reserve, so dither cannot re-authorize discharge (audit B-1).
		if share > 0 && blocked(b) {
			share = 0
		} else if share < 0 && !math.IsNaN(b.SOC) && b.SOC >= c.socFull {
			share = 0
		}
		batteries[i].PowerW = share
		e.setBattery(b.Name, share)
	}

	// EV current from the plan; only override active sessions (optimizer.go:562-572).
	for _, ev := range evses {
		if ev.Connected && ev.SessionActive {
			e.setEVSE(ev.StationID, ev.ConnectorID, target.EVMaxCurrentA)
		}
	}

	// Zero surplusW so self-consumption and TOU do not fire after us (optimizer.go:579).
	return batteries, 0, true
}

// ── Rule 4: self-consumption ────────────────────────────────────────────────────

// applySelfConsumption ports applySelfConsumptionRule (optimizer.go:1605-1680):
// route solar surplus into connected batteries, holding (re-issuing) an already
// sufficient charge rate rather than escalating it each tick on a lagging meter.
func (c *EconomicsConstraint) applySelfConsumption(batteries []orchestrator.BatteryState, surplusW float64, e *ecoPlan) ([]orchestrator.BatteryState, float64) {
	for i, b := range batteries {
		if !b.Connected || !b.Energized {
			continue
		}
		if e.hasBattery(b.Name) {
			continue
		}
		if !math.IsNaN(b.SOC) && b.SOC >= c.socFull {
			continue // battery full — skip charging
		}

		alreadyAbsorbingW := 0.0
		if b.PowerW < 0 {
			alreadyAbsorbingW = -b.PowerW
		}
		additionalSurplus := math.Max(0, surplusW-alreadyAbsorbingW)

		if additionalSurplus < c.excessSolarThreshold {
			// Already covering the surplus: re-issue the current setpoint so the
			// restore rule does not idle it, but do not escalate (optimizer.go:1632-1647).
			if alreadyAbsorbingW > 0 && surplusW > 0 {
				e.setBattery(b.Name, b.PowerW)
				batteries[i].PowerW = b.PowerW
				surplusW -= alreadyAbsorbingW
			}
			continue
		}

		headroom := b.AvailableChargeW()
		absorb := math.Min(headroom, additionalSurplus)
		if absorb < ecoMinBatteryW {
			// At capacity — hold current rate (optimizer.go:1652-1667).
			if alreadyAbsorbingW > 0 && surplusW > 0 {
				e.setBattery(b.Name, b.PowerW)
				surplusW -= alreadyAbsorbingW
				batteries[i].PowerW = b.PowerW
			}
			continue
		}
		newSetpoint := b.PowerW - absorb
		e.setBattery(b.Name, newSetpoint)
		surplusW -= absorb + alreadyAbsorbingW
		batteries[i].PowerW = newSetpoint
	}
	return batteries, surplusW
}

// ── Rule 5: TOU peak discharge ──────────────────────────────────────────────────

// applyTOU ports the TOU leg of Optimize (optimizer.go:321-349) + applyDemandResponseRule
// (optimizer.go:1688-1738, isDR=false). It discharges batteries during peak TOU
// hours, capping the total discharge so it cannot push export past an active
// export limit before the compliance layer's next-tick correction.
func (c *EconomicsConstraint) applyTOU(st orchestrator.SystemState, batteries []orchestrator.BatteryState, surplusW, exportLimitW float64, blocked func(orchestrator.BatteryState) bool, e *ecoPlan) ([]orchestrator.BatteryState, float64) {
	if c.costModel == nil {
		return batteries, surplusW
	}
	serverNow := time.Unix(utilitytime.ServerNowAt(st.Timestamp, st.ClockOffset), 0)
	if !c.costModel.IsPeakHour(serverNow) {
		return batteries, surplusW
	}

	// dischargeCapW caps the total discharge to the export headroom (optimizer.go:335-348).
	dischargeCapW := math.NaN()
	if !math.IsNaN(exportLimitW) {
		margin := c.exportMarginFrac
		if margin <= 0 {
			margin = 0.20
		}
		exportNowW := 0.0
		if !math.IsNaN(st.Grid.NetW) {
			exportNowW = math.Max(0, -st.Grid.NetW)
		} else {
			exportNowW = math.Max(0, surplusW)
		}
		dischargeCapW = math.Max(0, exportLimitW*(1-margin)-exportNowW)
	}

	capped := !math.IsNaN(dischargeCapW)
	remainingW := dischargeCapW
	for i, b := range batteries {
		if !b.Connected || !b.Energized {
			continue
		}
		if blocked(b) {
			continue // protect reserve (hysteretic latch)
		}
		available := b.AvailableDischargeW()
		if available < ecoMinBatteryW {
			continue
		}
		setpoint := b.MaxDischargeW
		if capped {
			if remainingW < ecoMinBatteryW {
				continue // export-limit headroom exhausted
			}
			setpoint = math.Min(setpoint, remainingW)
		}
		if e.hasBattery(b.Name) {
			continue
		}
		e.setBattery(b.Name, setpoint)
		surplusW += setpoint - b.PowerW
		batteries[i].PowerW = setpoint
		if capped {
			remainingW -= setpoint
		}
	}
	return batteries, surplusW
}

// ── Rule 6: EV charging allocation ──────────────────────────────────────────────

// applyEVCharging ports applyEVChargingRule (optimizer.go:1750-1919): distribute
// the remaining power budget across active EVSEs, honouring the import-limit
// suspend, the import cooldown, solar-surplus throttling, and the minimum-charge
// grid supplement. evImportSuppressed is reproduced from the economics-local
// cooldown counter (see the type doc).
func (c *EconomicsConstraint) applyEVCharging(evses []orchestrator.EVSEState, exportLimitW, importLimitW, netW, solarW, surplusW float64, e *ecoPlan) {
	evImportSuppressed := !math.IsNaN(importLimitW) && c.cooldown != nil && c.cooldown.Suppressed(c.evImportCooldownCycles)

	for _, evse := range evses {
		if !evse.Connected || !evse.SessionActive {
			continue
		}
		if e.hasEVSE(evse.StationID, evse.ConnectorID) {
			continue
		}

		// Hold EV at zero while the import guard is cooling down (optimizer.go:1762-1774).
		if evImportSuppressed {
			e.setEVSE(evse.StationID, evse.ConnectorID, 0)
			continue
		}

		voltage := evse.VoltageV
		if voltage <= 0 {
			voltage = ecoDefaultVoltage
		}
		maxPowerW := evse.MaxCurrentA * voltage
		minChargeW := ecoMinChargeA * voltage

		// Suspend if grid import is already at/above the limit (optimizer.go:1784-1795).
		if !math.IsNaN(importLimitW) && !math.IsNaN(netW) && netW >= importLimitW {
			e.setEVSE(evse.StationID, evse.ConnectorID, 0)
			continue
		}

		// No grid constraint: full rate, throttled to solar budget when producing
		// (optimizer.go:1808-1829).
		if math.IsNaN(exportLimitW) && math.IsNaN(importLimitW) {
			targetA := evse.MaxCurrentA
			if solarW > 0 {
				evBudgetW := surplusW + evse.PowerW
				budgetA := evBudgetW / voltage
				if budgetA < targetA {
					targetA = math.Max(ecoMinChargeA, budgetA)
				}
			}
			e.setEVSE(evse.StationID, evse.ConnectorID, targetA)
			continue
		}

		// Export limit active but site is importing: charge at full rate
		// (optimizer.go:1834-1845).
		if !math.IsNaN(exportLimitW) && !math.IsNaN(netW) && netW >= 0 {
			e.setEVSE(evse.StationID, evse.ConnectorID, evse.MaxCurrentA)
			continue
		}

		if solarW > 0 && surplusW < maxPowerW {
			budgetW := math.Max(0, surplusW)

			// Export limit active + solar below minimum charge: supplement from grid
			// unless the import limit forbids it (optimizer.go:1852-1881).
			if !math.IsNaN(exportLimitW) && budgetW > 0 && budgetW < minChargeW {
				supplementW := minChargeW - budgetW
				importHeadroom := math.Inf(1)
				if !math.IsNaN(importLimitW) && !math.IsNaN(netW) {
					importHeadroom = importLimitW - netW
				}
				if supplementW <= importHeadroom {
					e.setEVSE(evse.StationID, evse.ConnectorID, ecoMinChargeA)
				} else {
					e.setEVSE(evse.StationID, evse.ConnectorID, 0)
				}
				continue
			}

			limitA := budgetW / voltage
			if limitA < ecoMinChargeA {
				e.setEVSE(evse.StationID, evse.ConnectorID, 0)
			} else {
				limitA = math.Min(limitA, evse.MaxCurrentA)
				e.setEVSE(evse.StationID, evse.ConnectorID, limitA)
			}
			continue
		}

		// Sufficient power available: full rate (optimizer.go:1907-1917).
		e.setEVSE(evse.StationID, evse.ConnectorID, evse.MaxCurrentA)
	}
}
