package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, data, 0640); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return m
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected %s to not exist, stat err = %v", path, err)
	}
}

// TestProcessFile_V0GainsSchemaVersionPreservingUnknownKeys is the core
// "v0 file gains schema_version preserving unknown keys" case: every
// existing key (known today or not — "some_future_key" stands in for a key
// a newer release added that this old fixture predates) must survive the
// migration with its value unchanged.
func TestProcessFile_V0GainsSchemaVersionPreservingUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.json")
	writeJSON(t, path, map[string]any{
		"mqtt_broker":       "tcp://localhost:1883",
		"engine_interval_s": 15,
		"devices": []any{
			map[string]any{"name": "inverter-0", "role": "inverter"},
		},
		"some_future_key": map[string]any{"nested": true, "n": 42.5},
	})

	result, err := processFile(dir, "hub.json", false)
	if err != nil {
		t.Fatalf("processFile: %v", err)
	}
	if result != resultMigrated {
		t.Fatalf("result = %v, want resultMigrated", result)
	}

	got := readJSON(t, path)
	if got["schema_version"] != float64(1) {
		t.Errorf("schema_version = %v, want 1", got["schema_version"])
	}
	if got["mqtt_broker"] != "tcp://localhost:1883" {
		t.Errorf("mqtt_broker lost or changed: %v", got["mqtt_broker"])
	}
	if got["engine_interval_s"] != float64(15) {
		t.Errorf("engine_interval_s lost or changed: %v", got["engine_interval_s"])
	}
	future, ok := got["some_future_key"].(map[string]any)
	if !ok {
		t.Fatalf("some_future_key lost or wrong type: %#v", got["some_future_key"])
	}
	if future["nested"] != true || future["n"] != 42.5 {
		t.Errorf("some_future_key contents changed: %#v", future)
	}
	devices, ok := got["devices"].([]any)
	if !ok || len(devices) != 1 {
		t.Fatalf("devices lost or changed: %#v", got["devices"])
	}

	// The backup must hold the ORIGINAL pre-migration (v0) content — no
	// schema_version, original mqtt_broker.
	backup := readJSON(t, path+".pre-v0")
	if _, has := backup["schema_version"]; has {
		t.Errorf("backup should be the pre-migration file, but already has schema_version: %v", backup["schema_version"])
	}
	if backup["mqtt_broker"] != "tcp://localhost:1883" {
		t.Errorf("backup missing/changed original content: %#v", backup)
	}
}

// TestProcessFile_PreservesWS1AndWS8Keys pins that keys added by parallel,
// in-flight work (WS-1's "bench" flags, WS-8's "tariff_zone") survive the
// generic map round-trip untouched — the coordination note this task's
// principal asked for explicitly.
func TestProcessFile_PreservesWS1AndWS8Keys(t *testing.T) {
	dir := t.TempDir()

	ocppPath := filepath.Join(dir, "ocpp.json")
	writeJSON(t, ocppPath, map[string]any{
		"bench":           true,
		"cert_path":       "",
		"basic_auth_user": "",
	})
	if _, err := processFile(dir, "ocpp.json", false); err != nil {
		t.Fatalf("processFile ocpp.json: %v", err)
	}
	gotOCPP := readJSON(t, ocppPath)
	if gotOCPP["bench"] != true {
		t.Errorf("bench key dropped or changed: %#v", gotOCPP["bench"])
	}
	if gotOCPP["schema_version"] != float64(1) {
		t.Errorf("ocpp.json schema_version = %v, want 1", gotOCPP["schema_version"])
	}

	hubPath := filepath.Join(dir, "hub.json")
	writeJSON(t, hubPath, map[string]any{"tariff_zone": "America/Los_Angeles"})
	if _, err := processFile(dir, "hub.json", false); err != nil {
		t.Fatalf("processFile hub.json: %v", err)
	}
	gotHub := readJSON(t, hubPath)
	if gotHub["tariff_zone"] != "America/Los_Angeles" {
		t.Errorf("tariff_zone key dropped or changed: %#v", gotHub["tariff_zone"])
	}
}

// TestProcessFile_Idempotent: re-running on an already-current file is a
// silent no-op — same schema_version, same content, no second backup.
func TestProcessFile_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ocpp.json")
	writeJSON(t, path, map[string]any{"bench": false, "cert_path": ""})

	if _, err := processFile(dir, "ocpp.json", false); err != nil {
		t.Fatalf("first run: %v", err)
	}
	before := readJSON(t, path)

	result, err := processFile(dir, "ocpp.json", false)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if result != resultUnchanged {
		t.Fatalf("second run result = %v, want resultUnchanged", result)
	}
	after := readJSON(t, path)
	if before["schema_version"] != after["schema_version"] {
		t.Errorf("schema_version changed on idempotent re-run: %v -> %v", before["schema_version"], after["schema_version"])
	}
	if after["bench"] != false || after["cert_path"] != "" {
		t.Errorf("unexpected mutation on idempotent re-run: %#v", after)
	}

	matches, err := filepath.Glob(path + ".pre-v*")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 1 {
		t.Errorf("expected exactly one backup file after two runs, got %v", matches)
	}
}

// TestProcessFile_RefusesDownMigrate: a schema_version from a release newer
// than this binary's registry knows about is left completely untouched and
// reported as an error.
func TestProcessFile_RefusesDownMigrate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.json")
	writeJSON(t, path, map[string]any{"schema_version": 7, "mqtt_broker": "tcp://x:1883"})
	before := readJSON(t, path)

	result, err := processFile(dir, "hub.json", false)
	if err == nil {
		t.Fatalf("expected an error for a from-the-future schema_version, got nil (result=%v)", result)
	}
	t.Logf("got expected refusal: %v", err)

	after := readJSON(t, path)
	if after["schema_version"] != before["schema_version"] || after["mqtt_broker"] != before["mqtt_broker"] {
		t.Errorf("file was modified despite refused down-migrate: before=%#v after=%#v", before, after)
	}
	mustNotExist(t, path+".staged")
	matches, _ := filepath.Glob(path + ".pre-v*")
	if len(matches) != 0 {
		t.Errorf("no backup should be created for a refused down-migrate, got %v", matches)
	}
}

// TestProcessFile_BackupOnce: a pre-existing backup file must never be
// overwritten, even though the live file still migrates normally.
func TestProcessFile_BackupOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "modbus.json")
	writeJSON(t, path, map[string]any{"poll_interval_s": 10})

	backupPath := path + ".pre-v0"
	sentinel := map[string]any{"marker": "pre-existing-backup-must-not-be-overwritten"}
	writeJSON(t, backupPath, sentinel)

	if _, err := processFile(dir, "modbus.json", false); err != nil {
		t.Fatalf("processFile: %v", err)
	}

	got := readJSON(t, backupPath)
	if got["marker"] != sentinel["marker"] {
		t.Errorf("pre-existing backup was overwritten: %#v", got)
	}
	live := readJSON(t, path)
	if live["schema_version"] != float64(1) {
		t.Errorf("live file schema_version = %v, want 1 (should still migrate normally)", live["schema_version"])
	}
}

// TestProcessFile_PreservesFileMode: replacing a config via rename must not
// silently change its permission mode.
func TestProcessFile_PreservesFileMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "modbus.json")
	writeJSON(t, path, map[string]any{"poll_interval_s": 10})
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if _, err := processFile(dir, "modbus.json", false); err != nil {
		t.Fatalf("processFile: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("mode = %v, want 0600 (preserved from before migration)", info.Mode().Perm())
	}
}

// TestProcessFile_MissingSkippedSilently: a config file that doesn't exist
// is resultMissing, never an error — uncommissioned units ship without
// every service configured.
func TestProcessFile_MissingSkippedSilently(t *testing.T) {
	dir := t.TempDir()
	result, err := processFile(dir, "cloudlink.json", false)
	if err != nil {
		t.Fatalf("processFile on a missing file returned an error: %v", err)
	}
	if result != resultMissing {
		t.Fatalf("result = %v, want resultMissing", result)
	}
}

// TestReadVersion covers the schema_version decode edge cases: absent/null
// -> 0, a non-numeric value or a non-integer/negative number -> error
// (never a silent fallback to 0, which could skip a migration a corrupt
// file actually needs).
func TestReadVersion(t *testing.T) {
	cases := []struct {
		name    string
		doc     map[string]any
		want    int
		wantErr bool
	}{
		{"absent", map[string]any{}, 0, false},
		{"null", map[string]any{"schema_version": nil}, 0, false},
		{"zero", map[string]any{"schema_version": float64(0)}, 0, false},
		{"one", map[string]any{"schema_version": float64(1)}, 1, false},
		{"string", map[string]any{"schema_version": "1"}, 0, true},
		{"fractional", map[string]any{"schema_version": 1.5}, 0, true},
		{"negative", map[string]any{"schema_version": float64(-1)}, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readVersion(tc.doc)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got version %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("version = %d, want %d", got, tc.want)
			}
		})
	}
}
