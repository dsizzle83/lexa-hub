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
	"errors"
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

	// ImplausibleRejects counts every discovery cycle where the scheduler held
	// last-known-good because the freshly-served resource carried a
	// present-but-garbage control value (ActiveControl.ImplausibleReject —
	// audit: malform-huge-activepower), as distinct from a hold caused by an
	// empty/absent program list or the clock-regression guard. A nil-safe
	// *metrics.Counter (see metrics.Counter's doc), so callers/tests that
	// build a Metrics{} zero value can still call RunOnce without wiring a
	// registry.
	ImplausibleRejects *metrics.Counter // lexa_nb_implausible_rejects_total
}

// Discovery owns one discovery walk cycle plus the walk-loop goroutine
// itself (Loop): the ticker, the TASK-042 rewalk channel, and the watchdog
// kicks that prove liveness.
type Discovery struct {
	mc mqtt.Client
	// fetcher is this Discovery's dedicated TLS session. Typed as the
	// discovery.Fetcher interface (WP-7) rather than the concrete
	// *tlsclient.WolfSSLFetcher solely so RunOnce is exercisable in unit
	// tests with a fake resource tree; production wiring still passes the
	// wolfSSL fetcher (cmd/northbound/main.go), which satisfies it.
	fetcher discovery.Fetcher
	lfdi    string
	sched   *scheduler.Scheduler
	clk     *utilitytime.Clock
	tracker *responses.Tracker
	frm     *flowres.Manager
	metrics Metrics

	// pin is the optional WP-7/D4 registration-PIN verifier (nil =
	// registration_pin 0/disabled — the shipped default). Set via
	// SetPinVerifier before Loop starts; read only from the walk goroutine.
	pin *PinVerifier

	// curves publishes the resolved curve-linked content (bus.CurveSet,
	// retained lexa/csip/curves) alongside every ActiveControl publish —
	// including the PIN-freeze held republish, so the retained curve doc and
	// the retained control doc can never drift apart across a freeze (WP-8).
	// Hash-change deduped internally; touched only from the walk goroutine.
	curves publish.Curves

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

	// logEvents is the optional WP-6 LogEvent poster (nil when not wired):
	// after each successful walk, RunOnce feeds it the self EndDevice's
	// LogEventListLink href — the same per-walk path refresh the
	// flow-reservation manager gets via SetRequestPath. Set once at wiring
	// time (SetLogEventSink, before Loop starts) and only read from the
	// single discovery goroutine thereafter.
	logEvents LogEventSink

	// derReport is the optional WP-4 DER* PUT reporter (nil when not wired,
	// i.e. der_report=false): after each successful walk, RunOnce hands it
	// the self EndDevice's DERList entry's sub-resource hrefs (OnWalk),
	// which is both its href refresh and its per-walk status/availability
	// PUT cadence — the walk interval is already pollRate-paced (TASK-071),
	// so per-walk IS the G30 DERList-pollRate cadence. Same set-once/
	// read-from-walk-goroutine discipline as logEvents above.
	derReport DERReportSink

	// pollCfg/lastTree: TASK-071 (§12) walk-cadence pacing. pollCfg is fixed
	// at construction (New); lastTree is the most recently SUCCESSFUL walk's
	// tree (nil until the first success, and left untouched by a failed
	// walk — see RunOnce), read by Loop after every RunOnce call to decide
	// how long to wait before the next one. Both touched only from the
	// single discovery goroutine (Loop/RunOnce), same discipline as
	// failures above.
	pollCfg  PollRateConfig
	lastTree *discovery.ResourceTree
}

// New constructs a Discovery. mc is the MQTT client to publish on; fetcher is
// this Discovery's dedicated wolfSSL session (kept isolated from the
// response/flow-reservation fetchers — see the :179-181 isolation comment
// this replaced in cmd/northbound/main.go); sched/clk/tracker/frm are the
// shared scheduler, single-owner Clock (AD-004), response tracker, and flow-
// reservation manager this walk drives.
// pollCfg is TASK-071's walk-cadence pacing configuration (honor vs
// override server-advertised pollRate — see PollRateConfig/PollRateMode's
// docs in pollrate.go). Passing PollRateConfig{} (Go zero value, Mode "")
// reproduces pre-TASK-071 behavior exactly: effectiveInterval treats any
// Mode other than PollRateHonor as PollRateOverride.
func New(mc mqtt.Client, fetcher discovery.Fetcher, lfdi string, sched *scheduler.Scheduler, clk *utilitytime.Clock, tracker *responses.Tracker, frm *flowres.Manager, m Metrics, pollCfg PollRateConfig) *Discovery {
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
		pollCfg:    pollCfg,
	}
}

// LogEventSink is the slice of internal/northbound/logevent.Manager this
// package needs (defined at the point of consumption, 05 §2): the per-walk
// LogEventListLink path refresh.
type LogEventSink interface {
	SetPath(href string)
}

// SetLogEventSink wires the WP-6 LogEvent poster's path refresh into the
// walk cycle. Call once from main() before Loop starts (an additive setter
// rather than an eleventh New parameter); nil — the default — disables the
// refresh entirely.
func (d *Discovery) SetLogEventSink(s LogEventSink) {
	d.logEvents = s
}

// DERReportSink is the slice of internal/northbound/derreport.Manager this
// package needs (defined at the point of consumption, 05 §2): the per-walk
// href refresh + PUT-cadence trigger. Empty strings mean the walk found no
// link for that sub-resource.
type DERReportSink interface {
	OnWalk(capabilityHref, settingsHref, statusHref, availabilityHref string)
}

// SetDERReporter wires the WP-4 DER* PUT reporter into the walk cycle. Call
// once from main() before Loop starts (same additive-setter shape as
// SetLogEventSink); nil — the default, and der_report=false's wiring —
// disables the per-walk trigger entirely.
func (d *Discovery) SetDERReporter(s DERReportSink) {
	d.derReport = s
}

// HandleRewalk is bus.TopicCSIPRewalk's subscription handler (TASK-042):
// wire it directly into mqttutil.Subscribe from main(). See
// handleRewalkRequest for the mechanism.
func (d *Discovery) HandleRewalk(req bus.RewalkRequest) {
	handleRewalkRequest(d.mc, d.lastPub, d.rewalkGate, d.rewalkChan, req, time.Now())
}

// SetPinVerifier wires the WP-7/D4 registration-PIN verifier. Call before
// Loop starts (the field is read only from the walk goroutine); leaving it
// unset (registration_pin=0, the shipped default) disables the check
// entirely — RunOnce behaves exactly as before WP-7.
func (d *Discovery) SetPinVerifier(v *PinVerifier) {
	d.pin = v
}

// Loop runs the first discovery walk immediately, then loops on interval
// (via a resettable timer — see below) and on TASK-042 rewalk pokes, until
// ctx is cancelled. Callers run this in its own goroutine (it blocks until
// ctx.Done()).
//
// ctx also threads all the way down into the walk itself (TASK-070, R5):
// RunOnce passes it to discovery.Walker.Discover, which checks it between
// every fetch. So a shutdown ctx cancel no longer just stops the NEXT walk
// from starting (the pre-070 behavior, checked only here between ticks) —
// it also unwinds an in-progress walk between resource fetches, bounding
// shutdown latency to one fetch's ReadTimeout instead of the whole
// resource-tree walk.
//
// interval is the operator-configured base cadence (cmd/northbound's
// discovery_interval_s). Pre-TASK-071 this drove a fixed time.Ticker; now it
// is only the FLOOR/override value fed to effectiveInterval each cycle (see
// pollrate.go) — a plain time.Timer replaces the ticker so the wait can
// change cycle to cycle. In PollRateOverride (the bench default) or with a
// zero-value PollRateConfig, effectiveInterval always returns interval
// unchanged, so the wait is byte-identical to the old ticker's cadence.
func (d *Discovery) Loop(ctx context.Context, interval time.Duration) {
	d.RunOnce(ctx)
	// TASK-008: kick once the initial walk returns, success or fail-closed —
	// a walk that erred and held last-known-good is still a live, iterating
	// loop (QA 2026-07-02 northbound-hang/wan-outage-hold: a server that
	// stops responding must NOT starve this kick, only a wedged
	// walker/registry should).
	watchdog.Kick()
	timer := time.NewTimer(d.nextInterval(interval))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			// TASK-008: kick at the top of the loop body — every tick the
			// loop wakes up at all is itself the liveness signal;
			// RunOnce's internal errors are handled by its own fail-closed
			// logging and never prevent reaching this line.
			watchdog.Kick()
			d.RunOnce(ctx)
			timer.Reset(d.nextInterval(interval))
		case <-d.rewalkChan:
			// TASK-042: an immediate out-of-cadence walk requested via
			// lexa/csip/rewalk (HandleRewalk already republished the cached
			// control before poking this channel). Runs the IDENTICAL
			// RunOnce call the regular cadence uses — same code path, same
			// mutexes, nothing to drift out of sync — so single-flight is
			// free: this goroutine is the only caller of RunOnce, and it
			// can only be sitting in this select (not mid-walk) when it
			// picks this case up. The pending regular-cadence timer is
			// stopped/drained and restarted from now (TASK-071: an
			// out-of-cadence walk still refreshes the pollRate-derived
			// wait, same as a normal tick would).
			slog.Info("lexa-northbound: immediate walk triggered by rewalk request")
			watchdog.Kick()
			d.RunOnce(ctx)
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(d.nextInterval(interval))
		}
	}
}

// nextInterval wraps effectiveInterval (pollrate.go, TASK-071) with this
// Discovery's own pollCfg and the most recent successful walk's tree —
// nil until the first success, and left stale (not reset to nil) by a
// failed walk, so a transient error doesn't bounce the cadence back to the
// aggressive base interval and doesn't lose the last-known-good pollRate
// either.
func (d *Discovery) nextInterval(base time.Duration) time.Duration {
	return effectiveInterval(d.lastTree, base, d.pollCfg)
}

// RunOnce performs one discovery walk cycle: walk, clock resync, scheduler
// evaluation, ActiveControl publish, 24h schedule/pricing/billing/flow-
// reservation publishes, response tracker update, and the flow-reservation
// manager's request-path refresh.
//
// ctx (TASK-070, R5) is passed straight to the walker; RunOnce's only new
// responsibility is classifying what comes back. A shutdown-cancel walk
// (errors.Is(err, context.Canceled) or DeadlineExceeded) is NOT a discovery
// failure — the hub is exiting, not the server refusing to answer — so it
// must not increment d.failures/WalkFailures or log at the fail-closed WARN
// level operators and QA diagnosers grep for; that would poison the journal
// with a false fail-closed alarm on every single clean restart. It IS still
// a "publish nothing, hold last-known-good" outcome (below), same as any
// other walk error — cancel-mid-walk is exactly as fail-closed-safe as a
// wedged server, just for a different, non-alarming reason.
func (d *Discovery) RunOnce(ctx context.Context) {
	walkStart := time.Now()
	defer func() { d.metrics.WalkDuration.Set(time.Since(walkStart).Seconds()) }()

	walker := discovery.NewWalker(d.fetcher, d.lfdi)
	tree, err := walker.Discover(ctx, "/dcap")
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// Shutdown (or a future watchdog-driven cancel), not a failed walk:
			// hold last-known-good exactly like any other walk error (no
			// publish below this branch either), but don't count it against
			// discoveryFailures/lexa_nb_walk_failures_total and log at Info,
			// not the fail-closed WARN — a clean shutdown must never look like
			// a WAN outage in the journal.
			slog.Info("lexa-northbound: discovery walk canceled (shutdown) — holding last-published control", "err", err)
			return
		}
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
	// TASK-071: cache the successful walk's tree so Loop's NEXT interval
	// computation (nextInterval, run after this RunOnce returns) can read
	// whatever pollRate this server just advertised. Left untouched on a
	// walk error/cancel above — see lastTree's field doc.
	d.lastTree = tree
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

	// WP-7 (D4): verify the Registration PIN before adopting ANYTHING from
	// this walk's tree. On mismatch/fetch-failure the verifier freezes
	// (edge-logged Error + gauge + certstatus pin_ok inside Check): hold the
	// currently-adopted control via the scheduler's own last-known-good
	// discipline — Evaluate with no programs is the exact fail-closed hold
	// an absent program list gets, so the held control still releases at its
	// own ValidUntil, never sooner and never later — adopt no new control,
	// and republish the held control with a fresh Ts each walk (so the hub's
	// retained-staleness check, TASK-042, does not fire a rewalk storm at a
	// server we are deliberately not walking new state from). Every OTHER
	// consumer of this tree is skipped: schedule/pricing/billing/flow-
	// reservation publishes carry new server state (frozen out with the
	// rest), and the response tracker's Update is server egress (suspended;
	// its own egress gate additionally backstops the async CannotComply
	// path). Clock resync above deliberately still runs — LKG expiry
	// evaluation needs utility time regardless. Self-healing: the next walk
	// re-checks, and a match resumes the normal path below.
	if d.pin.Check(ctx, walker, tree.SelfDevice) {
		held := d.sched.Evaluate(nil, serverNow)
		msg := publish.ToActiveControl(held, tree.ClockOffset)
		if err := mqttutil.PublishJSONRetained(d.mc, bus.TopicCSIPControl, msg); err != nil {
			log.Printf("lexa-northbound: publish control: %v", err)
		} else {
			d.lastPub.set(msg)
		}
		d.curves.Publish(d.mc, held)
		return
	}

	active := d.sched.Evaluate(tree.Programs, serverNow)

	if active != nil && active.Held {
		// TASK-045: migrated to slog. "holding last-known-good" kept intact.
		slog.Warn("lexa-northbound: discovery resolved no valid control (empty/malformed resource); holding last-known-good (fail-closed)",
			"mrid", active.MRID, "valid_until", active.ValidUntil)
		if active.ImplausibleReject {
			// Distinct from the WARN above: this counts specifically the
			// "server served a present-but-garbage control value" case
			// (audit: malform-huge-activepower), not every fail-closed hold —
			// an empty program list or a clock-regression hold does not
			// increment this.
			d.metrics.ImplausibleRejects.Inc()
		}
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
	d.curves.Publish(d.mc, active)
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

	// LogEvent (WP-6, §11.4): refresh the poster's POST target from the self
	// EndDevice's LogEventListLink — "" when the server exposes none (the
	// function set is optional both sides; the poster drops-and-counts in
	// that case rather than spooling).
	if d.logEvents != nil {
		href := ""
		if tree.SelfDevice != nil && tree.SelfDevice.LogEventListLink != nil {
			href = tree.SelfDevice.LogEventListLink.Href
		}
		d.logEvents.SetPath(href)
	}

	// DER* reporting (WP-4): hand the reporter the DER sub-resource hrefs
	// this walk observed. This is also its per-walk PUT trigger (DERStatus/
	// DERAvailability every walk — the pollRate-paced cadence, see the
	// derReport field doc; DERCapability/DERSettings when the dersite
	// content hash changed). During a PIN freeze RunOnce returns before this
	// line, so DER* egress rides the walk freeze for free (D4), and the
	// reporter re-checks the shared egress gate itself for its MQTT-driven
	// path.
	if d.derReport != nil {
		cap, set, stat, avail := derHrefsFromTree(tree)
		d.derReport.OnWalk(cap, set, stat, avail)
	}
}

// derHrefsFromTree extracts the GFEMS DER entry's sub-resource hrefs from a
// walked tree (WP-4). The GFEMS profile is ONE EndDevice with ONE DERList
// entry (UTIL-002's per-DER mechanics are out of scope), so the FIRST entry
// is authoritative; extra entries — nothing this product provisions — are
// ignored here rather than guessed at. Missing links yield "" (the walker
// fetched these same links at walker.go's step 3c; this reuses its
// observations, never hardcoding a path).
func derHrefsFromTree(tree *discovery.ResourceTree) (capability, settings, status, availability string) {
	if tree == nil || tree.DERList == nil || len(tree.DERList.DER) == 0 {
		return "", "", "", ""
	}
	der := tree.DERList.DER[0]
	if der.DERCapabilityLink != nil {
		capability = der.DERCapabilityLink.Href
	}
	if der.DERSettingsLink != nil {
		settings = der.DERSettingsLink.Href
	}
	if der.DERStatusLink != nil {
		status = der.DERStatusLink.Href
	}
	if der.DERAvailabilityLink != nil {
		availability = der.DERAvailabilityLink.Href
	}
	return capability, settings, status, availability
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
