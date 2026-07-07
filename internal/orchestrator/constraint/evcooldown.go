package constraint

import "math"

// EVImportCooldown is the SINGLE owner of the EV-resume cooldown counter that gates
// EV charging resumption for N compliant ticks after an import-limit event
// (audit: battery-empty-import-cap — a HARD-preserve invariant).
//
// In the legacy cascade this was ONE field, impGuard.evSafeCount: written by
// applyImportLimitRule (the import path) and read by applyEVChargingRule (the EV
// path) — one counter, one update site, read downstream (optimizer.go:2001-2009,
// 1762-1774). TASK-061 split those into separate tiers and, lacking a shared owner,
// briefly kept TWO copies: the ImportLimitConstraint's (bit-faithful to legacy) and
// an economics-LOCAL copy whose seed/increment ordering could differ by a tick on
// cap arrival — legacy seeds THEN increments the same tick (seed+1 on a compliant
// arrival), the economics copy only seeded. The TASK-063 seam review §3 flagged
// that as the bounded on-cap divergence for TASK-064 to close.
//
// TASK-064 folds the two copies into this one object: the ImportLimitConstraint
// (compliance — the legacy WRITER's tier) is the sole writer; the
// EconomicsConstraint (the legacy EV rule's tier) is a READER. One counter, one
// update path, zero cross-copy divergence. The Stack builder (cmd/hub, the
// full-stack tests) constructs one EVImportCooldown and hands the SAME pointer to
// both constraints, so import runs first each tick (compliance before economics)
// and economics reads the value import just wrote — exactly the legacy ordering.
type EVImportCooldown struct {
	// safeCount is the count of consecutive ticks the site imported under the active
	// cap (the EV-resume cooldown). Ports impGuard.evSafeCount.
	safeCount int
	// activeLimitW is the import cap this cooldown session belongs to; NaN = no
	// active cap. Tracks the same meaningful-change tolerance band the import
	// session uses so a NEW cap starts a fresh cooldown.
	activeLimitW float64
}

// NewEVImportCooldown returns a cooldown in the no-active-cap state. Exported so
// cmd/hub can mint the shared instance both constraints receive.
func NewEVImportCooldown() *EVImportCooldown {
	return &EVImportCooldown{activeLimitW: math.NaN()}
}

// clear resets when the import cap clears to NaN — the whole compliance session is
// over (optimizer.go:1931 wholesale guard reset).
func (cd *EVImportCooldown) clear() {
	cd.safeCount = 0
	cd.activeLimitW = math.NaN()
}

// arrival seeds the cooldown for a MEANINGFULLY new cap value: satisfied (seeded to
// the cooldown length) when the site is ALREADY compliant under it — the cooldown
// exists for post-violation recovery, not limit arrival — else zero
// (optimizer.go:1943-1954). The import controller detects the cap change and calls
// this; advance() then runs the same tick, matching the legacy seed-then-increment.
func (cd *EVImportCooldown) arrival(importLimitW float64, compliant bool, seed int) {
	cd.activeLimitW = importLimitW
	if compliant {
		cd.safeCount = seed
	} else {
		cd.safeCount = 0
	}
}

// sameCap follows sub-threshold decode drift within an existing cooldown session
// (optimizer.go:1956): the cap value can jitter by a hair through the watts→
// ActivePower round-trip without being a new session.
func (cd *EVImportCooldown) sameCap(importLimitW float64) {
	cd.activeLimitW = importLimitW
}

// advance is the per-tick update the import controller applies after arrival/sameCap
// (optimizer.go:2005-2009): increment on a compliant tick (importing under the cap),
// reset to zero otherwise (over the cap, or exporting during an over-discharge
// settling transient). Applied on the SAME tick as arrival, so a compliant arrival
// lands at seed+1 exactly as legacy does.
func (cd *EVImportCooldown) advance(compliant bool) {
	if compliant {
		cd.safeCount++
	} else {
		cd.safeCount = 0
	}
}

// Suppressed reports whether EV resumption must still be held: the counter has not
// yet reached the cooldown length. The economics EV rule reads this in place of its
// former local copy (optimizer.go:1762 evImportSuppressed gate).
func (cd *EVImportCooldown) Suppressed(cooldownCycles int) bool {
	return cd.safeCount < cooldownCycles
}
