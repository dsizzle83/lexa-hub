package main

import (
	"encoding/json"
	"testing"
	"time"

	"lexa-hub/internal/metrics"
	"lexa-hub/internal/spool"
)

// TestCollectorSpecs_StreamPriorityMapping pins the §2.4 stream/priority
// contract EXACTLY. If a topic is added/moved/removed, this fails loud.
func TestCollectorSpecs_StreamPriorityMapping(t *testing.T) {
	want := map[string]struct {
		stream   string
		priority int
	}{
		"lexa/measurements/+":        {streamTelemetry, prioTelemetry},
		"lexa/battery/+/metrics":     {streamTelemetry, prioTelemetry},
		"lexa/evse/+/state":          {streamTelemetry, prioTelemetry},
		"lexa/hub/plan":              {streamPlan, prioPlanHealth},
		"lexa/northbound/certstatus": {streamHealth, prioPlanHealth},
		"lexa/hub/mode":              {streamHealth, prioPlanHealth},
		"lexa/csip/compliance/alert": {streamEvents, prioEvents},
		"lexa/reconcile/+/+/report":  {streamEvents, prioEvents},
		"lexa/intent/result":         {streamEvents, prioEvents},
		"lexa/scan/result":           {streamEvents, prioEvents},
	}

	got := collectorSpecs()
	if len(got) != len(want) {
		t.Fatalf("collectorSpecs len = %d, want %d", len(got), len(want))
	}
	seen := map[string]bool{}
	for _, spec := range got {
		w, ok := want[spec.topic]
		if !ok {
			t.Errorf("unexpected collector topic %q", spec.topic)
			continue
		}
		if seen[spec.topic] {
			t.Errorf("duplicate collector topic %q", spec.topic)
		}
		seen[spec.topic] = true
		if spec.stream != w.stream || spec.priority != w.priority {
			t.Errorf("%q → {%s,%d}, want {%s,%d}", spec.topic, spec.stream, spec.priority, w.stream, w.priority)
		}
	}
	// lexa/cloudlink/status must NOT be collected (self-authored — no self-loop).
	for _, spec := range got {
		if spec.topic == "lexa/cloudlink/status" {
			t.Error("cloudlink must not subscribe its own status topic")
		}
	}
}

// TestCollector_RoundTrip drives handlers directly (no real bus) and asserts
// each message lands in the spool as a self-describing recordFrame with the
// ORIGINAL envelope bytes verbatim and the mapped stream/priority.
func TestCollector_RoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		topic    string
		stream   string
		priority int
		payload  string
	}{
		{"telemetry", "lexa/measurements/inv1", streamTelemetry, prioTelemetry, `{"v":1,"voltage_v":240.5,"w":1200,"future":true}`},
		{"plan", "lexa/hub/plan", streamPlan, prioPlanHealth, `{"v":1,"ts":123}`},
		{"health", "lexa/hub/mode", streamHealth, prioPlanHealth, `{"v":1,"mode":"optimizer"}`},
		{"event", "lexa/csip/compliance/alert", streamEvents, prioEvents, `{"v":1,"active":true}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			sp, err := spool.Open(dir, 1<<20, nil)
			if err != nil {
				t.Fatalf("spool.Open: %v", err)
			}
			defer sp.Close()

			notify := make(chan struct{}, 1)
			u := newUplink(nil, sp, notify, newCloudlinkMetrics(metrics.New()))
			u.now = func() time.Time { return time.Unix(1751990000, 0) }

			u.handler(tc.stream, tc.priority)(tc.topic, json.RawMessage(tc.payload))

			recs, err := sp.Peek(10, 0)
			if err != nil {
				t.Fatalf("Peek: %v", err)
			}
			if len(recs) != 1 {
				t.Fatalf("Peek returned %d records, want 1", len(recs))
			}
			r := recs[0]
			if r.Stream != tc.stream {
				t.Errorf("record stream = %q, want %q", r.Stream, tc.stream)
			}
			if r.Priority != tc.priority {
				t.Errorf("record priority = %d, want %d", r.Priority, tc.priority)
			}

			var rf recordFrame
			if err := json.Unmarshal(r.Payload, &rf); err != nil {
				t.Fatalf("decode recordFrame: %v", err)
			}
			if rf.Topic != tc.topic {
				t.Errorf("frame topic = %q, want %q", rf.Topic, tc.topic)
			}
			if rf.Ts != 1751990000 {
				t.Errorf("frame ts = %d, want 1751990000 (arrival, not message ts)", rf.Ts)
			}
			if string(rf.Payload) != tc.payload {
				t.Errorf("frame payload = %q, want %q (verbatim, incl. unknown fields)", string(rf.Payload), tc.payload)
			}

			// A P0 event must poke the notify channel; others must not.
			select {
			case <-notify:
				if tc.priority != prioEvents {
					t.Errorf("non-event stream %q poked the batcher notify", tc.stream)
				}
			default:
				if tc.priority == prioEvents {
					t.Error("event append did NOT poke the batcher notify")
				}
			}
		})
	}
}

// TestCollector_AppendFaultNeverPanics ensures a spool that rejects an
// over-budget record is handled (rate-limited WARN) and never crashes the
// handler (AD-011).
func TestCollector_AppendFaultNeverPanics(t *testing.T) {
	dir := t.TempDir()
	// Tiny budget so any real record is over-budget and rejected by Append.
	sp, err := spool.Open(dir, 64, nil)
	if err != nil {
		t.Fatalf("spool.Open: %v", err)
	}
	defer sp.Close()

	u := newUplink(nil, sp, make(chan struct{}, 1), newCloudlinkMetrics(metrics.New()))
	// Must not panic.
	u.handler(streamTelemetry, prioTelemetry)("lexa/measurements/x", json.RawMessage(`{"v":1,"padding":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`))
	if b := sp.Bytes(); b != 0 {
		t.Errorf("spool bytes = %d, want 0 (over-budget record rejected)", b)
	}
}
