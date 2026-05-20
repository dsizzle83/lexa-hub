# LEXA Hub

DERMS hub for IEEE 2030.5 / CSIP compliance on a Digi SOM (ARM64 embedded Linux).
Bridges utility grid management (northbound, wolfSSL mTLS) to DER assets — solar PV,
battery storage, smart meter, EVSE (OCPP 2.0.1) — via an MQTT message bus.

## Architecture: separate systemd services

Each concern runs as its own process and communicates only via Mosquitto MQTT:

```
[mosquitto]          — MQTT broker (localhost:1883)
    │
    ├─ lexa-modbus   — polls SunSpec/Modbus devices; applies control commands
    ├─ lexa-csip     — IEEE 2030.5 discovery walker; publishes active DER control
    ├─ lexa-telemetry— subscribes to measurements; POSTs MUP readings to CSIP server
    ├─ lexa-ocpp     — OCPP 2.0.1 CSMS for EV chargers
    └─ lexa-hub      — energy optimizer engine (the "brain")
```

## MQTT topic map

| Topic | Publisher | Subscribers | QoS |
|---|---|---|---|
| `lexa/measurements/{device}` | lexa-modbus | lexa-hub, lexa-telemetry | 0 |
| `lexa/battery/{device}/metrics` | lexa-modbus | lexa-hub | 0 |
| `lexa/csip/control` *(retained)* | lexa-csip | lexa-hub, lexa-telemetry | 1 |
| `lexa/evse/{station}/state` | lexa-ocpp | lexa-hub | 0 |
| `lexa/control/battery/{device}` | lexa-hub | lexa-modbus | 1 |
| `lexa/control/solar/{device}` | lexa-hub | lexa-modbus | 1 |
| `lexa/evse/{station}/command` | lexa-hub | lexa-ocpp | 1 |

## Directory map

```
cmd/
  hub/        Orchestrator (optimizer engine) — no Modbus/wolfSSL dependency
  csip/       CSIP northbound client (wolfSSL CGo)
  modbus/     SunSpec/Modbus device poller + control applicator
  ocpp/       OCPP 2.0.1 CSMS for EV chargers
  telemetry/  MUP telemetry poster (wolfSSL CGo)

internal/
  bus/        MQTT topic constants + JSON message types (shared by all services)
  mqttutil/   Thin MQTT client helpers (connect, PublishJSON, Subscribe[T])
  csip/       IEEE 2030.5 model, discovery walker, scheduler, identity, DNS-SD
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
make build                   # builds all 5 binaries into bin/
make test                    # runs internal package unit tests
```

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
| `/etc/lexa/csip.json` | lexa-csip |
| `/etc/lexa/modbus.json` | lexa-modbus |
| `/etc/lexa/ocpp.json` | lexa-ocpp |
| `/etc/lexa/telemetry.json` | lexa-telemetry |
| `/etc/lexa/certs/` | ca.pem, client.pem, ocpp.crt etc. |

## Critical invariants

- **wolfSSL_Init**: process-global C state. `wolfssl.Init()` is called once in
  `main()` of lexa-csip and lexa-telemetry only. The other three services are
  pure Go and never touch wolfSSL.
- **Cipher**: `ECDHE-ECDSA-AES128-CCM-8 TLSv1.2` only (CSIP §5.2.1.1).
- **Bus messages**: `math.NaN()` never appears in JSON — use `*float64` (nil = absent).
- **CSIP control is retained**: lexa-csip publishes `lexa/csip/control` with
  retain=true so lexa-hub gets the last value immediately on (re)start.
- **Module path**: `lexa-hub` — used in all import paths.
- **Cross-compile**: lexa-csip and lexa-telemetry require CGo (wolfSSL headers
  are ARM64-only on the SOM). Build on target or with a proper cross toolchain.
  The other three services are `CGO_ENABLED=0` cross-compilable.

## Stack

Go 1.21 · wolfSSL CGo (csip + telemetry only) · eclipse/paho.mqtt.golang ·
lorenzodonini/ocpp-go · simonvetter/modbus · grandcat/zeroconf
