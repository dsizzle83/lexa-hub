// TASK-027: battery reconciler shadow mode for lexa-modbus.
//
// batteryShadow drives one internal/reconcile.Reconciler per shadow-mode
// battery device off the same three real data streams a live write-authority
// driver would use — the hub's retained desired doc, this process's own poll
// readbacks, and the legacy control path's actual writes — and logs what the
// reconciler WOULD do next to a real driver, compared against what the legacy
// path actually did and the device's last readback. It never calls a driver:
// there is no reachable call from this file to registry.ApplyControlTo /
// retryDevice.ApplyControl anywhere (grep-proof per the task's own review
// checklist). Shadow is a recorder, not an actuator.
//
// Reconnect-on-drop (ledger L4's "Reconnected" input) is deliberately NOT
// wired here: the task's shadow-feed list is desired docs, poll
// measurements, and legacy writes only. A device that drops mid-poll never
// reaches Observe at all (publishMeasurements skips poll-error updates before
// this file is invoked), so the reconciler's assessment simply holds through
// the outage and resumes at the next successful poll — a legitimate shadow
// observation in its own right (the reconciler's write-on-diff may lag
// legacy's unconditional reconnect-reassert by up to one poll interval); any
// such gap is exactly what the bench soak (deferred to the wave gate) is
// for, not something to paper over here.
package main

import (
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/metrics"
	"lexa-hub/internal/reconcile"
	"lexa-hub/internal/southbound/device"
	model "lexa-proto/csipmodel"
)

// batteryShadow is the per-device shadow shell. Its exported-to-the-package
// methods are the only entry points; all of them lock mu, because — unlike
// cmdDeduper, which lives entirely on the engine's one control goroutine —
// lexa-modbus's three feeds arrive on three different goroutines (the MQTT
// desired-doc subscription callback, publishMeasurements' loop, and the
// battery-control subscription callback). internal/reconcile.Reconciler
// documents itself as single-writer/no-locks BY DESIGN, leaving serialization
// to the caller; this type is that caller.
type batteryShadow struct {
	mu     sync.Mutex
	device string
	r      *reconcile.Reconciler

	haveLegacy bool
	legacyDesc string

	haveReadback   bool
	lastW, lastSOC float64

	// desiredHasConnect tracks whether the current standing desired doc
	// expresses a Connect opinion. This shell can only ever read back
	// SetpointW (device.Measurements has no Connect-state field — no
	// register on real hardware reports cease/energize state), so whenever
	// the doc also opines on Connect, internal/reconcile's completeness gate
	// (matches()) will hold FOREVER for this device — a real, documented
	// limitation, not a bug — and the shell must not misreport that hold as
	// a "match" (see TestBatteryShadow_IncompleteReadHeldNotCounted).
	desiredHasConnect bool

	// matches/divergences count Observe-driven verdicts only (the direct
	// "does the reconciler's assessment of measured state agree with legacy
	// having nothing further to do" signal — the steady-state gate the
	// acceptance criteria check). wouldWrites is the broader count of every
	// Write Action from ANY of SetDesired/Observe/Tick (new-target adoption
	// and reconnect/reassert writes included), for gauging write-storm risk
	// ahead of TASK-028's flip.
	matches     *metrics.Counter
	divergences *metrics.Counter
	wouldWrites *metrics.Counter
}

// newBatteryShadow builds the shadow shell for device, registering its
// counters (lexa_mb_shadow_*_total) on mreg.
func newBatteryShadow(deviceName string, cfg reconcile.Config, mreg *metrics.Registry) *batteryShadow {
	return &batteryShadow{
		device:      deviceName,
		r:           reconcile.New(bus.DesiredClassBattery, deviceName, cfg),
		matches:     mreg.Counter("lexa_mb_shadow_matches_total"),
		divergences: mreg.Counter("lexa_mb_shadow_divergences_total"),
		wouldWrites: mreg.Counter("lexa_mb_shadow_would_writes_total"),
	}
}

// setDesired feeds one accepted/rejected AD-013 document to the reconciler.
func (s *batteryShadow) setDesired(doc bus.DesiredState, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.desiredHasConnect = doc.Connect != nil
	action, reports := s.r.SetDesired(doc, now)
	// A rejected/heartbeat doc is a real decision point (rejects matter for
	// TASK-031 attribution) but is usually silent (ActionNone, no reports);
	// only log when there is something to say — the logDecision helper
	// already gates on that for the write side, reports print unconditionally.
	s.logDecision(action, reports)
}

// observe feeds one poll readback to the reconciler. plausible is the
// caller's plausibleW verdict for m.W (ledger L9's pattern, reused here); it
// is passed straight through as reconcile.Observed.Plausible so an implausible
// reading holds the previous assessment instead of being trusted as evidence.
// Connected is always true here: publishMeasurements only calls this for an
// update whose poll succeeded (upd.Err == nil) — a genuinely disconnected poll
// never reaches this method at all (see the file doc comment).
func (s *batteryShadow) observe(m device.Measurements, plausible bool, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	read := map[reconcile.Field]float64{}
	haveW := !math.IsNaN(m.W)
	if haveW {
		read[reconcile.SetpointW] = m.W
	}
	s.haveReadback = true
	s.lastW, s.lastSOC = m.W, m.SOC

	action, reports := s.r.Observe(reconcile.Observed{
		Read:      read,
		Connected: true,
		At:        now,
		Plausible: plausible,
	}, now)

	// A Write is unambiguous evidence of divergence regardless of source. A
	// None is ambiguous from out here — converged, OR the core holding a
	// diverged assessment behind its own retry backoff, OR (whenever the doc
	// also opines on Connect, which this shell can never read back) held as
	// permanently incomplete — so only count a match when the sample was
	// both assessable (plausible, real W) AND, so far as this shell can ever
	// supply, complete: an implausible/NaN-W/Connect-incomplete sample holds
	// the previous assessment (ledger L9) and must register as neither
	// outcome.
	complete := haveW && !s.desiredHasConnect
	if action.Kind == reconcile.ActionWrite {
		s.divergences.Inc()
	} else if plausible && complete {
		s.matches.Inc()
	}
	s.logDecision(action, reports)
}

// tick drives the reconciler's wall-clock timers (staleness / retry-backoff /
// slow reassert). Called on a fixed cadence, independent of poll/doc events,
// from a single dedicated goroutine (runBatteryShadowTicker) — never
// concurrently with itself.
func (s *batteryShadow) tick(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	action, reports := s.r.Tick(now)
	if action.Kind == reconcile.ActionNone && len(reports) == 0 {
		return // idle tick: nothing to report (05 §9 — logging is rate-conscious)
	}
	s.logDecision(action, reports)
}

// observeLegacyWrite records the most recent control the LEGACY path actually
// applied (registry.ApplyControlTo having returned success), for the shadow's
// verdict line to compare against. It is a recorder only — this is never a
// write, it is the shadow being told about a write someone else made.
func (s *batteryShadow) observeLegacyWrite(ctrl model.DERControlBase) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.haveLegacy = true
	s.legacyDesc = describeControl(ctrl)
}

// logDecision renders one decision-point log line plus any reports. Must be
// called with mu held.
func (s *batteryShadow) logDecision(action reconcile.Action, reports []reconcile.Report) {
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
		readback = fmt.Sprintf("W=%.0f,SOC=%.1f,conn=true", s.lastW, s.lastSOC)
	}
	log.Printf("lexa-modbus: reconciler[shadow] %s: would=%s legacy=%s readback=%s verdict=%s",
		s.device, would, legacy, readback, verdict)
	for _, rep := range reports {
		log.Printf("lexa-modbus: reconciler[shadow] %s: report=%s episode=%d mrid=%q reject=%s",
			s.device, rep.Kind, rep.Episode, rep.MRID, rep.Reject)
	}
}

// describeFields renders a reconcile.Action's Fields map in the same
// SetpointW=/Connect= shape describeControl uses, so a would/legacy pair in
// one log line is directly comparable.
func describeFields(fields map[reconcile.Field]float64) string {
	var parts []string
	if v, ok := fields[reconcile.SetpointW]; ok {
		parts = append(parts, fmt.Sprintf("SetpointW=%.0f", v))
	}
	if v, ok := fields[reconcile.Connect]; ok {
		parts = append(parts, fmt.Sprintf("Connect=%t", v != 0))
	}
	if len(parts) == 0 {
		return "(empty)"
	}
	return strings.Join(parts, ",")
}

// describeControl renders a model.DERControlBase in the reconcile.Field
// surface's own shape (SetpointW=/Connect=), decoding the SunSpec sign
// convention battCommandToControl encodes (positive discharge → OpModExpLimW,
// negative charge → OpModImpLimW) back into one signed watt value — the exact
// inverse, so the shadow's "legacy=" description is comparable to "would=".
func describeControl(ctrl model.DERControlBase) string {
	var parts []string
	switch {
	case ctrl.OpModExpLimW != nil:
		parts = append(parts, fmt.Sprintf("SetpointW=%.0f", wattsFromActivePower(*ctrl.OpModExpLimW)))
	case ctrl.OpModImpLimW != nil:
		parts = append(parts, fmt.Sprintf("SetpointW=%.0f", -wattsFromActivePower(*ctrl.OpModImpLimW)))
	}
	if ctrl.OpModConnect != nil {
		parts = append(parts, fmt.Sprintf("Connect=%t", *ctrl.OpModConnect))
	}
	if len(parts) == 0 {
		return "(empty)"
	}
	return strings.Join(parts, ",")
}

// wattsFromActivePower decodes a SunSpec ActivePower (value * 10^multiplier)
// back to watts — the inverse of activePowerFromWatts.
func wattsFromActivePower(ap model.ActivePower) float64 {
	return float64(ap.Value) * math.Pow10(int(ap.Multiplier))
}

// runBatteryShadowTicker drives every shadow's Tick on a fixed cadence from
// one dedicated goroutine (so Tick is never called concurrently with itself
// across devices' shared logging, and each device's own mutex still
// serializes it against that device's SetDesired/Observe/legacy-write calls).
// Runs until the process exits (mirrors publishMeasurements' own
// no-explicit-shutdown lifecycle).
func runBatteryShadowTicker(shadows map[string]*batteryShadow, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for now := range t.C {
		for _, s := range shadows {
			s.tick(now)
		}
	}
}
