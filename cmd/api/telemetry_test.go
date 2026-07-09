package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lexa-hub/internal/bus"
)

// TestTelemetryRing_EvictsOldestPastCapacity pins the 4096-entry bound: the
// (cap+1)th add must evict the oldest entry, not grow the ring.
func TestTelemetryRing_EvictsOldestPastCapacity(t *testing.T) {
	const capacity = 16 // small cap so the test is fast; behavior is size-independent
	r := newTelemetryRing(capacity)

	base := time.Now().Add(-time.Hour)
	for i := 0; i < capacity; i++ {
		r.add(telemetrySample{Kind: telemetryKindMeasurement, Device: "d", ArrivedAt: base.Add(time.Duration(i) * time.Second)})
	}
	if r.len() != capacity {
		t.Fatalf("len = %d after filling to capacity, want %d", r.len(), capacity)
	}

	// One more entry (the "4097th" in the task's terms) must evict the
	// oldest, keeping len at capacity, not capacity+1.
	newest := base.Add(time.Duration(capacity) * time.Second)
	r.add(telemetrySample{Kind: telemetryKindMeasurement, Device: "d", ArrivedAt: newest})
	if r.len() != capacity {
		t.Fatalf("len = %d after exceeding capacity, want still %d (oldest must be evicted)", r.len(), capacity)
	}

	all := r.since(time.Time{}) // everything
	if len(all) != capacity {
		t.Fatalf("since(zero time) returned %d entries, want %d", len(all), capacity)
	}
	oldestKept := all[0].ArrivedAt
	if !oldestKept.Equal(base.Add(1 * time.Second)) {
		t.Errorf("oldest kept entry = %v, want the SECOND original entry (base+1s) — the original oldest must have been evicted", oldestKept)
	}
	if !all[len(all)-1].ArrivedAt.Equal(newest) {
		t.Errorf("newest kept entry = %v, want %v", all[len(all)-1].ArrivedAt, newest)
	}
}

// TestTelemetryRing_SinceFiltersByArrivalTime pins the time-window filter.
func TestTelemetryRing_SinceFiltersByArrivalTime(t *testing.T) {
	r := newTelemetryRing(telemetryRingCap)
	now := time.Now()
	r.add(telemetrySample{Device: "d", ArrivedAt: now.Add(-20 * time.Minute)})
	r.add(telemetrySample{Device: "d", ArrivedAt: now.Add(-5 * time.Minute)})
	r.add(telemetrySample{Device: "d", ArrivedAt: now.Add(-1 * time.Minute)})

	got := r.since(now.Add(-10 * time.Minute))
	if len(got) != 2 {
		t.Fatalf("since(-10m) returned %d entries, want 2 (only the -5m and -1m samples)", len(got))
	}
}

// TestStateStore_TelemetryRingCapturesAllThreeKinds pins that
// onMeasurement/onBattMetrics/onEVSEState all feed the ring, each stamped
// with the right Kind and device key.
func TestStateStore_TelemetryRingCapturesAllThreeKinds(t *testing.T) {
	store := newStateStore(nil, time.Minute)

	w := 500.0
	store.onMeasurement(bus.MeasurementTopic("inv0"), bus.Measurement{Device: "inv0", W: &w})

	soc := 55.0
	store.onBattMetrics(bus.BattMetricsTopic("batt0"), bus.BattMetrics{Device: "batt0", SOC: &soc})

	cur := 16.0
	store.onEVSEState(bus.EVSEStateTopic("ev1"), bus.EVSEState{StationID: "ev1", ConnectorID: 1, CurrentA: &cur, Status: "Charging"})

	samples := store.telemetry.since(time.Time{})
	if len(samples) != 3 {
		t.Fatalf("ring has %d samples, want 3", len(samples))
	}

	kinds := map[telemetryKind]bool{}
	for _, s := range samples {
		kinds[s.Kind] = true
	}
	for _, k := range []telemetryKind{telemetryKindMeasurement, telemetryKindBattMetrics, telemetryKindEVSE} {
		if !kinds[k] {
			t.Errorf("ring missing a sample of kind %q", k)
		}
	}
}

// TestTelemetryRecentHandler_DefaultAndCapMinutes pins the ?minutes= query
// parameter's default (15) and cap (also 15 — DEVICE_ROADMAP.md §4.3: "cap
// 15").
func TestTelemetryRecentHandler_DefaultAndCapMinutes(t *testing.T) {
	ring := newTelemetryRing(telemetryRingCap)
	now := time.Now()
	ring.add(telemetrySample{Device: "inv0", Kind: telemetryKindMeasurement, ArrivedAt: now.Add(-10 * time.Minute)})
	ring.add(telemetrySample{Device: "inv0", Kind: telemetryKindMeasurement, ArrivedAt: now.Add(-20 * time.Minute)})

	h := telemetryRecentHandler(ring)

	t.Run("default is 15 minutes", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/telemetry/recent", nil)
		rec := httptest.NewRecorder()
		h(rec, req)
		var resp telemetryRecentResp
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Minutes != 15 {
			t.Errorf("Minutes = %d, want default 15", resp.Minutes)
		}
		if len(resp.Devices["inv0"]) != 1 {
			t.Fatalf("expected exactly 1 sample within the default 15m window, got %d", len(resp.Devices["inv0"]))
		}
	})

	t.Run("minutes=60 is capped to 15", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/telemetry/recent?minutes=60", nil)
		rec := httptest.NewRecorder()
		h(rec, req)
		var resp telemetryRecentResp
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Minutes != 15 {
			t.Errorf("Minutes = %d, want capped 15", resp.Minutes)
		}
		// Still only the -10m sample: the cap is on the SERVED window, not a
		// bypass back to the full ring.
		if len(resp.Devices["inv0"]) != 1 {
			t.Fatalf("expected exactly 1 sample even with minutes=60 (capped to 15), got %d", len(resp.Devices["inv0"]))
		}
	})

	t.Run("minutes=5 narrows the window", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/telemetry/recent?minutes=5", nil)
		rec := httptest.NewRecorder()
		h(rec, req)
		var resp telemetryRecentResp
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Devices) != 0 {
			t.Errorf("expected no devices within a 5m window (both samples are older), got %+v", resp.Devices)
		}
	})
}

// TestTelemetryRecentHandler_GroupsPerDeviceArrivalStamped pins the
// "grouped per device, arrival-ts stamped" shape requirement.
func TestTelemetryRecentHandler_GroupsPerDeviceArrivalStamped(t *testing.T) {
	ring := newTelemetryRing(telemetryRingCap)
	now := time.Now()
	w := 100.0
	ring.add(telemetrySample{Device: "inv0", Kind: telemetryKindMeasurement, ArrivedAt: now, W: &w})
	ring.add(telemetrySample{Device: "batt0", Kind: telemetryKindBattMetrics, ArrivedAt: now})

	h := telemetryRecentHandler(ring)
	req := httptest.NewRequest(http.MethodGet, "/telemetry/recent", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	var resp telemetryRecentResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Devices) != 2 {
		t.Fatalf("expected 2 device groups, got %d: %+v", len(resp.Devices), resp.Devices)
	}
	invSamples, ok := resp.Devices["inv0"]
	if !ok || len(invSamples) != 1 {
		t.Fatalf("inv0 group missing or wrong size: %+v", resp.Devices)
	}
	if invSamples[0].ArrivedAt == "" {
		t.Error("sample missing arrived_at stamp")
	}
	if invSamples[0].W == nil || *invSamples[0].W != 100.0 {
		t.Errorf("sample W = %v, want 100", invSamples[0].W)
	}
}
