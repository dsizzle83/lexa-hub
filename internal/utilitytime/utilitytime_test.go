package utilitytime

import (
	"testing"
	"time"
)

// fakeNow returns an injectable Config.Now that always reports t, letting
// tests advance it explicitly between calls.
func fakeNow(t *time.Time) func() time.Time {
	return func() time.Time { return *t }
}

func TestServerNowAt(t *testing.T) {
	// Table demonstrates ServerNowAt(now, off) == now.Unix()+off for a range
	// of offsets including the CLAUDE.md formula (serverNow = time.Now().Unix()
	// + tree.ClockOffset) — positive, negative, and zero offsets.
	base := time.Unix(1_800_000_000, 0).UTC()
	tests := []struct {
		name string
		now  time.Time
		off  int64
	}{
		{"zero offset", base, 0},
		{"positive offset", base, 42},
		{"negative offset", base, -42},
		{"large positive offset", base, 3600 * 24},
		{"large negative offset", base, -3600 * 24},
		{"epoch", time.Unix(0, 0).UTC(), 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := tt.now.Unix() + tt.off
			if got := ServerNowAt(tt.now, tt.off); got != want {
				t.Errorf("ServerNowAt(%v, %d) = %d, want %d", tt.now, tt.off, got, want)
			}
		})
	}
}

func TestClock_ServerNow_NoOffsetYet(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := New(Config{Now: fakeNow(&now)})
	// Before any SetOffset, offset is treated as 0 — ServerNow degrades to
	// the local clock, matching every existing consumer's zero-value
	// ClockOffset before the first successful /tm resync.
	if got, want := c.ServerNow(), now.Unix(); got != want {
		t.Errorf("ServerNow() before any offset = %d, want %d (local time)", got, want)
	}
	if off, have := c.Offset(); have || off != 0 {
		t.Errorf("Offset() before SetOffset = (%d, %v), want (0, false)", off, have)
	}
}

func TestClock_ServerNow_ArithmeticIncludesNegativeOffset(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := New(Config{Now: fakeNow(&now)})

	tests := []struct {
		name   string
		offset int64
	}{
		{"positive", 30},
		{"negative", -30},
		{"zero", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c.SetOffset(tt.offset)
			want := now.Unix() + tt.offset
			if got := c.ServerNow(); got != want {
				t.Errorf("ServerNow() after SetOffset(%d) = %d, want %d", tt.offset, got, want)
			}
			if off, have := c.Offset(); !have || off != tt.offset {
				t.Errorf("Offset() = (%d, %v), want (%d, true)", off, have, tt.offset)
			}
		})
	}
}

func TestClock_ServerNow_AdvancesWithInjectedClock(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := New(Config{Now: fakeNow(&now)})
	c.SetOffset(10)

	if got, want := c.ServerNow(), now.Unix()+10; got != want {
		t.Fatalf("ServerNow() = %d, want %d", got, want)
	}

	// Advance the injected clock without touching the offset: ServerNow
	// must track the local clock, never a wall-clock read of its own.
	now = now.Add(5 * time.Minute)
	if got, want := c.ServerNow(), now.Unix()+10; got != want {
		t.Fatalf("ServerNow() after clock advance = %d, want %d", got, want)
	}
}

func TestClock_SetOffset_Classification(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)

	t.Run("first offset is always First", func(t *testing.T) {
		c := New(Config{Now: fakeNow(&now), WobbleMaxS: 60})
		if got := c.SetOffset(5); got != First {
			t.Errorf("SetOffset on empty Clock = %v, want First", got)
		}
	})

	t.Run("wobble at the WobbleMaxS boundary (inclusive)", func(t *testing.T) {
		c := New(Config{Now: fakeNow(&now), WobbleMaxS: 60})
		c.SetOffset(0)
		if got := c.SetOffset(60); got != Wobble {
			t.Errorf("SetOffset(delta=60, WobbleMaxS=60) = %v, want Wobble", got)
		}
	})

	t.Run("step just past the WobbleMaxS boundary", func(t *testing.T) {
		c := New(Config{Now: fakeNow(&now), WobbleMaxS: 60})
		c.SetOffset(0)
		if got := c.SetOffset(61); got != Step {
			t.Errorf("SetOffset(delta=61, WobbleMaxS=60) = %v, want Step", got)
		}
	})

	t.Run("repeated steps each classify against the previous accepted offset", func(t *testing.T) {
		c := New(Config{Now: fakeNow(&now), WobbleMaxS: 60})
		c.SetOffset(0)
		if got := c.SetOffset(100); got != Step {
			t.Fatalf("first step = %v, want Step", got)
		}
		// Another step of the same magnitude from the NEW baseline (100) is
		// still a Step — classification always compares against the last
		// accepted offset, not the original.
		if got := c.SetOffset(200); got != Step {
			t.Errorf("second consecutive step = %v, want Step", got)
		}
	})

	t.Run("step back (negative delta) classifies on magnitude", func(t *testing.T) {
		c := New(Config{Now: fakeNow(&now), WobbleMaxS: 60})
		c.SetOffset(500)
		if got := c.SetOffset(0); got != Step {
			t.Errorf("SetOffset stepping back by 500 = %v, want Step", got)
		}
		off, _ := c.Offset()
		if off != 0 {
			t.Errorf("Offset() after step back = %d, want 0 (raw value always accepted)", off)
		}
	})

	t.Run("wobble back within threshold", func(t *testing.T) {
		c := New(Config{Now: fakeNow(&now), WobbleMaxS: 60})
		c.SetOffset(30)
		if got := c.SetOffset(0); got != Wobble {
			t.Errorf("SetOffset stepping back by 30 (<=60) = %v, want Wobble", got)
		}
	})

	t.Run("classification never alters ServerNow — raw offset always rules", func(t *testing.T) {
		c := New(Config{Now: fakeNow(&now), WobbleMaxS: 60})
		c.SetOffset(0)
		class := c.SetOffset(10_000) // an enormous step
		if class != Step {
			t.Fatalf("expected Step classification, got %v", class)
		}
		// Even though 10_000 is classified as a dramatic step, ServerNow
		// must reflect it immediately and exactly — no smoothing/filtering.
		if got, want := c.ServerNow(), now.Unix()+10_000; got != want {
			t.Errorf("ServerNow() after a Step = %d, want %d (raw, unsmoothed)", got, want)
		}
	})
}

func TestDefaultWobbleMaxS(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := New(Config{Now: fakeNow(&now)}) // WobbleMaxS unset -> defaults to 60
	c.SetOffset(0)
	if got := c.SetOffset(DefaultWobbleMaxS); got != Wobble {
		t.Errorf("default WobbleMaxS: delta=%d classified %v, want Wobble", DefaultWobbleMaxS, got)
	}
	c2 := New(Config{Now: fakeNow(&now)})
	c2.SetOffset(0)
	if got := c2.SetOffset(DefaultWobbleMaxS + 1); got != Step {
		t.Errorf("default WobbleMaxS: delta=%d classified %v, want Step", DefaultWobbleMaxS+1, got)
	}
}

func TestNew_ZeroConfigUsesRealClock(t *testing.T) {
	// New(Config{}) must not panic and must default Now to time.Now (the
	// only place this package is allowed a real wall-clock read).
	c := New(Config{})
	before := time.Now().Unix()
	got := c.ServerNow()
	after := time.Now().Unix()
	if got < before || got > after {
		t.Errorf("ServerNow() with default clock = %d, want within [%d, %d]", got, before, after)
	}
}

func TestStepClass_String(t *testing.T) {
	tests := []struct {
		class StepClass
		want  string
	}{
		{First, "first"},
		{Wobble, "wobble"},
		{Step, "step"},
		{StepClass(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.class.String(); got != tt.want {
			t.Errorf("StepClass(%d).String() = %q, want %q", tt.class, got, tt.want)
		}
	}
}

func TestExpired(t *testing.T) {
	tests := []struct {
		name       string
		validUntil int64
		serverNow  int64
		want       bool
	}{
		{"ValidUntil 0 never expires, even far in the future", 0, 1_000_000_000, false},
		{"ValidUntil 0 never expires, at serverNow 0", 0, 0, false},
		{"before ValidUntil: not expired", 1000, 999, false},
		{"boundary serverNow == ValidUntil: expired (>=)", 1000, 1000, true},
		{"past ValidUntil: expired", 1000, 1001, true},
		{"negative ValidUntil, serverNow crosses it: expired", -5, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Expired(tt.validUntil, tt.serverNow); got != tt.want {
				t.Errorf("Expired(%d, %d) = %v, want %v", tt.validUntil, tt.serverNow, got, tt.want)
			}
		})
	}
}

// --- TASK-037: monotonic anchoring + local clock-step detection ---
//
// Testability note (see the Config.Now doc comment): Go's time.Time only
// carries a real monotonic reading when it originates from time.Now(), and
// Time.Add shifts the wall and monotonic components by the exact same
// duration — there is no public API to desynchronize them. That is
// deliberate: in production, cfg.Now is real time.Now, whose monotonic
// component is a hardware guarantee (CLOCK_MONOTONIC) that is immune to
// wall-clock steps (NTP corrections, settimeofday) by construction — nothing
// in this package needs to reproduce that guarantee, only to correctly rely
// on it (i.e. never read the wall clock again after Anchor except via
// cfg.Now().Sub(anchorMono)). The tests below therefore prove two separate
// things instead of trying to fake an OS-level clock step:
//  1. Anchored ServerNow is purely elapsed-time-based (tracks a base+Add()
//     fake clock exactly, and — unlike the pre-anchor formula — completely
//     ignores SetOffset/wall-Unix reads once anchored).
//  2. LocalStep's drift arithmetic is correct given wall-vs-monotonic
//     elapsed inputs, exercised directly against the (wallElapsed,
//     monoElapsed) formula using deliberately constructed field values,
//     since real desync can only originate from an actual OS clock step.

func TestClock_Anchor_ServerNowTracksElapsed(t *testing.T) {
	base := time.Now() // must be time.Now()-derived to carry a monotonic reading
	now := base
	c := New(Config{Now: func() time.Time { return now }})

	c.Anchor(5_000)
	if !c.Anchored() {
		t.Fatal("Anchored() = false immediately after Anchor")
	}
	if got := c.ServerNow(); got != 5_000 {
		t.Fatalf("ServerNow() immediately after Anchor(5000) = %d, want 5000", got)
	}

	tests := []struct {
		name    string
		advance time.Duration
	}{
		{"advance 1s", 1 * time.Second},
		{"advance 1 more minute", time.Minute},
		{"advance 2 more hours", 2 * time.Hour},
	}
	elapsed := time.Duration(0)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			elapsed += tt.advance
			now = base.Add(elapsed)
			want := int64(5_000) + int64(elapsed.Seconds())
			if got := c.ServerNow(); got != want {
				t.Errorf("ServerNow() after %v elapsed = %d, want %d", elapsed, got, want)
			}
		})
	}
}

func TestClock_Anchor_IgnoresSetOffsetOnceAnchored(t *testing.T) {
	// Once anchored, ServerNow must never fall back to the raw offset path —
	// this is the property that makes it immune to whatever drove a wall
	// step (an NTP correction would, pre-TASK-037, have to be chased via a
	// new SetOffset/offset read; post-anchoring it is irrelevant).
	base := time.Now()
	now := base
	c := New(Config{Now: func() time.Time { return now }})

	c.SetOffset(30) // some pre-existing offset from server-time sync
	c.Anchor(1_000_000)
	now = base.Add(10 * time.Second)

	if got, want := c.ServerNow(), int64(1_000_010); got != want {
		t.Fatalf("ServerNow() = %d, want %d (anchored)", got, want)
	}

	// A dramatic SetOffset after anchoring (as if a fresh but wildly
	// different server sync arrived) must not move ServerNow — only a fresh
	// Anchor call re-anchors.
	c.SetOffset(999_999)
	if got, want := c.ServerNow(), int64(1_000_010); got != want {
		t.Errorf("ServerNow() after post-anchor SetOffset = %d, want %d (unaffected)", got, want)
	}
}

func TestClock_Anchor_ForwardLocalStepDoesNotMoveServerNow(t *testing.T) {
	// Simulates GAP-04's forward NTP-step scenario: the anchored Clock is fed
	// the TRUE elapsed time (5s — what really happened), while a parallel
	// "legacy" computation using ServerNowAt against a wall-stepped `now`
	// (simulating what the wall clock would read after a +1h forward step)
	// shows what the pre-TASK-037 code would have produced. The anchored
	// value must match ground truth; the legacy one is off by the step.
	base := time.Now()
	now := base
	c := New(Config{Now: func() time.Time { return now }})
	const offset = int64(20) // some server offset in effect at anchor time
	c.SetOffset(offset)
	c.Anchor(ServerNowAt(now, offset)) // anchor at server-time truth

	trueElapsed := 5 * time.Second
	now = base.Add(trueElapsed)
	anchoredServerNow := c.ServerNow()
	wantTruth := ServerNowAt(base, offset) + int64(trueElapsed.Seconds())
	if anchoredServerNow != wantTruth {
		t.Fatalf("anchored ServerNow() = %d, want %d (ground truth)", anchoredServerNow, wantTruth)
	}

	// What the OLD (unanchored) formula would have computed had the wall
	// clock stepped forward 1h during that same 5s of real elapsed time.
	stepped := base.Add(trueElapsed + time.Hour)
	legacyServerNow := ServerNowAt(stepped, offset)
	if legacyServerNow == anchoredServerNow {
		t.Fatal("expected the legacy formula to diverge from the anchored one under a simulated forward step")
	}
	if diff := legacyServerNow - anchoredServerNow; diff != 3600 {
		t.Errorf("legacy-vs-anchored divergence = %ds, want 3600s (the simulated step size)", diff)
	}
}

func TestClock_Anchor_BackwardLocalStepDoesNotMoveServerNow(t *testing.T) {
	// Mirror of the forward case for a −1h backward step.
	base := time.Now()
	now := base
	c := New(Config{Now: func() time.Time { return now }})
	const offset = int64(-10)
	c.SetOffset(offset)
	c.Anchor(ServerNowAt(now, offset))

	trueElapsed := 5 * time.Second
	now = base.Add(trueElapsed)
	anchoredServerNow := c.ServerNow()
	wantTruth := ServerNowAt(base, offset) + int64(trueElapsed.Seconds())
	if anchoredServerNow != wantTruth {
		t.Fatalf("anchored ServerNow() = %d, want %d (ground truth)", anchoredServerNow, wantTruth)
	}

	stepped := base.Add(trueElapsed - time.Hour)
	legacyServerNow := ServerNowAt(stepped, offset)
	if diff := anchoredServerNow - legacyServerNow; diff != 3600 {
		t.Errorf("anchored-vs-legacy divergence = %ds, want 3600s (the simulated backward step size)", diff)
	}
}

func TestClock_Anchor_ReanchorClearsDrift(t *testing.T) {
	base := time.Now()
	now := base
	c := New(Config{Now: func() time.Time { return now }})

	c.Anchor(100)
	now = base.Add(time.Hour) // a lot of elapsed / drift accrues
	if drift, stepped := c.LocalStep(); drift != 0 || stepped {
		t.Fatalf("LocalStep() after 1h of pure elapsed time = (%d, %v), want (0, false) — elapsed time alone is not a step", drift, stepped)
	}

	// Re-anchoring resets the reference point; LocalStep against the SAME
	// instant it just re-anchored at reports zero drift.
	c.Anchor(ServerNowAt(now, 0))
	if drift, stepped := c.LocalStep(); drift != 0 || stepped {
		t.Errorf("LocalStep() immediately after re-anchor = (%d, %v), want (0, false)", drift, stepped)
	}
}

func TestClock_LocalStep_NeverAnchored(t *testing.T) {
	c := New(Config{})
	if drift, stepped := c.LocalStep(); drift != 0 || stepped {
		t.Errorf("LocalStep() before any Anchor = (%d, %v), want (0, false)", drift, stepped)
	}
}

// TestClock_LocalStep_DriftClassification white-box tests the drift formula
// directly (wallElapsed − monoElapsed, classified against StepThresholdS).
// Real divergence between wall-elapsed and monotonic-elapsed can only arise
// from an actual OS-level clock step straddling two real time.Now() calls —
// not reproducible through the public API (see the package note above) — so
// this constructs the anchor fields directly (legal: same-package test) to
// exercise the classification arithmetic Anchor/LocalStep implement.
func TestClock_LocalStep_DriftClassification(t *testing.T) {
	anchorInstant := time.Now()
	tests := []struct {
		name       string
		anchorWall int64 // deliberately offset from anchorInstant.Unix() to simulate wall/mono divergence
		nowOffset  time.Duration
		threshold  int64
		wantDrift  int64
		wantStep   bool
	}{
		{"no drift", anchorInstant.Unix(), 5 * time.Second, 30, 0, false},
		{"forward drift under threshold", anchorInstant.Unix() - 20, 5 * time.Second, 30, 20, false},
		{"forward drift at threshold (inclusive)", anchorInstant.Unix() - 30, 5 * time.Second, 30, 30, true},
		{"forward drift over threshold", anchorInstant.Unix() - 3600, 5 * time.Second, 30, 3600, true},
		{"backward drift under threshold", anchorInstant.Unix() + 20, 5 * time.Second, 30, -20, false},
		{"backward drift at threshold (inclusive)", anchorInstant.Unix() + 30, 5 * time.Second, 30, -30, true},
		{"backward drift over threshold", anchorInstant.Unix() + 3600, 5 * time.Second, 30, -3600, true},
		{"default threshold applied when unset", anchorInstant.Unix() - 3600, 5 * time.Second, 0, 3600, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := anchorInstant.Add(tt.nowOffset)
			c := &Clock{
				cfg:          Config{Now: func() time.Time { return now }, StepThresholdS: tt.threshold},
				anchored:     true,
				anchorServer: 42,
				anchorMono:   anchorInstant,
				anchorWall:   tt.anchorWall,
			}
			drift, stepped := c.LocalStep()
			if drift != tt.wantDrift || stepped != tt.wantStep {
				t.Errorf("LocalStep() = (%d, %v), want (%d, %v)", drift, stepped, tt.wantDrift, tt.wantStep)
			}
		})
	}
}

func TestInWindow(t *testing.T) {
	const start, end = 1000, 1100
	tests := []struct {
		name      string
		serverNow int64
		want      bool
	}{
		{"before start: not in window", 999, false},
		{"at start: inclusive, in window", 1000, true},
		{"mid window", 1050, true},
		{"just before end: in window", 1099, true},
		{"at end: exclusive, not in window", 1100, false},
		{"past end: not in window", 1200, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := InWindow(start, end, tt.serverNow); got != tt.want {
				t.Errorf("InWindow(%d, %d, %d) = %v, want %v", start, end, tt.serverNow, got, tt.want)
			}
		})
	}
}
