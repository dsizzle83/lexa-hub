package main

import (
	"testing"
	"time"

	"lexa-hub/internal/bus"
)

func f64p(v float64) *float64 { return &v }

func TestStaleMeters(t *testing.T) {
	now := time.Now()
	mk := func(meterChanged, meterArrived, solarChanged time.Time) snapshot {
		return snapshot{
			now:        now,
			staleAfter: 60 * time.Second,
			devices: map[string]deviceSnap{
				"meter": {Name: "meter", Role: "meter", W: f64p(1000), WChangedAt: meterChanged, UpdatedAt: meterArrived},
				"pv":    {Name: "pv", Role: "inverter", W: f64p(3000), WChangedAt: solarChanged, UpdatedAt: now},
			},
		}
	}

	// Frozen value (W unchanged 25s) + fresh arrivals + solar moving → flagged.
	frozen := mk(now.Add(-25*time.Second), now.Add(-1*time.Second), now.Add(-2*time.Second))
	if got := frozen.staleMeters(); len(got) != 1 || got[0] != "meter" {
		t.Errorf("frozen meter should be flagged, got %v", got)
	}
	// Meter W changed recently → healthy, not flagged.
	healthy := mk(now.Add(-2*time.Second), now.Add(-1*time.Second), now.Add(-2*time.Second))
	if got := healthy.staleMeters(); len(got) != 0 {
		t.Errorf("recently-changed meter must not be flagged, got %v", got)
	}
	// Steady world (solar not moving) → not flagged even if the meter is flat:
	// a legitimately steady grid must not raise a false INV-STALE alarm.
	steady := mk(now.Add(-25*time.Second), now.Add(-1*time.Second), now.Add(-30*time.Second))
	if got := steady.staleMeters(); len(got) != 0 {
		t.Errorf("flat meter in a steady world must not be flagged, got %v", got)
	}
	// No fresh arrivals (dead publisher) → not a frozen-VALUE case (arrival-staleness
	// is handled separately by the optimizer's time-based check).
	dead := mk(now.Add(-25*time.Second), now.Add(-120*time.Second), now.Add(-2*time.Second))
	if got := dead.staleMeters(); len(got) != 0 {
		t.Errorf("a meter with no fresh arrivals is not frozen-value, got %v", got)
	}
}

func TestEvseStale(t *testing.T) {
	now := time.Now()
	snap := func(sessionActive bool, updatedAgo time.Duration) evseSnap {
		return evseSnap{State: bus.EVSEState{SessionActive: sessionActive}, UpdatedAt: now.Add(-updatedAgo)}
	}
	if !snap(true, 40*time.Second).stale(now) {
		t.Error("an active session silent for 40s should be stale")
	}
	if snap(true, 5*time.Second).stale(now) {
		t.Error("an active session updated 5s ago is fresh")
	}
	if snap(false, 120*time.Second).stale(now) {
		t.Error("an idle EVSE (no session) is not 'stale telemetry'")
	}
}

// /status must stop reporting a CSIP control once it is past ValidUntil (+
// grace) in server time: during a WAN outage nobody clears the retained bus
// message, and reporting it as active makes the hub look like it is enforcing
// withdrawn authority when the orchestrator has already dropped it.
func TestBuildStatus_ExpiredControlNotReported(t *testing.T) {
	now := time.Now()
	live := &bus.ActiveControl{Source: "event", MRID: "M-live", ValidUntil: now.Unix() + 300}
	snap := snapshot{now: now, csipControl: live}
	if got := buildStatus(snap); got.CSIPControl == nil || got.CSIPControl.MRID != "M-live" {
		t.Fatalf("unexpired control must be reported, got %+v", got.CSIPControl)
	}

	stale := &bus.ActiveControl{Source: "event", MRID: "M-stale", ValidUntil: now.Unix() - csipReportGraceS - 5}
	snap = snapshot{now: now, csipControl: stale}
	if got := buildStatus(snap); got.CSIPControl != nil {
		t.Errorf("control %ds past ValidUntil+grace still reported: %+v", csipReportGraceS+5, got.CSIPControl)
	}

	// Within the grace window: still reported (covers the hub's own debounce).
	graceful := &bus.ActiveControl{Source: "event", MRID: "M-grace", ValidUntil: now.Unix() - 5}
	snap = snapshot{now: now, csipControl: graceful}
	if got := buildStatus(snap); got.CSIPControl == nil {
		t.Error("control within the report grace must still be reported")
	}

	// ValidUntil=0 (DefaultDERControl) never expires on its own.
	def := &bus.ActiveControl{Source: "default", MRID: "M-default"}
	snap = snapshot{now: now, csipControl: def}
	if got := buildStatus(snap); got.CSIPControl == nil {
		t.Error("a ValidUntil=0 default control must always be reported")
	}
}
