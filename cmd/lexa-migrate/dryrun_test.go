package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestProcessFile_DryRunTouchesNothing: -dry-run must report a pending
// migration but write nothing at all — no mutated live file, no backup, no
// staged file.
func TestProcessFile_DryRunTouchesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "telemetry.json")
	writeJSON(t, path, map[string]any{"devices": []any{"inverter-0"}, "mup_post_rate_s": 60.0})

	beforeBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	result, err := processFile(dir, "telemetry.json", true)
	if err != nil {
		t.Fatalf("dry-run processFile: %v", err)
	}
	if result != resultMigrated {
		t.Fatalf("dry-run result = %v, want resultMigrated (there IS a pending migration to report)", result)
	}

	afterBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if string(beforeBytes) != string(afterBytes) {
		t.Errorf("dry-run modified the live file:\nbefore=%s\nafter=%s", beforeBytes, afterBytes)
	}
	mustNotExist(t, path+".pre-v0")
	mustNotExist(t, path+".staged")
}

// TestProcessFile_DryRunStillReportsDownMigrateRefusal: the down-migrate
// guard is a read-only check and must still fire (and still error) under
// -dry-run — this is what makes -dry-run useful as an OTA preflight check,
// not just a preview of successful migrations.
func TestProcessFile_DryRunStillReportsDownMigrateRefusal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.json")
	writeJSON(t, path, map[string]any{"schema_version": 99})

	if _, err := processFile(dir, "hub.json", true); err == nil {
		t.Fatalf("expected dry-run to still surface the down-migrate refusal as an error")
	}
	mustNotExist(t, path+".staged")
}

// TestRunMigrations_DryRunModeInSummaryLog is a light smoke test that
// runMigrations' dry-run path returns 0 for an all-clean directory (no
// down-migrate refusals) and touches nothing.
func TestRunMigrations_DryRunModeInSummaryLog(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, filepath.Join(dir, "hub.json"), map[string]any{"mqtt_broker": "x"})

	if exit := runMigrations(dir, true); exit != 0 {
		t.Fatalf("runMigrations dry-run exit = %d, want 0", exit)
	}
	got := readJSON(t, filepath.Join(dir, "hub.json"))
	if _, has := got["schema_version"]; has {
		t.Errorf("dry-run runMigrations should not have touched hub.json: %#v", got)
	}
}
