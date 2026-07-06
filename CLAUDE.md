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
| `lexa/desired/{class}/{device}` *(retained, AD-013)* | lexa-hub | lexa-modbus, lexa-ocpp | 1 |
| ~~`lexa/control/battery/{device}`~~ | — | — | — |
| ~~`lexa/control/solar/{device}`~~ | — | — | — |
| ~~`lexa/evse/{station}/command`~~ | — | — | — |
| `lexa/reconcile/{class}/{device}/report` *(retained)* | lexa-modbus, lexa-ocpp | lexa-hub | 1 |
| `lexa/csip/rewalk` *(TASK-042)* | lexa-hub | lexa-northbound | 1 |
| `lexa/northbound/certstatus` *(retained, TASK-072)* | lexa-northbound | lexa-api | 1 |

The three struck-through `lexa/control/*` / `lexa/evse/+/command` legacy command
topics were **removed in TASK-032**: the retained `lexa/desired/{class}/{device}`
document (AD-013) is now the sole command path, executed by the device
reconcilers. The topic-name constants remain in `internal/bus/topics.go` marked
`Deprecated` for one release (external tooling), with no producer or consumer.
| `lexa/csip/compliance/alert` | lexa-hub | lexa-northbound | 1 |

**CannotComply path (3 stages, TASK-031)** — collapsed from the old 5-hop chain:
(1) *evidence* — the optimizer's meter-level `Plan.Breach` AND the reconcilers'
device-level non-convergence (`ReconcileReport` NonConvergedBegin/End, retained
per device so the hub re-seeds state after a restart) — feeds (2) the named
`breachEpisodes` component (`cmd/hub/breach.go`), the single owner of episode
state, which merges both sources under one episode ID and emits ONE
edge-triggered `ComplianceAlert` (non-retained — an edge, not state) per episode
onset/clear, consumed by (3) northbound's `responses.Tracker`, which POSTs exactly
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

**Cert expiry monitoring (TASK-072/§10.5)**: lexa-northbound inspects its
configured `client_cert`/`ca_cert` PEM files (leaf NotAfter, via `crypto/x509`
— pure PEM inspection, no wolfSSL/cgo involvement) at startup and every 24h
(`cmd/northbound/certmon.go`). Results publish retained on
`lexa/northbound/certstatus` (`bus.CertStatus`), fold into `GET /status`'s
`cert_status` field (`cmd/api`), and log a WARN once `days_left` (the sooner
of the client/CA cert, whichever is more binding) falls to
`cert_expiry_warn_days` (config key, default 30) or an ERROR once either cert
has expired. Gauges: `lexa_cert_expiry_client_seconds` /
`lexa_cert_expiry_ca_seconds` (seconds to NotAfter, negative once expired)
and `lexa_cert_expiring_client` / `lexa_cert_expiring_ca` (1 once inside the
warn window or unreadable, else 0) — two flat gauge names rather than a
single `lexa_cert_expiry_seconds{cert=...}` because `internal/metrics` has no
label dimension (see certmon.go's `Monitor` doc). lexa-telemetry points its
own config at the SAME cert files but does not run a second monitor — one
inspection of the shared file's content is enough. An unreadable/unparsable
cert file is itself reported (fail-closed: `client_err`/`ca_err` populated on
the bus message and in `/status`) rather than a silent gap. TASK-073
(rotation, not yet built) will use this monitor's `days_left`/error state as
its own verification signal.

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

### Async actuator publishes + tick budget (TASK-046)

The tick path must never block on a PUBACK: `cmd/hub/desired.go`'s three
`desiredPublishing*Actuator` types and `main.go`'s plan-log publish use
`mqttutil.PublishJSON(Retained)Async`, which hands the message to paho and
returns immediately (`*mqttutil.PendingPub`). Completion is checked later
("harvested") — non-blocking, via `PendingPub.Harvest` — at the START of the
NEXT call on the same actuator/publisher: a confirmed delivery leaves the
optimistic state alone; a broker error or a harvest past
`mqttutil.PublishTimeout` (5 s) rolls the actuator's dedupe baseline back to
what it was before the publish was fired, so the identical content is
retried on the next call — the same "late/dropped commands are harmless
because they're re-issued" contract `mqttutil.publishTimeout`'s doc comment
has always described, just discovered a call later instead of synchronously.
Each actuator also does one FREE opportunistic harvest immediately after
firing (Harvest never blocks) so an already-resolved ack/error is caught
without waiting a whole extra tick. Per-device publish ORDER is preserved
by paho (one client, one connection, calls serialized in call order) even
when a second, content-different command is fired while an earlier one for
the same device is still pending — see `desiredPublishingBatteryActuator`'s
field docs for the "one-slot pending" reasoning.

The ONE publish that stays fully synchronous is the compliance alert
(`main.go`'s `emitAlerts`), via `mqttutil.PublishJSONTimeout` bounded at 1 s
instead of the 5 s default: it is rare, edge-triggered, and its
ordering/latency against the CannotComply episode matter more than sparing
this pass's tick budget.

Tick budget: `main.go`'s planObserver measures its own wall time plus the
PRIOR pass's total actuator `Apply*Command` time (a shared `*tickTiming` in
`desired.go`, since `PlanObserver` fires before `executePlan` — see
engine.go — so this pass's actuator time isn't known until the next call;
`internal/orchestrator` stays untouched, 05 §1). Exceeding 50% of the engine
interval sets `lexa_hub_tick_overruns_total` (Phase 4 exit criterion: zero
under the mqtt-broker-latency scenario in FAST mode) and logs edge-triggered.

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
  northbound/{run,publish,responses,flowres} Northbound god-file decomposition
              (TASK-068, D12/R5): run = walk loop + TASK-042 rewalk
              single-flight; publish = MQTT publishers/converters; responses
              = CORE-022/GEN.044 responseTracker (renamed responses.Tracker);
              flowres = §10.9 flow-reservation manager. cmd/northbound/main.go
              is wiring only.
  tlsclient/  wolfSSL mTLS client (keep-alive fetcher). HTTP/1.1 response
              parsing lives in the CGo-free httpwire/ leaf (fuzzed; 64 KiB
              header + 10 MiB body caps). Chunked Transfer-Encoding is DECODED
              (AD-009 option (b), TASK-069); net.Conn-shim-under-http.Transport
              was deferred as a P6-with-time backlog item.
  wolfssl/    CGo wrapper for wolfSSL_Init (call exactly once per process)
  southbound/ Modbus/SunSpec: device interface, inverter, battery, meter, registry
  orchestrator/ Rule-based optimizer engine (no I/O — reads SystemState, returns Plan)
  orchestrator/constraint/ Priority-ordered constraint controller (safety > compliance
              > economics; AD-007). Constraint/Demand/Arbiter/Session/Stack; Stack
              implements orchestrator.Optimizer. Also the shadow harness (TASK-059,
              shadow.go): Wrap(legacy, candidate) runs both per tick, diffs FINAL
              per-device outputs under tolerance bands, counts+logs divergences,
              returns the LEGACY plan unmodified. TASK-060 wired the first real
              constraint — ExportConstraint (export.go/export_session.go: ports
              applyExportLimitRule+expGuard and checkExportLimitConvergence+
              expOverTicks with the two reset cadences kept separate; adaptive
              detection window). The cascade in optimizer.go stays the AUTHORITATIVE
              live path; the export logic runs in the Stack in SHADOW only (0 diff
              gate passed) until the `export: active` flip.

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

`hub.json`'s `devices[]`/`stations[]` entries accept an optional per-entry
`"plant"` block (TASK-057, AD-007): the device's physical-response parameters —
inverter `max_ramp_{down,up}_w_per_s` + `control_latency_s`, battery
`capacity_kwh`/`soc_taper_start_pct`/`converge_frac`/`control_latency_s`
(+ optional `taper_curve`), meter/EVSE `meter_lag_s`. Fields are unit-suffixed and
wall-clock (per-second, not per-tick). Types live in
`internal/orchestrator/plantmodel.go`; the shipped example values equal today's
bench calibration. **The block is optional and currently UNWIRED** — nothing reads
it until TASK-064 swaps the bench-calibrated optimizer globals for these
parameters; a missing block ⇒ bench defaults (`withDefaults`), an unknown key
warns but never fails load, so pre-TASK-057 hub.json files parse unchanged.

`hub.json`'s top-level `"constraint_shadow"` bool (TASK-059, default false) turns
on the constraint-stack **shadow harness**: every economic tick ALSO runs the
candidate constraint `Stack` observe-only, diffs its final per-device outputs
against the live `DefaultOptimizer` under tolerance bands, and logs one
rate-limited JSON line (`constraint-shadow divergence`) per divergent tick plus a
`lexa_constraint_shadow_divergence_total` metric / `shadow_divergences` field on
`lexa/hub/plan`. **The legacy cascade stays the sole author of actuated plans** —
the candidate's plan is discarded, never actuated. Flag off ⇒ the wrapper is not
even constructed (zero behaviour change). This is the ≥1-week bench-shadow gate
for every P5 flip (03 §P5); TASK-060 adds the first real constraint.

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
reconciler is the **single reasserter**: TASK-032 deleted the transport-side
`retryDevice.lastCtrl` replay entirely, leaving the reconciler's
reassert-on-reconnect (via `retryDevice.onReconnect`) as the only one. TASK-032
also **deleted** the legacy `lexa/control/battery/{device}` command path; the
retained desired doc is the sole battery command path (config
`reconciler.battery` MUST be `"active"` — off/shadow is a fatal config error).

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
(clamped to WMax → 100%). The optimizer-side `restoreOnGenLimitClear` edge
emission was **deleted in TASK-032** as redundant (`applyRestoreRule` emits the
same NaN restore end-of-pass; the retained desired doc carries it to a dark
inverter on reconnect). There is no Tier-0 interlock for solar. The inverter
reconciler is the **single reasserter**: TASK-032 deleted `reassertLocked`, so
reassert-on-reconnect is the shell's Reconnected() plus an initial-desired
**seed** (restore ceiling) for the never-commanded case. TASK-032 **deleted** the
legacy `lexa/control/solar/{device}` command path (config `reconciler.solar` MUST
be `"active"`).

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
overlap). TASK-032 **deleted** the legacy `lexa/evse/{station}/command` path
(config `reconciler` MUST be `"active"` when stations are configured). Rollback
is now `git revert` of the relevant TASK-032 commit + redeploy — the config-flip
rollback ended with the legacy-path deletion.

Editing the Pi's copy without updating `configs/modbus.json` in-repo is undone
by a full `deploy-hub-pi.sh` (it overwrites `/etc/lexa/*.json`); the in-repo
default stays conservative (`battery: off`) and the flip is a deliberate,
binary-only deploy + hand-set Pi config (05 §6 discipline).

## Critical invariants

- **wolfSSL_Init**: process-global C state. `wolfssl.Init()` is called once in
  `main()` of lexa-northbound and lexa-telemetry only. The other three services are
  pure Go and never touch wolfSSL.
- **Cipher**: `ECDHE-ECDSA-AES128-CCM-8 TLSv1.2` only (CSIP §5.2.1.1).
- **Bus messages**: `math.NaN()` never appears in JSON — use `*float64` (nil = absent),
  and the DECODE layer rejects non-finite numeric input (NaN/Inf, quoted or bare) with
  an alarm; a NaN limit never reaches the optimizer (GAP-09, TASK-055: stdlib already
  refuses NaN/Inf into typed float64 fields — internal/bus/nan_reject_test.go pins that
  — plus a `Finite()` defense-in-depth check on every `*float64`-bearing message type,
  wired into `mqttutil.Subscribe` and counted via `bus.RecordDecodeFailure`).
- **CSIP control is retained**: lexa-northbound publishes `lexa/csip/control` with
  retain=true so lexa-hub gets the last value immediately on (re)start.
- **Retained control adoption is staleness-checked; corrupt retained control
  triggers re-request — never silent** (TASK-042, §8.3/GAP-01/GAP-02): a
  retained `lexa/csip/control` older than `retained_adoption_max_age_s`
  (default 300, config `cmd/hub/config.go`) at ADOPTION time is still
  enforced unchanged (enforce-but-verify, never fail-open — a stale-but-
  decodable cap is never dropped) but raises an alarm and publishes
  `lexa/csip/rewalk` (`bus.TopicCSIPRewalk`, hub→northbound, QoS 1, not
  retained). A retained payload that fails to decode at all (previously a
  silent log-and-drop, `mqttutil.Subscribe`) now does the same via
  `mqttutil.SubscribeDecodeErr`'s decode-error hook. lexa-northbound honors a
  rewalk request by immediately republishing its last-published control
  (fresh `Ts`) and triggering an out-of-cadence discovery walk; both sides
  rate-limit independently (hub: `cmd/hub/state.go`'s `rewalkRateLimit`;
  northbound: `internal/northbound/run`'s `rewalkGate`, `nbRewalkRateLimit`,
  TASK-068) —
  10 s floor each — since the retained control-plane topics redeliver on
  every broker reconnect.
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
  | lexa-northbound | 120 | walk-loop `for` body top + once after the initial walk (`internal/northbound/run.Discovery.Loop`, TASK-068) — sized ≥4x a legitimate long walk under `northbound-hang` conditions |
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
- **SOM zone must match the tariff zone** (TASK-079, GAP-05): `TOUCostModel.IsPeakHour`/
  `CurrentRate`/`CurrentPeriodLabel` (`internal/orchestrator/costmodel.go`) and the
  planner's `localHourOf`/price-shaping helpers (`planner.go:606-651`) all read
  `t.Hour()` — i.e. hour-of-day in whatever `time.Location` the caller's `time.Time`
  carries. The optimizer's `serverNow` (`time.Unix(utilitytime.ServerNowAt(now,
  ClockOffset), 0)`) renders in the **process's configured zone** (the SOM's
  `/etc/localtime`/`TZ`), which is correct *only if that zone matches the zone the TOU
  tariff is written in* — utility tariffs are defined in local clock time, so the
  correct fix is zone-aware evaluation, not zone-free (never collapse this to UTC). A
  SOM provisioned with the wrong zone (e.g. `TZ=UTC` serving an America/Los_Angeles
  tariff) silently misprices — and mis-times autonomous peak-shift discharge — every
  single evening, with no error or alarm anywhere in the stack today.
  **Deployment requirement: the hub SOM's configured timezone must be set to the
  tariff's zone at commissioning.** DST transitions (spring-forward gap / fall-back
  fold) are handled correctly by the hour-of-day arithmetic as long as the zone is
  right — pinned by `internal/orchestrator/costmodel_test.go` /`planner_test.go`'s DST
  tables (`TestTOU_UTCvsLA_Divergence_DeploymentHazard` pins the zone-mismatch
  divergence specifically).

## Defensive fault-handling (do not strip — each backs a mayhem-QA finding)

Hostile-QA (`csip-tls-test` mayhem suite) surfaced fault classes the optimizer must
survive; the guards below are deliberate, not redundant. Keep them when refactoring.

- **Trust measurement, not the command** (closed-loop convergence, `internal/orchestrator`):
  a device can ACK a write and ignore it. `checkGenLimitConvergence` (cross-checked against an
  independent meter floor `gen ≥ export − batteryDischarge`), `checkImportConvergence`, and the
  export rule's battery-absorption guard all compare commanded vs MEASURED effect and, on a
  sustained gap, curtail another lever or post a 2030.5 CannotComply. The `checkGenLimitConvergence`
  meter-independent floor is a HARD preserve (an inverter that echoes the cap while still generating
  is caught only by it). The export/gen/import convergence backstops are ALL ported to the
  constraint package running in SHADOW — `orchestrator/constraint/export.go` (TASK-060),
  `constraint/genlimit.go` (meter floor, verbatim) + `constraint/importlimit.go` (NaN-hold
  counter) (TASK-061); the optimizer.go copies stay authoritative until the per-axis `active`
  flips — do not strip either side.
- **Fail closed on bad CSIP**: the scheduler holds last-known-good on an empty/malformed
  resource (see `internal/northbound/CLAUDE.md` rule 6).
- **Plausibility-gate ingested telemetry**: `cmd/modbus` withholds power over nameplate
  (suspect scale factor); `cmd/ocpp` rejects MeterValues current over the station rating.
- **Battery reserve safety**: `checkBatterySafety` force-disconnects a pack measured
  discharging at/below its SOC reserve (a device inverting its setpoint).
- **Converge the device to the desired state** (the Device Reconciler,
  `internal/reconcile` + the modbus/ocpp shells): this is now the SINGLE owner of
  "make the device match what the hub wants." Verify-by-readback + write-on-diff
  (bounded by the poll/readback interval) and reassert-on-reconnect from the
  retained desired doc replaced the legacy convergence machinery — the actuator
  `cmdDeduper` + 60 s watchdog + breach-reset, and the transport-side
  `retryDevice.lastCtrl` reassert — all **deleted in TASK-032** (behaviors
  preserved; see `docs/refactor/PRESERVATION_LEDGER.md` L1–L4). Do not re-introduce
  a second writer/reasserter — a split-brain writer is the W6/D3 foot-gun this
  deletion closed.
- These hold the cap / surface the fault; the matching regression tests are
  `*_test.go` next to each (`convergence_test.go`, `failclosed_test.go`, etc.).

## Stack

Go 1.26 · wolfSSL CGo (northbound + telemetry only) · eclipse/paho.mqtt.golang v1.5.1 ·
lorenzodonini/ocpp-go · simonvetter/modbus · grandcat/zeroconf
