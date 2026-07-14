package egress

import "testing"

// TestGate_NilIsOpen pins the nil-receiver convention: a component wired
// with no gate must behave exactly as if egress were never suspended.
func TestGate_NilIsOpen(t *testing.T) {
	var g *Gate
	if g.Suspended() {
		t.Fatal("nil *Gate.Suspended() = true, want false")
	}
	if g.Reason() != "" {
		t.Fatalf("nil *Gate.Reason() = %q, want empty", g.Reason())
	}
	// Must not panic.
	g.Suspend("x")
	g.Resume()
}

// TestGate_SuspendResumeRoundTrip verifies the zero value starts open, and
// Suspend/Resume toggle state + reason as documented (idempotently).
func TestGate_SuspendResumeRoundTrip(t *testing.T) {
	g := &Gate{}
	if g.Suspended() {
		t.Fatal("zero-value Gate starts suspended, want open")
	}

	g.Suspend("registration-pin")
	if !g.Suspended() {
		t.Fatal("Suspended() = false after Suspend")
	}
	if g.Reason() != "registration-pin" {
		t.Fatalf("Reason() = %q, want %q", g.Reason(), "registration-pin")
	}

	g.Suspend("registration-pin") // idempotent re-assert
	if !g.Suspended() {
		t.Fatal("re-Suspend flipped the gate open")
	}

	g.Resume()
	if g.Suspended() {
		t.Fatal("Suspended() = true after Resume")
	}
	if g.Reason() != "" {
		t.Fatalf("Reason() after Resume = %q, want empty", g.Reason())
	}
}
