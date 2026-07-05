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
