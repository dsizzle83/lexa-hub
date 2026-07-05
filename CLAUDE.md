# LEXA Hub

DERMS hub for IEEE 2030.5 / CSIP compliance on a Digi SOM (ARM64 embedded Linux).
Bridges utility grid management (northbound, wolfSSL mTLS) to DER assets — solar PV,
battery storage, smart meter, EVSE (OCPP 2.0.1) — via an MQTT message bus.

**This is the product.** Its test bench (grid/device/EV simulators, conformance suites,
dashboard) lives in `~/projects/csip-tls-test`. Shared protocol code (`sunspec`,
`derbase`, `modbus`, `ocppserver`, `csipmodel` — this repo used to duplicate `sunspec`
and `ocppserver` in-tree; audit MTR-4) now lives in the `lexa-proto` module, imported by
both this repo and csip-tls-test via a pinned commit SHA (`proto.pin` at each repo's
root — `lexa-proto` has no hosted remote yet, AD-003(c); a committed
`vendor/lexa-proto/` tree, AD-003(e), lets both repos build without fetching it).
**Both repos must pin the identical `lexa-proto` commit — CI enforces it**
(`scripts/check-proto-pin.sh`, invoked from csip-tls-test where it lives; TASK-024,
replacing TASK-004's retired raw-diff `lockstep-check.sh`). Version bumps ship as
paired PRs (both `proto.pin` files + both `vendor/lexa-proto/` regenerated in the same
session) and deploy hub + sims together — the code half of MTR-4 lockstep is now
CI-gated; the deploy half remains an operational discipline
(`../csip-tls-test/docs/BENCH.md`). A local `go.work` (`go work init . ../lexa-proto`,
gitignored, never committed) is still the normal way to develop against a live
`lexa-proto` checkout.

## Architecture: separate systemd services

Each concern runs as its own process and communicates only via Mosquitto MQTT:

```
[mosquitto]          — MQTT broker (localhost:1883); per-service creds + topic ACL,
    │                   staged rollout — see "Broker security" below (TASK-013, AD-008)
    ├─ lexa-modbus   — polls SunSpec/Modbus devices; applies control commands
    ├─ lexa-northbound — IEEE 2030.5 discovery walker; publishes active DER control
    ├─ lexa-telemetry— subscribes to measurements; POSTs MUP readings to northbound server
    ├─ lexa-ocpp     — OCPP 2.0.1 CSMS for EV chargers
    ├─ lexa-hub      — energy optimizer engine (the "brain")
    └─ lexa-api      — HTTP /status + /logs on :9100 (legacy dashboard adapter);
                        bearer-token auth (`api_token_file`), staged rollout —
                        empty = open (TASK-014, AD-008); /healthz always open
```

## MQTT topic map

| Topic | Publisher | Subscribers | QoS |
|---|---|---|---|
| `lexa/measurements/{device}` | lexa-modbus | lexa-hub, lexa-telemetry | 0 |
| `lexa/battery/{device}/metrics` | lexa-modbus | lexa-hub | 0 |
| `lexa/csip/control` *(retained)* | lexa-northbound | lexa-hub, lexa-telemetry | 1 |
| `lexa/evse/{station}/state` | lexa-ocpp | lexa-hub | 0 |
| `lexa/control/battery/{device}` | lexa-hub | lexa-modbus | 1 |
| `lexa/control/solar/{device}` | lexa-hub | lexa-modbus | 1 |
| `lexa/evse/{station}/command` | lexa-hub | lexa-ocpp | 1 |

QoS is enforced by `bus.PubQoS` (`internal/bus/topics.go`), not hardcoded per
call site — publishers pass `bus.PubQoS(topic)` to
`mqttutil.PublishJSONQoS`. Previously every publish hardcoded QoS 1
regardless of this table (review D5); closed 2026-07-04.

Every bus message carries `"v"` (schema version): each top-level published
type embeds `bus.Envelope` and every publish site stamps its per-schema
constant (`internal/bus/envelope.go`); every subscriber version-checks before
decode (`mqttutil.Subscribe`'s gate, plus the one raw `mc.Subscribe` in
cmd/northbound for the FR-request topic). Absent `"v"` is legacy v0, accepted
during the transition (`bus.LegacyV0Accepted = true`, AD-006) — this is
deliberate: rejecting it would refuse a retained pre-envelope message at
boot. `Measurement`'s voltage field is `VoltageV`/`"voltage_v"`, not
`V`/`"v"` — that key is the schema version now; see the doc comment on
`Measurement` in `internal/bus/messages.go` for why. TASK-018 (2026-07-04)
rolled this out everywhere; the v0-tolerance flip to reject-only is a
separate, later change (AD-006 enforcement criteria).
## Broker security

Anonymous MQTT access is dead (TASK-013 / W7 / AD-008): the assumption that
"localhost-only ⇒ anonymous is fine" broke the day a third-party process
(csip-tls-test's `cmd/mqttproxy`, deployed by `scripts/mqtt-chaos.sh` for QA
fault injection) started running on the hub Pi — any local process can
otherwise command hardware. Each of the six lexa services plus the QA
`qa-inject` user (mqttproxy's `/inject`) now authenticates with its own
broker user, and `systemd/mosquitto-lexa.acl` grants each user only the
topics `internal/bus/topics.go` says it publishes/subscribes (re-derive the
matrix from actual `Subscribe`/`Publish` call sites before changing it — it's
an authorization boundary, not a topic map).

- **Credentials**: per-service passwords live under `/etc/lexa/mqtt/<svc>.pass`
  (0600, `lexa:lexa`, generated on-device by `scripts/deploy-hub-pi.sh` via
  `openssl rand -hex 16` — never committed to git or a deploy artifact). Each
  service's config carries `mqtt_user`/`mqtt_pass_file`; empty ⇒ anonymous
  connect (`mqttutil.Connect`/`ConnectAuth("", "", "", "")`).
- **Broker enforcement**: `password_file`/`acl_file` in
  `systemd/mosquitto-lexa.conf` (repo target state: `allow_anonymous false`).
  The deployed Pi conf.d drop-in (`deploy-hub-pi.sh`'s heredoc) stages this
  behind `--enable-mqtt-acl`: every deploy always generates credentials and
  patches them into every service's config, but `allow_anonymous` only flips
  to `false` — and `password_file`/`acl_file` only get installed — when that
  flag is passed. This lets a plain deploy leave every service holding valid
  credentials against a broker that still accepts anonymous connections,
  so credential rollout and ACL enforcement are separate, verifiable steps
  (journal evidence: `[mqtt] connected to ... (broker user=...)`).
- **qa-inject**: bench-only broker user for `cmd/mqttproxy`'s `/inject`
  endpoint (a hand-rolled MQTT CONNECT, `-user`/`-passfile` flags,
  `sim/mqttproxy.service`'s ExecStart); provisioned by `mqtt-chaos.sh deploy`
  into the same `/etc/mosquitto/lexa-passwd`. It's granted `lexa/#`
  read+write in the ACL — QA legitimately needs to forge any topic — never
  provisioned off-bench. mqttproxy's transparent PASSTHROUGH path needs no
  credentials of its own: proxied lexa services present their own end-to-end.
- Localhost-only listener (`listener 1883 localhost`) is unchanged — the ACL
  is defense-in-depth behind it, not a LAN opening.

## Directory map

```
cmd/
  hub/        Orchestrator (optimizer engine) — no Modbus/wolfSSL dependency
  northbound/ IEEE 2030.5 northbound client (wolfSSL CGo)
  modbus/     SunSpec/Modbus device poller + control applicator
  ocpp/       OCPP 2.0.1 CSMS for EV chargers
  telemetry/  MUP telemetry poster (wolfSSL CGo)
  api/        HTTP API server — subscribes to MQTT, serves /status + /logs SSE

internal/
  bus/        MQTT topic constants + JSON message types (shared by all services)
  mqttutil/   Thin MQTT client helpers (connect, PublishJSON, Subscribe[T])
  northbound/ IEEE 2030.5 discovery walker, scheduler, identity, DNS-SD
              (model types moved to lexa-proto/csipmodel — TASK-023)
  tlsclient/  wolfSSL mTLS client (keep-alive fetcher)
  wolfssl/    CGo wrapper for wolfSSL_Init (call exactly once per process)
  southbound/ Modbus/SunSpec: device interface, inverter, battery, meter, registry
  orchestrator/ Rule-based optimizer engine (no I/O — reads SystemState, returns Plan)

systemd/     Unit files + mosquitto config fragment
configs/     Example JSON configs for each service
```

## Build

```bash
go mod tidy                  # fetch dependencies (first time)
make build                   # builds all 6 binaries into bin/
make test                    # go test -race ./internal/...
make wolfssl-arm64           # rebuild arm64 wolfSSL sysroot (/tmp — wiped on reboot!)
make build-arm64             # cross-compile all 6 binaries → bin/arm64/
```

CI: `.github/workflows/ci.yml` — vet + `-race` (pure-Go) + cgo build on every PR; required checks on `main`.

## Current bench deployment

The ConnectCore dev kit (69.0.0.2) is offline; the hub runs on **hub-pi `dhpi4` at
69.0.0.1** (root systemd units, passwordless sudo, SSH `dmitri@`). Deploy with
`scripts/deploy-hub-pi.sh 69.0.0.1 dmitri` (needs `make build-arm64` first; stages client
certs from `../csip-tls-test/certs/client-staging/`). Full topology:
`../csip-tls-test/docs/BENCH.md`. Dev-kit runbook for when it returns: `DEVKIT.md`.

## Install on the Digi SOM

```bash
# On target device as root:
make install                 # copies binaries to /usr/local/sbin/
make install-configs         # copies example JSON to /etc/lexa/ (no overwrite)
make install-services        # installs & enables all systemd units + mosquitto fragment
make start                   # starts all services

# Operational commands:
make status                  # show service status
make logs                    # tail all lexa journals together
make stop
```

## Config files (on device)

All configs live in `/etc/lexa/`. Edit the copies created by `make install-configs`.

| File | Service |
|---|---|
| `/etc/lexa/hub.json` | lexa-hub |
| `/etc/lexa/northbound.json` | lexa-northbound |
| `/etc/lexa/modbus.json` | lexa-modbus |
| `/etc/lexa/ocpp.json` | lexa-ocpp |
| `/etc/lexa/telemetry.json` | lexa-telemetry |
| `/etc/lexa/api.json` | lexa-api |
| `/etc/lexa/certs/` | ca.pem, client.pem, ocpp.crt etc. |

## Critical invariants

- **wolfSSL_Init**: process-global C state. `wolfssl.Init()` is called once in
  `main()` of lexa-northbound and lexa-telemetry only. The other three services are
  pure Go and never touch wolfSSL.
- **Cipher**: `ECDHE-ECDSA-AES128-CCM-8 TLSv1.2` only (CSIP §5.2.1.1).
- **Bus messages**: `math.NaN()` never appears in JSON — use `*float64` (nil = absent).
- **CSIP control is retained**: lexa-northbound publishes `lexa/csip/control` with
  retain=true so lexa-hub gets the last value immediately on (re)start.
- **Module path**: `lexa-hub` — used in all import paths.
- **Cross-compile**: lexa-northbound and lexa-telemetry require CGo (wolfSSL headers
  are ARM64-only on the SOM). Build on target or with a proper cross toolchain.
  The other three services are `CGO_ENABLED=0` cross-compilable.
- **All six services are `Type=notify`** (TASK-007 on lexa-hub; TASK-008 on the
  remaining five), each kicked from its own natural liveness point via the shared
  `internal/watchdog` package (`Ready`/`Kick` over `NOTIFY_SOCKET`; no-ops when unset,
  i.e. dev/test). `WatchdogSec` per service:

  | Service | WatchdogSec | Kick site |
  |---|---|---|
  | lexa-hub | 60 | `Engine.PlanObserver`, every economic tick + safety pass (`cmd/hub/main.go`) |
  | lexa-northbound | 120 | walk-loop `for` body top + once after the initial walk (`cmd/northbound/main.go`) — sized ≥4x a legitimate long walk under `northbound-hang` conditions |
  | lexa-modbus | 60 | first statement of the update-drain body in `publishMeasurements` (`cmd/modbus/main.go`) |
  | lexa-telemetry | 60 | 10s ticker case in the same `select` as the MUP post loop (`cmd/telemetry/main.go`) |
  | lexa-ocpp | 60 | 10s ticker gated on `mc.IsConnected()` (`cmd/ocpp/main.go`) — process+MQTT only, OCPP listener health not probed |
  | lexa-api | 60 | 10s ticker gated on `mc.IsConnected()` AND a loopback `GET /healthz` 200 (`cmd/api/main.go`) |

  lexa-hub/northbound/modbus ride a real control/poll loop — a wedge there starves the
  kick directly. telemetry/ocpp/api have no tight loop; their kick tickers are either
  in the same select as the thing that matters (telemetry) or actively probe it
  (ocpp/api), which is weaker and documented as such in each unit file. A sustained
  MQTT broker outage restarts ocpp/api (accepted crash-only behavior, AD-011) but must
  NOT restart northbound/modbus, whose loops keep iterating fail-closed on fetch/poll
  errors (verify via the `northbound-hang`/`wan-outage-*` Mayhem scenarios before
  touching any of this).
- **journald caps** (TASK-009, review §11 flash wear / RSK-14): every `lexa-*.service`
  sets `LogRateLimitIntervalSec`/`LogRateLimitBurst`, and `systemd/journald-lexa.conf`
  (installed by `deploy-hub-pi.sh` to `/etc/systemd/journald.conf.d/lexa.conf`) caps the
  Pi's total journal at `SystemMaxUse=200M`. Rate/size math, per-service estimates, and
  the wear budget live in `docs/FLASH_BUDGET.md` — read it before changing per-tick
  logging or raising any cap.

## Defensive fault-handling (do not strip — each backs a mayhem-QA finding)

Hostile-QA (`csip-tls-test` mayhem suite) surfaced fault classes the optimizer must
survive; the guards below are deliberate, not redundant. Keep them when refactoring.

- **Trust measurement, not the command** (closed-loop convergence, `internal/orchestrator`):
  a device can ACK a write and ignore it. `checkGenLimitConvergence` (cross-checked against an
  independent meter floor `gen ≥ export − batteryDischarge`), `checkImportConvergence`, and the
  export rule's battery-absorption guard all compare commanded vs MEASURED effect and, on a
  sustained gap, curtail another lever or post a 2030.5 CannotComply.
- **Fail closed on bad CSIP**: the scheduler holds last-known-good on an empty/malformed
  resource (see `internal/northbound/CLAUDE.md` rule 6).
- **Plausibility-gate ingested telemetry**: `cmd/modbus` withholds power over nameplate
  (suspect scale factor); `cmd/ocpp` rejects MeterValues current over the station rating.
- **Battery reserve safety**: `checkBatterySafety` force-disconnects a pack measured
  discharging at/below its SOC reserve (a device inverting its setpoint).
- These hold the cap / surface the fault; the matching regression tests are
  `*_test.go` next to each (`convergence_test.go`, `failclosed_test.go`, etc.).

## Stack

Go 1.26 · wolfSSL CGo (northbound + telemetry only) · eclipse/paho.mqtt.golang v1.5.1 ·
lorenzodonini/ocpp-go · simonvetter/modbus · grandcat/zeroconf
