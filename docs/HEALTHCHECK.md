# `lexa-healthcheck` — the OTA commit-or-rollback gate

Reference for `cmd/healthcheck` (DEVICE_ROADMAP.md §8.1, TASK-098 / Unit
1.5). See that binary's package doc (`cmd/healthcheck/main.go`) for the
short version; this is the longer operator-facing reference, and
`scripts/mender/README.md` covers how it plugs into Mender/meta-mender.

## What it checks

One line per check to **stderr** (`[healthcheck] <name> PASS|FAIL|SKIP
(<detail>)`), a JSON summary to **stdout**, exit 0 iff every non-SKIP check
PASSed.

| # | Name | PASS when | Never gates on |
|---|---|---|---|
| 1 | `systemd` | `systemctl is-active` reports `active` for mosquitto + all six lexa-* units (plus lexa-cloudlink, only if `cloudlink.json` exists) | — |
| 2 | `api` | `GET /healthz` on the configured `listen_addr` returns 200 (tries https, falls back to http) | TLS being enabled or not (TASK-088 is a parallel, independently-landing unit) |
| 3 | `plan_heartbeat` | `/status`'s `plan_heartbeat.state == "ok"`, OR `"never"` **iff** `/etc/lexa/commissioned` is absent | — |
| 4 | `northbound` | `northbound.json`'s `server` is empty (idle by design), OR `/status.csip_programs > 0`, OR the northbound journal has an entry newer than boot | the WAN/utility server actually being reachable |
| 5 | `modbus` | every device in `modbus.json`'s `devices[]` appears in `/status.devices`, connected, and not in `stale_sources`; SKIPped when no devices are configured | — |
| 6 | `clock` | system year ≥ 2026 AND (`timedatectl` reports NTP-synced OR `/dev/rtc0` exists — see the weak-fallback caveat below) | actual NTP reachability at THIS instant (a synced-then-disconnected clock still passes) |
| 7 | `cloudlink` | absent/disabled `cloudlink.json` SKIPs; enabled ⇒ its `/metrics` (127.0.0.1:9106) is reachable AND (`lexa_cloudlink_connected==1` OR a `lexa_cloudlink_spool_bytes` gauge is present) | **cloud/WAN reachability** — an offline install must still commit |

A `SKIP` never fails the run (a check that legitimately doesn't apply, like
`modbus` with zero configured devices, must not block OTA commit); only a
`FAIL` does.

## Known weak signal: the clock check's RTC fallback

Check 6's `/dev/rtc0` fallback checks **existence only**, never the RTC's
actual value. A box with a present-but-wrong RTC (e.g. a dead coin cell
that free-runs from its own reset epoch) would need that epoch to also
happen to land ≥ 2026 to slip past — in practice this means the fallback
is really "NTP hasn't synced yet, but there's at least a plausible clock
source and the year happens to check out", not "the clock is provably
correct". This exists because `timedatectl`/`systemd-timedated` are not
guaranteed present on every Yocto image variant this tool might run on
(DEY ships chrony for sync; see DEVKIT.md's systemd-version-quirks notes).
If a future image guarantees `timedatectl`, this fallback can be tightened
or removed — see `cmd/healthcheck/check_clock.go`'s doc for the exact
reasoning.

## Flags

```
lexa-healthcheck [-budget 120s] [-commit] [-config-dir /etc/lexa] [-api-scheme ""]
```

| Flag | Default | Meaning |
|---|---|---|
| `-budget` | `120s` | Total time budget. In `-commit` mode, retries happen until PASS or this elapses. Even a single (non-`-commit`) run is bounded by it — no check can hang past it. |
| `-commit` | `false` | Retry the full check set every 5s until it passes or `-budget` is exhausted — the Mender gate mode. Without it, exactly one attempt runs regardless of outcome (ad-hoc operator use). |
| `-config-dir` | `/etc/lexa` | Where `api.json`/`modbus.json`/`northbound.json`/`cloudlink.json` and the `commissioned` marker live. Override for tests or a non-standard install. |
| `-api-scheme` | `""` (auto) | Force `"http"` or `"https"` against lexa-api instead of probing https-then-http. |

Note: in `-commit` mode, the retry loop's fixed 5s interval means total
wall time can overshoot `-budget` by up to one interval in the worst case
(the loop only checks the budget *between* attempts, not while an attempt
or its retry sleep is in flight) — negligible at the default 120s/5s
ratio, called out here rather than silently hidden.

## JSON summary schema (stable, `v: 1`)

```json
{
  "v": 1,
  "ts": "2026-07-09T00:00:00Z",
  "budget_s": 120,
  "commit": true,
  "attempts": 1,
  "duration_s": 0.42,
  "pass": true,
  "checks": [
    {"name": "systemd", "status": "PASS", "detail": "7 units active"}
  ]
}
```

`checks` reflects the **final** attempt only; use `attempts` to see how
many retries happened. `detail` is omitted (not empty-string) when there's
nothing to add. Bumping `v` is a deliberate breaking-schema-change
decision (`cmd/healthcheck/json_test.go`'s `TestSummaryV1` pins the current
value so that isn't done by accident).

## Troubleshooting a rollback

1. `journalctl -u mender-client` (or wherever the on-device Mender log
   lands) around the `ArtifactCommit_Enter` timestamp shows this tool's
   stderr lines verbatim — read them top to bottom; the first `FAIL`
   line is usually the actual root cause, not necessarily the last one
   printed.
2. Re-run by hand for a live JSON summary:
   `lexa-healthcheck -budget 10s -config-dir /etc/lexa` (no `-commit`,
   short budget — a quick single pass).
3. `systemd`/`api` failures usually mean a service crash-looped past its
   `StartLimitBurst` (see the repo's V1RC FINDING A) — check
   `systemctl status <unit>` and `systemctl reset-failed <unit>`.
4. `northbound`/`modbus` FAILs on an otherwise-healthy box often mean
   `api.json`'s device list and `modbus.json`'s device list have drifted
   apart (see `cmd/healthcheck/check_modbus.go`'s doc) — `/status`'s
   devices map is seeded from `api.json`, not `modbus.json`.
