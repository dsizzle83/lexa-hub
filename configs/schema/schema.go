// Package schema go:embeds the commissioning config-write allowlists
// (DEVICE_ROADMAP.md §4.5, TASK-090) — one minimal hand-rolled schema per
// lexa-* service config, consumed by cmd/api/configwrite.go's validator.
//
// Why embedded rather than read from an installed path: the ecosystem
// roadmap's §4.5 sketch describes a schema "shipped in the image, generated
// from the config structs at build time" and read from
// /usr/share/lexa/schema/<service>.json at request time. This repo has no
// such install step or generator yet, and Go's //go:embed patterns cannot
// cross a ".." directory boundary (verified: `go:embed ../../configs/schema`
// from cmd/api fails to compile with "invalid pattern syntax") — so a
// cmd/api-local embed of these files isn't possible without duplicating
// their content. Instead, this package lives IN configs/schema/ itself
// (where the embed pattern is a same-directory "*.json", no ".." needed) and
// cmd/api imports it. The schema and the binary that enforces it can now
// never drift apart, and no separate install/copy step is needed — a
// deliberate improvement on the roadmap sketch, not a shortfall. If a future
// requirement needs schema updates without a rebuild, swapping this embed.FS
// read for an os.ReadFile against a configurable directory is a small,
// isolated change confined to configwrite.go's loadConfigSchemas.
package schema

import "embed"

// FS holds every <service>.json file in this directory, embedded at compile
// time.
//
//go:embed *.json
var FS embed.FS
