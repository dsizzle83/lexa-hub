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
	return &Tracker{
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
	posted := rt.postResponse(mrid, model.ResponseCannotComply)

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
	rt.store.AppendClear()
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
	if err := rt.store.Compact(rt.posted, rt.confirmedAlerted); err != nil {
		slog.Warn("lexa-northbound: response-state compaction failed", "path", rt.store.Path, "err", err)
	}
}

// terminalResponse reports whether a response status ends an event's lifecycle:
// no further responses are sent for an mRID once it reaches one of these.
func terminalResponse(status uint8) bool {
	switch status {
	case model.ResponseEventCompleted, model.ResponseEventCancelled, model.ResponseEventSuperseded:
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
	}

	if active.ValidUntil > 0 && serverNow >= active.ValidUntil {
		rt.completeActive()
	}
}

// completeActive posts Completed(3) for the current active event unless it has
// already reached a terminal state (e.g. it was just cancelled or superseded),
// then clears the active mRID.
func (rt *Tracker) completeActive() {
	if rt.activeMRID == "" {
		return
	}
	if !terminalResponse(rt.posted[rt.activeMRID]) {
		rt.set(rt.activeMRID, model.ResponseEventCompleted)
	}
	rt.activeMRID = ""
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
	names := map[uint8]string{1: "Received", 2: "Started", 3: "Completed", 6: "Cancelled", 7: "Superseded", model.ResponseCannotComply: "CannotComply"}
	name := names[status]
	if name == "" {
		name = fmt.Sprintf("status=%d", status)
	}
	// TASK-045: migrated to slog. Each call is already a lifecycle edge (this
	// function is only reached from Update()'s transition logic, never per-tick).
	slog.Info("lexa-northbound: response posted", "status_name", name, "status", status, "mrid", mrid)
	return true
}
