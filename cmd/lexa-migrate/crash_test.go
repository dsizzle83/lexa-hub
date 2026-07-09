package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestProcessFile_RecoversFromStaleStaged_ValidContent simulates a crash
// AFTER writeStaged fully wrote+fsynced <file>.staged but BEFORE the rename
// that would have committed it — leaving the live file still looking like
// v0 and an orphaned, fully-valid .staged file holding the v1 content. The
// next run must finish that commit (rename .staged over the live file)
// rather than getting confused or re-migrating from scratch.
func TestProcessFile_RecoversFromStaleStaged_ValidContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api.json")
	writeJSON(t, path, map[string]any{"listen_addr": "127.0.0.1:9100"})
	writeJSON(t, path+".staged", map[string]any{
		"listen_addr":    "127.0.0.1:9100",
		"schema_version": 1,
	})

	result, err := processFile(dir, "api.json", false)
	if err != nil {
		t.Fatalf("processFile: %v", err)
	}
	// The leftover .staged already held the fully-migrated v1 content;
	// recovering it (rename over path) brings the live file to v1 before
	// the version check even runs, so there is nothing left to migrate.
	if result != resultUnchanged {
		t.Fatalf("result = %v, want resultUnchanged (recovery alone should have finished the v1 commit)", result)
	}

	got := readJSON(t, path)
	if got["schema_version"] != float64(1) {
		t.Errorf("schema_version = %v, want 1 (recovered from .staged)", got["schema_version"])
	}
	if got["listen_addr"] != "127.0.0.1:9100" {
		t.Errorf("listen_addr lost during recovery: %#v", got)
	}
	mustNotExist(t, path+".staged")
}

// TestProcessFile_DiscardsTornStaged simulates a crash DURING the staged
// write itself (before fsync completed) — a truncated, unparsable leftover
// .staged file. That content is untrustworthy and must be discarded, after
// which the still-intact live v0 file migrates completely normally.
func TestProcessFile_DiscardsTornStaged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "northbound.json")
	writeJSON(t, path, map[string]any{"server": "1.2.3.4:1111"})
	if err := os.WriteFile(path+".staged", []byte(`{"server": "1.2.3.4:1111", "sch`), 0640); err != nil {
		t.Fatalf("write torn staged: %v", err)
	}

	result, err := processFile(dir, "northbound.json", false)
	if err != nil {
		t.Fatalf("processFile: %v", err)
	}
	if result != resultMigrated {
		t.Fatalf("result = %v, want resultMigrated (torn .staged discarded, then the intact live v0 file migrates)", result)
	}
	mustNotExist(t, path+".staged")

	got := readJSON(t, path)
	if got["schema_version"] != float64(1) || got["server"] != "1.2.3.4:1111" {
		t.Errorf("unexpected final content: %#v", got)
	}
}

// TestRecoverStaged_DryRunTouchesNothing: even the crash-recovery path must
// respect -dry-run.
func TestRecoverStaged_DryRunTouchesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ocpp.json")
	writeJSON(t, path, map[string]any{"a": float64(1)})
	writeJSON(t, path+".staged", map[string]any{"a": float64(1), "schema_version": float64(1)})

	if err := recoverStaged(path, true); err != nil {
		t.Fatalf("recoverStaged dry-run: %v", err)
	}
	if _, err := os.Stat(path + ".staged"); err != nil {
		t.Errorf(".staged should still exist after a dry-run recovery: %v", err)
	}
	got := readJSON(t, path)
	if _, has := got["schema_version"]; has {
		t.Errorf("live file should be untouched by a dry-run recovery: %#v", got)
	}
}
