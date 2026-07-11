package main

import (
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually-advanced clock for the throttle/window state
// machines (no real sleeping in tests).
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(start time.Time) *fakeClock { return &fakeClock{t: start} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestPopThrottle_ThreeFailuresOpenBackoffThenResume(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	th := newPopThrottle(clk.now)

	// Fresh: advertising allowed.
	if !th.Allow() {
		t.Fatal("fresh throttle must allow advertising")
	}

	// Two failures: still under the threshold, still allowed.
	th.OnPopFailure()
	th.OnPopFailure()
	if !th.Allow() {
		t.Fatalf("2 failures (< %d) must still allow", popThrottleThreshold)
	}
	if th.backingOff() {
		t.Fatal("must not be backing off before the threshold")
	}

	// Third failure trips the backoff.
	th.OnPopFailure()
	if th.Allow() {
		t.Fatalf("%d failures must open the backoff window (Allow=false)", popThrottleThreshold)
	}
	if !th.backingOff() {
		t.Fatal("must report backing off at the threshold")
	}

	// Still inside the window one second before expiry.
	clk.advance(popThrottleBackoff - time.Second)
	if th.Allow() {
		t.Fatal("still inside backoff window must not allow")
	}

	// At/after expiry: resume, and the counter resets so it takes a fresh 3
	// failures to trip again.
	clk.advance(time.Second)
	if !th.Allow() {
		t.Fatal("at expiry the throttle must resume advertising")
	}
	if th.backingOff() {
		t.Fatal("window must be cleared after expiry")
	}
	th.OnPopFailure()
	th.OnPopFailure()
	if !th.Allow() {
		t.Fatal("counter must have reset — 2 fresh failures must not re-trip")
	}
	th.OnPopFailure()
	if th.Allow() {
		t.Fatal("a fresh 3rd failure must re-open the backoff")
	}
}

// TestPopThrottle_FailuresDuringBackoffDoNotExtend verifies extra grind
// attempts while already backing off do not re-arm (and thus cannot extend) the
// window.
func TestPopThrottle_FailuresDuringBackoffDoNotExtend(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	th := newPopThrottle(clk.now)

	for i := 0; i < popThrottleThreshold; i++ {
		th.OnPopFailure()
	}
	if th.Allow() {
		t.Fatal("threshold must open backoff")
	}
	// Grind several more failures partway through the window.
	clk.advance(popThrottleBackoff / 2)
	for i := 0; i < 10; i++ {
		th.OnPopFailure()
	}
	// The original window still expires on its original schedule.
	clk.advance(popThrottleBackoff/2 + time.Nanosecond)
	if !th.Allow() {
		t.Fatal("extra failures during backoff must not extend the original window")
	}
}

func TestPopThrottle_NilClockUsesRealTime(t *testing.T) {
	th := newPopThrottle(nil)
	if !th.Allow() {
		t.Fatal("nil clock throttle must default to time.Now and allow when fresh")
	}
}
