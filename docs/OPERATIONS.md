# Operations: crash-only services (AD-011)

This is the operator-facing companion to AD-011 (`docs/refactor/02_ARCHITECTURE_DECISIONS.md`
in `csip-tls-test`, review §10.6). It exists because the review asked for the
crash-only design to be **written down in operator terms**, not just decided
in an ADR: what a restart of each of the six lexa services actually loses,
what comes back on its own, and what to check afterward (TASK-045).

## The rule

**A wedged or failing lexa service is meant to crash and be restarted by
systemd, not to catch itself and limp on.** There is no blanket `recover()`
anywhere in this codebase, and none should be added. A panic recovered in
place is a service continuing to run in a state its own author didn't
anticipate — for a DERMS hub commanding real hardware, an honest crash
followed by a clean restart from retained/re-published state is safer than
a silently-corrupted control loop. `internal/watchdog`'s `sd_notify` kicks
(TASK-007/008) exist to catch the OTHER half of this: a process that is
still scheduling goroutines but whose actual work (engine tick, poll loop,
HTTP handler) has wedged. Do not add `recover()` to work around a specific
panic; fix the panic, or if the panic is truly expected/benign, return an
error instead of panicking.

## What restarts, and how fast

Every service is `Type=notify`, `Restart=on-failure`, with a
`WatchdogSec` sized for its own liveness signal (see each repo's
`systemd/lexa-*.service` and the lexa-hub `CLAUDE.md` watchdog table).
`RestartSec` (systemd's delay before restarting) is 5 s for lexa-hub,
lexa-api, and lexa-modbus, and 10 s for lexa-northbound, lexa-ocpp, and
lexa-telemetry. A crash loop (repeated failures within systemd's default
burst window) will eventually stop restarting — `systemctl status
lexa-<svc>` shows this as `Result: start-limit-hit`; `systemctl reset-failed
lexa-<svc>` clears it once the underlying cause is fixed.

## What is lost on a restart, per service

All six services keep their live state in memory only; nothing is persisted
to disk today (TASK-041, a JSON snapshot for hub breach-episode state, is
still TODO — see its own task file for scope and the AD-005 bound on what a
future snapshot would and would not cover). What comes back, and how fast,
differs by what MQTT gives each service back for free:

- **Retained topics come back immediately** on (re)subscribe:
  `lexa/csip/control`, `lexa/csip/pricing`, `lexa/csip/billing`,
  `lexa/csip/flowreservation/status`, `lexa/northbound/schedule`, and
  `lexa/hub/plan` (the plan heartbeat, TASK-045) are all published retained.
  A restarted lexa-hub or lexa-api sees the last value of each within one
  MQTT round trip of connecting — no gap where the hub thinks there is no
  active CSIP control just because it restarted.
- **Non-retained topics do NOT come back**: `lexa/measurements/{device}`,
  `lexa/battery/{device}/metrics`, and `lexa/evse/{station}/state` are
  live-only (QoS 0, freshness-gated by subscribers — see the lexa-hub
  `CLAUDE.md` MQTT topic map). A restarted lexa-hub has zero device state
  until lexa-modbus/lexa-ocpp publish their next reading, which happens on
  their own independent poll/session cadence (typically a few seconds) —
  it is not itself waiting on the restarted service.

**lexa-hub**: loses `MQTTSystemReader`'s in-memory snapshot (last
measurement per device, staleness-edge state, the resolved CSIP control's
expiry-confirm tick count) and the compliance-breach episode
(`activeBreachMRID`) and every actuator's command deduper. Effects:
  - The staleness/frozen-meter edge detectors (`noteStaleness`,
    `state.go`'s meter-frozen block) start from "never seen" — a real
    stale/frozen device won't re-alarm until its own staleness window
    elapses again post-restart, same as at first boot.
  - A restart mid-breach-episode WILL re-publish a fresh
    `ComplianceAlert{Active: true}` on the next tick that still sees the
    breach — a duplicate "begin" on the bus (harmless today; TASK-041 is
    the fix for the protocol-noise case where northbound also restarted and
    re-POSTs a duplicate CannotComply to the utility).
  - Every actuator's dedupe cache resets, so the FIRST command each
    actuator sends post-restart always goes out (never suppressed as
    "unchanged") — the opposite risk (a command NOT resent promptly) shows
    up on the **lexa-modbus** side instead, next bullet.
  - The optimizer's guard sessions (`expGuard`/`impGuard`/`genGuard`, etc.)
    reset to their unarmed state. This is intentional, not a gap to fix:
    they re-converge within a few ticks from live measurements, and
    persisting them across a restart risks exactly the guard×guard
    interaction class the review's W2 finding warns about.

**lexa-api**: purely a read-side aggregator — restarting it loses nothing
that matters operationally; every field it serves is rebuilt from retained
+ live MQTT within seconds. The plan heartbeat (TASK-045) starts at `never`
until the retained `lexa/hub/plan` redelivers (near-immediate if lexa-hub is
up), at which point it reports `ok` even though the redelivered plan's own
timestamp may be old — that is correct (see `cmd/api/heartbeat.go`'s
`planHeartbeat` doc): the heartbeat answers "is the bus delivering plans
right now," not "how fresh is this particular payload."

**lexa-northbound**: loses `responseTracker`'s dedupe state (which mRIDs it
has already POSTed a Response for) and `discoveryFailures`'s consecutive-fail
count. A restart can re-POST a Response for an event it already acknowledged
before restarting (the utility server is expected to tolerate a duplicate
Response for the same status; if a Test Server session flags this, it is a
restart-timing artifact, not a new protocol bug). The next discovery walk
(within `discovery_interval_s`) re-publishes retained control fresh either
way.

**lexa-modbus**: loses each device's `lastCtrl` (the desired command
`reassertLocked` re-applies on reconnect). Operator gotcha: if lexa-modbus
restarts while a control is active and UNCHANGED since lexa-hub last sent
it, lexa-hub's own dedupe (`cmdDeduper`, 60 s `reassertEvery`) may not
resend that command for up to 60 s, because from lexa-hub's point of view
nothing changed — it does not know lexa-modbus restarted. A battery/inverter
can therefore run on its last-applied setpoint (not a released/default
state — Modbus device registers hold whatever was last written) for up to
that window. This is bounded and self-correcting, not a hang; if faster
reconvergence is needed operationally, restart lexa-hub too (its dedupers
reset unconditionally, per above).

**lexa-ocpp**: loses all in-memory station/session state
(`mqttBridge`'s `stationState` map). Real chargers reconnect per the OCPP
spec's own reconnect backoff and re-send `BootNotification`/
`StatusNotification`; lexa-hub sees the affected EVSEs as stale
(`evseStaleAfter`, 90 s) until fresh state arrives, and treats a stale
active session as no longer chargeable in the meantime (fail-safe, not
fail-open).

**lexa-telemetry**: re-runs MUP registration for every configured device at
startup. Whether the utility Test Server treats re-registration as
idempotent (same MRID, no duplicate resource) depends on the server; this
has not caused an issue on the bench's gridsim, but a different Test Server
implementation is worth a smoke check the first time this repo talks to one.

## What to check after a restart

1. **Plan heartbeat** (TASK-045): `GET /status` on lexa-api →
   `plan_heartbeat.state`. `"never"` right after a bench bring-up where
   lexa-api came up before lexa-hub is expected and NOT an alarm; `"ok"`
   is the steady state; `"stalled"` (or the edge-triggered
   `lexa-api: plan heartbeat stalled` journal line, and the
   `lexa_api_plan_heartbeat_stalled` gauge going to 1) means lexa-hub's
   control loop is not advancing — check `journalctl -u lexa-hub` and the
   watchdog (a wedge should have already restarted it; a `stalled` reading
   that never recovers is a real page).
2. **`/status.last_plan.timestamp`**: should be within one engine interval of
   now. A frozen timestamp with a healthy heartbeat state would be a
   contradiction worth escalating (the heartbeat samples arrival, not this
   field — they should never disagree in steady state).
3. **`/status.csip_control`** and **`/status.stale_sources`**: confirm the
   retained control survived the restart as expected, and that no device
   is unexpectedly reported stale/frozen.
4. **`journalctl -u lexa-<svc> -f`**: with TASK-045's slog adoption, every
   migrated line carries `svc=lexa-<name>` plus stable keys (e.g.
   `mrid=...`, `age_s=...`) — `journalctl -u lexa-hub | grep 'svc=lexa-hub'`
   filters cleanly even when multiple services log to the same journal view.
5. **`/metrics`** on the restarted service: `lexa_up` back to 1,
   `lexa_mqtt_reconnects_total` incrementing as expected, and (lexa-api
   only) the two plan-heartbeat gauges consistent with `/status`.

## Do not

- Do not add `recover()` anywhere to suppress a crash — fix the panic's
  cause, or make the failure path return an error instead of panicking.
- Do not treat a `never` plan-heartbeat state as an alarm condition in any
  dashboard/alert you build on top of this — it is the documented silent
  state for "haven't seen a plan yet," not a fault.
- Do not persist optimizer guard state across a restart if you revisit
  TASK-041 later — that is a deliberate exclusion (see lexa-hub's restart
  bullet above), not an oversight.
