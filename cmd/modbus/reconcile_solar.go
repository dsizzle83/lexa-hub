// TASK-029: solar/inverter reconciler shell for lexa-modbus.
//
// solarShell drives one internal/reconcile.Reconciler per reconciled inverter
// device, in shadow or active mode, off the same real data streams as the
// battery shell — the hub's retained lexa/desired/solar/{device} doc, this
// process's own poll readbacks, the legacy solar command path's writes (shadow
// only), and (active only) the retryDevice reconnect signal. It differs from
// batteryShell in three ways that are the whole reason solar took three QA
// rounds to get right:
//
//   - ONE-SIDED divergence. An inverter legitimately produces LESS than its
//     ceiling (dusk, clouds); only OVER-ceiling generation is divergence. The
//     reconcile core is two-sided, so the shell synthesizes the readback it
//     feeds the core: measured ≤ ceiling+tolerance ⇒ report the ceiling exactly
//     (converged); measured > ceiling+tolerance ⇒ report the measured value
//     (diverged). Under-ceiling is never a write and never counted a divergence.
//
//   - RESTORE IS AN EXPLICIT WRITE, not an absence (ledger L1/L7). The hub's
//     desired doc carries CeilingW = bus.RestoreCeilingW on release; the
//     reconciler writes that ceiling (the device clamps it to WMax → 100%),
//     exactly reproducing restoreOnGenLimitClear. The retained, connectivity-
//     independent doc keeps the cap value while a cap is active even if the
//     inverter is dark, and the reconciler reasserts it on reconnect — so the
//     optimizer's solarCapActive dark-inverter gate (restore-while-dark, Mode B
//     / release-while-rebooting) needs no publisher equivalent.
//
//   - INITIAL DESIRED = restore ceiling (Background case 3). An inverter-class
//     reconciler with no doc yet defaults its standing desired to the restore
//     ceiling, mirroring reassertLocked's never-commanded-inverter branch: a
//     ceiling latched before this process started (or released while the device
//     was dark) is cleared on the first reconnect instead of persisting forever.
//     The seed is silent (its new-desired write is dropped) because
//     reassertLocked fires on RECONNECT, not startup; Reconnected() reasserts it.
//
// There is no Tier-0 interlock for solar (batteries have one, inverters do not),
// so an active solarShell has no interlockGate and every reconciler Write is
// applied unconditionally through the SAME registry path legacy solar used
// (registry.ApplyControlTo → retryDevice.ApplyControl, via solarFieldsToControl
// which is byte-identical to solarCommandToControl's non-nil branch).
package main

import (
	"fmt"
	"log"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/metrics"
	"lexa-hub/internal/reconcile"
	"lexa-hub/internal/southbound/device"
	model "lexa-proto/csipmodel"
)

// solarShell is the per-device inverter reconciler shell. Like batteryShell it
// locks mu on every entry point (feeds arrive on several goroutines).
type solarShell struct {
	mu     sync.Mutex
	device string
	mode   shellMode
	r      *reconcile.Reconciler

	// driver is nil in shadow mode (a recorder has no driver). In active mode it
	// is the SAME registryDriver the battery shell uses — solar has no interlock,
	// so there is no interlockGate/note collaborator here.
	driver reconcileDriver

	// desiredCeilingW is the standing ceiling the shell compares readbacks
	// against, one-sided. Updated only when the core ACCEPTS a doc (or by the
	// initial-desired seed) so a rejected/stale doc never moves it.
	desiredCeilingW    float64
	haveDesiredCeiling bool

	// reconnectPending: set by retryDevice's onReconnect callback (active only)
	// without taking mu; consumed by observe. Same lock-order discipline as
	// batteryShell.reconnectPending.
	reconnectPending atomic.Bool

	// Shadow legacy-write bookkeeping (the shadow verdict compares against it).
	haveLegacy bool
	legacyDesc string

	haveReadback bool
	lastW        float64

	// Shared reconciler counters (idempotent by name across shells; the log line
	// carries the device name + class tag for per-class triage).
	matches       *metrics.Counter
	divergences   *metrics.Counter
	wouldWrites   *metrics.Counter
	writes        *metrics.Counter
	writeFailures *metrics.Counter

	// pub forwards convergence-state reports (NonConvergedBegin/End) to MQTT for
	// the hub's breach-episode component (TASK-031); nil in shadow mode / tests.
	pub func(reconcile.Report)
}

// newSolarShadow builds a SHADOW-mode inverter shell (recorder; no driver).
func newSolarShadow(deviceName string, cfg reconcile.Config, mreg *metrics.Registry) *solarShell {
	return newSolarShell(deviceName, cfg, mreg, modeShadow, nil)
}

// newSolarShell builds an inverter shell in the given mode. In shadow mode
// driver MUST be nil; in active mode it MUST be non-nil.
func newSolarShell(deviceName string, cfg reconcile.Config, mreg *metrics.Registry, mode shellMode, driver reconcileDriver) *solarShell {
	return &solarShell{
		device:        deviceName,
		mode:          mode,
		r:             reconcile.New(bus.DesiredClassSolar, deviceName, cfg),
		driver:        driver,
		matches:       mreg.Counter("lexa_mb_shadow_matches_total"),
		divergences:   mreg.Counter("lexa_mb_shadow_divergences_total"),
		wouldWrites:   mreg.Counter("lexa_mb_shadow_would_writes_total"),
		writes:        mreg.Counter("lexa_mb_reconcile_writes_total"),
		writeFailures: mreg.Counter("lexa_mb_reconcile_write_failures_total"),
	}
}

func (s *solarShell) active() bool { return s.mode == modeActive }

// seedRestoreCeiling installs the restore ceiling as the initial standing
// desired for an active inverter with no hub doc yet (Background case 3). The
// seed's new-desired write is DROPPED: reassertLocked's inverter branch fires on
// reconnect, not startup, and the reconciler's Reconnected() reasserts this
// standing desired on the first reconnect. Once a real (retained) hub doc
// arrives on subscribe it supersedes the seed. IssuedAt is a second in the past
// so the hub's first doc (Seq 0, same publisher epoch) is strictly newer and
// wins the AD-013 gate rather than colliding.
func (s *solarShell) seedRestoreCeiling(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ceiling := restoreCeilingW
	doc := bus.DesiredState{
		Envelope:    bus.Envelope{V: bus.DesiredStateV},
		DeviceClass: bus.DesiredClassSolar,
		DeviceID:    s.device,
		CeilingW:    &ceiling,
		Source:      "safety",
		IssuedAt:    now.Add(-time.Second).Unix(),
		Seq:         0,
	}
	// Adopt as standing intent; deliberately drop the returned new-desired write.
	_, _ = s.r.SetDesired(doc, now)
	s.desiredCeilingW = ceiling
	s.haveDesiredCeiling = true
	log.Printf("lexa-modbus: reconciler[%s] %s: seeded initial desired CeilingW=restore (no hub doc yet)", s.tag(), s.device)
}

func (s *solarShell) tag() string {
	if s.active() {
		return "active"
	}
	return "shadow"
}

// setDesired feeds one accepted/rejected AD-013 solar document to the reconciler.
// In active mode a Write action (a NEW ceiling) is applied to hardware.
func (s *solarShell) setDesired(doc bus.DesiredState, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	action, reports := s.r.SetDesired(doc, now)
	if !rejected(reports) && doc.CeilingW != nil {
		s.desiredCeilingW = *doc.CeilingW
		s.haveDesiredCeiling = true
	}
	if s.active() && action.Kind == reconcile.ActionWrite {
		s.applyActionLocked(action)
	}
	s.logDecision(action, reports)
}

// observe feeds one poll readback to the reconciler with ONE-SIDED divergence.
// plausible is publishMeasurements' plausibleW verdict for m.W (ledger L9).
func (s *solarShell) observe(m device.Measurements, plausible bool, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var reports []reconcile.Report

	// Active-mode reassert-on-reconnect (ledger L4). Reproduces reassertLocked's
	// inverter branch: a dark inverter reconnecting under a standing cap
	// reasserts the CAP; after release the standing desired is the restore
	// ceiling, so it reasserts restore.
	if s.active() && s.reconnectPending.Swap(false) {
		ra, rr := s.r.Reconnected(now)
		reports = append(reports, rr...)
		if s.active() && ra.Kind == reconcile.ActionWrite {
			s.applyActionLocked(ra)
		}
	}

	haveW := !math.IsNaN(m.W)
	s.haveReadback = haveW
	if haveW {
		s.lastW = m.W
	}

	// One-sided synthesis: report the ceiling exactly when the inverter is at or
	// under it (converged); report the measured value only when it EXCEEDS the
	// ceiling beyond tolerance (diverged). An under-ceiling inverter at dusk must
	// never read as divergence (Common mistakes: two-sided readback for solar).
	read := map[reconcile.Field]float64{}
	if haveW && s.haveDesiredCeiling {
		tol := s.ceilingTolerance()
		if m.W > s.desiredCeilingW+tol {
			read[reconcile.CeilingW] = m.W
		} else {
			read[reconcile.CeilingW] = s.desiredCeilingW
		}
	}

	action, oreports := s.r.Observe(reconcile.Observed{
		Read:      read,
		Connected: true,
		At:        now,
		Plausible: plausible,
	}, now)
	reports = append(reports, oreports...)

	// Same counting semantics as batteryShell: a Write is unambiguous divergence;
	// a plausible, complete, non-write sample is a match. "Complete" for solar is
	// simply "we had a W to compare" (there is no unreadable-Connect ambiguity).
	complete := haveW && s.haveDesiredCeiling
	if action.Kind == reconcile.ActionWrite {
		s.divergences.Inc()
	} else if plausible && complete {
		s.matches.Inc()
	}

	if s.active() && action.Kind == reconcile.ActionWrite {
		s.applyActionLocked(action)
	}

	s.logDecision(action, reports)
}

// tick drives the reconciler's wall-clock timers (staleness / retry-backoff /
// slow reassert). In active mode a Write (retry / reassert) is applied.
func (s *solarShell) tick(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	action, reports := s.r.Tick(now)
	if s.active() && action.Kind == reconcile.ActionWrite {
		s.applyActionLocked(action)
	}
	if action.Kind == reconcile.ActionNone && len(reports) == 0 {
		return // idle tick: nothing to report
	}
	s.logDecision(action, reports)
}

// applyActionLocked executes one reconciler Write in ACTIVE mode through the
// registry driver. No interlock: inverters have no Tier-0 senior guard. Must be
// called with mu held.
func (s *solarShell) applyActionLocked(action reconcile.Action) {
	if action.Kind != reconcile.ActionWrite {
		return
	}
	ctrl := solarFieldsToControl(action.Fields)
	if err := s.driver.Apply(ctrl); err != nil {
		s.writeFailures.Inc()
		log.Printf("lexa-modbus: reconciler[active] %s: apply %s failed: %v", s.device, action.Reason, err)
		return
	}
	s.writes.Inc()
	log.Printf("lexa-modbus: reconciler[active] %s: applied %s (reason=%s)", s.device, describeControl(ctrl), action.Reason)
}

// markReconnected is retryDevice's onReconnect callback for an active-reconciled
// inverter: it only flags the pending reassert (atomic store), never mu.
func (s *solarShell) markReconnected() { s.reconnectPending.Store(true) }

// observeLegacyWrite records the most recent control the LEGACY solar path
// applied, for the shadow verdict line. Never called in active mode (legacy
// solar writes are ignored on hardware then).
func (s *solarShell) observeLegacyWrite(ctrl model.DERControlBase) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.haveLegacy = true
	s.legacyDesc = describeControl(ctrl)
}

// logDecision renders one decision-point log line plus any reports.
func (s *solarShell) logDecision(action reconcile.Action, reports []reconcile.Report) {
	tag := s.tag()
	would := "none"
	verdict := "match"
	if action.Kind == reconcile.ActionWrite {
		would = "write(" + describeFields(action.Fields) + ")"
		verdict = "diverge:" + action.Reason
		s.wouldWrites.Inc()
	}
	legacy := "none"
	if s.haveLegacy {
		legacy = "write(" + s.legacyDesc + ")"
	}
	readback := "none"
	if s.haveReadback {
		readback = fmt.Sprintf("W=%.0f,conn=true", s.lastW)
	}
	log.Printf("lexa-modbus: reconciler[%s] %s(solar): would=%s legacy=%s readback=%s verdict=%s",
		tag, s.device, would, legacy, readback, verdict)
	for _, rep := range reports {
		log.Printf("lexa-modbus: reconciler[%s] %s(solar): report=%s episode=%d mrid=%q reject=%s",
			tag, s.device, rep.Kind, rep.Episode, rep.MRID, rep.Reject)
		if s.pub != nil {
			s.pub(rep)
		}
	}
}

// ceilingTolerance returns the one-sided over-ceiling tolerance for this shell's
// reconciler config (defaults to the reconcile core's power tolerance).
func (s *solarShell) ceilingTolerance() float64 {
	// The reconcile core's default power tolerance is 1 W; a configured
	// ReadbackTolerance[CeilingW] would override it there, but the shell's
	// one-sided pre-filter uses the same effective value. Kept in one place.
	return reconcileCeilingTolerance
}

// reconcileCeilingTolerance mirrors reconcile.defaultPowerTolerance for the
// shell's one-sided pre-filter (the core applies the same value to the residual
// two-sided check). A converged synthesized read equals the ceiling exactly, so
// the two agree.
const reconcileCeilingTolerance = 1.0

// solarFieldsToControl converts a reconcile Write Action's Fields into a Modbus
// DERControlBase — byte-identical to solarCommandToControl's non-nil branch
// (OpModMaxLimW via activePowerFromWatts, the GS-1/MTR-1 multiplier scaling). A
// missing CeilingW yields an empty control (never happens for a solar write).
func solarFieldsToControl(fields map[reconcile.Field]float64) model.DERControlBase {
	if v, ok := fields[reconcile.CeilingW]; ok {
		ap := activePowerFromWatts(math.Max(0, v))
		return model.DERControlBase{OpModMaxLimW: &ap}
	}
	return model.DERControlBase{}
}

// rejected reports whether a SetDesired report set contains a RejectedDoc — the
// signal that the incoming doc did NOT become the standing intent, so the
// shell's tracked ceiling must not move.
func rejected(reports []reconcile.Report) bool {
	for _, rep := range reports {
		if rep.Kind == reconcile.ReportRejectedDoc {
			return true
		}
	}
	return false
}

// runSolarShellTicker drives every inverter shell's Tick on a fixed cadence.
func runSolarShellTicker(shells map[string]*solarShell, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for now := range t.C {
		for _, s := range shells {
			s.tick(now)
		}
	}
}
