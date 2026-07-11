# LEXA hub⇄app contract

Pointers to the two versioned contracts between the LEXA hub and the companion
app. There is no code here — this is a signpost.

## HTTP API contract (Workstream C)

The local HTTP API the app consumes (`cmd/api`, HTTPS `:9100`).

- **Authoritative wire shapes (golden fixtures):**
  [`internal/apicontract/http_v1/`](../internal/apicontract/http_v1) — one JSON
  fixture per app-consumed HTTP shape, captured from the real handlers.
- **Conformance checker:** [`internal/apicontract`](../internal/apicontract) —
  `Conform(golden, live)`, a hand-rolled, stdlib-only structural checker with
  additive-evolution semantics. `apicontract.Version` is the contract's major
  version.
- **Compatibility policy:** [`docs/API_CONTRACT.md`](../docs/API_CONTRACT.md) —
  what forces a version bump, the deprecation window, the load-bearing field
  inventory, the version-advertisement surfaces (header / JSON field / mDNS
  TXT), and the app-side drift list.
- **CI drift gate:** `cmd/api/contract_test.go` (`make contract`) drives the
  real handlers and fails a PR that breaks a v1 shape without a matching
  `apicontract.Version` bump.

## BLE provisioning contract (the precedent this mirrors)

The commissioning handshake the app runs over Bluetooth LE before the HTTP API
is reachable.

- **Byte-for-byte test vectors:**
  [`internal/provision/sec1/testdata/sec1_test_vectors.json`](../internal/provision/sec1/testdata/sec1_test_vectors.json).
- **Design:**
  [`docs/gap-close/ADR-0002-ble-commissioning-protocol.md`](../docs/gap-close/ADR-0002-ble-commissioning-protocol.md).
