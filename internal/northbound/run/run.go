// Package run owns the northbound discovery walk loop: one CSIP resource-tree
// walk per cycle, clock reconciliation, scheduler evaluation, publishing
// (delegated to internal/northbound/publish), response tracking (delegated to
// internal/northbound/responses), flow-reservation path refresh (delegated to
// internal/northbound/flowres), and the TASK-042 rewalk single-flight
// mechanism.
//
// Extracted from cmd/northbound/main.go (TASK-068, D12/R5) as a pure move —
// no behavior change, log lines byte-identical, fail-closed discipline
// untouched.
package run

import (
	"context"
	"log"
	"log/slog"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/metrics"
	"lexa-hub/internal/mqttutil"
	"lexa-hub/internal/northbound/discovery"
	"lexa-hub/internal/northbound/flowres"
	"lexa-hub/internal/northbound/publish"
	"lexa-hub/internal/northbound/responses"
	"lexa-hub/internal/northbound/schedule"
	"lexa-hub/internal/northbound/scheduler"
	"lexa-hub/internal/tlsclient"
	"lexa-hub/internal/utilitytime"
	"lexa-hub/internal/watchdog"
)

// Metrics bundles the TASK-044 Prometheus instruments Discovery updates on
// every walk cycle. A small struct rather than three more positional
// constructor parameters, which already takes several.
type Metrics struct {
	WalkDuration *metrics.Gauge   // lexa_nb_walk_duration_seconds
	WalkFailures *metrics.Counter // lexa_nb_walk_failures_total (monotonic total; contrast Discovery.failures, which is consecutive-and-resets)
	ClockOffset  *metrics.Gauge   // lexa_nb_clock_offset_seconds
}

// Discovery owns one discovery walk cycle plus the walk-loop goroutine
// itself (Loop): the ticker, the TASK-042 rewalk channel, and the watchdog
// kicks that prove liveness.
type Discovery struct {
	mc      mqtt.Client
	fetcher *tlsclient.WolfSSLFetcher
	lfdi    string
	sched   *scheduler.Scheduler
	clk     *utilitytime.Clock
	tracker *responses.Tracker
	frm     *flowres.Manager
	metrics Metrics

	// lastPub/rewalkChan/rewalkGate: the TASK-042 (GAP-01/02 re-request path)
	// rewalk single-flight mechanism. See lastPublishedStore and
	// handleRewalkRequest's doc comments below.
	lastPub    *lastPublishedStore
	rewalkChan chan struct{}
	rewalkGate *rewalkGate

	// failures counts consecutive failed walks, for the fail-closed log line
	// and operator triage (was a package var, discoveryFailures, before
	// TASK-068). Touched only from the single discovery goroutine (Loop).
	failures int
}

// New constructs a Discovery. mc is the MQTT client to publish on; fetcher is
// this Discovery's dedicated wolfSSL session (kept isolated from the
// response/flow-reservation fetchers — see the :179-181 isolation comment
// this replaced in cmd/northbound/main.go); sched/clk/tracker/frm are the
// shared scheduler, single-owner Clock (AD-004), response tracker, and flow-
// reservation manager this walk drives.
func New(mc mqtt.Client, fetcher *tlsclient.WolfSSLFetcher, lfdi string, sched *scheduler.Scheduler, clk *utilitytime.Clock, tracker *responses.Tracker, frm *flowres.Manager, m Metrics) *Discovery {
	return &Discovery{
		mc:         mc,
		fetcher:    fetcher,
		lfdi:       lfdi,
		sched:      sched,
		clk:        clk,
		tracker:    tracker,
		frm:        frm,
		metrics:    m,
		lastPub:    &lastPublishedStore{},
		rewalkChan: make(chan struct{}, 1),
		rewalkGate: &rewalkGate{},
	}
}

// HandleRewalk is bus.TopicCSIPRewalk's subscription handler (TASK-042):
// wire it directly into mqttutil.Subscribe from main(). See
// handleRewalkRequest for the mechanism.
func (d *Discovery) HandleRewalk(req bus.RewalkRequest) {
	handleRewalkRequest(d.mc, d.lastPub, d.rewalkGate, d.rewalkChan, req, time.Now())
}

// Loop runs the first discovery walk immediately, then loops on interval
// (the ticker) and on TASK-042 rewalk pokes, until ctx is cancelled. Callers
// run this in its own goroutine (it blocks until ctx.Done()).
func (d *Discovery) Loop(ctx context.Context, interval time.Duration) {
	d.RunOnce()
	// TASK-008: kick once the initial walk returns, success or fail-closed —
	// a walk that erred and held last-known-good is still a live, iterating
	// loop (QA 2026-07-02 northbound-hang/wan-outage-hold: a server that
	// stops responding must NOT starve this kick, only a wedged
	// walker/registry should).
	watchdog.Kick()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// TASK-008: kick at the top of the loop body — every tick the
			// loop wakes up at all is itself the liveness signal;
			// RunOnce's internal errors are handled by its own fail-closed
			// logging and never prevent reaching this line.
			watchdog.Kick()
			d.RunOnce()
		case <-d.rewalkChan:
			// TASK-042: an immediate out-of-cadence walk requested via
			// lexa/csip/rewalk (HandleRewalk already republished the cached
			// control before poking this channel). Runs the IDENTICAL
			// RunOnce call the ticker uses — same code path, same mutexes,
			// nothing to drift out of sync — so single-flight is free: this
			// goroutine is the only caller of RunOnce, and it can only be
			// sitting in this select (not mid-walk) when it picks this case
			// up.
			slog.Info("lexa-northbound: immediate walk triggered by rewalk request")
			watchdog.Kick()
			d.RunOnce()
		}
	}
}

// RunOnce performs one discovery walk cycle: walk, clock resync, scheduler
// evaluation, ActiveControl publish, 24h schedule/pricing/billing/flow-
// reservation publishes, response tracker update, and the flow-reservation
// manager's request-path refresh.
func (d *Discovery) RunOnce() {
	walkStart := time.Now()
	defer func() { d.metrics.WalkDuration.Set(time.Since(walkStart).Seconds()) }()

	walker := discovery.NewWalker(d.fetcher, d.lfdi)
	tree, err := walker.Discover("/dcap")
	if err != nil {
		// FAIL CLOSED on a walk error: publish NOTHING. "Server unreachable /
		// walk failed" is not "server says there are no controls" — publishing
		// a retained no-control here actively wiped the enforced cap the moment
		// the WAN dropped or the head-end wedged (QA 2026-07-02: northbound-hang
		// FAIL, wan-outage-hold DEGRADED — ~9.4 kW exported over a 0 W cap until
		// the server returned). The retained last-good control stays on the bus,
		// lexa-hub keeps enforcing it, and the hub's own local clock discipline
		// (csipExpiredTicks in cmd/hub/state.go) still releases it at ValidUntil
		// if the outage outlives the control. Only a SUCCESSFUL walk that
		// resolves no valid control may release — and that path already holds
		// last-known-good via the scheduler's fail-closed Evaluate.
		d.failures++
		d.metrics.WalkFailures.Inc()
		// TASK-045: migrated to slog. "holding last-published control (fail-closed)"
		// kept intact (WAN-outage vocabulary; grep-verified unquoted today).
		slog.Warn("lexa-northbound: discovery error — holding last-published control (fail-closed)",
			"consecutive", d.failures, "err", err)
		return
	}
	d.failures = 0
	d.metrics.ClockOffset.Set(float64(tree.ClockOffset))

	// Feed the walk's raw offset to the single-owner Clock (AD-004, TASK-035).
	// The Clock accumulates ownership of the accepted offset; SetOffset never
	// alters the value ServerNow returns (still local + raw offset), so this is
	// behavior-preserving. Log only a real Step transition (05 §9: transition
	// logs, not per-tick) — a stepped clock is the class the clock-jitter saga
	// hardened against, worth an operator breadcrumb; Wobble/First are silent.
	if class := d.clk.SetOffset(tree.ClockOffset); class == utilitytime.Step {
		slog.Info("lexa-northbound: utility clock stepped", "offset_s", tree.ClockOffset)
	}

	// Re-anchor the Clock's monotonic reference at this successful walk
	// (TASK-037/AD-004 extension): utilitytime.ServerNowAt(time.Now(), ...)
	// is the raw formula (local + this walk's fresh offset), computed once
	// here and locked in as the new anchor. Between now and the NEXT
	// successful walk, clk.ServerNow() derives purely from monotonic elapsed
	// time since this instant, making the responseTracker's CreatedDateTime
	// arithmetic (and every other ServerNow reader sharing this Clock) immune
	// to a LOCAL wall-clock step during a WAN outage/discovery gap — the
	// exposure GAP-04 identified (today's fallback holds last-known-good
	// indefinitely on a walk failure; a local step during that holdover used
	// to shift every subsequent ServerNow read by the step size). A stable
	// local clock makes this bit-identical to the pre-TASK-037 formula: the
	// anchor is reset to the same raw value every walk, so nothing changes
	// under the common case this task's "must not change" list protects.
	d.clk.Anchor(utilitytime.ServerNowAt(time.Now(), tree.ClockOffset))

	// serverNow now reads from the single-owner, now-anchored Clock (AD-004,
	// TASK-035/037). Immediately after Anchor above this is arithmetically
	// identical to the former scheduler.ServerNow(tree.ClockOffset); it only
	// diverges from that raw formula between walks, and only under a local
	// wall-clock step (see the Anchor call's comment). Computed ONCE per walk
	// and shared across Evaluate/Build/SupersededMRIDs, exactly as before.
	serverNow := d.clk.ServerNow()
	active := d.sched.Evaluate(tree.Programs, serverNow)

	if active != nil && active.Held {
		// TASK-045: migrated to slog. "holding last-known-good" kept intact.
		slog.Warn("lexa-northbound: discovery resolved no valid control (empty/malformed resource); holding last-known-good (fail-closed)",
			"mrid", active.MRID, "valid_until", active.ValidUntil)
	}

	msg := publish.ToActiveControl(active, tree.ClockOffset)
	if err := mqttutil.PublishJSONRetained(d.mc, bus.TopicCSIPControl, msg); err != nil {
		log.Printf("lexa-northbound: publish control: %v", err)
	} else {
		// TASK-042: cache the last SUCCESSFULLY published control so a rewalk
		// request (bus.TopicCSIPRewalk) can repair the retained value
		// immediately without waiting for a full walk to complete — see
		// lastPublishedStore's doc and handleRewalkRequest.
		d.lastPub.set(msg)
	}
	// 24-hour DER schedule — built from all discovered programs, curves, and
	// DER resource data. Published retained so lexa-hub always has the full plan.
	der24h := schedule.Build(tree, serverNow)
	publish.Schedule(d.mc, der24h)

	// TASK-045 per-tick demotion: a successful walk logs on EVERY discovery
	// cycle (discovery_interval_s, 60 s STOCK / faster FAST) — steady-state,
	// not a transition. walkDuration (lexa-northbound's TASK-044 gauge)
	// already covers "is discovery alive", so this drops to Debug rather than
	// Info; the fail-closed WARN/error paths above stay at Warn.
	slog.Debug("lexa-northbound: discovery OK",
		"programs", len(tree.Programs), "curves_programs", publish.CountProgramsWithCurves(tree.Programs),
		"pricing", len(tree.PricingProfiles), "billing", len(tree.BillingAccounts),
		"source", msg.Source, "mrid", msg.MRID, "clock_offset_s", tree.ClockOffset, "slots", len(der24h.Slots))

	d.tracker.Update(tree, active, d.sched.SupersededMRIDs(tree.Programs, serverNow))

	// Pricing (§10.5): publish if we discovered any tariff profiles.
	if len(tree.PricingProfiles) > 0 {
		publish.Pricing(d.mc, tree, serverNow)
	}

	// Billing (§10.7): publish if we discovered any customer accounts.
	if len(tree.BillingAccounts) > 0 {
		publish.Billing(d.mc, tree)
	}

	// Flow Reservation (§10.9): update the manager's request path and publish
	// current reservation statuses.
	d.frm.SetRequestPath(tree.FlowReservationRequestPath)
	publish.FlowReservations(d.mc, tree)
}

// lastPublishedStore caches the most recent bus.ActiveControl this process
// successfully published to lexa/csip/control (TASK-042, GAP-01/02's
// re-request mechanism), so a rewalk request (bus.TopicCSIPRewalk) can
// republish known-good truth immediately — with a fresh Ts — even when the
// WAN is down and a full discovery walk cannot complete. Written only by
// RunOnce's single walk-loop goroutine after a successful publish (see its
// "else" branch); read by the rewalk MQTT subscription's own goroutine
// (paho invokes subscription handlers on a goroutine distinct from the walk
// loop). A small mutex-guarded struct, not a package-level var, so it and
// rewalkGate are both constructed once in New and stay exercisable in
// table-driven unit tests without any shared global state leaking between
// them.
type lastPublishedStore struct {
	mu   sync.Mutex
	ctrl *bus.ActiveControl
}

// set stores a copy of ctrl as the current cache.
func (s *lastPublishedStore) set(ctrl bus.ActiveControl) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := ctrl
	s.ctrl = &c
}

// get returns a copy of the cached control, or nil if nothing has been
// published yet (e.g. every walk so far has failed — northbound's own
// fail-closed startup case).
func (s *lastPublishedStore) get() *bus.ActiveControl {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctrl == nil {
		return nil
	}
	c := *s.ctrl
	return &c
}

// nbRewalkRateLimit bounds how often lexa-northbound honors a
// lexa/csip/rewalk request (TASK-042). The hub already rate-limits how
// often it PUBLISHES one (cmd/hub/state.go's rewalkRateLimit), but this side
// defends independently rather than trusting the publisher's discipline
// alone (05 §12 "walk-rate courtesy": an out-of-cadence walk must stay rare
// even if a hub misbehaves or the retained topic gets redelivered
// repeatedly on a flapping broker connection, subRegistry.replay).
const nbRewalkRateLimit = 10 * time.Second

// rewalkGate is the rate-limit state for handleRewalkRequest.
type rewalkGate struct {
	mu   sync.Mutex
	last time.Time
}

// allow reports whether a rewalk request arriving at now should be honored,
// recording now as the side effect when it is. Split out as its own method
// (rather than inlined in handleRewalkRequest) so the rate-limit decision is
// unit-testable in isolation, without a fake MQTT client or channel.
func (g *rewalkGate) allow(now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.last.IsZero() && now.Sub(g.last) < nbRewalkRateLimit {
		return false
	}
	g.last = now
	return true
}

// handleRewalkRequest is bus.TopicCSIPRewalk's subscription handler
// (TASK-042), factored out so it is unit-testable without a real broker or a
// live walk loop. Rate-limited via gate.allow; when allowed:
//
//  1. If lp has a cached last-published control, republish it retained with
//     Ts refreshed to now — repairing a stale or corrupted retained value
//     even while the WAN is dark, without waiting for a walk to succeed.
//  2. Non-blocking-poke rewalkChan so the walk loop's single goroutine (see
//     Loop) runs an immediate out-of-cadence walk on its own next turn —
//     single-flight is free there (that goroutine is the only caller of
//     RunOnce); the buffered size-1 channel coalesces repeat pokes that
//     arrive while a walk is already in flight rather than queuing them.
func handleRewalkRequest(mc mqtt.Client, lp *lastPublishedStore, gate *rewalkGate, rewalkChan chan<- struct{}, req bus.RewalkRequest, now time.Time) {
	if !gate.allow(now) {
		slog.Debug("lexa-northbound: rewalk request rate-limited, ignoring", "reason", req.Reason)
		return
	}
	slog.Info("lexa-northbound: rewalk requested — republishing cached control and triggering immediate walk",
		"reason", req.Reason)

	if cached := lp.get(); cached != nil {
		refreshed := *cached
		refreshed.Ts = now.Unix()
		if err := mqttutil.PublishJSONRetained(mc, bus.TopicCSIPControl, refreshed); err != nil {
			log.Printf("lexa-northbound: rewalk republish: %v", err)
		} else {
			lp.set(refreshed)
		}
	}

	select {
	case rewalkChan <- struct{}{}:
	default:
		// A walk trigger is already pending; this request coalesces into it.
	}
}
