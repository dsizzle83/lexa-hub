package main

import (
	"log/slog"
	"sync"
	"time"

	"lexa-hub/internal/metrics"
)

// heartbeatState is the JSON-facing value of a planHeartbeat evaluation.
type heartbeatState string

const (
	// heartbeatNever: no PlanLog (bus.PlanLog, TopicHubPlan) has arrived
	// since this process started. Silent by design — see planHeartbeat's
	// doc and TASK-045's "Alarming on never" pitfall: a bench bring-up
	// order where lexa-api starts before lexa-hub, or a hub that has simply
	// never run yet, must not page.
	heartbeatNever heartbeatState = "never"
	// heartbeatOK: a PlanLog arrived within stallAfter of now.
	heartbeatOK heartbeatState = "ok"
	// heartbeatStalled: more than stallAfter has elapsed since the last
	// PlanLog ARRIVAL (see planHeartbeat's doc for why arrival, not Ts).
	heartbeatStalled heartbeatState = "stalled"
)

// heartbeatStatus is a point-in-time view of a planHeartbeat, used both to
// build /status.plan_heartbeat and to drive the metrics gauges.
type heartbeatStatus struct {
	State heartbeatState
	AgeS  float64
}

// planHeartbeat turns the retained lexa/hub/plan heartbeat (bus.PlanLog,
// published by cmd/hub's planObserver on EVERY engine pass — economic tick
// and safety tick alike) into an ACTED-ON signal (TASK-045; architecture
// review §11: "you now publish a plan heartbeat; nothing consumes it for
// action"; internal/bus/topics.go's TopicHubPlan doc: "a hub whose /status
// last_plan timestamp stops advancing has a wedged control loop").
//
// Three states, not two, because a bench bring-up race (lexa-api starting
// before lexa-hub) or a hub that has simply never run must never page:
//   - never:   no PlanLog has arrived since this process started.
//   - ok:      a PlanLog arrived within stallAfter of now.
//   - stalled: more than stallAfter has elapsed since the last arrival.
//
// Arrival time, not PlanLog.Ts: the retained message's own timestamp is the
// HUB's clock, which the Bench Replay driver warps
// (POST :11112/admin/clock) and a future NTP correction (TASK-037) could
// step — keying the stall detector off Ts would make it chase the hub's
// clock instead of the wall-clock question it exists to answer ("is the bus
// still delivering fresh plans, right now, from lexa-api's own point of
// view"). PlanLog.Ts is still tracked (lastPlanTs) and available for a
// future /status field or diagnosis, but never drives the state machine.
//
// Retained-topic redelivery on reconnect: when lexa-api (re)subscribes,
// Mosquitto immediately redelivers the retained lexa/hub/plan message. That
// is a fresh ARRIVAL (the bus is alive) even though the plan's own Ts may be
// old (the hub's last pass before lexa-api went away) — exactly the
// behavior this type wants: "is the bus delivering" is answered by arrival,
// not by how stale the redelivered payload's content happens to be.
type planHeartbeat struct {
	mu         sync.Mutex
	stallAfter time.Duration

	seen       bool
	lastPlanTs int64
	arrivedAt  time.Time

	// alarmed is the state this heartbeat last LOGGED an edge for (edge
	// detection in tick, below); zero value "" is treated as heartbeatNever
	// before the first tick. Distinct from evaluate()'s live result, which
	// callers (the /status handler) may compute more often than the 5 s
	// ticker calls tick.
	alarmed heartbeatState

	// Metric hooks (nil-safe per metrics.Gauge's doc), wired by main().
	stalledGauge *metrics.Gauge // lexa_api_plan_heartbeat_stalled (0/1)
	ageGauge     *metrics.Gauge // lexa_api_plan_heartbeat_age_seconds
}

// newPlanHeartbeat returns a planHeartbeat in the "never" state, alarming
// after stallAfter of arrival silence.
func newPlanHeartbeat(stallAfter time.Duration) *planHeartbeat {
	return &planHeartbeat{stallAfter: stallAfter}
}

// onPlanLog records one PlanLog arrival. now must be the ARRIVAL time
// (time.Now()) — never the PlanLog's own Ts. Called from the TopicHubPlan
// MQTT subscription handler in main.go, on every message (retained-redeliver
// included).
func (h *planHeartbeat) onPlanLog(planTs int64, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seen = true
	h.lastPlanTs = planTs
	h.arrivedAt = now
}

// evaluate computes the current heartbeat state as of now, with no side
// effects (no logging, no metrics) — safe to call from an HTTP handler on
// every request, in addition to the periodic ticker's tick calls below, so
// /status always reflects the live state rather than a up-to-5s-old cache.
func (h *planHeartbeat) evaluate(now time.Time) heartbeatStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.seen {
		return heartbeatStatus{State: heartbeatNever}
	}
	age := now.Sub(h.arrivedAt)
	st := heartbeatOK
	if age > h.stallAfter {
		st = heartbeatStalled
	}
	return heartbeatStatus{State: st, AgeS: age.Seconds()}
}

// tick evaluates the heartbeat as of now, updates the metric gauges
// (unconditionally — a gauge must track the live value on every call, not
// just on a state edge), and emits exactly one edge-triggered slog alarm on
// a transition INTO stalled (from ok or from never — see below) and exactly
// one on a transition OUT of stalled back to ok (recovery). never→ok is NOT
// alarmed: that is the expected, unremarkable first heartbeat, not a
// recovery from anything. Intended to be called by a periodic (5 s) ticker
// in main.go; evaluate (above) is the side-effect-free read path for
// /status.
func (h *planHeartbeat) tick(now time.Time) heartbeatStatus {
	st := h.evaluate(now)

	h.mu.Lock()
	prev := h.alarmed
	if prev == "" {
		prev = heartbeatNever
	}
	h.alarmed = st.State
	h.mu.Unlock()

	if st.State == heartbeatStalled {
		h.stalledGauge.Set(1)
	} else {
		h.stalledGauge.Set(0)
	}
	h.ageGauge.Set(st.AgeS)

	switch {
	case prev != heartbeatStalled && st.State == heartbeatStalled:
		// Covers both ok→stalled and the degenerate never→stalled (a
		// pathologically small plan_stall_after_s could in principle skip
		// straight past "ok" between two 5 s ticks) — either way, once a
		// plan has been seen at all, a stall is a stall and must not stay
		// silent just because this is the first tick to observe it.
		slog.Warn("lexa-api: plan heartbeat stalled", "age_s", st.AgeS)
	case prev == heartbeatStalled && st.State == heartbeatOK:
		slog.Info("lexa-api: plan heartbeat recovered", "age_s", st.AgeS)
	}
	return st
}
