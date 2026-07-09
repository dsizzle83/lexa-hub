// TASK-030: EVSE reconciler shell for lexa-ocpp.
//
// evseShell drives one internal/reconcile.Reconciler per configured charging
// station, in shadow or active mode, off the OCPP bridge's real data streams —
// the hub's retained lexa/desired/evse/{station} doc, the station's folded
// metered current (from OnMeterValues / OnTransactionEvent, AFTER
// applySamplesLocked's L11 implausible-current rejection), the legacy command
// path's writes (shadow only), and a reconnect signal from
// SetNewChargingStationHandler. The mode selects what happens with the
// reconciler's Write Actions:
//
//   - SHADOW: a recorder. It logs what the reconciler WOULD send to a real
//     SetChargingProfile driver, compared against what the legacy command path
//     actually sent and the station's last metered current. It NEVER calls the
//     driver (driver is nil).
//
//   - ACTIVE: authoritative. Every reconciler Write is a SetChargingProfile via
//     the SAME profileDriver the legacy path uses (bridge.Apply, which reuses
//     the L11 rejected-as-error logic verbatim). The reconciler is the single
//     owner of the charging profile; the legacy EVSE command topic keeps flowing
//     but is ignored on OCPP (belt and braces for instant rollback). It also
//     closes the reassert-on-reconnect gap the legacy path never had: a charger
//     that drops and reconnects gets its standing current limit re-sent
//     immediately instead of waiting on the hub's 60 s dedupe watchdog.
//
// Convergence is ONE-SIDED and judged ONLY from metered current (an EV drawing
// less than its limit is compliant; profile-Accepted is a write success, never
// convergence — the ev-accept-but-ignore / ack-before-effect lesson). Suspend
// (0 A) converges when metered current ≈ 0 (which a TransactionEvent Ended
// forces by zeroing s.currentA). A silent charger (ev-meter-freeze) yields no
// plausible readback, so the reconciler holds its last assessment and never
// treats silence as convergence.
//
// Keying: per (station, connector). The bench has one station/one connector, but
// the shell stores the connector from the desired doc (0 → 1, per applyCommand)
// and drives it explicitly, so nothing entrenches the single-connector
// assumption in new code (§8.5).
//
// Connect execution (Unit 6.2): OCPP SP-limits give this reconciler exactly
// ONE hardware verb — a charging-current ceiling via SetChargingProfile — so
// a desired doc's Connect opinion is deliberately never fed to
// internal/reconcile as its own Field. Doing so would wedge the core's
// completeness gate FOREVER, and unlike the rare battery/solar case (Connect
// is only occasionally expressed under gateway mode), cmd/hub's EVSE
// actuator has asserted a non-nil Connect on EVERY published doc since
// TASK-030 (desired.go's ApplyEVSECommand: "the EVSE doc has ALWAYS asserted
// connect") — so feeding it straight through would permanently disable
// Observe-driven divergence detection for MaxCurrentA too, not just Connect
// (internal/reconcile's completeness check is all-or-nothing across the
// whole desiredFields set; see reconcile.go's matches() doc). Instead,
// setDesired FOLDS Connect into the EFFECTIVE current handed to the core:
// Connect==false forces an effective 0 A regardless of any MaxCurrentA
// carried in the same doc ("disconnect WINS", the safety ordering this unit
// calls for); Connect==true, or no opinion yet, passes the doc's own
// MaxCurrentA through unfolded ("reconnect defers to the doc's current").
// Cease-to-energize IS a 0 A current limit for OCPP — there is no separate
// disconnect register to write — so folding it into the SAME Field the core
// already tracks and reads back (TransactionEvent Ended forces measured-0,
// per CLAUDE.md) is the honest representation of what actually happens on
// the wire, not a workaround: the EXISTING one-sided MaxCurrentA convergence
// check (metered current at/under the limit is compliant;
// TestEVSEShell_SuspendConvergesAtZero already pins 0 A converging) verifies
// Connect=false naturally, confirming the spec's expectation with ZERO core
// changes and zero new reconcile.Field usage.
package main

import (
	"fmt"
	"log"
	"math"
	"sync"
	"sync/atomic"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/metrics"
	"lexa-hub/internal/mqttutil"
	"lexa-hub/internal/reconcile"
)

// shellMode selects recorder (shadow) vs authoritative (active) behavior.
type shellMode int

const (
	modeShadow shellMode = iota
	modeActive
)

// profileDriver is how an ACTIVE shell writes a converged current limit to a
// charger. The one production implementation is *mqttBridge (bridge.Apply). Nil
// in shadow mode.
type profileDriver interface {
	Apply(stationID string, evseID int, limitA float64) error
}

// evseCurrentTolerance is the one-sided over-limit slack (A) before a metered
// current is judged to exceed its limit. Mirrors reconcile's default power
// tolerance; a converged synthesized read equals the limit exactly.
const evseCurrentTolerance = 1.0

// evseShell is the per-station reconciler shell. Every entry point locks mu:
// feeds arrive on several goroutines (the MQTT desired-doc subscription, the
// OCPP meter/transaction forwarders, the legacy command subscription, the tick
// goroutine).
type evseShell struct {
	mu          sync.Mutex
	stationID   string
	connectorID int // desired-doc connector (0 → 1); driven explicitly
	mode        shellMode
	r           *reconcile.Reconciler

	driver profileDriver // nil in shadow mode

	desiredMaxA    float64
	haveDesiredMax bool

	// desiredConnect is the shell's own standing connect opinion (Unit 6.2) —
	// carried forward across desired docs whose Connect is nil ("no opinion",
	// AD-013 field-absence semantics), mirroring cmd/hub's actuator-side
	// connect fold (desired.go's ApplyEVSECommand). nil until the first doc
	// ever expresses one — a doc that never opines writes MaxCurrentA
	// unfolded, byte-identical to pre-Unit-6.2 behavior. NEVER fed to
	// internal/reconcile directly; see the file doc's "Connect execution"
	// section for why setDesired folds it into MaxCurrentA instead.
	desiredConnect *bool

	// reconnectPending: set by the reconnect hook without taking mu; consumed by
	// observe. markReconnected does only an atomic store (no lock), so calling it
	// from the OCPP connect handler cannot deadlock against the apply path.
	reconnectPending atomic.Bool

	// Shadow legacy bookkeeping (the shadow verdict compares against it).
	haveLegacy bool
	legacyDesc string

	haveReadback bool
	lastA        float64
	lastConn     bool

	matches       *metrics.Counter
	divergences   *metrics.Counter
	wouldWrites   *metrics.Counter
	writes        *metrics.Counter
	writeFailures *metrics.Counter

	// pub forwards convergence-state reports (NonConvergedBegin/End) to MQTT for
	// the hub's breach-episode component (TASK-031); nil in shadow mode / tests.
	pub func(reconcile.Report)
}

// newEVSEShell builds a shell in the given mode. In shadow mode driver MUST be
// nil; in active mode it MUST be non-nil. The reconciler's RetryBackoff starts
// at 15 s — deliberately ≥ the driver's 10 s per-call bound so corrective
// re-writes to one station never overlap an in-flight SetChargingProfile.
func newEVSEShell(stationID string, mreg *metrics.Registry, mode shellMode, driver profileDriver) *evseShell {
	cfg := reconcile.Config{
		RetryBackoff: []time.Duration{15 * time.Second, 30 * time.Second, 60 * time.Second},
	}
	return &evseShell{
		stationID:     stationID,
		connectorID:   1,
		mode:          mode,
		r:             reconcile.New(bus.DesiredClassEVSE, stationID, cfg),
		driver:        driver,
		matches:       mreg.Counter("lexa_ocpp_shadow_matches_total"),
		divergences:   mreg.Counter("lexa_ocpp_shadow_divergences_total"),
		wouldWrites:   mreg.Counter("lexa_ocpp_shadow_would_writes_total"),
		writes:        mreg.Counter("lexa_ocpp_reconcile_writes_total"),
		writeFailures: mreg.Counter("lexa_ocpp_reconcile_write_failures_total"),
	}
}

func (s *evseShell) active() bool { return s.mode == modeActive }

func (s *evseShell) tag() string {
	if s.active() {
		return "active"
	}
	return "shadow"
}

// setDesired feeds one accepted/rejected AD-013 EVSE document to the reconciler.
// In active mode a Write action (a NEW current limit) is applied via the driver.
//
// Unit 6.2: Connect is folded into the EFFECTIVE current fed to the core
// BEFORE SetDesired ever sees it — see the file doc's "Connect execution"
// section. s.desiredConnect carries the opinion forward across docs that
// omit it (nil ⇒ "no opinion," never resets the standing value); a doc that
// has never expressed one leaves desiredConnect nil, so its fold is a no-op
// and MaxCurrentA passes through exactly as it always has.
func (s *evseShell) setDesired(doc bus.DesiredState, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if doc.Connect != nil {
		c := *doc.Connect
		s.desiredConnect = &c
	}
	coreDoc := doc
	if s.desiredConnect != nil && !*s.desiredConnect {
		// Disconnect WINS over any explicit current in the same doc (safety
		// ordering): force the effective limit to 0 A regardless of
		// doc.MaxCurrentA, even if that field was absent.
		zero := 0.0
		coreDoc.MaxCurrentA = &zero
	}
	// Never fed to internal/reconcile as its own Field (see the file doc) —
	// it has already been folded into coreDoc.MaxCurrentA above.
	coreDoc.Connect = nil

	action, reports := s.r.SetDesired(coreDoc, now)
	if !rejected(reports) && coreDoc.MaxCurrentA != nil {
		s.desiredMaxA = *coreDoc.MaxCurrentA
		s.haveDesiredMax = true
		s.connectorID = doc.ConnectorID
		if s.connectorID == 0 {
			s.connectorID = 1
		}
	}
	if s.active() && action.Kind == reconcile.ActionWrite {
		s.applyActionLocked(action)
	}
	s.logDecision(action, reports)
}

// observe feeds one metered-current readback to the reconciler with ONE-SIDED
// divergence. plausible is the tap's !implausibleCurrent verdict (L11);
// connected is the station's link state. Convergence is judged only from a
// plausible, connected sample — a frozen charger (no samples) holds the last
// assessment.
func (s *evseShell) observe(currentA float64, plausible, connected bool, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Active-mode reassert-on-reconnect (ledger L4; the gap the legacy path
	// lacked): reassert the standing current limit before the post-reconnect
	// reading is trusted.
	if s.active() && s.reconnectPending.Swap(false) {
		ra, _ := s.r.Reconnected(now)
		if s.active() && ra.Kind == reconcile.ActionWrite {
			s.applyActionLocked(ra)
		}
	}

	haveA := !math.IsNaN(currentA)
	s.haveReadback = haveA
	s.lastConn = connected
	if haveA {
		s.lastA = currentA
	}

	// One-sided synthesis: report the limit exactly when metered current is at or
	// under it (converged; includes the 0 A suspend case where currentA ≈ 0);
	// report the measured value only when it EXCEEDS the limit beyond tolerance
	// (diverged). An EV drawing less than its limit is never a divergence.
	read := map[reconcile.Field]float64{}
	if haveA && s.haveDesiredMax {
		if currentA > s.desiredMaxA+evseCurrentTolerance {
			read[reconcile.MaxCurrentA] = currentA
		} else {
			read[reconcile.MaxCurrentA] = s.desiredMaxA
		}
	}

	action, reports := s.r.Observe(reconcile.Observed{
		Read:      read,
		Connected: connected,
		At:        now,
		Plausible: plausible,
	}, now)

	complete := haveA && s.haveDesiredMax && connected
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
// non-convergence begin). In active mode a Write (retry / reassert) is applied.
func (s *evseShell) tick(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	action, reports := s.r.Tick(now)
	if s.active() && action.Kind == reconcile.ActionWrite {
		s.applyActionLocked(action)
	}
	if action.Kind == reconcile.ActionNone && len(reports) == 0 {
		return
	}
	s.logDecision(action, reports)
}

// applyActionLocked executes one reconciler Write in ACTIVE mode via the profile
// driver. A rejected/failed profile is a write FAILURE (L11), counted and
// logged; the reconcile core retries per its ≥10 s backoff. Must hold mu.
//
// Unit 6.2 note: limitA below is already the EFFECTIVE current setDesired
// folded Connect into (0 A for a disconnect, the doc's own MaxCurrentA
// otherwise) — this function needs no Connect-specific branch of its own;
// "honoring Connect" and "writing MaxCurrentA" are the same write for OCPP.
func (s *evseShell) applyActionLocked(action reconcile.Action) {
	if action.Kind != reconcile.ActionWrite {
		return
	}
	limitA, ok := action.Fields[reconcile.MaxCurrentA]
	if !ok {
		return
	}
	if err := s.driver.Apply(s.stationID, s.connectorID, limitA); err != nil {
		s.writeFailures.Inc()
		log.Printf("lexa-ocpp: reconciler[active] %s: SetChargingProfile %.1fA (%s) failed: %v",
			s.stationID, limitA, action.Reason, err)
		return
	}
	s.writes.Inc()
	log.Printf("lexa-ocpp: reconciler[active] %s: applied MaxCurrentA=%.1f connector=%d (reason=%s)",
		s.stationID, limitA, s.connectorID, action.Reason)
}

// markReconnected is the OCPP connect hook's callback: atomic store only.
func (s *evseShell) markReconnected() { s.reconnectPending.Store(true) }

// observeLegacyCommand records the most recent current limit the LEGACY command
// path applied, for the shadow verdict line. Never called in active mode.
func (s *evseShell) observeLegacyCommand(maxA float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.haveLegacy = true
	s.legacyDesc = fmt.Sprintf("MaxCurrentA=%.1f", maxA)
}

func (s *evseShell) logDecision(action reconcile.Action, reports []reconcile.Report) {
	tag := s.tag()
	would := "none"
	verdict := "match"
	if action.Kind == reconcile.ActionWrite {
		if v, ok := action.Fields[reconcile.MaxCurrentA]; ok {
			would = fmt.Sprintf("write(MaxCurrentA=%.1f)", v)
		} else {
			would = "write(?)"
		}
		verdict = "diverge:" + action.Reason
		s.wouldWrites.Inc()
	}
	legacy := "none"
	if s.haveLegacy {
		legacy = "write(" + s.legacyDesc + ")"
	}
	readback := "none"
	if s.haveReadback {
		readback = fmt.Sprintf("currentA=%.1f,conn=%t", s.lastA, s.lastConn)
	}
	log.Printf("lexa-ocpp: reconciler[%s] %s(evse): would=%s legacy=%s readback=%s verdict=%s",
		tag, s.stationID, would, legacy, readback, verdict)
	for _, rep := range reports {
		log.Printf("lexa-ocpp: reconciler[%s] %s(evse): report=%s episode=%d mrid=%q reject=%s",
			tag, s.stationID, rep.Kind, rep.Episode, rep.MRID, rep.Reject)
		if s.pub != nil {
			s.pub(rep)
		}
	}
}

// newReconcileReportPublisher returns a shell.pub sink that forwards
// convergence-state reports (NonConvergedBegin/End only) to the hub over MQTT
// (TASK-031), RETAINED on lexa/reconcile/{class}/{device}/report so the hub
// re-seeds current convergence STATE after a restart. Every other report kind
// stays shell-log-only. Called from the shell's mu-held logDecision.
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
			log.Printf("lexa-ocpp: publish reconcile report (%s %s): %v", r.DeviceClass, r.DeviceID, err)
		}
	}
}

// rejected reports whether a SetDesired report set contains a RejectedDoc (the
// incoming doc did NOT become the standing intent).
func rejected(reports []reconcile.Report) bool {
	for _, rep := range reports {
		if rep.Kind == reconcile.ReportRejectedDoc {
			return true
		}
	}
	return false
}

// runEVSEShellTicker drives every station shell's Tick on a fixed cadence.
func runEVSEShellTicker(shells map[string]*evseShell, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for now := range t.C {
		for _, s := range shells {
			s.tick(now)
		}
	}
}
