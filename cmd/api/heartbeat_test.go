package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"lexa-hub/internal/metrics"
)

func newTestHeartbeat(stallAfter time.Duration) *planHeartbeat {
	h, _ := newTestHeartbeatWithRegistry(stallAfter)
	return h
}

func newTestHeartbeatWithRegistry(stallAfter time.Duration) (*planHeartbeat, *metrics.Registry) {
	h := newPlanHeartbeat(stallAfter)
	reg := metrics.New()
	h.stalledGauge = reg.Gauge("test_plan_heartbeat_stalled")
	h.ageGauge = reg.Gauge("test_plan_heartbeat_age_seconds")
	return h, reg
}

// TestHeartbeatNeverBeforeAnyPlanLog pins the INCONCLUSIVE-safe "never"
// state: a fresh heartbeat that has never seen a PlanLog must report "never"
// indefinitely, not "stalled" — a bench bring-up race (api before hub) must
// not page (TASK-045 "Alarming on never" pitfall).
func TestHeartbeatNeverBeforeAnyPlanLog(t *testing.T) {
	h := newTestHeartbeat(75 * time.Second)
	now := time.Unix(1000, 0)

	st := h.evaluate(now)
	if st.State != heartbeatNever {
		t.Fatalf("state = %q, want %q", st.State, heartbeatNever)
	}
	// Even far in the future, still "never" — nothing has ever arrived.
	st = h.evaluate(now.Add(10 * time.Hour))
	if st.State != heartbeatNever {
		t.Fatalf("state after 10h with no arrival = %q, want %q", st.State, heartbeatNever)
	}
}

// TestHeartbeatOkThenStalledThenRecovered walks the full never→ok→stalled→ok
// cycle with an injected clock, pinning the stall bound and the arrival-time
// (not PlanLog.Ts) basis.
func TestHeartbeatOkThenStalledThenRecovered(t *testing.T) {
	stallAfter := 75 * time.Second
	h := newTestHeartbeat(stallAfter)
	base := time.Unix(100000, 0)

	// A PlanLog arrives with an intentionally STALE Ts (the hub's own clock
	// is old/warped, e.g. under bench-replay clock warp) — the heartbeat
	// must key off arrival, not this value.
	h.onPlanLog(base.Add(-10*time.Hour).Unix(), base)

	st := h.evaluate(base.Add(10 * time.Second))
	if st.State != heartbeatOK {
		t.Fatalf("10s after arrival: state = %q, want ok", st.State)
	}

	// Just past the stall bound.
	past := base.Add(stallAfter + time.Second)
	st = h.evaluate(past)
	if st.State != heartbeatStalled {
		t.Fatalf("past stallAfter: state = %q, want stalled (age=%v)", st.State, st.AgeS)
	}

	// A fresh arrival recovers it immediately.
	h.onPlanLog(past.Unix(), past)
	st = h.evaluate(past.Add(time.Second))
	if st.State != heartbeatOK {
		t.Fatalf("after fresh arrival: state = %q, want ok", st.State)
	}
}

// TestHeartbeatTickEdgeTriggeredExactlyOnce drives tick() across a
// never→ok→stalled→ok→stalled sequence and asserts the slog alarm fires
// exactly once per transition direction — not once per tick while the state
// persists (acceptance criteria: "exactly one line each way").
func TestHeartbeatTickEdgeTriggeredExactlyOnce(t *testing.T) {
	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(restore)

	stallAfter := 10 * time.Second
	h := newTestHeartbeat(stallAfter)
	base := time.Unix(200000, 0)

	// never: no alarm.
	h.tick(base)
	if strings.Contains(buf.String(), "heartbeat") {
		t.Fatalf("never state alarmed: %q", buf.String())
	}

	// ok: still no alarm (never→ok is silent).
	h.onPlanLog(base.Unix(), base)
	h.tick(base.Add(time.Second))
	if strings.Contains(buf.String(), "heartbeat") {
		t.Fatalf("never->ok alarmed: %q", buf.String())
	}

	// ok held across several ticks: no repeated alarm.
	h.tick(base.Add(2 * time.Second))
	h.tick(base.Add(3 * time.Second))
	if strings.Contains(buf.String(), "heartbeat") {
		t.Fatalf("steady ok alarmed: %q", buf.String())
	}

	// stalled: exactly one "stalled" line, even across repeated ticks while
	// it persists.
	stalledAt := base.Add(stallAfter + 2*time.Second)
	h.tick(stalledAt)
	h.tick(stalledAt.Add(5 * time.Second))
	h.tick(stalledAt.Add(10 * time.Second))
	stalledCount := strings.Count(buf.String(), "plan heartbeat stalled")
	if stalledCount != 1 {
		t.Fatalf("stalled alarm count = %d, want 1; log=%q", stalledCount, buf.String())
	}

	// recovered: exactly one "recovered" line.
	recoverAt := stalledAt.Add(15 * time.Second)
	h.onPlanLog(recoverAt.Unix(), recoverAt)
	h.tick(recoverAt)
	h.tick(recoverAt.Add(time.Second))
	recoveredCount := strings.Count(buf.String(), "plan heartbeat recovered")
	if recoveredCount != 1 {
		t.Fatalf("recovered alarm count = %d, want 1; log=%q", recoveredCount, buf.String())
	}

	// A second stall cycle re-alarms (edge-triggered per episode, not
	// latched forever after the first).
	secondStallAt := recoverAt.Add(stallAfter + 2*time.Second)
	h.tick(secondStallAt)
	if strings.Count(buf.String(), "plan heartbeat stalled") != 2 {
		t.Fatalf("second stall episode did not re-alarm; log=%q", buf.String())
	}
}

// TestHeartbeatTickGaugesTrackLiveState pins the metric side of tick(): the
// stalled gauge is 0/1 and the age gauge reflects arrival age, on every
// call (not just on an alarm edge).
func TestHeartbeatTickGaugesTrackLiveState(t *testing.T) {
	h, reg := newTestHeartbeatWithRegistry(10 * time.Second)
	base := time.Unix(300000, 0)

	h.tick(base) // never: gauges default to 0, which is correct here too.
	if !strings.Contains(reg.Format(), "test_plan_heartbeat_stalled 0") {
		t.Fatalf("never: stalled gauge not 0: %s", reg.Format())
	}

	h.onPlanLog(base.Unix(), base)
	h.tick(base.Add(2 * time.Second)) // ok
	out := reg.Format()
	if !strings.Contains(out, "test_plan_heartbeat_stalled 0") {
		t.Fatalf("ok: stalled gauge not 0: %s", out)
	}
	if !strings.Contains(out, "test_plan_heartbeat_age_seconds 2") {
		t.Fatalf("ok: age gauge not ~2: %s", out)
	}

	h.tick(base.Add(20 * time.Second)) // stalled (stallAfter=10s)
	out = reg.Format()
	if !strings.Contains(out, "test_plan_heartbeat_stalled 1") {
		t.Fatalf("stalled: stalled gauge not 1: %s", out)
	}
	if !strings.Contains(out, "test_plan_heartbeat_age_seconds 20") {
		t.Fatalf("stalled: age gauge not ~20: %s", out)
	}
}
