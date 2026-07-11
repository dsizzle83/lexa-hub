package main

// throttle.go implements the advertising brute-force throttle (GAP-3,
// ADR-0002 §Advertising): "After 3 failed PoP handshakes: stop advertising for
// 5 minutes." PoP entropy is the real defence; this only blunts an attacker who
// is present at the radio and grinding setup codes.
//
// Decoupling: the sec1 peripheral stays PURE — it merely counts PopFailures and
// fires the gatt Observer's OnPopFailure edge per wrong-PoP confirm. This
// throttle lives entirely in cmd/provision (the wiring layer): it consumes those
// edges and exposes a single predicate, Allow(), that the advertising gate ANDs
// with the marker/window decision. The peripheral never learns advertising
// exists; the Advertiser never learns PoP exists. The reconcile ticker in
// main() re-evaluates the composed gate every interval, so entering and leaving
// backoff both take effect within one tick — no direct call from the failure
// path into the radio.
//
// State machine (fake-clock tested): failures accrue until they reach the
// threshold, which opens a fixed backoff window; Allow() is false for the whole
// window; the first Allow() at or after expiry resets the counter and resumes.
// Additional failures observed WHILE backing off are ignored (the window is
// already open — re-arming it on every extra grind attempt would let an attacker
// extend their own lockout indefinitely, which is harmless but pointless).

import (
	"sync"
	"time"
)

const (
	// popThrottleThreshold is the number of failed PoP handshakes that trips
	// the backoff (ADR-0002: 3).
	popThrottleThreshold = 3
	// popThrottleBackoff is how long advertising stays off after the threshold
	// trips (ADR-0002: 5 minutes).
	popThrottleBackoff = 5 * time.Minute
)

// popThrottle is the advertising brute-force throttle state machine. The zero
// value is not usable; construct with newPopThrottle. Safe for concurrent use:
// OnPopFailure fires from the D-Bus/dispatch goroutine while Allow is read from
// the reconcile ticker goroutine.
type popThrottle struct {
	threshold int
	backoff   time.Duration
	now       func() time.Time

	mu           sync.Mutex
	failures     int
	backoffUntil time.Time // zero ⇒ not currently backing off
}

// newPopThrottle builds a throttle with the ADR-0002 thresholds. now is
// injectable for tests; nil uses time.Now.
func newPopThrottle(now func() time.Time) *popThrottle {
	if now == nil {
		now = time.Now
	}
	return &popThrottle{
		threshold: popThrottleThreshold,
		backoff:   popThrottleBackoff,
		now:       now,
	}
}

// OnPopFailure records one wrong-PoP confirm. Reaching the threshold opens the
// backoff window. Failures during an already-open window are ignored. This is
// the callback wired into gatt.Observer.OnPopFailure.
func (t *popThrottle) OnPopFailure() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.backoffUntil.IsZero() {
		return // already backing off
	}
	t.failures++
	if t.failures >= t.threshold {
		t.backoffUntil = t.now().Add(t.backoff)
	}
}

// Allow reports whether the throttle currently permits advertising: true unless
// inside an open backoff window. The first call at or after the window's expiry
// clears the window and resets the failure count, so advertising resumes on the
// next reconcile.
func (t *popThrottle) Allow() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.backoffUntil.IsZero() {
		return true
	}
	if t.now().Before(t.backoffUntil) {
		return false
	}
	// Window expired: reset and resume.
	t.backoffUntil = time.Time{}
	t.failures = 0
	return true
}

// backingOff reports whether a backoff window is currently open (diagnostics /
// tests only). It does NOT mutate state, so unlike Allow it will not clear an
// already-expired window.
func (t *popThrottle) backingOff() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.backoffUntil.IsZero() && t.now().Before(t.backoffUntil)
}
