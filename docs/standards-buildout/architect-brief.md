# Architect Brief — LEXA Hub standards-coverage build-out

Date 2026-07-14. Inputs: standards audit (`../audit/standards-coverage-audit.md`), DERMS expert review
(`../audit/derms-expert-review.md`), 13 digests (`../digests/*.md`), repo CLAUDE.md. Owner decisions
(recorded 2026-07-14, explicit user selections): V2G = type-system enablers only; scope = expert Phase 1
+ curve/PF/energize plumbing; OpenADR 3.1 CP VEN = build now; CSIP-AUS = include in build scope.
Schedule-envelope constraint tier (expert #13) explicitly deferred to a later campaign. Certification
campaign itself (lab time) is calendar, not code — out of scope here, but this scope IS the closure set.

## Approved scope (work items)

**A. Reporting cluster (cert prerequisite + commercial front door)**
- A1. Extend `bus.Measurement` (or sibling message) with reactive power (VAr), operational state,
  alarm bitmap, and interval energy (Wh import/export). Sources already parsed at device layer
  (701.Var/St/ConnSt/Alrm; meter 201/203 VAR; TotWhImp/Exp offsets declared-unparsed). Envelope `v`
  bump discipline per AD-006. This is the enabler for A2–A4.
- A2. HTTP PUT verb in `internal/tlsclient` (+ httpwire as needed) and client-initiated reporting:
  PUT DERCapability / DERSettings / DERStatus / DERAvailability. **Includes the GFEMS aggregation
  design**: what a single EndDevice reports for solar+battery+EVSE behind one PCC (DERType 83
  composite vs dominant type; summed ratings; settings ≤ ratings invariants that CORE-014
  operator-checks; SoC in DERStatus.stateOfChargeStatus; modesSupported truthful to what the hub
  actually enforces). Cadence: capability/settings at startup + on change; status at DERList pollRate.
  Requirements detail: digests/csip-implementation.md (G28–G30, Tables 12/13), csip-conformance-tests.md
  (CORE-009/014, BASIC-028, UTIL-002).
- A3. Telemetry: add VAr MirrorMeterReading (uom 63) to cmd/telemetry (CSIP Table 2 mandatory); add Wh
  posting only if trivially fitting MUP model — else Wh stays bus/API-side (position as operational/M&V).
- A4. LogEvent poster: CSIP Table 14 alarm codes 0–21 (alarm+RTN pairs, functionSet=11, timestamps),
  POST to EndDevice LogEventListLink; sources: 701 Alrm bitfield transitions + breach episodes.
- A5. CannotComply → standard IEEE 2030.5 Table 27 Response codes. Mapping (see digests/ieee-2030.5-2018.md
  §Response): rejected-at-receipt → 252/253; started-then-cannot-sustain → 8/10 (partial/no-participation)
  or abort codes 13/14 as fits episode semantics; keep 0xF0 emission available behind a bench/config flag
  during transition (gridsim compat), default standard codes.
- A6. Wire `VerifyRegistration` (PIN) into the production walk (CORE-003/BASIC-001); on mismatch: alarm +
  stop using server per 2030.5 §6.9.2(c) — decide fail posture carefully (fail-closed vs enforce-but-alarm)
  against the hub's existing enforce-but-verify philosophy.
- A7. Verification sweep (mostly tests/bench, some code): (i) prove COMM-004 reject-paths (bad-MICA,
  self-signed) fail closed in wolfSSL config, pcap-able on the bench; (ii) ERR-001: 301/302
  redirect-following in tlsclient/httpwire — add if missing; (iii) SunS marker probe at bases 0 and 50000
  in addition to 40000 (scanner.go currently 40000-only).

**B. OCPP southbound**
- B1. OCPP 1.6J dual-stack CSMS: version-dispatching listener; 1.6 bridge behind the same `bridge.Apply`
  seam (Boot/Start-StopTransaction/MeterValues/StatusNotification/SetChargingProfile-TxDefault/
  ClearChargingProfile/TriggerMessage equivalents); reuse reconciler shell + plausibility gates
  ("Accepted ≠ convergence" is protocol-agnostic). Security: TLS+Basic Auth analog under the same
  fail-closed config gate (bench opt-out preserved). 2.0.1 remains the preferred/certified path.
- B2. Authorize/pairing gate (roadmap R8): unknown chargers must NOT auto-become plant. Pending-station
  approval flow (retained `lexa/ocpp/pending` already exists as surface; add accept/deny + persisted
  allowlist + optional idTag/Authorize handling). Applies to both 1.6 and 2.0.1 paths.
- B3. ClearChargingProfile support (2.0.1 + 1.6 analog) — release semantics instead of re-set-higher-limit.

**C. Curve/PF/energize middle plumbing (CSIP cert closure long pole)**
The southbound halves are DONE in vendor/lexa-proto/derbase (704 PF sync groups, VarSet, 705/706/711/712
curve writes incl. AdptCrvReq/Rslt adoption handshake, 707–710 trips, 703 enter-service). Missing middle:
- C1. Carry advanced controls end-to-end: extend the northbound→hub ActiveControl path and AD-013 desired
  docs with: opModEnergize, opModFixedPF (inj/absorb + excitation), opModFixedVar, curve-mode references
  (volt-var/volt-watt/freq-watt/watt-PF + freq-droop params + ride-through trip curve sets), ramp defaults
  (setGradW/setSoftGradW), opModTargetW decision (parse-through at minimum). Scheduler passthrough replaces
  the extendedListToSimple silent-drop; add the ignored-curve alarm (backlog item) for anything still dropped.
- C2. Reconciler execution per axis: desired-doc → derbase writes with per-axis convergence semantics —
  curve adoption is verify-AdptCrvRslt=COMPLETED then re-read curve 1 (never trust the write); PF/var
  convergence judged from measured PF/Q (needs A1); energize via 703 ES + measured cessation; respect
  legacy-12x enable-rewrite rule where a device only offers 12x (per-series handling), 7xx immediate-effect
  otherwise. One-writer discipline: reconciler stays the single writer; interlock stays senior.
- C3. DefaultRvrtTms wiring (device-side reversion as belt to reconciler's suspender) — small, in-scope.
- C4. Mode-priority correctness: honor IEEE 1547 §4.7 ordering & the reactive-mode mutual-exclusivity
  (one of const-PF/volt-var/watt-var/const-Q at a time) in scheduler/optimizer/reconciler; volt-watt ∧
  droop lesser-value rule noted for device-side (device does physics; hub must not fight it).
- C5. Shadow-gate discipline: any optimizer/cascade change rides the constraint-stack shadow harness
  rules (AD-007/TASK-059) — new enforcement axes must have convergence backstops and breach/CannotComply
  integration like the existing export/gen/import axes.

**D. V2G enablers (type-system only — NO protocol build)**
- D1. `EVSECommand`/desired-doc EVSE types become signed (setpoint W or ±A), planner gains EV-as-storage
  discharge term (DP asset like battery, honoring EVGoal departure/SoC), breach/limit treatment of EV as
  DER. Actuation stays charge-only (clamp ≥0 at the OCPP bridge) until a V2X path exists — the point is
  to kill the charge-only construction in types/planner now.

**E. OpenADR 3.1 CP VEN (new service `lexa-openadr`)**
- E1. Pure-Go (stdlib TLS, no wolfSSL/CGo) VEN: OAuth2 client-credentials, programs/events/reports
  (+subscriptions optional), Continuous Pricing profile first (price/GHG/alert events). Mirrors
  lexa-northbound's shape: poll VTN → publish retained bus docs. Mapping: PRICE events → existing
  tariff/SetPrices compile path (cmd/hub/tariff.go seam); IMPORT/EXPORT_CAPACITY_LIMIT →
  GridState limit path **with source arbitration vs CSIP** (CSIP wins on conflict; document precedence);
  TELEMETRY reports ← existing bus measurements (needs A1 for Wh/VAr-quality reports); opt-out ←
  breach episodes. New broker user + ACL rows (mosquitto-lexa.acl), systemd unit, metrics port
  (next free: 9106), config JSON, watchdog Type=notify — full service-isolation pattern per CLAUDE.md.
  Details: digests/openadr.md (3.1 REST resources + certification profiles).

**F. CSIP-AUS (build scope; SPEC-GAP CAVEAT)**
- F1. **The standards library has NO CSIP-AUS document** — AUS items must be scoped from domain knowledge
  and marked "verify against CSIP-AUS v1.1a before cert". Known shape: dynamic operating envelopes via
  opModExpLimW/opModImpLimW (ALREADY enforced w/ convergence backstops — the hub's oddball strength),
  plus opModGenLimW/opModLoadLimW (parsed today, unenforced — enforce them; GenLimW = generation cap
  distinct from export cap, LoadLimW = load cap distinct from import cap), short-cadence control refresh
  (envelopes reissued ~5-min), DER capability/settings reporting per AUS profile, in-band registration
  variants. Architect: structure F as (a) enforce GenLimW/LoadLimW axes (concrete, testable now, rides C's
  plumbing pattern), (b) an AUS-conformance checklist doc flagging every unverifiable item for when the
  CSIP-AUS spec is obtained.

## Hard constraints (repo invariants — non-negotiable)

1. Service isolation: processes communicate ONLY via Mosquitto MQTT; per-service broker creds + ACL
   (re-derive ACL matrix from actual Subscribe/Publish call sites — it's an authorization boundary).
2. Bus messages: Envelope `v` versioning per AD-006 (subscribers version-check; additive evolution);
   no NaN in JSON (*float64 nil-absent + Finite() checks); QoS via bus.PubQoS.
3. AD-013: retained desired docs are the SOLE command path; reconcilers are the single writer/reasserter
   per device class; Tier-0 interlock senior to reconciler; never add a second writer.
4. Engine state single-writer (TASK-067): mutations via cmdCh commands; actuator registration pre-Start;
   safety tick untouched; tick budget — actuator publishes async, never block on PUBACK.
5. Optimizer changes respect AD-007 tiers (safety > compliance > economics) and the shadow-harness gate
   discipline for the constraint stack; the legacy cascade is authoritative until per-axis flips.
6. wolfSSL/CGo only in lexa-northbound + lexa-telemetry; wolfssl.Init once per process; cipher pin
   ECDHE-ECDSA-AES128-CCM-8 TLS1.2 untouched. lexa-openadr must be pure Go.
7. lexa-proto changes (sunspec/derbase/ocppserver/csipmodel) follow the pinned-commit paired-PR
   discipline: both repos' proto.pin + vendor/lexa-proto regenerated in the same session; local dev
   via go.work. OCPP 1.6 support lands in lexa-proto/ocppserver (or a sibling package) — plan the pin bump.
8. Crash-only (AD-011): no blanket recover(); retained topics re-seed state; watchdog kick sites per
   service table in CLAUDE.md.
9. Defensive fault-handling section of CLAUDE.md: never strip convergence backstops/meter floors;
   trust measurement, not the command — extends to every new control axis (incl. curve adoption results).
10. journald caps / FLASH_BUDGET: no new per-tick Info logging.
11. Config: additive JSON keys with safe defaults; unknown keys warn-not-fail; product defaults
    fail-closed (bench opt-outs explicit), per AD-008/WS-1 patterns.
12. Bench is currently occupied/offline-constrained (dev-kit offline; hub-pi is the deploy target) —
    plan for `make test` (-race, pure-Go) + cross-compile as the verification floor; hardware/bench
    validation queued separately.

## Deliverables requested from the architect

1. `architecture.md` — high-level architecture: how each scope item lands across services/packages;
   new/changed bus topics + message types (with ACL impact table); new/changed config keys; lexa-proto
   vs lexa-hub split; data-flow diagrams (text) for (a) DER* reporting path, (b) curve-control path
   northbound→scheduler→desired-doc→reconciler→derbase, (c) OCPP dual-stack listener, (d) lexa-openadr.
   Explicit design DECISIONS with rationale for: GFEMS aggregate DERCapability semantics; PIN-mismatch
   posture; CannotComply code mapping; CSIP-vs-OpenADR control-source precedence; EVSE signed-setpoint
   representation; curve desired-doc schema (how a 10-point curve + adoption state rides a retained doc).
2. `work-packages.md` — the plan broken into implementable work packages: each with scope, files/packages
   touched, dependencies (DAG), size (S/M/L), test plan (unit + which existing regression suites),
   acceptance criteria tied to the conformance-test IDs they close (e.g. "closes BASIC-008"), and
   explicit lexa-proto pin-bump points. Order them so the tree is releasable after each package
   (additive, feature-flagged where risky). Flag the packages suitable for parallel agent implementation
   vs those needing serialization.
3. Risk register: top 10 risks (e.g., desired-doc schema churn, dual-stack OCPP regressions, ACL drift,
   GFEMS aggregation semantics wrong vs lab expectations) with mitigations.
