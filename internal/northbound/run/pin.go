// pin.go implements WP-7 (a) / architecture D4: per-walk verification of the
// server's Registration.pIN against the operator-configured registration_pin
// (CORE-003 / BASIC-001), with the D4 fail-closed posture on failure:
//
//  1. Control fails CLOSED — the currently-adopted control is HELD via the
//     scheduler's own last-known-good discipline (RunOnce evaluates with no
//     programs, the exact hold an absent program list gets), and NO new
//     control from this server is adopted while the freeze is on. Dropping
//     enforcement on mismatch would fail OPEN — releasing caps because of a
//     provisioning error — the enforce-but-verify compromise TASK-042
//     established.
//  2. Server egress is suspended via the shared egress.Gate — Response and
//     FlowReservationRequest POSTs today; the WP-4 DER* PUT reporter and
//     WP-6 LogEvent poster check the same Gate when they land. MUP posts
//     (lexa-telemetry, separate process) key off the pin_ok field on the
//     retained certstatus doc instead.
//  3. Loud — slog Error edge, lexa_nb_pin_mismatch gauge, and pin_ok on
//     bus.CertStatus (via the onChange → certmon.CheckOnce republish).
//  4. Self-healing — re-checked every successful walk; a match clears the
//     freeze, gauge, and pin_ok immediately.
//
// registration_pin=0 (the shipped default) disables the check entirely with
// one startup WARN (WS-8 disabled-default pattern): no PinVerifier is
// constructed and Discovery.pin stays nil.
package run

import (
	"context"
	"log/slog"
	"sync"

	"lexa-hub/internal/metrics"
	"lexa-hub/internal/northbound/discovery"
	"lexa-hub/internal/northbound/egress"
	model "lexa-proto/csipmodel"
)

// pinStatus is the verifier's last verdict.
type pinStatus int

const (
	pinUnchecked pinStatus = iota // no successful walk has completed a check yet
	pinVerified
	pinFrozen
)

// PinVerifier owns the registration-PIN check state: the expected PIN, the
// shared egress gate it drives, the mismatch gauge, and the last verdict
// (for edge-triggered logging and the pin_ok surface).
type PinVerifier struct {
	expected uint32
	gate     *egress.Gate
	mismatch *metrics.Gauge // lexa_nb_pin_mismatch (nil-safe)
	// onChange fires — outside mu — on every verdict TRANSITION
	// (unchecked→verified, verified→frozen, frozen→verified), so the wiring
	// (cmd/northbound/main.go) can force an immediate certstatus republish
	// (certMon.CheckOnce) instead of waiting up to 24h for pin_ok to update.
	onChange func()

	mu     sync.Mutex
	status pinStatus
}

// NewPinVerifier constructs a verifier for expectedPIN (callers guarantee
// non-zero — zero means "check disabled, construct nothing"). gate, gauge,
// and onChange may each be nil.
func NewPinVerifier(expectedPIN uint32, gate *egress.Gate, mismatchGauge *metrics.Gauge, onChange func()) *PinVerifier {
	return &PinVerifier{expected: expectedPIN, gate: gate, mismatch: mismatchGauge, onChange: onChange}
}

// Check runs one verification against the walk's freshly-discovered self
// EndDevice, returning true when the D4 freeze must be (or stay) in force:
// the Registration resource's pIN mismatched, or the Registration fetch
// failed while the check is required (a server that hides Registration from
// a client configured to verify it is treated exactly like a mismatch —
// fetch-failure-when-required, never fail-open). Reuses
// discovery.VerifyRegistration verbatim: any error return — missing
// RegistrationLink, fetch error, PIN mismatch — freezes.
//
// nil-safe: a nil *PinVerifier (registration_pin=0) never freezes.
// Called only from the single walk goroutine (RunOnce).
func (v *PinVerifier) Check(ctx context.Context, w *discovery.Walker, self *model.EndDevice) bool {
	if v == nil {
		return false
	}
	if _, err := w.VerifyRegistration(ctx, self, v.expected); err != nil {
		v.transition(pinFrozen, err)
		return true
	}
	v.transition(pinVerified, nil)
	return false
}

// PinOK is the queryable pin_ok surface for the retained certstatus doc
// (bus.CertStatus.PinOK): nil when the check is disabled (nil verifier) or
// has not produced a verdict yet (no successful walk since start — the
// INCONCLUSIVE-safe state, mirroring the plan heartbeat's "never"), else
// the current verdict. Safe as a method value on a nil *PinVerifier.
func (v *PinVerifier) PinOK() *bool {
	if v == nil {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	switch v.status {
	case pinVerified:
		b := true
		return &b
	case pinFrozen:
		b := false
		return &b
	default:
		return nil
	}
}

// transition records the new verdict, re-asserts the gauge/gate every call
// (idempotent, self-healing), and logs + fires onChange on edges only.
// Gauge/gate/log/onChange all run OUTSIDE mu: onChange typically calls
// certMon.CheckOnce, which reads PinOK — re-entering mu — and none of them
// need the lock.
func (v *PinVerifier) transition(to pinStatus, err error) {
	v.mu.Lock()
	from := v.status
	v.status = to
	v.mu.Unlock()

	switch to {
	case pinFrozen:
		v.mismatch.Set(1)
		v.gate.Suspend("registration-pin")
		if from != pinFrozen {
			slog.Error("lexa-northbound: registration PIN verification FAILED — freezing control adoption and suspending server egress (D4 fail-closed: held control stays enforced until its own expiry; re-checked every walk)",
				"err", err)
		}
	case pinVerified:
		v.mismatch.Set(0)
		v.gate.Resume()
		switch from {
		case pinFrozen:
			slog.Info("lexa-northbound: registration PIN verified — freeze cleared, control adoption and server egress resumed")
		case pinUnchecked:
			slog.Info("lexa-northbound: registration PIN verified")
		}
	}
	if from != to && v.onChange != nil {
		v.onChange()
	}
}
