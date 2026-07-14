package main

import (
	"testing"
	"time"

	"lexa-hub/internal/bus"
)

func fp(v float64) *float64 { return &v }

// TestCollectorSnapshotAveragesAndDeltas: W samples average; Wh lifetime
// accumulators produce first→last deltas; the snapshot resets the window.
func TestCollectorSnapshotAveragesAndDeltas(t *testing.T) {
	start := time.Unix(1752480000, 0)
	c := newUsageCollector(start)

	c.OnMeasurement(bus.Measurement{Device: "meter-0", W: fp(-1000), WhImpTotal: fp(5000), WhExpTotal: fp(200)})
	c.OnMeasurement(bus.Measurement{Device: "meter-0", W: fp(-2000), WhImpTotal: fp(5400), WhExpTotal: fp(250)})
	c.OnMeasurement(bus.Measurement{Device: "inverter-0", W: fp(3000)})

	now := start.Add(time.Minute)
	snap := c.Snapshot(now)
	if snap.StartTs != start.Unix() || snap.EndTs != now.Unix() {
		t.Fatalf("window = [%d,%d]", snap.StartTs, snap.EndTs)
	}
	m := snap.Devices["meter-0"]
	if m.AvgW == nil || *m.AvgW != -1500 {
		t.Fatalf("meter AvgW = %v, want -1500", m.AvgW)
	}
	if m.WhImpDelta == nil || *m.WhImpDelta != 400 {
		t.Fatalf("meter WhImpDelta = %v, want 400", m.WhImpDelta)
	}
	if m.WhExpDelta == nil || *m.WhExpDelta != 50 {
		t.Fatalf("meter WhExpDelta = %v, want 50", m.WhExpDelta)
	}
	inv := snap.Devices["inverter-0"]
	if inv.AvgW == nil || *inv.AvgW != 3000 {
		t.Fatalf("inverter AvgW = %v, want 3000", inv.AvgW)
	}
	if inv.WhImpDelta != nil {
		t.Fatal("inverter has no Wh counters — delta must be nil (never fabricated)")
	}

	// Window reset: an immediate second snapshot is empty.
	snap2 := c.Snapshot(now.Add(time.Minute))
	if len(snap2.Devices) != 0 {
		t.Fatalf("window not reset: %+v", snap2.Devices)
	}
	if snap2.StartTs != now.Unix() {
		t.Fatalf("second window start = %d, want %d", snap2.StartTs, now.Unix())
	}
}

// TestCollectorSilentDeviceOmitted: a device that never reported anything in
// the window produces no entry (G27).
func TestCollectorSilentDeviceOmitted(t *testing.T) {
	c := newUsageCollector(time.Unix(0, 0))
	c.OnMeasurement(bus.Measurement{Device: "quiet-0"}) // no W, no Wh
	snap := c.Snapshot(time.Unix(60, 0))
	if _, ok := snap.Devices["quiet-0"]; ok {
		t.Fatal("dataless device appeared in snapshot")
	}
}

// TestCollectorBattMetricsSeam: battery metrics are tracked (the STORAGE_*
// report seam) without affecting the usage window.
func TestCollectorBattMetricsSeam(t *testing.T) {
	c := newUsageCollector(time.Unix(0, 0))
	c.OnBattMetrics(bus.BattMetrics{Device: "battery-0", SOC: fp(63.4)})
	c.mu.Lock()
	b, ok := c.batt["battery-0"]
	c.mu.Unlock()
	if !ok || b.SOC == nil || *b.SOC != 63.4 {
		t.Fatalf("battery metrics not tracked: %+v", b)
	}
	if n := len(c.Snapshot(time.Unix(60, 0)).Devices); n != 0 {
		t.Fatalf("battery metrics leaked into the usage window: %d devices", n)
	}
}
