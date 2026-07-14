package constraint

import (
	"math"

	"lexa-hub/internal/orchestrator"
)

// AusLoadLimitConstraint is the TierCompliance SHADOW mirror of the WP-11
// CSIP-AUS gross-load cap (opModLoadLimW). It ports applyAusLoadLimitRule +
// checkAusLoadConvergence (orchestrator/auslimits.go) into a pure Evaluate
// over a typed AusLoadSession, following the TASK-061 porting pattern
// (ImportLimitConstraint is the closest template — same NaN-hold convergence
// counter, same sticky anti-oscillation guard). Like every WP-11 shadow
// constraint it runs under constraint_shadow REGARDLESS of
// `enforce_aus_limits`; do not strip either copy.
//
// Gross load = home consumption + EV charging + battery charging, measured
// from the site energy balance: grossLoad = solar + batteryDischarge + netW.
// A meter-blind tick (netW NaN) re-emits the sticky EV ceiling (fail-closed —
// a cap is conservative) and holds every counter, mirroring the cascade and
// checkImportConvergence's NaN-hold semantics.
//
// Demand translation (cascade → demand model):
//   - the battery-charge cap becomes a FLOOR bound on AxisBatterySetpointW
//     (Min = −allowedCharge, Max unbounded) per connected battery: charge —
//     a negative setpoint — may not exceed the headroom the cap leaves over
//     the measured non-battery load. The cascade trims committed charge
//     commands post-economics; the arbiter clamps economics' charge point up
//     to the floor — identical outcome for the single-battery bench,
//     proportional to MaxChargeW across several.
//   - the EV curtail becomes a sticky CeilingDemand on AxisEVSECurrentA for
//     the FIRST active session (the export rule's single-EV pattern, kept for
//     parity with the cascade).
//
// Battery DISCHARGE is deliberately not a lever — it reduces import, not
// consumption; import relief is the "import" constraint's job.
type AusLoadLimitConstraint struct {
	sess AusLoadSession
}

// compile-time proof AusLoadLimitConstraint satisfies the Constraint interface.
var _ Constraint = (*AusLoadLimitConstraint)(nil)

// AusLoadSession is the constraint's typed inter-tick state. Ports
// ausLoadGuard (orchestrator/auslimits.go) field-for-field. Like
// ImportSession it keeps a SINGLE reset domain: a meaningful cap-value change
// resets every field together, including breachTicks.
type AusLoadSession struct {
	activeLimitW float64 // limit value when guard was reset; NaN = no active limit
	evLimitA     float64 // sticky EV current ceiling (A); NaN = EV lever not engaged
	safeCount    int     // consecutive ticks gross load ≤ hard cap (EV relax gate)
	breachTicks  int     // leaky convergence counter with NaN-hold semantics
}

func newAusLoadSession() AusLoadSession {
	return AusLoadSession{activeLimitW: math.NaN(), evLimitA: math.NaN()}
}

// clearForNoLimit is the cap-cleared reset (whole compliance session over).
func (s *AusLoadSession) clearForNoLimit() {
	*s = newAusLoadSession()
}

// resetForNewLimit starts a fresh guard session for a meaningfully changed cap
// value — evLimitA, safeCount, breachTicks all reset (single reset domain).
func (s *AusLoadSession) resetForNewLimit(limitW float64) {
	*s = AusLoadSession{activeLimitW: limitW, evLimitA: math.NaN()}
}

// Ported constants (cascade twins in orchestrator/auslimits.go; margin/relax
// mirror the DefaultOptimizer defaults the bench never overrides).
const (
	ausLoadMarginFrac = importMarginFrac // ImportMarginFrac default
	ausLoadRelaxCycle = importRelaxCycle // ExportRelaxCycles default, shared relax gate
	ausLoadEVMaxRelax = 2.0              // ausLoadEVMaxRelaxA (auslimits.go)
	ausLoadBreachTick = 3                // ausLoadBreachTicks — compliance latency policy
)

// NewAusLoadLimitConstraint builds the constraint in the no-active-limit state.
func NewAusLoadLimitConstraint() *AusLoadLimitConstraint {
	return &AusLoadLimitConstraint{sess: newAusLoadSession()}
}

// Name is the stable identity; it keys the Session and appears as
// Demand.Source (and in the FIX-F active set / breachOwner mapping).
func (c *AusLoadLimitConstraint) Name() string { return "load-aus" }

// Tier places the AUS load cap in the CSIP compliance band.
func (c *AusLoadLimitConstraint) Tier() Tier { return TierCompliance }

// effectiveAusLoadLimitW reads the gross-load cap. No DERControlBase leg —
// opModLoadLimW is an EXTENDED control, pre-distilled onto GridState by
// cmd/hub's unconditional adoption (WP-11). NaN = no cap.
func effectiveAusLoadLimitW(st orchestrator.SystemState) float64 {
	return st.Grid.LoadLimitW
}

// Evaluate ports applyAusLoadLimitRule + checkAusLoadConvergence.
func (c *AusLoadLimitConstraint) Evaluate(in Input, s *Session) ([]Demand, *orchestrator.ComplianceBreach) {
	st := in.State
	sess := &c.sess

	loadLimitW := effectiveAusLoadLimitW(st)
	if math.IsNaN(loadLimitW) {
		sess.clearForNoLimit()
		return nil, nil
	}

	// Session management: meaningful cap change restarts the whole guard;
	// sub-threshold decode drift is tracked (tolerance band, mirroring the
	// cascade and the import constraint).
	if math.IsNaN(sess.activeLimitW) || math.Abs(loadLimitW-sess.activeLimitW) > exportComplianceBreachW {
		sess.resetForNewLimit(loadLimitW)
	} else {
		sess.activeLimitW = loadLimitW
	}

	demands := c.applyLoadControl(st, loadLimitW)
	breach := c.checkConvergence(in, loadLimitW)
	if breach != nil && st.CSIPControl != nil {
		breach.MRID = st.CSIPControl.MRID
	}
	return demands, breach
}

// grossLoadW measures gross site load from the energy balance:
// grossLoad = solar + batteryDischarge + netW (NaN when meter-blind).
func grossLoadW(st orchestrator.SystemState) float64 {
	if math.IsNaN(st.Grid.NetW) {
		return math.NaN()
	}
	solarW := 0.0
	for _, sol := range st.Solar {
		if sol.Connected && sol.PowerW > 0 {
			solarW += sol.PowerW
		}
	}
	battDischargeW := 0.0
	for _, b := range st.Batteries {
		if b.Connected && b.PowerW > 0 {
			battDischargeW += b.PowerW
		}
	}
	return math.Max(0, solarW+battDischargeW+st.Grid.NetW)
}

// applyLoadControl ports applyAusLoadLimitRule's two levers into demands.
func (c *AusLoadLimitConstraint) applyLoadControl(st orchestrator.SystemState, loadLimitW float64) []Demand {
	sess := &c.sess

	gross := grossLoadW(st)
	// Meter-blind: hold the sticky EV ceiling (fail-closed), no new decision.
	if math.IsNaN(gross) {
		return c.stickyEVDemand(st)
	}

	if gross <= loadLimitW {
		sess.safeCount++
	} else {
		sess.safeCount = 0
	}
	conservativeW := loadLimitW * (1 - ausLoadMarginFrac)

	// Lever 1 — battery-charge floor: charge fits the headroom the
	// conservative target leaves over the measured non-battery load.
	battChargeW := 0.0
	for _, b := range st.Batteries {
		if b.Connected && b.PowerW < 0 {
			battChargeW += -b.PowerW
		}
	}
	nonBattLoadW := math.Max(0, gross-battChargeW)
	allowedChargeW := math.Max(0, conservativeW-nonBattLoadW)

	var demands []Demand
	totalRatingW := 0.0
	for _, b := range st.Batteries {
		if b.Connected {
			totalRatingW += math.Max(0, b.MaxChargeW)
		}
	}
	for _, b := range st.Batteries {
		if !b.Connected {
			continue
		}
		share := allowedChargeW
		if totalRatingW > 0 {
			share = allowedChargeW * math.Max(0, b.MaxChargeW) / totalRatingW
		}
		demands = append(demands, Demand{
			Device: b.Name,
			Axis:   AxisBatterySetpointW,
			Min:    -share, // charge (negative setpoint) bounded from below
			Max:    math.NaN(),
			Tier:   TierCompliance,
			Source: c.Name(),
		})
	}

	// Lever 2 — sticky EV curtail. Engage on a hard-cap breach; once engaged,
	// stay engaged until the cap clears or the session ends.
	if math.IsNaN(sess.evLimitA) && gross <= loadLimitW {
		return demands
	}
	ev := firstActiveEVSE(st)
	if ev == nil {
		sess.evLimitA = math.NaN() // no session — lever released
		return demands
	}
	voltage := ev.VoltageV
	if voltage <= 0 {
		voltage = 230.0
	}
	evMeasuredW := 0.0
	for _, e := range st.EVSEs {
		if e.Connected && e.SessionActive {
			evMeasuredW += e.PowerW
		}
	}
	homeW := math.Max(0, nonBattLoadW-evMeasuredW)
	// chargeAfterW mirrors the cascade's neutralise semantics: a charge that
	// fits the headroom survives whole; one that exceeds it is shed entirely
	// (the arbiter discards an out-of-bound economics proposal the same way).
	chargeAfterW := battChargeW
	if battChargeW > allowedChargeW+1 {
		chargeAfterW = 0
	}
	evAllowanceW := math.Max(0, conservativeW-homeW-chargeAfterW)
	targetA := math.Min(evAllowanceW/voltage, ev.MaxCurrentA)
	if targetA < exportEVMinChargeA {
		targetA = 0 // below the IEC floor → suspend
	}

	newA := targetA
	if prior := sess.evLimitA; !math.IsNaN(prior) && targetA > prior {
		// Relax only after sustained compliance, one bounded step at a time
		// (tightening is always immediate).
		if sess.safeCount < ausLoadRelaxCycle {
			newA = prior
		} else {
			newA = math.Min(targetA, prior+ausLoadEVMaxRelax)
			sess.safeCount = 0
		}
		if newA > 0 && newA < exportEVMinChargeA {
			if targetA >= exportEVMinChargeA {
				newA = exportEVMinChargeA
			} else {
				newA = 0
			}
		}
	}
	sess.evLimitA = newA
	return append(demands, CeilingDemand(evseKey(ev.StationID, ev.ConnectorID), AxisEVSECurrentA, newA, TierCompliance, c.Name()))
}

// stickyEVDemand re-emits the held EV ceiling on a meter-blind tick, if one is
// engaged and the session is still up.
func (c *AusLoadLimitConstraint) stickyEVDemand(st orchestrator.SystemState) []Demand {
	if math.IsNaN(c.sess.evLimitA) {
		return nil
	}
	ev := firstActiveEVSE(st)
	if ev == nil {
		return nil
	}
	return []Demand{CeilingDemand(evseKey(ev.StationID, ev.ConnectorID), AxisEVSECurrentA, c.sess.evLimitA, TierCompliance, c.Name())}
}

// firstActiveEVSE returns the first connected EVSE with an active session —
// the export rule's single-EV pattern, kept for cascade parity.
func firstActiveEVSE(st orchestrator.SystemState) *orchestrator.EVSEState {
	for i := range st.EVSEs {
		if st.EVSEs[i].Connected && st.EVSEs[i].SessionActive {
			return &st.EVSEs[i]
		}
	}
	return nil
}

// checkConvergence ports checkAusLoadConvergence: the measured-effect backstop
// with checkImportConvergence's NaN-HOLD semantics (HARD preserve) and leaky
// counter, over gross load.
func (c *AusLoadLimitConstraint) checkConvergence(in Input, loadLimitW float64) *orchestrator.ComplianceBreach {
	sess := &c.sess

	gross := grossLoadW(in.State)
	// A meter-blind tick is evidence of nothing: neither breach nor
	// compliance. HOLD the counter (never reset), mirroring
	// checkImportConvergence verbatim.
	if math.IsNaN(gross) {
		return nil
	}

	threshold := c.detectionWindowTicks(in)
	if gross > loadLimitW+exportComplianceBreachW {
		if sess.breachTicks < threshold {
			sess.breachTicks++
		}
	} else if sess.breachTicks > 0 {
		sess.breachTicks--
	}
	if sess.breachTicks >= threshold {
		return &orchestrator.ComplianceBreach{
			LimitType:  "load-aus",
			LimitW:     loadLimitW,
			MeasuredW:  gross,
			ShortfallW: gross - loadLimitW,
			Reason:     "gross load remains over the CSIP-AUS load cap after charge-shed and EV curtailment — the remaining load is not sheddable or a device is not honouring its command",
		}
	}
	return nil
}

// detectionWindowTicks derives the adaptive breach window from plant physics.
// The load levers are battery charge and EV current, both read back at the
// site meter — size from the slowest connected battery (the import
// constraint's analogue) with the meter-lag fallback. Bench defaults evaluate
// to 3, bit-identical to the cascade's fixed ausLoadBreachTicks.
func (c *AusLoadLimitConstraint) detectionWindowTicks(in Input) int {
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
