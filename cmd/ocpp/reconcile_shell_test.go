package main

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/metrics"
)

// recordingProfileDriver captures every profile apply and can be told to fail
// (simulating a delivered-but-rejected SetChargingProfile, ledger L11). WP-13
// adds the ApplyClear release path (same failure knob — a rejected Clear is
// the same L11 error class).
type recordingProfileDriver struct {
	applied []float64 // limitA per apply that SUCCEEDED
	cleared int       // ApplyClear calls that SUCCEEDED
	err     error     // non-nil ⇒ every Apply/ApplyClear fails (rejected)
	calls   int
}

func (d *recordingProfileDriver) Apply(_ string, _ int, limitA float64) error {
	d.calls++
	if d.err != nil {
		return d.err
	}
	d.applied = append(d.applied, limitA)
	return nil
}

func (d *recordingProfileDriver) ApplyClear(_ string, _ int) error {
	d.calls++
	if d.err != nil {
		return d.err
	}
	d.cleared++
	return nil
}

func evseDoc(maxA float64, connectorID int, seq uint64, at time.Time) bus.DesiredState {
	m := maxA
	return bus.DesiredState{
		DeviceClass: bus.DesiredClassEVSE, DeviceID: "cs-001",
		MaxCurrentA: &m, ConnectorID: connectorID,
		Source: "economic", IssuedAt: at.Unix(), Seq: seq,
	}
}

// TestEVSEShell_NewLimitWrites: a new current-limit doc is applied via the driver.
func TestEVSEShell_NewLimitWrites(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingProfileDriver{}
	s := newEVSEShell("cs-001", mreg, modeActive, drv)
	t0 := time.Now()
	s.setDesired(evseDoc(10, 0, 1, t0), t0)
	if len(drv.applied) != 1 || drv.applied[0] != 10 {
		t.Fatalf("expected one 10A apply, got %v", drv.applied)
	}
	if s.connectorID != 1 { // connector 0 → 1
		t.Errorf("connector 0 must map to 1, got %d", s.connectorID)
	}
}

// TestEVSEShell_AcceptedIsNotConvergence: a write that the driver ACCEPTS is a
// write success, NOT convergence. If metered current still exceeds the limit
// (ev-accept-but-ignore), the reconciler keeps correcting.
func TestEVSEShell_AcceptedIsNotConvergence(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingProfileDriver{} // Accepts every profile
	s := newEVSEShell("cs-001", mreg, modeActive, drv)
	t0 := time.Now()
	s.setDesired(evseDoc(10, 0, 1, t0), t0) // applied, "Accepted"
	// Charger keeps drawing 16 A despite the accepted 10 A profile.
	s.observe(16, true, true, t0.Add(time.Second))
	if len(drv.applied) != 2 {
		t.Fatalf("accepted-but-ignored must drive a corrective write, applies=%d", len(drv.applied))
	}
	if !strings.Contains(mreg.Format(), "lexa_ocpp_shadow_divergences_total 1") {
		t.Errorf("over-limit metered current must count as divergence, got:\n%s", mreg.Format())
	}
}

// TestEVSEShell_OneSidedUnderLimit: an EV drawing LESS than its limit is
// compliant — a match, never a divergence, never a corrective write.
func TestEVSEShell_OneSidedUnderLimit(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingProfileDriver{}
	s := newEVSEShell("cs-001", mreg, modeActive, drv)
	t0 := time.Now()
	s.setDesired(evseDoc(16, 0, 1, t0), t0) // applied #1
	s.observe(6, true, true, t0.Add(time.Second))
	if len(drv.applied) != 1 {
		t.Fatalf("under-limit must not correct, applies=%d", len(drv.applied))
	}
	if !strings.Contains(mreg.Format(), "lexa_ocpp_shadow_matches_total 1") {
		t.Errorf("under-limit must be a match, got:\n%s", mreg.Format())
	}
}

// TestEVSEShell_SuspendConvergesAtZero: a 0 A suspend converges when metered
// current is ≈0 (which TransactionEvent Ended forces by zeroing currentA).
func TestEVSEShell_SuspendConvergesAtZero(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingProfileDriver{}
	s := newEVSEShell("cs-001", mreg, modeActive, drv)
	t0 := time.Now()
	s.setDesired(evseDoc(0, 0, 1, t0), t0) // suspend, applied #1
	s.observe(0, true, true, t0.Add(time.Second))
	if len(drv.applied) != 1 {
		t.Fatalf("0 A metered against a 0 A suspend must converge (no correction), applies=%d", len(drv.applied))
	}
	if !strings.Contains(mreg.Format(), "lexa_ocpp_shadow_matches_total 1") {
		t.Errorf("suspend at 0 A must be a match, got:\n%s", mreg.Format())
	}
}

// TestEVSEShell_RejectedProfileRetries: a delivered-but-rejected profile (L11)
// is a write FAILURE; the charger keeps overshooting, so the next metered sample
// drives another attempt — reject → failure → retry (never a false success).
func TestEVSEShell_RejectedProfileRetries(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingProfileDriver{err: errors.New("rejected: status=\"Rejected\"")}
	s := newEVSEShell("cs-001", mreg, modeActive, drv)
	t0 := time.Now()
	s.setDesired(evseDoc(10, 0, 1, t0), t0)        // attempt #1 → fails
	s.observe(16, true, true, t0.Add(time.Second)) // still over → attempt #2 → fails
	if drv.calls < 2 {
		t.Fatalf("rejected profile must be retried on continued divergence, calls=%d", drv.calls)
	}
	if len(drv.applied) != 0 {
		t.Fatalf("a rejected profile must never count as an applied write, applied=%v", drv.applied)
	}
	if !strings.Contains(mreg.Format(), "lexa_ocpp_reconcile_write_failures_total 2") {
		t.Errorf("expected 2 write failures, got:\n%s", mreg.Format())
	}
}

// TestEVSEShell_ImplausibleIgnored: an implausible sample (ev-wrong-units) is
// evidence of nothing — no write, no match, no divergence.
func TestEVSEShell_ImplausibleIgnored(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingProfileDriver{}
	s := newEVSEShell("cs-001", mreg, modeActive, drv)
	t0 := time.Now()
	s.setDesired(evseDoc(16, 0, 1, t0), t0) // applied #1
	s.observe(6000, false /* implausible */, true, t0.Add(time.Second))
	if len(drv.applied) != 1 {
		t.Fatalf("implausible sample must not provoke a write, applies=%d", len(drv.applied))
	}
	out := mreg.Format()
	if !strings.Contains(out, "lexa_ocpp_shadow_matches_total 0") || !strings.Contains(out, "lexa_ocpp_shadow_divergences_total 0") {
		t.Errorf("implausible sample must be neither match nor divergence, got:\n%s", out)
	}
}

// TestEVSEShell_SilenceIsNotConvergence: a charger that goes silent (no samples)
// must NOT be treated as converged — ticks with no observation declare nothing.
func TestEVSEShell_SilenceIsNotConvergence(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingProfileDriver{}
	s := newEVSEShell("cs-001", mreg, modeActive, drv)
	t0 := time.Now()
	s.setDesired(evseDoc(10, 0, 1, t0), t0)
	// No observe at all; only wall-clock ticks.
	for i := 0; i < 5; i++ {
		s.tick(t0.Add(time.Duration(i+1) * 20 * time.Second))
	}
	if !strings.Contains(mreg.Format(), "lexa_ocpp_shadow_matches_total 0") {
		t.Errorf("silence must never be counted a match, got:\n%s", mreg.Format())
	}
}

// TestEVSEShell_ReconnectReasserts: a reconnect signal reasserts the standing
// limit (the gap the legacy path never closed) and is consumed once.
func TestEVSEShell_ReconnectReasserts(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingProfileDriver{}
	s := newEVSEShell("cs-001", mreg, modeActive, drv)
	t0 := time.Now()
	s.setDesired(evseDoc(10, 0, 1, t0), t0) // applied #1
	s.markReconnected()
	s.observe(10, true, true, t0.Add(time.Second)) // reassert #2, then converged
	if len(drv.applied) != 2 {
		t.Fatalf("reconnect must reassert the standing limit, applies=%d", len(drv.applied))
	}
	// Second observe: signal consumed, converged → no further write.
	s.observe(10, true, true, t0.Add(2*time.Second))
	if len(drv.applied) != 2 {
		t.Fatalf("reconnect signal must fire once, applies=%d", len(drv.applied))
	}
}

// TestEVSEShell_ShadowNeverWrites: a shadow shell has no driver and never routes
// a write, even under sustained over-limit divergence.
func TestEVSEShell_ShadowNeverWrites(t *testing.T) {
	mreg := metrics.New()
	s := newEVSEShell("cs-001", mreg, modeShadow, nil)
	if s.active() || s.driver != nil {
		t.Fatal("shadow shell must have no driver")
	}
	t0 := time.Now()
	s.setDesired(evseDoc(10, 0, 1, t0), t0)
	for i := 0; i < 5; i++ {
		s.observe(20, true, true, t0.Add(time.Duration(i+1)*40*time.Second))
	}
	if !strings.Contains(mreg.Format(), "lexa_ocpp_reconcile_writes_total 0") {
		t.Errorf("shadow must never apply a reconcile write, got:\n%s", mreg.Format())
	}
}

// TestEVSEShell_BackoffExceedsCallBound: corrective re-writes are spaced ≥ the
// 10 s per-call bound (the shell configures a 15 s first backoff) so writes to
// one station never overlap an in-flight SetChargingProfile.
func TestEVSEShell_BackoffExceedsCallBound(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingProfileDriver{}
	s := newEVSEShell("cs-001", mreg, modeActive, drv)
	t0 := time.Now()
	s.setDesired(evseDoc(10, 0, 1, t0), t0)        // applied #1
	s.observe(16, true, true, t0.Add(time.Second)) // divergence → applied #2
	if len(drv.applied) != 2 {
		t.Fatalf("first divergence should write, applies=%d", len(drv.applied))
	}
	// 5 s later, still diverged: backoff (15 s) not elapsed → NO overlapping write.
	s.observe(16, true, true, t0.Add(6*time.Second))
	if len(drv.applied) != 2 {
		t.Fatalf("a re-write within the 15 s backoff would overlap the call bound, applies=%d", len(drv.applied))
	}
	// Past the backoff: the corrective write resumes.
	s.observe(16, true, true, t0.Add(20*time.Second))
	if len(drv.applied) != 3 {
		t.Fatalf("after backoff the corrective write should resume, applies=%d", len(drv.applied))
	}
}

// TestEVSEShell_UnknownStationNoPanic guards the observeShell no-op path
// indirectly: observing a shell with no desired yet must not write or panic.
func TestEVSEShell_NoDesiredNoWrite(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingProfileDriver{}
	s := newEVSEShell("cs-001", mreg, modeActive, drv)
	s.observe(16, true, true, time.Now())
	if len(drv.applied) != 0 {
		t.Fatalf("no standing desired ⇒ no write, applies=%d", len(drv.applied))
	}
	_ = math.NaN
}

// evseConnectDoc builds an EVSE desired doc carrying an explicit Connect
// opinion alongside a current limit (Unit 6.2 gateway cease-to-energize
// fan-out — mirrors cmd/hub's ApplyEVSECommand shape).
func evseConnectDoc(maxA float64, connect bool, seq uint64, at time.Time) bus.DesiredState {
	d := evseDoc(maxA, 0, seq, at)
	c := connect
	d.Connect = &c
	return d
}

// TestEVSEShell_DisconnectWritesZeroAmps: Connect=false must drive the ONLY
// disconnect verb OCPP SP-limits give us — a 0 A SetChargingProfile — even
// though the doc also carries an explicit non-zero MaxCurrentA (disconnect
// WINS, the unit 6.2 safety ordering).
func TestEVSEShell_DisconnectWritesZeroAmps(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingProfileDriver{}
	s := newEVSEShell("cs-001", mreg, modeActive, drv)
	t0 := time.Now()
	s.setDesired(evseConnectDoc(20, false, 1, t0), t0)
	if len(drv.applied) != 1 || drv.applied[0] != 0 {
		t.Fatalf("disconnect must apply 0 A regardless of MaxCurrentA=20, applied=%v", drv.applied)
	}
}

// TestEVSEShell_ReconnectRestoresStandingCurrent: Connect flipping back to
// true in a later doc restores THAT doc's own current (not some
// separately-cached "last connected" value) — "reconnect defers to the
// doc's current" per the unit 6.2 spec.
func TestEVSEShell_ReconnectRestoresStandingCurrent(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingProfileDriver{}
	s := newEVSEShell("cs-001", mreg, modeActive, drv)
	t0 := time.Now()
	s.setDesired(evseConnectDoc(20, false, 1, t0), t0)                                  // disconnect → 0 A
	s.setDesired(evseConnectDoc(16, true, 2, t0.Add(time.Second)), t0.Add(time.Second)) // reconnect at 16 A
	if len(drv.applied) != 2 || drv.applied[1] != 16 {
		t.Fatalf("reconnect must restore the doc's own current (16A), applied=%v", drv.applied)
	}
}

// TestEVSEShell_ConnectToggleAloneWrites: a doc that changes ONLY Connect
// (the raw MaxCurrentA field value is unchanged) must still be treated as a
// new target — the fold changes the EFFECTIVE current even though the doc's
// own MaxCurrentA didn't move.
func TestEVSEShell_ConnectToggleAloneWrites(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingProfileDriver{}
	s := newEVSEShell("cs-001", mreg, modeActive, drv)
	t0 := time.Now()
	s.setDesired(evseConnectDoc(16, true, 1, t0), t0)                                    // applied #1: 16A
	s.setDesired(evseConnectDoc(16, false, 2, t0.Add(time.Second)), t0.Add(time.Second)) // same 16A field, disconnect
	if len(drv.applied) != 2 || drv.applied[1] != 0 {
		t.Fatalf("connect toggle alone must still fold to a new write, applied=%v", drv.applied)
	}
}

// TestEVSEShell_DisconnectConvergesAtMeteredZero verifies the unit 6.2 spec's
// central claim: cease-to-energize's convergence "verifies naturally" via
// the EXISTING one-sided MaxCurrentA check, with ZERO core/observe changes —
// a metered ≈0 A reading after a fold-to-0 write is a match.
func TestEVSEShell_DisconnectConvergesAtMeteredZero(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingProfileDriver{}
	s := newEVSEShell("cs-001", mreg, modeActive, drv)
	t0 := time.Now()
	s.setDesired(evseConnectDoc(20, false, 1, t0), t0) // → 0 A, applied #1
	s.observe(0, true, true, t0.Add(time.Second))
	if len(drv.applied) != 1 {
		t.Fatalf("metered 0A after disconnect must converge (no correction), applied=%d", len(drv.applied))
	}
	if !strings.Contains(mreg.Format(), "lexa_ocpp_shadow_matches_total 1") {
		t.Errorf("disconnect converging at 0A must count as a match, got:\n%s", mreg.Format())
	}
}

// TestEVSEShell_DisconnectStillDrawingDiverges: metered current above ~0
// after a disconnect is commanded is genuine divergence — the
// ev-accept-but-ignore lesson applies to Connect too, via a corrective
// 0 A rewrite.
func TestEVSEShell_DisconnectStillDrawingDiverges(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingProfileDriver{}
	s := newEVSEShell("cs-001", mreg, modeActive, drv)
	t0 := time.Now()
	s.setDesired(evseConnectDoc(20, false, 1, t0), t0) // → 0 A, applied #1
	s.observe(6, true, true, t0.Add(time.Second))      // still drawing despite disconnect
	if len(drv.applied) != 2 || drv.applied[1] != 0 {
		t.Fatalf("still-drawing after disconnect must correct back to 0A, applied=%v", drv.applied)
	}
}

// TestEVSEShell_ConnectTrueDoesNotWedgeCurrentConvergence guards the
// systemic bug this design choice avoids: cmd/hub's EVSE actuator has
// asserted a non-nil Connect on EVERY published doc since TASK-030, so had
// Connect been fed straight through to internal/reconcile as its own Field,
// the completeness gate would hold FOREVER and this corrective write would
// never fire in production. Folding Connect into MaxCurrentA before
// SetDesired keeps this axis exactly as verifiable as it was before Unit 6.2.
func TestEVSEShell_ConnectTrueDoesNotWedgeCurrentConvergence(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingProfileDriver{}
	s := newEVSEShell("cs-001", mreg, modeActive, drv)
	t0 := time.Now()
	s.setDesired(evseConnectDoc(10, true, 1, t0), t0) // applied #1: 10A, Connect=true
	s.observe(16, true, true, t0.Add(time.Second))    // still over → corrective write
	if len(drv.applied) != 2 {
		t.Fatalf("a Connect=true doc must not wedge MaxCurrentA convergence, applied=%d", len(drv.applied))
	}
}

// TestEVSEShell_AbsentConnectUnchanged: a doc with no Connect opinion at all
// (today's shape, and every existing test fixture in this file) must write
// MaxCurrentA byte-identically to pre-Unit-6.2 behavior — the upgrade guard.
func TestEVSEShell_AbsentConnectUnchanged(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingProfileDriver{}
	s := newEVSEShell("cs-001", mreg, modeActive, drv)
	t0 := time.Now()
	s.setDesired(evseDoc(10, 0, 1, t0), t0)
	if len(drv.applied) != 1 || drv.applied[0] != 10 {
		t.Fatalf("absent Connect must write MaxCurrentA unfolded, applied=%v", drv.applied)
	}
}
