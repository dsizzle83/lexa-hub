package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCmdTelemetry_Table(t *testing.T) {
	w1 := 250.0
	soc := 62.5
	resp := telemetryRecentResp{
		Minutes: 15,
		Devices: map[string][]telemetrySample{
			"inv1":  {{ArrivedAt: "2026-07-09T00:00:00Z", Kind: "measurement", W: &w1}},
			"batt1": {{ArrivedAt: "2026-07-09T00:00:01Z", Kind: "batt_metrics", SOC: &soc}},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/telemetry/recent" {
			t.Errorf("path = %s, want /telemetry/recent", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	code := cmdTelemetry(newTestClient(t, srv, ""), nil, &buf)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; output: %s", code, buf.String())
	}
	out := buf.String()
	for _, want := range []string{"inv1", "W=250", "batt1", "soc=62.5%", "last 15 minute(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q; got:\n%s", want, out)
		}
	}
}

func TestCmdTelemetry_MinutesParam(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(telemetryRecentResp{Minutes: 5, Devices: map[string][]telemetrySample{}})
	}))
	defer srv.Close()

	var buf bytes.Buffer
	code := cmdTelemetry(newTestClient(t, srv, ""), []string{"--minutes", "5"}, &buf)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if gotQuery != "minutes=5" {
		t.Errorf("query = %q, want minutes=5", gotQuery)
	}
}

func TestCmdTelemetry_NegativeMinutesIsValidationError(t *testing.T) {
	var buf bytes.Buffer
	code := cmdTelemetry(&client{}, []string{"--minutes", "-1"}, &buf)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

func TestCmdTelemetry_JSONPassthrough(t *testing.T) {
	const raw = `{"minutes":15,"devices":{}}` + "\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(raw))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	code := cmdTelemetry(newTestClient(t, srv, ""), []string{"-json"}, &buf)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if buf.String() != raw {
		t.Errorf("output = %q, want %q", buf.String(), raw)
	}
}

func TestCmdTelemetry_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	code := cmdTelemetry(newTestClient(t, srv, ""), nil, &buf)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

func TestCmdTelemetry_UsageError(t *testing.T) {
	var buf bytes.Buffer
	code := cmdTelemetry(&client{}, []string{"unexpected"}, &buf)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}
