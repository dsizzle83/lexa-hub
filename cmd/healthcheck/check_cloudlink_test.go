package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestEvalCloudlink(t *testing.T) {
	tests := []struct {
		name   string
		gauges map[string]float64
		want   Status
	}{
		{"connected=1 passes", map[string]float64{"lexa_cloudlink_connected": 1}, StatusPass},
		{"connected=0 but spool_bytes present passes (offline-safe)",
			map[string]float64{"lexa_cloudlink_connected": 0, "lexa_cloudlink_spool_bytes": 4096}, StatusPass},
		{"spool_bytes present alone passes", map[string]float64{"lexa_cloudlink_spool_bytes": 0}, StatusPass},
		{"neither gauge present fails", map[string]float64{"some_other_metric": 1}, StatusFail},
		{"connected=0 and no spool_bytes fails", map[string]float64{"lexa_cloudlink_connected": 0}, StatusFail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalCloudlink(tt.gauges)
			if got.Status != tt.want {
				t.Errorf("evalCloudlink(%v) = %v, want %v", tt.gauges, got.Status, tt.want)
			}
		})
	}
}

func TestParsePromGauges(t *testing.T) {
	body := []byte(`# TYPE lexa_cloudlink_connected gauge
lexa_cloudlink_connected 1
# TYPE lexa_cloudlink_spool_bytes gauge
lexa_cloudlink_spool_bytes 12345
# TYPE lexa_mqtt_reconnects_total counter
lexa_mqtt_reconnects_total 0
`)
	got := parsePromGauges(body)
	if got["lexa_cloudlink_connected"] != 1 {
		t.Errorf("lexa_cloudlink_connected = %v, want 1", got["lexa_cloudlink_connected"])
	}
	if got["lexa_cloudlink_spool_bytes"] != 12345 {
		t.Errorf("lexa_cloudlink_spool_bytes = %v, want 12345", got["lexa_cloudlink_spool_bytes"])
	}
}

func TestCheckCloudlink_AbsentConfig_Skip(t *testing.T) {
	dir := t.TempDir() // no cloudlink.json
	env := &Environment{ConfigDir: dir}
	res := checkCloudlink(context.Background(), env)
	if res.Status != StatusSkip {
		t.Fatalf("checkCloudlink with no cloudlink.json = %+v, want SKIP", res)
	}
}

func TestCheckCloudlink_Disabled_Skip(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cloudlink.json"), []byte(`{"enabled":false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	env := &Environment{ConfigDir: dir}
	res := checkCloudlink(context.Background(), env)
	if res.Status != StatusSkip {
		t.Fatalf("checkCloudlink disabled = %+v, want SKIP", res)
	}
}

// This test cannot point checkCloudlink at an httptest server, since the
// metrics port is a fixed 127.0.0.1:9106 per DEVICE_ROADMAP.md §1.5 (no
// config knob) — instead it exercises the enabled-but-unreachable path
// directly, trusting nothing listens on 9106 in the test sandbox.
func TestCheckCloudlink_EnabledButUnreachable_Fail(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cloudlink.json"), []byte(`{"enabled":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	env := &Environment{ConfigDir: dir, HTTPClient: newProbeHTTPClient()}
	res := checkCloudlink(context.Background(), env)
	if res.Status != StatusFail {
		t.Fatalf("checkCloudlink enabled+unreachable = %+v, want FAIL", res)
	}
}

func TestCheckCloudlink_MalformedConfig_Fail(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cloudlink.json"), []byte(`{not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	env := &Environment{ConfigDir: dir}
	res := checkCloudlink(context.Background(), env)
	if res.Status != StatusFail {
		t.Fatalf("checkCloudlink with malformed config = %+v, want FAIL", res)
	}
}

// sanity check that httptest + parsePromGauges compose correctly, exercised
// indirectly via probeGET (already covered in httpprobe_test.go) — this
// just pins the metrics-format assumption against a real HTTP round trip.
func TestFetchPromMetrics_RoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("lexa_cloudlink_connected 1\n"))
	}))
	defer srv.Close()
	host, port := splitTestAddr(t, srv.URL)

	res, err := probeGET(context.Background(), newProbeHTTPClient(), "http", host, port, "/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	gauges := parsePromGauges(res.Body)
	if gauges["lexa_cloudlink_connected"] != 1 {
		t.Errorf("gauges = %v, want lexa_cloudlink_connected=1", gauges)
	}
}
