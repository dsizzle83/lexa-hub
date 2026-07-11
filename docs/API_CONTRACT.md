# Hub⇄App HTTP API Contract

The companion app talks to the hub over the local HTTP API served by `lexa-api`
(`cmd/api`). This document is the **compatibility policy** for that API: what the
app may depend on, what the hub may change freely, and what forces a version
bump. It is the HTTP-API counterpart of the BLE provisioning contract
(`internal/provision/sec1/testdata/sec1_test_vectors.json`, ADR-0002).

- **Authoritative wire shapes:** `internal/apicontract/http_v1/*.json` — one
  golden fixture per app-consumed HTTP shape, captured from the real handlers.
- **Checker:** `internal/apicontract` (`Conform`, hand-rolled, stdlib-only).
- **Version constant:** `apicontract.Version` (currently **1**).
- **CI gate:** `cmd/api/contract_test.go` (run via `make contract`) drives the
  real handlers and fails on breaking drift.

## TLS is mandatory

The API is **HTTPS-only** in the product. `lexa-api` generates a per-device
self-signed leaf cert on first boot (`cmd/api/tlscert.go`); the app learns and
pins the leaf fingerprint during BLE commissioning (TOFU handoff) and verifies
it on every call. A plain-HTTP hub is unreachable by the app by design (TLS is
only disabled for the air-gapped bench via `"tls": false`). The API is bound
loopback-only by default, LAN-reachable only with bearer-token auth (WS-1). Any
port example other than the configured `listen_addr` (default `:9100`) is not
part of this contract.

## The compatibility rule: additive within a major version

The app parses **tolerantly** — it ignores unknown JSON fields. That makes the
following changes **non-breaking** (they keep `apicontract.Version` unchanged):

- Adding a new field to a response object.
- Adding a new route.
- Adding a new optional field to a request body, or a new accepted `/intent`
  kind.
- Populating a field that was previously omitted.

The following changes **break** the app and **require bumping
`apicontract.Version` to 2** plus a deprecation window (see below):

- Removing a field the app reads.
- Renaming a field (a rename is a remove + an add).
- Retyping a field — changing its JSON kind (string↔number, object↔array,
  bool→number, a concrete value→null-only, …).
- Changing a route's method/verb or its success-status contract.
- Tightening a request contract so a previously-accepted body is now rejected.

`Conform(golden, live)` encodes exactly this rule: it walks the golden shape and
flags any golden key missing from, or retyped in, the live response, while
allowing extra live keys. `null` in a golden value means "present, nullable"
(the app models optional quantities as nullable, e.g. `*float64`); JSON's single
number kind means int vs float is never a difference.

## How to make a breaking change

1. Bump `apicontract.Version` to the new major (e.g. `2`).
2. Add the new golden fixtures under `internal/apicontract/http_v2/` (keep
   `http_v1/` during the deprecation window so both can be checked).
3. Update the handlers; regenerate/author the v2 fixtures from the real
   handlers (see `cmd/api/contract_test.go`).
4. Ship the hub and the app together. The hub advertises the new version three
   ways (below) so an older app can detect the mismatch and warn instead of
   silently misparsing.
5. Announce the deprecation window; only after it closes may the v1 shapes and
   fixtures be removed.

## Version advertisement (three surfaces, one constant)

The hub advertises `apicontract.Version` so the app can detect a major mismatch:

| Surface | Where | Value |
|---|---|---|
| HTTP response header | every route (`cmd/api` `withContractVersion` middleware) | `X-Lexa-Contract-Version: 1` |
| JSON field | `GET /status`, `GET /site` | `"contract_version": 1` |
| mDNS TXT record | `_lexa-hub._tcp` advertisement (`cmd/api/mdns.go`) | `contract=1` |

All three read the single `apicontract.Version` constant, so they can never
disagree. An app seeing a `contract_version`/header/TXT major greater than it
was built against should warn the user to update rather than trust the parse.

## Load-bearing field inventory

These are the fields the app actually reads (from a survey of the app's models).
Each is pinned by a golden fixture and the CI gate. Fields are grouped by route;
`?` marks a nullable/optional quantity.

- **`GET /status`** (`status.json`): `timestamp`, `clock_offset_s`,
  `contract_version`, `fw`, `mode`, `api_cert_fp` (present only with TLS on),
  `power{solar_W, battery_W, grid_W, load_W}`,
  `devices{<id>:{role, connected, W_W, V_V, Hz_Hz, soc_pct, max_W}}`,
  `evse_stations[{station_id, connector_id, connected, session_active, status,
  current_A, max_current_A, current_a, max_current_a, voltage_V, power_W,
  soc_pct?}]` (current is emitted under BOTH the capital-`A` bench key and the
  lowercase app key — see the EVSE-current note below),
  `stale_sources[str]`, `last_plan{timestamp, decisions[{rule, reason, impact}]}`,
  `plan_heartbeat{state, age_s}`,
  `csip_control{source, mrid, valid_until, base{exp_lim_W?, max_lim_W?, imp_lim_W?, fixed_W?, connect?}}`,
  `cert_status{client_days_left, ca_days_left, days_left, checked_at}`,
  `cloud_link{connected, endpoint, spool_bytes, cert_days_left}`,
  `reserve{effective_pct?, floor_pct?, source}`,
  `tariff{source, updated_at, spec{currency, periods[{label, days, start_hh, end_hh, import_per_kwh, export_per_kwh?}]}}`.
- **`GET /site`** (`site.json`): `serial`, `fw`, `commissioned`, `tz`,
  `site_cache?`, `contract_version`.
- **`GET /devices`** (`devices.json`): `devices{…}`, `evse_stations[…]`,
  `scan_result{id, ts, devices[{url, unit_id, manufacturer, model, serial, fw_version, class, nameplate_w?}]}`,
  `ocpp_pending[{station_id, vendor, model, first_seen_ts, remote_addr}]`.
- **`GET /telemetry/recent`** (`telemetry_recent.json`): `minutes`,
  `devices{<key>:[{arrived_at, kind, …per-kind fields}]}` — measurement →
  `w/voltage_v/hz`; batt_metrics → `soc_pct/soh_pct/capacity_wh/max_charge_w/max_discharge_w`;
  evse → `current_a/max_current_a/power_w/energy_wh/status`.
- **`GET /mode`** (`mode.json`): `mode`, `since`, `actor`, `intent_id`. (The app
  tolerates `503`→null before the first mode arrives.)
- **`GET /plan`** (`plan.json`): `generated_at`, `horizon_h`, `slot_minutes`,
  `currency?`, `total_cost?`, `fixed_daily_charge?`,
  `solar_forecast[{t, solar_W}]`, `battery_plan[{t, setpoint_W, soc_pct?}]`,
  `ev_plan{<station>:[{t, power_W}]}`,
  `price_forecast[{t, import_per_kwh, delivery_per_kwh, export_per_kwh}]`,
  `cost_plan[{t, grid_W, marginal_cost}]`. (`503` before the first schedule.
  The economics fields — `currency`/`total_cost`/`fixed_daily_charge` and the
  `price_forecast`/`cost_plan` series — are additive and emitted only once the
  schedule carries them.)
- **`GET /scan`** (`scan_get.json`): `status[{id, phase, probed, found, detail, ts}]`,
  `result{id, ts, devices[…]}`.
- **`POST /scan`** (`scan_post.json`): `202 {id}`.
- **`POST /intent`** → `IntentResult` (`intent_result.json`): `{id, kind,
  outcome, detail, ts}`, `outcome ∈ applied|clamped|rejected|expired|duplicate`
  (plus `202 {outcome:"pending"}` on timeout; unknown outcomes tolerated).
- **`POST /intent` request bodies** (`intent_<kind>_request.json` for
  `mode|evgoal|reserve|tariff|chargenow`): the `{kind, body{…}}` shapes the app
  sends. `chargenow` requires `ttl_s > 0`; state kinds ignore any `ttl_s`.

## Non-JSON contract surfaces (not golden-fixtured)

These are part of the contract but are not JSON shapes, so they have no fixture:

- **`GET /healthz`** — liveness; body is the literal `ok`, always unauthenticated.
- **`GET /metrics`** — Prometheus text exposition, unauthenticated.
- **`GET /logs`** — Server-Sent Events; each event is a `data: <line>\n\n` frame.

## EVSE current key: resolved additively (hub-side)

The contract work surfaced a real drift: `/status` and `/devices` historically
emitted EVSE current only under the capital-`A` bench keys `current_A` /
`max_current_A` (read by csip-tls-test's `dashboard.html` + mayhem driver,
matching evsim's output and the `V_V`/`W_W` device-field style), while the
companion app's status model reads the lowercase `current_a` / `max_current_a`
(the same form `/telemetry/recent` already uses). JSON keys are case-sensitive,
so the app's EVSE-current fields were silently blank.

Resolved **additively on the hub** (`evseJSON.MarshalJSON`, `cmd/api/handlers.go`):
`/status` and `/devices` now emit current under **both** keys — the capital form
for the existing bench consumers and the lowercase form for the app. They are
pure aliases of one value and both are pinned in the v1 golden fixtures, so
neither side breaks and the contract enforces both going forward. No app or
bench change was required.

## Drift list handed to the app team

Remaining discrepancies between the app's models and the hub, surfaced while
pinning this contract (these are **app-side** notes — this workstream did
**not** edit the app):

- **Stale `/scan` bare-shape comments** in the app's `devices.dart` describe an
  older `/scan` response than the hub emits — refresh them against
  `scan_get.json` / `scan_post.json`.
- **Stale `8443` port example** in the app's `config.dart` — the hub's API
  default is `:9100`; `8443` is not part of this contract.
- **`/config` and `/metrics` are documented app routes with no app client** —
  `POST /config/{service}` (commissioning writes) and `/metrics` exist on the
  hub but nothing in the app calls them; either wire a client or drop them from
  the app's route docs.
