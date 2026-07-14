package main

import (
	"sync"
	"time"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/openadr"
)

// usageCollector accumulates the bus measurement stream into per-device
// windows for the VEN's USAGE reports (internal/openadr/report.go): a
// running W average plus the lifetime Wh accumulator deltas where the
// device publishes them. It also tracks the latest battery metrics per
// device — the documented STORAGE_* report seam (see report.go's
// supportedReportPayload TODO): the data is collected and ready; only the
// report-building half is deferred.
//
// Concurrency: OnMeasurement/OnBattMetrics run on paho callback goroutines;
// Snapshot runs on the poll goroutine — everything under one mutex.
type usageCollector struct {
	mu    sync.Mutex
	start time.Time
	dev   map[string]*devWindow
	batt  map[string]bus.BattMetrics // latest per device — STORAGE_* seam
}

type devWindow struct {
	wSum float64
	wN   int
	// first/last lifetime accumulator readings inside the window; the delta
	// is last−first. cmd/modbus already withholds non-monotonic samples
	// (whMonotonicGate), so a negative delta here is not defended twice.
	whImpFirst, whImpLast *float64
	whExpFirst, whExpLast *float64
}

func newUsageCollector(now time.Time) *usageCollector {
	return &usageCollector{
		start: now,
		dev:   make(map[string]*devWindow),
		batt:  make(map[string]bus.BattMetrics),
	}
}

func (c *usageCollector) window(device string) *devWindow {
	w, ok := c.dev[device]
	if !ok {
		w = &devWindow{}
		c.dev[device] = w
	}
	return w
}

// OnMeasurement folds one bus.Measurement into the current window.
func (c *usageCollector) OnMeasurement(m bus.Measurement) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.window(m.Device)
	if m.W != nil {
		w.wSum += *m.W
		w.wN++
	}
	if m.WhImpTotal != nil {
		v := *m.WhImpTotal
		if w.whImpFirst == nil {
			w.whImpFirst = &v
		}
		w.whImpLast = &v
	}
	if m.WhExpTotal != nil {
		v := *m.WhExpTotal
		if w.whExpFirst == nil {
			w.whExpFirst = &v
		}
		w.whExpLast = &v
	}
}

// OnBattMetrics records the latest battery metrics (STORAGE_* seam).
func (c *usageCollector) OnBattMetrics(b bus.BattMetrics) {
	c.mu.Lock()
	c.batt[b.Device] = b
	c.mu.Unlock()
}

// Snapshot returns the accumulated window as an openadr.UsageSnapshot and
// RESETS the window (the next window starts at now). Devices with no data at
// all are omitted (G27 — never fabricate a zero for a silent device).
func (c *usageCollector) Snapshot(now time.Time) openadr.UsageSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	snap := openadr.UsageSnapshot{
		StartTs: c.start.Unix(),
		EndTs:   now.Unix(),
		Devices: make(map[string]openadr.UsageWindow, len(c.dev)),
	}
	for name, w := range c.dev {
		var uw openadr.UsageWindow
		if w.wN > 0 {
			avg := w.wSum / float64(w.wN)
			uw.AvgW = &avg
		}
		if w.whImpFirst != nil && w.whImpLast != nil {
			d := *w.whImpLast - *w.whImpFirst
			uw.WhImpDelta = &d
		}
		if w.whExpFirst != nil && w.whExpLast != nil {
			d := *w.whExpLast - *w.whExpFirst
			uw.WhExpDelta = &d
		}
		if uw.AvgW != nil || uw.WhImpDelta != nil || uw.WhExpDelta != nil {
			snap.Devices[name] = uw
		}
	}
	c.start = now
	c.dev = make(map[string]*devWindow)
	return snap
}
