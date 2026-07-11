# configs/factory/

Factory/uncommissioned-state profiles for all seven lexa services (Unit
1.6, `docs/DEVICE_ROADMAP.md` §9). These are what `scripts/factory-reset.sh`
copies over `/etc/lexa/*.json` from a baked, read-only copy at
`/usr/share/lexa/factory/` (see that script's header for the exact steps,
and "Install path" below for what still needs to land in `meta-lexa`).

They are **not** used at runtime by any `cmd/*` binary directly — a service
still only ever reads `/etc/lexa/<name>.json`. This directory is the
*source* that file gets restored from.

## Uncommissioned semantics (DEVICE_ROADMAP.md §9)

| Service | Uncommissioned behavior |
|---|---|
| lexa-api | mDNS TXT `claimed=0`; `/config` writes allowed; `/status` reports `"commissioned": false` |
| lexa-modbus | scan requests honored (future unit); no devices configured ⇒ no polling (existing behavior) |
| lexa-northbound | ships with no server URL ⇒ cleanly idle in intent (existing fail-closed behavior; no code change — a config profile) — **see "Known gaps" below: this is not fully true against today's code** |
| lexa-hub | no devices/stations ⇒ engine idles safe (existing); heartbeat state `never` accepted by the (future) healthcheck |
| lexa-cloudlink | connects and publishes health/claim state to the quarantine namespace (cloud-side routing by claim status) — speculative; the service doesn't exist in this repo yet |

Marker: `/etc/lexa/commissioned` (empty file, created by the commissioning
wizard's final step; removed by `factory-reset.sh`). Its absence is what
`lexa-healthcheck` (a separate, not-yet-implemented unit, §8.1) is meant to
treat as "plan heartbeat `never` is fine" rather than a fault.

## Per-file notes

- **hub.json** — `devices: []`, `stations: []`. `tariff_zone: ""` — this
  disables the WS-8 zone-mismatch assertion (`cmd/hub/tariffzone.go`) with a
  WARN log, same as any hand-edited config missing the key. **Commissioning
  MUST set this** to the site's tariff zone once known (GAP-05 enforcement —
  the wizard writes it from the installer's answer, same flow that will
  eventually also set the CSIP server URL). Clean today: no known gap.

- **northbound.json** — `server: ""` is the one key that makes the walk
  loop have nothing to dial. Cert paths still point at the standard
  `/etc/lexa/certs/{ca,client,client-key}.pem` locations. **See "Known
  gaps" — this alone does not make the process start cleanly on a box with
  no certs present yet.**

- **modbus.json** — `devices: []`, and the `reconciler` key is **absent
  entirely** (not `{}`) — `cfg.ReconcilerMode(class)` treats a nil map the
  same as an absent key (`internal/../cmd/modbus/config.go`'s
  `ReconcilerMode`), and with zero battery/inverter-role devices configured,
  `loadConfig`'s "battery/solar reconciler must be active" gate never
  triggers (it only fires when a device of that role exists). Clean today.

- **ocpp.json** — `stations: []`, `cert_path`/`key_path`/`basic_auth_user`/
  `basic_auth_pass` all blank, **`bench: false`**. **This is intentionally
  the fail-closed target posture, not something loadable by today's
  binary** — see "Known gaps": `cmd/ocpp/config.go`'s WS-1 gate refuses
  *any* config with those four fields blank unless `bench: true` (or
  `OCPP_PROFILE=bench`), with no carve-out for zero stations. Do **not**
  "fix" this profile by setting `bench: true` — that would ship an open,
  unauthenticated `ws://` CSMS listener on a field-deployed, network-
  connected unit the moment it boots uncommissioned, which is a worse
  outcome than the service failing to start.

- **telemetry.json** — `devices: []`, `server: ""`. Same cert-file
  dependency as northbound.json (they point at the identical files); see
  "Known gaps".

- **api.json** — `listen_addr: ":9100"` (LAN-reachable, not loopback — the
  commissioning wizard/installer app needs to reach this over the LAN
  before the device has any other identity), `api_token_file:
  "/etc/lexa/api-secret"` (satisfies the WS-1 gate for a non-loopback bind
  — see "Known gaps" for what has to be true about that file for this to
  actually work), `bench: false`, `tls: true`, `mdns: true`, `serial_file:
  "/etc/lexa/identity/serial"`, `cert_dir: "/var/lib/lexa/api"`. `tls: true`
  is no longer distinctive to this factory profile — the repo's dev/bench
  example `configs/api.json` also ships `tls: true` now (bring-up fix,
  2026-07-11: the companion app is HTTPS-only/cert-pinned and cannot reach a
  plain-http hub, and `scripts/deploy-hub-pi.sh` was re-flipping every bench
  deploy back to plain HTTP via the old `tls: false` example). Both profiles
  agree on-value now; only `listen_addr`/`api_token_file`/`bench` still
  differ between them (LAN-reachable + pre-provisioned token here vs.
  loopback + no token in the bench example, per the bullets above and in
  `cmd/api/config.go`'s field docs).

- **cloudlink.json** — `enabled: false`, `endpoint: ""`. **Speculative**:
  `cmd/cloudlink` does not exist in this repo yet (it's TASK-085 in
  `docs/DEVICE_ROADMAP.md` §2). This file is shaped from that doc's §2.2
  example so `lexa-migrate`'s registry (which already includes
  `"cloudlink.json"`, ready for the day it starts shipping) and this
  factory bundle are consistent with each other now rather than needing a
  second coordinated change later. It has not been validated against any
  real `loadConfig` because none exists yet.

## Known gaps (found while building this profile — read before relying on it)

**1. RESOLVED (unit 1.7, 2026-07-09)** — option (a) below was implemented:
`cfg.Uncommissioned()` (`server == ""`) gates `main()` in both services onto
an idle path (`runIdle`) that skips wolfSSL init, fetcher construction, LFDI
derivation, and (northbound) certmon/rotation entirely while keeping MQTT,
/metrics, and watchdog Ready+kicks alive — a factory-fresh or freshly-reset
unit now idles cleanly instead of crash-looping into StartLimit. The
original analysis is kept below for provenance.

**1. `lexa-northbound` and `lexa-telemetry` both load their CA/client
cert/key files eagerly, unconditionally, at process construction** —
`cmd/northbound/main.go`'s `mustFetcher`/`tlsclient.NewWolfSSLFetcher` and
`cmd/telemetry/main.go`'s identical pattern both call `wolfssl.
LoadVerifyLocations`/`UseCertFile`/`UseKeyFile` on the configured
`ca_cert`/`client_cert`/`client_key` paths before either service ever looks
at `server`. An empty `server` does **not** by itself make these processes
"cleanly idle" — if the cert files at those paths don't exist (a genuinely
virgin device, or right after `factory-reset.sh` wipes `/etc/lexa/certs/*`
per its spec), `wolfSSL_CTX_use_certificate_file`/friends fail and
`main()` calls `log.Fatalf`. Under `Restart=on-failure` +
`StartLimitBurst=5` (both units), this is the exact V1RC FINDING A failure
signature: five failed starts in the window and the unit lands
permanently `failed`, needing `systemctl reset-failed` before it can run
again — even though, in intent, "no server configured" was supposed to mean
"idle," not "fails to start at all."

This means, concretely: a factory-reset run's final "restart the seven
services" step will very likely leave `lexa-northbound` and
`lexa-telemetry` in `failed` state until commissioning provisions real
certs into `/etc/lexa/certs/` **and** something (the commissioning flow,
or an operator) runs `systemctl reset-failed lexa-northbound
lexa-telemetry` afterward.

This was **not** fixed as part of this unit: fixing it means either (a) a
scoped code change in `cmd/northbound/main.go` and `cmd/telemetry/main.go`
to skip wolfSSL fetcher construction when `server == ""` (out of this
unit's file ownership), or (b) `meta-lexa` baking placeholder/dummy
self-signed cert files into the factory image at those exact paths so the
processes start and simply fail to dial (harmless — same shape as any
other `wan-outage`-style condition) instead of failing to construct their
TLS context at all. Recommend whoever picks up TASK-098
(`lexa-healthcheck`) or the commissioning flow (TASK-090) resolve this
explicitly — the healthcheck's own spec (§8.1 item 4, "config has no
server ⇒ cleanly idle") already assumes the fix is in place.

**2. RESOLVED (unit 6.1, 2026-07-09)** — lexa-ocpp now treats `stations: []` + `bench:false` as uncommissioned-idle (config loads, CSMS listener never binds; `cmd/ocpp/config.go`'s `uncommissionedIdle` + narrowed WS-1 gate). This profile is loadable as written. Original note kept below:

**(superseded) 2. `lexa-ocpp`'s factory profile is written in TARGET state, not
LOADABLE state, on purpose.** `cmd/ocpp/config.go`'s WS-1 gate refuses to
start with `cert_path`/`key_path`/`basic_auth_user`/`basic_auth_pass` all
blank unless `bench: true`. This profile ships `bench: false` (correctly —
setting it `true` would ship an open ws:// CSMS on a real network). Per
direction from the coordinating unit: a companion change (Unit 6.1) is
adding an "uncommissioned idle" gate to `lexa-ocpp` — no stations
configured **and** not bench ⇒ skip binding the CSMS listener entirely
(idle) instead of refusing to start. **Until Unit 6.1 lands, deploying
this exact factory profile makes `lexa-ocpp` fail to start** (loudly,
`log.Fatalf` from `loadConfig`'s error, not a silent hang) — this is a
known, deliberate, tracked gap, not an oversight in this file.

**3. `lexa-api`'s `api_token_file: "/etc/lexa/api-secret"` must actually
exist for the process to start.** `cfg.LoadAPIToken()` treats a configured-
but-unreadable-or-empty token file as a fatal startup error (fail loud, by
design — see that function's doc comment). This factory profile assumes
`/etc/lexa/api-secret` is **provisioned at manufacturing** (a per-device
random token written once at image/first-boot time — not part of this
factory JSON bundle, which is identical across every unit and therefore
cannot itself be the source of a per-device secret). If that provisioning
step doesn't exist yet wherever this profile gets deployed, `lexa-api`
will fail to start the same way northbound/telemetry do in gap 1.

**4. Factory reset does not rotate `/etc/lexa/api-secret`.** Worth flagging
even though it's outside this unit's explicit spec: if a device is
factory-reset for resale/relocation, the OLD owner's still-known bearer
token continues to work against the new owner's device unless something
regenerates that file. `scripts/factory-reset.sh` deliberately does not
touch it (it isn't identity, and it isn't listed in DEVICE_ROADMAP.md §9's
wipe list) — flagged here as an open question for whoever owns the
resale/relocation story, not solved by this script.

## Install path (for `meta-lexa`)

`scripts/factory-reset.sh` reads from `/usr/share/lexa/factory/*.json` on
the target device — this repo's `configs/factory/*.json` needs to be
staged there by the Yocto recipe (a straight file copy, same idea as this
repo's own `make install-configs` for `/etc/lexa`). That recipe is also the
natural place to solve gaps 1 and 3 above (placeholder certs / a
first-boot-generated `api-secret`) if the code-side fix isn't taken
instead — not done here since it's a `meta-lexa`-side change, outside this
repo.
