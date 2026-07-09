package main

import "fmt"

// migration is one versioned step for a single config file: apply mutates
// doc — already decoded from that file's current on-disk JSON object into a
// generic map[string]any — into the shape "schema_version" == version.
//
// Working on map[string]any rather than each service's typed Config struct
// is deliberate: lexa-migrate must never need to import cmd/hub's,
// cmd/ocpp's, etc. Config types (a new, unrelated field any of those add
// would otherwise force a lexa-migrate release too), and it must faithfully
// round-trip every key it doesn't itself touch — including ones this
// binary's own build predates. That is the entire point of putting a schema
// version on the wire in the first place (docs/DEVICE_ROADMAP.md §8.2): a
// migration tool that had to understand a file's full shape to touch one
// key would defeat the reason the version number exists.
type migration struct {
	version int
	apply   func(doc map[string]any) error
}

// addSchemaVersionV1 is the v0->v1 step every known config file shares
// today: stamp "schema_version": 1 and touch nothing else. All other keys —
// known ones this binary's build understands and unknown ones it doesn't —
// pass through doc unmodified; processFile's caller (writeStaged) re-encodes
// the whole map, so nothing here needs to enumerate or preserve them by
// hand.
func addSchemaVersionV1(doc map[string]any) error {
	doc["schema_version"] = 1
	return nil
}

// migrations is keyed by the config file's base name, including the .json
// extension (e.g. "hub.json") — see targetBases in main.go for the seven
// names lexa-migrate looks for. Every file ships the identical v0->v1 step
// today; the registry is per-file so a later release can add a file-specific
// step (e.g. a hypothetical hub.json v1->v2) without touching the other six.
var migrations = map[string][]migration{
	"hub.json":        {{version: 1, apply: addSchemaVersionV1}},
	"northbound.json": {{version: 1, apply: addSchemaVersionV1}},
	"modbus.json":     {{version: 1, apply: addSchemaVersionV1}},
	"ocpp.json":       {{version: 1, apply: addSchemaVersionV1}},
	"telemetry.json":  {{version: 1, apply: addSchemaVersionV1}},
	"api.json":        {{version: 1, apply: addSchemaVersionV1}},
	"cloudlink.json":  {{version: 1, apply: addSchemaVersionV1}},
}

// maxKnownVersion returns the highest version any step in steps produces (0
// if steps is empty). This is the ceiling processFile refuses to migrate
// PAST — a file whose on-disk schema_version already exceeds it came from a
// release newer than this binary, and is left completely untouched (see
// processFile).
func maxKnownVersion(steps []migration) int {
	max := 0
	for _, s := range steps {
		if s.version > max {
			max = s.version
		}
	}
	return max
}

// init sanity-checks the registry once at process start: steps for a given
// file must be sorted ascending, contiguous from 1, with no duplicates or
// gaps (schema_version 0 is the implicit/absent starting point and is never
// itself a step's target version). A violation here is a programming
// mistake in THIS file — not a runtime config problem — so it panics rather
// than surfacing as a per-file processing error.
func init() {
	for base, steps := range migrations {
		prev := 0
		for _, s := range steps {
			if s.version != prev+1 {
				panic(fmt.Sprintf("lexa-migrate: migrations[%q] is not contiguous ascending from 1 (got version %d right after %d)", base, s.version, prev))
			}
			prev = s.version
		}
	}
}
