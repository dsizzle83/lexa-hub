package main

import (
	"path/filepath"
	"testing"
)

// TestRunMigrations_MissingFilesDoNotFailTheRun: a directory with only some
// of the seven known configs present is a fully successful run.
func TestRunMigrations_MissingFilesDoNotFailTheRun(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, filepath.Join(dir, "hub.json"), map[string]any{"mqtt_broker": "x"})

	if exit := runMigrations(dir, false); exit != 0 {
		t.Fatalf("runMigrations exit = %d, want 0", exit)
	}
	got := readJSON(t, filepath.Join(dir, "hub.json"))
	if got["schema_version"] != float64(1) {
		t.Errorf("hub.json schema_version = %v, want 1", got["schema_version"])
	}
}

// TestRunMigrations_MixedResultsReturnsNonzeroOnAnyFailure: one file's
// refused down-migrate must be reported (non-zero exit) without preventing
// an unrelated, migratable file from being brought up to date.
func TestRunMigrations_MixedResultsReturnsNonzeroOnAnyFailure(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, filepath.Join(dir, "hub.json"), map[string]any{"mqtt_broker": "x"})
	writeJSON(t, filepath.Join(dir, "ocpp.json"), map[string]any{"schema_version": 99})

	if exit := runMigrations(dir, false); exit != 1 {
		t.Fatalf("runMigrations exit = %d, want 1", exit)
	}

	gotHub := readJSON(t, filepath.Join(dir, "hub.json"))
	if gotHub["schema_version"] != float64(1) {
		t.Errorf("hub.json should still have migrated despite ocpp.json's refusal: %#v", gotHub)
	}
	gotOCPP := readJSON(t, filepath.Join(dir, "ocpp.json"))
	if gotOCPP["schema_version"] != float64(99) {
		t.Errorf("ocpp.json should be left completely untouched: %#v", gotOCPP)
	}
}

// TestRunMigrations_EmptyDirAllMissing: an entirely empty config dir (the
// most uncommissioned possible state) is a clean, zero-exit no-op.
func TestRunMigrations_EmptyDirAllMissing(t *testing.T) {
	dir := t.TempDir()
	if exit := runMigrations(dir, false); exit != 0 {
		t.Fatalf("runMigrations exit = %d, want 0 for an entirely empty config dir", exit)
	}
}
