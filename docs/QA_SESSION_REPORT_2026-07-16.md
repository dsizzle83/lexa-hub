# QA & Fix Session Report — 2026-07-15 → 2026-07-16

**Author:** Claude (Opus 4.8, agent session)
**Span:** 2026-07-15 afternoon → 2026-07-16 late morning EDT (includes an ~11h overnight QA soak)
**Repos touched:** `lexa-hub` (branch `standards-buildout`), `csip-tls-test` (branch `feat/dashboard-v2`), `lexa-proto` (branch `main`)
**Bench:** hub dev-kit `ccimx93-dvk` @ 69.0.0.2 (root, Yocto); sims solar-pi .10 / battery-pi .11 / meter-pi .12 / ev-pi .14; dashboard+gridsim+vtnsim local/desktop.

> **Purpose of this document:** a complete, self-contained hand-off so another agent can (a) understand every change that landed and why, (b) pick up every open finding without re-deriving it, and (c) run the bench QA correctly. Nothing here should need to be reconstructed from chat history.

---

## Table of contents
0. [TL;DR](#0-tldr)
1. [Session context & governing directives](#1-session-context--governing-directives)
2. [Code changes that LANDED (committed)](#2-code-changes-that-landed-committed)
3. [Bench-config & infra changes (not commits)](#3-bench-config--infra-changes-not-commits)
4. [QA results — deferred standards scenarios (15)](#4-qa-results--deferred-standards-scenarios)
5. [QA results — Batch 2/3 new fault seams (15)](#5-qa-results--batch-23-new-fault-seams)
6. [QA results — the ~11h soak campaign](#6-qa-results--the-11h-soak-campaign)
7. [OPEN FINDINGS — flagged, NOT fixed (pick these up)](#7-open-findings--flagged-not-fixed)
8. [The boundary-dither / hunt theme (synthesis + recommendation)](#8-the-boundary-dither--hunt-theme)
9. [Known non-bugs / by-design behaviors (do NOT "fix")](#9-known-non-bugs--by-design-behaviors)
10. [Operational playbook — how to run the bench QA correctly](#10-operational-playbook)
11. [Bench state as left](#11-bench-state-as-left)
12. [Per-scenario config matrix](#12-per-scenario-config-matrix)
13. [Appendix — artifacts & pointers](#13-appendix--artifacts--pointers)

---

## 0. TL;DR

**Fixes that landed (all committed, all verified live at the grid meter / reconciler log, not just the oracle):**

| # | Commit | Repo | One-liner |
|---|---|---|---|
| 1 | `e0864c9` | lexa-hub | Cap a plan-followed battery **discharge** under an active export limit (was breaching OpenADR/CSIP caps). |
| 2 | `29e9552` | lexa-hub | Fixed-PF convergence is a **two-sided setpoint** check, not a one-sided ceiling (IEEE 1547.1 §5.14.3.3). |
| 3 | `fbcaca0` | csip-tls-test | AUS **load-cap** CannotComply-for-unmeetable-load is PASS, not DEGRADED (oracle/test fix). |
| 4 | `3e907f2` | csip-tls-test | evsim **ClearChargingProfile** releases the charger to native rate, not suspend (test-infra). |

**Scenario outcomes:** deferred standards **12/15 PASS** (+1 DEGRADED-by-design, +2 blocked by an architectural finding); Batch 2/3 new seams **15/15 PASS**; targeted regression **0 FAIL**; ~11h soak surfaced **6 findings**.

**Open findings for the next agent (none fixed — see §7):**
- **A. 1.6J EV-recovery** — blocked by a plan-follow-vs-reactive-EV architectural gap (proto-agnostic; needs a product decision).
- **B. soc-dither-at-reserve** — battery over-discharges below the 20% reserve (SAFETY, consistent 4/4).
- **C. csip-pagination-walk** — hub does not page the **DERControlList**, adopts the wrong cap (CONFORMANCE/SAFETY, reliable). The P1-1 pagination fix `cf5d2b7` is **incomplete**.
- **D. boundary-dither / hunt class** — event-delay, control-churn, local-clock-step, export-dither, malform-huge all intermittently breach/unseat the cap at a decision boundary (SAFETY).
- **E. modsim** — solar sim crashes to HTTP 500 under sustained fault injection (TEST-INFRA).

**Highest-leverage next step:** findings B+D are one defect class — the hub's guards chatter at decision boundaries. One hardening effort (debounce/hysteresis + hold-bias) retires several. See §8.

---

## 1. Session context & governing directives

This session continued a multi-phase "demo + QA build-out" campaign (see `~/.claude` memory `demo-buildout-campaign`). By the time this session began, Phases 1–3 (dashboard Ops-Plan chart, injection console, VAr/OpenADR backends) were live+committed, and a Phase-4 QA completeness audit had implemented fault seams. Pre-session commits providing context:
- `lexa-proto 60bdb1b` — chunk SunSpec model reads >125 registers (the P0 "model 701 unreadable" bug; also `lexa-hub da33264` bumps the pin, `csip-tls-test a512ab0` un-truncates the sim).
- `lexa-hub cf5d2b7` — "paginate CSIP list resources." **This session found it incomplete — see finding C.**
- `csip-tls-test 329ebf5 / 9ce9734 / bab16d0` — Phase-4 QA audit + fault seams (Batch 2/3 scenarios).

**Governing user directives (in order given):**
1. "Run the live confirmation" → run the mayhem/QA fault suite against the live bench.
2. **"Don't stop until you get all of them passing. Don't cheat. If anything, make it harder for yourself and question the passing results."** — fix real root causes (hub *or* oracle-correctness bugs), never weaken an oracle to force a pass; adversarially verify every pass at ground truth.
3. "Fix the 1.6J bug then run batch 2/3."
4. "Set the bench up and run 8 hours of QA testing… Don't change the code in the meantime… keep it running and review the results until you hear back."
5. "Wrap up the soak and give me a report" → then this document.

The "don't cheat" directive shaped two decisions: (a) I fixed the hub `fixed_pf` bug that a test *exposed* rather than weakening the test; (b) I **refused to override** `cannotcomply-table27`'s DEGRADED because its author pins it deliberately (§9).

---

## 2. Code changes that LANDED (committed)

All four are on feature branches (not `main`). Working trees are clean.

### 2.1 `lexa-hub e0864c9` — cap plan-followed discharge under an active export limit

**File:** `internal/orchestrator/optimizer.go` (+ 3 tests in `optimizer_rules_test.go`).

**Symptom:** mayhem `openadr-limit-adopt` FAIL (site exported 3600W for 69s under an adopted 1000W OpenADR cap) and `openadr-csip-precedence` DEGRADED (75s over the merged 0W CSIP cap). Hub log showed `[plan/follow] following 24h plan: battery=5000W` while `[csip/export-limit]` only curtailed solar.

**Root cause:** The reactive export-limit rule (Rule 3, `applyExportLimitRule`) absorbs surplus into battery-**charge** / EV and curtails solar, but it models a battery **discharge** as an *immovable* export source — it never reduces one. A 24h plan built before the reactive OpenADR/CSIP export cap arrived (the `DailyPlanner` never sees the reactive `grid.ExportLimitW`) keeps following its peak-TOU discharge target, exporting past the cap for a whole replan interval. The existing `dischargeCapW` guard bounded only the *autonomous* TOU rule (Rule 5), which the plan-follow path **suppresses** (`planFollowed=true` → rules 4–6 skipped). So the plan-follow discharge had no export cap on it at all.

**Fix:** Added `DefaultOptimizer.planExportDischargeCapW(limits, state)` returning an **absolute** discharge bound = `conservativeLimit − baseExport`, where `baseExport = signedNetExport − measuredBatteryDischarge` (i.e. export attributable to everything *but* the battery). Threaded into `applyPlanRule`, which clamps the aggregate plan discharge. **Absolute** (not the autonomous rule's *headroom* bound) because `applyPlanRule` re-issues the command every tick and a headroom cap — which shrinks once the meter reflects the discharge — would oscillate; subtracting the measured discharge makes it stable through the ramp-down and correctly credits local load (a net-importing site may still discharge up to its load under a 0W export cap). `NaN` (no export limit) ⇒ no cap ⇒ byte-identical plans. Charge plans (`setW < 0`) are never capped.

**Verified live (bench 69.0.0.2):** the new `[plan/export-cap] plan discharge 5000W would export past the active limit → capped to NW` decision fired every tick, and the grid meter held under the cap. `openadr-limit-adopt` → PASS (only a 1s adoption transient, no false CannotComply for the OpenADR-only bind); `openadr-csip-precedence` → PASS (6s physical ramp-down transient, CannotComply correctly attributed to the CSIP mRID).

**Regression check:** targeted export/CSIP regression batch (malformed-csip, control-churn, pv-flicker, clock-jump-forward, mode-flip, resource-410) = 5 PASS + 1 expected-DEGRADED (pv-flicker), 0 FAIL. The change can only *reduce* export, never increase it.

**Follow-up noted, NOT done:** the constraint-package port (`internal/orchestrator/constraint/economics.go applyPlan`) runs in SHADOW only and should get the same cap before its `export: active` flip, for shadow parity.

### 2.2 `lexa-hub 29e9552` — fixed-PF convergence is a two-sided setpoint check

**Files:** `cmd/modbus/reconcile_adv.go` (`synthesizeLocked`, `AdvAxisFixedPF` case + the `advPFBand` doc), `cmd/modbus/reconcile_adv_test.go`.

**Symptom:** mayhem `pf-var-measured-convergence` FAIL — the advanced-DER reconciler reported `adopt_state=adopted` for the `fixed_pf` axis while the `pf_ack_ignore` sim fault was armed (the 704 write ACKs but model-701 measured PF never moves off its free-running ~0.98). Live measured PF: `0.981 → 0.981` under a commanded `0.90`.

**Root cause:** fixed-PF convergence was **one-sided**: `if got >= want − advPFBand { converged }` — a measured `|PF|` *closer to unity* than commanded was judged compliant ("less reactive than allowed — the limit direction"). But constant power factor is a **SETPOINT**, not a ceiling: IEEE 1547.1 §5.14.3.3 verifies the DER's reactive power **tracks** the commanded PF; IEEE 1547 §5.3.2 defines constant-PF mode as *holding* that PF to deliver reactive support. An inverter left near unity while commanded 0.90 is failing to deliver the requested VARs — the accept-but-ignore fault. The very same `synthesizeLocked` already treated `fixed_var` **two-sided** ("a SETPOINT, not a limit") — an internal inconsistency.

**Fix:** two-sided `if math.Abs(got − want) <= advPFBand { converged }`, matching `fixed_var` and the "trust measurement, not the command" discipline. The `advPFAssessMinW` low-power floor and the debounce/corrective-write path are unchanged. Updated the pinning unit test `TestAdvShell_FixedPFMeasuredConvergence` (which had asserted the one-sided "above-commanded PF must converge") to assert the two-sided behavior + the accept-but-ignore divergence.

**Verified live:** report topic showed `axis:"fixed_pf",adopt_state:"diverged"` under a 0.90 command with measured PF held at 0.981; scenario PASS.

**Important operational caveat (see §12):** this scenario only becomes judgeable with `advanced_der:off`. With the bench's demo default `advanced_der:on`, the hub's WP-9 author keeps republishing an `energize` desired-adv doc that **supersedes** the scenario's injected `fixed_pf` doc, so the retained report reads the wrong axis (INCONCLUSIVE). The hub was *always* correct on the curve axis; the one-sided PF bug was only *observable* once WP-9 was isolated off.

### 2.3 `csip-tls-test fbcaca0` — AUS load-cap CannotComply-for-unmeetable is PASS (ORACLE fix)

**Files:** `cmd/dashboard/mayhem_adv.go` (`diagnoseAusCap`), `cmd/dashboard/mayhem_adv_test.go`.

**Symptom:** mayhem `aus-load-cap` DEGRADED — home load 6500W injected against a 3000W gross-load cap; the hub honestly posted CannotComply (irreducible baseload > cap), yet was scored DEGRADED.

**Root cause:** the shared `diagnoseAusCap` marked *any* breach-the-hub-only-admitted (CannotComply, not converged) as DEGRADED. That's correct for a **generation** cap (PV is always curtailable, so a CannotComply there is a real failure to curtail) but wrong for a **load** cap — home baseload alone can exceed the cap with no lever to shed it, and the scenario's own `Expected` says CannotComply is "a legitimate, correct outcome here, not just an acceptable one." The suite even reflected the asymmetry: there is a `TestDiagnoseAusGenCap_Degraded_CannotComplyReported` pinning gen→DEGRADED, but **no** equivalent load pin.

**Fix:** threaded a `cannotComplyLegit bool` (false=gen, true=load). Load cap + correct CannotComply on an unmeetable load = PASS; gen cap stays DEGRADED; a **silent** sustained breach (no CannotComply) stays FAIL for both. Added `TestDiagnoseAusLoadCap_Pass_CannotComplyUnmeetable`.

**This is a test/oracle-correctness fix, NOT a hub change.** It aligns the oracle with its own documented Expected. Verified: `aus-load-cap` PASS live. **Deliberately did NOT** apply the analogous change to `cannotcomply-table27` (its author pins DEGRADED on purpose — see §9).

### 2.4 `csip-tls-test 3e907f2` — evsim ClearChargingProfile releases, not suspends (TEST-INFRA)

**Files:** `sim/evsim/battery.go` (`ResumeNative`), `sim/evsim/state.go` (`handleClearChargingProfile`), `sim/evsim/ocpp16_test.go`.

**Symptom:** mayhem `ocpp16-smart-charge` FAIL / `clear-profile-release` BLIND — the 1.6J charger stayed pinned near ~0.0A after the control was cleared.

**Root cause (in the SIM, not the hub):** `handleClearChargingProfile` (shared by the 1.6J and 2.0.1 handlers, `ocpp16.go`/`ocpp201.go`) called `SetCommandedA(0)` on a ClearChargingProfile. But `evBattery.Tick` (battery.go:~106) reads `commandedA <= 0` as **do-not-charge**, and `StopCharging` uses `0` for session-end. So `0` was overloaded — the clear handler meant "unrestricted" (see the old test's *name*, `...RestoresUnrestricted`, which asserted `commandedA==0`) while the battery model reads `0` as *stop*. Clearing a profile therefore **suspended** the charger. The hub's `applyClear16`/`ApplyClear` (bridge16.go/main.go) were **correct** (mirror the working 2.0.1 path; I confirmed live the 2.0.1 reconciler issues `ClearChargingProfile` + `MaxCurrentA=1000000` = the `RestoreCurrentA` release sentinel).

**Fix:** added `evBattery.ResumeNative()` (sets `commandedA = MaxCurrentA`) and call it on clear. Disambiguates: `0` = stop (session end), `MaxCurrentA` = unrestricted (clear). Covers both stacks. Fixed the mis-named unit test to assert resume-to-native.

**⚠️ This fix is correct but NOT SUFFICIENT** — the ocpp16 scenarios still fail for an *independent* architectural reason (finding A, §7.1). See there.

---

## 3. Bench-config & infra changes (not commits)

These are on-device / operational, not code. They matter for reproducibility.

- **`metrics_addr` gap (fixed on-device):** the Phase-3 surgical deploy left `metrics_addr:""` (⇒ 127.0.0.1) on the lexa services, so the remote dashboard couldn't scrape `:910x` → every metric-corroborated oracle degraded ("metrics scrape failed"). Set `metrics_addr:"0.0.0.0:<port>"` on hub(9101)/northbound(9102)/modbus(9103)/ocpp(9104)/telemetry(9105). This upgraded `adv-shadow-no-writes` DEGRADED→PASS and corroborated pf/aus. **`deploy-hub-pi.sh` should set this per CLAUDE.md's bench pattern — the surgical deploy missed it.**
- **Stale gridsim (fixed by rebuild):** the *running* `csip-gridsim` transient unit was an older build lacking new admin endpoints (`/admin/responses`, `randomizeDuration`) → `cancelled-superseded-roundtrip` INCONCLUSIVE + `randomize-duration-honored` FAIL, both **false**. Fix: `go build -o bin/server ./sim/server` (NOT `cmd/server`) + restart the `csip-gridsim` `systemd-run --user` unit (see `scripts/bench-up.sh:101`). Both PASS after.
- **Binaries deployed (arm64):** `lexa-hub`, `lexa-modbus` (for the two hub fixes) to `/usr/local/sbin/` via stop→swap→start (avoids ETXTBSY); `evsim` to `dmitri@69.0.0.14:/home/dmitri/bin/evsim`. Backups left as `.bak-<ts>`.

---

## 4. QA results — deferred standards scenarios

The 15 "bench-deferred" standards-buildout scenarios (`csip-tls-test/docs/QA_STANDARDS_BUILDOUT.md:86-97`). **Result: 12 PASS, 1 DEGRADED-by-design, 2 open.**

| Scenario | Result | Notes |
|---|---|---|
| adv-shadow-no-writes | PASS | needed `metrics_addr:0.0.0.0` (§3) + `reconciler.adv:shadow` |
| curve-adopt-readback-divergence | PASS | needed `advanced_der:off` (WP-9 pollution) |
| pf-var-measured-convergence | PASS | **fix 2.2**; needed `advanced_der:off` |
| logevent-alarm-pair | PASS | |
| openadr-limit-adopt | PASS | **fix 2.1** |
| openadr-csip-precedence | PASS | **fix 2.1** |
| redirect-storm | PASS | |
| aus-gen-cap | PASS | needed `enforce_aus_limits:true` |
| aus-load-cap | PASS | **fix 2.3** (oracle) |
| pin-freeze-egress-halt | PASS | needed `registration_pin`≠111115 armed + nb restart; **RESTORE to 0 after** or all nb egress stays frozen |
| ev-setpoint-clamp | PASS | needed `ev_storage:true` |
| pairing-gate-hold | PASS | needed evsim `-id evse-999` (unknown station) + `pairing_mode:"gated"` |
| **cannotcomply-table27** | **DEGRADED (by design)** | hub 100% correct; author pins DEGRADED — see §9 |
| **ocpp16-smart-charge** | **OPEN (FAIL)** | finding A (§7.1) |
| **clear-profile-release** | **OPEN (FAIL/BLIND)** | finding A (§7.1) |

---

## 5. QA results — Batch 2/3 new fault seams

**Result: 15/15 PASS** (after the stale-gridsim rebuild, §3).

- **Batch 2 (CSIP edge):** malform-pagination, csip-pagination-walk\*, cancelled-superseded-roundtrip, randomize-duration-honored, csip-event-delay\*, csip-slow-loris, negative-export-limit.
- **Batch 3 (modbus):** modbus-tcp-drop, modbus-unit-id-confusion, modbus-register-tearing.
- **Batch 3 (OCPP):** ocpp-out-of-order-txevent, ocpp-boot-mid-tx (audit flagged these as likely hub-bug surfacers; hub stayed up — the oracle notes "EV draw coherence not exercised" because the EV isn't charging under plan-follow, tying to finding A).
- **Batch 3 (network/cert):** net-partition-gridsim, net-dns-fail, cert-rotation-failclosed. All self-restored cleanly.

\* `csip-pagination-walk` and `csip-event-delay` PASSED in this Batch-2 pass but the ~11h soak later showed **csip-pagination-walk is a reliable FAIL** and **csip-event-delay is ~25% intermittent** — see §7. (csip-pagination-walk PASSed here only because it was actually INCONCLUSIVE under the then-stale gridsim and I re-confirmed only randomize/cancelled after the rebuild — a gap in my earlier confirmation.)

---

## 6. QA results — the ~11h soak campaign

**2026-07-16 00:31 → ~11:30 EDT. NO code changed (per directive).** ~20 mayhem runs: 1 full-suite baseline (101 scenarios) + a curated soak of **95 scenarios in 5 batches (aa–ae) × ~3.5 cycles**, each scenario exercised ~3–4×. Special-config scenarios (adv-isolation, pin, 1.6J-EV, pairing) were excluded from the soak so batches complete with accurate final reports.

**Core-service stability was excellent:** a 5-min health logger recorded **108 snapshots with 0 anomalies** — hub/northbound/modbus/ocpp/api + gridsim + dashboard never went down across the whole campaign.

**Stable core (deterministic across all cycles):** CSIP robustness (malform family, corrupted-control, redirect, clock), transport chaos (netem loss/reorder, hub↔gridsim partition, DNS blackout), OCPP convergence, cert-rotation, supersede/cancel/randomize, AUS gen/load, negative-limit. Batch `aa` was **16 PASS / 4 DEGRADED every single cycle**. All "device ACKs then ignores the command → hub detects & flags it" scenarios correctly land DEGRADED.

**The soak surfaced 6 findings (§7).** It also showed **late-soak flakiness rises after ~11h** — cycle 4 sprouted intermittent FAILs on batches clean for 3 cycles (malform-huge, logevent), partly the leaky sims degrading the bench, partly known intermittents surfacing statistically. Signal past ~cycle 3 is noisier.

---

## 7. OPEN FINDINGS — flagged, NOT fixed

> None of these were fixed. B–E were found during the "don't change the code" soak. A was found earlier and needs a product decision. Each includes evidence, suspected root cause, and code pointers.

### 7.1 Finding A — 1.6J EV-recovery blocked by plan-follow vs reactive-EV (architectural)

- **Scenarios:** `ocpp16-smart-charge` (FAIL), `clear-profile-release` (FAIL/BLIND). Also weakens `ocpp-out-of-order-txevent` / `netem-jitter-evse` (EV never draws → "coherence not exercised" / INCONCLUSIVE).
- **Symptom:** after an import cap that curtails the EV is cleared, the 1.6J charger stays pinned at ~0A; the scenarios' premise (EV charging → curtail → release) never holds.
- **Root cause (proven with a live capture):** the hub is in **plan-follow** mode and the 24h cost-optimal plan commands `ev=0.0A` for 60/60 ticks (`[plan/follow] following 24h plan: battery=0W ev=0.0A`) — it schedules EV charging for a cheaper time. Plan-follow **suppresses** the reactive EV rule (`optimizer.go` rules 4–6 are gated on `!planFollowed`). A newly-started session isn't in the stale plan (replan interval 900s), so within the ~90s scenario window the EV never charges → there is nothing to curtail or release. **Proto-agnostic** (would affect 2.0.1 too — the evsim fix 2.4 was necessary but not sufficient).
- **Decision needed (product):** should a newly-connected EV session trigger a replan, or should reactive EV control run even under plan-follow? On a real deployment a plugged-in EV waiting up to 15 min (replan cadence) to start charging is poor UX.
- **Where to look:** `cmd/hub` `applyPlanRule` (sets `ev=target.EVMaxCurrentA`) and the `planFollowed` suppression of `applyEVChargingRule`; the `DailyPlanner` EV scheduling; replan trigger on session-connect.

### 7.2 Finding B — soc-dither-at-reserve over-discharges below reserve  ·  SAFETY  ·  **consistent 4/4**

- **Symptom:** when the pack's SoC dithers ±1pt across the 20% `SOCReserve` line, the hub keeps discharging **below reserve** — `INV-SOC-RESERVE` violated on ~62 samples/window, e.g. discharging 4700W at SoC 19% (≤ reserve). The hub *does* post CannotComply but still over-discharges.
- **Why it matters:** battery-reserve protection is a safety guard; it should hold the line on *every* sample, not on average.
- **Suspected root cause:** the reserve guard `dischargingAtReserve` chatters at the boundary instead of hold-biasing. Fix-hint in the oracle references **GAP-08** — this likely confirms a known gap live.
- **Where to look:** `internal/orchestrator` `checkBatterySafety` / `dischargingAtReserve` — add debounce + hold-bias at the boundary.

### 7.3 Finding C — csip-pagination-walk: DERControlList not paged  ·  CONFORMANCE/SAFETY  ·  **reliable**

- **Symptom:** the hub does a single GET of the **DERControlList** (`/derp/0/derc`, `all=2`/`results=1`), adopts the page-1 5000W cap, and **never fetches page-2's superseding 1000W cap** — the exact P1-1 field failure. Fails in isolation (not flaky).
- **Key point:** your P1-1 pagination fix (`lexa-hub cf5d2b7`) covers the DER**Program**List (`malform-pagination` PASSes) but **not** the DER**Control**List — even though the control list *is* wired to a pager: `internal/northbound/discovery/walker.go:339 fetchExtendedDERControlListPaged` → `paginate.go fetchPagedList` (with correct `all`/`results`/`href`). So it is **not** a simple missing-wire.
- **Suspects (read-only analysis):** (a) `model.ExtendedDERControlList`'s `All`/`Results` parse hitting `fetchPagedList`'s single-page fast-path (`len(acc) >= all`); (b) the gridsim not actually serving `?s=1` for `/derp/0/derc` (verify the harness first — it's a new scenario from `bab16d0`); (c) the scheduler's supersession picking page-1. Also note step 6c `fetchDERControlList` (ActiveDERControlList) is a **bare** fetch (not paged) by design.
- **Where to look:** `internal/northbound/discovery/paginate.go` (fast-path condition), `walker.go` steps 6b/6c, `sim/gridsim` paged DERControlList serving.

### 7.4 Finding D — boundary-dither / hunt class (intermittent)  ·  SAFETY

Five scenarios, same shape (see §8): under dithering or adversarial CSIP-server timing the hub intermittently breaches/unseats the active cap.

| Scenario | Trigger | Rate observed | Failure mode |
|---|---|---|---|
| **csip-event-delay** | 20s/GET `/dcap` serve delay | **~25% (6P/2F of 8)** | unseats the active 0W export cap; FAIL runs breach the whole 42–50s window, PASS runs only 0–9s |
| **control-churn** | cap delete-and-replaced every ~12s (new mRID) | intermittent (PASS cy1&2, FAIL cy3) | **silent** 16s/3900W breach — device doesn't converge and hub posts **no** CannotComply; each replacement resets the guard session |
| **local-clock-step-forward** | hub wall clock +1h mid-control | intermittent (INCONCLUSIVE→FAIL) | brief 10s/1230W unseat + an *unnecessary* CannotComply (it could just curtail PV); residual gap in TASK-037 anchoring during re-anchor |
| **export-dither-at-breach** | measured export dithers at the compliance band | intermittent | hunts/breaches across the line |
| **malform-huge-activepower** | malformed absurd ActivePower limit | intermittent (clean 3 cycles, FAIL cy4) | `reacted=False`, "malformed resource unseated the safe control," 69s breach |

- **Where to look:** `internal/northbound/run.RunOnce` fail-closed walk-error hold; `internal/tlsclient` read/response deadlines; `checkExportLimitConvergence` accumulation across mRID supersession; `TASK-037` `expiryConfirmTicks` / freshness anchoring.

### 7.5 Finding E — modsim crashes to HTTP 500 under sustained fault injection  ·  TEST-INFRA

- **Symptom:** the solar sim's process stays `active` but `/state` returns 500 after many fault injections — observed **3× and accelerating** (~hourly by the end), **independent of the active scenario** (crashed during transport scenarios that don't use it) → a cumulative resource leak.
- **Impact:** spurious inverter/meter BLINDs and *sim-induced* "hub FAILs" (I confirmed `openadr-limit-adopt` and others PASS once the sim is healthy — the export-cap fix is solid). **Not a hub bug.**
- **Mitigation used:** `systemctl --user restart modsim` recovers it; I moved to a **preventive restart before each batch**, which kept runs clean.
- **Where to look:** `csip-tls-test/sim/southbound` (modsim) — resource leak / unhandled fault path under repeated `/inject` + `/fault`.

---

## 8. The boundary-dither / hunt theme

**The single most valuable synthesis of the campaign.** Findings **B (soc-dither)** and the five in **D** are the *same defect shape*: the hub's guards **chatter or breach when a value straddles a decision boundary, or when the CSIP server behaves adversarially (delay / churn / clock-step)**. They should *hold-bias* at the line; instead they coin-flip.

| Boundary | Scenario(s) | Consistency |
|---|---|---|
| 20% SoC reserve line | soc-dither-at-reserve | consistent (safety-critical) |
| export cap under adversarial server timing | csip-event-delay, control-churn, local-clock-step, malform-huge | intermittent |
| measured export at the compliance band | export-dither-at-breach | intermittent |

**Recommendation (highest leverage):** treat these as **one hardening task** — add debounce / hysteresis + hold-bias to the guard decisions:
- `dischargingAtReserve` (battery reserve),
- `checkExportLimitConvergence` + guard-session accumulation across mRID churn (export enforcement),
- `run.RunOnce` fail-closed hold + `tlsclient` deadlines (walk under delay),
- TASK-037 freshness anchoring (clock step).

so a value straddling the line, or a control re-issued/delayed faster than a guard cycle, does not produce a coin-flip. **One effort retires several findings.** `soc-dither-at-reserve` is the most severe single item (consistent + battery safety) and the natural first target.

---

## 9. Known non-bugs / by-design behaviors (do NOT "fix")

- **`cannotcomply-table27` → DEGRADED.** The hub is 100% correct: a forced *unresolvable* breach (`ack_before_effect`) → CannotComply carrying **IEEE 2030.5 Table 27** codes (not legacy 0xF0). Its author **pins DEGRADED** in `mayhem_reporting_test.go` (`TestDiagnoseCannotComplyVocab_AdmittedBreach_Table27`): *"diagnoseConverge's own base verdict for a ReportedCannot breach is DEGRADED … an honest admission, not a full PASS … which this oracle passes through unchanged."* I started to override it, then **reverted** — that would be exactly the oracle-weakening the "don't cheat" directive forbids. (Worth noting: `openadr-csip-precedence`'s oracle PASSes the *same* forced-breach+correct-CannotComply shape — the two oracles are inconsistent and could be harmonized, but that's a deliberate design call, not mine to make silently.)
- **DEGRADED for "device ACKs then ignores" scenarios** (battery-charge-disabled, battery-soc-refuse, ev-accept-but-ignore, ev-profile-reject, ramp-limit-curtail, pv-flicker, etc.): correct **defensive posture** (the hub detects the device fault and flags/admits it). DEGRADED here = success, not a bug.

---

## 10. Operational playbook

**How to run the bench mayhem QA correctly (learned the hard way):**

- **Never `timeout`-kill a mayhem run.** It strands the dashboard's *server-side* engine → the next run gets **HTTP 409 "a mayhem run is already in progress."** Use `python3 -u scripts/mayhem.py --abort` to clear + restore the bench. Give runs a timeout **longer** than they need, or run smaller batches that finish.
- **The full 101-scenario suite takes >2h.** Prefer **curated `--only` batches** (~20 scenarios, ~25 min) that *complete* — mayhem interleaves start-lines and verdict-lines, so a **truncated** run's scenario↔verdict pairing is **unreliable**; only the **final report's `[FAIL]`/`[DEGRADED]` section headers** are accurate. (This cost me one early mis-read.)
- **Always run with `python3 -u`** (unbuffered) so you can monitor progress in the log; **do not** append `&` under a background launcher (it detaches from task tracking).
- **Check the sims at every review**, not just hub services — the modsim (finding E) crashes silently (`active` but `/state`=500) and induces spurious BLINDs/FAILs. A `curl -s -o /dev/null -w '%{http_code}' http://69.0.0.10:6020/state` per review catches it. Consider a preventive `systemctl --user restart modsim` before sim-heavy batches.
- **Adv-isolation scenarios (curve/pf/adv-shadow)** need `advanced_der:off` AND run **individually** with a clear of `lexa/{desired,reconcile}/adv/inverter-0[/report]` between them (retained-report cross-scenario pollution; the hub's WP-9 author supersedes injected docs when `advanced_der:on`). Recommended future hardening: make the adv oracle match the report's `mrid` so it's robust to WP-9 and cross-scenario pollution.
- **pin-freeze-egress-halt** arms `registration_pin`≠111115 + restarts nb, which **freezes ALL northbound egress**. Run it isolated and **restore `registration_pin:0` + restart nb afterward**, or every later CannotComply/PUT/LogEvent scenario silently fails.

---

## 11. Bench state as left

**Up and healthy; soak stopped, all background monitors stopped.** Verified: all 7 lexa services active; 4 sims + gridsim + dashboard = HTTP 200; 0 iptables DROP residue; hub clock correct.

**Config on the hub (69.0.0.2)** — carried over from the campaign, **not** the shipped/product defaults:

| Key | File | Value | Note |
|---|---|---|---|
| advanced_der | hub.json | `"on"` | demo default (adv authoring). Flip **off** for adv-isolation scenarios. |
| ev_storage | hub.json | `false` | ceiling-mode EV (needed for ocpp16 oracles). |
| **enforce_aus_limits** | hub.json | **`true`** | I flipped this on for AUS coverage — **revert to `false`** for the pre-campaign default. |
| reconciler.adv | modbus.json | `"active"` | |
| metrics_addr | all svc json | `"0.0.0.0:<port>"` | §3 fix — keep (enables metric scrapes). |
| legacy_cannotcomply_code | northbound.json | `false` | product default (Table 27). |
| port_16 | ocpp.json | `0` | 1.6J listener off (restored). |
| pairing_mode | ocpp.json | `""` | |
| registration_pin | northbound.json | `0` | restored (egress unfrozen). |
| timing | — | FAST | engine 3s / discovery 5s (bench-up.sh; restore with `--stock`). |

Sims: all freshly restarted. evsim on 69.0.0.14 is back to `-csms ws://69.0.0.2:8887/ocpp` (OCPP 2.0.1, `evse-001`) with the **fixed** binary (§2.4).

---

## 12. Per-scenario config matrix

Which non-default config each special scenario needs (the rest run under §11's config):

| Scenario(s) | Requires |
|---|---|
| curve-adopt-readback-divergence, pf-var-measured-convergence | `advanced_der:off`, `reconciler.adv:active`, `der_gen:7xx`; run **individually**, clear adv topics between |
| adv-shadow-no-writes | `advanced_der:off`, **`reconciler.adv:shadow`**, `metrics_addr:0.0.0.0` |
| aus-gen-cap, aus-load-cap | `enforce_aus_limits:true` |
| cannotcomply-table27 | `legacy_cannotcomply_code:false` + gridsim Table-27 accept |
| pin-freeze-egress-halt | `registration_pin`≠111115 + nb restart → **restore 0 after** |
| ocpp16-smart-charge, clear-profile-release | evsim `-proto 1.6` → hub `port_16:8888`, `ev_storage:false` (ceiling mode) |
| pairing-gate-hold | evsim `-id evse-999` (unknown) + `pairing_mode:"gated"` |
| ev-setpoint-clamp | `ev_storage:true` |

---

## 13. Appendix — artifacts & pointers

- **Scratchpad campaign docs** (this session's working area; copy anything worth keeping into the repo):
  `…/scratchpad/QA_CAMPAIGN_SUMMARY.md` (soak summary) and `…/scratchpad/QA_CAMPAIGN_FINDINGS.md` (per-run soak log, run #1–#20 with timestamps and health checks). Scratchpad root: `/tmp/claude-1000/-home-dmitri-projects-lexa-hub/7cbd4a28-d4ad-4ec1-b53e-cddd53b7a131/scratchpad/`.
- **Planning/runbooks:** `csip-tls-test/docs/QA_STANDARDS_BUILDOUT.md` (deferred-scenario recipes), `csip-tls-test/docs/QA_COMPLETENESS_AUDIT.md` (§3 deferred list, §4 batches).
- **Commits (this session):** lexa-hub `e0864c9`, `29e9552`; csip-tls-test `fbcaca0`, `3e907f2`. Branches: lexa-hub `standards-buildout`, csip-tls-test `feat/dashboard-v2`. **All on feature branches — not merged to `main`.**
- **Related memory:** `demo-buildout-campaign`, `standards-audit-campaign`, `csip-conformance-setup`, `orchestrator-review-findings`, `lexa-hub-audit-fixes` (in `~/.claude/projects/-home-dmitri-projects-lexa-hub/memory/`).

**Suggested next-agent priority:** (1) finding B (soc-dither, consistent battery-safety), then the rest of the §8 boundary-dither/hunt hardening; (2) finding C (DERControlList pagination — silent wrong-cap enforcement); (3) finding A (1.6J EV — needs a product decision first). Findings D-intermittents are best verified/fixed alongside the §8 hardening; finding E is test-infra.
