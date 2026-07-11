// Package apicontract is the versioned, mechanically-enforced contract for the
// hub⇄app local HTTP API (cmd/api, HTTPS :9100) that the companion app
// consumes. It pairs a set of golden wire-shape fixtures (http_v1/*.json,
// captured from the real handlers) with a small, dependency-free structural
// conformance checker (Conform) whose semantics encode this contract's
// ADDITIVE-EVOLUTION rule: the hub may add fields freely within a major
// version, but removing/renaming/retyping a field the app reads is a breaking
// change that requires bumping Version.
//
// This is a leaf package: it imports only the standard library, matching the
// house posture of internal/metrics and internal/watchdog (hand-rolled,
// minimal-dep). It is the HTTP-API analogue of the BLE provisioning contract
// pinned by internal/provision/sec1/testdata/sec1_test_vectors.json (ADR-0002)
// — a single authoritative artifact both ends build against.
//
// The compatibility policy (what forces a bump, the deprecation window, the
// load-bearing field inventory, the non-JSON contract surfaces) lives in
// docs/API_CONTRACT.md.
package apicontract

// Version is the hub⇄app HTTP contract's MAJOR version. The hub advertises it
// three ways so the app can detect a mismatch and warn: the
// X-Lexa-Contract-Version response header on every route (cmd/api middleware),
// the additive "contract_version" field in GET /status and GET /site, and the
// "contract=<Version>" mDNS TXT record (cmd/api/mdns.go).
//
// Bumping rule (full policy in docs/API_CONTRACT.md): additive changes — a NEW
// response field, a new route, a new optional request field — keep this at its
// current value. A BREAKING change to any v1 wire shape the app depends on
// (removing, renaming, or retyping a field; changing a route's method/status
// contract) requires bumping this to 2 and honoring a deprecation window. The
// golden fixtures in http_v1/ are the authoritative v1 shapes; the CI gate
// (cmd/api/contract_test.go) fails a PR that drifts a live handler away from
// them without a matching Version bump + fixture regeneration.
const Version = 1
