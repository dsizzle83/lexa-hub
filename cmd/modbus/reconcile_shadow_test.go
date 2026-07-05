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
