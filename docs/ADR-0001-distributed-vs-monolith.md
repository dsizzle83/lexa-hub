# ADR-0001 — Keep the distributed service architecture; fix the control loop, not the topology

- **Status:** Accepted (2026-07-01)
- **Deciders:** Principal architecture review, driven by the v3 Mayhem QA campaign
- **Context repos:** `lexa-hub` (product), `csip-tls-test` (test bench / Mayhem QA)

## Context

The v3 QA campaign (`csip-tls-test/docs/QA_REPORT_V3_20260701.md`, 42 scenarios × 10
cycles) leaves a residue of "sticky" FAILs — `battery-wrong-sign` (80% FAIL),
`curtailment-release` Mode A (70%), and the `ack-before-effect` / `battery-soc-refuse`
CannotComply timing races. The report attributes these to the distributed architecture:
"the distributed lexa-hub architecture introduces timing variability that the old
monolithic hub didn't have." That framing raised the question of whether the MQTT +
separate-services design is limiting performance and whether a monolithic rewrite is
warranted before customer deployment.

This ADR records the decision after tracing the actual data pathways.

## The measurement that settles it

Latency budget for a safety reaction (`battery-wrong-sign`, FAST mode: engine tick 3 s):

| Stage | Latency | Tunable |
|---|---|---|
| Modbus poll granularity (measurement freshness) | **0–10 s** | `poll_interval_s`, **not** lowered by fast mode |
| MQTT measurement publish (localhost, QoS 0) | <1 ms | — |
| Hub reads cached state, ticks | every 3 s (fast) / 15 s (stock) | `engine_interval_s` |
| `battWrongDirTicks=3` debounce | **9 s (fast) / 45 s (stock)** | code constant |
| Optimize() compute | sub-ms | — |
| MQTT control publish (QoS 1) | ~1–2 ms | — |
| modbus applies control (sync in MQTT callback) + Modbus write | ~5–30 ms | — |

Of the ~16–20 s the distributed hub takes to disconnect a mis-wired battery in fast mode,
**MQTT and process separation account for under 5 ms — less than 0.1%.** The remaining
99.9% is two software parameters — the 10 s poll and the 3-tick debounce — both of which
would be **identical in a monolith**.

The report's own evidence is a misread of causation:

> "V1 avoided this 6/10 times because the old monolithic hub's synchronous in-process
> Modbus write could disconnect the battery within a single polling cycle (~2 s vs
> ~10–30 s for the distributed hub)."

V1 was faster because it ran a ~2 s tight poll-and-write loop with little/no debounce — a
*loop-rate* property, not a *topology* property. It still failed 4/10; it was faster and
got lucky more often. A monolith with a 15 s tick and a 3-tick debounce would be exactly
as slow.

## Root causes (none of which a monolith fixes)

1. **One control loop doing two jobs.** `Optimizer.Optimize()` runs economic optimization
   (TOU arbitrage, peak shaving, CSIP-envelope following) *and* safety reflexes
   (`checkBatterySafety`, reserve-drain / sign-inversion trips) on one ticker. Safety
   must fire in <1–2 s and must not be debounced away; economics *should* be slow and
   debounced. Fusing them means every attempt to speed up safety chatters the actuators.

2. **Measurement-freshness ceiling.** The 10 s Modbus poll bounds how fresh any reading
   can be, and `hub-replay-tune.sh fast` never lowers it (it edits only `hub.json` and
   `northbound.json`). The poll loop is serial across devices with a 5 s per-device
   timeout, yet a full 3-device SunSpec read is ~50–100 ms — the southbound is ~99% idle.

3. **Tick-denominated thresholds.** `battWrongDirTicks=3`, `genBreachTicks=3`,
   `importBreachTicks=5` change their wall-clock meaning 5× between fast (3 s) and stock
   (15 s). The product ships in stock timing but is validated in fast timing — so the
   safety/CannotComply latencies that ship are **not the ones that were tested**.

## Decision

**Keep the distributed (MQTT + separate-services) architecture. Reject the monolith
rewrite. Fix the control loop inside the existing services.**

### Why not a monolith — weighed against a dual-core ARM64 + Go

- The box is ~idle (10 s polls, low single-digit CPU). Six Go runtimes cost ~60–120 MB
  RSS total and near-zero CPU while blocked on I/O. CPU/RAM are not the constraint; there
  is ample headroom to poll 10× faster and add a fast loop. "Dual-core is limiting us" is
  false — self-imposed conservative timing is.
- **Fault isolation:** a wolfSSL CGo segfault (grid-facing TLS) or an OCPP-library panic
  must not take down the battery-disconnect path. In a monolith it shares an address space
  with the safety-critical control loop. For a grid-tied device this is a real safety
  property.
- **CGo blast radius / build:** `lexa-modbus` and `lexa-hub` are `CGO_ENABLED=0` pure Go
  and cross-compile trivially; only `northbound`/`telemetry` link the ARM64 wolfSSL
  sysroot. A monolith forces the CGo cross-compile burden onto the whole product.
- **Boundaries are already good:** grid-facing (untrusted, CGo, TLS) / device-facing
  (trusted LAN, pure Go) / brain (pure Go, no I/O, fully unit-testable). The last property
  is why the optimizer has real regression tests; a monolith tends to erode it.
- **Effort:** a monolith is weeks of rework + full re-verification and would **not** move
  the QA scoreboard, because the debounce and poll-rate causes travel with the code.

## Consequence: the two-loop control hierarchy (protection relay vs. EMS)

Standard power-systems split — a fast, dumb, local protection layer under a slow, smart,
central optimization layer — mapped onto the existing services with **no new processes**:

- **Tier 0 — Edge interlock (in `lexa-modbus`):** last-ditch local reflex (reserve
  over-discharge, sign inversion, cease-to-energize echo) that trips without waiting on the
  hub or broker. Survives a hub crash / broker restart. *(Phase 2.)*
- **Tier 1 — Fast protection loop (in `lexa-hub`):** extract safety checks out of
  `Optimize()`; generalize `Engine.Wake()` (today only CSIP-disconnect) so the southbound
  can push a threshold-crossing event that wakes the hub in <1 s; reserve-proximity trips
  bypass the debounce. *(Phase 1a starts this with the reserve-proximity fast trip.)*
- **Tier 2 — Economic loop (unchanged):** `Optimize()` + `DailyPlanner` keep their 15 s /
  15 min cadence and their debounce.
- **Cross-cutting:** denominate thresholds in wall-clock time, not ticks, so fast and stock
  behave identically (`tickInterval`-scaled thresholds — Phase 1a). Split the Modbus poll
  (fast safety-register poll + parallel per-device reads — Phase 1b/Phase 0).

## Roadmap to customer deployment

- **Phase 0 — Stop guessing:** edge-triggered latency telemetry (fault-onset→trip,
  sustained-breach duration); make fast mode also lower the Modbus poll.
- **Phase 1 — High-leverage, low-risk:** wall-clock-scaled thresholds + reserve-proximity
  fast trip; parallel/faster poll; uncurtail-on-release write; fix the
  `malform-empty-program` test baseline.
- **Phase 2 — Two-loop split:** `checkSafety` extraction; southbound-event `Wake()`;
  Tier-0 edge interlock.
- **Phase 3 — Deterministic QA:** drive scenarios against a fixed clock + settle-detection
  so PASS/FAIL reflects hub behavior, not bench jitter.
- **Phase 4 — Deployment hardening:** systemd watchdogs; defined broker-loss behavior;
  southbound re-assert-on-reconnect; cert rotation without control interruption;
  regenerated conformance evidence.

## Notes

- `battery-wrong-sign` is not fully cleared by Phase 1 alone; the reserve-proximity fast
  trip (Phase 1a) helps, but the full fix is the Tier-0/Tier-1 split in Phase 2.
- If `curtailment-release` Mode A persists after the Phase 1c uncurtail-on-release write,
  the residue is the southbound re-assert-on-reconnect gap (Phase 4): the actuator deduper
  assumes device state survives a reconnect.
- Lockstep: southbound poll/register changes touch `csip-tls-test` too (audit MTR-4).
