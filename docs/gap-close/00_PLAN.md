# lexa-hub ⇄ lexa-app gap-close campaign

*Started 2026-07-11. Closes the hub-side gaps in `lexa-app/docs/LEXA_HUB_GAPS.md`
so the companion app's provisioning + dashboard tracks work on real hardware.
Modeled on the extension campaign (`docs/extension/00_PROGRESS.md`): bite-sized
units, agents write, principal reviews every diff + runs suites + commits, tests
mirror the app's conformance artifacts.*

## Contract sources (authoritative — the hub must match these)

- **BLE protocol**: `lexa-app/docs/adr/ADR-0002-ble-commissioning-protocol.md`
  + the byte-for-byte conformance vectors
  `lexa-app/packages/lexa_core/test/ble/sec1_test_vectors.json` (copied into
  this repo as the H.2 fixture) + the reference `FakeHubPeripheral`
  (`lexa-app/.../ble/fake_peripheral.dart`) whose behavior the hub replicates.
- **HTTP API**: `lexa-app/docs/HUB_API.md` + the app's model classes
  (`lexa_core/lib/src/models/*`) + `lexa-app/tools/mockhub` (the contract
  mirror — kept in sync).

## Gap status (verified 2026-07-11)

| Gap | What | Verdict |
|---|---|---|
| GAP-4 | mode manager + intent consumers | **already CLOSED** by the extension campaign (units 3.3/3.4); `/mode` serves ModeStatus, intents produce real IntentResults. Verify on hardware, mark done. |
| GAP-5 | firmware version stamping (`fw=dev`) | small — lexa-api + Makefile. **Unit A1.** |
| GAP-8 | tariff + reserve read-back | medium — effective reserve is engine-unobservable (needs accessor), tariff spec not retained hub-side. **Unit A2.** |
| GAP-7 | plan/forecast 24 h series (`GET /plan`) | large — the 288-slot DailyPlan + forecast curve are engine-private on no bus topic; needs an accessor + a new hub→api publish. **Unit A3.** |
| GAP-1/2/3 | BLE commissioning (`cmd/provision`, H.1–H.4) | large, security-critical, but **hardware-closeable** (dev-kit BlueZ + NM verified live). **Units B1–B4.** |
| GAP-6 | remote access (Phase 3) | not a blocker; DRM-vs-cloudlink ADR later. Out of scope. |

## Dependency decision (flag)

- **B1 (sec1 crypto)**: stdlib only — `crypto/ecdh` (X25519), `crypto/hkdf`,
  `crypto/aes`+`crypto/cipher` (GCM). Verified present in Go 1.26. No dep.
- **B2/B3 (BlueZ GATT + NetworkManager)**: needs **`github.com/godbus/dbus/v5`**
  — a NEW module dependency (BSD-2, de-facto standard Go D-Bus). Unavoidable
  per ADR-0002 (BlueZ GATT over D-Bus); hand-rolling D-Bus is not reasonable.
  Recorded as **ADR-0002 (hub-side)**; the only new supply-chain surface on an
  otherwise lean fleet. Added at B2, vendored via `GOWORK=off go mod vendor`.

## Units

### Phase A — HTTP API gaps (no hardware needed to develop)

- **A1 — GAP-5 build-version stamping** (S): a tiny `internal/buildinfo` package
  with a build-injected `var Version` (default `"dev"`); Makefile `-ldflags
  "-X lexa-hub/internal/buildinfo.Version=$(VERSION)"` on the api build (and the
  arm64 api line); lexa-api surfaces it in mDNS `fw=`, `/site.fw`, and a new
  `/status.fw`. Mirror in `tools/mockhub` `/site`. Accept: `/site.fw`/`/status.fw`/
  mDNS all report the injected version; `make build-arm64 VERSION=…` stamps it.
- **A2 — GAP-8 reserve + tariff read-back** (M, radioactive-adjacent): new
  `Engine.EffectiveReservePct()` accessor (the `ForecastSource()` atomic pattern,
  written where the reserve clamp lands in `buildPlannerParams`); the intent
  adopter retains the requested `TariffSpec` + source and the reserve source;
  hub publishes a retained `lexa/hub/settings` doc (`bus.HubSettings`
  `{reserve{effective_pct,floor_pct,source}, tariff{source,updated_at,spec}}`);
  cmd/api subscribes it and folds `reserve` + `tariff` objects into `/status`
  (shapes exactly per the app map — `spec` parses with the app's
  `TariffSpec.fromJson`). Mirror in mockhub `/status`. Accept: after a reserve
  intent, `/status.reserve.effective_pct` reflects the post-clamp value + source;
  after a tariff intent, `/status.tariff.spec` round-trips + `source` distinguishes
  manual vs csip-overridden.
- **A3 — GAP-7 `GET /plan` 24 h series** (L, RADIOACTIVE): capture the planner
  inputs (`SolarForecastKw`, price arrays) alongside the `DailyPlan` (currently
  discarded after `Plan()`); `Engine.DailyPlanSnapshot()` accessor returning the
  288 slots + the forecast curve; new `bus.HubSchedule` type; cmd/hub publishes
  retained `lexa/hub/schedule` on each replan; cmd/api subscribes + serves
  `GET /plan` in the app's target shape (`solar_forecast[{t,solar_W}]`,
  `battery_plan[{t,setpoint_W,soc_pct}]`, `ev_plan{station:[{t,power_W}]}`).
  Add mockhub `/plan`. Accept: `GET /plan` returns time-stamped battery/EV
  setpoints + the solar forecast the optimizer used; radioactive-zone rule
  (existing orchestrator tests green unmodified).

### Phase B — BLE commissioning service `cmd/provision` (ADR-0002)

- **B1 — sec1 session crypto + framing** (M, Opus, pure Go, no hardware):
  `internal/provision/sec1` — X25519 ECDH, `HKDF-SHA256(ikm=shared,
  salt=UTF8(pop), info="lexa-prov-v1", len=16)`, AES-128-GCM with the
  `dir‖0x000000‖counter(8B BE)` implicit nonce, per-direction counters,
  abort-on-any-auth-failure; the chunk framing (`flags: FIN|ENC`, seq, 8 KiB cap);
  the handshake state machine (hello/confirm/ok/err) + op message types.
  **Acceptance: reproduce `sec1_test_vectors.json` byte-for-byte** (the file is
  copied into `internal/provision/sec1/testdata/`), plus a port of the fake
  peripheral's abort/replay/downgrade scenarios. Gotcha: HKDF salt is the PoP
  string (the Dart side passes it as `nonce:`).
- **B2 — BlueZ GATT peripheral + advertising** (L, Opus, dev-kit BlueZ):
  `cmd/provision` — `godbus/dbus/v5` GATT server exporting the ADR-0002 service +
  6 characteristics (info plaintext read; session/wifi/config/status per the
  table), `LEAdvertisingManager1` advertising `LEXA-<serial6>` gated on the
  ABSENCE of `/etc/lexa/commissioned`; systemd unit (`Type=notify` where
  feasible, hardened). Wires B1's session onto the encrypted characteristics.
  Accept: on the dev kit, `bluetoothctl`/a Go central discovers the service and
  reads `info` while uncommissioned; the service is off-radio once committed.
- **B3 — NetworkManager join + wifi scan + status streaming** (M, Opus):
  wifi scan via NM D-Bus (deduped, RSSI-sorted, top 20); `join{ssid,psk}` →
  `AddAndActivateConnection`; stream `state{joining,joined,failed}` on `status`
  with the reason enum `{not_found,auth_failed,dhcp_timeout,timeout,internal}`.
  Accept: good creds → `joined{ip,port,...}`; bad → `failed{reason}` per the enum.
- **B4 — handoff + re-provision window + PoP + factory-reset** (M):
  handoff payload reads the api cert fingerprint (same SHA-256-of-leaf-DER as
  `cmd/api/tlscert.go`) + the token (`api_token_file`); re-provision window
  (`lexactl provision --window` + a physical-button hook stub); manufacturing
  PoP at `/etc/lexa/provision-pop` (devkit hardcode allowed, product must not);
  advertising throttle (3 PoP failures → 5 min); `scripts/factory-reset.sh`
  restores provisioning-ready state without touching `/etc/lexa/identity`.
  Accept: a Go test central completes QR→handshake→join→handoff→`done`; the
  handoff `api_cert_fp` matches `/status.api_cert_fp` on the same box.

### Phase C — hardware validation (dev kit)

- **C1**: deploy A1/A2/A3, verify the new API surface on the dev kit against the
  app model shapes (and mockhub parity).
- **C2**: deploy `cmd/provision`, prove the full BLE flow on the dev-kit radio
  with a Go central: advertise-when-uncommissioned, handshake (right/wrong PoP),
  scan, join (real nmcli profile against a test SSID), handoff, silent-when-
  commissioned, throttle. This is the H.1–H.4 hardware acceptance.
- **C3**: GAP-4 hardware re-confirm; mockhub/app-contract diff clean.

## Sequencing

- **Wave 1**: A1 (sonnet), A2 (opus), B1 (opus). + copy the sec1 vectors &
  mirror ADR-0002 into this repo.
- **Wave 2**: A3 (opus), B2 (opus).
- **Wave 3**: B3, B4.
- **Wave 4**: C1–C3 hardware validation.

## App-repo follow-ups (lexa-app team, out of scope here — noted for handoff)

The app already has models/placeholder cards waiting; wiring the new `/plan`
series and `/status.reserve|tariff` into the Flutter screens (solar/battery/ev
plan charts, reserve slider seed, tariff viewer authoritative source) is
app-side work. mockhub mirrors land here to keep the contract testable.

## Board

| Unit | Name | Size | Model | Status |
|---|---|---|---|---|
| A1 | build-version stamping | S | sonnet | todo |
| A2 | reserve + tariff readback | M | opus | todo |
| A3 | GET /plan series | L | opus | todo |
| B1 | sec1 crypto + framing | M | opus | todo |
| B2 | BlueZ GATT + advertising | L | opus | todo |
| B3 | NM join + scan + status | M | opus | todo |
| B4 | handoff + reprovision + PoP + factory-reset | M | opus | todo |
| C1–C3 | hardware validation | — | — | blocked on A/B |
