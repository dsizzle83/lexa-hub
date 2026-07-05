package utilitytime

import "testing"

func TestDebouncedExpiry_ConsecutiveConfirm(t *testing.T) {
	d := &DebouncedExpiry{Confirm: 3}

	if d.Observe(true) {
		t.Fatal("tick 1 of 3: must not fire yet")
	}
	if d.Observe(true) {
		t.Fatal("tick 2 of 3: must not fire yet")
	}
	if !d.Observe(true) {
		t.Fatal("tick 3 of 3 consecutive: must fire")
	}
}

func TestDebouncedExpiry_2of3FlappingNeverFires(t *testing.T) {
	d := &DebouncedExpiry{Confirm: 3}

	// true, true, false, true, true, false, ... — never 3 IN A ROW.
	pattern := []bool{true, true, false, true, true, false, true, true, false}
	for i, expired := range pattern {
		if got := d.Observe(expired); got {
			t.Fatalf("tick %d: flapping 2-of-3 pattern must never fire, but Observe(%v) returned true", i, expired)
		}
	}
}

func TestDebouncedExpiry_FalseResetsCounter(t *testing.T) {
	d := &DebouncedExpiry{Confirm: 3}
	d.Observe(true)
	d.Observe(true)
	// One false resets — the next two trues must NOT complete a stale run.
	d.Observe(false)
	if d.Observe(true) {
		t.Fatal("tick 1 after reset: must not fire")
	}
	if d.Observe(true) {
		t.Fatal("tick 2 after reset: must not fire")
	}
	if !d.Observe(true) {
		t.Fatal("tick 3 after reset: must fire")
	}
}

func TestDebouncedExpiry_Reset(t *testing.T) {
	d := &DebouncedExpiry{Confirm: 3}
	d.Observe(true)
	d.Observe(true)
	d.Reset()
	if d.Observe(true) {
		t.Fatal("tick 1 after Reset(): must not fire")
	}
	if d.Observe(true) {
		t.Fatal("tick 2 after Reset(): must not fire")
	}
	if !d.Observe(true) {
		t.Fatal("tick 3 after Reset(): must fire")
	}
}

func TestDebouncedExpiry_ZeroConfirmTreatedAsOne(t *testing.T) {
	d := &DebouncedExpiry{} // Confirm unset (0)
	if !d.Observe(true) {
		t.Fatal("Confirm<=0 must behave as Confirm=1 (fire immediately), not never-fire")
	}
}

func TestDebouncedExpiry_NeverExpiredStaysFalse(t *testing.T) {
	d := &DebouncedExpiry{Confirm: 3}
	for i := 0; i < 10; i++ {
		if d.Observe(false) {
			t.Fatalf("tick %d: Observe(false) must never fire", i)
		}
	}
}

// TestDebouncedExpiry_TransientJumpRidesOut mirrors the exact scenario
// documented at cmd/hub/state.go:348-371: an NTP correction or flapping
// grid-server clock can momentarily push server-now past ValidUntil for one
// or two ticks. That transient excursion must ride out (the control keeps
// being reported/enforced) as long as it never reaches Confirm consecutive
// ticks, while a SUSTAINED expiry (Confirm consecutive ticks past
// ValidUntil) drops on the Confirm-th tick — matching expiryConfirmTicks=3.
func TestDebouncedExpiry_TransientJumpRidesOut(t *testing.T) {
	d := &DebouncedExpiry{Confirm: 3}

	// Ticks 1-2: server clock (falsely) reads past ValidUntil (a transient
	// forward jump). Not yet 3 consecutive — control must still be held.
	if d.Observe(true) {
		t.Fatal("tick 1: transient excursion must not drop the control")
	}
	if d.Observe(true) {
		t.Fatal("tick 2: transient excursion must not drop the control")
	}
	// Tick 3: the clock corrects itself back before ValidUntil. The
	// excursion rides out completely — counter resets, control stays held
	// indefinitely afterward as long as expiry doesn't return.
	if d.Observe(false) {
		t.Fatal("tick 3: clock settled back under ValidUntil, must not drop")
	}
	for i := 0; i < 5; i++ {
		if d.Observe(false) {
			t.Fatalf("post-settle tick %d: must stay held, no expiry pending", i)
		}
	}

	// Now a GENUINE, sustained expiry: 3 consecutive ticks past ValidUntil
	// (the control's publisher is actually gone / the event genuinely
	// ended) — must drop exactly on the 3rd tick, not before.
	if d.Observe(true) {
		t.Fatal("sustained tick 1 of 3: must not drop yet")
	}
	if d.Observe(true) {
		t.Fatal("sustained tick 2 of 3: must not drop yet")
	}
	if !d.Observe(true) {
		t.Fatal("sustained tick 3 of 3: must drop now")
	}
}

func TestReportGrace_ValidUntilZeroAlwaysReportable(t *testing.T) {
	g := ReportGrace{GraceS: 15}
	if !g.Reportable(0, 1_000_000_000) {
		t.Error("ValidUntil=0 (e.g. a DefaultDERControl) must always be reportable")
	}
	if !g.Reportable(0, -1) {
		t.Error("ValidUntil=0 must be reportable regardless of serverNow")
	}
}

// TestReportGrace_15sBoundary mirrors cmd/api/stale_test.go's
// TestBuildStatus_ExpiredControlNotReported table: within grace still
// reportable, past grace not reportable, at the exact boundary not
// reportable (strict "<", matching handlers.go:145-146's ">=" for "expired").
func TestReportGrace_15sBoundary(t *testing.T) {
	g := ReportGrace{GraceS: 15}
	const validUntil = 1000

	tests := []struct {
		name      string
		serverNow int64
		want      bool
	}{
		{"well before ValidUntil", 500, true},
		{"at ValidUntil", 1000, true},
		{"within grace (validUntil+5)", 1005, true},
		{"just under grace boundary (validUntil+14)", 1014, true},
		{"at grace boundary (validUntil+15): not reportable", 1015, false},
		{"past grace (validUntil+20)", 1020, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := g.Reportable(validUntil, tt.serverNow); got != tt.want {
				t.Errorf("Reportable(%d, %d) = %v, want %v", validUntil, tt.serverNow, got, tt.want)
			}
		})
	}
}

func TestReportGrace_ZeroGrace(t *testing.T) {
	g := ReportGrace{GraceS: 0}
	if !g.Reportable(1000, 999) {
		t.Error("serverNow before ValidUntil must be reportable even with zero grace")
	}
	if g.Reportable(1000, 1000) {
		t.Error("serverNow == ValidUntil with zero grace must not be reportable")
	}
}
