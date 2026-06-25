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
