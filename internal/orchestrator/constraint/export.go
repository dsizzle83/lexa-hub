package constraint

import (
	"math"

	"lexa-hub/internal/orchestrator"
	model "lexa-proto/csipmodel"
)

// ExportConstraint is the TierCompliance migration of the legacy export-limit
// path (TASK-060, first P5 flip). It ports, field-for-field, the ceiling
// controller (applyExportLimitRule + exportGuard, optimizer.go:643-1159) and the
// measured-effect convergence backstop (checkExportLimitConvergence + expOverTicks,
// optimizer.go:1170-1231) into a pure Evaluate over a typed ExportSession.
//
// Emitted demands (TierCompliance):
//   - AxisSolarCeilingW ceiling per connected inverter — the sticky, slew- and
//     feed-forward-limited generation cap that holds export ≤ the CSIP limit.
//   - AxisBatterySetpointW point (= −absorb) per battery it charges to soak the
//     surplus before curtailing solar (battery-first).
//   - a ComplianceBreach when the site cannot converge to the cap (zero-lever
//     over-export, or sustained over-cap past the adaptive detection window).
//
// EV-current emission is DEFERRED (see the EV block): the EV setpoint is still
// computed in full because evCmdW/evSetpointA feed the conservation identity, the
// feed-forward ceiling and the relax gate — dropping that computation would make
// the SOLAR ceiling diverge from legacy. Only the emission of the EVSECommand is
// held back, because the 058 Stack.emitCommands cannot yet carry a non-zero OCPP
// connector (it hardcodes ConnectorID 0), so emitting it in shadow would trip a
// spurious per-tick divergence on every EV tick that is a stack-wiring artifact,
// not a controller disagreement. The candidate simply expresses no opinion on the
// EVSE axis, which the candidate-scoped shadow diff correctly treats as
// "still legacy-owned" (shadow.go). Faithful EV emission + connector-carrying
// Stack wiring lands with the active flip (task step 4).
type ExportConstraint struct {
	// sess is this constraint's typed inter-tick state (one instance per Stack).
	// The Stack passes a base *Session for tick scaling; the typed state lives
	// here because there is exactly one ExportConstraint per Stack and the Stack
	// reuses one Session per constraint name across ticks (stack.go), so the
	// binding is 1:1 — the same shape DefaultOptimizer uses for expGuard.
	sess ExportSession
}

// compile-time proof the ExportConstraint satisfies the Constraint interface.
var _ Constraint = (*ExportConstraint)(nil)

// NewExportConstraint builds the export constraint with its session in the
// no-active-cap state.
func NewExportConstraint() *ExportConstraint {
	return &ExportConstraint{sess: newExportSession()}
}

// Name is the stable identity; it keys the Session and appears as Demand.Source.
func (c *ExportConstraint) Name() string { return "export" }

// Tier places the export cap in the CSIP compliance band.
func (c *ExportConstraint) Tier() Tier { return TierCompliance }

// Ported constants. Kept identical to optimizer.go (parameterization is
// TASK-064); the export prefix avoids colliding with the sibling constraints
// that will port import/gen on this pattern.
const (
	exportFilterAlpha       = 0.4    // optimizer.go:696
	exportSOCTaperStart     = 80.0   // optimizer.go:778
	exportSOCStepEstimate   = 1.0    // optimizer.go:787
	exportCeilGain          = 0.5    // optimizer.go:1036
	exportMaxDropW          = 1500.0 // optimizer.go:1060
	exportMaxRiseW          = 500.0  // optimizer.go:1061
	exportEVMinChargeA      = 6.0    // optimizer.go:903
	exportEVDeadbandA       = 0.5    // optimizer.go:904
	exportEVMaxTightenA     = 2.0    // optimizer.go:905
	exportEVMaxRelaxA       = 1.0    // optimizer.go:906
	exportComplianceBreachW = 100.0  // optimizer.go:2143 (complianceBreachW)
	exportBattConvergeFrac  = 0.5    // optimizer.go:72
	exportBattBreachTicks   = 3      // optimizer.go:73
	exportBreachTicks       = 3      // optimizer.go:1168
	exportMarginFrac        = 0.20   // NewDefaultOptimizer default (optimizer.go:226); bench does not override
	exportRelaxCycles       = 5      // NewDefaultOptimizer default (optimizer.go:227)
	exportSOCFull           = 95.0   // SOCFullThreshold default (optimizer.go:224); bench does not override
)

// effectiveExportLimitW reproduces deriveGridConstraints' export leg
// (optimizer.go:447-463): the grid-reported export limit intersected with the
// active CSIP OpModExpLimW override (most-restrictive). NaN = no export limit.
func effectiveExportLimitW(st orchestrator.SystemState) float64 {
	lim := st.Grid.ExportLimitW
	if st.CSIPControl != nil {
		if ap := st.CSIPControl.Base.OpModExpLimW; ap != nil {
			lim = nanMin(lim, apW(ap))
		}
	}
	return lim
}

// apW / nanMin mirror the unexported optimizer helpers (optimizer.go:2240-2251).
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

// Evaluate ports applyExportLimitRule + checkExportLimitConvergence. It updates
// the session's two reset domains on their own cadences, emits the ceiling and
// battery demands, and returns the worst compliance breach for this tick.
func (c *ExportConstraint) Evaluate(in Input, s *Session) ([]Demand, *orchestrator.ComplianceBreach) {
	st := in.State
	sess := &c.sess

	exportLimitW := effectiveExportLimitW(st)

	// Cap cleared → both reset domains clear, no demands (optimizer.go:655-658 +
	// checkExportLimitConvergence :1195-1197: "cap cleared — compliance session over").
	if math.IsNaN(exportLimitW) {
		sess.clearForNoLimit()
		return nil, nil
	}
	// New cap VALUE → fresh controller session; overTicks is deliberately NOT
	// touched here (optimizer.go:661-663 and the expOverTicks note).
	if exportLimitW != sess.ctrl.activeLimitW {
		sess.resetControllerForNewLimit(exportLimitW)
	}

	demands, zeroLeverBreach := c.applyExportControl(in, s, exportLimitW)
	convBreach := c.checkConvergence(in, s, exportLimitW)

	// recordBreach keeps the worst; the convergence check only fires when the
	// controller's own zero-lever breach did not (optimizer.go:1218 "plan.Breach == nil").
	breach := zeroLeverBreach
	if breach == nil {
		breach = convBreach
	}
	if breach != nil && st.CSIPControl != nil {
		breach.MRID = st.CSIPControl.MRID // optimizer.go:374-375
	}
	return demands, breach
}

// applyExportControl ports applyExportLimitRule's controller body
// (optimizer.go:665-1158): filter + safeCount → battery absorption with SOC taper,
// ratchet and stall counter → EV pre-position (computed, not emitted) → sticky
// slew-limited ceiling with feed-forward saturation curtail → zero-lever breach.
func (c *ExportConstraint) applyExportControl(in Input, s *Session, exportLimitW float64) ([]Demand, *orchestrator.ComplianceBreach) {
	st := in.State
	ctrl := &c.sess.ctrl

	margin := exportMarginFrac
	relaxCycles := exportRelaxCycles
	conservativeW := exportLimitW * (1.0 - margin)

	evseW := st.TotalEVSEW()
	netW := st.Grid.NetW

	// Signed net export (+ export, − import). optimizer.go:676-690.
	signedNetExportW := math.NaN()
	if !math.IsNaN(netW) {
		signedNetExportW = -netW
	} else {
		signedNetExportW = 0
		for _, sol := range st.Solar {
			signedNetExportW += sol.PowerW
		}
		for _, b := range st.Batteries {
			signedNetExportW += math.Max(0, b.PowerW)
		}
		signedNetExportW -= evseW
	}
	actualExportW := math.Max(0, signedNetExportW)

	// Low-pass filter (optimizer.go:696-702).
	if math.IsNaN(ctrl.filteredExportW) {
		ctrl.filteredExportW = actualExportW
	} else {
		ctrl.filteredExportW = exportFilterAlpha*actualExportW + (1-exportFilterAlpha)*ctrl.filteredExportW
	}
	filteredExportW := ctrl.filteredExportW

	if filteredExportW <= conservativeW {
		ctrl.safeCount++
	} else {
		ctrl.safeCount = 0
	}

	// Measured battery absorption before any command this tick (optimizer.go:711-716).
	measuredBatteryAbsorbW := 0.0
	for _, b := range st.Batteries {
		if b.Connected && b.PowerW < 0 {
			measuredBatteryAbsorbW += -b.PowerW
		}
	}

	// Active-EV detection (optimizer.go:721-732). No prior-rule command exists in
	// the constraint stack, so the hasEVSECommand guard is unconditionally clear.
	var ev *orchestrator.EVSEState
	for i := range st.EVSEs {
		if st.EVSEs[i].Connected && st.EVSEs[i].SessionActive {
			ev = &st.EVSEs[i]
			break
		}
	}
	if ev == nil {
		ctrl.evSetpointA = math.NaN()
		ctrl.evCmdW = math.NaN()
	}

	// Conservation identity using last COMMANDED values (optimizer.go:748-769).
	identityBattW := measuredBatteryAbsorbW
	if !math.IsNaN(ctrl.batteryAbsorbW) {
		identityBattW = ctrl.batteryAbsorbW
	}
	identityEvW := evseW
	if !math.IsNaN(ctrl.evCmdW) {
		identityEvW = ctrl.evCmdW
	}
	unconstrainedExportW := signedNetExportW + identityBattW + identityEvW

	totalSolarW := 0.0
	for _, sol := range st.Solar {
		if sol.Connected {
			totalSolarW += sol.PowerW
		}
	}
	if unconstrainedExportW > totalSolarW {
		unconstrainedExportW = totalSolarW
	}

	// ── Battery absorption with SOC taper + EV pre-position + ratchet ──────────
	// optimizer.go:777-856.
	var demands []Demand
	batteryAbsorbW := 0.0
	predictedBatteryAbsorbW := 0.0
	for _, b := range st.Batteries {
		if !b.Connected {
			continue
		}
		if !math.IsNaN(b.SOC) && b.SOC >= exportSOCFull {
			continue
		}
		if b.MaxChargeW < 50 {
			continue
		}
		taperFactor := func(soc float64) float64 {
			if math.IsNaN(soc) || soc <= exportSOCTaperStart {
				return 1.0
			}
			if soc >= exportSOCFull || exportSOCFull <= exportSOCTaperStart {
				return 0.0
			}
			return math.Max(0, (exportSOCFull-soc)/(exportSOCFull-exportSOCTaperStart))
		}
		effectiveMaxNow := b.MaxChargeW * taperFactor(b.SOC)
		nextSOC := b.SOC + exportSOCStepEstimate
		effectiveMaxNext := b.MaxChargeW * taperFactor(nextSOC)

		need := math.Max(0, unconstrainedExportW-conservativeW)
		absorb := math.Min(effectiveMaxNow, need)
		predictedNext := math.Min(effectiveMaxNext, need)

		// Ratchet against transient meter noise; taper-driven drops bypass it
		// (optimizer.go:821-829).
		if !math.IsNaN(ctrl.batteryAbsorbW) && ctrl.batteryAbsorbW > absorb {
			if absorb < effectiveMaxNow {
				if ctrl.safeCount < relaxCycles {
					absorb = math.Min(ctrl.batteryAbsorbW, effectiveMaxNow)
				} else {
					absorb = math.Min((absorb+ctrl.batteryAbsorbW)/2, effectiveMaxNow)
				}
			}
		}

		if absorb < 50 {
			continue
		}
		setpoint := -absorb
		demands = append(demands, PointDemand(b.Name, AxisBatterySetpointW, setpoint, TierCompliance, c.Name()))
		batteryAbsorbW += absorb
		predictedBatteryAbsorbW += predictedNext
	}
	if batteryAbsorbW > 0 {
		ctrl.batteryAbsorbW = batteryAbsorbW
	} else {
		ctrl.batteryAbsorbW = math.NaN() // optimizer.go:855
	}

	// ── Closed-loop battery-absorption convergence (leaky stall counter) ──────
	// optimizer.go:858-892. Threshold uses the scaled legacy battBreachTicks
	// (the battery-stall lever is NOT the adaptive-window fix; the export-detection
	// window below is). Ported via the Session's ScaleTicks, identical to
	// DefaultOptimizer.scaleTicks.
	battStallThreshold := s.ScaleTicks(exportBattBreachTicks)
	if batteryAbsorbW > exportComplianceBreachW && measuredBatteryAbsorbW < batteryAbsorbW*exportBattConvergeFrac {
		if ctrl.battStallTicks < battStallThreshold {
			ctrl.battStallTicks++
		}
	} else if ctrl.battStallTicks > 0 {
		ctrl.battStallTicks--
	}
	if ctrl.battStallTicks >= battStallThreshold {
		predictedBatteryAbsorbW = measuredBatteryAbsorbW // discredit phantom absorption
	}

	// ── EV: filtered P-controller (COMPUTED for state/feed-forward, NOT emitted) ─
	// optimizer.go:894-987. See the type doc for why the EVSECommand is deferred.
	if ev != nil {
		voltage := ev.VoltageV
		if voltage <= 0 {
			voltage = 230.0
		}
		residualNeed := unconstrainedExportW - predictedBatteryAbsorbW - conservativeW
		targetA := math.Min(math.Max(residualNeed/voltage, exportEVMinChargeA), ev.MaxCurrentA)

		var newCurrentA float64
		if math.IsNaN(ctrl.evSetpointA) {
			newCurrentA = exportEVMinChargeA // soft-start (optimizer.go:923-927)
		} else {
			diffA := targetA - ctrl.evSetpointA
			switch {
			case math.Abs(diffA) < exportEVDeadbandA:
				newCurrentA = ctrl.evSetpointA
			case diffA > 0:
				newCurrentA = ctrl.evSetpointA + math.Min(diffA, exportEVMaxTightenA)
			default:
				if ctrl.safeCount < relaxCycles {
					newCurrentA = ctrl.evSetpointA
				} else {
					newCurrentA = math.Max(ctrl.evSetpointA+math.Max(diffA, -exportEVMaxRelaxA), exportEVMinChargeA)
					ctrl.safeCount = 0
				}
			}
		}
		// Pre-flight against the hard limit (optimizer.go:966-975).
		predictedExportW := unconstrainedExportW - batteryAbsorbW - newCurrentA*voltage
		if predictedExportW > exportLimitW {
			boost := math.Min((predictedExportW-exportLimitW)/voltage, ev.MaxCurrentA-newCurrentA)
			if boost > 0 {
				newCurrentA += boost
			}
		}
		ctrl.evSetpointA = newCurrentA
		ctrl.evCmdW = newCurrentA * voltage
	}

	// ── Solar curtailment: sticky integrating controller on MEASURED export ────
	// optimizer.go:989-1142.
	newBatteryAbsorbW := math.Max(0, batteryAbsorbW-measuredBatteryAbsorbW)
	effectiveExportW := actualExportW - newBatteryAbsorbW

	totalNameplateW := 0.0
	for _, sol := range st.Solar {
		if sol.Connected {
			totalNameplateW += sol.MaxW
		}
	}

	prevCeilingW := ctrl.solarCeilingW
	var desiredCeilingW float64
	if math.IsNaN(prevCeilingW) {
		desiredCeilingW = totalSolarW + (conservativeW - effectiveExportW)
	} else {
		desiredCeilingW = prevCeilingW + exportCeilGain*(conservativeW-effectiveExportW)
	}
	if desiredCeilingW < 0 {
		desiredCeilingW = 0
	}

	// Slew-limit the feedback change (optimizer.go:1059-1070); skipped on the
	// first tick of an episode (NaN prev).
	if !math.IsNaN(prevCeilingW) {
		if desiredCeilingW < prevCeilingW-exportMaxDropW {
			desiredCeilingW = prevCeilingW - exportMaxDropW
		} else if desiredCeilingW > prevCeilingW+exportMaxRiseW {
			desiredCeilingW = prevCeilingW + exportMaxRiseW
		}
		if desiredCeilingW < 0 {
			desiredCeilingW = 0
		}
	}

	// Feed-forward proactive curtailment, always applied, bypassing the down-slew
	// (optimizer.go:1072-1102).
	homeSinkW := totalSolarW - actualExportW - measuredBatteryAbsorbW - evseW
	if homeSinkW < 0 {
		homeSinkW = 0
	}
	evCmdW := 0.0
	if !math.IsNaN(ctrl.evCmdW) {
		evCmdW = ctrl.evCmdW
	}
	feedForwardCeilingW := homeSinkW + predictedBatteryAbsorbW + evCmdW + conservativeW
	if feedForwardCeilingW < desiredCeilingW {
		desiredCeilingW = math.Max(0, feedForwardCeilingW)
	}

	// Sticky ceiling: clamp to nameplate, stay engaged, emit per-inverter shares
	// (optimizer.go:1104-1142).
	if totalNameplateW > 0 {
		if desiredCeilingW > totalNameplateW {
			desiredCeilingW = totalNameplateW
		}
		ctrl.solarCeilingW = desiredCeilingW
		for _, sol := range st.Solar {
			if !sol.Connected {
				continue
			}
			curtailTo := desiredCeilingW * (sol.MaxW / totalNameplateW)
			demands = append(demands, CeilingDemand(sol.Name, AxisSolarCeilingW, curtailTo, TierCompliance, c.Name()))
		}
	}

	// Zero-lever compliance breach (optimizer.go:1144-1156).
	var breach *orchestrator.ComplianceBreach
	if actualExportW > exportLimitW+exportComplianceBreachW && desiredCeilingW <= exportComplianceBreachW {
		breach = &orchestrator.ComplianceBreach{
			LimitType:  "export",
			LimitW:     exportLimitW,
			MeasuredW:  actualExportW,
			ShortfallW: actualExportW - exportLimitW,
			Reason:     "generation curtailed to minimum; battery and EV cannot absorb the surplus",
		}
	}
	return demands, breach
}

// checkConvergence ports checkExportLimitConvergence (optimizer.go:1194-1231):
// the measured-effect backstop keyed to the SESSION-scoped overTicks counter.
//
// The detection window is the ADAPTIVE Plant.ExportDetectionWindowTicks
// (AD-007) — control latency + meter lag over the tick — in place of the fixed,
// scaled exportBreachTicks. This is the M2 fix: the fixed ~9 s window raced the
// ~11 s oracle boundary on battery-charge-disabled (058 doc). With bench FAST
// defaults (controlLatency 3 s, meterLag 5 s, tick 3 s) it evaluates to 3 ticks —
// bit-identical to the scaled legacy constant — so shadow parity holds while the
// window tracks real plant physics for a slower plant.
func (c *ExportConstraint) checkConvergence(in Input, s *Session, exportLimitW float64) *orchestrator.ComplianceBreach {
	sess := &c.sess
	netW := in.State.Grid.NetW

	// NaN cap is handled by Evaluate's early clear; here the cap is always active.
	// A meter-blind tick is evidence of nothing: HOLD the counter (optimizer.go:1199-1204,
	// "a blind meter must not launder a breach").
	if math.IsNaN(netW) {
		return nil
	}
	exportW := math.Max(0, -netW)
	threshold := c.detectionWindowTicks(in)
	if exportW > exportLimitW+exportComplianceBreachW {
		if sess.overTicks < threshold {
			sess.overTicks++
		}
	} else if sess.overTicks > 0 {
		sess.overTicks--
	}
	if sess.overTicks >= threshold {
		return &orchestrator.ComplianceBreach{
			LimitType:  "export",
			LimitW:     exportLimitW,
			MeasuredW:  exportW,
			ShortfallW: exportW - exportLimitW,
			Reason:     "export remains over the cap after correction was commanded — the site is not converging to the limit",
		}
	}
	return nil
}

// detectionWindowTicks derives the adaptive export-breach window from plant
// physics. The export cap is site-wide, so it takes the LARGEST per-inverter
// window across connected inverters (the slowest plant governs: never fire before
// any inverter could have shown the correction at the meter). With no connected
// inverter it falls back to the meter lag alone; the floor of 2 in
// DetectionWindowTicks keeps it sane.
func (c *ExportConstraint) detectionWindowTicks(in Input) int {
	tick := in.TickSeconds
	window := 0
	for _, sol := range in.State.Solar {
		if !sol.Connected {
			continue
		}
		if n := in.Plant.ExportDetectionWindowTicks(sol.Name, tick); n > window {
			window = n
		}
	}
	if window == 0 {
		window = DetectionWindowTicks(0, in.Plant.Meter.MeterLagS, tick)
	}
	return window
}
