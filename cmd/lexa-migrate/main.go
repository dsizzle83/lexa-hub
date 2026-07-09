// lexa-migrate is the boot-time config schema migrator (Unit 1.6,
// docs/DEVICE_ROADMAP.md §8.2). It runs once, as a systemd oneshot ordered
// before the lexa-*.service units (systemd/lexa-migrate.service's Before=
// list), and brings every /etc/lexa/<name>.json file it knows about up to
// the schema version its own migrations registry (migrations.go)
// understands.
//
// Why this exists: this box updates its rootfs A/B (Mender), but
// /etc/lexa + /var/lib/lexa live on a persistent data partition shared by
// both slots. A new release's config-reading code can require a JSON shape
// an OLD config file doesn't have; without an explicit, versioned migration
// step, "roll the rootfs forward" and "the data partition already matches"
// are two different, silently-divergible facts. Putting a "schema_version"
// integer on every config file and stepping it forward under a dedicated,
// tested tool is the same discipline bus.Envelope already applies to the
// MQTT wire (every message carries "v"); this is that habit applied to the
// data partition.
//
// Behavior summary (see migrate.go/migrations.go for the mechanics):
//
//   - Missing files are skipped silently — an uncommissioned unit, or one
//     that simply has no config for a given service, is not an error.
//   - A file already at its registry's max known version is a silent
//     no-op — safe to run on every boot, not just the first one on a new
//     slot.
//   - A file whose "schema_version" is NEWER than anything this binary's
//     registry knows about is left completely untouched and reported as a
//     loud failure (non-zero exit, ERROR log line). This is the case an
//     A/B rollback creates: the rootfs slot you rolled back TO carries an
//     OLDER lexa-migrate binary than the one that last touched this config,
//     and an old binary must never guess how to "downgrade" a shape it
//     does not fully understand. The per-step backups
//     (<file>.pre-v<N>, written before each forward step ever mutated the
//     file) are the recovery path if a config genuinely needs to be forced
//     back to an earlier shape by hand.
//
// Usage:
//
//	lexa-migrate [-config-dir /etc/lexa] [-dry-run]
package main

import (
	"flag"
	"log"
	"os"
)

// targetBases are the config files lexa-migrate looks for, in the order
// docs/DEVICE_ROADMAP.md §8.2 lists the seven lexa services. "cloudlink.json"
// does not exist on any deployed unit yet (cmd/cloudlink lands in a later
// unit, TASK-085) — it is listed here so its v0->v1 step is registered and
// ready the day that config file starts shipping; until then processing it
// is always a silent resultMissing.
var targetBases = []string{
	"hub.json",
	"northbound.json",
	"modbus.json",
	"ocpp.json",
	"telemetry.json",
	"api.json",
	"cloudlink.json",
}

func main() {
	configDir := flag.String("config-dir", "/etc/lexa", "directory containing the lexa service JSON configs")
	dryRun := flag.Bool("dry-run", false, "log intended actions without writing, backing up, or renaming any files")
	flag.Parse()

	os.Exit(runMigrations(*configDir, *dryRun))
}

// runMigrations processes every targetBases entry under configDir and
// returns the process exit code the systemd unit's ExecStart relies on: 0
// if every present file is now (or was already) at its known-max schema
// version, 1 if ANY file failed. Files are processed independently — one
// file's failure (most commonly a refused down-migrate) never stops the
// others from being brought up to date, since each config belongs to a
// different service and there is no reason a bad hub.json should leave a
// migratable ocpp.json untouched.
//
// A refused down-migrate is deliberately reported as a failure even under
// -dry-run: dry-run exists so this exact condition can be caught by a
// preflight check (e.g. an OTA test harness) before it ever happens for
// real on a booting device, not just to preview successful migrations.
func runMigrations(configDir string, dryRun bool) int {
	exit := 0
	var migrated, unchanged, missing int
	for _, base := range targetBases {
		result, err := processFile(configDir, base, dryRun)
		if err != nil {
			log.Printf("lexa-migrate: %s: ERROR: %v", base, err)
			exit = 1
			continue
		}
		switch result {
		case resultMissing:
			missing++
		case resultUnchanged:
			unchanged++
		case resultMigrated:
			migrated++
		}
	}
	mode := ""
	if dryRun {
		mode = " [dry-run]"
	}
	log.Printf("lexa-migrate%s: done: %d migrated, %d already current, %d absent (of %d known configs)",
		mode, migrated, unchanged, missing, len(targetBases))
	return exit
}
