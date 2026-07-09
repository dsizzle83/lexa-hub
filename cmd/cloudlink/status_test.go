package main

import (
	"encoding/json"
	"testing"
	"time"

	"lexa-hub/internal/bus"
)

type fakeSession struct{ connected bool }

func (f fakeSession) Connected() bool { return f.connected }

type fakeSpool struct {
	bytes    int64
	oldestTs int64
}

func (f fakeSpool) Bytes() int64    { return f.bytes }
func (f fakeSpool) OldestTs() int64 { return f.oldestTs }

func TestBuildStatus_Shape(t *testing.T) {
	cfg := &Config{Enabled: true, Endpoint: "ssl://example:8883"}
	now := time.Unix(1751990000, 0)
	st := buildStatus(cfg, fakeSession{connected: true}, fakeSpool{bytes: 42, oldestTs: 100}, now)

	if st.V != bus.CloudlinkStatusV {
		t.Errorf("V = %d, want %d", st.V, bus.CloudlinkStatusV)
	}
	if !st.Connected {
		t.Error("Connected = false, want true")
	}
	if st.Endpoint != cfg.Endpoint {
		t.Errorf("Endpoint = %q, want %q", st.Endpoint, cfg.Endpoint)
	}
	if st.SpoolBytes != 42 || st.SpoolOldestTs != 100 {
		t.Errorf("spool fields = %d/%d, want 42/100", st.SpoolBytes, st.SpoolOldestTs)
	}
	if st.Ts != now.Unix() {
		t.Errorf("Ts = %d, want %d", st.Ts, now.Unix())
	}

	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v, ok := m["v"]; !ok || v != float64(1) {
		t.Errorf(`"v" = %v, want 1`, m["v"])
	}
	if m["connected"] != true {
		t.Errorf(`"connected" = %v, want true`, m["connected"])
	}
	if m["endpoint"] != "ssl://example:8883" {
		t.Errorf(`"endpoint" = %v, want ssl://example:8883`, m["endpoint"])
	}
}

func TestBuildStatus_EndpointOmittedWhenDisabled(t *testing.T) {
	cfg := &Config{Enabled: false, Endpoint: "ssl://example:8883"}
	st := buildStatus(cfg, fakeSession{}, fakeSpool{}, time.Now())
	if st.Endpoint != "" {
		t.Errorf("Endpoint = %q, want empty when Enabled=false", st.Endpoint)
	}

	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["endpoint"]; ok {
		t.Errorf(`wire shape includes "endpoint" key when empty; want omitted (omitempty)`)
	}
}

func TestBuildStatus_DisconnectedAndEmptySpool(t *testing.T) {
	cfg := &Config{Enabled: true, Endpoint: "ssl://example:8883"}
	st := buildStatus(cfg, fakeSession{connected: false}, fakeSpool{}, time.Now())
	if st.Connected {
		t.Error("Connected = true, want false")
	}
	if st.SpoolBytes != 0 || st.SpoolOldestTs != 0 {
		t.Errorf("spool fields = %d/%d, want 0/0", st.SpoolBytes, st.SpoolOldestTs)
	}
}

func TestTopicCloudlinkStatusConstant(t *testing.T) {
	if bus.TopicCloudlinkStatus != "lexa/cloudlink/status" {
		t.Errorf("TopicCloudlinkStatus = %q, want %q", bus.TopicCloudlinkStatus, "lexa/cloudlink/status")
	}
}

func TestStubCloudSession_AlwaysDisconnected(t *testing.T) {
	var s cloudSession = stubCloudSession{}
	if s.Connected() {
		t.Error("stubCloudSession.Connected() = true, want false")
	}
}

func TestStubSpoolStats_AlwaysZero(t *testing.T) {
	var sp spoolStats = stubSpoolStats{}
	if sp.Bytes() != 0 || sp.OldestTs() != 0 {
		t.Errorf("stubSpoolStats = %d/%d, want 0/0", sp.Bytes(), sp.OldestTs())
	}
}
