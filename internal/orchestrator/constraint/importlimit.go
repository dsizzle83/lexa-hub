package constraint

import (
	"fmt"
	"math"

	"lexa-hub/internal/orchestrator"
)

// ImportLimitConstraint is the TierCompliance migration of the legacy import-limit
// path (TASK-061). It ports applyImportLimitRule (optimizer.go:1929-2139) — the
// sticky battery-discharge controller with relax-cycle ramp-down and the
// battery-headroom breach — and checkImportConvergence (optimizer.go:1401-1440) —
// the measured-effect backstop with NaN-HOLD semantics — into a pure Evaluate over
// a typed ImportSession.
//
// Emitted demands (TierCompliance):
//   - AxisBatterySetpointW point (= +discharge) per battery it discharges to hold
//     grid import ≤ the CSIP limit. Sticky: it ratchets up immediately when import
//     exceeds the hard limit and ramps down only after safeCount consecutive
//     under-limit ticks (anti-oscillation, optimizer.go:1922-1928).
//   - a ComplianceBreach{LimitType:"import"} when the site is over the hard cap and
//     the battery has no discharge headroom left (all packs at/below the SOC
//     reserve, or maxed) — a physically unavoidable miss (optimizer.go:2123-2137) —
//     OR when measured import stays over the cap past the detection window with no
//     lever converging (checkImportConvergence, optimizer.go:1427-1439).
//
// Two legacy behaviours are DEFERRED, exactly as ExportConstraint defers its EV
// emission:
//   - EV-suspend emission. The legacy import path suspends EV charging via a
//     SEPARATE rule (applyEVChargingRule gated by impGuard.evSafeCount,
//     optimizer.go:356). That rule is TierEconomics and is not migrated yet, so the
//     import constraint TRACKS evSafeCount (so the flip inherits correct cooldown
//     state) but emits no EVSE demand.
//   - Charge-neutralisation. Legacy walks the shared plan and zeroes any commanded
//     battery CHARGE while defending the cap (optimizer.go:2045-2059). In the
//     constraint model there is no shared plan: each constraint emits its own
//     demand and the arbiter resolves the battery axis most-restrictively. When an
//     export cap (charge) and an import cap (discharge) bind the SAME battery
//     simultaneously — a contradictory pair not exercised by the scenario families —
//     the arbiter collapses to the tightest bound rather than legacy's
//     import-wins-and-neutralises; see TestImportGen_SimultaneousCapArbitration and
//     the shadow report. Faithful shared-axis authorship lands with economics
//     migration.
type ImportLimitConstraint struct {
	sess ImportSession
}

// compile-time proof ImportLimitConstraint satisfies the Constraint interface.
var _ Constraint = (*ImportLimitConstraint)(nil)

// NewImportLimitConstraint builds the import constraint in the no-active-limit state.
func NewImportLimitConstraint() *ImportLimitConstraint {
	return &ImportLimitConstraint{sess: newImportSession()}
}

// Name is the stable identity; it keys the Session and appears as Demand.Source.
func (c *ImportLimitConstraint) Name() string { return "import" }

// Tier places the import cap in the CSIP compliance band.
func (c *ImportLimitConstraint) Tier() Tier { return TierCompliance }

// Ported constants. Kept identical to the DefaultOptimizer defaults, which the
// bench does not override (grep: SOCReserve/ImportMarginFrac are never assigned in
// cmd/hub). Parameterisation is TASK-064.
const (
	importMarginFrac = 0.20  // ImportMarginFrac default (optimizer.go:228)
	importMaxRelaxW  = 250.0 // ramp-down step (optimizer.go:2023)
	importSOCReserve = 20.0  // SOCReserve default (optimizer.go:223)
	importRelaxCycle = 5     // ExportRelaxCycles default, shared by the import ramp (optimizer.go:227,1986)
)

// effectiveImportLimitW reproduces deriveGridConstraints' import leg
// (optimizer.go:460-462): the grid-reported import limit intersected with the
// active CSIP OpModImpLimW override (most-restrictive). NaN = no import limit.
func effectiveImportLimitW(st orchestrator.SystemState) float64 {
	lim := st.Grid.ImportLimitW
	if st.CSIPControl != nil {
		if ap := st.CSIPControl.Base.OpModImpLimW; ap != nil {
			lim = nanMin(lim, apW(ap))
		}
	}
	return lim
}

// Evaluate ports applyImportLimitRule + checkImportConvergence: manage the single
// reset domain, run the sticky discharge controller, then the convergence backstop.
func (c *ImportLimitConstraint) Evaluate(in Input, s *Session) ([]Demand, *orchestrator.ComplianceBreach) {
	st := in.State
	sess := &c.sess

	importLimitW := effectiveImportLimitW(st)

	// Cap cleared → whole session clears, no demands (optimizer.go:1930-1932 +
	// checkImportConvergence :1402-1404).
	if math.IsNaN(importLimitW) {
		sess.clearForNoLimit()
		return nil, nil
	}

	c.manageSession(s, importLimitW, st.Grid.NetW)
	demands, headroomBreach := c.applyImportControl(st, importLimitW)
	convBreach := c.checkConvergence(in, importLimitW)

	// The headroom breach (battery cannot discharge further) takes precedence; the
	// convergence backstop only fires when no lever-exhaustion breach did this tick
	// (mirrors optimizer.go:1427 "plan.Breach == nil").
	breach := headroomBreach
	if breach == nil {
		breach = convBreach
	}
	if breach != nil && st.CSIPControl != nil {
		breach.MRID = st.CSIPControl.MRID // optimizer.go:374-375
	}
	return demands, breach
}

// manageSession ports the guard-session logic at the top of applyImportLimitRule
// (optimizer.go:1935-1957): a fresh guard on a MEANINGFUL cap change (tolerance
// band > complianceBreachW), tracking sub-threshold decode drift otherwise, and
// seeding the EV resume gate as satisfied when the cap ARRIVES while already
// compliant (the cooldown exists for post-violation recovery, not limit arrival).
func (c *ImportLimitConstraint) manageSession(s *Session, importLimitW, netW float64) {
	sess := &c.sess
	if math.IsNaN(sess.activeLimitW) || math.Abs(importLimitW-sess.activeLimitW) > exportComplianceBreachW {
		sess.resetForNewLimit(importLimitW)
		if !math.IsNaN(netW) && netW >= 0 && netW <= importLimitW {
			sess.evSafeCount = c.evCooldown(s)
		}
	} else {
		sess.activeLimitW = importLimitW // same session; track sub-threshold drift
	}
}

// evCooldown mirrors cmd/hub's tick-denominated EVImportCooldownCycles derivation
// (main.go:180-184: ~1 min of wall clock, floored at 4). Only seeds/gates the
// deferred EV resume, so it never changes emitted demands — computed for state
// fidelity so the active flip inherits the correct cooldown.
func (c *ImportLimitConstraint) evCooldown(s *Session) int {
	cycles := int(60.0 / s.TickSeconds())
	if cycles < 4 {
		cycles = 4
	}
	return cycles
}

// applyImportControl ports applyImportLimitRule's controller body
// (optimizer.go:1959-2138 minus session management and the shared-plan
// charge-neutralisation — see the type doc): conservation identity → hysteresis
// counters → sticky slew-limited target discharge → per-battery distribution
// respecting the SOC reserve → battery-headroom breach.
func (c *ImportLimitConstraint) applyImportControl(st orchestrator.SystemState, importLimitW float64) ([]Demand, *orchestrator.ComplianceBreach) {
	sess := &c.sess
	netW := st.Grid.NetW

	importW := 0.0
	if !math.IsNaN(netW) {
		importW = math.Max(0, netW) // positive netW = importing from grid
	}

	// Measured battery discharge before this tick's commands (optimizer.go:1966-1971).
	measuredDischargeW := 0.0
	for _, b := range st.Batteries {
		if b.Connected && b.PowerW > 0 {
			measuredDischargeW += b.PowerW
		}
	}

	// Conservation identity: the meter import already reflects whatever the battery
	// is currently discharging, so the unconstrained import (meter with battery idle)
	// is importW + measured discharge. Measured (not commanded) is used deliberately —
	// substituting a stale commanded value compounds each tick (optimizer.go:1973-1980).
	unconstrainedImportW := importW + measuredDischargeW

	conservativeLimitW := importLimitW * (1.0 - importMarginFrac)

	// Hysteresis: safeCount against the HARD limit, not the conservative one, so a
	// steady operating point at the conservative target still counts as safe
	// (optimizer.go:1992-1999).
	if importW <= importLimitW {
		sess.safeCount++
	} else {
		sess.safeCount = 0
	}

	// evSafeCount gates EV resumption: only when actually importing (positive netW)
	// and under the cap; negative netW (export from over-discharge) resets it
	// (optimizer.go:2001-2009).
	if !math.IsNaN(netW) && netW >= 0 && netW <= importLimitW {
		sess.evSafeCount++
	} else {
		sess.evSafeCount = 0
	}

	// Target discharge brings unconstrained import down to the conservative limit
	// (optimizer.go:2012).
	targetDischargeW := math.Max(0, unconstrainedImportW-conservativeLimitW)

	// Slew: ratchet up immediately (defend the limit fast), ramp down only after
	// safeCount accumulates so we do not chatter across the boundary
	// (optimizer.go:2014-2028).
	commandedDischargeW := targetDischargeW
	if !math.IsNaN(sess.dischargeW) {
		prior := sess.dischargeW
		if targetDischargeW < prior {
			if sess.safeCount < importRelaxCycle {
				commandedDischargeW = prior // hold
			} else {
				commandedDischargeW = math.Max(targetDischargeW, prior-importMaxRelaxW)
				sess.safeCount = 0 // restart hold window after each ramp-down step
			}
		}
	}

	if commandedDischargeW < 50 {
		// Nothing to defend; clear the guard so a fresh episode starts cleanly on the
		// next over-limit event (optimizer.go:2030-2035). No demand emitted — the
		// candidate expresses no opinion on the battery axis this tick.
		sess.dischargeW = math.NaN()
		return nil, nil
	}

	// No shared plan in the constraint model: committed discharge is 0 and there is
	// no commanded charge to neutralise (see type doc). Distribute the full commanded
	// discharge across batteries with headroom, respecting the SOC reserve
	// (optimizer.go:2074-2113).
	remaining := commandedDischargeW
	totalCommanded := 0.0
	var demands []Demand
	for _, b := range st.Batteries {
		if remaining < 1 {
			break
		}
		if !b.Connected {
			continue
		}
		if !math.IsNaN(b.SOC) && b.SOC <= importSOCReserve {
			continue
		}
		add := math.Min(b.AvailableDischargeW(), remaining)
		if add <= 0 {
			continue
		}
		demands = append(demands, PointDemand(b.Name, AxisBatterySetpointW, add, TierCompliance, c.Name()))
		remaining -= add
		totalCommanded += add
	}

	if totalCommanded > 0 {
		sess.dischargeW = totalCommanded
	} else {
		// No battery could actually discharge (all at reserve, etc.). Clear the guard
		// so we do not carry a phantom setpoint (optimizer.go:2117-2121).
		sess.dischargeW = math.NaN()
	}

	// Compliance breach: over the hard import limit with no discharge headroom left
	// to close the gap — a physically unavoidable miss the grid server must be told
	// about. `remaining` is the discharge still needed but uncommandable
	// (optimizer.go:2123-2137).
	var breach *orchestrator.ComplianceBreach
	if importW > importLimitW && remaining > exportComplianceBreachW {
		breach = &orchestrator.ComplianceBreach{
			LimitType:  "import",
			LimitW:     importLimitW,
			MeasuredW:  importW,
			ShortfallW: importW - importLimitW,
			Reason:     importBatteryHeadroomReason(st.Batteries, importSOCReserve),
		}
	}
	return demands, breach
}

// checkConvergence ports checkImportConvergence (optimizer.go:1401-1440): the
// measured-effect backstop keyed to breachTicks with the NaN-HOLD semantics.
func (c *ImportLimitConstraint) checkConvergence(in Input, importLimitW float64) *orchestrator.ComplianceBreach {
	sess := &c.sess
	netW := in.State.Grid.NetW

	// A meter-blind tick (netW NaN) is evidence of nothing: neither breach nor
	// compliance. HOLD the counter rather than resetting it, or a single blind tick
	// inside a sustained breach silently restarts the escalation and the CannotComply
	// loses the race against the constraint window (V3 Issue-3; QA 2026-07-01:
	// battery-soc-refuse). Ported verbatim from optimizer.go:1406-1413 — HARD
	// preserve, TASK-061.
	if math.IsNaN(netW) {
		return nil
	}
	importW := math.Max(0, netW)

	// Leaky counter (optimizer.go:1415-1426): a single under-cap sample decrements
	// instead of zeroing, so a sustained breach with occasional noise still escalates
	// while genuine convergence drains within a few ticks. Adaptive detection window
	// (AD-007) in place of the fixed scaleTicks(importBreachTicks); bench defaults
	// yield 3 — bit-identical.
	threshold := c.detectionWindowTicks(in)
	if importW > importLimitW+exportComplianceBreachW {
		if sess.breachTicks < threshold {
			sess.breachTicks++
		}
	} else if sess.breachTicks > 0 {
		sess.breachTicks--
	}
	if sess.breachTicks >= threshold {
		return &orchestrator.ComplianceBreach{
			LimitType:  "import",
			LimitW:     importLimitW,
			MeasuredW:  importW,
			ShortfallW: importW - importLimitW,
			Reason:     "import remains over the cap after all levers — a load is not honouring the hub's curtailment/suspend command",
		}
	}
	return nil
}

// detectionWindowTicks derives the adaptive import-breach window from plant
// physics. The import lever is battery discharge, so the window is sized from the
// slowest connected battery's control latency plus the meter lag (the analogue of
// the export ceiling's per-inverter window). With no connected battery it falls
// back to the meter lag alone; the floor of 2 in DetectionWindowTicks keeps it sane.
func (c *ImportLimitConstraint) detectionWindowTicks(in Input) int {
	tick := in.TickSeconds
	window := 0
	for _, b := range in.State.Batteries {
		if !b.Connected {
			continue
		}
		if n := in.Plant.ImportDetectionWindowTicks(b.Name, tick); n > window {
			window = n
		}
	}
	if window == 0 {
		window = DetectionWindowTicks(0, in.Plant.Meter.MeterLagS, tick)
	}
	return window
}

// importBatteryHeadroomReason ports batteryHeadroomReason (optimizer.go:2154-2168):
// why the battery could not discharge further.
func importBatteryHeadroomReason(batteries []orchestrator.BatteryState, socReserve float64) string {
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
