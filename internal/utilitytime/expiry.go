package utilitytime

// This file holds the two composable expiry policies that TASK-036 will
// substitute for the hub's and API's local, one-off expiry constants. Both
// are pure state machines (DebouncedExpiry carries state; ReportGrace is a
// stateless predicate) driven entirely by serverNow values the caller
// computes via ServerNow/ServerNowAt above — neither reads a clock itself.

// DebouncedExpiry requires an expiry condition to be observed Confirm
// consecutive times before it reports true, riding out a transient forward
// clock jump that would otherwise make a control look momentarily expired.
//
// Generalizes cmd/hub/state.go's expiryConfirmTicks=3 constant and the
// debounce block in ReadSystemState (state.go:348-371): "require the expiry
// to PERSIST for expiryConfirmTicks consecutive ticks before dropping, and
// keep enforcing the control in the meantime (a cap is conservative, so
// holding it across a transient clock jump is the safe choice)."
//
// Zero value: a DebouncedExpiry{} has Confirm==0, which Observe treats as
// Confirm==1 (fire immediately on the first true) so a caller that forgets
// to set Confirm gets a safe, non-panicking default rather than a policy
// that can never fire.
type DebouncedExpiry struct {
	// Confirm is how many consecutive Observe(true) calls are required
	// before Observe reports expired. Confirm<=0 is treated as 1.
	Confirm int

	count int // unexported: consecutive-true counter, reset on any false
}

// Observe records one tick's expiry observation and reports whether the
// condition has now persisted for Confirm consecutive observations. Any
// Observe(false) resets the counter to zero — a single tick back under the
// threshold (e.g. the clock settling back before ValidUntil) rides out the
// whole excursion, matching state.go's "else { r.csipExpiredTicks = 0 }".
func (d *DebouncedExpiry) Observe(expired bool) bool {
	if !expired {
		d.count = 0
		return false
	}
	d.count++
	confirm := d.Confirm
	if confirm <= 0 {
		confirm = 1
	}
	if d.count >= confirm {
		return true
	}
	return false
}

// Reset clears the consecutive-true counter, e.g. when the caller has acted
// on expiry and wants a fresh debounce window for whatever control replaces
// the expired one.
func (d *DebouncedExpiry) Reset() {
	d.count = 0
}

// ReportGrace is a pure predicate for reporting surfaces: a control stays
// reportable until GraceS seconds after its ValidUntil, in server time.
//
// Generalizes cmd/api/handlers.go's csipReportGraceS=15 constant and its use
// at handlers.go:145-146: "the hub debounces with expiryConfirmTicks before
// RELEASING; for pure reporting a small fixed grace is enough and needs no
// tick cadence." Unlike DebouncedExpiry, ReportGrace carries no state across
// calls — every reporting tick is judged independently against the same
// validUntil, so there is nothing to reset.
type ReportGrace struct {
	// GraceS is how many seconds past ValidUntil (server time) the control
	// remains reportable.
	GraceS int64
}

// Reportable reports whether a control with the given validUntil (0 meaning
// "never expires", e.g. a DefaultDERControl) is still reportable at
// serverNow: validUntil==0, or serverNow < validUntil+GraceS.
func (g ReportGrace) Reportable(validUntil, serverNow int64) bool {
	if validUntil == 0 {
		return true
	}
	return serverNow < validUntil+g.GraceS
}
