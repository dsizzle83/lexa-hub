package main

import (
	"testing"

	"lexa-hub/internal/orchestrator"
)

func breachPlan(mrid string) orchestrator.Plan {
	return orchestrator.Plan{Breach: &orchestrator.ComplianceBreach{
		MRID: mrid, LimitType: "generation", LimitW: 1000, MeasuredW: 4650, ShortfallW: 3650,
	}}
}

// breachAlert must publish once per control episode but NEVER miss a new control's
// breach because a previous control was still breaching (the reject-write/
// enable-gate-curtail flakiness: an mRID-agnostic latch dropped the second
// scenario's alert).
func TestBreachAlert(t *testing.T) {
	cur := ""

	// First breach (control A) → publish Active alert for A.
	a, cur := breachAlert(cur, breachPlan("A"))
	if a == nil || !a.Active || a.MRID != "A" {
		t.Fatalf("first breach should publish an active alert for A, got %+v", a)
	}
	if cur != "A" {
		t.Fatalf("active mRID = %q, want A", cur)
	}

	// Same breach continues → no re-publish (one per episode).
	a, cur = breachAlert(cur, breachPlan("A"))
	if a != nil {
		t.Errorf("a continuing breach must not re-publish, got %+v", a)
	}

	// A DIFFERENT control breaches with NO intervening clear → must publish for B
	// (this is the bug: an mRID-agnostic flag would stay latched and drop this).
	a, cur = breachAlert(cur, breachPlan("B"))
	if a == nil || !a.Active || a.MRID != "B" {
		t.Fatalf("a new control's breach must publish even while one was active, got %+v", a)
	}
	if cur != "B" {
		t.Fatalf("active mRID = %q, want B", cur)
	}

	// Breach clears → publish a clear, reset.
	a, cur = breachAlert(cur, orchestrator.Plan{Breach: nil})
	if a == nil || a.Active {
		t.Fatalf("clearing breach should publish an inactive alert, got %+v", a)
	}
	if cur != "" {
		t.Errorf("active mRID = %q, want empty after clear", cur)
	}

	// No breach, none active → nothing.
	if a, _ := breachAlert("", orchestrator.Plan{Breach: nil}); a != nil {
		t.Errorf("no breach and none active must publish nothing, got %+v", a)
	}
}
