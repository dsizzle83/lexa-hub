package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestEndToEnd_HealthyHub wires every real check (allChecks) against a
// fully healthy fake hub: a temp config dir with realistic api.json/
// modbus.json/northbound.json (no cloudlink.json — TASK-085 not landed on
// this fake unit), a fake systemctl/timedatectl Runner reporting
// everything nominal, and one httptest server answering both /healthz and
// /status. This is the integration confidence check tying every check_*.go
// gather+evaluate pair together the way -commit actually exercises them.
func TestEndToEnd_HealthyHub(t *testing.T) {
	dir := t.TempDir()

	sp := statusPayload{
		CSIPPrograms: 0, // northbound is idle (empty server), so this is unused
		Devices: map[string]struct {
			Connected bool `json:"connected"`
		}{
			"inverter-0": {Connected: true},
			"battery-0":  {Connected: true},
			"meter-0":    {Connected: true},
		},
	}
	sp.PlanHeartbeat.State = "ok"
	sp.PlanHeartbeat.AgeS = 2.0

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sp)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	host, port := splitTestAddr(t, srv.URL)

	writeAPIJSON(t, dir, host+":"+port)
	mustWriteFile(t, filepath.Join(dir, "modbus.json"),
		`{"devices":[{"name":"inverter-0"},{"name":"battery-0"},{"name":"meter-0"}]}`)
	mustWriteFile(t, filepath.Join(dir, "northbound.json"), `{"server":""}`)
	mustWriteFile(t, filepath.Join(dir, "commissioned"), "")
	// no cloudlink.json

	units := unitsToCheck(dir)
	r := newFakeRunner()
	r.set("systemctl", append([]string{"is-active"}, units...), strings.Repeat("active\n", len(units)), nil)
	r.set("timedatectl", []string{"show", "-p", "NTPSynchronized", "--value"}, "yes\n", nil)

	env := &Environment{
		ConfigDir:  dir,
		Runner:     r,
		HTTPClient: newProbeHTTPClient(),
		APIScheme:  "http",
		Now:        func() time.Time { return time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC) },
		Uptime:     func() (time.Duration, error) { return time.Hour, nil },
		RTCExists:  func() bool { return false },
		JournalDir: func(svc string) string { return filepath.Join(dir, "journal", svc) },
	}

	sum := Run(context.Background(), env, allChecks(), RunOptions{Budget: 10 * time.Second})

	if !sum.Pass {
		t.Fatalf("expected a fully healthy hub to PASS overall; results: %+v", sum.Checks)
	}
	want := map[string]Status{
		"systemd":        StatusPass,
		"api":            StatusPass,
		"plan_heartbeat": StatusPass,
		"northbound":     StatusPass,
		"modbus":         StatusPass,
		"clock":          StatusPass,
		"cloudlink":      StatusSkip,
	}
	got := map[string]Status{}
	for _, r := range sum.Checks {
		got[r.Name] = r.Status
	}
	for name, wantStatus := range want {
		if got[name] != wantStatus {
			t.Errorf("check %s = %s, want %s (checks: %+v)", name, got[name], wantStatus, sum.Checks)
		}
	}
}

// TestEndToEnd_BrokenServiceFailsOverall exercises the flip side: one
// systemd unit down must fail the whole run even though every other check
// is healthy — the exact "false PASS bricks trust in OTA" failure mode
// this tool exists to prevent.
func TestEndToEnd_BrokenServiceFailsOverall(t *testing.T) {
	dir := t.TempDir()

	sp := statusPayload{}
	sp.PlanHeartbeat.State = "never" // uncommissioned unit; irrelevant here since we don't mark commissioned
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sp)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	host, port := splitTestAddr(t, srv.URL)

	writeAPIJSON(t, dir, host+":"+port)
	mustWriteFile(t, filepath.Join(dir, "modbus.json"), `{"devices":[]}`)
	mustWriteFile(t, filepath.Join(dir, "northbound.json"), `{"server":""}`)
	// no commissioned marker: plan_heartbeat "never" is expected to PASS
	// anyway (uncommissioned idle-by-design) — the failure must come
	// purely from systemd.

	units := unitsToCheck(dir)
	states := make([]string, len(units))
	for i := range states {
		states[i] = "active"
	}
	states[2] = "failed" // lexa-modbus, arbitrary pick
	r := newFakeRunner()
	r.set("systemctl", append([]string{"is-active"}, units...), strings.Join(states, "\n")+"\n", nil)
	r.set("timedatectl", []string{"show", "-p", "NTPSynchronized", "--value"}, "yes\n", nil)

	env := &Environment{
		ConfigDir:  dir,
		Runner:     r,
		HTTPClient: newProbeHTTPClient(),
		APIScheme:  "http",
		Now:        func() time.Time { return time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC) },
		Uptime:     func() (time.Duration, error) { return time.Hour, nil },
		RTCExists:  func() bool { return true },
		JournalDir: func(svc string) string { return filepath.Join(dir, "journal", svc) },
	}

	sum := Run(context.Background(), env, allChecks(), RunOptions{Budget: 10 * time.Second})
	if sum.Pass {
		t.Fatalf("expected overall FAIL when one systemd unit is down, got Pass=true: %+v", sum.Checks)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
