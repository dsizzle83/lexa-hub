package openadr

import (
	"testing"
	"time"
)

func reportEvent(freq int) Event {
	return Event{
		ID: "evt-r", ProgramID: "prog-r",
		ReportDescriptors: []ReportDescriptor{{
			PayloadType: "USAGE", ReadingType: "DIRECT_READ", Units: "KWH", Frequency: freq,
		}},
		IntervalPeriod: &IntervalPeriod{Start: "2026-07-14T08:00:00Z", Duration: "PT15M"},
		Intervals: []Interval{
			{ID: 0, Payloads: []ValuesMap{{Type: "PRICE", Values: []any{0.1}}}},
			{ID: 1, Payloads: []ValuesMap{{Type: "PRICE", Values: []any{0.2}}}},
		},
	}
}

// TestReportSchedulerDueAndCadence: a stream is due immediately, not due
// again inside its period (frequency × interval duration), due again after.
func TestReportSchedulerDueAndCadence(t *testing.T) {
	rs := NewReportScheduler()
	inst := []EventInstance{{Event: reportEvent(2)}} // period = 2 × 15m = 30m
	defaultPeriod := time.Minute

	due := rs.Due(inst, now14, defaultPeriod)
	if len(due) != 1 {
		t.Fatalf("initial due = %d, want 1", len(due))
	}
	if due[0].Period != 30*time.Minute {
		t.Fatalf("resolved period = %v, want 30m (frequency 2 × PT15M)", due[0].Period)
	}
	rs.MarkPosted(due[0], now14)

	if d := rs.Due(inst, now14.Add(10*time.Minute), defaultPeriod); len(d) != 0 {
		t.Fatalf("due inside period = %d, want 0", len(d))
	}
}

// TestReportSchedulerDueAgainAfterPeriod uses a long event (four 15 m
// intervals) to pin the "due again once the period elapsed" half.
func TestReportSchedulerDueAgainAfterPeriod(t *testing.T) {
	ev := Event{
		ID: "evt-long", ProgramID: "p",
		ReportDescriptors: []ReportDescriptor{{PayloadType: "USAGE", Frequency: 1}},
		IntervalPeriod:    &IntervalPeriod{Start: "2026-07-14T08:00:00Z", Duration: "PT15M"},
		Intervals: []Interval{
			{ID: 0}, {ID: 1}, {ID: 2}, {ID: 3}, // 08:00–09:00
		},
	}
	rs := NewReportScheduler()
	inst := []EventInstance{{Event: ev}}
	due := rs.Due(inst, now14, time.Minute)
	if len(due) != 1 || due[0].Period != 15*time.Minute {
		t.Fatalf("due = %+v", due)
	}
	rs.MarkPosted(due[0], now14)
	if d := rs.Due(inst, now14.Add(5*time.Minute), time.Minute); len(d) != 0 {
		t.Fatalf("due 5m after post = %d, want 0", len(d))
	}
	if d := rs.Due(inst, now14.Add(16*time.Minute), time.Minute); len(d) != 1 {
		t.Fatalf("due 16m after post = %d, want 1", len(d))
	}
}

// TestReportSchedulerInactiveAndUnsupported: no reports for an event outside
// its active window, nor for descriptors this VEN cannot serve.
func TestReportSchedulerInactiveAndUnsupported(t *testing.T) {
	rs := NewReportScheduler()
	future := reportEvent(1)
	future.IntervalPeriod.Start = "2026-07-14T18:00:00Z"
	if d := rs.Due([]EventInstance{{Event: future}}, now14, time.Minute); len(d) != 0 {
		t.Fatalf("future event produced due reports: %+v", d)
	}
	storage := reportEvent(1)
	storage.ReportDescriptors[0].PayloadType = "STORAGE_CHARGE_LEVEL" // TODO seam — not yet served
	if d := rs.Due([]EventInstance{{Event: storage}}, now14, time.Minute); len(d) != 0 {
		t.Fatalf("unsupported descriptor produced due reports: %+v", d)
	}
}

// TestBuildUsageReportShape pins the built report: per-device resources,
// kWh from Wh deltas (DIRECT_READ) vs integrated average power (ESTIMATED
// when any device fell back), the window's interval period, and the echoed
// descriptor payloadType.
func TestBuildUsageReportShape(t *testing.T) {
	req := ReportRequest{Event: reportEvent(1), Descriptor: reportEvent(1).ReportDescriptors[0], Period: time.Minute}
	avgW := -1200.0 // net consumption 1.2 kW (bus sign: − = load)
	impD, expD := 500.0, 100.0
	snap := UsageSnapshot{
		StartTs: now14.Add(-time.Minute).Unix(),
		EndTs:   now14.Unix(),
		Devices: map[string]UsageWindow{
			"meter-0":    {WhImpDelta: &impD, WhExpDelta: &expD}, // (500−100)/1000 = 0.4 kWh
			"inverter-0": {AvgW: &avgW},                          // −(−1200)×60s → 0.02 kWh
		},
	}
	rep, ok := BuildUsageReport(req, snap, "lexa-hub")
	if !ok {
		t.Fatal("BuildUsageReport returned !ok with data present")
	}
	if rep.ProgramID != "prog-r" || rep.EventID != "evt-r" || rep.ClientName != "lexa-hub" {
		t.Fatalf("report identity: %+v", rep)
	}
	if len(rep.PayloadDescriptors) != 1 || rep.PayloadDescriptors[0].PayloadType != "USAGE" ||
		rep.PayloadDescriptors[0].Units != "KWH" {
		t.Fatalf("payload descriptor: %+v", rep.PayloadDescriptors)
	}
	if rep.PayloadDescriptors[0].ReadingType != "ESTIMATED" {
		t.Fatalf("readingType = %q, want ESTIMATED (one device integrated)", rep.PayloadDescriptors[0].ReadingType)
	}
	if len(rep.Resources) != 2 {
		t.Fatalf("resources = %d, want 2 (per device)", len(rep.Resources))
	}
	// Sorted device order: inverter-0, meter-0.
	if rep.Resources[0].ResourceName != "inverter-0" || rep.Resources[1].ResourceName != "meter-0" {
		t.Fatalf("resource order: %q, %q", rep.Resources[0].ResourceName, rep.Resources[1].ResourceName)
	}
	mkwh, _ := rep.Resources[1].Intervals[0].Payloads[0].FirstNumber()
	if diff := mkwh - 0.4; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("meter kWh = %v, want 0.4", mkwh)
	}
	ikwh, _ := rep.Resources[0].Intervals[0].Payloads[0].FirstNumber()
	if diff := ikwh - 0.02; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("inverter kWh = %v, want 0.02 (1200 W × 60 s)", ikwh)
	}
	if rep.Resources[0].IntervalPeriod.Duration != "PT60S" {
		t.Fatalf("interval duration = %q, want PT60S", rep.Resources[0].IntervalPeriod.Duration)
	}
}

// TestBuildUsageReportAggregate: aggregate descriptors sum every device into
// one venName-named resource.
func TestBuildUsageReportAggregate(t *testing.T) {
	ev := reportEvent(1)
	ev.ReportDescriptors[0].Aggregate = true
	req := ReportRequest{Event: ev, Descriptor: ev.ReportDescriptors[0], Period: time.Minute}
	imp1, imp2 := 300.0, 700.0
	snap := UsageSnapshot{
		StartTs: now14.Add(-time.Minute).Unix(),
		EndTs:   now14.Unix(),
		Devices: map[string]UsageWindow{
			"a": {WhImpDelta: &imp1},
			"b": {WhImpDelta: &imp2},
		},
	}
	rep, ok := BuildUsageReport(req, snap, "lexa-hub")
	if !ok {
		t.Fatal("!ok")
	}
	if len(rep.Resources) != 1 || rep.Resources[0].ResourceName != "lexa-hub" {
		t.Fatalf("aggregate resources: %+v", rep.Resources)
	}
	kwh, _ := rep.Resources[0].Intervals[0].Payloads[0].FirstNumber()
	if diff := kwh - 1.0; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("aggregate kWh = %v, want 1.0", kwh)
	}
}

// TestBuildUsageReportEmptySnapshot: no data ⇒ ok=false — never fabricate a
// zero report (G27).
func TestBuildUsageReportEmptySnapshot(t *testing.T) {
	req := ReportRequest{Event: reportEvent(1), Descriptor: reportEvent(1).ReportDescriptors[0], Period: time.Minute}
	snap := UsageSnapshot{StartTs: now14.Unix() - 60, EndTs: now14.Unix(), Devices: map[string]UsageWindow{}}
	if _, ok := BuildUsageReport(req, snap, "lexa-hub"); ok {
		t.Fatal("empty snapshot built a report")
	}
}
