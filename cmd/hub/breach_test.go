package main

import (
	"testing"
	"time"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/orchestrator"
	"lexa-hub/internal/reconcile"
)

var testNow = time.Unix(1700000000, 0)

func breachPlan(mrid string) orchestrator.Plan {
	return orchestrator.Plan{Breach: &orchestrator.ComplianceBreach{
		MRID: mrid, LimitType: "generation", LimitW: 1000, MeasuredW: 4650, ShortfallW: 3650,
	}}
}

func nonConverged(dev, mrid string) bus.ReconcileReport {
	return bus.ReconcileReport{
		Kind: reconcile.ReportNonConvergedBegin.String(), DeviceClass: "battery",
		DeviceID: dev, MRID: mrid, IssuedAt: 1700000042,
	}
}

func converged(dev, mrid string) bus.ReconcileReport {
	return bus.ReconcileReport{
		Kind: reconcile.ReportNonConvergedEnd.String(), DeviceClass: "battery",
		DeviceID: dev, MRID: mrid,
	}
}

// one asserts a single edge alert was returned and returns it.
func one(t *testing.T, alerts []bus.ComplianceAlert) bus.ComplianceAlert {
	t.Helper()
	if len(alerts) != 1 {
		t.Fatalf("expected exactly one edge alert, got %d: %+v", len(alerts), alerts)
	}
	return alerts[0]
}

// none asserts no edge alert was returned.
func none(t *testing.T, alerts []bus.ComplianceAlert) {
	t.Helper()
	if len(alerts) != 0 {
		t.Fatalf("expected no edge alert, got %+v", alerts)
	}
}

// Ported from breachalert_test.go (TestBreachAlert): the component must publish
// once per control episode but NEVER miss a new control's breach because a
// previous control was still breaching (the reject-write/enable-gate-curtail
// flakiness — an mRID-agnostic latch dropped the second scenario's alert).
func TestBreachEpisodes_OnsetSwitchClear(t *testing.T) {
	b := newBreachEpisodes()

	// First breach (control A) → publish Active alert for A, with an episode ID.
	a := one(t, b.OnPlan(breachPlan("A"), testNow))
	if !a.Active || a.MRID != "A" {
		t.Fatalf("first breach should publish an active alert for A, got %+v", a)
	}
	if a.EpisodeID == "" {
		t.Fatalf("onset alert must carry an episode ID, got %+v", a)
	}
	epA := a.EpisodeID
	if !b.Active() {
		t.Fatalf("episode should be active after onset")
	}

	// Same breach continues → no re-publish (one per episode).
	none(t, b.OnPlan(breachPlan("A"), testNow))

	// A DIFFERENT control breaches with NO intervening clear → must publish for B
	// (the bug: an mRID-agnostic flag would stay latched and drop this).
	a = one(t, b.OnPlan(breachPlan("B"), testNow))
	if !a.Active || a.MRID != "B" {
		t.Fatalf("a new control's breach must publish even while one was active, got %+v", a)
	}
	if a.EpisodeID == epA {
		t.Fatalf("an mRID switch must open a NEW episode ID, got same %q", a.EpisodeID)
	}

	// Breach clears → publish a clear.
	a = one(t, b.OnPlan(orchestrator.Plan{Breach: nil}, testNow))
	if a.Active {
		t.Fatalf("clearing breach should publish an inactive alert, got %+v", a)
	}
	if b.Active() {
		t.Fatalf("episode should be inactive after clear")
	}

	// No breach, none active → nothing.
	none(t, b.OnPlan(orchestrator.Plan{Breach: nil}, testNow))
}

// Ported from breachalert_test.go (TestBreachAlert_SafetyPlanHoldsEpisode). A
// fast-loop safety plan carries no breach evaluation; its nil Breach must NOT
// read as a clear edge (2026-07-03 fix), or a wrong-sign disconnect firing
// between economic ticks mid-breach would publish a spurious "compliance
// restored" to the grid server.
//
// This is also the Safety-guard mutation test (05 §8): remove the `if
// plan.Safety { return nil }` guard in OnPlan and this test fails — the safety
// plan's nil Breach would clear the still-open episode A.
func TestBreachEpisodes_SafetyPlanHoldsEpisode(t *testing.T) {
	b := newBreachEpisodes()
	one(t, b.OnPlan(breachPlan("A"), testNow)) // open episode A

	// Safety plan mid-episode: no edge, episode preserved.
	none(t, b.OnPlan(orchestrator.Plan{Safety: true}, testNow))
	if !b.Active() {
		t.Fatalf("safety plan must preserve the active episode")
	}

	// A following economic tick with the breach still present must NOT re-alert
	// (proves the safety plan did not silently clear the evidence).
	none(t, b.OnPlan(breachPlan("A"), testNow))

	// Safety plan with no episode active is equally a no-op.
	b2 := newBreachEpisodes()
	none(t, b2.OnPlan(orchestrator.Plan{Safety: true}, testNow))
	if b2.Active() {
		t.Fatalf("safety plan with no episode must stay inactive")
	}
}

// A device that refuses while the meter is fine opens an episode from the
// reconciler evidence alone, and clears when it converges.
func TestBreachEpisodes_ReconcilerOnlyEpisode(t *testing.T) {
	b := newBreachEpisodes()

	a := one(t, b.OnReport(nonConverged("bat0", "X"), testNow))
	if !a.Active || a.MRID != "X" || a.EpisodeID == "" {
		t.Fatalf("reconciler non-convergence should open an episode for X, got %+v", a)
	}

	// Redelivered retained NonConvergedBegin (restart-shaped replay) → no new edge.
	none(t, b.OnReport(nonConverged("bat0", "X"), testNow))

	// Device converges → clear.
	a = one(t, b.OnReport(converged("bat0", "X"), testNow))
	if a.Active {
		t.Fatalf("device convergence should clear the episode, got %+v", a)
	}
}

// Both sources reporting the same control form ONE episode: one begin, one end
// only when BOTH have cleared.
func TestBreachEpisodes_BothSourcesOneEpisode(t *testing.T) {
	b := newBreachEpisodes()

	// Optimizer opens episode A.
	a := one(t, b.OnPlan(breachPlan("A"), testNow))
	epA := a.EpisodeID

	// Device also reports non-convergence for the same control → NO new edge
	// (same episode).
	none(t, b.OnReport(nonConverged("bat0", "A"), testNow))

	// Optimizer clears, but the device is still non-converged → episode HOLDS,
	// no clear edge.
	none(t, b.OnPlan(orchestrator.Plan{Breach: nil}, testNow))
	if !b.Active() {
		t.Fatalf("episode must stay open while the device is still non-converged")
	}

	// Device converges → NOW the episode clears (one end), same episode ID.
	a = one(t, b.OnReport(converged("bat0", "A"), testNow))
	if a.Active || a.EpisodeID != epA {
		t.Fatalf("clear should close the same episode %q, got %+v", epA, a)
	}
}

// An empty-mRID device fault (no active control) is NOT a CannotComply: it must
// not open a CSIP episode.
func TestBreachEpisodes_EmptyMRIDNoEpisode(t *testing.T) {
	b := newBreachEpisodes()
	none(t, b.OnReport(nonConverged("bat0", ""), testNow))
	if b.Active() {
		t.Fatalf("empty-mRID device fault must not open an episode")
	}
}

// The episode ID formed at onset is reused for the whole episode across sources.
func TestBreachEpisodes_EpisodeIDStable(t *testing.T) {
	b := newBreachEpisodes()
	open := one(t, b.OnPlan(breachPlan("A"), testNow))
	clear := one(t, b.OnPlan(orchestrator.Plan{Breach: nil}, testNow))
	if open.EpisodeID != clear.EpisodeID || open.EpisodeID == "" {
		t.Fatalf("onset and clear must carry the same non-empty episode ID, got %q / %q",
			open.EpisodeID, clear.EpisodeID)
	}
}
