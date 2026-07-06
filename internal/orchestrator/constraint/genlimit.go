package constraint

import (
	"math"

	"lexa-hub/internal/orchestrator"
)

// GenLimitConstraint is the TierCompliance migration of the legacy
// generation-limit path (TASK-061). It ports applyGenLimitRule (optimizer.go:1251-1284)
// — the absolute, sticky, nameplate-distributed inverter ceiling — and
// checkGenLimitConvergence (optimizer.go:1299-1379) — the measured-effect backstop
// with the meter-INDEPENDENT generation floor — into a pure Evaluate over a typed
// GenSession.
//
// Emitted demands (TierCompliance):
//   - AxisSolarCeilingW ceiling per connected inverter: maxLimitW distributed by
//     nameplate share. Absolute (not slewed/filtered like the export ceiling) and
//     re-issued every tick while the cap is active — the inverter clamps output to
//     min(potential, ceiling), so commanding it can never raise generation and is a
//     harmless no-op when potential is already under the cap. The legacy rule's
//     manual "keep the tighter of gen and export" reconciliation
//     (optimizer.go:1271-1275) is GONE: the arbiter now intersects gen's ceiling
//     with the ExportConstraint's per-inverter ceiling most-restrictively, which is
//     exactly "keep the tighter" but explicit and diagnosable.
//   - a ComplianceBreach{LimitType:"generation"} when measured generation stays over
//     the cap past the detection window after curtailment was commanded — the
//     device ACKed the write but did not honour it (audit: enable-gate-curtail /
//     reject-write-curtail).
//
// The cap-release-edge explicit uncurtail is NOT ported: it was deleted upstream in
// TASK-032 (optimizer.go:305-309 note) and folded into applyRestoreRule, which stays
// in DefaultOptimizer. In shadow the candidate simply emits no gen ceiling once the
// cap clears (expressing no opinion on the solar axis), which the candidate-scoped
// diff correctly reads as "still legacy-owned" — the legacy NaN restore is not a
// divergence. Faithful restore emission lands with economics migration, not here.
type GenLimitConstraint struct {
	sess GenSession
}

// compile-time proof GenLimitConstraint satisfies the Constraint interface.
var _ Constraint = (*GenLimitConstraint)(nil)

// NewGenLimitConstraint builds the generation constraint in the no-active-cap state.
func NewGenLimitConstraint() *GenLimitConstraint {
	return &GenLimitConstraint{sess: newGenSession()}
}

// Name is the stable identity; it keys the Session and appears as Demand.Source.
func (c *GenLimitConstraint) Name() string { return "gen" }

// Tier places the generation cap in the CSIP compliance band.
func (c *GenLimitConstraint) Tier() Tier { return TierCompliance }

// effectiveGenLimitW reproduces deriveGridConstraints' max-limit leg
// (optimizer.go:457-459): the grid-reported absolute generation cap intersected
// with the active CSIP OpModMaxLimW override (most-restrictive). NaN = no cap.
func effectiveGenLimitW(st orchestrator.SystemState) float64 {
	lim := st.Grid.MaxLimitW
	if st.CSIPControl != nil {
		if ap := st.CSIPControl.Base.OpModMaxLimW; ap != nil {
			lim = nanMin(lim, apW(ap))
		}
	}
	return lim
}

// Evaluate ports applyGenLimitRule + checkGenLimitConvergence.
func (c *GenLimitConstraint) Evaluate(in Input, s *Session) ([]Demand, *orchestrator.ComplianceBreach) {
	st := in.State
	sess := &c.sess

	genLimitW := effectiveGenLimitW(st)

	// Cap cleared → session clears, no demands (optimizer.go:1252-1254 +
	// checkGenLimitConvergence :1300-1303).
	if math.IsNaN(genLimitW) {
		sess.clearForNoLimit()
		return nil, nil
	}

	demands := c.applyGenControl(st, genLimitW)
	breach := c.checkConvergence(in, genLimitW)
	if breach != nil && st.CSIPControl != nil {
		breach.MRID = st.CSIPControl.MRID // optimizer.go:374-375
	}
	return demands, breach
}

// applyGenControl ports applyGenLimitRule's body (optimizer.go:1255-1283): the
// absolute per-inverter ceiling distributed by nameplate share. Stateless — no
// session write. When no inverter is connected there is nothing to curtail.
func (c *GenLimitConstraint) applyGenControl(st orchestrator.SystemState, genLimitW float64) []Demand {
	totalNameplateW := 0.0
	for _, sol := range st.Solar {
		if sol.Connected {
			totalNameplateW += sol.MaxW
		}
	}
	if totalNameplateW <= 0 {
		return nil
	}
	var demands []Demand
	for _, sol := range st.Solar {
		if !sol.Connected {
			continue
		}
		curtailTo := genLimitW * (sol.MaxW / totalNameplateW)
		demands = append(demands, CeilingDemand(sol.Name, AxisSolarCeilingW, curtailTo, TierCompliance, c.Name()))
	}
	return demands
}

// checkConvergence ports checkGenLimitConvergence (optimizer.go:1299-1379): the
// tolerance-band session reset, the meter-independent generation floor, and the
// leaky convergence counter.
func (c *GenLimitConstraint) checkConvergence(in Input, genLimitW float64) *orchestrator.ComplianceBreach {
	st := in.State
	sess := &c.sess

	// Reset the counter only when the cap changes MEANINGFULLY. The decoded cap can
	// vary by a hair tick-to-tick (the watts→ActivePower value×10^mult round-trip
	// through the bus), and resetting on a bit-exact inequality zeroed overCount
	// every tick so a sustained breach never reached the threshold — part of the
	// reject-write/enable-gate-curtail nondeterminism. Track the session by a
	// tolerance band and follow minor drift instead (optimizer.go:1304-1314).
	if math.IsNaN(sess.activeLimitW) || math.Abs(genLimitW-sess.activeLimitW) > exportComplianceBreachW {
		sess.resetForNewLimit(genLimitW)
	} else {
		sess.activeLimitW = genLimitW
	}

	measuredGenW := 0.0
	for _, sol := range st.Solar {
		if sol.Connected && sol.PowerW > 0 {
			measuredGenW += sol.PowerW
		}
	}

	// Independent generation floor from the grid meter. The inverter's self-reported
	// power can be corrupted by the same fault that ignores the curtailment — a
	// device that echoes the commanded limit back while still generating full output
	// (audit: enable-gate-curtail) reports a compliant power even though it is not.
	// The meter is independent: from the site energy balance, generation = export +
	// load + evse + batteryCharge − batteryDischarge, and load/evse/batteryCharge are
	// all ≥ 0, so generation ≥ export − batteryDischarge regardless of what the
	// inverter claims. Use this lower bound so an echoed-but-ignored limit is still
	// caught. (Ported verbatim from optimizer.go:1323-1343 — HARD preserve, TASK-061.)
	netW := st.Grid.NetW
	if !math.IsNaN(netW) {
		exportW := math.Max(0, -netW)
		batteryDischargeW := 0.0
		for _, b := range st.Batteries {
			if b.Connected && b.PowerW > 0 {
				batteryDischargeW += b.PowerW
			}
		}
		if floor := exportW - batteryDischargeW; floor > measuredGenW {
			measuredGenW = floor
		}
	}

	// Leaky counter, not a hard consecutive run (optimizer.go:1345-1359): a single
	// sub-threshold sample decrements rather than zeroing a climbing breach, so a
	// sustained miss with occasional noise still escalates while a genuine
	// convergence drains to zero within a few ticks. The detection window is the
	// ADAPTIVE Plant window (AD-007) in place of the fixed scaleTicks(genBreachTicks);
	// at bench defaults it evaluates to 3 — bit-identical to the legacy constant —
	// but tracks real plant physics for a slower plant.
	threshold := c.detectionWindowTicks(in)
	if measuredGenW > genLimitW+exportComplianceBreachW {
		if sess.overCount < threshold {
			sess.overCount++
		}
	} else if sess.overCount > 0 {
		sess.overCount--
	}

	if sess.overCount >= threshold {
		return &orchestrator.ComplianceBreach{
			LimitType:  "generation",
			LimitW:     genLimitW,
			MeasuredW:  measuredGenW,
			ShortfallW: measuredGenW - genLimitW,
			Reason:     "inverter output remains above the generation cap after curtailment was commanded — the device is not honouring the command",
		}
	}
	return nil
}

// detectionWindowTicks derives the adaptive gen-breach window from plant physics.
// The generation cap is site-wide over the inverters, so it takes the LARGEST
// per-inverter window across connected inverters (the slowest plant governs: never
// fire before any inverter could have shown the correction at the meter). With no
// connected inverter it falls back to the meter lag alone; the floor of 2 in
// DetectionWindowTicks keeps it sane. Identical derivation to the export ceiling —
// both are inverter-lever compliance caps read at the site meter.
func (c *GenLimitConstraint) detectionWindowTicks(in Input) int {
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
