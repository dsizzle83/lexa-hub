package main

// uplink.go is unit 2.2's collector half: it subscribes the §1.4 local-bus
// read set and re-frames every message into the spool as a small, opaque,
// self-describing record. It never interprets a payload beyond stamping the
// stream, the arrival time, and the source topic.
//
// # Raw passthrough (why json.RawMessage, not typed decode + re-marshal)
//
// The collector subscribes each topic as json.RawMessage via mqttutil.Subscribe.
// json.RawMessage captures the producer's payload BYTE-FOR-BYTE (verified: a
// json.Unmarshal into RawMessage stores the value verbatim, no re-compaction),
// so the ORIGINAL bus.Envelope — its "v" version and EVERY field, including
// forward-compatible keys this service's structs don't know — rides opaque to
// the cloud, which decodes exactly what the producer published. A typed decode
// + re-marshal would silently drop unknown fields and could reorder/reformat
// the bytes; for a store-and-forward pipe that never acts on the content, that
// is strictly worse.
//
// This follows the SPIRIT of cmd/northbound's one raw mc.Subscribe (raw
// payload, own version gate) but goes THROUGH mqttutil.Subscribe rather than
// paho's mc.Subscribe directly, on purpose:
//
//   - mqttutil.Subscribe records the subscription in the client's subRegistry,
//     so it is REPLAYED after a broker reconnect. A bare mc.Subscribe is not:
//     with CleanSession=true the broker keeps no session and paho re-sends no
//     SUBSCRIBE, leaving the client permanently deaf after a broker restart.
//     That is tolerable for northbound's single FR-request topic but not for a
//     mission-critical multi-topic collector whose silence would stop all
//     uplink data.
//   - mqttutil.Subscribe still applies the AD-006 version gate (bus.CheckVersion
//     on m.Topic()), so a wrong-version or non-JSON payload is rejected/alarmed
//     BEFORE it consumes spool budget.
//   - The Finite() defense-in-depth is intentionally skipped here (json.RawMessage
//     has no Finite() method, so the assertion is a no-op): cloudlink never
//     interprets a numeric payload, and the cloud re-validates on decode.
//     Producers never emit NaN/Inf anyway (house *float64 rule), so there is
//     nothing on the wire for a Finite() check to catch.

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/mqttutil"
	"lexa-hub/internal/spool"
)

// Stream names (the frame's "stream" field and the per-stream seq namespace).
const (
	streamEvents    = "events"
	streamHealth    = "health"
	streamPlan      = "plan"
	streamTelemetry = "telemetry"
)

// allStreams is every stream the batcher keeps a persistent seq for. Order is
// not significant.
var allStreams = []string{streamEvents, streamHealth, streamPlan, streamTelemetry}

// Priority classes (spool p0/p1/p2). events=0 drains first and is dropped last;
// telemetry=2 is dropped-oldest first under spool pressure. plan and health
// share P1 (the spool has three classes for four streams — the batcher splits a
// P1 peek by stream; see batch.go).
const (
	prioEvents     = 0 // events
	prioPlanHealth = 1 // plan + health
	prioTelemetry  = 2 // telemetry
)

// collectorSpec maps one subscribed local-bus topic to its uplink stream and
// spool priority. This table IS the §2.4 stream/priority contract; the
// collectors test asserts it verbatim.
type collectorSpec struct {
	topic    string
	stream   string
	priority int
}

// collectorSpecs is the §1.4 read set with each topic's stream + priority.
//
// lexa/cloudlink/status is deliberately absent: this service AUTHORS it
// (status.go), so subscribing it would spool our own retained status in an
// ever-growing self-loop. Its content is already reflected cloud-side by the
// health stream's other members plus the direct MQTT retain.
func collectorSpecs() []collectorSpec {
	return []collectorSpec{
		// telemetry (P2) — highest volume, sacrificed first under pressure.
		{bus.SubMeasurements, streamTelemetry, prioTelemetry},
		{bus.SubBattMetrics, streamTelemetry, prioTelemetry},
		{bus.SubEVSEState, streamTelemetry, prioTelemetry},
		// plan (P1).
		{bus.TopicHubPlan, streamPlan, prioPlanHealth},
		// health (P1).
		{bus.TopicNorthboundCertStatus, streamHealth, prioPlanHealth},
		{bus.TopicHubMode, streamHealth, prioPlanHealth},
		// events (P0) — never dropped while any lower-priority record remains.
		{bus.TopicCSIPComplianceAlert, streamEvents, prioEvents},
		{bus.SubReconcileReport, streamEvents, prioEvents},
		{bus.TopicIntentResult, streamEvents, prioEvents},
		{bus.TopicScanResult, streamEvents, prioEvents},
	}
}

// recordFrame is the per-message self-describing frame stored as a spool
// record's payload. The original bus bytes ride verbatim in Payload
// (json.RawMessage marshals back out unchanged), so the cloud sees exactly
// what the producer published.
type recordFrame struct {
	Topic   string          `json:"topic"`
	Ts      int64           `json:"ts"` // arrival time on THIS box (clock-warp-safe, like planHeartbeat)
	Payload json.RawMessage `json:"payload"`
}

// uplink owns the local-bus collectors.
type uplink struct {
	mc     mqtt.Client
	spool  *spool.Spool
	notify chan<- struct{} // poked (non-blocking) on a P0 append so events drain promptly
	m      *cloudlinkMetrics

	now func() time.Time // seam for tests
	rl  *rlLogger        // rate-limited WARN for append/marshal faults (never crash — AD-011)
}

func newUplink(mc mqtt.Client, sp *spool.Spool, notify chan<- struct{}, m *cloudlinkMetrics) *uplink {
	return &uplink{
		mc:     mc,
		spool:  sp,
		notify: notify,
		m:      m,
		now:    time.Now,
		rl:     &rlLogger{gap: 30 * time.Second},
	}
}

// subscribeAll wires every collector. A subscribe error is logged, not fatal
// (AD-011): the subRegistry replays on the next (re)connect, and a persistently
// unsubscribable topic degrades that one stream rather than crash-looping the
// service.
func (u *uplink) subscribeAll() {
	for _, spec := range collectorSpecs() {
		// T is inferred as json.RawMessage — the raw-passthrough seam (see the
		// package doc atop this file for why raw, and why via mqttutil).
		if err := mqttutil.Subscribe(u.mc, spec.topic, u.handler(spec.stream, spec.priority)); err != nil {
			slog.Error("cloudlink: subscribe failed", "topic", spec.topic, "err", err)
		}
	}
}

// handler returns the message handler for one (stream, priority): re-frame →
// spool.Append → (for events) poke the batcher.
func (u *uplink) handler(stream string, priority int) func(topic string, raw json.RawMessage) {
	return func(topic string, raw json.RawMessage) {
		rf := recordFrame{Topic: topic, Ts: u.now().Unix(), Payload: raw}
		payload, err := json.Marshal(rf)
		if err != nil {
			// Effectively unreachable (RawMessage always re-marshals), but never
			// crash on a bad payload — count nothing new, just rate-limited WARN.
			u.rl.warn("cloudlink: reframe marshal failed", "topic", topic, "err", err)
			return
		}
		rec := spool.Record{Stream: stream, Priority: priority, Ts: u.now().Unix(), Payload: payload}
		if err := u.spool.Append(rec); err != nil {
			// Append counts its own drops/errors (spool.Metrics wired via
			// spoolMetrics); we only add a rate-limited WARN. A near-budget
			// eviction returns nil (counted, not errored), so this fires only on
			// a real I/O fault or an over-budget single record.
			u.rl.warn("cloudlink: spool append failed", "topic", topic, "err", err)
			return
		}
		if priority == prioEvents {
			// Wake the batcher so P0 events drain within the coalesce window
			// instead of waiting a whole telemetry interval. Non-blocking: a
			// full buffer already means "drain pending".
			select {
			case u.notify <- struct{}{}:
			default:
			}
		}
	}
}

// rlLogger is a minimal time-based rate-limited WARN logger shared by the
// collectors: at most one line per gap, so a storm of append faults cannot
// blow the journal budget (TASK-009 / FLASH_BUDGET.md). Safe for the
// concurrent paho callback goroutines.
type rlLogger struct {
	mu   sync.Mutex
	last time.Time
	gap  time.Duration
}

func (r *rlLogger) warn(msg string, args ...any) {
	r.mu.Lock()
	now := time.Now()
	if !r.last.IsZero() && now.Sub(r.last) < r.gap {
		r.mu.Unlock()
		return
	}
	r.last = now
	r.mu.Unlock()
	slog.Warn(msg, args...)
}
