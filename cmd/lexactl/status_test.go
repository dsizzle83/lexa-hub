package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCmdStatus_Summary(t *testing.T) {
	body := `{
		"mode": "gateway",
		"plan_heartbeat": {"state": "ok", "age_s": 3.5},
		"cloud_link": {"connected": true, "endpoint": "ssl://cloud.example:8883"},
		"devices": {"inv1": {"connected": true}, "batt1": {"connected": false}},
		"api_cert_fp": "deadbeef"
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			t.Errorf("path = %s, want /status", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	code := cmdStatus(newTestClient(t, srv, ""), nil, &buf)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; output: %s", code, buf.String())
	}
	out := buf.String()
	for _, want := range []string{
		"mode: gateway",
		"plan heartbeat: ok (age 3.5s)",
		"cloud link: connected (ssl://cloud.example:8883)",
		"devices: 2 (1 connected)",
		"cert fp: deadbeef",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q; got:\n%s", want, out)
		}
	}
}

func TestCmdStatus_MissingFieldsRenderAsUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	code := cmdStatus(newTestClient(t, srv, ""), nil, &buf)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	out := buf.String()
	if !strings.Contains(out, "mode: unknown") {
		t.Errorf("expected mode: unknown, got:\n%s", out)
	}
	if !strings.Contains(out, "cloud link: not reporting") {
		t.Errorf("expected cloud link: not reporting, got:\n%s", out)
	}
	if !strings.Contains(out, "devices: 0 (0 connected)") {
		t.Errorf("expected devices: 0 (0 connected), got:\n%s", out)
	}
	if !strings.Contains(out, "cert fp: (tls disabled)") {
		t.Errorf("expected cert fp placeholder, got:\n%s", out)
	}
}

func TestCmdStatus_JSONPassthrough(t *testing.T) {
	const raw = `{"mode":"optimizer","plan_heartbeat":{"state":"ok","age_s":1},"devices":{}}` + "\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(raw))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	code := cmdStatus(newTestClient(t, srv, ""), []string{"-json"}, &buf)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if buf.String() != raw {
		t.Errorf("output = %q, want raw passthrough %q", buf.String(), raw)
	}
}

func TestCmdStatus_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	code := cmdStatus(newTestClient(t, srv, ""), nil, &buf)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; output: %s", code, buf.String())
	}
}

func TestCmdStatus_UsageError(t *testing.T) {
	var buf bytes.Buffer
	code := cmdStatus(&client{}, []string{"unexpected-arg"}, &buf)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}
