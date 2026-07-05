package main

import (
	"testing"
	"time"

	"lexa-hub/internal/bus"
)

// The /status last_plan field must relay the hub's actual plan trace. Until
// 2026-07-03 it was a hardcoded empty stub, so the QA harness's decision
// introspection ("decisionLine") read "plan log empty" on every scenario —
// including the battery-wrong-sign C08 run whose diagnosis that artifact
// derailed. These tests pin the relay end to end through the store.
func TestStatusRelaysHubPlan(t *testing.T) {
	store := newStateStore(nil, time.Minute)
	planTs := time.Now().Add(-5 * time.Second).Unix()
	store.onPlanLog(bus.TopicHubPlan, bus.PlanLog{
		Ts: planTs,
		Decisions: []bus.PlanDecision{
			{Rule: "csip/export-limit", Reason: "export over cap", Impact: "curtail pv"},
			{Rule: "safety/battery-direction", Reason: "sign inversion", Impact: "disconnect bat"},
		},
	})

	resp := buildStatus(store.snapshot(), heartbeatStatus{State: heartbeatNever})

	if len(resp.LastPlan.Decisions) != 2 {
		t.Fatalf("last_plan.decisions = %d entries, want 2", len(resp.LastPlan.Decisions))
	}
	if resp.LastPlan.Decisions[0].Rule != "csip/export-limit" ||
		resp.LastPlan.Decisions[1].Impact != "disconnect bat" {
		t.Errorf("decisions not relayed faithfully: %+v", resp.LastPlan.Decisions)
	}
	// The timestamp must be the PLAN's evaluation time, not "now": a frozen
	// last_plan timestamp is the engine-wedge signal the QA harness watches.
	want := time.Unix(planTs, 0).UTC().Format(time.RFC3339)
	if resp.LastPlan.Timestamp != want {
		t.Errorf("last_plan.timestamp = %s, want the plan's own time %s", resp.LastPlan.Timestamp, want)
	}
}

// A heartbeat plan (no decisions) still updates the timestamp, and the
// decision list stays an empty array (not nil — the dashboard iterates it).
func TestStatusHeartbeatPlanEmptyDecisions(t *testing.T) {
	store := newStateStore(nil, time.Minute)
	planTs := time.Now().Unix()
	store.onPlanLog(bus.TopicHubPlan, bus.PlanLog{Ts: planTs})

	resp := buildStatus(store.snapshot(), heartbeatStatus{State: heartbeatNever})

	if resp.LastPlan.Decisions == nil || len(resp.LastPlan.Decisions) != 0 {
		t.Errorf("heartbeat plan must yield an empty (non-nil) decision list, got %#v", resp.LastPlan.Decisions)
	}
	want := time.Unix(planTs, 0).UTC().Format(time.RFC3339)
	if resp.LastPlan.Timestamp != want {
		t.Errorf("heartbeat timestamp = %s, want %s", resp.LastPlan.Timestamp, want)
	}
}

// Before any plan arrives, /status behaves as it always did (empty list).
func TestStatusNoPlanYet(t *testing.T) {
	store := newStateStore(nil, time.Minute)
	resp := buildStatus(store.snapshot(), heartbeatStatus{State: heartbeatNever})
	if len(resp.LastPlan.Decisions) != 0 {
		t.Errorf("no plan received: decisions must be empty, got %+v", resp.LastPlan.Decisions)
	}
}

// TestStatusPlanHeartbeatField pins /status.plan_heartbeat's JSON shape
// (TASK-045): whatever heartbeatStatus buildStatus is given passes through
// verbatim as {"state":..., "age_s":...} — the store/snapshot path (last_plan)
// and the heartbeat path are independent inputs to buildStatus.
func TestStatusPlanHeartbeatField(t *testing.T) {
	store := newStateStore(nil, time.Minute)

	cases := []struct {
		name string
		hb   heartbeatStatus
	}{
		{"never", heartbeatStatus{State: heartbeatNever}},
		{"ok", heartbeatStatus{State: heartbeatOK, AgeS: 3.5}},
		{"stalled", heartbeatStatus{State: heartbeatStalled, AgeS: 120}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := buildStatus(store.snapshot(), c.hb)
			if resp.PlanHeartbeat.State != string(c.hb.State) {
				t.Errorf("plan_heartbeat.state = %q, want %q", resp.PlanHeartbeat.State, c.hb.State)
			}
			if resp.PlanHeartbeat.AgeS != c.hb.AgeS {
				t.Errorf("plan_heartbeat.age_s = %v, want %v", resp.PlanHeartbeat.AgeS, c.hb.AgeS)
			}
		})
	}
}
