package constraint

import "math"

// GenSession is the GenLimitConstraint's typed inter-tick state. It ports
// genGuard (optimizer.go:48-51) field-for-field.
//
// Unlike ExportSession, gen keeps a SINGLE reset domain: the legacy generation
// path has no separate controller (applyGenLimitRule is stateless — it re-derives
// the absolute per-inverter ceiling from the cap every tick), so the only carried
// state is the convergence guard, and checkGenLimitConvergence resets activeLimitW
// AND overCount TOGETHER on a meaningful cap change (optimizer.go:1310-1314). There
// is deliberately no export-style two-cadence split here — the gen cap has no
// churn-survival requirement, and inventing one would diverge from legacy.
//
//	activeLimitW — cap value when the guard was reset; NaN = no active cap
//	               (optimizer.go:49). Tracks sub-threshold decode drift within a
//	               session (the watts→ActivePower×10^mult bus round-trip wobble).
//	overCount    — consecutive ticks MEASURED generation stayed over the cap after
//	               curtailment was commanded (optimizer.go:50); leaky counter.
type GenSession struct {
	activeLimitW float64
	overCount    int
}

// newGenSession returns the no-active-cap state NewDefaultOptimizer builds
// (optimizer.go:242: genGuard{activeLimitW: NaN}).
func newGenSession() GenSession {
	return GenSession{activeLimitW: math.NaN()}
}

// clearForNoLimit is the cap-cleared reset: both fields zero because the whole
// compliance session is over. Ports optimizer.go:1301
// (genGuard{activeLimitW: NaN}).
func (s *GenSession) clearForNoLimit() {
	s.activeLimitW = math.NaN()
	s.overCount = 0
}

// resetForNewLimit starts a fresh convergence session for a meaningfully changed
// cap value: overCount zeroes. Ports optimizer.go:1311
// (genGuard{activeLimitW: maxLimitW}).
func (s *GenSession) resetForNewLimit(limitW float64) {
	s.activeLimitW = limitW
	s.overCount = 0
}
