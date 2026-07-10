# Extension Campaign — Progress Board

*Ecosystem-interface extension program. Spec: `docs/DEVICE_ROADMAP.md` (the
implementation contract) · unit decomposition: `docs/DEVICE_ROADMAP_UNITS.md`
· ecosystem context: `docs/ECOSYSTEM_ROADMAP.md`. Campaign modeled on
`csip-tls-test/docs/refactor/` (00_MASTER_INDEX / 05_ENGINEERING_PRINCIPLES /
task-file discipline). Started 2026-07-09.*

## Working agreements (inherited + adapted)

- All refactor-campaign engineering principles apply verbatim
  (`csip-tls-test/docs/refactor/05_ENGINEERING_PRINCIPLES.md`): dependency
  direction, one-owner-per-concept, CGo containment, fail-closed control
  plane, one-writer/snapshot-reads concurrency, wall-clock-denominated
  thresholds, plant-model config discipline, authn-with-new-surface,
  invariant-and-outcome tests, structured rate-conscious logging.
- **Extension, not refactor:** every change additive; bounded touches to
  existing files; the authoritative control paths (legacy optimizer cascade,
  reconcilers, Tier-0/1 safety, northbound compliance) are never modified.
- **Radioactive zone still applies** (`internal/orchestrator/*`,
  reconnect/interlock paths): one unit per merge, never batched, every
  existing test green unmodified.
- **Bench is occupied ~24 h (until ≈2026-07-10):** all units gate on unit
  suites + `-race` now; hardware/Mayhem validation enters the
  **bench-deferred queue** below and is executed when the bench frees.
  Nothing that requires a deploy (proto pin bumps, ACL install, csip-tls-test
  edits) happens while the bench runs.
- Coding agents do NOT commit; the principal reviews diffs, runs the suite,
  and commits per unit with house-style messages. Symbol names are
  authoritative; line numbers are hints re-verified at execution time.
- No new module dependencies without an explicit decision; no `go.mod`/
  `go.sum` edits by coding agents.

## Status board

Status: `todo` · `in-flight(W#)` · `review` · `rework` · `done(<commit>)` ·
`blocked`. Unit definitions: `docs/DEVICE_ROADMAP_UNITS.md`.

| Unit | Name | Size | Model | Status |
|---|---|---|---|---|
| 1.1+1.2 | Bus + journal schemas, topics/versions/SupportedV | S+S | sonnet | **done(52e0091)** |
| 1.3 | Deploy surface: ACL + cloudlink broker creds | S | sonnet | **done(1d71068 + hotfix)** |
| 1.4 | `internal/spool` | M | opus | **done(7013940)** |
| 1.5 | `lexa-healthcheck` + Mender scripts | M | sonnet | **done(73decd0)** — 8.5 rollback proof on bench queue |
| 1.6 | Boot infra: migrate + factory + clock trust | M | sonnet | **done(70989b0)** |
| 1.7 | Uncommissioned-idle gates: northbound/telemetry cert eager-load fix | S | sonnet | **done(9d60d08)** |
| 2.1 | Cloudlink skeleton (owns cloudlink.json + unit file) | M | sonnet | **done(f9909c4)** |
| 2.2 | Uplink collectors → spool | M | opus | **done(9e1fed1)** |
| 2.3 | Cloud session + batcher | M | opus | **done(9e1fed1)** |
| 2.4 | Downlink validator → intents | M | sonnet | **done(e38366d)** |
| 2.5 | Cert monitor + diag bundle | S | sonnet | **done(e38366d)** |
| 3.1 | Engine setters + planner gates | M | opus | **done(192ed82)** — RADIOACTIVE; Mayhem gate deferred to bench queue |
| 3.2 | Cost-model swap + tariff compile | M | opus | **done(de0ed85)** — RADIOACTIVE |
| 3.3 | Hub intent adopter | M | sonnet | **done(1b6c7d6)** |
| 3.4 | Mode manager | M | opus | **done(f3ea88d)** |
| 3.5 | `CSIPPassthrough` + EVSE policy | M | sonnet | **done(bbe001d)** — unwired until 3.6 |
| 3.6 | Gateway wiring + observability + AxisConnect fix | M | opus | **done(82a13d4)** |
| 4.1 | API HTTPS + strict auth + mDNS | M | sonnet | **done(c25d1f5)** — incl. live watchdog-probe bugfix |
| 4.2 | API read routes | M | sonnet | **done(see 4.3)** |
| 4.3 | `POST /intent` + resultWaiter | M | sonnet | **done(w 4.2)** |
| 4.4 | Commissioning config-write | M | sonnet | **done(047ae2d)** |
| 5.1 | `lexa-proto` identity + sweep | L | opus | **done(lexa-proto 6922865)** — pin bump deferred (queue item 3) |
| 5.2 | Modbus scan controller | M | sonnet | **done(see log)** |
| 6.1 | OCPP pending + uncommissioned-idle gate | S/M | sonnet | **done(15c62bf)** |
| 4.5 | Privilege-free commissioning restart trigger (.path unit) | S | sonnet | **done(8d6ba15)** |
| 6.2 | Reconciler-side Connect execution (solar OpModConnect, EVSE 0A-on-disconnect) | S | sonnet | **done(b815a4f)** |
| 7.1 | `lexactl` | S | sonnet | **done(7a53d05)** |
| 8.1–8.5 | Bench validation units | M×5 | — | blocked (bench occupied) |

## Scope amendments

- **6.1 amended (2026-07-09):** in addition to the pending-station surface,
  lexa-ocpp gains an **uncommissioned-idle gate**: `stations` empty AND
  `bench:false` ⇒ do not bind the CSMS listener (idle cleanly, log once)
  instead of refusing at the new WS-1 SP2 config gate. Required so the
  fail-closed factory profile (`configs/factory/ocpp.json`: SP2 blank,
  bench:false) is loadable on uncommissioned units without shipping an open
  `ws://` listener. Until 6.1 lands, that factory profile is target-state
  only (documented in configs/factory/README.md).
- **Upstream moved under W1 (2026-07-09):** commit `7e40292` landed WS-1
  (OCPP SP2 fail-closed config gate + lexa-api loopback/token bind gate +
  MQTT ACL default-on in deploy) and WS-8 (`hub.json` `tariff_zone` startup
  assertion, `cmd/hub/tariffzone.go`), plus `35f0952` (shadow panic latch +
  per-axis counters + Tier-1 safety shadow-diff). Course correction sent to
  unit 1.6 (factory profiles); 4.1 reads the current tree and coexists;
  3.2's tariff intent will interact with WS-8's `tariff_zone` (note for its
  brief); ACL-default-on strengthens the case that 1.3's ACL delta must land
  before any cloudlink bench deploy.

## Wave log

- **W2 (2026-07-09):** launched 1.3 (ACL+cloudlink creds), 1.7 (NB/telemetry
  idle gates), 2.1 (cloudlink skeleton — owns configs/cloudlink.json + unit
  file, moved from 1.3 to keep one owner), 3.2 (tariff compile + SwapCostModel,
  opus — optimizer.go radioactive), 4.2+4.3 combined (one owner of cmd/api),
  6.1 (ocpp pending + uncommissioned-idle gate, amended scope). Disjoint
  ownership: acl+deploy / northbound+telemetry / cmd/cloudlink / optimizer+
  cmd-hub-tariff / cmd/api / cmd/ocpp. Holding for deps: 2.2–2.5 (need 1.4
  spool + 2.1), 3.3 (needs 3.2), 3.4/3.6 (need 3.3), 4.4 (needs 1.6+4.1 ✓ —
  next wave), 5.2 (needs vendor regen — bench queue), 7.1 (needs 4.2/4.3).

- **W1 (2026-07-09):** launched 1.1+1.2, 1.4, 1.5, 1.6, 3.1, 3.5, 4.1, 5.1 —
  eight agents, disjoint path ownership (bus+journal / spool / healthcheck /
  boot-infra+systemd / orchestrator core / constraint pkg / cmd/api /
  lexa-proto). Known deferred conflict: `configs/api.json` owned by 4.1 this
  wave; 1.6 adds `schema_version` to every config EXCEPT api.json (principal
  reconciles at integration).

## Bench-deferred validation queue

Executed when the bench frees (order matters — OTA proof first):

1. 8.5 OTA rollback (needs 1.5, 1.6 + meta-lexa/mender integration session)
2. FINDING A/D re-confirmation deploy (pre-existing V1RC item — piggyback)
3. 5.1b: lexa-proto pin bump + vendor regen BOTH repos + sim scan fixtures
   (paired session, MTR-4)
4. ACL install + cloudlink broker cred provisioning (1.3's deploy half)
5. 8.1 → 8.2 → 8.4 → 8.3 scenario waves per DEVICE_ROADMAP_UNITS Batch G

## Review findings log

*(appended per unit at review; disposition required before `done`)*

- **1.1+1.2 (done 52e0091):** clean pass — spec-verbatim types, house-style
  docs, 0 deletions in modified files. Two downstream contracts set here that
  later unit briefs MUST honor: (a) journal constructors are bus-decoupled
  scalar-field pairs (`NewModeChange(from,to,actor,origin,intentID)` +
  `NewModeChangeEvent(svc,p)`), NOT the flattened single-call sketch in
  DEVICE_ROADMAP §3.5 — units 3.3/3.4/4.4 adapt to this; (b) `ScanRun`
  journal payload is `{ID, Phase("refused"|"done"), DevicesFound, Detail}` —
  unit 5.2 uses exactly this. `SupportedV`'s intent arm returns 1 for
  unknown `lexa/intent/*` topics (consistent with the global default).
- **3.1 (done 192ed82):** near-clean pass; one CONFIRMED principal fix —
  `resampleForecast` clamped negative offsets (a spec error in the brief,
  faithfully implemented): future-start forecasts were start-aligned, shifting
  the solar curve early and mispricing peak timing. Fixed to time-aligned
  leading-zero-fill; test re-pinned. Sound agent decisions kept: reserve
  clamps against the CFG-DERIVED floor (default 20%, matching the battery
  loop — raw cfg field would under-run it); diurnal high-water observation
  hoisted unconditional; defensive slice copies (SetPrices doesn't copy,
  intent handlers reuse decode buffers — documented). **Open for 3.3:**
  `EVGoalIntent.CapacityKwh` has NO landing point — EV-goal override is inert
  when planner cfg has no `ev_capacity_kwh`; 3.3 must either extend `EVGoal`
  with an optional capacity override (small additive engine change) or
  document user-stated capacity as commissioning-config-only. **Open for
  3.6/hub wiring:** stale-forecast edge-triggered alarm lives in cmd/hub's
  planObserver (deliberately NOT a per-tick log in the radioactive package);
  wire it when plan-log fields land. Mayhem campaign for this unit rides the
  bench-deferred queue (radioactive-zone rule).
- **3.5 (done bbe001d):** clean pass — reused the package's existing `apW`
  converter and NaN-ceiling restore convention (better than the brief's
  numeric sketch); FixedW sign traced to `applyFixedDispatchRule` +
  `economics.go` twins. **CONFIRMED gap for 3.6 (blocking gateway
  actuation):** `stack.go`'s `emitCommands` maps `AxisConnect` to
  `BatteryCommand.Connect` ONLY — solar/EVSE cease-to-energize demands are
  dropped on the floor. 3.6 must extend `emitCommands` (additive case arms)
  or explicitly ship the passthrough-only fallback without Connect fan-out.
  Minor accepted debt (documented in-code): `GatewayEVSEPolicy.WithDefaults`
  zero-sentinel can't express a window starting/ending exactly at midnight
  (plant-model convention carried over).
- **5.1 (done lexa-proto 6922865):** clean pass; four principal rulings —
  (1) `modbus.NewSerialTransport` scope expansion ACCEPTED (agent verified
  simonvetter/modbus never URL-parses baud; without it `bauds` is a silent
  no-op); (2) `SweepTCP/RTU` names kept despite TASK-053 `Sweep*` crowding
  (filename `bussweep.go` avoids the clobber); (3) `rtuQuietGap` package-var
  test seam kept; (4) proto empty-TCP unit default is `{1}` — **unit 5.2 must
  pass the bus-contract default `{1,2,3,126}` explicitly** (defaults belong
  at the caller). NO pin bump: lexa-hub/csip-tls-test still pin the old SHA;
  5.2 develops against a `go.work` checkout; paired lockstep session is
  bench-queue item 3.
- **1.5 (done 73decd0):** clean pass — gather/eval split per check, 63
  subtests, commit-script contract documented in the script header, smoke-
  tested end-to-end. Principal applied the Makefile/CI wiring (SERVICES +
  build-arm64 + two CI build lines; `make bin/lexa-healthcheck` verified).
  Accepted debt (in-code + HEALTHCHECK.md): decode structs are hand-copied
  subsets of service schemas (package-main import wall) — any /status or
  config field rename must update cmd/healthcheck too; -commit can overshoot
  budget by ≤1 retry interval. meta-lexa must chmod +x the state scripts at
  install (git bit not authoritative on Yocto recipe fetch).
- **4.1 (done c25d1f5):** clean pass + two field-critical catches. (a)
  **Watchdog-probe bug, live on WS-1 defaults**: healthz URL string-concat
  malformed for host-full listen_addr — api watchdog kicks silently starving;
  fixed via SplitHostPort. **Bench-queue addendum: verify api watchdog kicks
  land on the deployed config pre/post-fix** (WS-1's default change
  interacted with TASK-008-era concat; the WS-1 session should know). (b)
  systemd `RestrictAddressFamilies` needed `AF_NETLINK` for
  net.Interfaces()/zeroconf — mDNS would hard-fail under sandboxing in the
  field. Contracts set: fingerprint = lowercase hex sha256 of leaf DER (no
  colons — lexactl displays as-is or pretty-prints, its call);
  `requireBearerStrict` pinned for 4.3; no build-stamped version var exists
  anywhere yet ("dev" placeholder in mDNS TXT `fw=`) — add ldflags stamping
  when release-artifact work lands. Spool flake seen by this agent
  (`TestNoFsyncPerAppend`) — triage when 1.4 reports.
- **1.6 (done 70989b0):** clean pass; WS-1/WS-8 course correction applied
  in full (factory ocpp in TARGET state — unloadable until 6.1's idle gate;
  documented). **NEW CONFIRMED GAP → unit 1.7 (S, Wave 2):** northbound +
  telemetry eagerly load CA/cert/key at construction regardless of server
  config — factory-fresh/reset units crash-loop into StartLimit instead of
  idling cleanly (FINDING-A signature). Fix = bounded gate: no server
  configured ⇒ skip fetcher construction, idle loop, log once. Also open:
  /etc/lexa/api-secret manufacturing provisioning is not implemented
  anywhere (assumed by factory api.json); factory-reset does NOT rotate the
  api secret (resale concern — backlog); meta-lexa must populate
  /usr/share/lexa/factory/ from configs/factory/. Bench-queue addendum:
  verify busybox `timeout`(1) exists in the DEY image (factory-reset uses
  it defensively around systemctl).
- **1.4 (done 7013940):** clean pass, WAVE 1 COMPLETE. Triple -race run
  deterministic (the TestNoFsyncPerAppend flake other agents saw was mid-dev
  state, resolved by the syncFn/now seams). Agent-caught beyond-spec guard:
  lower-priority append never displaces higher-priority data (drop+count
  instead of budget breach). Contracts for 2.2/2.3: Commit(n) consumes from
  the class of the most recent Peek (lastPeekClass — robust against a
  higher-priority append landing between Peek and Commit); Metrics fields
  are typed nil-safe {Bytes, Appends, Commits, Drops, DropBytes, Errors};
  cursor files (≤32B×3) live outside the data budget; Record.Priority
  clamped to [0,2].
- **1.7 (done 9d60d08):** clean pass — gate keys off server config only
  (configured-but-broken still crashes loudly, test-pinned); idle path keeps
  MQTT/metrics/watchdog alive (healthcheck #1 passes on factory units);
  certmon/rotation skipped when idle (nothing to report on). Factory README
  Known-gaps #1 marked resolved. Factory-reset's "services land failed"
  caveat is now obsolete — bench queue: verify factory-profile boot on the
  dev kit when free.
- **1.3 (done 1d71068 + hotfix):** clean pass; 21 topic strings grep-verified
  against topics.go; deliberate asymmetries commented in-file. **CONFIRMED
  pre-existing ACL bug found by the agent's sweep → separate hotfix commit:
  the whole lexa/reconcile/+/+/report family had NO grants since TASK-031**
  (worked only via dev-kit allow_anonymous / pre-WS-1 opt-in enforcement).
  Grants added per-class per-writer + hub wildcard read. Bench-queue item 4
  gains an acceptance check: reconcile verdicts flow under enforcement.
  Rulings: cloudlink.pass filename kept (roadmap-verbatim); 2.1 owns the
  cloudlink unit file/config.
- **3.2 (done de0ed85):** radioactive diff surgically minimal (1 import,
  1 field + 2 methods, 1 substitution site); agent closed a TOCTOU by
  caching costModel() per pass. Honest-mapping rejections (day-variant
  rates/labels, non-zero export, midnight wrap, non-USD) all test-pinned —
  the CLOUD/app layer must pre-normalize tariffs to these constraints
  (note for the app/tariff-editor spec). peak=top-tier-only is a documented
  judgment call, revisit if shoulder tiers should discharge.
- **2.1 (done f9909c4):** clean pass; unit-file conventions all present
  (verified grep: StartLimit[Unit]/Wants/time-sync/StateDirectory/
  ReadOnlyPaths/WatchdogSec). Seams pinned for 2.2/2.3 (cloudSession,
  spoolStats — spool's methods already match). Principal wired
  Makefile/CI. Factory cloudlink.json lacks journal block — defaulted by
  loadConfig, pinned by fixture test.

- **W3 (2026-07-09):** launched 3.3 (hub intent adopter — sonnet; principal
  rulings baked in: EVGoalIntent.CapacityKwh is validation-only in v1, mode
  kind deferred to 3.4, chargenow = accelerate standing goal else reject)
  and 2.2+2.3 combined (cloudlink uplink path — opus; principal decision:
  enabled:false means collectors do NOT run and spool stays empty — local-
  only boxes never consume flash for a cloud that never comes); then 4.4
  (config-write, cmd/api freed by 4.2+4.3 merge) and 7.1 (lexactl, new dir)
  as they unblocked. Disjoint:
  cmd/hub vs cmd/cloudlink; W2 stragglers own cmd/api and cmd/ocpp.
- **6.1 (done 15c62bf):** truth table pinned (8 combos; dangerous WS-1 cases
  unchanged); factory ocpp.json acceptance test green; README gap #2
  resolved. One pre-existing test superseded (premise negated by the
  amendment) — principal-approved, documented in the commit.
- **4.2+4.3 (done, same commit as this entry's sibling):** 98 subtests;
  ACL surface cross-checked against 1.3 stanzas exactly; site_cache nested
  (not flattened) — confirm against wizard client when app work starts;
  /config/{service} deliberately deferred to 4.4. tariff_zone omitted from
  /site (api has no source) — 4.4/commissioning must expose it via hub
  config instead.
- **3.3 (done 1b6c7d6):** clean pass; three rulings resolved as implemented
  (SourceTs staleness anchor; chargenow-without-standing-goal rejected —
  callers post evgoal first; reserve clamp unobservable → "applied" only,
  backlog: engine accessor to surface the clamp). The intent surface is now
  LIVE end-to-end: app → api POST /intent → bus → hub adopter → engine →
  IntentResult → api/cloudlink. Remaining intent kind: mode (3.4).
- **2.2+2.3 (done 9e1fed1):** clean pass. Approved decisions: RawMessage
  through mqttutil (reconnect-replay + version gate — bare subscribe goes
  deaf after broker restart); seq-bump BEFORE Commit (crash ⇒ benign dup,
  never dedup-drop loss); split-loop termination proven; enabled:false =
  zero flash. Mapping authority = §1.4 ACL (roadmap §2.4 table superseded —
  hub/mode→health, scan/result→events). Metric name
  lexa_cloudlink_cloud_reconnects_total approved.
- **4.4 (done 047ae2d):** clean pass + one REAL deployment blocker
  surfaced: lexa-api.service hardening vs the write/restart path. Principal
  ruling: ReadWritePaths=/etc/lexa added; **NoNewPrivileges KEPT** (never
  weaken the LAN-exposed service) ⇒ sudo restart blocked on product units;
  honest {restarted:false} degradation until **NEW unit 4.5 (S): privilege-
  free restart trigger** (root-owned systemd .path unit watching a request
  file; api writes request, oneshot restarts from a closed allowlist,
  result file polled). go:embed schema package deviation approved (schema
  can never drift from binary). api-secret rotation = restart-required
  (pinned by test).
- **7.1 (done 7a53d05):** clean pass; two self-caught CLI bugs (dispatch
  order, flag-after-positional) fixed pre-ship. Rulings: exit codes 1=value/
  API 2=usage approved; /mode 503-unknown = success approved. Principal
  wired Makefile (explicit lexactl rule — the lexa-% pattern claim in the
  brief was wrong) + CI. Binary ships as /usr/local/sbin/lexactl.
- **3.4 (done f3ea88d):** safety mutation-check is exemplary (sentinel
  provenance + neither-author-ran assertion). One CONFIRMED principal fix:
  safety delegate = the shadow wrapper when constraint_shadow is on (raw-opt
  wiring would have silently disconnected WS-5.3's Tier-1 safety shadow-diff
  during P5 soaks; wrapper returns legacy plan unmodified by contract so
  mode-invariance holds through it). Retained ModeIntent + ModeStatus boot
  interplay analyzed benign (converges, duplicate no-op). configs/hub.json
  mode/gateway example keys → fold into 3.6. Gateway mode ships functional
  for solar-ceiling/battery-setpoint/EVSE-current; AxisConnect gap → 3.6.
- **4.5 (done 8d6ba15):** clean pass — api-last ordering with durable-result-
  first, reqAt staleness filter, sudoers deleted pre-deploy. NoNewPrivileges
  preserved on lexa-api. Bench-queue addenda: (a) deploy-hub-pi.sh needs a
  follow-up pass (stage .path/.service/apply script, drop any sudoers
  staging); (b) the script's timeout-124 path is untested in Go (30s real
  time) — verify once on hardware.
- **3.6 (done 82a13d4):** clean pass + bonus fix (pre-existing emitCommands
  bug: solar/EVSE connect spuriously emitted battery commands). Stale docs
  in constraint.go/passthrough.go updated by principal. **NEW unit 6.2 (S):
  reconciler-side Connect execution** — solarFieldsToControl must emit
  OpModConnect; ocpp applyActionLocked must honor Connect (0A on
  disconnect); until then gateway cease-to-energize for solar/EVSE rides
  the compliance tier's 0W/0A limits (documented in-code). Gateway mode
  default-off; 8.3 bench scenarios queued.
- **2.4+2.5 (done e38366d):** clean pass (one transient API-error mid-run,
  resumed without loss). Rulings: rejection audit = counter + rate-limited
  WARN only in v1 (journal-per-reject = flash hazard under forgery spam —
  **add to pen-test scope**: forgery attempts are not on the durable audit
  trail); chargenow TTLS=0 composition (downlink passes, hub adopter
  rejects) approved; diag over-redaction approved; new
  lexa_cloudlink_intent_pub_fail_total series approved (accepted-vs-
  delivered split). Cloud command service spec note: mint diag request IDs.
  Cloudlink is now FEATURE-COMPLETE (2.1–2.5): uplink, downlink, certmon,
  diag — the seventh service is whole.
- **6.2 (done b815a4f) — CODE PHASE COMPLETE:** final code unit. Agent-found
  latent hazard neutralized: EVSE docs have carried Connect=true since
  TASK-030; a naive per-field implementation would have wedged the
  completeness gate and silently disabled ev-accept-but-ignore for every
  station (masked by Connect-omitting fixtures). Bench-queue addendum:
  re-run the ev-accept-but-ignore scenario to confirm NonConverged
  detection fires on the new fold path. Backlog: wire Device.Status()
  (dead code today) through the registry for an honest solar connect
  readback; solar over-ceiling divergence detection pauses while Connect
  is expressed (battery-pattern tradeoff, documented).

## CODE PHASE CLOSED — 2026-07-09

27 units + 1 security hotfix merged (26 lexa-hub commits + 1 lexa-proto).
Remaining: 5.2 (needs proto pin-bump — bench queue item 3), units 8.1–8.5
+ all bench-queue items (hardware window). Principal review yield across
the campaign: 2 spec bugs fixed (forecast time-alignment, resample clamp);
4 live/latent bugs found (api watchdog probe, AF_NETLINK sandbox,
reconcile-report ACL hole, EVSE completeness wedge); 2 crash-loop classes
eliminated (factory certs, ocpp SP2 gate); WS-5.3 telemetry preserved
against a faithful-but-wrong brief instruction; every downstream contract
logged here at review time.

## Integration day — 2026-07-10 (bench free)

- **Merge f3059ce**: refactor-endgame (R4 flips ACTIVE, 26 commits) ×
  extension campaign (24 commits). 12 conflicts, zero markers, no test
  weakened either side; opus-driven, principal-reviewed (composition chain
  FIX-F wrapper → modeManager → engine verified line-level). Gates: 34/34
  packages -race. Campaign docs recovered from the extension-wip salvage
  branch (cherry-pick fec7432; stray 11MB binary stripped).
- **Deployed to dev kit 69.0.0.2**: all 10 merged ARM64 binaries (unified
  vintage), new units (cloudlink, migrate, commission.path/.service),
  commission-apply script; configs migrated to schema_version 1 on-device
  (backups /root/lexa-cfg-bak-*, .pre-v0 files); flipped constraint_modes
  PRESERVED (ACTIVE banner confirmed on the merged binary); api tls:false
  (bench); station-ID mismatch cs-001→evse-001 FIXED (0 "no EVSE actuator"
  lines on the new process — was every-tick before).
- **Hardware validation (smoke)**: lexa-healthcheck EXIT=0 first hardware
  run (clock via RTC weak fallback — NTPSynchronized=no on this image, note
  for chrony config); cloudlink first boot clean (local-only, retained
  status on broker); lexactl status/mode/reserve/ev-goal round-trips all
  `applied`; ocpp pending doc empty (station configured); dispatch journal
  shows active-stack actuation with Connect folds.
- **Known cosmetic gap**: `mode: unknown` until the first mode flip — the
  modeManager publishes retained ModeStatus only in request(); add a boot
  publish after SealBoot (small fix, post-campaign).
- **IN PROGRESS**: full FAST Mayhem campaign on the merged build (the
  radioactive-merge gate) vs the endgame baseline.
- **CAMPAIGN GATE PASS (2026-07-10 14:50 EDT)**: full FAST Mayhem on the
  merged build — 43P/18D/0F/0B/1I, aggregate-identical to the flip-4
  baseline; 4 swaps all known flappers (accepted signatures); zero crashes/
  watchdog/unintended restarts; per-axis legacy-override-dropped counters
  live on all five axes under fault, active_fallback=0, panic latch 0.
  Report: csip-tls-test/qa-mayhem-20260710-145010.md. Mode boot-publish fix
  (6a67f9c) deployed post-gate and verified (retained ModeStatus at boot).
  Operational note: the disk-full scenario vacuums ALL archived journald —
  pull forensic journals before it runs, or exclude it when journal
  evidence matters. Remaining on the queue: proto pin-bump paired session
  (unblocks 5.2), 8.x scenario authoring, OTA/Mender rollback proof
  (needs meta-lexa integration), ev-accept-but-ignore held D in-gate ✓.

## Extension scenarios — HARDWARE VALIDATED (2026-07-10, dev kit)

Four new Mayhem scenarios (csip-tls-test 468b73f) run solo on 69.0.0.2 — ALL PASS:
- mode-flip-under-active-event (8.3): export cap held across optimizer→gateway
  →optimizer, heartbeat never stalled, both flips observed. **R10 gate cleared
  on hardware** — gateway mode under a live utility cap.
- mode-flip-under-fault (8.3): INV-SOC held through fault + both flips;
  reassert-on-reconnect evidenced. Mode-invariant Tier-1 safety proven.
- scan-during-live-control-refused (8.4): refused in 1s, control undisturbed.
- intent-flood-rate-limit (8.2): 49/50 results (1 tick-coalesced, oracle-pass
  ≥80%), 72 journald lines vs 300 budget, 0 restarts.

Field-bug caught during validation: api.json flipped to tls:true on the dev
kit (cert regenerated 15:24 by ensureServerCert on some api restart) —
blinded the HTTP bench toolchain (dashboard/mayhem/lexactl all speak HTTP).
Fixed: tls:false (the documented bench escape). HTTPS listener SEPARATELY
proven on hardware first (openssl: CN=lexa-ccimx93-dvk, valid 2026→2036,
clean handshake) — unit 4.1 TLS path validated on the device, not just in
unit tests. Watch item RESOLVED: .pre-v0 backup proves tls:false at deploy & now; lexa-api does NOT rewrite its own config (ensureServerCert writes /var/lib/lexa/api/, not /etc/lexa/api.json) and 0 config_write journal events — no recurring path. The transient tls:true at a 16:24 restart is unreconstructable (disk-full vacuumed pre-14:08 journal); one-off, config stable across restarts now.
mid-session (config-write path? stray restart?) — the config was tls:false
through the whole regression campaign.
