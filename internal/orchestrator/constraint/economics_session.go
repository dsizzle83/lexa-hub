package constraint

import "math"

// EconomicsSession is the EconomicsConstraint's typed inter-tick state.
//
// The economic rules are almost entirely per-tick pure functions of state
// (fixed-dispatch, plan-following, self-consumption, TOU) — the ONE cross-tick
// signal they need is the EV import cooldown. In the cascade that counter
// (impGuard.evSafeCount) is import-session-owned (TASK-061), incremented in
// applyImportLimitRule and READ by applyEVChargingRule to gate EV resumption for
// N compliant ticks after an import-limit event (audit: battery-empty-import-cap;
// a HARD-preserve invariant, TASK-063 "Things that must NOT change").
//
// A below-compliance economics layer cannot reach into the import constraint's
// session, so this session keeps an economics-LOCAL copy of the counter, advanced
// each tick from the effective import limit and measured grid net power exactly as
// the legacy import rule advances it. Folding the two copies into one owner is a
// TASK-064 item (the shared-state seam the compliance interleaving exposes); until
// then the import constraint's copy is vestigial and this one drives the gate.
type EconomicsSession struct {
	// evSafeCount is the count of consecutive ticks the site has imported under the
	// active import limit — the EV-resume cooldown. Ports impGuard.evSafeCount
	// (optimizer.go:2001-2009).
	evSafeCount int
	// activeImportLimitW tracks the import cap this cooldown session belongs to, so
	// a NEW/meaningfully-changed cap starts a fresh cooldown (NaN = no active cap).
	activeImportLimitW float64
}

// newEconomicsSession returns an empty session in the no-active-import-cap state.
func newEconomicsSession() EconomicsSession {
	return EconomicsSession{activeImportLimitW: math.NaN()}
}

// updateEVCooldown advances the EV-resume cooldown for this tick. It mirrors the
// import rule's counter lifecycle (optimizer.go:1930-1932 clear, ImportSession
// manageSession seed-on-arrival, and the :2001-2009 compliant-tick increment):
//
//   - No import cap active            → reset the counter and clear the session.
//   - Cap newly arrived / meaningfully changed → fresh cooldown, SEEDED satisfied
//     when the site is already importing under the cap (the cooldown exists for
//     post-violation recovery, not cap arrival).
//   - Same cap, importing under it    → increment (a compliant tick).
//   - Same cap, over it or exporting  → reset to 0 (restart the cooldown).
func (s *EconomicsSession) updateEVCooldown(importLimitW, netW float64, cooldownCycles int) {
	if math.IsNaN(importLimitW) {
		s.evSafeCount = 0
		s.activeImportLimitW = math.NaN()
		return
	}
	compliant := !math.IsNaN(netW) && netW >= 0 && netW <= importLimitW
	if math.IsNaN(s.activeImportLimitW) || math.Abs(importLimitW-s.activeImportLimitW) > exportComplianceBreachW {
		s.activeImportLimitW = importLimitW
		if compliant {
			s.evSafeCount = cooldownCycles // seed satisfied — do not suspend on arrival
		} else {
			s.evSafeCount = 0
		}
		return
	}
	s.activeImportLimitW = importLimitW
	if compliant {
		s.evSafeCount++
	} else {
		s.evSafeCount = 0
	}
}
