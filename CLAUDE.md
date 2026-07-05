# LEXA Hub

DERMS hub for IEEE 2030.5 / CSIP compliance on a Digi SOM (ARM64 embedded Linux).
Bridges utility grid management (northbound, wolfSSL mTLS) to DER assets — solar PV,
battery storage, smart meter, EVSE (OCPP 2.0.1) — via an MQTT message bus.

**This is the product.** Its test bench (grid/device/EV simulators, conformance suites,
dashboard) lives in `~/projects/csip-tls-test`. Two packages are duplicated across the
repos and must change in lockstep: `internal/southbound/sunspec` register maps (audit
MTR-4 — a lone change misreads real hardware) and `internal/ocppserver`. Enforced by
`scripts/ci/lockstep-check.sh` in csip-tls-test CI (TASK-004) — report-only until Phase 1
replaces the duplication with a shared module (AD-003/TASK-024).

## Architecture: separate systemd services

Each concern runs as its own process and communicates only via Mosquitto MQTT:

```
[mosquitto]          — MQTT broker (localhost:1883)
    │
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
  northbound/ IEEE 2030.5 model, discovery walker, scheduler, identity, DNS-SD
  tlsclient/  wolfSSL mTLS client (keep-alive fetcher)
  wolfssl/    CGo wrapper for wolfSSL_Init (call exactly once per process)
  southbound/ Modbus/SunSpec: device interface, inverter, battery, meter, registry
  ocppserver/ OCPP 2.0.1 CSMS (pure Go, no wolfSSL)
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

Go 1.21 · wolfSSL CGo (northbound + telemetry only) · eclipse/paho.mqtt.golang ·
lorenzodonini/ocpp-go · simonvetter/modbus · grandcat/zeroconf
