// Package egress provides the northbound server-egress gate (WP-7,
// architecture D4): one shared, queryable freeze switch every component that
// POSTs/PUTs to the utility server checks before transmitting. It exists so
// the registration-PIN verifier (internal/northbound/run/pin.go) has a single
// place to suspend "everything we send to this server" — 2030.5 §6.9.2(c)
// "stop using server" — without each poster growing its own freeze plumbing.
//
// Consumers today: responses.Tracker (Response POSTs) and flowres.Manager
// (FlowReservationRequest POSTs). The WP-4 DER* PUT reporter and WP-6
// LogEvent poster are expected to take the same *Gate at construction and
// check Suspended() before each PUT/POST — no rework needed, just wiring.
// MUP posts live in lexa-telemetry (a separate process), so their suspension
// surface is the pin_ok field on the retained lexa/northbound/certstatus doc
// (bus.CertStatus), not this in-process Gate.
package egress

import "sync"

// Gate is a suspend/resume switch, safe for concurrent use. Its zero value
// is an open (not suspended) gate.
//
// Nil-safe: every method on a nil *Gate behaves as an open gate (matches the
// metrics.Counter/journal.Writer nil-receiver convention throughout this
// codebase), so components take an optional gate and tests that don't care
// about egress freezing pass nil unchanged.
type Gate struct {
	mu        sync.Mutex
	suspended bool
	reason    string
}

// Suspend closes the gate, recording reason for logs/queries. Idempotent —
// re-suspending refreshes the reason and nothing else, so a per-walk
// re-assertion is harmless.
func (g *Gate) Suspend(reason string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.suspended = true
	g.reason = reason
	g.mu.Unlock()
}

// Resume reopens the gate. Idempotent.
func (g *Gate) Resume() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.suspended = false
	g.reason = ""
	g.mu.Unlock()
}

// Suspended reports whether server egress is currently frozen. A nil *Gate
// is never suspended.
func (g *Gate) Suspended() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.suspended
}

// Reason returns the most recent Suspend reason, or "" when open/nil.
func (g *Gate) Reason() string {
	if g == nil {
		return ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reason
}
