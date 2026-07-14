// Package logevent implements the northbound half of the WP-6 LogEvent
// pipeline (standards-buildout A4, BASIC-027/G31/G32): it subscribes the
// hub's lexa/hub/logevent edges (bus.LogEventMsg — CSIP Table 14 DER
// alarm/RTN occurrences) and POSTs each as a csipmodel.LogEvent to the
// EndDevice's LogEventListLink, the href the discovery walk observed on the
// self EndDevice (run.RunOnce feeds it via SetPath, the same shape as
// flowres.Manager.SetRequestPath).
//
// Delivery stance is crash-only (AD-011): a LogEvent the server did not take
// after one retry is DROPPED and counted, never spooled — a missed LogEvent
// is an occurrence record, not state the system runs on, and the alarm's
// level is still visible to the utility via DERStatus alarmStatus (Table 13,
// WP-4). QoS 1 at-least-once redelivery from the bus is made idempotent by
// LogEventMsg.DedupeKey (only confirmed POSTs are recorded, mirroring
// responses.Tracker's confirmedAlerted reasoning: a failed attempt must stay
// retryable).
package logevent

import (
	"encoding/xml"
	"log/slog"
	"sync"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/metrics"
	"lexa-hub/internal/utilitytime"
	model "lexa-proto/csipmodel"
)

// Poster is the subset of tlsclient.WolfSSLFetcher the Manager needs
// (defined at the point of consumption, 05 §2 — same shape as
// responses.Poster/flowres.Poster), keeping HandleLogEvent unit testable
// without a live TLS session.
type Poster interface {
	Post(path string, body []byte, contentType string) ([]byte, string, error)
}

// profileIDCSIP identifies the IEEE 2030.5 CSIP profile in
// LogEvent.profileID (0 = default IEEE 2030.5, 2 = CSIP) — Table 14's alarm
// vocabulary is the CSIP profile's.
const profileIDCSIP uint8 = 2

// logEventPENStandard is the logEventPEN stamped on every posted event: the
// PEN field scopes WHO defined the logEventCode space, and the Table 14
// codes are standard-defined (IEEE 2030.5 §10.10.6 / CSIP), not a private
// vendor space — 0 marks that, reserving real PENs for genuinely
// manufacturer-defined codes (which this repo never emits).
const logEventPENStandard uint32 = 0

// seenCap bounds the dedupe set. LogEvents are rare edges; hitting the cap
// means either a very long-lived process (fine — oldest keys evict FIFO and
// their QoS 1 redelivery window is long gone) or a runaway producer (the
// hub-side rate floor's job, not this one's).
const seenCap = 512

// Manager subscribes lexa/hub/logevent and POSTs csipmodel.LogEvents.
type Manager struct {
	mu      sync.Mutex
	fetcher Poster
	// clk is the shared single-owner utility Clock (AD-004): CreatedTs on the
	// bus message is hub-LOCAL Unix seconds, and the server wants utility
	// time, so posting adds the walk's accumulated offset when one is known.
	clk *utilitytime.Clock

	// path is the EndDevice's LogEventListLink.Href from the most recent
	// successful walk; "" until then (or when the server does not expose the
	// link — LogEvent lists are optional both sides, 2030.5 §9.6).
	path string

	// suspended is the WP-7 D4 egress-suspend seam: while true, every event
	// is dropped (counted + logged), never spooled — "don't feed data to a
	// server we can't authenticate our registration against" (2030.5
	// §6.9.2(c)). TODO(WP-7): nothing sets this yet in this tree — the PIN
	// verify's freeze gate lands with WP-7 and should call
	// SetEgressSuspended from the same place it suspends DER*/MUP egress.
	suspended bool

	// seen holds DedupeKeys whose POST was CONFIRMED (never in-flight/failed
	// attempts — those must stay retryable on redelivery), with seenOrder as
	// its FIFO eviction ring.
	seen      map[string]bool
	seenOrder []string

	posted  *metrics.Counter // lexa_nb_logevents_posted_total; nil-safe
	dropped *metrics.Counter // lexa_nb_logevents_dropped_total; nil-safe
}

// New constructs a Manager that POSTs via f, reading utility time from clk,
// counting confirmed POSTs on posted and retry-exhausted/undeliverable
// events on dropped (both nil-safe, matching metrics.Counter's contract).
func New(f Poster, clk *utilitytime.Clock, posted, dropped *metrics.Counter) *Manager {
	return &Manager{
		fetcher: f,
		clk:     clk,
		seen:    make(map[string]bool),
		posted:  posted,
		dropped: dropped,
	}
}

// SetPath updates the LogEventListLink href to POST to. Called after each
// successful discovery walk (run.RunOnce) with the self EndDevice's link;
// "" when the server does not expose one.
func (m *Manager) SetPath(href string) {
	m.mu.Lock()
	m.path = href
	m.mu.Unlock()
}

// SetEgressSuspended flips the WP-7 D4 egress-suspend gate (see the field
// doc — a suspended Manager drops, it never spools).
func (m *Manager) SetEgressSuspended(s bool) {
	m.mu.Lock()
	m.suspended = s
	m.mu.Unlock()
}

// HandleLogEvent is bus.TopicHubLogEvent's subscription handler: wire it
// directly into mqttutil.Subscribe from main() (the version gate and decode
// live there). Runs on the MQTT subscription goroutine; the POST is
// synchronous here, which is fine — this is not the hub's tick path, and the
// fetcher's own ReadTimeout bounds it.
func (m *Manager) HandleLogEvent(_ string, msg bus.LogEventMsg) {
	if msg.FunctionSet != bus.LogEventFunctionSetDER || !bus.LogEventCodeValid(msg.LogEventCode) {
		// Outside the Table 14 vocabulary this poster exists for — the code
		// space is the standard's, never extended here (2030.5 §11.2).
		slog.Warn("lexa-northbound: logevent outside Table 14 vocabulary — dropped",
			"device", msg.Device, "function_set", msg.FunctionSet, "code", msg.LogEventCode)
		m.dropped.Inc()
		return
	}

	m.mu.Lock()
	if m.suspended {
		m.mu.Unlock()
		slog.Warn("lexa-northbound: logevent dropped — egress suspended (PIN freeze)",
			"device", msg.Device, "code", msg.LogEventCode, "key", msg.DedupeKey)
		m.dropped.Inc()
		return
	}
	if m.seen[msg.DedupeKey] {
		m.mu.Unlock()
		return // QoS 1 redelivery of an already-confirmed POST
	}
	path := m.path
	m.mu.Unlock()

	if path == "" {
		// No walk has observed a LogEventListLink yet (or the server offers
		// none — the function set is optional both sides). Crash-only: drop
		// and count, never spool.
		slog.Warn("lexa-northbound: logevent dropped — no LogEventListLink discovered yet",
			"device", msg.Device, "code", msg.LogEventCode, "key", msg.DedupeKey)
		m.dropped.Inc()
		return
	}

	ev := model.LogEvent{
		CreatedDateTime: m.serverTime(msg.CreatedTs),
		FunctionSet:     msg.FunctionSet,
		LogEventCode:    msg.LogEventCode,
		LogEventID:      msg.LogEventID,
		LogEventPEN:     logEventPENStandard,
		ProfileID:       profileIDCSIP,
	}
	body, err := xml.Marshal(&ev)
	if err != nil {
		slog.Warn("lexa-northbound: marshal LogEvent — dropped", "err", err)
		m.dropped.Inc()
		return
	}

	// Retry once, then drop (crash-only — a missed LogEvent is not state to
	// spool; the fetcher auto-redials a dead session on the first error, so
	// the single retry covers exactly the stale-keepalive case).
	location, postErr := m.post(path, body)
	if postErr != nil {
		location, postErr = m.post(path, body)
	}
	if postErr != nil {
		slog.Warn("lexa-northbound: LogEvent POST failed after retry — dropped",
			"path", path, "device", msg.Device, "code", msg.LogEventCode,
			"key", msg.DedupeKey, "err", postErr)
		m.dropped.Inc()
		return
	}

	m.posted.Inc()
	m.markSeen(msg.DedupeKey)
	slog.Info("lexa-northbound: LogEvent posted",
		"device", msg.Device, "code", msg.LogEventCode, "alarm", msg.Alarm,
		"location", location, "key", msg.DedupeKey)
}

// post is one POST attempt (201 Created + Location is the success shape —
// postResult in tlsclient enforces 2xx and hands the Location header back).
func (m *Manager) post(path string, body []byte) (string, error) {
	_, location, err := m.fetcher.Post(path, body, "application/sep+xml")
	return location, err
}

// serverTime converts a hub-local Unix timestamp to utility/server time by
// adding the discovery walk's accumulated clock offset; before the first
// successful walk (no offset known) the local timestamp is the best — and
// per CSIP §5.2.1.3's 30 s clock discipline, a close — available value.
func (m *Manager) serverTime(localTs int64) int64 {
	if off, ok := m.clk.Offset(); ok {
		return localTs + off
	}
	return localTs
}

// markSeen records a CONFIRMED post's dedupe key, FIFO-evicting past seenCap.
func (m *Manager) markSeen(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seen[key] {
		return
	}
	m.seen[key] = true
	m.seenOrder = append(m.seenOrder, key)
	if len(m.seenOrder) > seenCap {
		delete(m.seen, m.seenOrder[0])
		m.seenOrder = m.seenOrder[1:]
	}
}
