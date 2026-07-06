package constraint

import (
	"testing"
	"time"
)

// TestScaleTicks_ParityWithOptimizer pins the copied semantics of
// DefaultOptimizer.scaleTicks (optimizer.go): round-to-nearest with a floor of
// 2, no-op at the tuned cadence or when unset. The expected values are computed
// from the identical algorithm; if optimizer.go's scaleTicks ever changes, this
// table must change with it (FAST/STOCK equivalence of every migrated breach
// threshold depends on the two staying identical — TASK-064).
func TestScaleTicks_ParityWithOptimizer(t *testing.T) {
	cases := []struct {
		name     string
		interval time.Duration
		ticks    int
		want     int
	}{
		// Unset cadence: identity (unit-test path).
		{"unset-3", 0, 3, 3},
		{"unset-20", 0, 20, 20},
		// Tuned cadence (FAST 3 s): identity.
		{"tuned-3", 3 * time.Second, 3, 3},
		{"tuned-5", 3 * time.Second, 5, 5},
		// STOCK 15 s: hold=ticks*3 s, round(hold/15), floor 2.
		{"stock-3", 15 * time.Second, 3, 2},   // 9/15→1→floor 2
		{"stock-5", 15 * time.Second, 5, 2},   // 15/15→1→floor 2
		{"stock-10", 15 * time.Second, 10, 2}, // 30/15→2
		{"stock-20", 15 * time.Second, 20, 4}, // 60/15→4 (EV cooldown ~1 min)
		// 4.5 s (state.go's worked example).
		{"mid-3", 4500 * time.Millisecond, 3, 2},    // 9/4.5→2
		{"mid-5", 4500 * time.Millisecond, 5, 3},    // 15/4.5→3.33→3
		{"mid-20", 4500 * time.Millisecond, 20, 13}, // 60/4.5→13.33→13
		// 6 s: exercises round-half-away-from-zero (2.5→3).
		{"six-5", 6 * time.Second, 5, 3},    // 15/6→2.5→3
		{"six-20", 6 * time.Second, 20, 10}, // 60/6→10
	}
	for _, tc := range cases {
		s := NewSession("t", tc.interval)
		if got := s.ScaleTicks(tc.ticks); got != tc.want {
			t.Errorf("%s: ScaleTicks(%d)@%v=%d want %d", tc.name, tc.ticks, tc.interval, got, tc.want)
		}
	}
}

func TestSessionTickSeconds(t *testing.T) {
	if s := NewSession("t", 0); s.TickSeconds() != 3.0 {
		t.Errorf("unset TickSeconds=%v want 3", s.TickSeconds())
	}
	if s := NewSession("t", 15*time.Second); s.TickSeconds() != 15.0 {
		t.Errorf("stock TickSeconds=%v want 15", s.TickSeconds())
	}
}

func TestSessionResetHooks(t *testing.T) {
	s := NewSession("t", 0)
	a, b := 0, 0
	s.OnReset(func() { a++ })
	s.OnReset(func() { b += 10 })
	s.OnReset(nil) // ignored
	s.Reset()
	s.Reset()
	if a != 2 || b != 20 {
		t.Errorf("reset hooks ran a=%d b=%d want 2/20", a, b)
	}
	if s.Name() != "t" {
		t.Errorf("Name=%q want t", s.Name())
	}
}
