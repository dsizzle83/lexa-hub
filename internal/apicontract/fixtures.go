package apicontract

import (
	"embed"
	"encoding/json"
	"io/fs"
)

// httpV1FS embeds the authoritative v1 golden wire-shape fixtures. Each file
// is one app-consumed HTTP shape, captured from the real cmd/api handler it
// documents (see cmd/api/contract_test.go, which both verifies live handlers
// against these and can regenerate them). The directory name encodes the
// contract major version: a future breaking v2 would live under http_v2/
// alongside this one during its deprecation window.
//
//go:embed http_v1/*.json
var httpV1FS embed.FS

// goldenDir is the embed subdirectory holding the v1 fixtures.
const goldenDir = "http_v1"

// Golden returns the raw bytes of the named fixture (e.g. "status.json"), or
// nil if no such fixture is embedded. The bytes are the fixture verbatim —
// pass them straight to Conform as the golden argument.
func Golden(name string) []byte {
	b, err := httpV1FS.ReadFile(goldenDir + "/" + name)
	if err != nil {
		return nil
	}
	return b
}

// Fixtures returns every embedded golden fixture keyed by its base filename
// (e.g. "status.json"). Used by the fixture-integrity test and by any tooling
// that needs to enumerate the full contract surface.
func Fixtures() map[string]json.RawMessage {
	out := make(map[string]json.RawMessage)
	entries, err := fs.ReadDir(httpV1FS, goldenDir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := httpV1FS.ReadFile(goldenDir + "/" + e.Name())
		if err != nil {
			continue
		}
		out[e.Name()] = json.RawMessage(b)
	}
	return out
}
