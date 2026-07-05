package main

import (
	"math"
	"strings"
	"testing"
	"time"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/metrics"
	"lexa-hub/internal/reconcile"
	"lexa-hub/internal/southbound/device"
	model "lexa-proto/csipmodel"
)

func ptrF(v float64) *float64 { return &v }
func ptrB(v bool) *bool       { return &v }

// TestBatteryShadow_NeverWrites is the shell's core safety property: driven
// with maximally divergent inputs (a desired doc the readback never matches,
// across many polls), the shadow keeps computing "would" actions and never
// calls anything resembling a device write. There is no controlApplier /
// registry reference anywhere in batteryShadow's fields for this test to even
// accidentally exercise — the absence of that dependency IS the guarantee;
// this test additionally pins the observable behavior (divergence keeps
// getting recorded, not silently dropped or panicking).
func TestBatteryShadow_NeverWrites(t *testing.T) {
	mreg := metrics.New()
	s := newBatteryShadow("battery-0", reconcile.Config{ConvergeTimeout: time.Second}, mreg)

	t0 := time.Now()
	setpoint := -500.0
	// SetpointW only (no Connect opinion): the shell has no register to read
	// Connect back from (see reconcile_shadow.go's file doc), so a doc
	// expressing Connect too would make every Observe here "incomplete, hold"
	// instead of "diverged" — a real, separately-covered limitation
	// (TestBatteryShadow_IncompleteReadHeldNotCounted below), not what this
	// test is pinning.
	s.setDesired(bus.DesiredState{
		DeviceClass: "battery", DeviceID: "battery-0",
		SetpointW: ptrF(setpoint),
		Source:    "economic", IssuedAt: t0.Unix(), Seq: 1,
	}, t0)

	// Every poll reads a wildly different, never-converging value, spaced
	// past DefaultRetryBackoff's longest tier (30s) so the core's rate
	// limiter never holds off a corrective write — every single Observe call
	// below is guaranteed to return a Write action, making the count exact.
	for i := 0; i < 10; i++ {
		now := t0.Add(time.Duration(i+1) * 40 * time.Second)
		s.observe(device.Measurements{W: 4000, SOC: 50, VA: math.NaN(), Var: math.NaN(), V: math.NaN(), Hz: math.NaN(), PF: math.NaN(), DCV: math.NaN(), DCW: math.NaN(), TmpCab: math.NaN()}, true, now)
	}

	out := mreg.Format()
	if !strings.Contains(out, "lexa_mb_shadow_divergences_total 10") {
		t.Errorf("expected 10 divergence observations recorded, got:\n%s", out)
	}
	if !strings.Contains(out, "lexa_mb_shadow_matches_total 0") {
		t.Errorf("expected 0 matches (never converges), got:\n%s", out)
	}
	// 1 (SetDesired's new-desired write) + 10 (one retry write per Observe).
	if !strings.Contains(out, "lexa_mb_shadow_would_writes_total 11") {
		t.Errorf("expected 11 would-writes total, got:\n%s", out)
	}
	// The type itself: batteryShadow holds no controlApplier/registry handle,
	// so there is no field through which a write could even be routed.
	_ = s
}

// TestBatteryShadow_ConvergedIsMatch verifies the steady-state case the
// acceptance criteria name explicitly: once desired and readback agree, the
// verdict is "match" and the matches counter (not divergences) advances.
func TestBatteryShadow_ConvergedIsMatch(t *testing.T) {
	mreg := metrics.New()
	s := newBatteryShadow("battery-0", reconcile.Config{}, mreg)
	t0 := time.Now()

	// SetpointW only: see TestBatteryShadow_IncompleteReadHeldNotCounted for
	// the (separately covered) Connect-included case, which this shell can
	// never assess as a match (no Connect-state register to read back).
	s.setDesired(bus.DesiredState{
		DeviceClass: "battery", DeviceID: "battery-0",
		SetpointW: ptrF(-500),
		Source:    "economic", IssuedAt: t0.Unix(), Seq: 1,
	}, t0)

	// Readback matches the desired setpoint exactly.
	s.observe(device.Measurements{W: -500, SOC: 60, VA: math.NaN(), Var: math.NaN(), V: math.NaN(), Hz: math.NaN(), PF: math.NaN(), DCV: math.NaN(), DCW: math.NaN(), TmpCab: math.NaN()}, true, t0.Add(time.Second))

	out := mreg.Format()
	if !strings.Contains(out, "lexa_mb_shadow_matches_total 1") {
		t.Errorf("expected 1 match, got:\n%s", out)
	}
	if !strings.Contains(out, "lexa_mb_shadow_divergences_total 0") {
		t.Errorf("expected 0 divergences, got:\n%s", out)
	}
}

// TestBatteryShadow_ImplausibleReadingHeldNotCounted mirrors ledger L9
// (plausibleW): an implausible reading must not be trusted as evidence of
// convergence OR divergence — the reconciler holds its previous assessment,
// and since nothing has been assessed yet, the very first observation being
// implausible must produce NEITHER a match nor a divergence.
func TestBatteryShadow_ImplausibleReadingHeldNotCounted(t *testing.T) {
	mreg := metrics.New()
	s := newBatteryShadow("battery-0", reconcile.Config{}, mreg)
	t0 := time.Now()

	s.setDesired(bus.DesiredState{
		DeviceClass: "battery", DeviceID: "battery-0",
		SetpointW: ptrF(-500),
		Source:    "economic", IssuedAt: t0.Unix(), Seq: 1,
	}, t0)
	s.observe(device.Measurements{W: 48000, SOC: 60, VA: math.NaN(), Var: math.NaN(), V: math.NaN(), Hz: math.NaN(), PF: math.NaN(), DCV: math.NaN(), DCW: math.NaN(), TmpCab: math.NaN()}, false /* implausible */, t0.Add(time.Second))

	out := mreg.Format()
	if !strings.Contains(out, "lexa_mb_shadow_matches_total 0") {
		t.Errorf("implausible reading must not count as a match, got:\n%s", out)
	}
	if !strings.Contains(out, "lexa_mb_shadow_divergences_total 0") {
		t.Errorf("implausible reading must not count as a divergence, got:\n%s", out)
	}
}

// TestBatteryShadow_IncompleteReadHeldNotCounted covers a real lexa-modbus
// limitation surfaced while wiring this shell: a battery doc commonly
// expresses BOTH SetpointW and Connect (cmd/hub's optimizer routinely sends
// both together), but there is no Modbus register this shell can read
// Connect back from — every such poll is "field-incomplete" per
// internal/reconcile's matches() (fixed alongside this task to be
// deterministic regardless of map iteration order; see reconcile.go's
// matches doc). It must hold — never a match, never a divergence, never a
// write — no matter how far the SetpointW reading is from desired.
func TestBatteryShadow_IncompleteReadHeldNotCounted(t *testing.T) {
	mreg := metrics.New()
	s := newBatteryShadow("battery-0", reconcile.Config{}, mreg)
	t0 := time.Now()

	s.setDesired(bus.DesiredState{
		DeviceClass: "battery", DeviceID: "battery-0",
		SetpointW: ptrF(-500), Connect: ptrB(true),
		Source: "economic", IssuedAt: t0.Unix(), Seq: 1,
	}, t0)
	for i := 0; i < 20; i++ {
		s.observe(device.Measurements{W: 4000, SOC: 50, VA: math.NaN(), Var: math.NaN(), V: math.NaN(), Hz: math.NaN(), PF: math.NaN(), DCV: math.NaN(), DCW: math.NaN(), TmpCab: math.NaN()},
			true, t0.Add(time.Duration(i+1)*40*time.Second))
	}

	out := mreg.Format()
	if !strings.Contains(out, "lexa_mb_shadow_matches_total 0") {
		t.Errorf("incomplete read must never count as a match, got:\n%s", out)
	}
	if !strings.Contains(out, "lexa_mb_shadow_divergences_total 0") {
		t.Errorf("incomplete read must never count as a divergence, got:\n%s", out)
	}
	// 1: only SetDesired's own new-desired write — the 20 Observe calls above
	// must never themselves trigger a would-write while Connect is unreadable.
	if !strings.Contains(out, "lexa_mb_shadow_would_writes_total 1") {
		t.Errorf("incomplete read must never itself trigger a would-write, got:\n%s", out)
	}
}

// TestBatteryShadow_ObserveLegacyWriteRoundTrip pins the sign-convention
// decode in describeControl against battCommandToControl's own encode
// ("must NOT change: battery command sign convention" per the task) — a
// positive BattCommand.SetpointW (discharge) round-trips through
// OpModExpLimW back to the same positive watt figure, negative (charge)
// through OpModImpLimW back to the same negative figure.
func TestBatteryShadow_ObserveLegacyWriteRoundTrip(t *testing.T) {
	mreg := metrics.New()
	s := newBatteryShadow("battery-0", reconcile.Config{}, mreg)

	discharge := 750.0
	ctrl := battCommandToControl(bus.BattCommand{SetpointW: &discharge, Connect: ptrB(true)})
	s.observeLegacyWrite(ctrl)
	if !s.haveLegacy {
		t.Fatal("observeLegacyWrite did not record")
	}
	if s.legacyDesc != "SetpointW=750,Connect=true" {
		t.Errorf("discharge round-trip = %q, want SetpointW=750,Connect=true", s.legacyDesc)
	}

	charge := -300.0
	ctrl2 := battCommandToControl(bus.BattCommand{SetpointW: &charge})
	s.observeLegacyWrite(ctrl2)
	if s.legacyDesc != "SetpointW=-300" {
		t.Errorf("charge round-trip = %q, want SetpointW=-300", s.legacyDesc)
	}
}

func TestDescribeFields(t *testing.T) {
	if got := describeFields(map[reconcile.Field]float64{reconcile.SetpointW: -500, reconcile.Connect: 1}); got != "SetpointW=-500,Connect=true" {
		t.Errorf("got %q", got)
	}
	if got := describeFields(nil); got != "(empty)" {
		t.Errorf("got %q, want (empty)", got)
	}
}

func TestDescribeControl_Empty(t *testing.T) {
	if got := describeControl(model.DERControlBase{}); got != "(empty)" {
		t.Errorf("got %q, want (empty)", got)
	}
}

// TestBatteryShadow_TickIdleIsSilent guards the rate-consciousness call:
// tick must return (no reports, no counter movement) for a device with no
// standing desired doc — the common case for a battery that has never
// received one yet.
func TestBatteryShadow_TickIdleIsSilent(t *testing.T) {
	mreg := metrics.New()
	s := newBatteryShadow("battery-0", reconcile.Config{}, mreg)
	s.tick(time.Now())

	out := mreg.Format()
	if !strings.Contains(out, "lexa_mb_shadow_would_writes_total 0") {
		t.Errorf("idle tick with no desired doc must not would-write, got:\n%s", out)
	}
}

// === TASK-028: active mode ================================================

// recordingDriver captures every control an active shell applies, in order.
type recordingDriver struct {
	applied []model.DERControlBase
	err     error
}

func (d *recordingDriver) Apply(ctrl model.DERControlBase) error {
	if d.err != nil {
		return d.err
	}
	d.applied = append(d.applied, ctrl)
	return nil
}

// fakeGate is a controllable interlockGate for the pure-suppression tests.
type fakeGate struct{ trip map[string]bool }

func (g *fakeGate) isTripped(dev string) bool { return g.trip[dev] }

// battChargeDoc builds a battery desired doc.
func battChargeDoc(setpointW float64, connect *bool, seq uint64, at time.Time) bus.DesiredState {
	d := bus.DesiredState{
		DeviceClass: "battery", DeviceID: "battery-0",
		SetpointW: ptrF(setpointW), Connect: connect,
		Source: "economic", IssuedAt: at.Unix(), Seq: seq,
	}
	return d
}

// lastImpW decodes the charge watts (OpModImpLimW) of a control, NaN if absent.
func lastImpW(ctrl model.DERControlBase) float64 {
	if ctrl.OpModImpLimW == nil {
		return math.NaN()
	}
	return wattsFromActivePower(*ctrl.OpModImpLimW)
}

// TestActiveShell_SetDesiredWritesThroughDriver: a new desired doc is applied to
// hardware through the driver (the same battCommandToControl sign mapping legacy
// used) AND the interlock's charge intent is fed from that doc.
func TestActiveShell_SetDesiredWritesThroughDriver(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingDriver{}
	var noted []bus.BattCommand
	s := newBatteryShell("battery-0", reconcile.Config{}, mreg, modeActive, drv, &fakeGate{trip: map[string]bool{}},
		func(cmd bus.BattCommand) { noted = append(noted, cmd) })

	t0 := time.Now()
	s.setDesired(battChargeDoc(-500, nil, 1, t0), t0)

	if len(drv.applied) != 1 {
		t.Fatalf("expected 1 applied control, got %d", len(drv.applied))
	}
	if w := lastImpW(drv.applied[0]); w != 500 { // −500 charge → OpModImpLimW=500
		t.Errorf("applied charge = %.0f, want OpModImpLimW encoding 500", w)
	}
	if len(noted) != 1 || noted[0].SetpointW == nil || *noted[0].SetpointW != -500 {
		t.Errorf("interlock intent not fed from desired doc: %+v", noted)
	}
	if !strings.Contains(mreg.Format(), "lexa_mb_reconcile_writes_total 1") {
		t.Errorf("expected 1 reconcile write, got:\n%s", mreg.Format())
	}
}

// TestActiveShell_DivergedObserveCorrects: a readback that diverges from desired
// triggers a corrective write through the driver (verify-by-readback, ledger L3).
func TestActiveShell_DivergedObserveCorrects(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingDriver{}
	s := newBatteryShell("battery-0", reconcile.Config{}, mreg, modeActive, drv, &fakeGate{trip: map[string]bool{}},
		func(bus.BattCommand) {})
	t0 := time.Now()
	s.setDesired(battChargeDoc(-500, nil, 1, t0), t0) // SetpointW only → readback assessable

	// Pack is measured discharging 4000 W — nowhere near the −500 charge desire.
	s.observe(device.Measurements{W: 4000, SOC: 50, VA: math.NaN(), Var: math.NaN(), V: math.NaN(), Hz: math.NaN(), PF: math.NaN(), DCV: math.NaN(), DCW: math.NaN(), TmpCab: math.NaN()}, true, t0.Add(time.Second))

	if len(drv.applied) != 2 {
		t.Fatalf("expected new-desired + corrective write (2), got %d", len(drv.applied))
	}
	if !strings.Contains(mreg.Format(), "lexa_mb_shadow_divergences_total 1") {
		t.Errorf("expected 1 divergence recorded, got:\n%s", mreg.Format())
	}
}

// TestActiveShell_ConvergedObserveNoWrite: once the readback matches desired, no
// further write is issued.
func TestActiveShell_ConvergedObserveNoWrite(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingDriver{}
	s := newBatteryShell("battery-0", reconcile.Config{}, mreg, modeActive, drv, &fakeGate{trip: map[string]bool{}},
		func(bus.BattCommand) {})
	t0 := time.Now()
	s.setDesired(battChargeDoc(-500, nil, 1, t0), t0)
	s.observe(device.Measurements{W: -500, SOC: 60, VA: math.NaN(), Var: math.NaN(), V: math.NaN(), Hz: math.NaN(), PF: math.NaN(), DCV: math.NaN(), DCW: math.NaN(), TmpCab: math.NaN()}, true, t0.Add(time.Second))

	if len(drv.applied) != 1 {
		t.Fatalf("expected only the new-desired write (1), got %d", len(drv.applied))
	}
}

// TestActiveShell_InterlockSuppressesConnectRestore: while Tier-0 has the pack
// tripped, a connect-restoring write is suppressed (not applied), reported, and
// counted — the reconciler must never fight the interlock.
func TestActiveShell_InterlockSuppressesConnectRestore(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingDriver{}
	gate := &fakeGate{trip: map[string]bool{"battery-0": true}}
	s := newBatteryShell("battery-0", reconcile.Config{}, mreg, modeActive, drv, gate, func(bus.BattCommand) {})
	t0 := time.Now()
	// Desired opines charge AND connect=true → the write restores connect.
	s.setDesired(battChargeDoc(-500, ptrB(true), 1, t0), t0)

	if len(drv.applied) != 0 {
		t.Fatalf("connect-restore write must be suppressed while tripped, got %d applied", len(drv.applied))
	}
	if !strings.Contains(mreg.Format(), "lexa_mb_interlock_holds_total 1") {
		t.Errorf("expected 1 interlock hold, got:\n%s", mreg.Format())
	}
}

// TestActiveShell_InterlockAllowsSetpointOnlyWrite: a write with no connect
// opinion is NOT a connect-restore and passes through even while tripped (it
// lands as deferred intent on a disconnected pack).
func TestActiveShell_InterlockAllowsSetpointOnlyWrite(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingDriver{}
	gate := &fakeGate{trip: map[string]bool{"battery-0": true}}
	s := newBatteryShell("battery-0", reconcile.Config{}, mreg, modeActive, drv, gate, func(bus.BattCommand) {})
	t0 := time.Now()
	s.setDesired(battChargeDoc(-500, nil, 1, t0), t0) // no Connect opinion

	if len(drv.applied) != 1 {
		t.Fatalf("setpoint-only write must pass through while tripped, got %d applied", len(drv.applied))
	}
	if !strings.Contains(mreg.Format(), "lexa_mb_interlock_holds_total 0") {
		t.Errorf("setpoint-only write must not count as an interlock hold, got:\n%s", mreg.Format())
	}
}

// TestActiveShell_InterlockSeniorityIntegration exercises the real Tier-0
// interlock against an active shell: charge is applied, a sign-inversion trip
// force-disconnects the pack, the reconciler's reconnect-reassert (which would
// restore connect) is HELD while tripped (no oscillation), and once the fault
// clears the reassert resumes. This is the guard-versus-guard case the program
// exists to kill (ledger L8).
func TestActiveShell_InterlockSeniorityIntegration(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingDriver{}
	ilApplier := &fakeApplier{} // the interlock's own disconnect sink
	il := newBatterySafetyInterlock(ilApplier, &Config{Devices: []DeviceConfig{{Name: "battery-0", Role: "battery"}}})
	s := newBatteryShell("battery-0", reconcile.Config{}, mreg, modeActive, drv, il,
		func(cmd bus.BattCommand) { il.noteControl("battery-0", cmd) })

	t0 := time.Now()
	// Hub commands charge (with connect). Not tripped yet → applied, and the
	// interlock's charge intent is now set from this desired doc.
	s.setDesired(battChargeDoc(-3000, ptrB(true), 1, t0), t0)
	if len(drv.applied) != 1 {
		t.Fatalf("initial charge should apply, got %d", len(drv.applied))
	}

	// Pack inverts its setpoint: discharging near reserve while charge-commanded.
	if !il.check("battery-0", device.Measurements{W: 4800, SOC: 12}) {
		t.Fatal("interlock should have tripped on sign inversion at reserve")
	}
	if !il.isTripped("battery-0") {
		t.Fatal("interlock should report tripped")
	}

	// A reconnect now would reassert charge+connect — must be HELD, not applied.
	s.markReconnected()
	s.observe(device.Measurements{W: 4800, SOC: 12, VA: math.NaN(), Var: math.NaN(), V: math.NaN(), Hz: math.NaN(), PF: math.NaN(), DCV: math.NaN(), DCW: math.NaN(), TmpCab: math.NaN()}, true, t0.Add(2*time.Second))
	if len(drv.applied) != 1 {
		t.Fatalf("reconnect-reassert must be suppressed while tripped, applied=%d", len(drv.applied))
	}
	if !strings.Contains(mreg.Format(), "lexa_mb_interlock_holds_total 1") {
		t.Errorf("expected 1 interlock hold during trip, got:\n%s", mreg.Format())
	}

	// Fault clears: the pack is now charging correctly, so the interlock releases.
	if il.check("battery-0", device.Measurements{W: -3000, SOC: 40}) {
		t.Fatal("interlock should not trip on a correctly charging pack")
	}
	if il.isTripped("battery-0") {
		t.Fatal("interlock should have cleared its trip once the fault cleared")
	}
	// Reconnect again: with the trip cleared, the reassert now goes through.
	s.markReconnected()
	s.observe(device.Measurements{W: -3000, SOC: 40, VA: math.NaN(), Var: math.NaN(), V: math.NaN(), Hz: math.NaN(), PF: math.NaN(), DCV: math.NaN(), DCW: math.NaN(), TmpCab: math.NaN()}, true, t0.Add(4*time.Second))
	if len(drv.applied) != 2 {
		t.Fatalf("reassert should resume once the interlock cleared, applied=%d", len(drv.applied))
	}
}

// TestActiveShell_ReconnectReasserts: a reconnect signal makes the reconciler
// reassert the standing desired before the post-reconnect readback is trusted
// (ledger L4), and the signal is consumed (a second observe does not re-fire).
func TestActiveShell_ReconnectReasserts(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingDriver{}
	s := newBatteryShell("battery-0", reconcile.Config{}, mreg, modeActive, drv, &fakeGate{trip: map[string]bool{}},
		func(bus.BattCommand) {})
	t0 := time.Now()
	s.setDesired(battChargeDoc(-500, nil, 1, t0), t0) // applied #1

	s.markReconnected()
	s.observe(device.Measurements{W: -500, SOC: 60, VA: math.NaN(), Var: math.NaN(), V: math.NaN(), Hz: math.NaN(), PF: math.NaN(), DCV: math.NaN(), DCW: math.NaN(), TmpCab: math.NaN()}, true, t0.Add(time.Second))
	if len(drv.applied) != 2 {
		t.Fatalf("reconnect must reassert desired (applied=2), got %d", len(drv.applied))
	}
	// Second observe: no pending reconnect, converged → no further write.
	s.observe(device.Measurements{W: -500, SOC: 60, VA: math.NaN(), Var: math.NaN(), V: math.NaN(), Hz: math.NaN(), PF: math.NaN(), DCV: math.NaN(), DCW: math.NaN(), TmpCab: math.NaN()}, true, t0.Add(2*time.Second))
	if len(drv.applied) != 2 {
		t.Fatalf("reconnect signal must be consumed once, applied=%d", len(drv.applied))
	}
}

// TestActiveShell_ShadowUnaffected: a shadow shell still records verdicts and
// NEVER touches a driver, even though the shell type now carries one — the mode
// gate, not the type, is what withholds writes.
func TestActiveShell_ShadowUnaffected(t *testing.T) {
	mreg := metrics.New()
	s := newBatteryShadow("battery-0", reconcile.Config{}, mreg)
	if s.active() {
		t.Fatal("newBatteryShadow must build a shadow-mode shell")
	}
	if s.driver != nil || s.interlock != nil || s.note != nil {
		t.Fatal("shadow shell must have no driver/interlock/note")
	}
	t0 := time.Now()
	s.setDesired(battChargeDoc(-500, nil, 1, t0), t0)
	s.observe(device.Measurements{W: 4000, SOC: 50, VA: math.NaN(), Var: math.NaN(), V: math.NaN(), Hz: math.NaN(), PF: math.NaN(), DCV: math.NaN(), DCW: math.NaN(), TmpCab: math.NaN()}, true, t0.Add(40*time.Second))
	if !strings.Contains(mreg.Format(), "lexa_mb_reconcile_writes_total 0") {
		t.Errorf("shadow must never apply a reconcile write, got:\n%s", mreg.Format())
	}
}
