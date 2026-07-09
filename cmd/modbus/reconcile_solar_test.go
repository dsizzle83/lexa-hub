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

// -------------------------------------------------------------------------
// WS-2 fix 2: seedRestoreCeiling fail-closed (reseedFromStaleDoc)
// -------------------------------------------------------------------------

// TestSolarActive_StaleFirstDocReseedsCapNotRestore is WS-2 fix 2's core
// acceptance criterion: lexa-modbus restarts (seedRestoreCeiling seeds
// restore, silently), and the FIRST real hub doc this process ever sees is
// too stale to adopt (the hub-down-during-consumer-restart scenario) — the
// standing intent must become the stale doc's CAP, not stay at the seeded
// restore, and a subsequent reconnect must reassert the CAP.
func TestSolarActive_StaleFirstDocReseedsCapNotRestore(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingDriver{}
	s := newSolarShell("inverter-0", reconcile.Config{}, mreg, modeActive, drv)
	t0 := time.Now()

	s.seedRestoreCeiling(t0)
	if len(drv.applied) != 0 {
		t.Fatalf("seed must not write at startup, applied=%d", len(drv.applied))
	}

	// A stale cap doc arrives (the hub's last real command before it went
	// down, delivered as the MQTT retained message on subscribe) — 400s old,
	// past the 300s StaleAfter bound, so the reconciler core rejects it.
	staleCap := solarCapDoc(1800, 7, t0.Add(-400*time.Second))
	s.setDesired(staleCap, t0)
	if len(drv.applied) != 0 {
		t.Fatalf("a rejected-stale doc must never itself write to hardware, applied=%d", len(drv.applied))
	}
	if !s.standingIsSeed {
		t.Fatal("re-seeding from a stale doc is still a seed, not a genuinely adopted doc")
	}
	if !s.haveDesiredCeiling || s.desiredCeilingW != 1800 {
		t.Fatalf("desiredCeilingW = %v (have=%v), want 1800 (the stale doc's cap, fail-closed over the restore seed)", s.desiredCeilingW, s.haveDesiredCeiling)
	}

	// Reconnect must reassert the CAP (1800), never the restore ceiling —
	// this is the WS-2 fail-open this fix closes: without it, the reconnect
	// reassert below would deliver full output over an inverter the hub last
	// told to curtail to 1800 W.
	s.markReconnected()
	s.observe(solarMeas(0), true, t0.Add(time.Second))
	if len(drv.applied) != 1 {
		t.Fatalf("reconnect must reassert the re-seeded cap, applied=%d", len(drv.applied))
	}
	if w := lastMaxW(drv.applied[0]); math.Abs(w-1800) > 0.5 {
		t.Errorf("reconnect reassert = %.0f, want the fail-closed cap 1800 (NOT restore)", w)
	}
}

// TestSolarActive_NeverCommandedInverterStillSeedsRestore is WS-2 fix 2's
// other half, pinned explicitly: an inverter with NO hub doc at all (no
// stale doc ever arrives — genuinely never-commanded) must still seed and
// reassert restore, exactly as before this fix. (Also covered end-to-end by
// TestSolarActive_SeedThenReconnectReassertsRestore; this test isolates the
// standingIsSeed bookkeeping the WS-2 fix added.)
func TestSolarActive_NeverCommandedInverterStillSeedsRestore(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingDriver{}
	s := newSolarShell("inverter-0", reconcile.Config{}, mreg, modeActive, drv)
	t0 := time.Now()

	s.seedRestoreCeiling(t0)
	if !s.standingIsSeed || s.desiredCeilingW != restoreCeilingW {
		t.Fatalf("fresh seed must be restore, got desiredCeilingW=%v standingIsSeed=%v", s.desiredCeilingW, s.standingIsSeed)
	}

	s.markReconnected()
	s.observe(solarMeas(3000), true, t0.Add(time.Second))
	if len(drv.applied) != 1 {
		t.Fatalf("reconnect must reassert the seeded restore ceiling, applied=%d", len(drv.applied))
	}
	if w := lastMaxW(drv.applied[0]); w < 1e8 {
		t.Errorf("never-commanded inverter's reconnect reassert = %.0f, want restore", w)
	}
}

// TestSolarActive_ReseedOnlyAppliesBeforeARealDocIsAccepted verifies the
// fail-closed re-seed is a ONE-TIME, startup-only correction: once a real
// doc has been genuinely accepted (standingIsSeed cleared), a LATER
// stale/replay doc must be ignored exactly as it always was — it must NOT
// perturb an already-adopted standing intent, even though it carries a
// CeilingW.
func TestSolarActive_ReseedOnlyAppliesBeforeARealDocIsAccepted(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingDriver{}
	s := newSolarShell("inverter-0", reconcile.Config{}, mreg, modeActive, drv)
	t0 := time.Now()

	// A real, fresh doc is genuinely adopted first (no seed involved).
	s.setDesired(solarCapDoc(2500, 5, t0), t0)
	if s.standingIsSeed {
		t.Fatal("a genuinely accepted doc must clear standingIsSeed")
	}
	if len(drv.applied) != 1 {
		t.Fatalf("expected the real doc to be applied, applied=%d", len(drv.applied))
	}
	if w := lastMaxW(drv.applied[0]); math.Abs(w-2500) > 0.5 {
		t.Fatalf("expected the real doc to be applied as 2500, got %.0f", w)
	}

	// A stale, older doc later shows up (e.g. a stray retained redelivery) —
	// must be rejected and must NOT move the already-adopted 2500 standing
	// intent, exactly as the pre-WS-2 rejected-docs-never-move-desired rule
	// requires.
	stale := solarCapDoc(999, 1, t0.Add(-400*time.Second))
	s.setDesired(stale, t0)
	if s.desiredCeilingW != 2500 {
		t.Fatalf("desiredCeilingW = %v, want unchanged 2500 (a stale doc after real adoption must never move it)", s.desiredCeilingW)
	}
	if len(drv.applied) != 1 {
		t.Fatalf("a rejected stale doc must not write, applied=%d", len(drv.applied))
	}
}

// TestSolarActive_ReseedIgnoresNonStaleRejections verifies staleRejected
// only fires the WS-2 re-seed for RejectStale — a NaN doc rejection while
// still on the startup seed must be ignored exactly as before (no signal
// worth re-seeding from), not mistaken for a stale-but-informative doc.
func TestSolarActive_ReseedIgnoresNonStaleRejections(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingDriver{}
	s := newSolarShell("inverter-0", reconcile.Config{}, mreg, modeActive, drv)
	t0 := time.Now()
	s.seedRestoreCeiling(t0)

	nan := math.NaN()
	nanDoc := bus.DesiredState{
		DeviceClass: bus.DesiredClassSolar, DeviceID: "inverter-0",
		CeilingW: &nan, Source: "economic", IssuedAt: t0.Unix(), Seq: 5,
	}
	s.setDesired(nanDoc, t0)
	if !s.standingIsSeed || s.desiredCeilingW != restoreCeilingW {
		t.Fatalf("a NaN-rejected doc must not perturb the restore seed, got desiredCeilingW=%v standingIsSeed=%v", s.desiredCeilingW, s.standingIsSeed)
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
}
