package constraint

import "math"

// ExportSession is the ExportConstraint's typed inter-tick state. It ports the
// two DIFFERENT reset domains the legacy export path kept as separate fields on
// DefaultOptimizer, and the whole point of giving the constraint a typed session
// (rather than an untyped counter map) is that these two cadences stay visibly
// distinct — merging them is the exact bug the 2026-07-03 fix closed.
//
//	ctrl      — the ceiling CONTROLLER's state (ports exportGuard,
//	            optimizer.go:13-22). Resets whenever the cap VALUE changes: a new
//	            cap is a new controller session, so its filter/ratchet/slew state
//	            must start fresh.
//	overTicks — the measured-effect COMPLIANCE counter (ports expOverTicks,
//	            optimizer.go:132-143). Resets ONLY when the cap clears to NaN. A
//	            rewritten cap value is a new controller session but the SAME
//	            compliance obligation, so this counter deliberately survives every
//	            ctrl reset — control-churn rewrites the cap every ~12 s and a
//	            counter on the controller's reset cadence could never fire for the
//	            fault it exists to catch (optimizer.go:133-143, mutation-tested by
//	            TestExportConstraint_ChurnEscalatesCannotComply here and
//	            TestOptimizer_ExportChurnEscalatesCannotComply upstream).
type ExportSession struct {
	ctrl      exportController
	overTicks int // ports expOverTicks (optimizer.go:143); compliance-cadence reset
}

// exportController ports exportGuard (optimizer.go:13-23) field-for-field. Every
// field here resets on a cap-value change (newSession); none of it survives into
// a new controller session.
type exportController struct {
	evSetpointA     float64 // optimizer.go:15 — last EV current issued; NaN until first command
	evCmdW          float64 // optimizer.go:16 — last EV power commanded (A×V at command); NaN = none
	batteryAbsorbW  float64 // optimizer.go:17 — last battery absorption commanded (+W); NaN = none
	safeCount       int     // optimizer.go:18 — consecutive ticks actual export ≤ conservative target
	activeLimitW    float64 // optimizer.go:19 — cap value when the controller was reset; NaN = no active cap
	filteredExportW float64 // optimizer.go:20 — low-pass-filtered actual export
	solarCeilingW   float64 // optimizer.go:21 — sticky generation ceiling commanded; NaN = uncurtailed
	battStallTicks  int     // optimizer.go:22 — consecutive ticks battery commanded to absorb but didn't
}

// newExportSession returns a session with the controller in the all-NaN "no
// active cap" state NewDefaultOptimizer builds (optimizer.go:230-237) and the
// compliance counter at zero.
func newExportSession() ExportSession {
	return ExportSession{ctrl: freshController(math.NaN())}
}

// freshController is the exportGuard reset value: all controller state cleared,
// activeLimitW set to the cap that opened this controller session (NaN when the
// cap has cleared). Mirrors optimizer.go:656 (clear) and :662 (new-value).
func freshController(activeLimitW float64) exportController {
	return exportController{
		evSetpointA:     math.NaN(),
		evCmdW:          math.NaN(),
		batteryAbsorbW:  math.NaN(),
		activeLimitW:    activeLimitW,
		filteredExportW: math.NaN(),
		solarCeilingW:   math.NaN(),
	}
}

// resetControllerForNewLimit starts a fresh controller session for a changed cap
// value WITHOUT touching overTicks — the load-bearing separation. Ports
// optimizer.go:661-663.
func (s *ExportSession) resetControllerForNewLimit(limitW float64) {
	s.ctrl = freshController(limitW)
}

// clearForNoLimit is the cap-cleared reset: BOTH domains reset because the whole
// compliance session is over. Ports optimizer.go:656 (guard) +
// checkExportLimitConvergence :1196 (overTicks). This is the ONLY place
// overTicks is zeroed — see the type comment.
func (s *ExportSession) clearForNoLimit() {
	s.ctrl = freshController(math.NaN())
	s.overTicks = 0
}
