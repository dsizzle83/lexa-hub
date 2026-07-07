# V1.0-RC gate findings — product-side (TASK-081, 2026-07-06)

The V1.0 release-candidate gate (bench repo `csip-tls-test`, branch
`task/081-release-gate`, full report `docs/QA_REPORT_V1RC_20260706.md`)
deployed this repo's `main@c730302` as one release build and validated it on
hardware. Two findings land in **lexa-hub** (product) and one in the deploy
tooling. All fixes are separate reviewed changes — none authored at the gate.

## FINDING A (med) — `power-cut-retained-rollback`: rollback breach + `lexa-api` start-limit
- **Observable:** the GAP-01 unclean SIGKILL power-cut causes a ~40 s 4400 W
  export-cap breach during the retained-control rollback window (SAFETY held —
  no INV-SOC/EVMAX violation; this is compliance timing). Separately, the
  power-cut's restart storm knocks **`lexa-api` permanently `inactive`** by
  tripping its systemd start-limit — during the gate this poisoned five later
  scenarios (all diagnosed "/status unreachable" while `lexa-hub` +
  `lexa-northbound` stayed up and kept enforcing).
- **Root cause (unit file):** `systemd/lexa-api.service` places
  `StartLimitIntervalSec=60` / `StartLimitBurst=5` in the **`[Service]`**
  section, where systemd ignores them (journal: *"Unknown key
  'StartLimitIntervalSec' in section [Service], ignoring"*) — so it falls back
  to the default 10 s/5 window and gives up under the storm. The same keys are
  worth auditing on the other five units.
- **RESOLVED 2026-07-06 (two commits, bench-traced):**
  1. `StartLimit*` moved `[Service]`→`[Unit]` in all 6 units (commit ~`29f0ddb`).
  2. **The real mechanism** (found in bench validation): the power-cut scenario
     cleanly *stops* mosquitto, and every lexa unit had `Requires=mosquitto`,
     which stop-propagates a clean stop (exit 0 — so `Restart=on-failure`
     never fires) to all 6 lexa units; the scenario's recovery restarts only
     `lexa-hub` (+ its `Wants=`), leaving `lexa-api`/`lexa-telemetry`
     permanently dead. Fixed by `Requires=mosquitto`→`Wants=mosquitto` in all
     6 units (commit `c571419`) — a broker stop/restart no longer stops the
     lexa services; they survive via paho auto-reconnect (the crash-only
     design; `mqtt-broker-restart` already proved this). **RE-CONFIRM at next
     bench deploy:** redeploy + re-run `power-cut-retained-rollback`, assert
     `lexa-api` stays active throughout. The ~40 s rollback export-breach
     window is a separate 042-tuning item (SAFETY held) → tighten the
     staleness-bound/rewalk latency; tracked in `09_RELEASE_CHECKLIST.md`.
- **Fix:** move `StartLimitIntervalSec`/`StartLimitBurst` to the **`[Unit]`**
  section (all six units); size the retained-rollback re-adopt window so the
  post-power-cut breach stays inside the oracle.
- **Recovery on the bench:** `systemctl reset-failed lexa-api && systemctl
  restart lexa-api`.

## FINDING B (low) — `export-dither-at-breach`: false CannotComply on sub-threshold dither
- **Observable:** under a pure ±ε export dither at `exportCap ≤ 0 W` that never
  sustains a real breach (breach-seconds 4 of 304 s), the hub posts
  CannotComply — the leaky breach counter (`expOverTicks` /
  `genGuard.overCount`) accumulates across sub-threshold dither cycles. SAFETY
  held; this is compliance-reporting precision, same class as the
  `control-churn` convergence-window borderline.
- **Fix vector:** TASK-064 (R4 constraint controller) sizes the
  breach-detection window adaptively from the plant model rather than a leaky
  fixed counter — eliminates the class rather than tuning a constant. Do not
  oracle-tune (06 §4.5).

## FINDING D (low) — deploy: `/var/lib/lexa` not provisioned
- **Observable:** the release `lexa-northbound` (journal feature 039/041) exits
  at startup — cannot create `/var/lib/lexa/journal/northbound` because neither
  the systemd unit nor `scripts/deploy-hub-pi.sh` creates `/var/lib/lexa`
  (owned `lexa:lexa`).
- **Fix:** add `StateDirectory=lexa` to the units (systemd auto-creates
  `/var/lib/lexa` as the service user) or an `install -d -o lexa -g lexa
  /var/lib/lexa` step in the deploy script.

## What validated cleanly (product-side)
Reconciler control (battery/solar/EVSE), fail-closed disciplines
(wan-outage/northbound-hang/expired-control/malform-*/clock-step), retained
trust (042/043 `corrupted-retained-control` PASS), cert monitoring (072) +
rotation mechanism (073), OCPP SP2 (074, wss+BasicAuth live), MQTT ACL (013),
lexa-api auth (014), compliance journal (039/040) + snapshot (041) live on
disk, `/metrics` on all services (044), watchdog on all six (007/008), CSIP +
Modbus conformance.
