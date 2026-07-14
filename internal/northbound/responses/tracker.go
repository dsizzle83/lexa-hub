// Package responses implements the northbound end of the GEN.044 / CORE-022
// Response state machine: it tracks every DERControl event's lifecycle
// (Received/Started/Completed/Cancelled/Superseded) and posts the
// corresponding Response to the utility server, plus the CannotComply
// compliance-breach Response (TASK-031, reworked to a per-episode dedupe).
//
// Extracted from cmd/northbound/main.go (TASK-068, D12/R5) as a pure move —
// no behavior change from the original responseTracker.
package responses

import (
	"encoding/xml"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"sync"

	"lexa-hub/internal/journal"
	"lexa-hub/internal/metrics"
	"lexa-hub/internal/northbound/discovery"
	"lexa-hub/internal/northbound/egress"
	"lexa-hub/internal/northbound/scheduler"
	"lexa-hub/internal/utilitytime"
	model "lexa-proto/csipmodel"
)

// Poster is the subset of tlsclient.WolfSSLFetcher the response state
// machine needs. Narrowing it to an interface keeps the CORE-022 logic unit
// testable without a live TLS session.
type Poster interface {
	Post(path string, body []byte, contentType string) ([]byte, string, error)
}

// Tracker drives the GEN.044 / CORE-022 Response state machine.
type Tracker struct {
	poster          Poster
	lfdi            string
	responseSetPath string
	// clk is the single-owner Clock (AD-004, TASK-035); Response CreatedDateTime
	// values read serverNow from it. It shares the same Clock instance the
	// discovery loop feeds via SetOffset, so postResponse sees the same
	// accumulated offset the last walk accepted — identical arithmetic to the
	// former scheduler.ServerNow(rt.clockOffset).
	clk *utilitytime.Clock
	// posted records the last Response status sent for each event mRID, so we
	// never re-post a transition and can tell whether an event has already
	// reached a terminal state (Completed/Cancelled/Superseded).
	posted     map[string]uint8
	activeMRID string
	// alerted records mRIDs for which a CannotComply Response has been posted
	// in the current breach episode, so a redelivered MQTT alert does not
	// double-post. Cleared when the hub signals the breach has cleared.
	alerted map[string]bool
	// confirmedAlerted mirrors the subset of alerted[] that has been
	// durably persisted to store (WS-4.2, persist.go): AlertCannotComply
	// marks alerted[key]=true immediately (in-memory dedup, unchanged from
	// pre-persistence behavior) but only adds to confirmedAlerted — and
	// only then calls store.AppendAlerted — once postResponse confirms the
	// POST succeeded. maybeCompact's checkpoint reads from THIS map, never
	// alerted directly: an in-flight/failed attempt is real in alerted
	// (blocks a same-process retry, unchanged) but must never be written
	// durably, or a genuine restart would lose its only chance to retry a
	// CannotComply the utility never actually received.
	confirmedAlerted map[string]bool
	// mu guards the tracker: Update() runs on the discovery goroutine while
	// AlertCannotComply()/ClearAlerts() run on the MQTT subscription goroutine.
	mu sync.Mutex
	// responsesPosted counts every successful POST (lexa_nb_responses_posted_total,
	// TASK-044); nil-safe (metrics.Counter's Inc is a no-op on a nil receiver),
	// so tests constructing a Tracker without wiring metrics need no change.
	responsesPosted *metrics.Counter

	// jw is the optional TASK-040 event journal; nil disables the
	// cannot_comply_posted emit in AlertCannotComply below.
	jw *journal.Writer

	// store is the optional WS-4.2 response-state persistence path (nil
	// disables persistence entirely — RAM-only, pre-WS-4.2 behavior). See
	// persist.go's package doc for the format and why it is not
	// internal/journal.Writer reused as-is.
	store *Store

	// legacyCannotComply (WP-7, D5): true restores the pre-WP-7 wire
	// behavior byte-for-byte — 0xF0 (model.ResponseCannotComply) at breach
	// onset, Completed(3) at every event end, and no receipt-reject
	// Responses at all — for benches whose gridsim still expects the LEXA
	// profile extension (config legacy_cannotcomply_code). false (the
	// default) emits standard IEEE 2030.5 Table 27 codes: 8 at episode
	// onset, 3/8/10 at event end (endOfEventCode), 252/253 at receipt
	// rejection (ReceiptReject). Set via SetLegacyCannotComplyCode at
	// wiring time, before subscriptions/walks start.
	legacyCannotComply bool

	// gate is the optional WP-7/D4 egress gate: when suspended (the
	// registration-PIN freeze), postResponse transmits nothing. nil = never
	// suspended (egress.Gate's nil convention). During a freeze the walk
	// loop already skips Update() entirely (run.RunOnce); this gate
	// backstops the async AlertCannotComply path, which arrives on the MQTT
	// subscription goroutine regardless of walk state.
	gate *egress.Gate

	// epi (WP-7, D5) is the breach-overlap record for the CURRENT active
	// event, feeding endOfEventCode's 3/8/10 selection at event end.
	// Sampled once per Update cycle (walk cadence) while the event stays
	// active, marked directly at breach onset/clear, reset on event
	// start/end, and persisted on every change (setEpisode) so a restart
	// mid-event keeps both the record and activeMRID. Guarded by mu like
	// every other tracker field.
	epi EpisodeState
}

// New constructs a Tracker that POSTs Responses via p, identifying as lfdi,
// to responseSetPath, reading time from clk, counting successful POSTs on
// responsesPosted (nil-safe), optionally journaling CannotComply posts via
// jw (nil disables journaling), and optionally persisting posted/alerted
// state via store (nil disables persistence — RAM-only, pre-WS-4.2
// behavior). initial seeds the maps (e.g. from persist.LoadState after a
// restart); a zero State starts empty exactly as before WS-4.2. Every key
// present in initial.Alerted is treated as already confirmed-persisted
// (LoadState only ever returns confirmed entries — see persist.go).
// initial.Episode (WP-7) additionally restores the active event's
// breach-overlap record AND its activeMRID, so a restart mid-event neither
// re-posts Started nor forgets the breach history its end-of-event code
// (3/8/10) depends on.
func New(p Poster, lfdi, responseSetPath string, clk *utilitytime.Clock, responsesPosted *metrics.Counter, jw *journal.Writer, store *Store, initial State) *Tracker {
	posted := initial.Posted
	if posted == nil {
		posted = make(map[string]uint8)
	}
	alerted := initial.Alerted
	if alerted == nil {
		alerted = make(map[string]bool)
	}
	confirmed := make(map[string]bool, len(alerted))
	for k := range alerted {
		confirmed[k] = true
	}
	rt := &Tracker{
		poster:           p,
		lfdi:             lfdi,
		responseSetPath:  responseSetPath,
		clk:              clk,
		posted:           posted,
		alerted:          alerted,
		confirmedAlerted: confirmed,
		responsesPosted:  responsesPosted,
		jw:               jw,
		store:            store,
	}
	if initial.Episode != nil && initial.Episode.MRID != "" {
		rt.epi = *initial.Episode
		rt.activeMRID = initial.Episode.MRID
	}
	return rt
}

// SetLegacyCannotComplyCode selects the wire vocabulary (WP-7, D5): true
// restores the pre-WP-7 0xF0/Completed-only behavior byte-for-byte (config
// legacy_cannotcomply_code, for gridsim/bench compat); false — the default,
// and a fresh Tracker's zero value — emits standard Table 27 codes. Call at
// wiring time, before the walk loop and MQTT subscriptions start.
func (rt *Tracker) SetLegacyCannotComplyCode(on bool) {
	rt.mu.Lock()
	rt.legacyCannotComply = on
	rt.mu.Unlock()
}

// SetEgressGate wires the WP-7/D4 egress gate postResponse consults before
// transmitting; nil (the default) never suspends. Call at wiring time,
// before the walk loop and MQTT subscriptions start.
func (rt *Tracker) SetEgressGate(g *egress.Gate) {
	rt.mu.Lock()
	rt.gate = g
	rt.mu.Unlock()
}

// AlertCannotComply posts a single CannotComply Response per breach episode.
// The hub edge-triggers the alert; this guard makes a redelivered MQTT message
// idempotent.
//
// Dedupe key (TASK-031): the episode ID when present, falling back to the mRID
// for pre-TASK-031 publishers. The mRID is ALSO recorded and checked
// unconditionally as a safety net: a hub restart mid-episode re-derives a fresh
// episode ID for the same still-breaching control, and keying on episode alone
// would let that re-post — so a matching mRID (this tracker's alerted map
// survives a hub restart because northbound does not restart with it) still
// dedupes, exactly as the pre-TASK-031 mRID-keyed guard did. ClearAlerts wipes
// both keys, so a genuine new episode after a clear re-alerts.
func (rt *Tracker) AlertCannotComply(mrid, episodeID string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.alerted[mrid] || (episodeID != "" && rt.alerted[episodeID]) {
		return
	}
	rt.alerted[mrid] = true
	if episodeID != "" {
		rt.alerted[episodeID] = true
	}

	// WP-7 (D5): mark the breach against the active event's episode record
	// so end-of-event reconciliation can pick 8 vs 10 — regardless of
	// whether the POST below succeeds (the breach happened either way).
	if mrid == rt.activeMRID && rt.activeMRID != "" {
		e := rt.epi
		e.SawBreach = true
		e.BreachActive = true
		rt.setEpisode(e)
	}

	// Wire code (WP-7, D5): standard mode posts 8 (partial completion) at
	// episode onset — the earliest Table 27 signal that execution is
	// degraded; legacy mode keeps the LEXA 0xF0 extension byte-for-byte.
	code := model.ResponsePartialOptOut
	if rt.legacyCannotComply {
		code = model.ResponseCannotComply
	}
	posted := rt.postResponse(mrid, code)

	// WS-4.2: persist ONLY once the POST is confirmed — deliberately AFTER
	// the in-memory mark above, not alongside it. A process crash between
	// the mark and here leaves nothing durable for this key: a fresh
	// Tracker's alerted map won't have it, so a redelivered alert genuinely
	// retries instead of being silently swallowed by a "recorded but never
	// actually sent" ghost dedupe entry — the exact restart-window loss
	// WS-4.2 exists to close. See persist.go's Store.AppendAlerted doc.
	if posted {
		rt.confirmedAlerted[mrid] = true
		if episodeID != "" {
			rt.confirmedAlerted[episodeID] = true
		}
		rt.store.AppendAlerted(mrid, episodeID)
		rt.maybeCompact()
	}

	// TASK-040: journal only the successful POST (the err == nil branch) —
	// this dedupe guard already ensures at most one attempt per episode, so
	// a failed POST here is not retried and must not leave a false
	// "posted" record in the evidence trail.
	if posted && rt.jw != nil {
		// The CSIP responseSet endpoint always answers 201 Created for a
		// well-formed Response (verified against csip-tls-test's
		// sim/gridsim/server.go handleResponsePost; responsePoster.Post
		// itself only ever treats 201/204 as success either way) —
		// responsePoster does not surface the raw status code to its
		// caller, so 201 is recorded here rather than widening the
		// interface for one observability field.
		if ev, everr := journal.NewCannotComplyPostedEvent("northbound", journal.NewCannotComplyPosted(episodeID, mrid, http.StatusCreated)); everr == nil {
			_ = rt.jw.Append(ev)
		}
	}
}

// ClearAlerts ends the current breach episode so a future breach re-alerts.
func (rt *Tracker) ClearAlerts() {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.alerted = make(map[string]bool)
	rt.confirmedAlerted = make(map[string]bool)
	// WP-7 (D5): the breach ended while the event may still be running —
	// SawBreach stays (history for the end-of-event code), only the live
	// flag clears so subsequent Update samples count as compliant.
	if rt.epi.BreachActive {
		e := rt.epi
		e.BreachActive = false
		rt.setEpisode(e)
	}
	rt.store.AppendClear()
	rt.maybeCompact()
}

// setEpisode replaces the active event's breach-overlap record, persisting
// on change only (WP-7, D5). Change-gated so the per-cycle Update sample
// costs nothing once its flag is already set — appends happen on event
// start/end and breach onset/clear/first-sample edges, never per tick.
// Must be called with rt.mu held.
func (rt *Tracker) setEpisode(e EpisodeState) {
	if rt.epi == e {
		return
	}
	rt.epi = e
	rt.store.AppendEpisode(e)
	rt.maybeCompact()
}

// maybeCompact triggers a Store compaction once the append log crosses its
// size threshold. Always called with rt.mu held: Store's own mutex nests
// safely under rt.mu (rt.mu -> store.mu, never the reverse — Store never
// calls back into Tracker), matching how rt.jw.Append is already called
// under rt.mu elsewhere in this file. A compaction failure is logged and
// otherwise ignored — this store is dedupe-hint state, never worth failing
// a live control-adjacent path over (AD-011).
func (rt *Tracker) maybeCompact() {
	if rt.store == nil || !rt.store.NeedsCompact() {
		return
	}
	var epi *EpisodeState
	if rt.epi.MRID != "" {
		e := rt.epi
		epi = &e
	}
	if err := rt.store.Compact(rt.posted, rt.confirmedAlerted, epi); err != nil {
		slog.Warn("lexa-northbound: response-state compaction failed", "path", rt.store.Path, "err", err)
	}
}

// terminalResponse reports whether a response status ends an event's lifecycle:
// no further responses are sent for an mRID once it reaches one of these.
//
// 8 (ResponsePartialOptOut) and 10 (ResponseNoParticipation) are terminal
// HERE because they only ever enter the posted map as end-of-event codes
// (completeActive → endOfEventCode, WP-7/D5) — the episode-ONSET 8 posted by
// AlertCannotComply deliberately bypasses set()/posted[] (it always has,
// back when it was 0xF0), so an onset-8 never freezes a still-running
// event's lifecycle.
func terminalResponse(status uint8) bool {
	switch status {
	case model.ResponseEventCompleted, model.ResponseEventCancelled, model.ResponseEventSuperseded,
		model.ResponsePartialOptOut, model.ResponseNoParticipation:
		return true
	default:
		return false
	}
}

// Update drives the GEN.044 / CORE-022 response state machine for one poll
// cycle: Received(1) on first sighting, Started(2)/Completed(3) as the active
// event begins/ends, Cancelled(6) when the server cancels a received event
// (CORE-022 step 7), and Superseded(7) when an overlapping event wins
// (CORE-023). superseded is the set of currently-superseded event mRIDs from
// scheduler.SupersededMRIDs.
func (rt *Tracker) Update(tree *discovery.ResourceTree, active *scheduler.ActiveControl, superseded map[string]bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	serverNow := rt.clk.ServerNow()

	// Pass 1 — receipt, cancellation, and supersession for every event.
	for _, ps := range tree.Programs {
		if ps.Controls == nil {
			continue
		}
		for _, ctrl := range ps.Controls.DERControl {
			mrid := ctrl.MRID
			last, seen := rt.posted[mrid]

			if ctrl.EventStatus != nil && ctrl.EventStatus.CurrentStatus == 6 {
				// Cancelled. Acknowledge only events we previously received;
				// events that arrive already-cancelled are dropped silently.
				if seen && !terminalResponse(last) {
					rt.set(mrid, model.ResponseEventCancelled)
					if rt.activeMRID == mrid {
						rt.activeMRID = ""
						rt.setEpisode(EpisodeState{}) // WP-7: event over, drop its record
					}
				}
				continue
			}

			if !seen {
				rt.set(mrid, model.ResponseEventReceived)
				last = model.ResponseEventReceived
			}

			if superseded[mrid] && !terminalResponse(last) {
				rt.set(mrid, model.ResponseEventSuperseded)
				if rt.activeMRID == mrid {
					rt.activeMRID = ""
					rt.setEpisode(EpisodeState{}) // WP-7: event over, drop its record
				}
			}
		}
	}

	// Pass 2 — start/complete transitions for the active event.
	if active == nil || active.Source == "default" {
		rt.completeActive()
		return
	}

	if active.MRID != rt.activeMRID {
		rt.completeActive()
		rt.set(active.MRID, model.ResponseEventStarted)
		rt.activeMRID = active.MRID
		// WP-7 (D5): fresh episode record for the new event. Seeded from the
		// alerted map: a breach alert for this mRID that raced ahead of this
		// Started (the hub adopts the control from the SAME walk cycle that
		// posts Started, and its alert rides a separate MQTT goroutine) still
		// counts as overlapping from the start. No compliance sample this
		// first cycle — sampling starts next cycle, so an event that breaches
		// within its first walk interval can still reconcile as 10.
		seed := rt.alerted[active.MRID]
		rt.setEpisode(EpisodeState{MRID: active.MRID, SawBreach: seed, BreachActive: seed})
	} else {
		// WP-7 (D5): one breach-overlap sample per poll cycle while the
		// event stays active — the walk-cadence evidence endOfEventCode
		// reconciles into 3 (never breached), 8 (both compliant and
		// breached cycles observed), or 10 (every observed cycle breached).
		e := rt.epi
		if e.BreachActive {
			e.SawBreach = true
		} else {
			e.SawCompliant = true
		}
		rt.setEpisode(e)
	}

	if active.ValidUntil > 0 && serverNow >= active.ValidUntil {
		rt.completeActive()
	}
}

// completeActive posts the end-of-event Response for the current active
// event unless it has already reached a terminal state (e.g. it was just
// cancelled or superseded), then clears the active mRID and its episode
// record. The code is Completed(3) in legacy mode (pre-WP-7 byte-compat) or
// endOfEventCode's D5 reconciliation (3/8/10) in standard mode.
func (rt *Tracker) completeActive() {
	if rt.activeMRID == "" {
		return
	}
	if !terminalResponse(rt.posted[rt.activeMRID]) {
		rt.set(rt.activeMRID, rt.endOfEventCode())
	}
	rt.activeMRID = ""
	rt.setEpisode(EpisodeState{})
}

// endOfEventCode maps the active event's episode record to its Table 27
// end-of-event Response code (WP-7, D5):
//
//	3  (Completed)        — no breach episode overlapped the event;
//	8  (PartialOptOut)    — breach and compliance both observed (partial);
//	10 (NoParticipation)  — every observed sample of the event's tenure was
//	                        breaching: onset at/before start (or before the
//	                        first post-start cycle) and never cleared.
//
// "Throughout" is judged on walk-cadence evidence (rt.epi's per-cycle
// samples) — the finest granularity the tracker observes; sub-cycle
// compliant intervals are invisible by construction. Legacy mode always
// answers 3, preserving the pre-WP-7 wire behavior byte-for-byte.
func (rt *Tracker) endOfEventCode() uint8 {
	if rt.legacyCannotComply || !rt.epi.SawBreach {
		return model.ResponseEventCompleted
	}
	if rt.epi.SawCompliant {
		return model.ResponsePartialOptOut
	}
	return model.ResponseNoParticipation
}

// ReceiptReject posts a Table 27 receipt-rejection Response for mrid (WP-7,
// D5 "rejected at receipt — never adopted"). Today's only producer is the
// scheduler's plausibility-gate hook (scheduler.Scheduler.RejectHook →
// code 253, ResponseRejectedInvalid). code 252 (ResponseRejectedParam —
// mode the site cannot execute) is the documented seam for WP-9's
// modesSupported/capability knowledge: no component can classify
// "incapable" yet, so nothing calls this with 252 until then.
//
// Deduped per event mRID via the posted map (the hook re-fires every walk
// while the malformed control stays served): a repeat of the same code, or
// any terminal status, suppresses the post. The recorded status is
// non-terminal on purpose — a server that FIXES the event content under
// the same mRID can still be adopted and Start it later. No-op in legacy
// mode (pre-WP-7 posted no rejection Responses — byte-compat) and for an
// empty mrid.
func (rt *Tracker) ReceiptReject(mrid string, code uint8) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.legacyCannotComply || mrid == "" {
		return
	}
	if last, seen := rt.posted[mrid]; seen && (last == code || terminalResponse(last)) {
		return
	}
	rt.set(mrid, code)
}

// set posts a Response and records it as the latest status for mrid. Its
// idempotency contract is unchanged by TASK-040: posted[mrid] is recorded
// unconditionally (the caller never sees, or acts on, postResponse's success
// return) — a failed POST here was already silently swallowed before this
// task, and stays that way.
func (rt *Tracker) set(mrid string, status uint8) {
	rt.postResponse(mrid, status)
	rt.posted[mrid] = status
	// WS-4.2: persisted unconditionally, matching this method's own
	// unconditional in-memory record above (see the doc comment: a failed
	// POST here was already silently swallowed before this task).
	rt.store.AppendPosted(mrid, status)
	rt.maybeCompact()
}

// postResponse POSTs a Response for mrid/status and reports whether it
// succeeded. The bool return is TASK-040's addition — solely so
// AlertCannotComply can gate its journal emit on "the err == nil branch"
// without duplicating this method's marshal/POST/log logic; every other
// call site (set, above) still discards it, preserving the pre-TASK-040
// behavior bit-for-bit.
func (rt *Tracker) postResponse(mrid string, status uint8) bool {
	// WP-7 (D4): server egress suspended (registration-PIN freeze) —
	// transmit nothing. Reached only from the async AlertCannotComply path
	// during a freeze (the walk loop skips Update entirely), so this stays
	// an edge, not a per-tick line.
	if rt.gate.Suspended() {
		slog.Warn("lexa-northbound: response POST suppressed — server egress suspended",
			"reason", rt.gate.Reason(), "mrid", mrid, "status", status)
		return false
	}
	resp := model.Response{
		CreatedDateTime: rt.clk.ServerNow(),
		EndDeviceLFDI:   rt.lfdi,
		Status:          status,
		Subject:         mrid,
	}
	body, err := xml.Marshal(&resp)
	if err != nil {
		log.Printf("lexa-northbound: marshal Response: %v", err)
		return false
	}
	if _, _, err = rt.poster.Post(rt.responseSetPath, body, "application/sep+xml"); err != nil {
		log.Printf("lexa-northbound: POST response (mrid=%s status=%d): %v", mrid, status, err)
		return false
	}
	rt.responsesPosted.Inc()
	names := map[uint8]string{
		1: "Received", 2: "Started", 3: "Completed", 6: "Cancelled", 7: "Superseded",
		model.ResponsePartialOptOut:   "PartialOptOut",
		model.ResponseNoParticipation: "NoParticipation",
		model.ResponseRejectedParam:   "RejectedParam",
		model.ResponseRejectedInvalid: "RejectedInvalid",
		model.ResponseRejectedExpired: "RejectedExpired",
		model.ResponseCannotComply:    "CannotComply",
	}
	name := names[status]
	if name == "" {
		name = fmt.Sprintf("status=%d", status)
	}
	// TASK-045: migrated to slog. Each call is already a lifecycle edge (this
	// function is only reached from Update()'s transition logic, never per-tick).
	slog.Info("lexa-northbound: response posted", "status_name", name, "status", status, "mrid", mrid)
	return true
}
