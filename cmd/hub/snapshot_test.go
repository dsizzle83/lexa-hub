package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestSaveHubSnapshot_AtomicRename proves a concurrent reader never observes
// a partially-written file: one goroutine repeatedly Saves (matching
// production, where breachEpisodes.mu already serializes every writer — this
// is not a "two writers, one tmp name" test, which would race on the shared
// tmp filename for a reason that has nothing to do with atomicity) while a
// concurrent reader goroutine hammers the raw bytes and must always see
// either the previous complete JSON object or the new one — never a
// truncated/torn mix, and never a "file missing" gap around the rename. This
// is the "atomicity test" the acceptance criteria call for (tmp+rename); it
// is also the "kill the writer mid-Save" property in spirit — a reader
// racing every single rename boundary many times over is what would surface
// a real window if os.Rename here were not atomic.
func TestSaveHubSnapshot_AtomicRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.json")

	// Seed an initial valid file so the reader always has something to read.
	if err := saveHubSnapshot(path, hubSnapshot{ActiveBreach: &breachSnapshot{EpisodeID: "seed", MRID: "seed", Counter: 0}}); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	const iterations = 120
	stop := make(chan struct{})

	// Reader: hammer the file path and demand every read is complete,
	// parseable JSON (never a half-written tmp artifact leaking through, and
	// never an empty/truncated file). Runs until told to stop via `stop`.
	var readerErr error
	var readerMu sync.Mutex
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			b, err := os.ReadFile(path)
			if err != nil {
				// A missing file mid-rename is acceptable only if the rename
				// truly never leaves a gap — os.Rename is atomic on POSIX, so
				// this should not happen; treat it as a failure if it does.
				readerMu.Lock()
				readerErr = err
				readerMu.Unlock()
				return
			}
			var snap hubSnapshot
			if err := json.Unmarshal(b, &snap); err != nil {
				readerMu.Lock()
				readerErr = err
				readerMu.Unlock()
				return
			}
		}
	}()

	// Writer: sequential Saves, exactly like the single breachEpisodes.mu
	// holder in production.
	for j := 0; j < iterations; j++ {
		snap := hubSnapshot{ActiveBreach: &breachSnapshot{EpisodeID: "ep", MRID: "m", Counter: uint64(j)}}
		if err := saveHubSnapshot(path, snap); err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	close(stop)
	select {
	case <-readerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for reader goroutine to stop")
	}

	readerMu.Lock()
	defer readerMu.Unlock()
	if readerErr != nil {
		t.Fatalf("reader observed a torn/corrupt file during concurrent Save: %v", readerErr)
	}

	// A leftover .tmp file would mean a Save path failed to clean up after
	// itself, or (worse) the rename never won the race.
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Fatalf("leftover tmp file %s.tmp after all saves completed", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat %s.tmp: %v", path, err)
	}
}

// TestSaveHubSnapshot_TmpSameDirectory verifies the tmp file is created in
// the SAME directory as the target (a cross-filesystem os.Rename fails, per
// "Common mistakes to avoid" in TASK-041).
func TestSaveHubSnapshot_TmpSameDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "hub.json")
	if err := saveHubSnapshot(path, hubSnapshot{}); err != nil {
		t.Fatalf("saveHubSnapshot: %v", err)
	}
	// The tmp path is path+".tmp", i.e. dir/sub/hub.json.tmp — verify no
	// artifact was left anywhere else (e.g. the OS temp dir) and the final
	// file landed exactly at path.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected final file at %s: %v", path, err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "sub"))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "hub.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected exactly one file (hub.json) in %s/sub, got %v", dir, names)
	}
}

func TestLoadHubSnapshot_MissingFileIsErrNotExist(t *testing.T) {
	dir := t.TempDir()
	_, err := loadHubSnapshot(filepath.Join(dir, "nope.json"), 300*time.Second, time.Now())
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected os.ErrNotExist passthrough, got %v", err)
	}
}

func TestLoadHubSnapshot_CorruptFileRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := loadHubSnapshot(path, 300*time.Second, time.Now())
	if err == nil {
		t.Fatal("expected an error loading a corrupt snapshot, got nil")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("corrupt file must not look like ErrNotExist, got %v", err)
	}
}

func TestLoadHubSnapshot_WrongVersionRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.json")
	b, _ := json.Marshal(hubSnapshot{V: 99, WrittenAt: time.Now().Unix()})
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := loadHubSnapshot(path, 300*time.Second, time.Now())
	if !errors.Is(err, ErrSnapshotVersion) {
		t.Fatalf("expected ErrSnapshotVersion, got %v", err)
	}
}

func TestLoadHubSnapshot_StaleRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.json")
	now := time.Unix(1_800_000_000, 0)
	old := hubSnapshot{V: hubSnapshotV, WrittenAt: now.Add(-10 * time.Minute).Unix(),
		ActiveBreach: &breachSnapshot{EpisodeID: "ep", MRID: "m"}}
	b, _ := json.Marshal(old)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := loadHubSnapshot(path, 300*time.Second, now)
	if !errors.Is(err, ErrSnapshotStale) {
		t.Fatalf("expected ErrSnapshotStale for a 10 min old snapshot with a 300 s max age, got %v", err)
	}
}

// TestLoadHubSnapshot_FutureWrittenAtRejected covers a local clock step
// (TASK-037): a snapshot whose written_at is AHEAD of now must be discarded
// exactly like a stale one, never trusted just because "age" would come out
// negative.
func TestLoadHubSnapshot_FutureWrittenAtRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.json")
	now := time.Unix(1_800_000_000, 0)
	future := hubSnapshot{V: hubSnapshotV, WrittenAt: now.Add(1 * time.Hour).Unix(),
		ActiveBreach: &breachSnapshot{EpisodeID: "ep", MRID: "m"}}
	b, _ := json.Marshal(future)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := loadHubSnapshot(path, 300*time.Second, now)
	if !errors.Is(err, ErrSnapshotStale) {
		t.Fatalf("expected ErrSnapshotStale for a future-dated written_at, got %v", err)
	}
}

func TestLoadHubSnapshot_FreshValidRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.json")
	now := time.Unix(1_800_000_000, 0)
	want := hubSnapshot{WrittenAt: now.Unix(), ActiveBreach: &breachSnapshot{EpisodeID: "X@1700#3", MRID: "X", Counter: 3}}
	if err := saveHubSnapshot(path, want); err != nil {
		t.Fatalf("saveHubSnapshot: %v", err)
	}
	got, err := loadHubSnapshot(path, 300*time.Second, now)
	if err != nil {
		t.Fatalf("loadHubSnapshot: %v", err)
	}
	if got.ActiveBreach == nil || *got.ActiveBreach != *want.ActiveBreach {
		t.Fatalf("round-tripped ActiveBreach = %+v, want %+v", got.ActiveBreach, want.ActiveBreach)
	}
}

func TestLoadHubSnapshot_NoActiveBreachIsValid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.json")
	now := time.Unix(1_800_000_000, 0)
	if err := saveHubSnapshot(path, hubSnapshot{WrittenAt: now.Unix()}); err != nil {
		t.Fatalf("saveHubSnapshot: %v", err)
	}
	got, err := loadHubSnapshot(path, 300*time.Second, now)
	if err != nil {
		t.Fatalf("loadHubSnapshot: %v", err)
	}
	if got.ActiveBreach != nil {
		t.Fatalf("expected nil ActiveBreach for a steady-state snapshot, got %+v", got.ActiveBreach)
	}
}

// TestSaveHubSnapshot_FileHasTrailingContentOnly is a light sanity check that
// the written file is exactly one JSON object (no NDJSON-style multi-line
// leftovers from a botched implementation) — one bufio.Scanner line.
func TestSaveHubSnapshot_FileHasTrailingContentOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.json")
	if err := saveHubSnapshot(path, hubSnapshot{ActiveBreach: &breachSnapshot{EpisodeID: "e", MRID: "m"}}); err != nil {
		t.Fatalf("saveHubSnapshot: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	lines := 0
	for sc.Scan() {
		lines++
	}
	if lines != 1 {
		t.Fatalf("expected exactly 1 line in the snapshot file, got %d", lines)
	}
}
