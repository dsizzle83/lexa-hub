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

// solarMeas builds a device.Measurements carrying only W (all other fields NaN),
// as an inverter poll yields for the reconciler's purposes.
func solarMeas(w float64) device.Measurements {
	return device.Measurements{
		W: w, SOC: math.NaN(), VA: math.NaN(), Var: math.NaN(), V: math.NaN(),
		Hz: math.NaN(), PF: math.NaN(), DCV: math.NaN(), DCW: math.NaN(), TmpCab: math.NaN(),
	}
}

// solarCapDoc builds a solar desired doc with an explicit ceiling.
func solarCapDoc(ceilingW float64, seq uint64, at time.Time) bus.DesiredState {
	c := ceilingW
	return bus.DesiredState{
		DeviceClass: bus.DesiredClassSolar, DeviceID: "inverter-0",
		CeilingW: &c, Source: "economic", IssuedAt: at.Unix(), Seq: seq,
	}
}

// lastMaxW decodes the ceiling watts (OpModMaxLimW) of a control, NaN if absent.
func lastMaxW(ctrl model.DERControlBase) float64 {
	if ctrl.OpModMaxLimW == nil {
		return math.NaN()
	}
	return wattsFromActivePower(*ctrl.OpModMaxLimW)
}

// TestSolarShadow_UnderCeilingIsMatch is THE solar semantic: an inverter
// producing LESS than its ceiling (dusk / clouds) is COMPLIANT — one-sided. It
// must count as a match, never a divergence, and never a would-write.
func TestSolarShadow_UnderCeilingIsMatch(t *testing.T) {
	mreg := metrics.New()
	s := newSolarShadow("inverter-0", reconcile.Config{}, mreg)
	t0 := time.Now()
	s.setDesired(solarCapDoc(3000, 1, t0), t0)

	// Producing 1000 W under a 3000 W cap — well under the ceiling.
	s.observe(solarMeas(1000), true, t0.Add(time.Second))

	out := mreg.Format()
	if !strings.Contains(out, "lexa_mb_shadow_matches_total 1") {
		t.Errorf("under-ceiling must be a match, got:\n%s", out)
	}
	if !strings.Contains(out, "lexa_mb_shadow_divergences_total 0") {
		t.Errorf("under-ceiling must never be a divergence, got:\n%s", out)
	}
	// Only SetDesired's own new-desired would-write; the under-ceiling observe
	// must not itself produce one.
	if !strings.Contains(out, "lexa_mb_shadow_would_writes_total 1") {
		t.Errorf("under-ceiling observe must not would-write, got:\n%s", out)
	}
}

// TestSolarShadow_OverCeilingDiverges: an inverter producing ABOVE its ceiling
// beyond tolerance is a genuine divergence (the cap is being violated).
func TestSolarShadow_OverCeilingDiverges(t *testing.T) {
	mreg := metrics.New()
	s := newSolarShadow("inverter-0", reconcile.Config{ConvergeTimeout: time.Second}, mreg)
	t0 := time.Now()
	s.setDesired(solarCapDoc(3000, 1, t0), t0)

	// 4500 W against a 3000 W cap — over the ceiling, spaced past the longest
	// backoff so each observe writes.
	for i := 0; i < 3; i++ {
		s.observe(solarMeas(4500), true, t0.Add(time.Duration(i+1)*40*time.Second))
	}

	out := mreg.Format()
	if !strings.Contains(out, "lexa_mb_shadow_divergences_total 3") {
		t.Errorf("over-ceiling must count as divergence, got:\n%s", out)
	}
	if !strings.Contains(out, "lexa_mb_shadow_matches_total 0") {
		t.Errorf("over-ceiling must not count as a match, got:\n%s", out)
	}
}

// TestSolarShadow_NeverWrites: a shadow shell has no driver; driven maximally
// divergent it records forever and never routes a write.
func TestSolarShadow_NeverWrites(t *testing.T) {
	mreg := metrics.New()
	s := newSolarShadow("inverter-0", reconcile.Config{}, mreg)
	if s.active() || s.driver != nil {
		t.Fatal("newSolarShadow must build a shadow shell with no driver")
	}
	t0 := time.Now()
	s.setDesired(solarCapDoc(2000, 1, t0), t0)
	for i := 0; i < 10; i++ {
		s.observe(solarMeas(4800), true, t0.Add(time.Duration(i+1)*40*time.Second))
	}
	if !strings.Contains(mreg.Format(), "lexa_mb_reconcile_writes_total 0") {
		t.Errorf("shadow must never apply a reconcile write, got:\n%s", mreg.Format())
	}
}

// TestSolarActive_NewCeilingWrites: a new cap doc is applied through the driver
// as an OpModMaxLimW encoding the ceiling (the same activePowerFromWatts scaling
// legacy solar used).
func TestSolarActive_NewCeilingWrites(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingDriver{}
	s := newSolarShell("inverter-0", reconcile.Config{}, mreg, modeActive, drv)
	t0 := time.Now()
	s.setDesired(solarCapDoc(3000, 1, t0), t0)

	if len(drv.applied) != 1 {
		t.Fatalf("expected 1 applied control, got %d", len(drv.applied))
	}
	if w := lastMaxW(drv.applied[0]); math.Abs(w-3000) > 0.5 {
		t.Errorf("applied ceiling = %.0f, want OpModMaxLimW encoding 3000", w)
	}
}

// TestSolarActive_RestoreIsExplicitWrite: restore (CeilingW == RestoreCeilingW)
// is a WRITE of the restore ceiling, not an absence (ledger L1/L7).
func TestSolarActive_RestoreIsExplicitWrite(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingDriver{}
	s := newSolarShell("inverter-0", reconcile.Config{}, mreg, modeActive, drv)
	t0 := time.Now()
	// First a cap, then release to restore.
	s.setDesired(solarCapDoc(3000, 1, t0), t0)
	s.setDesired(solarCapDoc(bus.RestoreCeilingW, 2, t0.Add(time.Second)), t0.Add(time.Second))

	if len(drv.applied) != 2 {
		t.Fatalf("cap then restore should be 2 writes, got %d", len(drv.applied))
	}
	if w := lastMaxW(drv.applied[1]); w < 1e8 {
		t.Errorf("restore write must carry a huge ceiling, got %.0f", w)
	}
}

// TestSolarActive_UnderCeilingNoCorrectiveWrite: once producing at or under the
// cap, no corrective write is issued (one-sided convergence).
func TestSolarActive_UnderCeilingNoCorrectiveWrite(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingDriver{}
	s := newSolarShell("inverter-0", reconcile.Config{}, mreg, modeActive, drv)
	t0 := time.Now()
	s.setDesired(solarCapDoc(3000, 1, t0), t0) // applied #1
	s.observe(solarMeas(2500), true, t0.Add(time.Second))
	s.observe(solarMeas(500), true, t0.Add(2*time.Second)) // dusk, well under
	if len(drv.applied) != 1 {
		t.Fatalf("under-ceiling observes must not write, applied=%d", len(drv.applied))
	}
}

// TestSolarActive_OverCeilingCorrects: an over-ceiling reading triggers a
// corrective write of the cap.
func TestSolarActive_OverCeilingCorrects(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingDriver{}
	s := newSolarShell("inverter-0", reconcile.Config{}, mreg, modeActive, drv)
	t0 := time.Now()
	s.setDesired(solarCapDoc(3000, 1, t0), t0) // applied #1
	s.observe(solarMeas(4500), true, t0.Add(time.Second))
	if len(drv.applied) != 2 {
		t.Fatalf("over-ceiling must correct, applied=%d", len(drv.applied))
	}
	if w := lastMaxW(drv.applied[1]); math.Abs(w-3000) > 0.5 {
		t.Errorf("corrective write must reassert the cap 3000, got %.0f", w)
	}
}

// TestSolarActive_SeedThenReconnectReassertsRestore: the initial-desired seed
// installs the restore ceiling silently (no write at seed time); a reconnect
// then reasserts restore (Background case 3 mirrors reassertLocked). This is the
// never-commanded-inverter stale-ceiling clear.
func TestSolarActive_SeedThenReconnectReassertsRestore(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingDriver{}
	s := newSolarShell("inverter-0", reconcile.Config{}, mreg, modeActive, drv)
	t0 := time.Now()
	s.seedRestoreCeiling(t0)
	if len(drv.applied) != 0 {
		t.Fatalf("seed must NOT write at startup (reassertLocked fires on reconnect), applied=%d", len(drv.applied))
	}
	// Reconnect: reassert the standing restore ceiling before trusting readback.
	s.markReconnected()
	s.observe(solarMeas(3000), true, t0.Add(time.Second))
	if len(drv.applied) != 1 {
		t.Fatalf("reconnect must reassert the seeded restore ceiling, applied=%d", len(drv.applied))
	}
	if w := lastMaxW(drv.applied[0]); w < 1e8 {
		t.Errorf("reconnect reassert must be the restore ceiling, got %.0f", w)
	}
}

// TestSolarActive_ReconnectUnderCapReassertsCap: a dark inverter reconnecting
// while a cap is the standing desired reasserts the CAP (never full output
// mid-cap) — the restore-while-dark contract (release-while-rebooting).
func TestSolarActive_ReconnectUnderCapReassertsCap(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingDriver{}
	s := newSolarShell("inverter-0", reconcile.Config{}, mreg, modeActive, drv)
	t0 := time.Now()
	s.setDesired(solarCapDoc(2000, 1, t0), t0) // applied #1: cap
	s.markReconnected()
	s.observe(solarMeas(0), true, t0.Add(time.Second)) // dark→back, producing 0
	if len(drv.applied) != 2 {
		t.Fatalf("reconnect under a cap must reassert the cap, applied=%d", len(drv.applied))
	}
	if w := lastMaxW(drv.applied[1]); math.Abs(w-2000) > 0.5 {
		t.Errorf("reconnect must reassert the CAP (2000), not full output, got %.0f", w)
	}
}

// TestSolarActive_ReconnectAfterReleaseDeliversRestore: a cap released while the
// inverter was dark converges to FULL output on reconnect (the other half of the
// restore-while-dark contract).
func TestSolarActive_ReconnectAfterReleaseDeliversRestore(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingDriver{}
	s := newSolarShell("inverter-0", reconcile.Config{}, mreg, modeActive, drv)
	t0 := time.Now()
	s.setDesired(solarCapDoc(2000, 1, t0), t0)                                                  // cap
	s.setDesired(solarCapDoc(bus.RestoreCeilingW, 2, t0.Add(time.Second)), t0.Add(time.Second)) // release (device dark)
	before := len(drv.applied)
	s.markReconnected()
	s.observe(solarMeas(4800), true, t0.Add(2*time.Second)) // full output on reconnect
	if len(drv.applied) != before+1 {
		t.Fatalf("reconnect after release must deliver restore, applied delta=%d", len(drv.applied)-before)
	}
	if w := lastMaxW(drv.applied[len(drv.applied)-1]); w < 1e8 {
		t.Errorf("post-release reconnect must converge to restore, got %.0f", w)
	}
}

// TestSolarActive_ImplausibleHeld: an implausible reading (solar-bad-scale,
// ledger L9) is evidence of nothing — no write, no match, no divergence.
func TestSolarActive_ImplausibleHeld(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingDriver{}
	s := newSolarShell("inverter-0", reconcile.Config{}, mreg, modeActive, drv)
	t0 := time.Now()
	s.setDesired(solarCapDoc(3000, 1, t0), t0) // applied #1
	s.observe(solarMeas(48000), false /* implausible */, t0.Add(time.Second))
	if len(drv.applied) != 1 {
		t.Fatalf("implausible reading must not provoke a write, applied=%d", len(drv.applied))
	}
	out := mreg.Format()
	if !strings.Contains(out, "lexa_mb_shadow_matches_total 0") || !strings.Contains(out, "lexa_mb_shadow_divergences_total 0") {
		t.Errorf("implausible reading must be neither match nor divergence, got:\n%s", out)
	}
}

// TestSolarFieldsToControl pins the fields→OpModMaxLimW mapping (byte-identical
// to solarCommandToControl's non-nil branch).
func TestSolarFieldsToControl(t *testing.T) {
	ctrl := solarFieldsToControl(map[reconcile.Field]float64{reconcile.CeilingW: 3000})
	if ctrl.OpModMaxLimW == nil {
		t.Fatal("expected OpModMaxLimW set")
	}
	if w := wattsFromActivePower(*ctrl.OpModMaxLimW); math.Abs(w-3000) > 0.5 {
		t.Errorf("ceiling round-trip = %.0f, want 3000", w)
	}
	// Compare to the legacy encoding for the same watts.
	legacy := solarCommandToControl(bus.SolarCommand{CurtailToW: ptrF(3000)})
	if wattsFromActivePower(*legacy.OpModMaxLimW) != wattsFromActivePower(*ctrl.OpModMaxLimW) {
		t.Errorf("fields mapping must match legacy solarCommandToControl encoding")
	}
	// Unit 6.2 upgrade guard: a Connect-free Fields map (today's shape, and
	// every optimizer-mode write) must leave OpModConnect nil.
	if ctrl.OpModConnect != nil {
		t.Errorf("absent Connect must leave OpModConnect nil, got %v", *ctrl.OpModConnect)
	}
}

// solarConnectDoc builds a solar desired doc carrying an explicit Connect
// opinion alongside a ceiling (Unit 6.2 gateway cease-to-energize fan-out).
func solarConnectDoc(ceilingW float64, connect bool, seq uint64, at time.Time) bus.DesiredState {
	d := solarCapDoc(ceilingW, seq, at)
	c := connect
	d.Connect = &c
	return d
}

// TestSolarFieldsToControl_Connect pins Connect materializing as a REAL
// OpModConnect write (Unit 6.2), alongside — never instead of — CeilingW.
func TestSolarFieldsToControl_Connect(t *testing.T) {
	ctrl := solarFieldsToControl(map[reconcile.Field]float64{reconcile.CeilingW: 3000, reconcile.Connect: 0})
	if ctrl.OpModConnect == nil || *ctrl.OpModConnect != false {
		t.Fatalf("expected OpModConnect=false, got %v", ctrl.OpModConnect)
	}
	if w := wattsFromActivePower(*ctrl.OpModMaxLimW); math.Abs(w-3000) > 0.5 {
		t.Errorf("ceiling must still be carried alongside Connect, got %.0f", w)
	}

	ctrl2 := solarFieldsToControl(map[reconcile.Field]float64{reconcile.CeilingW: 3000, reconcile.Connect: 1})
	if ctrl2.OpModConnect == nil || *ctrl2.OpModConnect != true {
		t.Fatalf("expected OpModConnect=true, got %v", ctrl2.OpModConnect)
	}
}

// TestSolarActive_ConnectDemandWritesOpModConnect: a desired doc expressing
// Connect=false alongside a ceiling drives a SINGLE write carrying BOTH
// OpModMaxLimW and OpModConnect — the gateway cease-to-energize execution
// this unit closes for solar.
func TestSolarActive_ConnectDemandWritesOpModConnect(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingDriver{}
	s := newSolarShell("inverter-0", reconcile.Config{}, mreg, modeActive, drv)
	t0 := time.Now()
	s.setDesired(solarConnectDoc(3000, false, 1, t0), t0)

	if len(drv.applied) != 1 {
		t.Fatalf("expected 1 applied control, got %d", len(drv.applied))
	}
	ctrl := drv.applied[0]
	if ctrl.OpModConnect == nil || *ctrl.OpModConnect != false {
		t.Fatalf("expected OpModConnect=false in the write, got %v", ctrl.OpModConnect)
	}
	if w := lastMaxW(ctrl); math.Abs(w-3000) > 0.5 {
		t.Errorf("ceiling must still be carried alongside Connect, got %.0f", w)
	}
}

// TestSolarActive_ConnectReconnectRestoresTrue: Connect flipping back to true
// in a later doc still materializes as an explicit OpModConnect=true write
// (never merely an absence).
func TestSolarActive_ConnectReconnectRestoresTrue(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingDriver{}
	s := newSolarShell("inverter-0", reconcile.Config{}, mreg, modeActive, drv)
	t0 := time.Now()
	s.setDesired(solarConnectDoc(3000, false, 1, t0), t0)
	s.setDesired(solarConnectDoc(3000, true, 2, t0.Add(time.Second)), t0.Add(time.Second))

	if len(drv.applied) != 2 {
		t.Fatalf("expected 2 applied controls, got %d", len(drv.applied))
	}
	ctrl := drv.applied[1]
	if ctrl.OpModConnect == nil || *ctrl.OpModConnect != true {
		t.Fatalf("expected OpModConnect=true on reconnect, got %v", ctrl.OpModConnect)
	}
}

// TestSolarActive_ConnectExpressedHoldsCompleteness documents the accepted,
// TASK-027-style limitation this unit inherits rather than works around: an
// inverter has no register this shell can read connect/energize state back
// from (confirmed by inspection — see the file doc), so once a doc opines on
// Connect, internal/reconcile's completeness gate holds FOREVER for that
// device: no match, no divergence, no corrective write out of Observe, no
// matter how far over-ceiling the readback is. This is NOT a bug — it must
// simply never be misreported as a match — and the write path itself
// (SetDesired/Reconnected/Tick) is entirely unaffected by it.
func TestSolarActive_ConnectExpressedHoldsCompleteness(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingDriver{}
	s := newSolarShell("inverter-0", reconcile.Config{}, mreg, modeActive, drv)
	t0 := time.Now()
	s.setDesired(solarConnectDoc(3000, false, 1, t0), t0) // applied #1: new target
	for i := 0; i < 5; i++ {
		s.observe(solarMeas(4500), true, t0.Add(time.Duration(i+1)*40*time.Second)) // wildly over ceiling
	}

	out := mreg.Format()
	if !strings.Contains(out, "lexa_mb_shadow_matches_total 0") {
		t.Errorf("Connect-expressed doc must never count a match, got:\n%s", out)
	}
	if !strings.Contains(out, "lexa_mb_shadow_divergences_total 0") {
		t.Errorf("Connect-expressed doc must never count a divergence via Observe, got:\n%s", out)
	}
	if len(drv.applied) != 1 {
		t.Fatalf("incomplete (Connect-expressed) reads must never write via Observe, applied=%d", len(drv.applied))
	}
}
