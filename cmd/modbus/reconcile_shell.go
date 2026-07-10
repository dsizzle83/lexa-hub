// TASK-027/028: battery reconciler shell for lexa-modbus.
//
// batteryShell drives one internal/reconcile.Reconciler per reconciled battery
// device off the same real data streams in both modes — the hub's retained
// desired doc, this process's own poll readbacks, the legacy control path's
// actual writes (shadow only), and (active only) the retryDevice
// reconnect signal. The mode selects what happens with the reconciler's Write
// Actions:
//
//   - SHADOW (TASK-027): a recorder. It logs what the reconciler WOULD do next
//     to a real driver, compared against what the legacy path actually did and
//     the device's last readback. It NEVER calls a driver — driver is nil and
//     there is no reachable call from a shadow shell to registry.ApplyControlTo
//     / retryDevice.ApplyControl (grep-proof per the task's review checklist).
//
//   - ACTIVE (TASK-028): authoritative. Every reconciler Write is converted
//     back through the SAME battCommandToControl sign mapping the legacy path
//     used and applied through the registry driver — with Tier-0 interlock
//     seniority: while the interlock has the pack force-disconnected, a
//     connect-restoring write is SUPPRESSED (reported as InterlockHold) rather
//     than fighting it. The reconciler is the single reasserter-on-reconnect
//     (retryDevice's lastCtrl reassert is suppressed for the device); the
//     legacy battery command topic keeps flowing but is ignored on hardware
//     (belt and braces for instant rollback). The interlock's charge intent is
//     fed from the desired doc the reconciler executes (moved here from the
//     legacy subscribe path).
//
// Reconnect-on-drop (ledger L4's "Reconnected" input) is wired in ACTIVE mode
// only: retryDevice sets reconnectPending (an atomic, so the poll goroutine
// never takes this shell's mutex — avoiding a lock-order inversion with the
// apply path) after a successful reopen, and observe consumes it, reasserting
// the standing desired before the post-reconnect readback is trusted. In SHADOW
// mode Reconnected is deliberately NOT wired (the task's shadow-feed list is
// desired docs, poll measurements, and legacy writes only); a dropped device's
// assessment simply holds through the outage and resumes at the next poll.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/metrics"
	"lexa-hub/internal/mqttutil"
	"lexa-hub/internal/reconcile"
	"lexa-hub/internal/southbound/device"
	model "lexa-proto/csipmodel"
)

// shellMode selects recorder (shadow) vs authoritative (active) behavior.
type shellMode int

const (
	modeShadow shellMode = iota
	modeActive
)

// reconcileDriver is how an ACTIVE shell writes a converged control to
// hardware. The one production implementation is registryDriver (below), which
// routes through the SAME registry.ApplyControlTo path the legacy battery
// branch used. Nil in shadow mode — a recorder has no driver.
type reconcileDriver interface {
	Apply(model.DERControlBase) error
}

// registryDriver applies a control to one named device via the southbound
// registry — the identical path legacy battery commands took
// (registry.ApplyControlTo → retryDevice.ApplyControl). controlApplier is the
// interface already defined in interlock.go (same ApplyControlTo shape).
type registryDriver struct {
	reg controlApplier
	dev string
}

func (d registryDriver) Apply(ctrl model.DERControlBase) error {
	return d.reg.ApplyControlTo(d.dev, ctrl)
}

// interlockGate is the read-only slice of the Tier-0 interlock an ACTIVE shell
// consults to honour seniority (batterySafetyInterlock.isTripped). Nil in
// shadow mode.
type interlockGate interface {
	isTripped(dev string) bool
}

// batteryShell is the per-device reconciler shell. Its exported-to-the-package
// methods are the only entry points; all of them lock mu, because — unlike
// cmdDeduper, which lives entirely on the engine's one control goroutine —
// lexa-modbus's feeds arrive on several goroutines (the MQTT desired-doc
// subscription callback, publishMeasurements' loop, the battery-control
// subscription callback, the tick goroutine). internal/reconcile.Reconciler
// documents itself as single-writer/no-locks BY DESIGN, leaving serialization
// to the caller; this type is that caller.
type batteryShell struct {
	mu     sync.Mutex
	device string
	mode   shellMode
	r      *reconcile.Reconciler

	// Active-mode collaborators (all nil in shadow mode).
	driver    reconcileDriver
	interlock interlockGate
	// note feeds the Tier-0 interlock the charge intent from the desired doc
	// the reconciler is executing (mirrors interlock.noteControl's logic; moved
	// off the legacy subscribe path in active mode). Nil in shadow mode.
	note func(bus.BattCommand)
	// reconnectPending is set by retryDevice's onReconnect callback (active
	// only) after a successful reopen, WITHOUT taking mu — the poll goroutine
	// must never block on this shell's lock, and taking mu there would invert
	// the mu→registry(retryDevice.mu) order the apply path uses and could
	// deadlock. Consumed (swapped to false) by observe under mu.
	reconnectPending atomic.Bool

	// Legacy-write bookkeeping (shadow only: the shadow's verdict line compares
	// against what the legacy path actually wrote).
	haveLegacy bool
	legacyDesc string

	haveReadback   bool
	lastW, lastSOC float64

	// desiredHasConnect tracks whether the current standing desired doc
	// expresses a Connect opinion. This shell can only ever read back SetpointW
	// (device.Measurements has no Connect-state field — no register on real
	// hardware reports cease/energize state), so whenever the doc also opines on
	// Connect, internal/reconcile's completeness gate (matches()) holds FOREVER
	// for this device — a real, documented limitation, not a bug — and the shell
	// must not misreport that hold as a "match".
	desiredHasConnect bool

	// Shadow verdict counters (Observe-driven only) — see TASK-027.
	matches     *metrics.Counter
	divergences *metrics.Counter
	wouldWrites *metrics.Counter

	// Active-mode counters.
	writes         *metrics.Counter // lexa_mb_reconcile_writes_total — applied writes
	writeFailures  *metrics.Counter // lexa_mb_reconcile_write_failures_total
	interlockHolds *metrics.Counter // lexa_mb_interlock_holds_total — Tier-0-suppressed connect-restores

	// pub forwards convergence-state reports (NonConvergedBegin/End) to MQTT for
	// the hub's breach-episode component (TASK-031); nil in shadow mode and in
	// tests (a no-op then). Set by main.go in active mode.
	pub func(reconcile.Report)
}

// newBatteryShadow builds a SHADOW-mode shell (a recorder; no driver, no
// interlock, no reconnect feed). Retained as the shadow constructor so
// TASK-027's tests and the shadow deploy path are unchanged.
func newBatteryShadow(deviceName string, cfg reconcile.Config, mreg *metrics.Registry) *batteryShell {
	return newBatteryShell(deviceName, cfg, mreg, modeShadow, nil, nil, nil)
}

// newBatteryShell builds a shell in the given mode. In shadow mode driver,
// interlock and note MUST be nil; in active mode all three MUST be non-nil.
func newBatteryShell(deviceName string, cfg reconcile.Config, mreg *metrics.Registry, mode shellMode, driver reconcileDriver, interlock interlockGate, note func(bus.BattCommand)) *batteryShell {
	return &batteryShell{
		device:         deviceName,
		mode:           mode,
		r:              reconcile.New(bus.DesiredClassBattery, deviceName, cfg),
		driver:         driver,
		interlock:      interlock,
		note:           note,
		matches:        mreg.Counter("lexa_mb_shadow_matches_total"),
		divergences:    mreg.Counter("lexa_mb_shadow_divergences_total"),
		wouldWrites:    mreg.Counter("lexa_mb_shadow_would_writes_total"),
		writes:         mreg.Counter("lexa_mb_reconcile_writes_total"),
		writeFailures:  mreg.Counter("lexa_mb_reconcile_write_failures_total"),
		interlockHolds: mreg.Counter("lexa_mb_interlock_holds_total"),
	}
}

// active reports whether this shell owns the hardware write path.
func (s *batteryShell) active() bool { return s.mode == modeActive }

// setDesired feeds one accepted/rejected AD-013 document to the reconciler. In
// active mode a Write action (a NEW target) is applied to hardware, and the
// interlock's charge intent is refreshed from the desired doc (intent only ever
// changes on a new target, so noting here — not on every write — is sufficient
// and matches interlock.noteControl's old placement: before the write).
func (s *batteryShell) setDesired(doc bus.DesiredState, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.desiredHasConnect = doc.Connect != nil
	action, reports := s.r.SetDesired(doc, now)
	if s.active() && action.Kind == reconcile.ActionWrite {
		s.note(battFieldsToCommand(action.Fields))
		reports = append(reports, s.applyActionLocked(action, now)...)
	}
	s.logDecision(action, reports)
}

// observe feeds one poll readback to the reconciler. plausible is the caller's
// plausibleW verdict for m.W (ledger L9's pattern, reused here). Connected is
// always true here: publishMeasurements only calls this for a poll that
// succeeded (upd.Err == nil). In active mode a just-reconnected pack reasserts
// desired (ledger L4) BEFORE its readback is judged, and a diverged read is
// corrected by a write.
func (s *batteryShell) observe(m device.Measurements, plausible bool, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var reports []reconcile.Report

	// Active-mode reassert-on-reconnect (ledger L4). The retryDevice callback
	// set reconnectPending on reopen without taking mu; consume it here. The
	// reassert runs before Observe so the pack is at the hub's current desire
	// before the post-reconnect reading is trusted (a rebooted pack may hold
	// defaults). Suppressed by the interlock like any other write.
	if s.active() && s.reconnectPending.Swap(false) {
		ra, rr := s.r.Reconnected(now)
		reports = append(reports, rr...)
		reports = append(reports, s.applyActionLocked(ra, now)...)
	}

	read := map[reconcile.Field]float64{}
	haveW := !math.IsNaN(m.W)
	if haveW {
		read[reconcile.SetpointW] = m.W
	}
	s.haveReadback = true
	s.lastW, s.lastSOC = m.W, m.SOC

	action, oreports := s.r.Observe(reconcile.Observed{
		Read:      read,
		Connected: true,
		At:        now,
		Plausible: plausible,
	}, now)
	reports = append(reports, oreports...)

	// A Write is unambiguous evidence of divergence regardless of source; a None
	// is ambiguous from out here (converged, held behind backoff, or Connect-
	// incomplete). Only count a match when the sample was both assessable
	// (plausible, real W) AND complete (no unreadable Connect opinion).
	complete := haveW && !s.desiredHasConnect
	if action.Kind == reconcile.ActionWrite {
		s.divergences.Inc()
	} else if plausible && complete {
		s.matches.Inc()
	}

	if s.active() && action.Kind == reconcile.ActionWrite {
		reports = append(reports, s.applyActionLocked(action, now)...)
	}

	s.logDecision(action, reports)
}

// tick drives the reconciler's wall-clock timers (staleness / retry-backoff /
// slow reassert). Called on a fixed cadence from a single dedicated goroutine.
// In active mode a Write action (retry / reassert) is applied.
func (s *batteryShell) tick(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	action, reports := s.r.Tick(now)
	if s.active() && action.Kind == reconcile.ActionWrite {
		reports = append(reports, s.applyActionLocked(action, now)...)
	}
	if action.Kind == reconcile.ActionNone && len(reports) == 0 {
		return // idle tick: nothing to report (05 §9 — logging is rate-conscious)
	}
	s.logDecision(action, reports)
}

// applyActionLocked executes one reconciler Write Action in ACTIVE mode,
// honouring Tier-0 interlock seniority. Must be called with mu held. It returns
// any Report the shell synthesizes (an InterlockHold when a write is
// suppressed); the reconciler's own Reports are handled by the caller.
//
// Interlock seniority (the critical design point): while the interlock has this
// pack force-disconnected, a write that would RESTORE connect (Fields[Connect]
// == 1) is suppressed — issuing it would rewrite Conn=1 against the Tier-0
// force-disconnect, the exact guard-versus-guard oscillation this program
// exists to kill. A write with no connect-restore opinion (a bare setpoint) is
// still allowed through; on a disconnected pack it simply lands as deferred
// intent that takes effect when the hub reconnects the pack. The interlock
// clears its trip on the poll after the fault clears, so normal reconciliation
// resumes on its own.
func (s *batteryShell) applyActionLocked(action reconcile.Action, now time.Time) []reconcile.Report {
	if action.Kind != reconcile.ActionWrite {
		return nil
	}
	restoresConnect := action.Fields[reconcile.Connect] == 1
	if restoresConnect && s.interlock.isTripped(s.device) {
		s.interlockHolds.Inc()
		log.Printf("lexa-modbus: reconciler[active] %s: interlock HOLD — Tier-0 senior, connect-restore suppressed (reason=%s)",
			s.device, action.Reason)
		return []reconcile.Report{{
			Kind:        reconcile.ReportInterlockHold,
			DeviceClass: bus.DesiredClassBattery,
			DeviceID:    s.device,
			At:          now,
		}}
	}
	ctrl := battCommandToControl(battFieldsToCommand(action.Fields))
	if err := s.driver.Apply(ctrl); err != nil {
		s.writeFailures.Inc()
		log.Printf("lexa-modbus: reconciler[active] %s: apply %s failed: %v", s.device, action.Reason, err)
		return nil
	}
	s.writes.Inc()
	log.Printf("lexa-modbus: reconciler[active] %s: applied %s (reason=%s)",
		s.device, describeControl(ctrl), action.Reason)
	return nil
}

// markReconnected is retryDevice's onReconnect callback for an active-reconciled
// device: it only flags the pending reassert (an atomic store), never touching
// mu, so the poll goroutine that calls it under retryDevice.mu cannot deadlock
// against the apply path (mu → registry → retryDevice.mu).
func (s *batteryShell) markReconnected() { s.reconnectPending.Store(true) }

// observeLegacyWrite records the most recent control the LEGACY path actually
// applied, for the SHADOW verdict line to compare against. Never a write; the
// shell is being told about a write someone else made. In active mode the
// legacy path does not write battery hardware, so this is never called.
func (s *batteryShell) observeLegacyWrite(ctrl model.DERControlBase) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.haveLegacy = true
	s.legacyDesc = describeControl(ctrl)
}

// logDecision renders one decision-point log line plus any reports. Must be
// called with mu held. The "would=" verb is shadow's framing; in active mode a
// separate applied-line (applyActionLocked) records the real write, and this
// line's would= still shows the reconciler's intent for continuity.
func (s *batteryShell) logDecision(action reconcile.Action, reports []reconcile.Report) {
	tag := "shadow"
	if s.active() {
		tag = "active"
	}
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
	log.Printf("lexa-modbus: reconciler[%s] %s: would=%s legacy=%s readback=%s verdict=%s",
		tag, s.device, would, legacy, readback, verdict)
	for _, rep := range reports {
		log.Printf("lexa-modbus: reconciler[%s] %s: report=%s episode=%d mrid=%q reject=%s",
			tag, s.device, rep.Kind, rep.Episode, rep.MRID, rep.Reject)
		if s.pub != nil {
			s.pub(rep)
		}
	}
}

// newReconcileReportPublisher returns a shell.pub sink that forwards
// convergence-state reports to the hub over MQTT (TASK-031). Only
// NonConvergedBegin/End are published — those two carry the device-level
// "won't converge under the active control" level the breach-episode component
// consumes; every other report kind stays shell-log-only. Published RETAINED on
// lexa/reconcile/{class}/{device}/report (topic derived from the report's own
// class/device) so the hub re-seeds the current convergence STATE after a
// restart (latest level wins; the alert EDGE itself is non-retained, published
// by the hub). Called from the shell's mu-held logDecision, so it inherits the
// device serialization; the publish is fire-with-timeout (mqttutil).
func newReconcileReportPublisher(mc mqtt.Client) func(reconcile.Report) {
	return func(r reconcile.Report) {
		if r.Kind != reconcile.ReportNonConvergedBegin && r.Kind != reconcile.ReportNonConvergedEnd {
			return
		}
		msg := bus.ReconcileReport{
			Envelope:    bus.Envelope{V: bus.ReconcileReportV},
			Kind:        r.Kind.String(),
			DeviceClass: r.DeviceClass,
			DeviceID:    r.DeviceID,
			MRID:        r.MRID,
			Seq:         r.Seq,
			IssuedAt:    r.IssuedAt,
			Episode:     r.Episode,
			Ts:          r.At.Unix(),
		}
		topic := bus.ReconcileReportTopic(r.DeviceClass, r.DeviceID)
		if err := mqttutil.PublishJSONRetained(mc, topic, msg); err != nil {
			log.Printf("lexa-modbus: publish reconcile report (%s %s): %v", r.DeviceClass, r.DeviceID, err)
		}
	}
}

// healSubscribeWaitTimeout/healRetainedWaitTimeout bound
// healStaleRetainedReport's one-shot startup probe (WS-4.5): how long to
// wait for the SUBSCRIBE to ack, and how long to wait for a retained message
// to arrive on it (nothing arriving within healRetainedWaitTimeout means
// there was no retained message at all — nothing to heal). Kept as
// variables, not inlined into healStaleRetainedReport, purely so
// reconcile_shell_test.go can drive the "nothing retained" timeout path
// without a real multi-second wait — production always calls
// healStaleRetainedReport, never healStaleRetainedReportT directly.
var (
	healSubscribeWaitTimeout = 5 * time.Second
	healRetainedWaitTimeout  = 3 * time.Second
)

// healStaleRetainedReport corrects a retained NonConvergedBegin report left
// over from a PREVIOUS process instance of this shell (a crash/restart
// between Begin and its matching End) — see WS-4.5 (docs/refactor/HANDOFF.md
// §8, csip-tls-test repo). Call exactly ONCE, at shell startup (active mode
// only), BEFORE this instance's own reconciler starts publishing — this
// deliberately uses a plain one-shot mc.Subscribe, NOT mqttutil.Subscribe's
// reconnect-replay registry: a broker reconnect during a still-running shell
// (not a process restart) must not be confused with this case — the
// in-memory reconciler may legitimately still be mid-episode then, and
// healing would wrongly close a real episode. Only a genuine process
// restart resets the reconciler's episode memory to "not tracking
// anything", which is exactly when a leftover retained Begin becomes
// untrustworthy and needs healing.
func healStaleRetainedReport(mc mqtt.Client, class, deviceID string, now time.Time) {
	healStaleRetainedReportT(mc, class, deviceID, now, healSubscribeWaitTimeout, healRetainedWaitTimeout)
}

// healStaleRetainedReportT is healStaleRetainedReport with injectable wait
// bounds (see healSubscribeWaitTimeout/healRetainedWaitTimeout's doc).
func healStaleRetainedReportT(mc mqtt.Client, class, deviceID string, now time.Time, subscribeWait, retainedWait time.Duration) {
	topic := bus.ReconcileReportTopic(class, deviceID)
	done := make(chan *bus.ReconcileReport, 1)
	handler := func(_ mqtt.Client, m mqtt.Message) {
		if !m.Retained() {
			done <- nil // a live (non-retained) message arrived; nothing was retained
			return
		}
		var rep bus.ReconcileReport
		if err := json.Unmarshal(m.Payload(), &rep); err != nil {
			log.Printf("lexa-modbus: heal stale retained report %s: unmarshal: %v", topic, err)
			done <- nil
			return
		}
		done <- &rep
	}

	tok := mc.Subscribe(topic, 1, handler)
	if !tok.WaitTimeout(subscribeWait) || tok.Error() != nil {
		log.Printf("lexa-modbus: heal stale retained report: subscribe %s failed", topic)
		return
	}
	defer mc.Unsubscribe(topic)

	var retained *bus.ReconcileReport
	select {
	case retained = <-done:
	case <-time.After(retainedWait):
		return // no retained message at all — nothing to heal
	}

	end := decideStaleHeal(retained, class, deviceID, now)
	if end == nil {
		return
	}
	if err := mqttutil.PublishJSONRetained(mc, topic, *end); err != nil {
		log.Printf("lexa-modbus: heal stale retained report %s: publish End: %v", topic, err)
		return
	}
	log.Printf("lexa-modbus: healed stale retained NonConvergedBegin for %s/%s (mrid=%s) on shell start",
		class, deviceID, end.MRID)
}

// decideStaleHeal is healStaleRetainedReport's pure decision core, split out
// for table-testing without a broker or fake mqtt.Client (this package has
// no fake mqtt.Client today — mirroring newReconcileReportPublisher, which
// also has no direct unit test; the Subscribe/Unsubscribe/Publish glue in
// healStaleRetainedReportT above is exercised by reconcile_shell_test.go's
// fake client, but a real-broker end-to-end proof is an accepted gap, same
// honesty standard as the rest of this package's MQTT glue). retained is nil
// when nothing was retained on the topic, or the retained payload failed to
// decode — both treated as "nothing to heal". A retained report whose Kind
// isn't NonConvergedBegin (e.g. it already ended cleanly) is also left
// alone. Only a retained NonConvergedBegin produces a corrective End,
// carrying the SAME MRID/Episode forward so a consumer can correlate it with
// the stale Begin it corrects.
func decideStaleHeal(retained *bus.ReconcileReport, class, deviceID string, now time.Time) *bus.ReconcileReport {
	if retained == nil || retained.Kind != reconcile.ReportNonConvergedBegin.String() {
		return nil
	}
	return &bus.ReconcileReport{
		Envelope:    bus.Envelope{V: bus.ReconcileReportV},
		Kind:        reconcile.ReportNonConvergedEnd.String(),
		DeviceClass: class,
		DeviceID:    deviceID,
		MRID:        retained.MRID,
		Episode:     retained.Episode,
		IssuedAt:    now.Unix(),
		Ts:          now.Unix(),
	}
}

// battFieldsToCommand converts a reconcile Write Action's Fields back into a
// bus.BattCommand so battCommandToControl (the SINGLE sign-mapping owner, reused
// not reimplemented) can encode it into the SunSpec DERControlBase. Only the
// battery-relevant fields are carried; a missing field stays nil ("no opinion").
func battFieldsToCommand(fields map[reconcile.Field]float64) bus.BattCommand {
	var cmd bus.BattCommand
	if v, ok := fields[reconcile.SetpointW]; ok {
		w := v
		cmd.SetpointW = &w
	}
	if v, ok := fields[reconcile.Connect]; ok {
		b := v != 0
		cmd.Connect = &b
	}
	return cmd
}

// describeFields renders a reconcile.Action's Fields map in the same
// SetpointW=/Connect= shape describeControl uses, so a would/legacy pair in one
// log line is directly comparable.
func describeFields(fields map[reconcile.Field]float64) string {
	var parts []string
	if v, ok := fields[reconcile.SetpointW]; ok {
		parts = append(parts, fmt.Sprintf("SetpointW=%.0f", v))
	}
	if v, ok := fields[reconcile.CeilingW]; ok {
		parts = append(parts, fmt.Sprintf("CeilingW=%.0f", v))
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
// inverse, so a description is comparable to describeFields' output.
func describeControl(ctrl model.DERControlBase) string {
	var parts []string
	switch {
	case ctrl.OpModExpLimW != nil:
		parts = append(parts, fmt.Sprintf("SetpointW=%.0f", wattsFromActivePower(*ctrl.OpModExpLimW)))
	case ctrl.OpModImpLimW != nil:
		parts = append(parts, fmt.Sprintf("SetpointW=%.0f", -wattsFromActivePower(*ctrl.OpModImpLimW)))
	}
	// Solar (TASK-029): the inverter ceiling rides on OpModMaxLimW; render it so a
	// would/legacy pair for an inverter is directly comparable to describeFields.
	if ctrl.OpModMaxLimW != nil {
		parts = append(parts, fmt.Sprintf("CeilingW=%.0f", wattsFromActivePower(*ctrl.OpModMaxLimW)))
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

// runBatteryShellTicker drives every shell's Tick on a fixed cadence from one
// dedicated goroutine (so Tick is never called concurrently with itself across
// devices, and each device's own mutex still serializes it against that
// device's SetDesired/Observe/legacy-write calls). Runs until the process
// exits (mirrors publishMeasurements' own no-explicit-shutdown lifecycle).
func runBatteryShellTicker(shells map[string]*batteryShell, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for now := range t.C {
		for _, s := range shells {
			s.tick(now)
		}
	}
}
