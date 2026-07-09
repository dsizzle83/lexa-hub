package watchdog

import (
	"testing"
	"time"
)

// TestDeafTracker_NeverDisconnectedIsZero covers the default/never-lost
// state: a fresh tracker (and one that has only ever been connected) reports
// DeafFor == 0 regardless of "now".
func TestDeafTracker_NeverDisconnectedIsZero(t *testing.T) {
	tr := NewDeafTracker()
	now := time.Now()

	if got := tr.DeafFor(now); got != 0 {
		t.Fatalf("DeafFor() on a fresh tracker = %s, want 0", got)
	}
	if got := tr.DeafFor(now.Add(time.Hour)); got != 0 {
		t.Fatalf("DeafFor(now+1h) on a never-disconnected tracker = %s, want 0", got)
	}
}

// TestDeafTracker_GrowsAfterConnectionLost covers the core behavior: once
// OnConnectionLost fires, DeafFor grows with the caller-supplied "now".
func TestDeafTracker_GrowsAfterConnectionLost(t *testing.T) {
	tr := NewDeafTracker()
	start := time.Now()
	tr.disconnectedAt = start // simulate OnConnectionLost at a known instant

	if got := tr.DeafFor(start); got != 0 {
		t.Fatalf("DeafFor(start) = %s, want 0 (no time has elapsed yet)", got)
	}
	if got, want := tr.DeafFor(start.Add(30*time.Second)), 30*time.Second; got != want {
		t.Fatalf("DeafFor(start+30s) = %s, want %s", got, want)
	}
	if got, want := tr.DeafFor(start.Add(10*time.Minute)), 10*time.Minute; got != want {
		t.Fatalf("DeafFor(start+10m) = %s, want %s", got, want)
	}
}

// TestDeafTracker_OnConnectionLostThenDeafForViaAPI exercises the real public
// entry point (not the struct field directly), pinning OnConnectionLost's
// observable effect against a fake clock captured before/after the call.
func TestDeafTracker_OnConnectionLostThenDeafForViaAPI(t *testing.T) {
	tr := NewDeafTracker()
	before := time.Now()
	tr.OnConnectionLost()

	later := before.Add(5 * time.Minute)
	got := tr.DeafFor(later)
	// disconnectedAt was set to time.Now() inside OnConnectionLost, which is
	// >= before; DeafFor(later) must therefore be <= 5 minutes (and > 0,
	// barring an implausibly slow test run).
	if got <= 0 || got > 5*time.Minute {
		t.Fatalf("DeafFor(before+5m) = %s, want in (0, 5m]", got)
	}
}

// TestDeafTracker_OnReconnectResetsToZero covers the recovery path: an
// OnReconnect call after OnConnectionLost clears the outage, so DeafFor
// reports 0 again even at a later "now".
func TestDeafTracker_OnReconnectResetsToZero(t *testing.T) {
	tr := NewDeafTracker()
	tr.OnConnectionLost()
	tr.OnReconnect()

	if got := tr.DeafFor(time.Now().Add(time.Hour)); got != 0 {
		t.Fatalf("DeafFor() after OnReconnect = %s, want 0", got)
	}
}

// TestDeafTracker_RepeatedConnectionLostDoesNotAdvanceClock is the
// idempotency requirement called out in the design brief: a SECOND
// OnConnectionLost call without an intervening OnReconnect must not push
// disconnectedAt forward — the outage is measured from the FIRST loss, not
// the most recent one.
func TestDeafTracker_RepeatedConnectionLostDoesNotAdvanceClock(t *testing.T) {
	tr := NewDeafTracker()
	start := time.Now()
	tr.disconnectedAt = start // simulate the FIRST OnConnectionLost at start

	// A later, spurious second OnConnectionLost call (e.g. paho's handler
	// firing again while still disconnected) must be a no-op for the clock.
	tr.OnConnectionLost()

	if got, want := tr.DeafFor(start.Add(2*time.Minute)), 2*time.Minute; got != want {
		t.Fatalf("DeafFor(start+2m) after a repeated OnConnectionLost = %s, want %s (disconnectedAt must not have moved)", got, want)
	}
}
