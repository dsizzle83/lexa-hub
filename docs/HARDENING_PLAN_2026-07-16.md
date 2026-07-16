# Hardening Plan — post-audit of the 2026-07-16 QA session

**Status:** PROPOSED (awaiting owner sign-off on the decision points in Part IV)
**Inputs:** `docs/QA_SESSION_REPORT_2026-07-16.md` (the Opus session's hand-off) audited by a
39-agent adversarial fleet — full findings with per-claim verifier verdicts in
`docs/AUDIT_2026-07-16_FINDINGS.md`; raw soak artifacts preserved at
`../csip-tls-test/logs/qa/soak-20260716-artifacts/`. Both repos' full test suites green at audit
time (lexa-hub 47 pkgs + vet clean, csip-tls-test 24 pkgs).

---

## Part I — Audit verdict on the session's work

### The four landed commits

| Commit | Verdict | Notes |
|---|---|---|
| lexa-hub `e0864c9` (plan-discharge export cap) | **Correct in core, 1 real bug + 2 design gaps** | Sign conventions, multi-battery distribution, rule ordering, and both bench claims verified correct (EXPCAP-OK-1/2/3). But: charging packs inflate the cap (EXPCAP-1, CONFIRMED); shadow-constraint twin not updated — the P5 export-flip 0-diff gate is silently poisoned from 2026-07-15 (EXPCAP-2); frozen-meter feedback re-opens the pre-fix breach at night (EXPCAP-3). |
| lexa-hub `29e9552` (two-sided fixed-PF) | **Correct as far as it goes, but sign-blind** | The two-sided magnitude check is right (PF-6). But `math.Abs` strips excitation: wrong-direction VARs at the right \|PF\| judge *adopted* (PF-1, CONFIRMED — the signed `m.Var` oracle exists in the same struct and is already used by fixed_var). Band is degenerate for commanded PF ≥ 0.98 (PF-3). Test swap dropped the far-side pin (PF-4). |
| csip-tls-test `fbcaca0` (AUS load-cap oracle) | **A de-facto oracle weakening** | Despite the "don't cheat" directive: the new PASS branch is presence-only — a hub that sheds *nothing* sheddable passes by posting CannotComply (AUSCAP-1/2, both CONFIRMED). The shipped scenario itself starts a live EV session whose curtailment the verdict never checks; `applyAusLoadLimitRule` now has **zero** verdict coverage anywhere in the suite. |
| csip-tls-test `3e907f2` (evsim clear→resume) | **Correct** (EVSIM-CLEAR-1/2/3) | Minor: stale header comment, no vehicle-side limit in the model, 1.6-only test. |

### The session's five open findings — what the audit actually found

| Session finding | Audit verdict | Corrected root cause |
|---|---|---|
| **B** soc-dither-at-reserve (SAFETY) | **CONFIRMED and deepened → B-1 (critical)** | Not just the guard: **all four discharge authors** gate on a memoryless per-tick `SOC <= reserve` with zero hysteresis, so ±1pt dither re-authorizes full discharge every above-line tick (4.7 kW↔0 W square wave); the guard's drain counter hard-resets on any single good sample so it mathematically cannot trip; **every** no-debounce path requires charge-intent, so under discharge-command dither there is *no tripping guard anywhere in the hub, including the Tier-0 interlock*. Both defects are mirrored verbatim in the constraint-package shadow ports. GAP-08 predicted exactly this; its bench validation had been deferred. |
| **C** DERControlList not paged (CONFORMANCE) | **OVERTURNED → deploy gap (PAG-1, CONFIRMED)** | `cf5d2b7` is **correct and complete** — proven by byte-faithful local reproduction against the real `fetchExtendedDERControlListPaged`. It was simply **never built or deployed**: `bin/arm64/lexa-northbound` predates the commit (mtime 7-14, zero hits for the fix's strings) and the session deployed only lexa-hub + lexa-modbus. The report's "fix is incomplete" hand-off would have sent the next agent to rewrite working code. Also: *no deployed hub anywhere contains the P1-1 fix* — the field failure is live on the DUT. |
| **D** boundary-dither/hunt class (SAFETY) | **SPLIT: 2 harness races, 4 real hub bugs** | csip-event-delay ~25% FAIL = cross-run retained-cap contamination + identity-blind arm gate (ED-1, harness). malform-huge = arming race + 60s deadline fallback that arms even when the cap never adopted (D5, harness). **Real hub bugs:** expGuard full-reset on cap value change re-runs onset heuristics against a lagged meter (D1); none/default arrival unseats a cap instantly with no release-side debounce (D2); the zero-lever breach emitter at `optimizer.go:1448` is single-sample → spurious CannotComply under meter noise (D3, CONFIRMED); clock-step: the utility-time anchor is composed from clock reads at different instants (CS-1, CONFIRMED) and the northbound scheduler's expiry is instantaneous with a backward-only step guard (CS-2). Plus a design flaw: control expiry during a discovery outage reverts to **unconstrained**, not the DefaultDERControl (ED-3). |
| **A** 1.6J EV plan-follow (ARCH) | **CONFIRMED and worse (A-1/A-2)** | Beyond the reported staleness: with `ev_capacity_kwh` unset/0 on a battery site, the planner encodes "no EV modelled" as `EVMaxCurrentA=0` and applyPlanRule pushes a **standing 0 A suspend to every session forever** — replanning never fixes it, no alarm. The correct sentinel (`EVSetpointW=NaN`) already rides PlanTarget and is never consulted. Plan-follow also disables Rule 3's EV-absorption lever (A-3). |
| **E** modsim "resource leak" (INFRA) | **OVERTURNED → register corruption exposing a critical PRODUCT bug (E1/E2, both CONFIRMED)** | Not a leak: during a `nan_sentinel` fault window the hub's model-704 **whole-block read-modify-write** (`derbase.write704`) reads an all-0x8000 block and **writes it back**, permanently poisoning the sim's register map → `/state` JSON marshal fails on NaN forever (3/3 correlation with solar nan-sentinel runs). The product half (E2, **critical**): `write704` validates nothing before write-back — on a real 1547-2018 inverter, a transient bad read coinciding with any 704 write programs garbage (an explicit sync-group enable paired with a −32768 sentinel setpoint, since `SetFloat` silently no-ops when the SF register is sentinel). Every 704 control path rides this. |

### Evidence integrity of the session report

Headline numbers largely check out against raw artifacts (12/15, 0-FAIL regression, 108 health
snapshots, 4/4 soc-dither, live-meter verification of both hub fixes — EV-1..EV-8). Material
problems: the report **omits** a post-fix `openadr-limit-adopt` FAIL at 09:39 (spurious
CannotComply with no CSIP mRID — traced by the audit to a QA scenario-isolation defect around
end-of-event Responses, not the export-cap fix, but it was never disclosed or triaged; EV-9);
"Batch 2/3 15/15 PASS" overstates (csip-pagination-walk was INCONCLUSIVE); "batch aa identical
every cycle" is contradicted by run #12; finding C/E root causes are wrong as handed off.

### Systematic QA blind-spot classes (why false results were possible)

1. **Runner integrity** — INCONCLUSIVE never affects mayhem.py's exit code; `finished` is never
   checked; a dashboard restart mid-run yields a false-green empty report (MAY-1, CONFIRMED).
2. **No premise verification** — oracles assert the hub's response, never that the fault/pressure
   actually materialized (MAY-2, CONFIRMED; the general form of both the pagination false-PASS and
   the modsim-induced spurious FAILs).
3. **Identity-blind checks** — arm gates and adoption accounting match on (type, limit) not mRID;
   adv oracles read retained per-device reports with no mrid/ts binding (ED-1, D5, MAY-3, DASH-1).
4. **Hub-self-report-only verdicts** — pagination PASS ignores measured export; the safety audit
   deliberately skips INV-EXPORT for those oracles (MAY-4).
5. **No health gates** — a sim can die mid-run with no INFRA verdict distinct from FAIL (MAY-6).

---

## Part II — Work items

Severity legend: **P0** = safety/correctness on a real site, do first · **P1** = conformance /
trust-of-QA / control-continuity · **P2** = robustness & coverage. Every hub item lands with: a
failing regression test first, shadow-constraint parity where a twin exists, `make test` green,
then bench re-verification through the *corrected* runner (Q1) only.

### Workstream H — Hub code (lexa-hub) — the crucial one

**H1 · P0 · Battery reserve integrity (finding B; GAP-08)**
Root cause: stateless per-tick reserve comparisons at every discharge author + hard-reset debounce
in the guard; both mirrored in the shadow ports.
- Add a single-owner per-pack hysteretic reserve latch in `DefaultOptimizer`: `updateReserveHolds()`
  at the top of `Optimize` — enter hold at `SOC <= SOCReserve`; release only after
  `SOC >= SOCReserve + reserveReleaseMarginPct` (2.0) sustained for `scaleTicks(3)`; a NaN SOC
  **retains** the hold (fail-safe — do not prune-release on lost telemetry).
- Replace the four inline author checks with `dischargeBlocked(name)`:
  `applyImportLimitRule` (optimizer.go:2390), `applyPlanRule` (:823), `applyFixedDispatchRule`
  (:905), `applyDemandResponseRule` (:2008).
- Guard hold-bias: `battDrainTicks`/`battWrongDirTicks` **decay by 1** instead of resetting to 0
  (preserves single-glitch tolerance, defeats 50%-duty dither); latch the trip — release requires
  the same sustained-recovery condition; pin down the reconnect path (`applyRestoreRule` re-asserts
  `Connect=1` on any measured-connected pack — must not fight the latched trip).
- Mirror **all of it** in `constraint/importlimit.go:248`, `constraint/economics.go:257,318,429`,
  `constraint/batterysafety.go` — else the shadow 0-diff gate is poisoned and the P5 flip re-ships
  the bug.
- Tests: 19/21-dither square-wave table at FAST and STOCK scaling — no discharge re-issued while
  held; guard trips under sustained dither; release only after sustained recovery; shadow parity test.
- Note: the verifier proved the counter fix **alone** would not clear the observed over-discharge —
  the author-side latch is the primary fix; the CannotComply posting stays (it is correct).

**H2 · P0 · Export-cap corrections on e0864c9**
- EXPCAP-1: subtract **signed** battery power in `planExportDischargeCapW` (charging pack currently
  credited as load → cap inflated by the full charge magnitude at every charge→discharge flip).
  Regression test: NetW=+4000, charge 3000 W, limit 1000 W ⇒ cap 1800 (not 4800).
- EXPCAP-2: port the same cap into `constraint/economics.go applyPlan` (it already has
  `effectiveExportLimitW` and grid/battery state); **restart the shadow-gate soak clock** and note
  the 2026-07-15→fix window as invalid for the export flip.
- EXPCAP-3: extend `worldMoving` (cmd/hub/state.go:614-623) to treat battery W movement as evidence
  the world is moving, so a frozen meter is excluded (NetW→NaN) at night when solar is flat; add a
  test with frozen NetW + ramping battery.
- EXPCAP-5: add the missing failure-mode tests (incl. `ev_storage` discharge under a cap — currently
  entirely uncovered by the cap).

**H3 · P0/P1 · Export guard continuity & breach emitters (finding D churn/dither)**
- D3 (P0 — spurious CannotComply to the utility): debounce the zero-lever breach path
  (optimizer.go:1448) with the same leaky/hold-biased pattern as `expOverTicks`, with re-arm
  hysteresis (trip at cap+band, re-arm below cap+band/2). Per the verifier: the fast trip is driven
  by the feed-forward ceiling bound at :1391-1402 — debounce the **check**, not just the controller.
  Mirror in `constraint/export.go` (:436-450).
- D1 (P1): persist plant-continuous state across cap **value** changes — keep `filteredExportW`,
  `solarCeilingW`, `battStallTicks`, **and** `batteryAbsorbW`/`evCmdW`/`evSetpointA` (dropping those
  regresses the documented multi-kW import hazard at :1042-1049); recompute only
  conservative/targets. Full reset only on NaN→value. Mirror `constraint/export_session.go`.
- D2 (P1): release-side debounce — an arriving none/default control holds the outgoing cap for an
  expiry-confirm-style dwell before unseating (mirror of the 9 s expiry debounce; fail-closed
  direction, delaying a *release* by seconds is safe). This also covers the suspected primary
  churn mechanism (transient no-cap during delete→replace turnover, 3.9 kW-class peaks) — confirm
  with a hub-journal capture during a churn re-run.
- Tests: cap 0↔500 W flip every 4 ticks with 1-tick-lagged meter model (ceiling continuity, no
  over-band sample post-convergence); 1-2-tick 150 W excursions under a 0 W cap must NOT breach,
  sustained ones must; shadow parity.

**H4 · P1 · Clock-step robustness (CS-1, CS-2, CS-4, ED-2)**
- CS-1: make the server-time observation atomic — capture ONE monotonic `time.Time` at `/tm` parse;
  derive the anchor and the published timestamp from it (publish a single `server_ts`); the rewalk
  republish recomputes from `clk.ServerNow()`, never fresh-wall + cached offset. Tests: injected
  ±3600 s steps between /tm parse and publish — anchor unchanged; plan-slot stability across steps.
- CS-2: give the northbound scheduler a debounced expiry (mirror the hub's `expiryConfirmWindowS`
  rationale) and make `failClosed`'s hold symmetric for forward steps: a still-served event that
  flips to expired in a single Evaluate is held one confirm cycle.
- STOCK-cadence gap (verifier correction): `confirmTicksFor(15s)` = 1 tick — raise the hub's expiry
  confirm to `max(2 ticks, 9 s)` so the debounce is real at product cadence.
- CS-4: `onCSIPControl`'s stale-at-adoption age check → monotonic/anchored arithmetic.
- ED-2: make `doGet`'s same-path retry conditional (skip when the failure consumed the read
  deadline) so a per-resource stall doesn't double the walk blackout.

**H5 · P1 · Fail to DefaultDERControl on expiry-during-outage (ED-3) — *design decision, see IV-2***
- Carry the highest-priority program's DefaultDERControl on the retained bus doc (additive fields on
  `bus.ActiveControl` or a second retained topic); the hub's expiry-drop path falls back to
  enforcing it (`Source="default"`) instead of nil. IEEE 2030.5 event-end semantics revert to the
  default, not to unconstrained. Test: adopt event with default present, age past ValidUntil with
  walks sealed ⇒ enforcement degrades to default limits.

**H6 · P1 · EV plan-follow architecture (A-1/A-2/A-3) — *product decision, see IV-1***
Recommended: option (b), bounded — the plan owns the EV axis **only when it actually modelled an EV**:
- Gate `applyPlanRule`'s EV block on the plan having a usable EV action space (the
  `EVSetpointW=NaN` sentinel **plus** the `EVMaxChargeKw≈0` edge per A-2's verifier — gate on
  "planner had non-zero EV actions", not just capacity), else fall through to
  `applyEVChargingRule` — passing the **pre-zeroed** surplus (applyPlanRule returns surplusW=0,
  which would floor the reactive rule at 6 A; the fall-through must see the real surplus).
- Add a session-connect → `Engine.RequestReplan()` non-blocking wake (≈30 LOC, plannerLoop stays
  the single dailyPlan writer) so modelled-EV sites replan promptly instead of after ≤900 s.
- Fix the lying doc comment at planner.go:213. Regression tests: unmodelled-EV battery site with an
  active session gets a reactive command, never a standing 0 A; modelled-EV site replans on connect.

**H7 · P0 · lexa-proto: `write704` blind read-modify-write (E2) — *paired-PR discipline (MTR-4)***
- In `derbase.write704` (and the 703 twin): validate the read block before mutation — refuse to
  write (error out) when any referenced SF register decodes to the not-implemented sentinel or when
  >N registers of the block read 0x8000; scope the write-back to the target sync-group's register
  span (preserves PF+Ext atomicity with a far smaller poison surface).
- Fix the second defect the verifier surfaced: `View.SetFloat` silently no-ops on a sentinel SF —
  today that can produce *enable-without-setpoint* even absent faults. Make it an error.
- Ships as a lexa-proto commit + BOTH `proto.pin` bumps + BOTH `vendor/lexa-proto/` regens in one
  session; CI lockstep check must pass.
- Sequencing interlock: 29e9552 (two-sided PF) **increases** corrective 704 write frequency (CC-3),
  widening exposure to this bug — land H7 before or with any adv-active bench soak.

**H8 · P0 (deploy) · Ship the pagination fix + build provenance (PAG-1)**
- `make wolfssl-arm64 && make build-arm64`, deploy **lexa-northbound** to 69.0.0.2, re-run
  csip-pagination-walk (expect PASS through the corrected runner).
- Add per-service build provenance: surface `internal/buildinfo.Version` on every service (a
  `lexa_build_info` metric and/or /status field) and make the QA runner preflight assert the DUT
  SHA — this class of "tested the wrong binary" becomes structurally impossible.
- Step 6c's bare ActiveDERControlList fetch: document as unpaged-by-design (zero consumers today)
  or page it for symmetry (P2).

**H9 · P1 · Fixed-PF excitation + band (PF-1/2/3/4/7)**
- PF-1: when |W| ≥ floor and |PF| is in-band, additionally require `sign(m.Var)` to agree with the
  commanded excitation (dead-band around Var≈0 where commanded PF≈1); optionally read back
  `PFWInj_Ext` on the slow reassert. Test: commanded 0.90 over-excited, measured 0.90 with Var<0 ⇒
  diverged.
- PF-3: for commanded PF ≥ ~0.98 the ±0.02 band cannot detect accept-but-ignore — use the Var
  mismatch as the detector there, or document the limitation in the band's contract.
- PF-2: band-edge write-churn — verify episode pacing bounds corrective writes; add re-arm
  hysteresis if not (same boundary-dither class as H1/H3).
- PF-4/7: restore the far-side divergence test; fix the stale "one-sided" comments.

### Workstream Q — QA suite (csip-tls-test, mayhem oracles + runner)

**Q1 · P0 · Runner integrity (MAY-1, DASH-3)** — INCONCLUSIVE exits non-zero (or requires
`--allow-inconclusive`); require `finished==true` and all-selected-judged else exit 2; loud
"N INCONCLUSIVE — not judged" banner; handle SIGTERM like SIGINT (post abort); update the docstring
contract. *Everything else in this plan is re-verified through this runner — do it first.*

**Q2 · P0 · Identity-aware oracles (ED-1, D5, MAY-3/DASH-1, MAY-9)**
- `armAfterCapAdopted` matches the scenario's own `cons.MRID` (postCap already returns it); the 60 s
  deadline fallback marks INCONCLUSIVE instead of arming anyway (D5's second path).
- `scanSamples` adoption accounting compares `AdoptedMRID`, not (type, limit).
- Adv oracles: add `MRID` to `advReportMsg`, match against the injected mrid (hub already publishes
  it — **no hub change needed**, per verifier); `advReportWithRetry` treats present-but-mismatched
  as not-yet-present; on exhaustion report the mismatch as INCONCLUSIVE with attribution.
- `resetForScenario` clears the adv retained topics; inter-run cap hygiene: size events to the hold
  (no straddle) and/or wait out a leftover cap before arming.

**Q3 · P1 · Premise/efficacy assertions (MAY-2)** — diagnoseStale's fault-didn't-take branch →
INCONCLUSIVE, not PASS; pagination scenario asserts gridsim actually *served* a paged response
(admin counter — mTLS makes a direct probe infeasible); constraint scenarios assert injected PV
materialized (`SolarPossibleW ≈ pvHighW` on ≥80% of window) else INCONCLUSIVE; count injection
errors into verdicts.

**Q4 · P1 · Ground-truth backstops + un-weaken fbcaca0 (MAY-4, AUSCAP-1/2)**
- aus-load-cap: PASS-on-CannotComply becomes conditional on best-effort — tail sheddable load ≈ 0
  (EV at curtailed floor, battery not charging) or `gross − sheddable ≤ limW + tol`; add
  `TestDiagnoseAusLoadCap_Degraded_SheddableLoadRemains`; add a **meetable** load-cap companion
  scenario so `applyAusLoadLimitRule`'s levers have real coverage again.
- diagnosePaginationWalk: gate PASS on INV-EXPORT-clean/TailClean, not just AdoptedMRID.
- Document the CannotComply verdict standard once (PASS-if-legitimately-unmeetable with best-effort
  proof; DEGRADED otherwise) and align the three oracle families to it deliberately.

**Q5 · P1 · CannotComply attribution isolation (EV-9)** — scope gridsim's alert accounting per
scenario window/mRID ownership (end-of-event Responses from a prior scenario's control must not
count against the current one); then re-run openadr-limit-adopt ~10× solo + paired after mqtt-storm
to close the omitted 09:39 FAIL properly; document the outcome in the QA report addendum.

**Q6 · P2 · Coverage gaps (CC-1/4/5/6/11)** — telemetry MUP-roundtrip scenario (gridsim already
serves /mup; assert new readings land, VAr/Wh types when flags on) + health-logger slots for
telemetry/openadr; lexa-openadr VEN scenarios (needs deploy wiring first); watchdog-restart,
plan-heartbeat-stalled, api-auth-reject, cert-expiry-warn scenarios; flow-reservation needs a
gridsim endpoint (scope decision, IV-3); note the broker-auth boundary is untestable on the Yocto
bench (anonymous broker) — record as a bench limitation, don't fake it.

**Q7 · P2 · Flake/repeat support (MAY-7)** — first-class `--repeat N` / flake detection so
intermittents like this campaign's are visible in a standard run, not only in ad-hoc soaks.

**Q8 · P0 · Health gates (MAY-6, DASH-5)** — per-scenario preflight of every sim (`/state` 200) and
mid-run death detection producing an **INFRA** verdict distinct from FAIL/BLIND; retire the
"preventively restart modsim" operator ritual once S1 lands.

**Q9 · P2 · Timing preconditions (DRIFT-5)** — mayhem.py preflights FAST timing
(engine_interval_s) the way mayhem-campaign.sh already does.

**Q10 · P1 · Re-verification of the session's fixes under shared config (CC-2, CC-9)** — after H2/H9
land: one combined-config bench pass covering both commits (their verification configs were mutually
exclusive; the PF fix currently runs live under the exact config where its scenario is
INCONCLUSIVE); restore `enforce_aus_limits:false` per the report's own instruction (or decide to
keep it and update the profile).

### Workstream S — Simulations (csip-tls-test/sim)

**S1 · P0 · modsim resilience (E1)** — `simapi.writeJSON` sanitizes non-finite floats (degrade to
null + log once, never 500); same guard in `broadcastLoop` (currently silently mutes WS);
`advSnapshot` maps NaN to null-able fields; mark SF registers read-only in the sim's RegisterMap
(real-device fidelity — makes the hub's bad write-back an *observable divergence*); regression test
hammering /inject+/fault through a nan-sentinel window asserting /state stays 200.

**S2 · P2 · evsim polish (EVSIM-CLEAR-4/5/6)** — vehicle-side current limit in the model; 2.0.1-side
clear test; fix the stale handler comment.

**S3 · P1 · gridsim support for Q3/Q5** — per-scenario alert scoping (window/mRID ownership);
"served-paged-response" counter for the pagination premise check; (optional, IV-3) FlowReservation
endpoints.

**S4 · P1 · Post-fault register-hygiene oracle (E2's bench half)** — after transport-fault windows,
sample the sim's registers for 0x8000 in writable 704 registers: turns a hub bad write-back from
invisible into a FAIL. (Lands with H7 so it goes green, not red.)

### Workstream D — Dashboard application (csip-tls-test/cmd/dashboard)

**D1 · P1 · Engine run lifecycle (DASH-3/4, MAY-8)** — server-side run lease/heartbeat with
auto-abort on client death (kills the 409-stranding class); execution deadlines on the SSH helpers
(a hung remote command currently wedges `Running=true` indefinitely); live findings print scenario
ids.

**D2 · P2 · Verdict surfaces (DASH-2, MAY-5)** — machine-readable INFRA marker for probe/scrape
failures, folded consistently (a correctly-admitting hub must not FAIL because /admin/alerts was
unreachable).

**D3 · P1 · Programmatic health surface (DASH-5)** — per-sim health endpoint the engine and runner
preflight consume (pairs with Q8).

*(The oracle-logic changes live in cmd/dashboard too but are tracked under Q2–Q5.)*

### Workstream P — Repo & deployment hygiene (protects everything above)

**P1 · P0 · One-line conformance fix (DRIFT-3)** — `configs/northbound.json`
`legacy_cannotcomply_code` → false (or drop the key; code default is false; gridsim Table-27 accept
is already committed). Same commit: update the stale CLAUDE.md footnote + config.go comment. A
redeploy today regresses the hub to nonstandard 0xF0 codes.

**P2 · P1 · A real deploy path for the dev kit (DRIFT-1/2, DRIFT-7)** — `scripts/deploy-devkit.sh`
(or `--yocto` mode): no mosquitto_passwd/apt/useradd, preserves/patches the committed **bench
profile** (metrics_addr 0.0.0.0, advanced_der, reconciler.adv, FAST timing, mqttproxy repoint…)
instead of hand-edits; plus a drift-check script (bench config vs profile) wired into bench-up. The
metrics_addr gap and this campaign's six hand-set values are structural consequences of not having
this.

**P3 · P0 · Evidence & branch preservation (DRIFT-4, CC-8)** — soak artifacts copied to
`../csip-tls-test/logs/qa/soak-20260716-artifacts/` (**done during this audit**); commit the QA
report + audit findings + this plan; push/back up all three repos (lexa-proto has **no remote** —
65 unpushed commits across repos ride one workstation disk); open the merge-review path for
`standards-buildout` and `feat/dashboard-v2`.

**P4 · P2 · Standby-Pi foot-gun (DRIFT-8)** — `hub-replay-tune.sh` defaults to 69.0.0.1: make the
target explicit/required.

**P5 · P1 · Correct the record** — append an audit addendum to `QA_SESSION_REPORT_2026-07-16.md`
(finding C → deploy gap; finding E → corruption + E2; EV-9 disclosure; §5/§6 tally corrections) so
the next reader isn't misled; leave the original text intact as history.

---

## Part III — Sequencing & verification protocol

**Phase 0 — stop the bleeding (no design risk):** P1 (config flip) · P3 (preserve/commit/push) ·
H8-deploy (ship lexa-northbound; the P1-1 field failure is live until then) · Q1 (runner
integrity) · Q8+S1 (health gates + modsim fix) · restore `enforce_aus_limits` per Q10.
*Exit: the bench can be trusted to re-verify everything that follows.*

**Phase 1 — hub safety core:** H1 (reserve latch + guard hold-bias, shadow-parity) · H2
(EXPCAP-1/2/3) · H3-D3 (zero-lever debounce). Each: failing test → fix → `make test` → bench
scenario re-run (soc-dither ×10, export-dither ×10, openadr pair ×10 through the fixed runner).

**Phase 2 — control continuity:** H3-D1/D2 (guard continuity + release debounce, with a
journal-instrumented churn re-run to confirm the primary mechanism) · H4 (clock atomicity +
scheduler debounce) · H5 (default fallback — after IV-2 sign-off).

**Phase 3 — proto + adv:** H7 (write704, paired pin bump) → S4 (register-hygiene oracle) → H9 (PF
excitation) → re-run the adv isolation trio + a nan-sentinel window with the new oracle.

**Phase 4 — EV architecture:** H6 after IV-1 sign-off; re-run the ocpp16 pair + ev scenarios
(A-4's oracle attribution fix rides along in Q-work).

**Phase 5 — close the loop:** Q10 combined-config verification · restart the constraint-shadow
soak clock (EXPCAP-2/H1/H3 all touched shadow parity) · Q6 coverage additions · full curated
mayhem sweep ×3 cycles through the corrected runner · updated QA report addendum (P5).

**Standing rules for this work** (radioactive-code discipline):
- No hub behavior change without a pinned failing test first, and no "fix" that only moves a
  threshold — every boundary defect gets *state* (latch/hysteresis/debounce), not a wider band.
- Every optimizer change lands on BOTH sides (live cascade + constraint port) in the same commit,
  with a parity test — CLAUDE.md's do-not-strip-either-side rule.
- lexa-proto changes only as paired PRs with both pins + both vendor trees regenerated.
- Bench verification only through the Q1-fixed runner, with per-scenario health gates on, and
  identity-aware oracles for any scenario being used as proof.
- FAST-cadence results don't certify STOCK behavior for timing-sensitive fixes (H1/H3/H4): re-run
  the affected scenario at STOCK before calling it closed (CS-1's product exposure is *worse* at
  STOCK).

## Part IV — Decisions needed from the owner

1. **H6 (EV plan-follow):** approve option (b) — plan owns the EV axis only when it modelled an EV,
   reactive rule otherwise + session-connect replan wake? This changes live EV behavior on
   battery+EVSE sites (today: standing 0 A suspend when the EV is unmodelled). Alternatives and
   trade-offs in AUDIT findings A-1.
2. **H5 (expiry-during-outage):** approve falling back to the last-known DefaultDERControl instead
   of unconstrained? 2030.5-correct and fail-closed, but it is a control-behavior change and adds
   (additive) bus schema.
3. **Scope:** FlowReservation + lexa-openadr bench coverage (CC-4/5) in this campaign, or defer to
   the next one? (Both need new sim/gridsim surface.)
4. **Merge strategy:** PR review + merge `standards-buildout` and `feat/dashboard-v2` after Phase 1,
   or hold everything on branches until Phase 5? And where should `lexa-proto` get a remote?
