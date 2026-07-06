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

Every service also exposes Prometheus `/metrics` (TASK-044) — see "Metrics" below.

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
| `lexa/reconcile/{class}/{device}/report` *(retained)* | lexa-modbus, lexa-ocpp | lexa-hub | 1 |
| `lexa/csip/compliance/alert` | lexa-hub | lexa-northbound | 1 |

**CannotComply path (3 stages, TASK-031)** — collapsed from the old 5-hop chain:
(1) *evidence* — the optimizer's meter-level `Plan.Breach` AND the reconcilers'
device-level non-convergence (`ReconcileReport` NonConvergedBegin/End, retained
per device so the hub re-seeds state after a restart) — feeds (2) the named
`breachEpisodes` component (`cmd/hub/breach.go`), the single owner of episode
state, which merges both sources under one episode ID and emits ONE
edge-triggered `ComplianceAlert` (non-retained — an edge, not state) per episode
onset/clear, consumed by (3) northbound's `responseTracker`, which POSTs exactly
one 2030.5 CannotComply per episode (dedupes on episode ID, falling back to
MRID). The `activeBreachMRID` closure is gone; episode state is a named,
snapshot-ready struct (TASK-041).

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

## Metrics (TASK-044)

Every service serves Prometheus text-exposition `/metrics`: `lexa-hub :9101 ·
lexa-northbound :9102 · lexa-modbus :9103 · lexa-ocpp :9104 · lexa-telemetry
:9105 · lexa-api :9100/metrics` (existing listener, new route — unauthenticated,
same as `/healthz`; AD-008's bearer-token rollout only covers `/status`/`/logs`).
Config key `metrics_addr` per service JSON: empty ⇒ the port above bound to
`127.0.0.1` (product default — no new externally-reachable surface); the
literal `"off"` disables the listener. The bench's deployed configs override
this to `0.0.0.0` so the desktop can scrape it (AD-008's bench-vs-product bind
pattern; scrape config + rationale in `../csip-tls-test/scripts/prometheus-bench.yml`
and `docs/BENCH.md`'s Metrics section).

**Library**: `internal/metrics` is a minimal hand-rolled Prometheus text-exposition
package (Registry/Counter/Gauge/Handler/Collect), not `prometheus/client_golang` —
see the package doc for the dependency-posture rationale (same reasoning as
`internal/watchdog` hand-rolling sd_notify and `cmd/mqttproxy` hand-rolling MQTT
3.1.1 in the bench repo). `mqttutil.ConnectAuthInstrumented` wires an optional
`Instrumentation{OnPublishFail, OnReconnect, OnConnectionLost}` — function values,
not a metrics import, so `internal/mqttutil` stays decoupled from whichever
metrics implementation a caller wires in.

A registered-but-zero counter is normal and expected for sources not yet wired
(e.g. `lexa_hub_tick_overruns_total`, real source lands in TASK-046).

## Logging & the plan heartbeat (TASK-045)

All six services install a `log/slog` default (`internal/logutil.Setup`, first
line of `main()`): a text `key=value` handler on stderr — journald timestamps
every line itself, so slog's own `time=` key is not a duplicate. Config key
`log_level` per service JSON (`"debug"|"info"|"warn"|"error"`, default
`"info"`); an empty/unrecognized value fails soft to `info`
(`internal/logutil.ParseLevel`). Adoption is pragmatic, not a full sweep:
only structured-value transition sites (staleness/frozen-meter/control-expiry
edges, compliance-breach begin/clear, reconnect/reassert, decode-reject
alarms, discovery fail-closed/response-posted) were migrated; most
`log.Printf` call sites are unchanged (slog does not touch the standard `log`
package's output) — see TASK-045's task file for the full migration list and
the per-tick demotion table (lines that used to log every cycle at Info now
log at Debug or only on a state edge: lexa-northbound's per-walk "discovery
OK", lexa-ocpp's bare MeterValues and TransactionEvent Updated, lexa-telemetry's
per-post line).

**Plan heartbeat**: lexa-api now ACTS on the retained `lexa/hub/plan` topic
instead of only relaying it. `cmd/api/heartbeat.go`'s `planHeartbeat` tracks
the ARRIVAL time (not the plan's own `Ts` — a hub with a warped/stepped clock,
e.g. under csip-tls-test's Bench Replay clock warp, must not corrupt stall
detection) of the last PlanLog and reports one of three states, evaluated
fresh on every `/status` request and on a 5 s ticker that also drives the
edge-triggered alarm + metrics:

- `never` — no PlanLog seen since this process started. **Never alarms**
  (a bench bring-up race, api before hub, or a hub that has never run, must
  not page) — this is the INCONCLUSIVE-safe state review §11 asked for.
- `ok` — a PlanLog arrived within `plan_stall_after_s` (config key, default
  75 — safe at both the STOCK 15 s `engine_interval_s` and the FAST bench
  cadence; the hub also publishes on every safety tick, so real advancement
  is normally far faster than this bound).
- `stalled` — more than `plan_stall_after_s` since the last arrival: a wedged
  control loop (`internal/bus/topics.go`'s `TopicHubPlan` doc).

`/status` gains `"plan_heartbeat": {"state": "...", "age_s": N}` (additive
JSON); metrics `lexa_api_plan_heartbeat_stalled` (0/1 gauge) and
`lexa_api_plan_heartbeat_age_seconds`. `/status`'s existing fields
(`last_plan` and its relay semantics) are unchanged — `cmd/api/plan_test.go`
still pins that.

**Crash-only (AD-011)**: this repo intentionally has no blanket `recover()` —
a wedged or panicking service is meant to die and let systemd restart it (5 s),
with retained MQTT topics re-seeding state. Operator-facing detail (what dies
with a service, what comes back, what to check after a restart):
`docs/OPERATIONS.md`.

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
  metrics/    Prometheus text-exposition (Registry/Counter/Gauge/Handler) — leaf
              package, no lexa-hub imports (TASK-044)
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

`modbus.json`'s `"reconciler"` key (TASK-027/028, AD-002/AD-013) maps device
class → `"off"|"shadow"|"active"`, e.g. `{"battery":"shadow"}`. `"shadow"` runs
the Device Reconciler (`internal/reconcile`) as a passive recorder alongside the
legacy control path — zero hardware writes, logs `reconciler[shadow] ...`
verdict lines and `lexa_mb_shadow_*_total` metrics.

`"active"` gives the reconciler **write authority**. In `lexa-modbus` it is
legal for **`battery` (TASK-028)** and **`solar` (TASK-029)**; `evse` active is a
fatal config error in `modbus.json` because the EVSE reconciler lives in
`lexa-ocpp` (its own scalar `"reconciler"` key in `ocpp.json`, TASK-030). In
battery-active mode the reconciler owns hardware writes:
desired-doc → `reconcile.SetDesired` → `battCommandToControl` (the single
sign-mapping owner, reused) → `registry.ApplyControlTo` — the same registry path
legacy used; a diverged readback drives a corrective write (`reconciler[active]
… applied …` journal lines, `lexa_mb_reconcile_writes_total`), and a reconnected
pack reasserts the standing desired (`reconcile.Reconnected`, ledger L4). The
reconciler is the **single reasserter**: `retryDevice.lastCtrl` recording/replay
is suppressed for an active-reconciled battery (solar keeps it). The legacy
`lexa/control/battery/{device}` topic **keeps publishing and being subscribed**
(belt and braces for instant rollback) but is ignored on hardware when active
(`legacy battery command ignored (reconciler active)`); rollback = set `shadow`
and restart lexa-modbus, legacy writes resume immediately.

**Interlock seniority (critical):** the Tier-0 battery interlock
(`cmd/modbus/interlock.go`, ledger L8) stays **senior to the reconciler**. While
it has a pack force-disconnected (`isTripped`), the reconciler **suppresses
connect-restoring writes** (reports `InterlockHold`) rather than rewriting
`Conn=1` against the Tier-0 disconnect — the exact guard-versus-guard
oscillation the program exists to kill. The interlock re-evaluates and clears
its trip each poll once the fault clears; normal reconciliation resumes then.
The interlock's charge intent is fed from the desired doc the reconciler
executes (moved off the legacy subscribe path).

**Solar-active (TASK-029):** the inverter reconciler owns `OpModMaxLimW` writes
via the same `registry.ApplyControlTo` path, driven by explicit-ceiling desired
docs (`lexa/desired/solar/{device}`). Two solar-specific rules: (1) divergence is
**one-sided** — an inverter producing *under* its ceiling (dusk, clouds) is
compliant; only *over*-ceiling generation writes; (2) **restore is an explicit
write**, not an absence — a released cap publishes `CeilingW = RestoreCeilingW`
(clamped to WMax → 100%), reproducing `restoreOnGenLimitClear`. There is no
Tier-0 interlock for solar. The inverter reconciler is the **single reasserter**:
`reassertLocked`'s inverter branch is suppressed for an active inverter and
replaced by the shell's Reconnected() reassert plus an initial-desired **seed**
(restore ceiling) for the never-commanded case (the seed's startup write is
dropped — reassertLocked fires on reconnect, not startup). Legacy
`lexa/control/solar/{device}` keeps flowing but is ignored on hardware
(`legacy solar command ignored (reconciler active)`).

**EVSE reconciler (`ocpp.json` `"reconciler": "off"|"shadow"|"active"`,
TASK-030):** lives in `lexa-ocpp`, one shell per station. Active mode owns
`SetChargingProfile` via the SAME driver the legacy path uses (`bridge.Apply`,
L11 rejected-as-error reused verbatim); convergence is judged **only from metered
current, one-sided** (an EV under its limit is compliant; profile-Accepted is a
write success, *never* convergence — `ev-accept-but-ignore`); suspend (0 A)
converges at `currentA ≈ 0` (TransactionEvent Ended forces it). It **closes the
reassert-on-reconnect gap** the legacy path lacked (a reconnecting charger gets
its standing limit re-sent immediately, not after the 60 s watchdog). Corrective
re-write backoff starts at 15 s (≥ the 10 s per-call bound, so calls never
overlap). Legacy `lexa/evse/{station}/command` keeps flowing, ignored on OCPP
when active. Rollback for either class = set `shadow` and restart the service.

Editing the Pi's copy without updating `configs/modbus.json` in-repo is undone
by a full `deploy-hub-pi.sh` (it overwrites `/etc/lexa/*.json`); the in-repo
default stays conservative (`battery: off`) and the flip is a deliberate,
binary-only deploy + hand-set Pi config (05 §6 discipline).

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
- **Local wall-clock steps** (TASK-037, GAP-04, AD-004 extension): must not move
  utility-time evaluation (`internal/utilitytime`'s monotonic anchoring — `Clock.Anchor`/
  `ServerNow`) nor freshness windows (already monotonic ages, `time.Now()`+`Sub`).

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
