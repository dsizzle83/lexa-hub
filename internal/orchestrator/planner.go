package orchestrator

import (
	"math"
	"time"
)

const (
	planSteps     = 288           // 24 h / 5-min intervals
	planStepSec   = int64(5 * 60) // seconds per interval
	planStepHours = 5.0 / 60.0    // hours per interval
)

// PlannerParams is the full input to DailyPlanner.Plan.
// All zero-value fields receive sensible defaults via withDefaults().
type PlannerParams struct {
	// Now is the caller's current time, stamped when ReadSystemState was called.
	// Zero → Plan uses time.Now() as a fallback.
	Now time.Time

	// WindowStart is the first interval start (Unix s); snapped to 5-min boundary.
	WindowStart int64

	// Battery asset. BattCapacityKwh == 0 → no battery modelled.
	BattCapacityKwh    float64
	BattMaxChargeKw    float64
	BattMaxDischargeKw float64
	BattEfficiency     float64 // one-way efficiency [0,1]; default 0.96
	InitialBattSocKwh  float64
	MinBattSocKwh      float64 // operating floor; 0 → 10% of capacity
	MaxBattSocKwh      float64 // operating ceiling; 0 → BattCapacityKwh

	// TerminalSocKwh is the target final battery SOC for the end-of-horizon
	// penalty. 0 → defaults to InitialBattSocKwh (no net daily discharge). Set it
	// below initial to let the battery run down to a reserve across an expensive
	// evening; with receding-horizon replanning the end-of-window dump never
	// actually executes (the present action is always near the start of a fresh
	// 24 h window).
	TerminalSocKwh float64

	// EV asset. EVCapacityKwh == 0 → no EV modelled.
	EVCapacityKwh   float64
	EVMaxChargeKw   float64
	EVEfficiency    float64 // default 0.95
	InitialEVSocKwh float64
	EVTargetSocKwh  float64 // required SOC at departure; 0 → no constraint
	EVDepartureUnix int64   // 0 → no departure constraint
	EVVoltageV      float64 // for A→W conversion; default 240

	// SolarForecastKw is the per-step solar generation (kW); len may be < planSteps.
	// Missing steps treated as zero.
	SolarForecastKw []float64

	// LoadForecastKw is the expected site load (kW) excluding EV. Used as the
	// flat/scalar load for every step when LoadProfileKw is empty.
	LoadForecastKw float64

	// LoadProfileKw is the per-step site load (kW) on the 288-slot 5-min grid,
	// excluding EV. Empty ⇒ the scalar LoadForecastKw is used for every step;
	// a missing/out-of-range step falls back to LoadForecastKw too (planStepLoad).
	LoadProfileKw []float64

	// ImportPricePerKwh and ExportPricePerKwh are $/kWh per step.
	// nil → FallbackTOU is used.
	ImportPricePerKwh []float64
	ExportPricePerKwh []float64
	FallbackTOU       *TOUCostModel // used when price slices are nil

	// DeliveryTOU is an optional volumetric delivery/distribution charge
	// ($/kWh) modelled as an orthogonal adder on the IMPORT price only: you
	// pay delivery on every imported kWh, never on export. nil ⇒ no delivery
	// charge (the shipped default; withDefaults needs nothing for it). It is
	// evaluated at the SAME wall-clock hour as the supply import price
	// (planStepDeliveryPrice reuses planStepImportPrice's local-time
	// derivation), so a delivery tariff written in the SOM's zone lines up
	// slot-for-slot with the supply tariff. Folded into the all-in import
	// price (planStepImportAllIn) at both the forward DP objective and the
	// backtrack MarginalCost mirror, so the DP optimises against — and reports
	// MarginalCost at — the true all-in import cost. The export branch is
	// untouched.
	DeliveryTOU *TOUCostModel

	// FixedDailyCharge is a flat $/day charge (e.g. a fixed distribution or
	// service fee) added ONCE to DailyPlan.TotalCost — the horizon is exactly
	// 24 h = 1 day. 0 ⇒ none (the correct no-op default). Being a constant that
	// does not vary with dispatch, it deliberately does NOT enter any per-slot
	// MarginalCost and never shifts a setpoint; it only raises the reported
	// daily total.
	FixedDailyCharge float64

	// DERConstraints holds per-step DER operating constraints from the northbound
	// schedule. nil or short → unconstrained.
	DERConstraints []StepConstraint

	// SOCStepKwh is the DP discretisation step. Default 0.5 kWh.
	SOCStepKwh float64

	// EVStorage is hub.json's `ev_storage` flag (D8/WP-14, default false).
	// false (the acceptance-gated default): the EV action space is
	// charge-only/non-negative, exactly as before this field existed —
	// EVERY code path this flag guards is a no-op, so Plan's output is
	// byte-identical (pinned by TestPlan_EVStorage_FlagOff_ByteIdentical).
	// true: the EV becomes a bidirectional DP asset like the battery —
	// discretizeEVCurrents' charge-side IEC 61851 levels are mirrored
	// negative (discharge, symmetric charger rating — see
	// mirrorSignedLevels), the SOC transition applies the discharge-side
	// efficiency split (mirrors battDest's charge/discharge asymmetry),
	// and EV discharge participates in the CSIP-AUS GenLimW/LoadLimW gross
	// generation/load math as a DER (mirrors how battery discharge already
	// does). EVGoal departure/target SOC/capacity honoring is UNCHANGED
	// either way — this flag only widens the ACTION SPACE, never the goal
	// enforcement.
	EVStorage bool
}

// StepConstraint is the DER control envelope for one 5-min interval.
// NaN values mean unconstrained.
type StepConstraint struct {
	Disconnect bool    // true → battery must be offline (OpModConnect=false)
	ExpLimW    float64 // max net export to grid (W)
	ImpLimW    float64 // max net import from grid (W)
	MaxLimW    float64 // total generation cap (W)
	FixedW     float64 // fixed battery dispatch (W); NaN → DP chooses freely

	// CSIP-AUS dynamic-envelope axes (WP-11), carried on DERScheduleSlot
	// since WP-8. GenLimW caps gross generation (solar + battery discharge);
	// LoadLimW caps gross load (home + EV + battery charge). NaN =
	// unconstrained — construction sites MUST set NaN explicitly (a zero
	// value is a real 0 W cap, not absence), same as every field above.
	GenLimW  float64 // max gross generation (W) — opModGenLimW
	LoadLimW float64 // max gross load (W) — opModLoadLimW
}

// PlanInterval is the cost-optimised dispatch target for one 5-min slot.
type PlanInterval struct {
	Start         int64   // interval start (Unix s)
	BattSetpointW float64 // + discharge, − charge; NaN = no battery
	EVMaxCurrentA float64 // 0 = suspend EV; NEVER negative (see EVSetpointW)
	ExpectedGridW float64 // + import, − export
	MarginalCost  float64 // net cost for this interval (negative = earning)
	// SocKwh is the planned battery state of charge (kWh) at the START of this
	// slot — the DP's own tracked source-level SOC for the chosen path, snapped
	// to the SOC discretisation grid (SOCStepKwh), NOT a re-derived integration.
	// It is the value backtracking reads off battLevels[srcBattIdx], so it is
	// exactly the SOC the DP costed this slot's dispatch against. NaN when there
	// is no battery asset (mirrors BattSetpointW's NaN sentinel) or, defensively,
	// when a slot has no recorded predecessor. GAP-7: surfaced (via
	// Engine.DailyPlanSnapshot) so the app's battery-plan chart can render
	// planned soc_pct per slot. Additive/back-compat: existing PlanInterval
	// constructions use keyed fields, so this zero-value-safe addition leaves
	// them unchanged.
	SocKwh float64

	// EVSetpointW is the D8/WP-14 signed EV power counterpart to
	// BattSetpointW: + discharge to site, − charge (the same battery sign
	// convention EVSECommand.SetpointW/bus.DesiredState.SetpointW carry).
	// NaN when there is no EV asset (mirrors BattSetpointW's NaN-no-battery
	// sentinel); otherwise ALWAYS populated — including in the charge-only
	// (EVStorage false) universe, where it is guaranteed <= 0 (never a
	// discharge value, since the underlying DP current can only go
	// negative when EVStorage was true when this plan was built). This is
	// what carries a genuine V2G discharge decision to
	// optimizer.go's applyPlanRule → EVSECommand.SetpointW; EVMaxCurrentA
	// keeps its pre-D8 never-negative ceiling-mode contract regardless.
	EVSetpointW float64
}

// DailyPlan is the 24-hour cost-optimal plan produced by DailyPlanner.Plan.
type DailyPlan struct {
	BuildTime   time.Time
	WindowStart int64 // Unix s
	WindowEnd   int64 // Unix s
	Intervals   [planSteps]PlanInterval
	TotalCost   float64
	// EVModelled is true when this plan was built with a usable EV action space
	// (EVCapacityKwh > 0 AND EVMaxChargeKw > 0). When false, every interval's
	// EVMaxCurrentA is a FORCED 0 (the DP had no charge lever), NOT a cost-optimal
	// "charge later" decision — so applyPlanRule must NOT push that 0 A as a
	// standing EV suspend; the reactive EV rule handles the session instead
	// (audit A-2/H6). Plan-level constant.
	EVModelled bool
}

// PlanTarget is the current-interval directive extracted from a DailyPlan.
// It is injected into SystemState by the Engine so the reactive optimizer
// can follow it while hard CSIP overrides still take precedence.
type PlanTarget struct {
	BattSetpointW float64
	EVMaxCurrentA float64
	// EVSetpointW is PlanInterval.EVSetpointW's counterpart here — see that
	// field's doc (D8/WP-14).
	EVSetpointW float64
	// EVModelled mirrors DailyPlan.EVModelled: false ⇒ EVMaxCurrentA=0 is a
	// forced no-EV-lever value, not a plan decision, so applyPlanRule must not
	// push it as a standing suspend (audit A-2/H6).
	EVModelled bool
}

// CurrentTarget returns the PlanTarget for the interval containing t,
// or nil when t is outside the plan window.
func (dp *DailyPlan) CurrentTarget(t time.Time) *PlanTarget {
	if dp == nil {
		return nil
	}
	unix := t.Unix()
	if unix < dp.WindowStart || unix >= dp.WindowEnd {
		return nil
	}
	idx := int((unix - dp.WindowStart) / planStepSec)
	if idx < 0 || idx >= planSteps {
		return nil
	}
	iv := dp.Intervals[idx]
	return &PlanTarget{
		BattSetpointW: iv.BattSetpointW,
		EVMaxCurrentA: iv.EVMaxCurrentA,
		EVSetpointW:   iv.EVSetpointW,
		EVModelled:    dp.EVModelled,
	}
}

// PlannerCfg holds the operator-tunable parameters for DailyPlanner.
// It is read from hub.json and passed to Engine via Config.
type PlannerCfg struct {
	// EV asset. EVCapacityKwh == 0 (or EVMaxChargeKw == 0) means no chargeable EV
	// was modelled: the plan sets DailyPlan.EVModelled=false and the optimizer
	// hands any live EV session to the reactive charging rule instead of pushing
	// the plan's forced 0 A as a standing suspend (audit A-2/H6). It does NOT
	// leave the EV uncharged.
	EVCapacityKwh  float64 `json:"ev_capacity_kwh"`
	EVMaxChargeKw  float64 `json:"ev_max_charge_kw"`
	EVEfficiency   float64 `json:"ev_efficiency"`   // default 0.95
	EVVoltageV     float64 `json:"ev_voltage_v"`    // default 240
	EVDepartureHH  int     `json:"ev_departure_hh"` // local hour 0–23
	EVDepartureMM  int     `json:"ev_departure_mm"` // local minute 0–59
	EVTargetSocPct float64 `json:"ev_target_soc_pct"`

	// Battery overrides; 0 = derive from live MQTT metrics.
	BattCapacityKwh    float64 `json:"batt_capacity_kwh"`
	BattMaxChargeKw    float64 `json:"batt_max_charge_kw"`
	BattMaxDischargeKw float64 `json:"batt_max_discharge_kw"`
	BattEfficiency     float64 `json:"batt_efficiency"` // default 0.96

	// TerminalReservePct is the end-of-horizon target SOC as a percent of
	// capacity. It lets the battery net-discharge down to a reserve across an
	// expensive evening instead of being pinned at its starting SOC. 0 → 20.
	TerminalReservePct float64 `json:"terminal_reserve_pct"`

	// SolarPeakKw seeds the diurnal solar forecast (clear-sky PV peak, kW) before
	// any live generation is observed — e.g. the first overnight replan. 0 → rely
	// on the running high-water estimate derived from observed generation.
	SolarPeakKw float64 `json:"solar_peak_kw"`

	// LoadAvgKw is the average site load (home, excluding EV) in kW used to
	// SYNTHESIZE a diurnal load-forecast curve (diurnalLoadForecast) when no
	// per-step load profile arrives from an app/cloud intent. 0 (the default)
	// disables synthesis: the planner falls back to the scalar LoadForecastKw
	// (the live inferred load held flat), exactly as before this field existed,
	// so a hub.json without this key plans byte-identically. A positive value
	// gives the DP a realistic evening-peaked residential load to arbitrage
	// against (peak-shaving, EV/solar shifting) even on a solar-masked or
	// baseload-free bench where the instantaneous inferred load reads ~0.
	LoadAvgKw float64 `json:"load_avg_kw"`

	// DP discretisation (0 = default 0.5 kWh).
	SOCStepKwh float64 `json:"soc_step_kwh"`

	// How often the planner re-runs when not triggered by an input change.
	ReplanIntervalS int `json:"replan_interval_s"` // default 900
}

// DailyPlanner runs the dynamic-programming cost optimiser.
// It holds no mutable state; Plan is safe for concurrent use.
type DailyPlanner struct{}

// NewDailyPlanner creates a DailyPlanner.
func NewDailyPlanner() *DailyPlanner { return &DailyPlanner{} }

// Plan runs the full 24-hour DP optimiser and returns the cost-optimal plan.
//
// The algorithm minimises total electricity cost subject to:
//   - Battery SOC bounds and charge/discharge power limits
//   - EV SOC bounds, IEC 61851 current steps, and departure-time target
//   - DER constraints (export/import/generation limits, forced disconnect, fixed dispatch)
//   - Terminal constraint: final battery SOC ≥ TerminalSocKwh (default: initial)
//
// Complexity: O(planSteps × nBattLevels × nEVLevels × nBattPowers × nEVCurrents)
// with O(1) per transition (SOC transitions are precomputed per (level, action)
// pair before the time loop). For a 10 kWh battery + 75 kWh EV at 0.5 kWh step
// this measures ~350 ms on a desktop x86 core (see BenchmarkDailyPlanner_Plan);
// budget a low single-digit number of seconds on the ARM64 SOM.
func (pl *DailyPlanner) Plan(p PlannerParams) *DailyPlan {
	p = p.withDefaults()
	ws := p.WindowStart - (p.WindowStart % planStepSec)
	we := ws + int64(planSteps)*planStepSec

	hasBatt := p.BattCapacityKwh > 0
	hasEV := p.EVCapacityKwh > 0

	// Discretise state spaces.
	battLevels := []float64{0}
	if hasBatt {
		battLevels = discretizeLevels(p.MinBattSocKwh, p.maxBattSoc(), p.SOCStepKwh)
	}
	nBatt := len(battLevels)

	evLevels := []float64{0}
	if hasEV {
		evLevels = discretizeLevels(0, p.EVCapacityKwh, p.SOCStepKwh)
	}
	nEV := len(evLevels)

	// Discretise action spaces.
	battPowers := []float64{0}
	if hasBatt {
		battPowers = discretizePowers(-p.BattMaxChargeKw, p.BattMaxDischargeKw, 0.5)
	}

	evCurrents := []float64{0}
	if hasEV && p.EVMaxChargeKw > 0 {
		v := p.EVVoltageV
		if v == 0 {
			v = 240
		}
		chargeCurrents := discretizeEVCurrents(p.EVMaxChargeKw * 1000 / v)
		if p.EVStorage {
			// D8/WP-14: bidirectional action space — mirror the charge-side
			// IEC 61851 levels negative (discharge), under the SAME charger
			// current rating governing both directions (a symmetric-rating
			// simplification; the J3072/V2G-AC profile carries one active-
			// power setpoint for both directions —
			// docs/standards-buildout/digests/v2g-ac-profile.md). Inert
			// unless EVStorage is true, so the flag-off action space below
			// is exactly chargeCurrents, unchanged.
			evCurrents = mirrorSignedLevels(chargeCurrents)
		} else {
			evCurrents = chargeCurrents
		}
	}

	const inf = math.MaxFloat64 / 2

	// DP tables.
	dp := makeDP2D(nBatt, nEV, inf)
	dpNext := makeDP2D(nBatt, nEV, inf)

	// Seed: place probability mass at the initial state.
	iBatt0 := closestIdx(battLevels, p.InitialBattSocKwh)
	iEV0 := 0
	if hasEV {
		iEV0 = closestIdx(evLevels, p.InitialEVSocKwh)
	}
	dp[iBatt0][iEV0] = 0

	// Backtrack table: back[t][destBatt][destEV] = {srcBatt, srcEV, battCmdIdx, evCmdIdx}.
	type backNode struct {
		src [2]int16
		cmd [2]int16
	}
	back := make([][][]backNode, planSteps)
	for t := range back {
		back[t] = make([][]backNode, nBatt)
		for i := range back[t] {
			back[t][i] = make([]backNode, nEV)
			for j := range back[t][i] {
				back[t][i][j].src = [2]int16{-1, -1}
				back[t][i][j].cmd = [2]int16{0, 0}
			}
		}
	}

	// Compute EV departure step index (−1 = no constraint).
	evDeptStep := -1
	if hasEV && p.EVDepartureUnix > 0 {
		s := int((p.EVDepartureUnix - ws) / planStepSec)
		if s >= 0 && s < planSteps {
			evDeptStep = s
		}
	}
	evTargetIdx := 0
	if hasEV && p.EVTargetSocKwh > 0 {
		evTargetIdx = closestIdx(evLevels, p.EVTargetSocKwh)
	}

	// Fixed-power index cache: for FixedW constraint, only this battPowers index is valid.
	fixedPwrIdx := make([]int, planSteps)
	for t := range fixedPwrIdx {
		fixedPwrIdx[t] = -1
	}
	for t, c := range p.DERConstraints {
		if t >= planSteps {
			break
		}
		if !math.IsNaN(c.FixedW) {
			fixedPwrIdx[t] = closestIdx(battPowers, c.FixedW/1000)
		}
	}

	// ─── Precompute transition tables ─────────────────────────────────────────
	// Destination SOC indices depend only on the (level, action) pair, never on
	// the step, so resolve them once here.  Previously the innermost DP loop ran
	// an O(n) nearest-level scan per transition — ~10^10 operations for a 75 kWh
	// EV at the default step, i.e. seconds to minutes per plan.
	voltV := p.EVVoltageV
	evKwOf := make([]float64, len(evCurrents))
	for ci, evA := range evCurrents {
		evKwOf[ci] = evA * voltV / 1000
	}

	// zeroEVIdx is the index of the "no EV action" (0 A) entry in evCurrents.
	// Always 0 for the charge-only (non-mirrored) action space — discretizeEVCurrents
	// always starts with 0 — so this is a no-op for the flag-off path; the
	// D8/WP-14 bidirectional (mirrored) space puts the negative levels FIRST,
	// so 0 sits at a different index, and the evGone/no-EV gate below must
	// find it explicitly rather than assume index 0.
	zeroEVIdx := 0
	for i, a := range evCurrents {
		if a == 0 {
			zeroEVIdx = i
			break
		}
	}

	// battDest[i][bi] → destination battery level index, or −1 when the SOC
	// transition leaves the allowed band.
	// battEffKw[i][bi] → the effective AC power implied by the *snapped*
	// destination level (+ discharge, − charge). Cost and grid balance use this
	// rather than the commanded power so the DP can never gain energy that the
	// SOC state didn't actually move: a command whose SOC change rounds to zero
	// level has zero effective power, which removes the "phantom free discharge"
	// the DP would otherwise exploit at coarse SOC steps.
	battDest := make([][]int, nBatt)
	battEffKw := make([][]float64, nBatt)
	for i := range battDest {
		battDest[i] = make([]int, len(battPowers))
		battEffKw[i] = make([]float64, len(battPowers))
		for bi, battKw := range battPowers {
			newBSoc := battLevels[i]
			if hasBatt {
				if battKw > 0 {
					newBSoc -= battKw * planStepHours / p.BattEfficiency
				} else if battKw < 0 {
					newBSoc += (-battKw) * planStepHours * p.BattEfficiency
				}
				if newBSoc < p.MinBattSocKwh-1e-6 || newBSoc > p.maxBattSoc()+1e-6 {
					battDest[i][bi] = -1
					continue
				}
			}
			ni := closestIdx(battLevels, newBSoc)
			battDest[i][bi] = ni
			if hasBatt {
				// Invert the SOC↔AC relation on the snapped level delta:
				// discharge AC out = SoC drop × efficiency; charge AC in =
				// SoC rise ÷ efficiency. Clamp to the device power range so a
				// rounding overshoot can't imply a setpoint beyond the rating.
				dSoc := battLevels[ni] - battLevels[i] // + charged, − discharged
				var eff float64
				if dSoc < 0 {
					eff = (-dSoc) * p.BattEfficiency / planStepHours
				} else if dSoc > 0 {
					eff = -(dSoc / p.BattEfficiency) / planStepHours
				}
				battEffKw[i][bi] = math.Max(-p.BattMaxChargeKw, math.Min(p.BattMaxDischargeKw, eff))
			}
		}
	}

	// evDest[j][ci] → destination EV level index, or −1 when the transition
	// overshoots the pack.  ci=0 (no charging) always maps back to j, which
	// also covers the evGone / no-EV cases where only ci=0 is iterated.
	//
	// evKwOf[ci] >= 0 (charging or idle — the ENTIRE flag-off universe,
	// since evCurrents never contains a negative entry unless EVStorage
	// built the bidirectional action space above) uses the exact original
	// formula. evKwOf[ci] < 0 (discharge, D8/WP-14, unreachable when the
	// flag is off) applies the efficiency the OTHER way — more energy
	// leaves the pack than reaches the site — mirroring battDest's
	// charge/discharge asymmetric-efficiency split above.
	evDest := make([][]int, nEV)
	for j := range evDest {
		evDest[j] = make([]int, len(evCurrents))
		for ci := range evCurrents {
			var newESoc float64
			if evKwOf[ci] >= 0 {
				newESoc = evLevels[j] + evKwOf[ci]*planStepHours*p.EVEfficiency
			} else {
				newESoc = evLevels[j] + evKwOf[ci]*planStepHours/p.EVEfficiency
			}
			if newESoc < -1e-6 || newESoc > p.EVCapacityKwh+1e-6 {
				evDest[j][ci] = -1
				continue
			}
			newESoc = math.Max(0, math.Min(p.EVCapacityKwh, newESoc))
			evDest[j][ci] = closestIdx(evLevels, newESoc)
		}
	}

	// ─── Forward DP ───────────────────────────────────────────────────────────
	for t := 0; t < planSteps; t++ {
		clearDP2D(dpNext, inf)

		c := planStepConstraint(p, t)
		solarKw := planStepSolar(p, t)
		loadKw := planStepLoad(p, t)
		stepT := ws + int64(t)*planStepSec
		impPrice := planStepImportAllIn(p, t, stepT)
		expPrice := planStepExportPrice(p, t, stepT)
		evGone := evDeptStep >= 0 && t > evDeptStep
		fixedBI := fixedPwrIdx[t]

		for i := 0; i < nBatt; i++ {
			for j := 0; j < nEV; j++ {
				cost0 := dp[i][j]
				if cost0 >= inf {
					continue
				}
				// Enforce EV departure constraint: at the departure step, only
				// states where EV SOC ≥ target are valid starting points.
				if evDeptStep >= 0 && t == evDeptStep && hasEV && j < evTargetIdx {
					continue
				}

				for bi, battKw := range battPowers {
					if c.Disconnect && battKw != 0 {
						continue
					}
					if fixedBI >= 0 && bi != fixedBI {
						continue
					}
					// Battery SOC transition (precomputed); −1 = out of band.
					ni := battDest[i][bi]
					if ni < 0 {
						continue
					}
					// Effective battery AC power for this transition (tracks the
					// snapped SOC change, not the raw command) drives the grid
					// balance, constraints, and cost.
					battKwEff := battEffKw[i][bi]

					for ci := range evCurrents {
						if (evGone || !hasEV) && ci != zeroEVIdx {
							continue
						}
						// EV SOC transition (precomputed); −1 = overshoots pack.
						nj := evDest[j][ci]
						if nj < 0 {
							continue
						}
						evKw := evKwOf[ci]

						// Grid balance: + = import, − = export.
						// grid = load + ev_draw − solar − batt_net
						gridKw := loadKw + evKw - solarKw - battKwEff

						// Apply DER constraints.
						if !math.IsNaN(c.ExpLimW) && -gridKw > c.ExpLimW/1000+1e-6 {
							continue
						}
						if !math.IsNaN(c.ImpLimW) && gridKw > c.ImpLimW/1000+1e-6 {
							continue
						}
						if !math.IsNaN(c.MaxLimW) {
							gen := solarKw
							if battKwEff > 0 {
								gen += battKwEff
							}
							if gen > c.MaxLimW/1000+1e-6 {
								continue
							}
						}
						// CSIP-AUS gross-generation cap (WP-11): solar plus
						// battery discharge — here the "gen includes battery
						// discharge" formula is the cap's actual definition
						// (for MaxLimW above it is a conservative planning
						// approximation of an inverter-output cap).
						if !math.IsNaN(c.GenLimW) {
							gen := solarKw
							if battKwEff > 0 {
								gen += battKwEff
							}
							// D8/WP-14: a discharging EV is DER generation too.
							// evKw < 0 is unreachable unless EVStorage built the
							// bidirectional action space above, so this is a
							// no-op for the flag-off path.
							if evKw < 0 {
								gen += -evKw
							}
							if gen > c.GenLimW/1000+1e-6 {
								continue
							}
						}
						// CSIP-AUS gross-load cap (WP-11): home load plus EV
						// draw plus battery charge.
						if !math.IsNaN(c.LoadLimW) {
							load := loadKw + evKw
							// D8/WP-14: "gross" must never NET against a
							// discharging asset — cancel the += evKw above for
							// a discharge (evKw < 0, unreachable when
							// EVStorage is off) the same way battKwEff's
							// charge-only treatment below never lets discharge
							// reduce load.
							if evKw < 0 {
								load -= evKw
							}
							if battKwEff < 0 {
								load += -battKwEff
							}
							if load > c.LoadLimW/1000+1e-6 {
								continue
							}
						}

						// Step cost.
						var stepCost float64
						if gridKw > 0 {
							stepCost = gridKw * planStepHours * impPrice
						} else {
							stepCost = gridKw * planStepHours * expPrice
						}

						// Update next DP table.
						if nc := cost0 + stepCost; nc < dpNext[ni][nj] {
							dpNext[ni][nj] = nc
							back[t][ni][nj] = backNode{
								src: [2]int16{int16(i), int16(j)},
								cmd: [2]int16{int16(bi), int16(ci)},
							}
						}
					}
				}
			}
		}

		dp, dpNext = dpNext, dp
	}

	// ─── Find best terminal state ─────────────────────────────────────────────
	// Terminal constraint: battery should not end below a target SOC.
	// The target defaults to the initial SOC (no net daily discharge), but a
	// caller can lower it (TerminalSocKwh) to a reserve floor so the battery may
	// run down across an expensive evening. Soft penalty rather than a hard
	// filter so the solver always has a feasible answer.
	terminalTarget := p.TerminalSocKwh
	if terminalTarget <= 0 {
		terminalTarget = p.InitialBattSocKwh
	}
	bestCost := inf
	bestBatt, bestEV := iBatt0, iEV0
	for i := 0; i < nBatt; i++ {
		for j := 0; j < nEV; j++ {
			c := dp[i][j]
			if c >= inf {
				continue
			}
			// Add a penalty proportional to the SOC shortfall (100 $/kWh is
			// much larger than any realistic energy price, so the solver will
			// avoid ending below the target SOC unless it has no choice).
			if hasBatt && battLevels[i] < terminalTarget-1e-6 {
				c += 100.0 * (terminalTarget - battLevels[i])
			}
			if c < bestCost {
				bestCost = c
				bestBatt, bestEV = i, j
			}
		}
	}
	totalCost := dp[bestBatt][bestEV]
	if totalCost >= inf {
		totalCost = 0 // no feasible path found
	}

	// ─── Backtrack to extract per-step plan ───────────────────────────────────
	buildTime := p.Now
	if buildTime.IsZero() {
		buildTime = time.Now()
	}
	out := &DailyPlan{
		BuildTime:   buildTime,
		WindowStart: ws,
		WindowEnd:   we,
		TotalCost:   totalCost,
		EVModelled:  hasEV && p.EVMaxChargeKw > 0,
	}

	bi, ej := bestBatt, bestEV
	for t := planSteps - 1; t >= 0; t-- {
		node := back[t][bi][ej]

		// battW stays NaN when there is no battery asset. Use the effective power
		// implied by the snapped SOC change (consistent with the DP cost), not
		// the raw command, so the setpoint moves only the energy the plan booked.
		battW := math.NaN()
		// socKwh is the DP's tracked SOC at the START of this slot: the source
		// battery level of the chosen transition (battLevels[node.src[0]]),
		// snapped to the SOC grid. NaN when there is no battery or the slot has
		// no recorded predecessor (GAP-7 — captured for DailyPlanSnapshot).
		socKwh := math.NaN()
		var evA float64
		if hasBatt && node.src[0] >= 0 {
			battW = battEffKw[node.src[0]][node.cmd[0]] * 1000
			socKwh = battLevels[node.src[0]]
		}
		if node.src[0] >= 0 {
			evA = evCurrents[node.cmd[1]]
		}

		solarKw := planStepSolar(p, t)
		vv := p.EVVoltageV
		evKw := evA * vv / 1000
		bKw := battW / 1000
		if math.IsNaN(bKw) {
			bKw = 0
		}
		gridW := (planStepLoad(p, t) + evKw - solarKw - bKw) * 1000
		stepT := ws + int64(t)*planStepSec
		impP := planStepImportAllIn(p, t, stepT)
		expP := planStepExportPrice(p, t, stepT)
		gKw := gridW / 1000
		var margCost float64
		if gKw > 0 {
			margCost = gKw * planStepHours * impP
		} else {
			margCost = gKw * planStepHours * expP
		}

		// EVMaxCurrentA keeps its pre-D8 never-negative ceiling-mode contract
		// (0 = suspend, >0 = charge ceiling) even when EVStorage's
		// bidirectional action space chose a discharge (negative) current
		// for this step — that intent is carried on EVSetpointW instead
		// (battery sign convention), never by letting this field go
		// negative. A no-op when evA is already >= 0 (the entire flag-off
		// universe).
		evMaxA := math.Max(0, evA)

		// EVSetpointW: NaN when no EV asset was modelled (mirrors
		// BattSetpointW's NaN-no-battery sentinel), else always populated —
		// including in the charge-only universe, where it is guaranteed
		// <= 0 (never a discharge value; see the field's doc).
		evSetpointW := math.NaN()
		if hasEV {
			evSetpointW = -evKw * 1000
		}

		out.Intervals[t] = PlanInterval{
			Start:         stepT,
			BattSetpointW: battW,
			EVMaxCurrentA: evMaxA,
			EVSetpointW:   evSetpointW,
			ExpectedGridW: gridW,
			MarginalCost:  margCost,
			SocKwh:        socKwh,
		}

		if node.src[0] >= 0 {
			bi = int(node.src[0])
			ej = int(node.src[1])
		}
	}

	// TotalCost of the RETURNED plan: the sum of its per-slot marginal costs (the
	// true import/export $ the backtracked dispatch incurs) plus the flat daily
	// charge. For a cleanly feasible plan this equals the forward-pass optimum
	// dp[best] the plan was seeded with above; but when the terminal SOC/EV-target
	// penalty forces a best-effort path, dp[best] is pinned to 0 by the
	// "no feasible path" guard while the plan still imports to serve load — so
	// report the marginals actually planned. This never reads misleadingly low on
	// a plan that costs money, and keeps TotalCost consistent with the cost_plan
	// series on the wire (which is exactly these marginals).
	//
	// Fixed daily charge: a flat, dispatch-independent cost added ONCE (the
	// horizon is exactly 24 h = 1 day, planStepHours*planSteps == 24). It stays
	// out of every per-slot MarginalCost — a constant cannot change marginal
	// dispatch — so it never moves a setpoint, only the reported daily total.
	planCost := 0.0
	for i := range out.Intervals {
		planCost += out.Intervals[i].MarginalCost
	}
	out.TotalCost = planCost + p.FixedDailyCharge

	return out
}

// ── PlannerParams helpers ─────────────────────────────────────────────────────

func (p PlannerParams) withDefaults() PlannerParams {
	if p.BattEfficiency == 0 {
		p.BattEfficiency = 0.96
	}
	if p.EVEfficiency == 0 {
		p.EVEfficiency = 0.95
	}
	if p.EVVoltageV == 0 {
		p.EVVoltageV = 240
	}
	if p.SOCStepKwh == 0 {
		p.SOCStepKwh = 0.5
	}
	if p.MinBattSocKwh == 0 && p.BattCapacityKwh > 0 {
		p.MinBattSocKwh = 0.10 * p.BattCapacityKwh
	}
	return p
}

func (p *PlannerParams) maxBattSoc() float64 {
	if p.MaxBattSocKwh > 0 {
		return p.MaxBattSocKwh
	}
	return p.BattCapacityKwh
}

func planStepConstraint(p PlannerParams, t int) StepConstraint {
	if t < len(p.DERConstraints) {
		c := p.DERConstraints[t]
		return c
	}
	return StepConstraint{ExpLimW: math.NaN(), ImpLimW: math.NaN(), MaxLimW: math.NaN(), FixedW: math.NaN(), GenLimW: math.NaN(), LoadLimW: math.NaN()}
}

func planStepSolar(p PlannerParams, t int) float64 {
	if t < len(p.SolarForecastKw) {
		return p.SolarForecastKw[t]
	}
	return 0
}

// planStepLoad returns the site load (kW) for step t: the per-step profile value
// when a LoadProfileKw is set and covers t, otherwise the scalar LoadForecastKw.
// Mirrors planStepSolar, but the empty/out-of-range fallback is the scalar
// LoadForecastKw (not zero) — an unspecified step is "typical load", never "no
// load". Behaviour is identical to reading p.LoadForecastKw directly whenever
// LoadProfileKw is empty.
func planStepLoad(p PlannerParams, t int) float64 {
	if t >= 0 && t < len(p.LoadProfileKw) {
		return p.LoadProfileKw[t]
	}
	return p.LoadForecastKw
}

// Daylight window for the clear-sky forecast shape (local hours).
const (
	solarSunriseHr = 6.0
	solarSunsetHr  = 20.0
)

// clearSkyShape returns the normalised clear-sky PV factor [0,1] at a local
// hour-of-day: a half-sine that is zero outside [sunrise,sunset] and peaks at
// solar noon. Multiply by an estimated peak kW to get a generation forecast.
func clearSkyShape(hour float64) float64 {
	if hour <= solarSunriseHr || hour >= solarSunsetHr {
		return 0
	}
	return math.Sin(math.Pi * (hour - solarSunriseHr) / (solarSunsetHr - solarSunriseHr))
}

// solarEstMinShape is the minimum clear-sky shape at which the peak back-calc
// curKw/shape is trusted. Near sunrise/sunset the shape is tiny, so a modest
// reading divided by it extrapolates to a wildly inflated "peak" (a 4 kW dusk
// reading / 0.15 ≈ 27 kW). Only sample when the sun is high (~10:00–14:00 for a
// 6–20h day), where the ratio is a reliable estimate of the clear-sky peak.
const solarEstMinShape = 0.6

// estimateSolarPeakKw updates the clear-sky solar-peak high-water mark that
// seeds the diurnal forecast. It back-calculates the peak from a live reading
// (curKw / shape) ONLY when the sun is high enough to trust the ratio
// (shape ≥ solarEstMinShape) and clamps the result to the inverter nameplate
// (nameplateKw, a physical ceiling) — an inverter cannot produce more than its
// rating, and an unclamped low-sun estimate that overshoots the export cap for
// hours is what drives the daily planner infeasible (it has no PV-curtailment
// lever). The mark only ratchets UP (never below prev) so a replan after dark
// still forecasts tomorrow; the nameplate clamp bounds that ratchet. A
// nameplateKw ≤ 0 (unknown rating) leaves the high-sun gate as the only guard.
func estimateSolarPeakKw(curKw, shape, nameplateKw, prev float64) float64 {
	if curKw <= 0 || shape < solarEstMinShape {
		return prev
	}
	est := curKw / shape
	if nameplateKw > 0 && est > nameplateKw {
		est = nameplateKw
	}
	if est > prev {
		return est
	}
	return prev
}

// localHourOf returns the local hour-of-day (with fractional minutes) of a Unix
// time. The forecast must use the same clock the optimizer evaluates TOU on, so
// callers pass server (offset-adjusted) Unix seconds.
func localHourOf(unix int64) float64 {
	lt := time.Unix(unix, 0).Local()
	return float64(lt.Hour()) + float64(lt.Minute())/60
}

// diurnalSolarForecast builds a per-step (5-min) solar generation forecast (kW)
// over the 24 h horizon starting at baseUnix, shaping a clear-sky bell curve to
// the given peak. Returns nil when peakKw <= 0 (no information — same as the old
// zero forecast). This replaces a flat-from-current forecast that assumed
// constant sun all day and, worse, zero sun for the whole horizon on any replan
// after sunset.
func diurnalSolarForecast(baseUnix int64, peakKw float64) []float64 {
	if peakKw <= 0 {
		return nil
	}
	out := make([]float64, planSteps)
	for t := range out {
		out[t] = peakKw * clearSkyShape(localHourOf(baseUnix+int64(t)*planStepSec))
	}
	return out
}

// residentialLoadShape returns an UNNORMALISED residential load factor at a
// local hour-of-day: a low overnight base, a modest morning bump, and a
// dominant early-evening peak — the demand side of the classic "duck curve".
// Only the SHAPE matters; diurnalLoadForecast scales it to a configured average,
// so the absolute magnitudes of the constants here are arbitrary.
func residentialLoadShape(hour float64) float64 {
	base := 0.5
	morning := 0.7 * gaussianBump(hour, 7.5, 1.5)
	midday := 0.25 * gaussianBump(hour, 13.0, 2.5)
	evening := 1.8 * gaussianBump(hour, 19.5, 2.5)
	return base + morning + midday + evening
}

// gaussianBump is an un-normalised Gaussian (peak 1.0 at mu) used to shape the
// residential load curve's morning/evening humps.
func gaussianBump(x, mu, sigma float64) float64 {
	d := (x - mu) / sigma
	return math.Exp(-0.5 * d * d)
}

// diurnalLoadForecast builds a per-step (5-min) site-load forecast (kW) over the
// 24 h horizon starting at baseUnix, shaping a residential day
// (residentialLoadShape) and scaling it so the MEAN over the window equals
// avgKw. Returns nil when avgKw <= 0 (no synthesis — the caller keeps the scalar
// LoadForecastKw), mirroring diurnalSolarForecast's nil-on-no-information
// contract. The window is exactly 24 h, so it covers every hour once regardless
// of start: the scaled mean equals avgKw no matter when the day begins.
func diurnalLoadForecast(baseUnix int64, avgKw float64) []float64 {
	if avgKw <= 0 {
		return nil
	}
	raw := make([]float64, planSteps)
	var sum float64
	for t := range raw {
		raw[t] = residentialLoadShape(localHourOf(baseUnix + int64(t)*planStepSec))
		sum += raw[t]
	}
	mean := sum / float64(planSteps)
	if mean <= 0 {
		return nil
	}
	out := make([]float64, planSteps)
	for t := range raw {
		out[t] = avgKw * raw[t] / mean
	}
	return out
}

func planStepImportPrice(p PlannerParams, t int, unixT int64) float64 {
	if t < len(p.ImportPricePerKwh) {
		return p.ImportPricePerKwh[t]
	}
	if p.FallbackTOU != nil {
		return p.FallbackTOU.CurrentRate(time.Unix(unixT, 0).Local())
	}
	return 0.20
}

// planStepDeliveryPrice returns the volumetric delivery/distribution charge
// ($/kWh) for step t: p.DeliveryTOU.CurrentRate at the slot's local time when a
// DeliveryTOU is set, else 0 (no delivery charge). It renders the local time
// exactly as planStepImportPrice does for its FallbackTOU lookup
// (time.Unix(unixT, 0).Local()), so supply and delivery are always evaluated at
// the same wall-clock hour. Delivery has no per-slot override array — it is a
// pure TOU adder — so t is accepted only to mirror planStepImportPrice's
// signature and keep the call sites uniform.
func planStepDeliveryPrice(p PlannerParams, t int, unixT int64) float64 {
	if p.DeliveryTOU != nil {
		return p.DeliveryTOU.CurrentRate(time.Unix(unixT, 0).Local())
	}
	return 0
}

// planStepImportAllIn is the all-in import price ($/kWh) for step t: the supply
// import price plus the orthogonal delivery adder. This is what the DP costs an
// imported kWh at — used by BOTH the forward objective and the backtrack
// MarginalCost mirror so the two stay consistent. The export price has no such
// adder (you do not pay delivery on export), so the export branch keeps using
// planStepExportPrice directly.
func planStepImportAllIn(p PlannerParams, t int, unixT int64) float64 {
	return planStepImportPrice(p, t, unixT) + planStepDeliveryPrice(p, t, unixT)
}

func planStepExportPrice(p PlannerParams, t int, unixT int64) float64 {
	if t < len(p.ExportPricePerKwh) {
		return p.ExportPricePerKwh[t]
	}
	return 0
}

// ── Discretisation helpers ────────────────────────────────────────────────────

// discretizeLevels returns evenly-spaced values from lo to hi (inclusive)
// at the given step, with exact endpoints.
func discretizeLevels(lo, hi, step float64) []float64 {
	if step <= 0 || hi <= lo {
		return []float64{lo}
	}
	n := int(math.Round((hi-lo)/step)) + 1
	if n < 2 {
		n = 2
	}
	out := make([]float64, n)
	for i := range out {
		out[i] = math.Round((lo+float64(i)*step)/step) * step
	}
	out[0] = lo
	out[n-1] = hi
	return out
}

// discretizePowers returns battery power levels from lo to hi at step, always
// including 0.
func discretizePowers(lo, hi, step float64) []float64 {
	if step <= 0 {
		step = 0.5
	}
	n := int(math.Round((hi-lo)/step)) + 1
	if n < 1 {
		return []float64{0}
	}
	out := make([]float64, n)
	for i := range out {
		out[i] = math.Round((lo+float64(i)*step)/step) * step
	}
	out[0] = lo
	out[n-1] = hi
	return out
}

// discretizeEVCurrents returns [0, 6, 8, 10, …, maxA] following IEC 61851
// standard current levels. The minimum charging current is 6 A.
func discretizeEVCurrents(maxA float64) []float64 {
	standards := []float64{6, 8, 10, 12, 16, 20, 24, 32, 40, 48, 63, 80}
	out := []float64{0}
	for _, a := range standards {
		if a > maxA+0.5 {
			break
		}
		out = append(out, a)
	}
	if len(out) == 1 && maxA >= 1 {
		out = append(out, maxA) // at least one non-zero level
	}
	return out
}

// mirrorSignedLevels returns levels (assumed sorted ascending and starting at
// 0 — discretizeEVCurrents' own contract) mirrored negative-then-positive,
// e.g. [0, 6, 8] → [-8, -6, 0, 6, 8]. This is the ev_storage (D8/WP-14)
// bidirectional EV action space: the SAME charger current rating governs
// discharge as charge (a symmetric-rating simplification — the J3072/V2G-AC
// profile carries one active-power setpoint for both directions;
// docs/standards-buildout/digests/v2g-ac-profile.md).
func mirrorSignedLevels(levels []float64) []float64 {
	out := make([]float64, 0, 2*len(levels)-1)
	for i := len(levels) - 1; i > 0; i-- {
		out = append(out, -levels[i])
	}
	out = append(out, levels...)
	return out
}

// closestIdx returns the index of the level closest to v.
func closestIdx(levels []float64, v float64) int {
	if len(levels) == 0 {
		return 0
	}
	best, bestDist := 0, math.Abs(levels[0]-v)
	for i := 1; i < len(levels); i++ {
		if d := math.Abs(levels[i] - v); d < bestDist {
			bestDist = d
			best = i
		}
	}
	return best
}

// makeDP2D allocates a 2-D float64 slice filled with fill.
func makeDP2D(nR, nC int, fill float64) [][]float64 {
	dp := make([][]float64, nR)
	for i := range dp {
		dp[i] = make([]float64, nC)
		for j := range dp[i] {
			dp[i][j] = fill
		}
	}
	return dp
}

// clearDP2D resets all entries to fill.
func clearDP2D(dp [][]float64, fill float64) {
	for i := range dp {
		for j := range dp[i] {
			dp[i][j] = fill
		}
	}
}
