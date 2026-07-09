# DEVICE_ROADMAP execution units — parallel-agent decomposition

Prepared 2026-07-09. Source spec: `docs/DEVICE_ROADMAP.md` (§1–12,
TASK-082–099). Ground rules inherited verbatim: additive-only, bounded
touches to existing files, safety invariants preserved (§12 of the roadmap).

## Strategy

The decomposition is breadth-first off two thin schema units (bus types →
topic/version constants) that unblock every service in parallel; pure-package
units (`internal/spool`, engine setters, `CSIPPassthrough`, `lexa-proto`
sweep) have **zero** dependencies and start at t=0. Sequential chains exist
only where the same file must be touched in order (`internal/bus`,
`cmd/hub/main.go` wiring), and all ACL edits are concentrated in one unit
(1.3) so parallel agents never contend on the security boundary. Validation
(Phase 8) is integration-only and serialized on the shared bench.

## Task breakdown

| Unit | Name | Scope (roadmap §) | Size | Phase | Deps | Exit criteria | Mayhem scenario |
|---|---|---|---|---|---|---|---|
| 1.1 | Bus + journal schemas | §1.2, §3.6 | S | 1 | — | New types round-trip encode/decode; `Finite()` rejects NaN/Inf on every float-bearing type (table-driven); no field collides with `"v"` key (test); journal event constructors added append-only, `SchemaV` unchanged. ACL: none (types only) | — |
| 1.2 | Topic + version constants, `SupportedV` | §1.1, §1.3 | S | 1 | 1.1 | One `SupportedV` case per new topic (sweep test); `IntentTopic` builder; `PubQoS` untouched — assert QoS 1 default for all new topics. ACL: none | — |
| 1.3 | Deploy surface: ACL delta + unit file + configs | §1.4, §1.5, §2.9 | S | 1 | 1.2 | ACL stanzas match §1.4 exactly (incl. deliberate asymmetries: api cannot write forecasts; only hub writes `intent/result`/`hub/mode`); `lexa-cloudlink.service` (`Type=notify`, `Wants=mosquitto`, `StartLimit*` in `[Unit]`, `StateDirectory=lexa`); `configs/cloudlink.json` parses; deploy script provisions cloudlink broker cred. **This unit IS the ACL review gate** | — |
| 1.4 | `internal/spool` | §2.5 | M | 1 | — | Property tests: byte budget never exceeded; lowest-priority-first eviction; oldest-first drain; torn final record tolerated on open; crash between Peek and Commit redelivers; fsync only on segment close / 5 s interval. ACL: none | — |
| 1.5 | `lexa-healthcheck` + Mender state scripts | §8.1 | M | 1 | — | All 7 checks unit-tested against fake endpoints; uncommissioned unit accepts heartbeat `never`; `--commit` exit codes; `ArtifactCommit_Enter` script wired with ~120 s budget; never gates on WAN. ACL: none (loopback reads) | validated in 8.5 |
| 1.6 | Boot infra: migrate + factory mode + clock trust | §8.2, §9 | M | 1 | — | `schema_version` 0→1 migration: backup + atomic rename, down-migrate refused; factory config profiles boot clean; `factory-reset.sh` preserves `/etc/lexa/identity`; `After=time-sync.target` added to northbound/telemetry/cloudlink units; `commissioned` marker semantics documented per service. ACL: none | — |
| 2.1 | Cloudlink service skeleton | §2.1–2.3 | M | 2 | 1.2 (deploy: 1.3) | House `main()` ordering (logutil→journal→metrics→MQTT→watchdog); `enabled:false` idles publishing retained `CloudlinkStatus{connected:false}`; metrics `127.0.0.1:9106` with `"off"` disable; watchdog kick gated on `IsConnected()` AND spool-healthy. ACL: writes `lexa/cloudlink/status` (granted in 1.3) | — |
| 2.2 | Uplink collectors → spool | §2.4 | M | 2 | 2.1, 1.4 | Subscribes exactly the §1.4 read set (test enumerates topics); stream/priority mapping table pinned; re-frames without interpreting payloads; version-gate + `Finite()` arrive via `mqttutil.Subscribe`, never bypassed. ACL: reads per 1.3 | feeds 8.1 |
| 2.3 | Cloud session + batcher | §2.4 | M | 2 | 2.1, 1.4 | mTLS (`crypto/tls`, CGO off) against a client-cert test broker; gzip frames ≤ 96 KiB, oversize splits (test); PUBACK→`Commit`, failure→respool; per-frame `seq` monotonic; at-least-once contract pinned by test. ACL: none local | feeds 8.1 |
| 2.4 | Downlink validator → intents | §2.6 | M | 2 | 2.1 | Rejection table pinned: malformed / version / unknown-kind / non-finite / expired / origin-forgery / rate-limit; `intent_received` journaled before effect; publishes **only** `lexa/intent/{kind}` (ACL assert in test) | feeds 8.1 |
| 2.5 | Cloud cert monitor + diag bundle | §2.7, §2.8 | S | 2 | 2.1 (diag: 2.4) | Certmon pattern-copy: gauges + status field + WARN inside window; diag tar redacts `*_pass*`/keys (fixture-tree test), bounded size/time, request+outcome journaled | — |
| 3.1 | Engine setters + planner gates | §3.2, §3.3 | M | 3 | — | Five setters copy the `enqueue`/`setPlanIn`/`signalReplan` idiom (race detector clean, no new mutex); diurnal block bypassed only for a fresh external forecast — both paths pinned; reserve clamp ≥ config floor (test proves intents can only raise); EV-goal override; `LoadProfileKw` + `planStepLoad`; `ForecastSource()` accessor. Pure `internal/orchestrator` — no bus dep | — |
| 3.2 | Cost-model swap + tariff compile | §3.4, `TariffSpec` | M | 3 | 1.1 | `SwapCostModel` atomic (`-race` with ticking optimizer); `costModel()` substitution behavior-preserving (existing optimizer tests green, untouched); `TariffSpec`→`TOUCostModel` with DST tables extended from `costmodel_test.go`; CSIP `SetPrices` still wins via nil-slice fallback (test) | — |
| 3.3 | Hub intent adopter | §3.1 | M | 3 | 1.2, 3.1, 3.2 | Per-kind applied/clamped/rejected/expired/duplicate paths table-tested; retained-redelivery dedupe by ID (no journal spam); `chargenow` TTL revert restores standing EV goal (fake clock); `IntentResult` published with 1 s bound; journal events emitted. ACL: hub reads 7 kinds, writes `intent/result` (review vs 1.3) | intent-flood-rate-limit (run in 8.2) |
| 3.4 | Mode manager | §3.5 (manager + transitions) | M | 3 | 1.2, 3.3 | `EvaluateSafety` delegates to legacy **regardless of mode** (test — the load-bearing invariant); flip Wakes engine; retained `lexa/hub/mode` + boot re-seed order (retained ▸ hub.json ▸ optimizer) pinned; `mode_change` journaled with actor/origin/intent-ID | — |
| 3.5 | `CSIPPassthrough` + gateway EVSE policy | §3.5, §6 | M | 3 | — | Demand table tests: MaxLimW→ceiling, FixedW→setpoint, Connect fan-out, no-control→**explicit** restore + battery idle-0 (restore-is-a-write asserted); EVSE scheduled/full policies; no TOU/economics import (lint). Pure constraint pkg — parallel-safe | — |
| 3.6 | Gateway stack wiring + observability | §3.5, §3.7 | M | 3 | 3.1, 3.4, 3.5 | Gateway stack = BatterySafety+Export+GenLimit+ImportLimit+Passthrough (constructor test); plan log gains `mode`/`forecast_source`/`forecast_age_s` additively (v unchanged; `cmd/api/plan_test.go` still green); new hub metrics registered; `hub.json` `mode`/`forecast_max_age_s`/`gateway` keys parse with defaults. **R10 gate: lands only per constraint-stack P5 sequencing** | mode-flip-under-active-event (run in 8.3) |
| 4.1 | API HTTPS + strict auth + mDNS | §4.1, §4.2, §4.4 | M | 4 | 1.6 (marker for TXT; stubable) | Cert generated once, fingerprint stable across restarts; SANs = hostname + `<serial>.local` + LAN IPs; `tls:false` bench escape; `requireBearerStrict` on writes (empty token = reads only); zeroconf Register/SetText/Shutdown lifecycle; loopback `/healthz` watchdog probe works under TLS | — |
| 4.2 | API read routes | §4.3 | M | 4 | 1.2 | `/site` `/devices` `/telemetry/recent` `/mode` + `/scan` projections off `stateStore` + retained topics; bounded ring buffer; CORS/OPTIONS match existing handlers; existing `/status` contract unchanged (pinned). ACL: api read grants (1.3) | — |
| 4.3 | `POST /intent` + resultWaiter | §4.3 | M | 4 | 1.2 | Kind whitelist excludes `solarforecast`/`loadprofile`; stamps `Origin:"app"` + uuid + `IssuedAt`; 200-with-hub-outcome / 202-pending at 3 s; resultWaiter muxes by ID (goroutine-leak test); 64 KiB body cap. ACL: api writes 5 intent kinds + `scan/request`, reads `intent/result` (review vs 1.3) | e2e in 8.2 |
| 4.4 | Commissioning config-write path | §4.5 | M | 4 | 1.6, 4.1 | 403 when commissioned with no armed window; schema validation rejects bad enums/paths (incl. `mqtt_pass_file` outside `/etc/lexa/mqtt/`); staged-write + rename (kill-mid-write leaves valid file); sudoers fragment enumerates exactly the six units; `config_write` journaled with before/after hashes; **security checklist sign-off required** | exercised in 8.4 |
| 5.1 | `lexa-proto` identity reader + sweep (paired PR) | §5.1 | L | 5 | — | `ReadCommon` decodes model 1 from register fixtures (NUL padding, missing model); `SweepTCP`/`SweepRTU` vs mock transports (timeouts, partial responders, Modbus-but-not-SunSpec); RTU strictly sequential; **both repos' `proto.pin` bumped + both `vendor/` trees regenerated in the same session** (`check-proto-pin.sh` green); bench sims answer the sweep. Single agent owns both repos | — |
| 5.2 | Modbus scan controller | §5.2 | M | 5 | 1.2, 5.1 | Refused unless zero devices configured or all reconcilers `off` (table test); `ScanStatus` progress stream; classification table (701/10x inverter, 802/12x battery, 20x meter, unknowns); retained `ScanResult`; `scan_run` journaled; metrics registered. ACL: modbus reads `scan/request`, writes `scan/status|result` (review vs 1.3) | scan-during-live-control-refused (run in 8.4) |
| 6.1 | OCPP pending-station surface | §6 | S | 6 | 1.2 | Unknown station → retained `PendingStations` entry (vendor/model from BootNotification when seen); stays measurement-only — no shell, existing behavior pinned by test; approved/departed entries dropped on rebuild. ACL: ocpp writes `lexa/ocpp/pending` (review vs 1.3) | approval path in 8.4 |
| 7.1 | `lexactl` | §7 | S | 7 | 4.2, 4.3 | Every subcommand round-trips against a live api (integration); fingerprint check against on-disk cert; token from `/etc/lexa/api-secret`; non-zero exit on failure; no path bypasses the intent journal (by construction — HTTP only) | — |
| 8.1 | Cloudlink bench trio | §2.10 | M | 8 | 2.2, 2.3, 2.4 | `wan-outage-spool-drain`: P2 evicted first, P0 intact, oldest-first drain, **zero safety/compliance deltas**; `cloud-cmd-forgery-rejected`; `stale-intent-expired` — all green on the dev kit | those three |
| 8.2 | Intent end-to-end | §3.1, §4.3, §2.6 | M | 8 | 2.4, 3.3, 4.3 | App-path (api) and cloud-path (cloudlink) intents produce identical hub outcomes + journal trail + uplinked result; `intent-flood-rate-limit` green | intent-flood-rate-limit |
| 8.3 | Mode-flip scenarios | §3.5 | M | 8 | 3.6 | `mode-flip-under-active-event`: no breach beyond oracle in either direction; `mode-flip-under-fault`: device dark during flip reasserts correctly on reconnect; restart preserves mode via retained re-seed | mode-flip-under-active-event, mode-flip-under-fault |
| 8.4 | Commissioning end-to-end | §4.5, §5, §6 | M | 8 | 4.4, 5.2, 6.1 | Sealed-box dry run on bench: scan → config-write → restart → devices live → pending EVSE approved → `commissioned` marker set; `scan-during-live-control-refused` green; every config write journaled | scan-during-live-control-refused |
| 8.5 | OTA rollback validation | §8.1, §8.2 | M | 8 | 1.5, 1.6 | Dev kit: deliberately broken service auto-rolls back via healthcheck commit gate; schema migration runs on new-slot first boot; scenario documented as repeatable | ota-broken-service-rollback |

Sizes: S ≤ 3 days · M ≈ 1–2 weeks · L ≈ 2–4 weeks (agent-relative effort).

## Dependency graph

```
t=0 parallel starts:  1.1   1.4   1.5   1.6   3.1   3.5   5.1   4.1*
                       │     (*4.1's TXT claimed= flag stubs until 1.6)
1.1 ─► 1.2 ─► 1.3
       │
       ├────────► 2.1 ─┬─(+1.4)─► 2.2 ─┐
       │               ├─(+1.4)─► 2.3 ─┼──► 8.1
       │               ├────────► 2.4 ─┘        └► (2.4 also feeds 8.2)
       │               └────────► 2.5
       │
       ├─(+3.1, 3.2)─► 3.3 ─► 3.4 ─┐
       │   (3.2 ◄ 1.1)             ├─(+3.1, 3.5)─► 3.6 ─► 8.3
       │                           │
       ├────────► 4.2 ─┐           │
       ├────────► 4.3 ─┼─► 7.1     └────► (3.3 + 4.3 + 2.4) ─► 8.2
       ├────────► 6.1 ─┐
       └─(+5.1)─► 5.2 ─┼─► 8.4 ◄── 4.4 ◄── (1.6 + 4.1)
                       │
1.5 + 1.6 ─────────────┴──────────► 8.5
```

**Critical path (6 units):** `1.1 → 1.2 → 3.3 → 3.4 → 3.6 → 8.3`, with
3.1/3.2/3.5 running in parallel alongside the prefix so 3.3/3.6 are never
input-starved. Everything else hangs off 1.2 at depth ≤ 4.

## Suggested claim order (parallel batches)

- **Batch A (t=0, 8 agents):** 1.1, 1.4, 1.5, 1.6, 3.1, 3.5, 5.1, 4.1
- **Batch B (1.1 lands):** 1.2, 3.2
- **Batch C (1.2 lands):** 1.3, 2.1, 4.2, 4.3, 6.1
- **Batch D:** 2.2, 2.3, 2.4, 2.5 (2.1+1.4 land) · 3.3 (3.1+3.2 land) · 5.2 (5.1 lands)
- **Batch E:** 3.4 · 4.4 (1.6+4.1 land) · 7.1
- **Batch F:** 3.6 (R10 sequencing gate applies)
- **Batch G (bench, serialized):** 8.5 first (proves OTA before features ride it), then 8.1 → 8.2 → 8.4 → 8.3

## Orchestration cautions

- **Merge-conflict hotspots are already serialized by the DAG:** `internal/bus`
  files (1.1→1.2 only), `cmd/hub/main.go` wiring (3.3→3.4→3.6 only),
  `engine.go` (3.1 is its single owner). Do not split these further.
- **1.3 is the single ACL owner.** Any later unit believing it needs an ACL
  line must route the change through a 1.3 amendment, not its own edit — the
  ACL is an authorization boundary reviewed once, not a merge target.
- **5.1 must be one agent, both repos, one session** (MTR-4 paired-PR
  lockstep; CI enforces identical `proto.pin`).
- **Phase 8 shares the physical bench** — units are serialized; everything
  before it is bench-independent (unit/property tests + mocks).
- **3.6 carries the R10 gate:** it consumes the constraint stack; confirm the
  P5 flip sequencing before claiming it, and keep the naive-passthrough
  fallback (scoped in the task file) as the de-risk if stack actuation
  validation slips.
