package constraint

import "math"

// ImportSession is the ImportLimitConstraint's typed inter-tick state. It ports
// importGuard (optimizer.go:32-38) field-for-field.
//
// Like GenSession — and UNLIKE ExportSession — import keeps a SINGLE reset domain.
// The legacy import path resets the WHOLE guard (including breachTicks) on a
// meaningful cap change: applyImportLimitRule replaces the struct wholesale
// (optimizer.go:1944) and checkImportConvergence only ever touches breachTicks
// (:1401-1440), so a cap-VALUE change zeroes every field together. There is no
// export-style expOverTicks churn-survival split — the import counter deliberately
// resets on a meaningful new cap, and TestApplyImportLimitRule_CapDriftKeepsGuardSession
// pins that it survives only sub-threshold DRIFT.
//
//	dischargeW   — last battery discharge commanded (+W); NaN = none. Anti-oscillation
//	               stickiness: held across ticks and ramped down only after safeCount
//	               (optimizer.go:33).
//	safeCount    — consecutive ticks importW ≤ hard limit; the battery ramp-down gate
//	               (optimizer.go:34).
//	evSafeCount  — consecutive ticks 0 ≤ importW ≤ hard limit; the EV resume gate
//	               (optimizer.go:35). Tracked for fidelity; EV emission is DEFERRED
//	               (see ImportLimitConstraint doc), so it does not affect emitted
//	               demands, but it must track exactly so the flip inherits correct state.
//	activeLimitW — limit value when the guard was reset; NaN = no active limit
//	               (optimizer.go:36). Follows sub-threshold decode drift within a session.
//	breachTicks  — consecutive ticks measured import stayed over the cap; the leaky
//	               convergence counter with NaN-HOLD semantics (optimizer.go:37).
type ImportSession struct {
	dischargeW   float64
	safeCount    int
	evSafeCount  int
	activeLimitW float64
	breachTicks  int
}

// newImportSession returns the no-active-limit state NewDefaultOptimizer builds
// (optimizer.go:238-241: importGuard{dischargeW: NaN, activeLimitW: NaN}).
func newImportSession() ImportSession {
	return ImportSession{dischargeW: math.NaN(), activeLimitW: math.NaN()}
}

// clearForNoLimit is the cap-cleared reset: every field zeroes because the whole
// compliance session is over. Ports optimizer.go:1931 (applyImportLimitRule guard
// reset) + :1403 (checkImportConvergence breachTicks reset).
func (s *ImportSession) clearForNoLimit() {
	*s = newImportSession()
}

// resetForNewLimit starts a fresh guard session for a meaningfully changed cap
// value — dischargeW, safeCount, evSafeCount, breachTicks all zero. Ports the
// wholesale struct replacement at optimizer.go:1944.
func (s *ImportSession) resetForNewLimit(limitW float64) {
	*s = ImportSession{dischargeW: math.NaN(), activeLimitW: limitW}
}
