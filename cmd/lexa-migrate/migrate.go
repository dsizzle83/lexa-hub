package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"syscall"
)

// fileResult reports what processFile did for one config file.
type fileResult int

const (
	resultMissing   fileResult = iota // file does not exist — silently skipped, never an error
	resultUnchanged                   // file already at its registry's max known version
	resultMigrated                    // file was stepped forward one or more versions
)

// processFile brings the single config file base (e.g. "hub.json") inside
// dir up to its migrations registry's max known version, in place. It is
// idempotent (re-running on an already-current file is resultUnchanged,
// nil) and per-file isolated (an error here never touches any other file).
//
// dryRun performs every read and every version/down-migrate check exactly
// as a real run would, but skips every filesystem write (no backup file, no
// staged file, no rename) — see the dryRun branches below.
func processFile(dir, base string, dryRun bool) (fileResult, error) {
	path := filepath.Join(dir, base)

	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return resultMissing, nil
	} else if err != nil {
		return resultMissing, fmt.Errorf("stat: %w", err)
	}

	// Finish an interrupted commit from a prior crashed run before doing
	// anything else — see recoverStaged's doc. This can itself replace path
	// (via rename), so the os.Stat/os.ReadFile below always see whatever
	// recovery left behind, not a stale view from before it ran.
	if err := recoverStaged(path, dryRun); err != nil {
		return resultMissing, fmt.Errorf("recover staged file: %w", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		return resultMissing, fmt.Errorf("stat after recovery: %w", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return resultMissing, fmt.Errorf("read: %w", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return resultMissing, fmt.Errorf("parse: %w", err)
	}

	version, err := readVersion(doc)
	if err != nil {
		return resultMissing, err
	}

	steps := migrations[base]
	maxV := maxKnownVersion(steps)

	if version > maxV {
		return resultMissing, fmt.Errorf(
			"schema_version %d is NEWER than this binary's known max %d for %s — refusing to down-migrate. "+
				"This usually means an A/B rollback landed an OLDER lexa-migrate binary in front of a config "+
				"already migrated forward by the release it rolled back FROM. This file is left COMPLETELY "+
				"UNTOUCHED; if it genuinely needs to be forced back to an earlier shape, the per-step backups "+
				"%s.pre-v<N> (written before each forward migration ever mutated the file) are the recovery path",
			version, maxV, base, path)
	}
	if version == maxV {
		return resultUnchanged, nil
	}

	cur := version
	for _, step := range steps {
		if step.version <= cur {
			continue // already applied — keeps this loop correct even if the registry were ever non-contiguous
		}
		backupPath := fmt.Sprintf("%s.pre-v%d", path, cur)
		if dryRun {
			log.Printf("lexa-migrate: [dry-run] %s: would back up to %s and migrate v%d -> v%d",
				base, filepath.Base(backupPath), cur, step.version)
			cur = step.version
			continue
		}
		if err := backupOnce(path, backupPath, info); err != nil {
			return resultMissing, fmt.Errorf("backup before v%d->v%d: %w", cur, step.version, err)
		}
		if err := step.apply(doc); err != nil {
			return resultMissing, fmt.Errorf("apply v%d->v%d: %w", cur, step.version, err)
		}
		doc["schema_version"] = step.version // belt-and-suspenders even if apply forgot to set it
		if err := writeStaged(path, doc, info); err != nil {
			return resultMissing, fmt.Errorf("commit v%d: %w", step.version, err)
		}
		log.Printf("lexa-migrate: %s: migrated v%d -> v%d (backup %s)", base, cur, step.version, filepath.Base(backupPath))
		cur = step.version
	}
	return resultMigrated, nil
}

// readVersion returns doc's "schema_version" as an int, or 0 if the key is
// absent or null — absent means "every config file that predates TASK-099",
// the whole reason this tool exists. encoding/json decodes JSON numbers into
// map[string]any as float64, so a present-but-non-numeric or non-integer
// value is a hard parse error (a hand-edited or corrupt config), not a
// silent fallback to 0 — silently treating a corrupt version as "current"
// could skip a migration the file actually needs.
func readVersion(doc map[string]any) (int, error) {
	raw, ok := doc["schema_version"]
	if !ok || raw == nil {
		return 0, nil
	}
	f, ok := raw.(float64)
	if !ok {
		return 0, fmt.Errorf("schema_version is %T (%v), want a number", raw, raw)
	}
	if f < 0 || f != float64(int(f)) {
		return 0, fmt.Errorf("schema_version %v is not a non-negative integer", f)
	}
	return int(f), nil
}

// backupOnce copies path to backupPath, but ONLY if backupPath does not
// already exist. A retried migration (e.g. re-run after a crash, or a
// no-op second run over a file some earlier step already touched) must
// never overwrite the TRUE pre-migration original with a since-mutated
// copy — the backup's entire value is being an untouched snapshot of what
// this exact version used to look like. info is path's os.Stat result at
// the time this step started, used to preserve its owner/group/mode on the
// backup file (see writeFileLike).
func backupOnce(path, backupPath string, info os.FileInfo) error {
	if _, err := os.Stat(backupPath); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return writeFileLike(backupPath, data, info)
}

// writeStaged marshals doc, writes it to path+".staged" (fsynced), and
// renames it over path. This is the same write-fsync-rename discipline
// internal/journal uses for its own rotation (journal.go's rotateFiles) —
// the live file is never opened for in-place writes, so a crash mid-write
// can never leave it torn. A crash between the fsync'd write and the
// rename leaves path+".staged" behind for the NEXT run's recoverStaged to
// finish.
//
// Note on fidelity: doc round-trips through a generic map[string]any, so
// the committed file is NOT byte-identical to the original — key order is
// not preserved (Go's map iteration order for json.Marshal is sorted by
// key, not the original file's order) and numeric literal formatting can
// change (e.g. an original "10.0" becomes "10" — both decode to the
// identical float64, so no consumer of these files, which all
// encoding/json-decode into typed struct fields, can observe a difference).
// Every KEY and VALUE is preserved; only incidental textual formatting is
// not. This is a deliberate simplicity/robustness tradeoff: a tool that
// tried to patch the original bytes in place to preserve formatting exactly
// would be far more fragile for very little practical benefit.
func writeStaged(path string, doc map[string]any, info os.FileInfo) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	stagedPath := path + ".staged"
	if err := writeFileLike(stagedPath, data, info); err != nil {
		return err
	}
	return os.Rename(stagedPath, path)
}

// writeFileLike writes data to path (create/truncate, fsynced) and, when
// like is non-nil, matches its permission mode and (Linux-specific — the
// only platform this repo targets, an ARM64 embedded Linux SOM) owner/group.
// lexa-migrate runs as root (systemd/lexa-migrate.service) so it can write
// configs regardless of their current ownership, but the six lexa-*.service
// units run as an unprivileged user reading these same files — replacing a
// file via rename must never silently change who can read it.
func writeFileLike(path string, data []byte, like os.FileInfo) error {
	mode := os.FileMode(0640)
	if like != nil {
		mode = like.Mode().Perm()
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if like == nil {
		return nil
	}
	// OpenFile's mode is masked by umask; chmod explicitly so the result
	// matches the original file's mode exactly, not mode&^umask.
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	if st, ok := like.Sys().(*syscall.Stat_t); ok {
		if err := os.Chown(path, int(st.Uid), int(st.Gid)); err != nil {
			return err
		}
	}
	return nil
}

// recoverStaged completes an interrupted commit from a PRIOR run.
// writeStaged only ever renames path+".staged" over path AFTER the staged
// file has been fully written and fsynced, so a leftover path+".staged"
// found at the START of a run means a previous run crashed between that
// write and the rename — the staged content is trustworthy (it was
// fsynced) and the rename just needs finishing.
//
// A staged file that fails to parse as JSON is instead a torn write from a
// crash DURING the write itself (before fsync completed, e.g. mid-write
// power loss); it is untrustworthy and is discarded, leaving path (the last
// successfully committed version) as the source of truth — never commit
// unparsable bytes over a good config.
func recoverStaged(path string, dryRun bool) error {
	stagedPath := path + ".staged"
	data, err := os.ReadFile(stagedPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var probe map[string]any
	if json.Unmarshal(data, &probe) != nil {
		log.Printf("lexa-migrate: %s: discarding an unparsable leftover .staged file (a torn write from an earlier crash)", filepath.Base(path))
		if dryRun {
			return nil
		}
		return os.Remove(stagedPath)
	}
	log.Printf("lexa-migrate: %s: recovering an interrupted commit (renaming leftover .staged over the live file)", filepath.Base(path))
	if dryRun {
		return nil
	}
	return os.Rename(stagedPath, path)
}
