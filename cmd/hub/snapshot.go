package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// TASK-041 (AD-005 second half, review W5/§11/§8.3): a small atomic JSON
// snapshot of the hub's breach-episode identity, so a restart mid-breach does
// not re-emit a duplicate CannotComply "begin" for an episode that is still
// genuinely open. See breachEpisodes' doc comment in breach.go for exactly
// which fields are persisted and why (only the three identity fields —
// activeMRID, episodeID, counter — the evidence maps re-seed live).
//
// Deliberately NOT covered by this snapshot (AD-005's bound: "only state
// whose loss causes protocol noise or safety regressions"): the optimizer's
// guard sessions (expGuard/impGuard/genGuard, internal/orchestrator/
// optimizer.go). They re-converge within a few ticks, and restoring stale
// guard state across a restart is exactly the guard×guard interaction class
// W2 warns about — do not "complete" this snapshot with it.
//
// Restore trust (§8.3 — a stale snapshot is a stale-state hazard, same
// family as retained-message trust): every snapshot carries written_at;
// loadHubSnapshot discards (never trusts) a snapshot whose written_at is
// older than maxAge or in the future (a local clock step — TASK-037).

// hubSnapshotV is the snapshot file's own schema version (independent of
// internal/journal's SchemaV — this is a different file, a different
// format). Bump only on a breaking change to hubSnapshot's shape.
const hubSnapshotV = 1

// DefaultSnapshotMaxAgeS is the default staleness bound applied when a
// SnapshotConfig omits max_age_s.
const DefaultSnapshotMaxAgeS = 300

// SnapshotConfig is the on-disk "snapshot" hub.json block. Enabled gates
// RESTORE only (seeding breachEpisodes' identity at start) — the snapshot is
// still WRITTEN whenever Path is non-empty regardless of Enabled, so the
// rollout can run one full campaign write-only (files appear, nothing reads
// them) before an ops-only config flip turns restore on (no code change
// accompanies that flip — see TASK-041's "Implementation strategy").
type SnapshotConfig struct {
	Enabled bool   `json:"enabled"`
	Path    string `json:"path"`
	MaxAgeS int    `json:"max_age_s"` // 0 -> DefaultSnapshotMaxAgeS
}

// maxAge returns sc's staleness bound, defaulting a nil/zero-valued config to
// DefaultSnapshotMaxAgeS.
func (sc *SnapshotConfig) maxAge() time.Duration {
	if sc == nil || sc.MaxAgeS <= 0 {
		return DefaultSnapshotMaxAgeS * time.Second
	}
	return time.Duration(sc.MaxAgeS) * time.Second
}

// hubSnapshot is the file's top-level shape. ActiveBreach is nil when no
// episode was open at write time (a valid, common steady-state snapshot —
// restoring it is simply a no-op).
type hubSnapshot struct {
	V            int             `json:"v"`
	WrittenAt    int64           `json:"written_at"`
	ActiveBreach *breachSnapshot `json:"active_breach,omitempty"`
}

// breachSnapshot is the persisted subset of breachEpisodes' identity state —
// see breach.go's "TASK-041 snapshot note" for why only these three fields.
type breachSnapshot struct {
	EpisodeID string `json:"episode_id"`
	MRID      string `json:"mrid"`
	Counter   uint64 `json:"counter"`
}

// ErrSnapshotStale is returned by loadHubSnapshot for a snapshot whose
// written_at is older than the configured max age, or in the future (a local
// clock step, TASK-037) — either way, a doubtful snapshot that restore must
// discard rather than trust.
var ErrSnapshotStale = errors.New("snapshot: stale or future-dated written_at")

// ErrSnapshotVersion is returned by loadHubSnapshot for a v field this
// binary does not understand.
var ErrSnapshotVersion = errors.New("snapshot: unsupported version")

// saveHubSnapshot atomically writes snap to path: marshal -> write path+
// ".tmp" (same directory as path, so the rename below is a same-filesystem,
// atomic rename, never a cross-filesystem copy) -> fsync the tmp file ->
// os.Rename(tmp, path). A concurrent Load (or a reader killed mid-write)
// never observes a partial file: the rename is what makes the new content
// visible, and it is atomic within one filesystem.
func saveHubSnapshot(path string, snap hubSnapshot) error {
	if path == "" {
		return errors.New("snapshot: empty path")
	}
	snap.V = hubSnapshotV
	if snap.WrittenAt == 0 {
		snap.WrittenAt = time.Now().Unix()
	}
	b, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("snapshot: marshal: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("snapshot: mkdir %s: %w", dir, err)
	}

	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("snapshot: create tmp %s: %w", tmp, err)
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("snapshot: write tmp %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("snapshot: fsync tmp %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("snapshot: close tmp %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("snapshot: rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}

// loadHubSnapshot reads and validates path. A missing file returns
// os.ErrNotExist unwrapped (os.ReadFile's own sentinel) so callers can treat
// "no snapshot yet" (first boot, or a fresh volume) as ordinary via
// errors.Is, not as a fault to log loudly. A corrupt file, an unrecognized
// version, or a stale/future-dated written_at is discarded with a typed
// error — restore must never act on a doubtful snapshot (§8.3).
func loadHubSnapshot(path string, maxAge time.Duration, now time.Time) (hubSnapshot, error) {
	var snap hubSnapshot
	b, err := os.ReadFile(path)
	if err != nil {
		return snap, err
	}
	if err := json.Unmarshal(b, &snap); err != nil {
		return hubSnapshot{}, fmt.Errorf("snapshot: corrupt %s: %w", path, err)
	}
	if snap.V != hubSnapshotV {
		return hubSnapshot{}, fmt.Errorf("%w: got %d want %d", ErrSnapshotVersion, snap.V, hubSnapshotV)
	}
	age := now.Unix() - snap.WrittenAt
	if age < 0 || time.Duration(age)*time.Second > maxAge {
		return hubSnapshot{}, fmt.Errorf("%w: written_at=%d now=%d max_age=%s",
			ErrSnapshotStale, snap.WrittenAt, now.Unix(), maxAge)
	}
	return snap, nil
}
