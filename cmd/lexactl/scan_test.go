package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCmdScanRun_NoWatch(t *testing.T) {
	var gotCidr string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		var body struct {
			TCPCidr string `json:"tcp_cidr"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotCidr = body.TCPCidr
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "scan-1"})
	}))
	defer srv.Close()

	var buf bytes.Buffer
	code := cmdScanRun(newTestClient(t, srv, ""), []string{"--cidr", "192.168.1.0/24"}, &buf)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; output: %s", code, buf.String())
	}
	if gotCidr != "192.168.1.0/24" {
		t.Errorf("tcp_cidr = %q, want 192.168.1.0/24", gotCidr)
	}
	if !strings.Contains(buf.String(), "scan-1") {
		t.Errorf("expected id in output, got:\n%s", buf.String())
	}
}

func TestCmdScanRun_Refused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "reconciler active", http.StatusForbidden)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	code := cmdScanRun(newTestClient(t, srv, ""), nil, &buf)
	if code != 1 {
		t.Errorf("exit code = %d, want 1; output: %s", code, buf.String())
	}
}

func TestCmdScanShow(t *testing.T) {
	resp := scanGetResp{
		Status: []scanStatusLine{{ID: "s1", Phase: "done", Probed: 10, Found: 2, Ts: "2026-07-09T00:00:00Z"}},
		Result: &scanResult{
			ID: "s1", Ts: "2026-07-09T00:00:00Z",
			Devices: []scanHit{{URL: "tcp://192.168.1.40:502", UnitID: 1, Class: "inverter", Manufacturer: "Acme"}},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	code := cmdScanShow(newTestClient(t, srv, ""), nil, &buf)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; output: %s", code, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "phase=done") {
		t.Errorf("expected status line, got:\n%s", out)
	}
	if !strings.Contains(out, "tcp://192.168.1.40:502") || !strings.Contains(out, "Acme") {
		t.Errorf("expected result line, got:\n%s", out)
	}
}

func TestCmdScanShow_NoResultYet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(scanGetResp{})
	}))
	defer srv.Close()

	var buf bytes.Buffer
	code := cmdScanShow(newTestClient(t, srv, ""), nil, &buf)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(buf.String(), "no scan result yet") {
		t.Errorf("expected 'no scan result yet', got:\n%s", buf.String())
	}
}

// scriptedScanServer serves a pre-scripted sequence of GET /scan responses,
// advancing one step per call (staying on the last entry once exhausted) —
// the "scan --watch loop against a scripted status sequence" fixture the
// unit brief calls for.
func scriptedScanServer(t *testing.T, sequence []scanGetResp) *httptest.Server {
	t.Helper()
	var call int64
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&call, 1) - 1
		idx := int(n)
		if idx >= len(sequence) {
			idx = len(sequence) - 1
		}
		_ = json.NewEncoder(w).Encode(sequence[idx])
	}))
}

func TestWatchScan_ReachesDone(t *testing.T) {
	sequence := []scanGetResp{
		{Status: []scanStatusLine{{Phase: "tcp", Probed: 5, Found: 0}}},
		{Status: []scanStatusLine{{Phase: "identify", Probed: 20, Found: 1}}},
		{
			Status: []scanStatusLine{{Phase: "done", Probed: 20, Found: 1}},
			Result: &scanResult{ID: "s1", Devices: []scanHit{{URL: "tcp://x", Class: "meter"}}},
		},
	}
	srv := scriptedScanServer(t, sequence)
	defer srv.Close()

	var buf bytes.Buffer
	code := watchScan(newTestClient(t, srv, ""), &buf, time.Millisecond, time.Second)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; output: %s", code, buf.String())
	}
	out := buf.String()
	for _, want := range []string{"phase=tcp", "phase=identify", "phase=done", "result:"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q; got:\n%s", want, out)
		}
	}
}

func TestWatchScan_Refused(t *testing.T) {
	sequence := []scanGetResp{
		{Status: []scanStatusLine{{Phase: "refused", Detail: "reconciler active"}}},
	}
	srv := scriptedScanServer(t, sequence)
	defer srv.Close()

	var buf bytes.Buffer
	code := watchScan(newTestClient(t, srv, ""), &buf, time.Millisecond, time.Second)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; output: %s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "refused") {
		t.Errorf("expected 'refused' in output, got:\n%s", buf.String())
	}
}

func TestWatchScan_TimesOutWithoutReachingTerminalPhase(t *testing.T) {
	sequence := []scanGetResp{
		{Status: []scanStatusLine{{Phase: "tcp", Probed: 1, Found: 0}}},
	}
	srv := scriptedScanServer(t, sequence)
	defer srv.Close()

	var buf bytes.Buffer
	code := watchScan(newTestClient(t, srv, ""), &buf, time.Millisecond, 10*time.Millisecond)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (timeout); output: %s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "timed out") {
		t.Errorf("expected timeout message, got:\n%s", buf.String())
	}
}

func TestCmdScanRun_WatchEndToEnd(t *testing.T) {
	var postCalls, getCalls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			atomic.AddInt64(&postCalls, 1)
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "scan-1"})
			return
		}
		n := atomic.AddInt64(&getCalls, 1)
		phase := "tcp"
		if n >= 2 {
			phase = "done"
		}
		_ = json.NewEncoder(w).Encode(scanGetResp{
			Status: []scanStatusLine{{Phase: phase, Probed: int(n) * 10}},
			Result: mkScanResultIfDone(phase),
		})
	}))
	defer srv.Close()

	origInterval, origCap := scanPollInterval, scanWatchCap
	scanPollInterval, scanWatchCap = time.Millisecond, time.Second
	defer func() { scanPollInterval, scanWatchCap = origInterval, origCap }()

	var buf bytes.Buffer
	code := cmdScanRun(newTestClient(t, srv, ""), []string{"--watch"}, &buf)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; output: %s", code, buf.String())
	}
	if atomic.LoadInt64(&postCalls) != 1 {
		t.Errorf("POST /scan calls = %d, want 1", postCalls)
	}
	if !strings.Contains(buf.String(), "phase=done") {
		t.Errorf("expected phase=done in watch output, got:\n%s", buf.String())
	}
}

func mkScanResultIfDone(phase string) *scanResult {
	if phase != "done" {
		return nil
	}
	return &scanResult{ID: "scan-1", Devices: []scanHit{{URL: "tcp://done", Class: "battery"}}}
}

func TestCmdScanRun_UsageError(t *testing.T) {
	var buf bytes.Buffer
	code := cmdScanRun(&client{}, []string{"unexpected"}, &buf)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestDispatchScan_UsageErrors(t *testing.T) {
	tests := [][]string{nil, {"frobnicate"}}
	for _, args := range tests {
		var buf bytes.Buffer
		code := dispatchScan(&client{}, args, &buf)
		if code != 2 {
			t.Errorf("dispatchScan(%v): exit code = %d, want 2", args, code)
		}
	}
}
