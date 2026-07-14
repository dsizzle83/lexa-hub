package constraint

import (
	"math"

	"lexa-hub/internal/orchestrator"
)

// AusGenLimitConstraint is the TierCompliance SHADOW mirror of the WP-11
// CSIP-AUS gross-generation cap (opModGenLimW). It ports
// applyAusGenerationLimitRule + checkAusGenerationConvergence
// (orchestrator/auslimits.go) into a pure Evaluate over a typed AusGenSession,
// following the TASK-061 porting pattern (GenLimitConstraint is the template).
// It runs under constraint_shadow REGARDLESS of hub.json's
// `enforce_aus_limits` — shadow observes the axis even while the cascade does
// not enforce it, exactly how the export shadow ran before its flip. Per the
// repo's do-not-strip rule, this copy and the cascade copy must BOTH exist
// until the per-axis `active` flip.
//
// DISAMBIGUATION: "gen-aus" is NOT "gen" (GenLimitConstraint). "gen" mirrors
// opModMaxLimW — a cap on inverter OUTPUT alone, battery excluded by design.
// This constraint mirrors opModGenLimW — GROSS generation, solar output PLUS
// battery discharge, the CSIP-AUS dynamic-envelope axis.
//
// Demand translation (cascade → demand model):
//   - the battery-discharge participation cap becomes a CeilingDemand (Max
//     bound) on AxisBatterySetpointW per connected battery: batteries may only
//     discharge into the headroom measured solar leaves under the cap. The
//     cascade trims COMMITTED commands in command order and threads the
//     remainder into Rule 5; the demand model expresses that same authority as
//     an upper bound the arbiter intersects with economics' point demand —
//     identical outcome for the single-battery bench, proportional to
//     MaxDischargeW across several (the demand-model rendering of the
//     cascade's sequential allocation).
//   - the solar ceiling becomes a CeilingDemand on AxisSolarCeilingW, the full
//     cap distributed by nameplate share. The cascade subtracts discharge
//     COMMITTED BEFORE its slot in the pass (fixed-dispatch/plan/import-rule
//     discharge — zero in every in-family scenario); a constraint cannot see
//     sibling demands, so this mirror emits the full cap and lets its battery
//     ceiling do the battery-side narrowing. The two sides agree everywhere
//     except a simultaneous import-cap-discharge + gen-cap tick, the same
//     documented contradictory-cap deviation TASK-061 recorded for
//     import-vs-gen arbitration (TestImportGen_SimultaneousCapArbitration).
type AusGenLimitConstraint struct {
	sess AusGenSession
}

// compile-time proof AusGenLimitConstraint satisfies the Constraint interface.
var _ Constraint = (*AusGenLimitConstraint)(nil)

// AusGenSession is the constraint's typed inter-tick state. Ports ausGenGuard
// (orchestrator/auslimits.go) field-for-field — like GenSession it keeps a
// SINGLE reset domain: the control side is stateless (the ceilings re-derive
// from the cap every tick), so the only carried state is the convergence
// guard, and activeLimitW + overCount reset TOGETHER on a meaningful cap
// change.
type AusGenSession struct {
	activeLimitW float64 // cap value when guard was reset; NaN = no active cap
	overCount    int     // consecutive ticks measured GROSS generation stayed over the cap
}

func newAusGenSession() AusGenSession {
	return AusGenSession{activeLimitW: math.NaN()}
}

// clearForNoLimit is the cap-cleared reset (whole compliance session over).
func (s *AusGenSession) clearForNoLimit() {
	s.activeLimitW = math.NaN()
	s.overCount = 0
}

// resetForNewLimit starts a fresh convergence session for a meaningfully
// changed cap value.
func (s *AusGenSession) resetForNewLimit(limitW float64) {
	s.activeLimitW = limitW
	s.overCount = 0
}

// NewAusGenLimitConstraint builds the constraint in the no-active-cap state.
func NewAusGenLimitConstraint() *AusGenLimitConstraint {
	return &AusGenLimitConstraint{sess: newAusGenSession()}
}

// Name is the stable identity; it keys the Session and appears as
// Demand.Source (and in the FIX-F active set / breachOwner mapping).
func (c *AusGenLimitConstraint) Name() string { return "gen-aus" }

// Tier places the AUS generation cap in the CSIP compliance band.
func (c *AusGenLimitConstraint) Tier() Tier { return TierCompliance }

// effectiveAusGenLimitW reads the gross-generation cap. Unlike the
// export/gen/import legs there is no DERControlBase override to intersect:
// opModGenLimW is an EXTENDED control that reaches the optimizer pre-distilled
// on GridState (cmd/hub adopts bus.ActiveControl.gen_lim_w — WP-11 adoption is
// unconditional; only enforcement is flagged). NaN = no cap.
func effectiveAusGenLimitW(st orchestrator.SystemState) float64 {
	return st.Grid.GenLimitW
}

// Evaluate ports applyAusGenerationLimitRule + checkAusGenerationConvergence.
func (c *AusGenLimitConstraint) Evaluate(in Input, s *Session) ([]Demand, *orchestrator.ComplianceBreach) {
	st := in.State
	genLimitW := effectiveAusGenLimitW(st)

	// Cap cleared → session clears, no demands (the candidate expresses no
	// opinion; the candidate-scoped shadow diff stays inert).
	if math.IsNaN(genLimitW) {
		c.sess.clearForNoLimit()
		return nil, nil
	}

	demands := c.applyGenControl(st, genLimitW)
	breach := c.checkConvergence(in, genLimitW)
	if breach != nil && st.CSIPControl != nil {
		breach.MRID = st.CSIPControl.MRID
	}
	return demands, breach
}

// applyGenControl ports applyAusGenerationLimitRule's two levers into demands.
// Stateless — no session write.
func (c *AusGenLimitConstraint) applyGenControl(st orchestrator.SystemState, genLimitW float64) []Demand {
	var demands []Demand

	// Lever 1 — battery-discharge participation cap: discharge only into the
	// headroom measured solar leaves under the cap (solar is never curtailed
	// to make room for stored energy).
	totalSolarW := 0.0
	for _, sol := range st.Solar {
		if sol.Connected && sol.PowerW > 0 {
			totalSolarW += sol.PowerW
		}
	}
	dischargeCapW := math.Max(0, genLimitW-totalSolarW)
	totalRatingW := 0.0
	for _, b := range st.Batteries {
		if b.Connected {
			totalRatingW += math.Max(0, b.MaxDischargeW)
		}
	}
	for _, b := range st.Batteries {
		if !b.Connected {
			continue
		}
		share := dischargeCapW
		if totalRatingW > 0 {
			share = dischargeCapW * math.Max(0, b.MaxDischargeW) / totalRatingW
		}
		demands = append(demands, CeilingDemand(b.Name, AxisBatterySetpointW, share, TierCompliance, c.Name()))
	}

	// Lever 2 — solar ceiling: the full cap distributed by nameplate share
	// (see the type doc for why the cascade's committed-discharge subtraction
	// has no demand-model equivalent), merged most-restrictively by the
	// arbiter with the "export"/"gen" ceilings.
	totalNameplateW := 0.0
	for _, sol := range st.Solar {
		if sol.Connected {
			totalNameplateW += sol.MaxW
		}
	}
	if totalNameplateW > 0 {
		for _, sol := range st.Solar {
			if !sol.Connected {
				continue
			}
			curtailTo := genLimitW * (sol.MaxW / totalNameplateW)
			demands = append(demands, CeilingDemand(sol.Name, AxisSolarCeilingW, curtailTo, TierCompliance, c.Name()))
		}
	}
	return demands
}

// checkConvergence ports checkAusGenerationConvergence: tolerance-band session
// reset, the ADAPTED gross-generation meter floor, and the leaky counter.
func (c *AusGenLimitConstraint) checkConvergence(in Input, genLimitW float64) *orchestrator.ComplianceBreach {
	st := in.State
	sess := &c.sess

	if math.IsNaN(sess.activeLimitW) || math.Abs(genLimitW-sess.activeLimitW) > exportComplianceBreachW {
		sess.resetForNewLimit(genLimitW)
	} else {
		sess.activeLimitW = genLimitW
	}

	// Measured GROSS generation: solar output plus battery discharge.
	measuredGrossW := 0.0
	for _, sol := range st.Solar {
		if sol.Connected && sol.PowerW > 0 {
			measuredGrossW += sol.PowerW
		}
	}
	for _, b := range st.Batteries {
		if b.Connected && b.PowerW > 0 {
			measuredGrossW += b.PowerW
		}
	}

	// Independent GROSS-generation floor from the grid meter — ADAPTED from
	// GenLimitConstraint's `gen ≥ export − batteryDischarge` floor (HARD
	// preserve). Battery discharge is INSIDE this cap's quantity, so the
	// subtraction drops out: grossGen = export + load + evse + batteryCharge
	// − import with load/evse/batteryCharge all ≥ 0, hence grossGen ≥ −netW
	// regardless of what any device self-reports. An echoed-but-ignored
	// curtailment is still caught. (Ported verbatim from
	// checkAusGenerationConvergence, orchestrator/auslimits.go.)
	if !math.IsNaN(st.Grid.NetW) {
		if floor := -st.Grid.NetW; floor > measuredGrossW {
			measuredGrossW = floor
		}
	}

	// Leaky counter with the ADAPTIVE plant detection window (AD-007) in
	// place of the cascade's fixed scaleTicks(ausGenBreachTicks); bench
	// defaults evaluate to 3 — bit-identical.
	threshold := c.detectionWindowTicks(in)
	if measuredGrossW > genLimitW+exportComplianceBreachW {
		if sess.overCount < threshold {
			sess.overCount++
		}
	} else if sess.overCount > 0 {
		sess.overCount--
	}

	if sess.overCount >= threshold {
		return &orchestrator.ComplianceBreach{
			LimitType:  "generation-aus",
			LimitW:     genLimitW,
			MeasuredW:  measuredGrossW,
			ShortfallW: measuredGrossW - genLimitW,
			Reason:     "gross generation (solar + battery discharge) remains above the CSIP-AUS generation cap after curtailment was commanded — a device is not honouring the command",
		}
	}
	return nil
}

// detectionWindowTicks derives the adaptive breach window from plant physics.
// The gross-generation cap has TWO lever families — inverter ceilings and
// battery-discharge caps — so the window is the LARGEST across both (the
// slowest lever governs: never fire before every lever could have shown its
// correction at the meter). With no connected device it falls back to the
// meter lag alone; DetectionWindowTicks' floor of 2 keeps it sane.
func (c *AusGenLimitConstraint) detectionWindowTicks(in Input) int {
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
