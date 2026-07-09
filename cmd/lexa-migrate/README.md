# lexa-migrate

Boot-time config schema migrator (Unit 1.6, `docs/DEVICE_ROADMAP.md` §8.2).
Runs once as a systemd oneshot (`systemd/lexa-migrate.service`), ordered
before the six lexa services, and steps every `/etc/lexa/<name>.json` file it
knows about forward to the schema version its own registry (`migrations.go`)
understands.

## Why

This box updates its rootfs A/B (Mender), but `/etc/lexa` and `/var/lib/lexa`
live on a persistent data partition shared by both slots. A new release's
config-reading code can expect a JSON shape an old, still-on-disk config
file doesn't have. `"schema_version"` on every config file, stepped forward
by a small, independently-tested tool, is the `bus.Envelope` habit (every
MQTT message carries `"v"`) applied to the data partition instead of the
wire.

## Usage

```
lexa-migrate [-config-dir /etc/lexa] [-dry-run]
```

- `-config-dir` — directory holding the seven config files (default
  `/etc/lexa`; overridden in tests).
- `-dry-run` — logs every action it would take (backups, version bumps)
  without writing, backing up, or renaming anything.

Exit code: `0` if every present file ended at (or already was at) its known
max schema version; `1` if any file failed. Failures are per-file — one
bad file never blocks another from migrating.

## What it does, per file

For each of `hub.json`, `northbound.json`, `modbus.json`, `ocpp.json`,
`telemetry.json`, `api.json`, `cloudlink.json` that **exists**:

1. Read `"schema_version"` (absent/null = `0`).
2. If that version is higher than anything this binary's registry knows for
   this file: refuse. Log an `ERROR`, leave the file **completely
   untouched**, exit non-zero.
3. If it already equals the registry's max known version: no-op.
4. Otherwise, apply each pending step in order. Each step:
   - Backs up the file's *current* content to `<file>.pre-v<N>` (`N` = the
     version *before* this step), but **only if that backup doesn't already
     exist** — a retried run must never clobber the true original with a
     since-mutated copy.
   - Applies the step to an in-memory `map[string]any` decode of the file.
   - Writes the result to `<file>.staged`, `fsync`s it, then renames it over
     the live file (the same write-fsync-rename discipline
     `internal/journal` uses for its own rotation — the live file is never
     opened for an in-place write).

Missing files are skipped silently — an uncommissioned unit, or a unit that
simply never had a given service configured, is not a failure.

## The v0 → v1 migration

Identical across all seven files: add `"schema_version": 1`, touch nothing
else. Implementation detail worth knowing: because this is done via a
generic `map[string]any` decode/mutate/encode round-trip (deliberately, so
this tool never has to import every service's typed `Config` struct — see
`migrations.go`'s doc comment), the migrated file is **not byte-identical**
to the original:

- **Key order changes** (Go's JSON encoder emits map keys sorted
  alphabetically, not in the original file's order).
- **Numeric literal formatting can change** (e.g. an original `10.0` becomes
  `10` — both decode to the identical `float64`, so no consumer, all of
  which `encoding/json`-decode into typed struct fields, can observe a
  difference).

Every key and value is preserved; only incidental text formatting is not.

## The backups are the recovery path

**This is the important part.** A `lexa-migrate` binary refusing to touch a
file because its `schema_version` looks like it's from the future is, by
construction, an A/B-rollback scenario: the rootfs slot you rolled back *to*
carries an *older* `lexa-migrate` than the one that last touched this
config on the slot you rolled back *from*. An old binary must never guess
how to downgrade a shape it doesn't fully understand — that is exactly the
kind of silent data corruption a versioned migration exists to prevent.

If a config genuinely needs to be forced back to an earlier shape by hand
(rather than just left alone — remember, services generally tolerate extra
keys they don't recognize, since none of them use
`json.Decoder.DisallowUnknownFields`, so a "too new" config is often
perfectly usable as-is), the `<file>.pre-v<N>` backups this tool wrote
*before ever mutating the file forward* are the only sanctioned recovery
path: `cp /etc/lexa/hub.json.pre-v0 /etc/lexa/hub.json` restores the exact
pre-migration original. `lexa-migrate` itself never deletes a backup it
created, and never overwrites one that already exists.

## Crash safety

- **Staged-rename atomicity**: a crash between the fsync'd write of
  `<file>.staged` and the rename that commits it leaves `<file>.staged`
  behind. The next run's `recoverStaged` finishes that commit (renames it
  over the live file) if it parses as valid JSON — the fsync already proved
  it was fully written — or discards it as a torn write (a crash *during*
  the write, before fsync) if it doesn't parse, leaving the live file (the
  last successfully committed version) as the source of truth.
- **Ownership/mode preservation**: `lexa-migrate` runs as root
  (`systemd/lexa-migrate.service`) so it can write configs regardless of
  their current ownership, but the six `lexa-*.service` units run as an
  unprivileged user reading the very same files. Every write this tool does
  (backups and staged commits alike) matches the original file's owner,
  group, and permission mode — replacing a file via rename never silently
  changes who can read it.

## Tests

`*_test.go` in this package cover: v0 file gains `schema_version` while
preserving unknown/future keys byte-for-byte in *value* (not literal
bytes — see above); idempotent re-run; down-migrate refusal (including
under `-dry-run`, so it's usable as an OTA preflight check); staged-rename
atomicity for both a valid leftover `.staged` (finishes the commit) and a
torn one (discards it); `-dry-run` writes nothing at all; missing files
are skipped without error; a pre-existing backup is never overwritten;
file mode is preserved across the replace.
