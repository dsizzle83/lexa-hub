# Flash wear budget — LEXA hub Pi journal

*TASK-009 (review §11 "flash wear [Likely]", RSK-14). Owns the number that
per-tick logging changes get reviewed against instead of a vibe (05
§9: "flash is a consumable").*

## Status of the numbers in this doc

**This is a pre-deploy estimate, not a bench measurement.** TASK-009's code,
unit-file, and doc changes land in this session; the bench measurement pass
(step 1 of the task), the live deploy, and the fault-storm suppression check
(steps 4–5) are deferred to the P0-exit gate alongside TASK-007/008's staged
bench work (see `docs/refactor/00_MASTER_INDEX.md`). The rates below are
derived from the architecture review's own background numbers
(`docs/refactor/tasks/TASK-009.md`) and each service's documented tick/poll
cadence (this repo's `CLAUDE.md`, `docs/BENCH.md` in `csip-tls-test`) — they
are the basis for the per-unit `LogRateLimitBurst` values shipped in this
commit, sized generously (≈20×) over the estimate specifically so that
replacing this table with real `journalctl` counts later should only ever
need to raise, never lower, those caps. **Re-run step 1 and update this
table with measured numbers at or before the P0-exit gate; if measured
rates exceed what's budgeted here, raise `LogRateLimitBurst` before merging
anything that increases logging further.**

## Estimated rates table

FAST mode (bench QA tick/poll cadences; see `csip-tls-test/CLAUDE.md`):

> **Measured 2026-07-05 (P0-exit gate, FAST, 10-min window):** hub 108
> lines/min, northbound 12, modbus 12.6, ocpp 12, telemetry 3, api 0.1 —
> total ≈148/min ≈ 213k/day. The hub runs ≈2.7× the per-tick estimate
> below (≈54/30 s vs est. ~20) but stays far under its 400 burst cap; caps
> unchanged. Journal disk usage at measurement: 34.8 M of the 200 M
> `SystemMaxUse`. Re-measure at the P5 gate when the optimizer split
> changes the hub's logging profile.

| Service | Cadence (FAST) | Est. lines/cycle (normal) | Est. lines/30s | `LogRateLimitBurst` (20×, rounded up) |
|---|---|---|---|---|
| lexa-hub | 3 s engine tick | ~2 (plan summary + occasional decision) | ~20 est. / **~54 measured** | 400 |
| lexa-northbound | 5 s discovery walk | ~2 (discovery OK line + occasional response/pricing/billing) | ~12–24 | 300 |
| lexa-modbus | 3 s tick (battery re-command), 2 s × 3 devices poll (silent unless error) | ~1 per battery device on tick | ~10 | 300 |
| lexa-telemetry | 60 s MUP post (bench) | ~1 per device per post | <5 | 150 |
| lexa-ocpp | event-driven (StatusNotification/MeterValues/TransactionEvent) | ~1 per OCPP message; active session can be sub-5s | variable, session-dependent | 300 |
| lexa-api | event-driven, but per-event detail goes to the in-memory `/logs` SSE buffer (`lb.Emit`), NOT the journal — journal only sees startup/Fatalf/probe-warning lines | ~0 steady-state | ~0–2 | 100 |

STOCK mode (customer-deployed cadences: hub tick 15s, northbound/modbus
walks/polls proportionally slower) is expected at roughly a third to a
fifth of the FAST rates above per the review's own FAST/STOCK ratio; no
STOCK-specific caps are set separately — the FAST-sized `LogRateLimitBurst`
values already cover STOCK with even more headroom.

Total estimated volume, FAST, all six services normal operation: ≈50–60k
lines/day (review §11's own figure, "~2 journald lines per hub tick ≈ 50k
lines/day in FAST", extended pro-rata across the other five services whose
walk/poll cadences are similar order of magnitude) — order 20–30 MB/day at
an estimated ~100–150 B/line on-disk (journald per-entry overhead:
timestamp, unit metadata, message). STOCK is expected at a few MB/day.

## Journal size cap

`systemd/journald-lexa.conf` (installed to
`/etc/systemd/journald.conf.d/lexa.conf` by `scripts/deploy-hub-pi.sh`):

```
[Journal]
SystemMaxUse=200M
SystemKeepFree=500M
MaxRetentionSec=1month
```

200 MB at ~20–30 MB/day FAST ≈ 1–4 weeks of retention (comfortably covers a
multi-day Mayhem campaign's forensic window); at STOCK's few-MB/day it's
months. `SystemKeepFree=500M` leaves headroom for the rest of the SD/eMMC
(OS, `/etc/lexa` configs, Mosquitto's `autosave_interval 60` persistence
file — see the note below) so a busy journal never starves the rest of the
filesystem. `MaxRetentionSec=1month` is a time-based belt-and-suspenders cap
alongside the size cap; whichever triggers first wins.

`MaxLevelStore` was considered and rejected for this task: every service
logs at info level throughout (no separate debug tier to filter out), so
level-based filtering isn't the mechanism that bounds volume here — the
per-unit rate caps and this size cap are.

## SD/eMMC wear math

At an estimated 20–30 MB/day (FAST) of journal writes, plus Mosquitto's own
`autosave_interval 60` persistence writes (same partition, not itself
changed by this task — see below), total sustained write load is on the
order of tens of MB/day. Typical SD/eMMC endurance for a decent-quality card
is on the order of thousands of P/E cycles per cell with wear-leveling
across the full device capacity; a 16–32 GB card doing tens of MB/day of
writes represents a total-bytes-written figure many orders of magnitude
below typical rated TBW (terabytes written) endurance specs. **Wear is not
the risk this budget guards against — a *regression* is.** A debug-spam bug
logging at ~100 lines/s (a real failure mode: an unbounded per-request or
per-packet log line) would write roughly 1 GB/day, which is the scenario
the per-unit `LogRateLimitBurst` caps exist to catch and suppress before it
either fills the 200 MB journal cap in hours or meaningfully accelerates
wear. The caps in this doc are set to pass all currently-known normal and
fault-storm traffic (pending the bench validation noted above) while still
catching that class of bug within one or two `LogRateLimitIntervalSec`
windows.

## The rule

**A change adding more than ~0.2 lines/tick sustained, on any service,
updates this doc** (05 §9). "Tick" means that service's own natural cadence
from the table above (hub's engine tick, northbound/modbus's walk/poll,
telemetry's post interval) — an event-driven service like ocpp/api is judged
against its own governing event rate instead. Re-run the TASK-009 step 1
measurement commands (`journalctl -u <unit> --since -10min | wc -l`,
`journalctl --disk-usage`) after any such change and update the rates table
and, if needed, that unit's `LogRateLimitBurst`.

## Related budgets

- TASK-039's event journal (P3) must set its own quota that fits *inside*
  this budget, not stack an independent one on top of it.
- TASK-045 (structured logging, if/when adopted) obeys the same
  per-unit/per-tick budget — structuring the log line doesn't exempt it from
  the rate math.
- TASK-050 (disk-full scenario) validates behavior once the cap here is
  actually hit; not exercised by this task.
- TASK-078 (soak) is the vehicle that should capture real wear/rate
  telemetry over a long run and replace the estimates in this doc.

## Mosquitto persistence note

The distro's Mosquitto `autosave_interval 60` write load lives in the
distro's own `mosquitto.conf` on the Pi (not the `lexa.conf` listener
drop-in this repo installs — see `scripts/deploy-hub-pi.sh`) and is on the
same SD/eMMC partition as the journal. It contributes to the same physical
wear budget but is measured, not changed, by this task. Retained-message
store trust and any persistence tuning are TASK-042's problem.
