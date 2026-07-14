# LEXA Hub Standards Build-Out — Work Packages

Companion to architecture.md (same dir). 17 packages. Tree is releasable after every package
(additive + feature-flagged). Verification floor: `make test` (-race, pure-Go) + `make build-arm64`
cross-compile; bench/hardware validation queued separately (bench occupied).

## Serialization rule (read first)

Files `internal/bus/{messages,desired,topics,envelope}.go` are a SINGLE-OWNER LANE: WP-2 → WP-8 →
WP-9 → WP-13(bus slice) → WP-14 → WP-15(bus slice) must land in that order (each adds types/fields/
SupportedV arms to the same files). Everything else parallelizes per the DAG. New bus TYPES go in
new files (bus/dersite.go, bus/curves.go, bus/openadr.go...) to shrink the conflict surface, but
envelope.go constants + topics.go SupportedV arms still serialize.

## DAG (edges = "depends on")

```
WP-1 (proto) ──► WP-2 ──► WP-4, WP-5, WP-6(also←WP-1)
WP-1 ─────────► WP-6, WP-7(constants), WP-12
WP-3 ──────────► WP-4
WP-2 ══bus lane══► WP-8 ──► WP-9 ──► WP-10
WP-8 ──────────► WP-11
WP-12 ─────────► WP-13
WP-9 ══bus lane══► WP-13(slice) ══► WP-14 ══► WP-15(slice)
WP-2 ──────────► WP-15 (reports need enriched Measurement)
WP-11 ─────────► WP-16 (checklist references the shipped axes)
ALL ───────────► WP-17
```
Parallel lanes once WP-1 + WP-2 are in:
- **Lane N (northbound/reporting)**: WP-3 → WP-4; WP-5; WP-6; WP-7 — WP-3/5/6/7 mutually parallel.
- **Lane C (control plumbing)**: WP-8 → WP-9 → WP-10; WP-11 forks after WP-8.
- **Lane O (OCPP)**: WP-12 → WP-13.
- **Lane E (OpenADR)**: WP-15 (bus slice coordinated with lane owner).
- WP-14 rides the bus lane tail; WP-16 anytime; WP-17 last.
Suggested agent assignment: one agent OWNS the bus lane end-to-end; lanes N/O/E are independent
agents; WP-10/11 (southbound+optimizer) one agent (both touch convergence machinery).

---

### WP-1 — lexa-proto change-set (THE pin bump) — Size L
**Scope**: the single batched proto change (architecture §4): (a) derbase.Measurements additive
fields (OpSt/InvSt/ConnSt/Alrm/WhImpTotal/WhExpTotal) + ReadMeasurementsM701 populates from
Parse701; (b) csipmodel LogEvent type + Table 27 Response constants + DER*Full marshal round-trip
tests; (c) sunspec.Scan probes bases 0/40000/50000 (40000 first); (d) new `ocppserver16` package
(1.6J CentralSystem: Core+SmartCharging, TLS+BasicAuth constant-time, handler seam mirroring
ocppserver).
**Files**: lexa-proto: derbase/derbase.go, csipmodel/{resources,logevent}.go, sunspec/scanner.go,
ocppserver16/* (new). lexa-hub + csip-tls-test: proto.pin, vendor/lexa-proto/ regenerated —
paired PRs, same session (MTR-4/TASK-024 CI gate).
**Deps**: none. **Flags**: none (pure library surface; nothing consumes it yet).
**Pin bump**: THIS IS IT — the only planned bump. Sub-scopes are independent inside one commit;
if 1.6 drags, split (d) into a second bump as the explicit fallback (2 bumps max).
**Tests**: derbase table tests (701 regs → new fields; NaN defaults on 103 path); csipmodel
xmlns round-trips (DERCapabilityFull/Settings/Status/Availability/LogEvent); scanner 3-base sweep
against sunspecsweep fixtures; ocppserver16 handshake + basic-auth unit tests (mirror
sp2_auth_test.go). csip-tls-test: `go test ./tests/` compile pass with new vendor tree.
**Acceptance**: both repos pin identical SHA, CI check-proto-pin green; SunSpec CLI-4 base-probe
gap closed at lib level; no lexa-hub behavior change (vendor swap only).

### WP-2 — bus.Measurement enrichment + modbus publishers (A1) — Size M
**Scope**: additive Measurement fields (var_w, va, pf, op_state, conn_state, alarm_bits,
wh_imp_total, wh_exp_total; architecture §2.2); Finite() extension; cmd/modbus populates from
device.Measurements (inverter/battery via new derbase fields) and meter (parse 201/203
TotWhImp/Exp using the declared offsets — lexa-hub internal/southbound/meter.go); plausibility:
Wh totals must be monotonic non-decreasing per device else withheld (scale-factor suspicion,
existing gate pattern main.go:486-501).
**Files**: internal/bus/messages.go (LANE), internal/southbound/meter/*, cmd/modbus/main.go.
**Deps**: WP-1. **Flags**: none (additive fields; absent when device lacks them — G27).
**Tests**: bus/nan_reject_test.go + envelope_test.go extended; meter parse table tests (Wh
offsets, scale wrap via sunspecsweep); modbus plausible_test.go monotonic-Wh cases.
**Acceptance**: 1547 Table 29 monitoring set (P,Q,V,f,state,conn,alarms,SoC,Wh) present on the
bus with file:line evidence; old subscribers decode unchanged (fixture test with new payload
against pre-change struct).

### WP-3 — tlsclient PUT verb + redirect following (A2 enabler, A7ii) — Size S
**Scope**: buildPutRequest (CRLF guard reused); WolfSSLFetcher.Put/PutContext (mirror
Post/PostContext, fetcher.go:264-310; accept 200/201/204); 301/302 following per D3 (same-host,
`redirect_max` default 3, never scheme-downgrade), applied in doGet/doPost/doPut.
**Files**: internal/tlsclient/{request,fetcher,response}.go, cmd/northbound/config.go
(redirect_max key).
**Deps**: none. **Flags**: redirect_max (0 disables).
**Tests**: parsing_test.go + new redirect unit tests (mock reloadable/conn); httpwire fuzz corpus
gains 301/302+Location responses; injection-guard tests for PUT path/host.
**Acceptance**: **closes ERR-001** (code half; bench pcap later); PUT usable by WP-4.

### WP-4 — DER* reporting: hub dersite + northbound derreport (A2) — Size M
**Scope**: bus.DERSiteReport (new file bus/dersite.go + envelope const + SupportedV + topic
`lexa/hub/dersite`); cmd/hub aggregator (GFEMS math per D2: ratings sums, settings ≤ ratings by
construction, modesSupported truth-mask, SoC/alarm aggregation; change-detect + 60 s min republish,
async publish); internal/northbound/derreport: PUT DERCapability/DERSettings at startup + on
dersite change, PUT DERStatus/DERAvailability at DERList pollRate; suspend on PIN-freeze (D4 hook,
lands with WP-7); ACL rows (hub write / northbound read).
**Files**: internal/bus/dersite.go (+LANE files), cmd/hub/{main,dersite}.go,
internal/northbound/derreport/* (new), internal/northbound/run/run.go (wire cadence),
systemd/mosquitto-lexa.acl, configs/northbound.json (`der_report`).
**Deps**: WP-2, WP-3. **Flags**: `der_report` (default true; 404/405-skip per resource).
**Tests**: D2 aggregation table tests (multi-battery SoC weighting, omission-not-fabrication,
set≤rtg invariant, truth-mask vs enforcement flags); derreport cadence tests with fake Fetcher
(discovery.Fetcher seam); walk-loop regression (run tests) unchanged.
**Acceptance**: **closes CORE-009 (PUT half), CORE-014, BASIC-028**; UTIL-002's PUT mechanics
demonstrated for the single-EndDevice GFEMS profile. Utility can see nameplate/SoC/status.

### WP-5 — telemetry VAr + Wh MMRs (A3) — Size S
**Scope**: quantity rows: VAr (uom 63, kind power-reactive, mult 0) and Wh imp/exp (uom 72,
kind energy, flowDirection split) in cmd/telemetry postMeasurements (main.go:404-457); NaN-skip
preserved; config `post_var`/`post_wh`.
**Files**: cmd/telemetry/{main,config}.go, configs/telemetry.json.
**Deps**: WP-2 (fields on bus.Measurement — telemetry consumes the bus, not devices).
**Flags**: post_var (true), post_wh (true).
**Tests**: telemetry unit tests for MMR encoding (mirror existing W/V/Hz table); ReadingType
mRID-discipline fixture.
**Acceptance**: **closes the BASIC-029 VAr gap** (all four CSIP Table 2 mandatory quantities
posted); Wh positions M&V/OpenADR usage reports.

### WP-6 — LogEvent pipeline (A4) — Size M
**Scope**: cmd/hub alarm-edge detector: 701 alarm_bits transitions (per device, debounced,
`logevent_min_interval_s` floor) + breach-episode onset/clear → bus.LogEventMsg on
`lexa/hub/logevent` (edge, QoS1, not retained); mapping table alarm_bits → CSIP Table 14 codes
0–21 (alarm+RTN pairs, functionSet=11, createdDateTime); northbound logevent poster: POST to the
EndDevice's LogEventListLink (href from walker; resources.go:137 already parses the link);
retry-once then drop with counter (crash-only: missed LogEvent is not state to spool); ACL rows.
**Files**: internal/bus/logevent.go (+LANE), cmd/hub/logevent.go,
internal/northbound/logevent/* (new), systemd/mosquitto-lexa.acl.
**Deps**: WP-1 (csipmodel.LogEvent), WP-2 (alarm_bits).
**Flags**: logevent_min_interval_s (rate floor; flash budget — edges only, no per-tick lines).
**Tests**: transition-table tests (bit set→alarm code, bit clear→RTN, pair completeness);
dedupe/rate-floor tests; poster unit test with fake Fetcher (201+Location).
**Acceptance**: **closes BASIC-027, G31/G32** — alarm+RTN pairs posted as they occur.

### WP-7 — PIN verify + Table 27 response codes (A5, A6) — Size S
**Scope**: (a) wire discovery/helpers.go VerifyRegistration into the production walk
(run.RunOnce after findSelfDevice) with D4 posture: adoption freeze + egress suspend + pin_ok on
certstatus doc + gauge; `registration_pin` config (0=disabled+WARN). (b) responses.Tracker: D5
mapping (252/253 at receipt-reject — new scheduler reject hook; 8 at episode onset; 3/8/10
end-of-event reconciliation; 13/14 where cross-program supersession is detected);
`legacy_cannotcomply_code` flag (default false).
**Files**: internal/northbound/{run/run.go,responses/tracker.go,scheduler/scheduler.go (reject
hook only)}, cmd/northbound/config.go, internal/bus/messages.go (CertStatus.pin_ok — LANE, tiny).
**Deps**: WP-1 (constants). Coordinate the one-field CertStatus touch with the bus lane owner.
**Tests**: tracker_test extensions (code emission per lifecycle path, episode dedupe unchanged,
legacy-flag byte-compat with today's 0xF0); failclosed_test.go: PIN-freeze holds LKG and blocks
new adoption; persist round-trip (WS-4.2 store) with new codes.
**Acceptance**: **closes CORE-003/BASIC-001 PIN elements; CORE-022/023 code discipline**; interop
bug #6 (0xF0) dead by default. Bench note: flip gridsim expectation in the SAME session
(csip-tls-test paired change) or set legacy flag in bench configs.

### WP-8 — Advanced-control carriage northbound (C1) — Size M
**Scope**: retire the extendedListToSimple/extendedDefaultToSimple silent drop
(walker.go:472-532) — ProgramState carries the extended base + resolved curves through
scheduler.Evaluate (scheduler logic keyed on scalar fields is UNCHANGED; extended content is
carried, not evaluated); publish.ToActiveControl emits the additive scalars (energize, PF, var,
gen/load lims, ramps, target_w, rvrt_tms_s, curve_set_id); new publish.Curves → retained
`lexa/csip/curves` (bus.CurveSet, content-hashed); ignored-content alarm for anything STILL
dropped (counter + WARN edge). Plausibility gates extend to new numerics (PF ∈ (0,1], var pct ≤
100, lims ≤ 1 GW — scheduler maxPlausibleLimitW pattern).
**Files**: internal/northbound/{discovery/walker.go,scheduler/scheduler.go,publish/publish.go},
internal/bus/{messages.go,curves.go} (LANE), systemd/mosquitto-lexa.acl (northbound write curves;
hub read).
**Deps**: WP-2 (lane order). **Flags**: none needed — new fields are inert until WP-9 consumes
them (hub ignores unknown keys today).
**Tests**: scheduler fuzz_test.go + failclosed_test.go re-run UNCHANGED (fail-closed semantics
must not move — acceptance gate); new passthrough tests (extended event → ActiveControl fields;
curve resolution → CurveSet hash stability); publish golden fixtures.
**Acceptance**: curve/PF/energize content visible on the bus with zero change to scalar
control adoption (cmd/hub state_test.go green untouched); ignored-curve alarm live (closes the
10_BACKLOG companion item).

### WP-9 — Adv desired doc + hub authoring (C1/C3/C4) — Size M
**Scope**: bus.DesiredAdvanced (new family, D6 schema) + topic + SupportedV; cmd/hub author
behind `advanced_der:"on"`: consume ActiveControl+CurveSet, D7 arbitration (reactive-mode
exclusivity, event>default, dynamic>static, ignored-mode alarm), device mapping via
DERStatusSummary.ModesSupported + device class + Has70x knowledge (from dersite/device config),
rvrt_tms computation (ValidUntil−now clamped to [60 s, 24 h]), content-hash dedupe, heartbeat
republish (desiredHeartbeatInterval pattern), async publishes; ACL rows.
**Files**: internal/bus/{desired_adv.go,topics.go,envelope.go} (LANE), cmd/hub/{adv.go,main.go,
config.go}, systemd/mosquitto-lexa.acl.
**Deps**: WP-8. **Flags**: `advanced_der` default "off" (flag off ⇒ author not constructed —
constraint_shadow precedent).
**Tests**: arbitration table tests (D7 cases incl. torn-state impossibility); doc golden
fixtures; heartbeat/dedupe tests (mirror desired_test.go); hub main wiring test.
**Acceptance**: adv docs published for capable devices when flag on; NO reconciler consumes yet
(releasable: docs sit retained, harmless); ignored-mode alarm counts.

### WP-10 — Advanced reconciler execution (C2/C3) — Size L
**Scope**: cmd/modbus adv shell (per device, driven by Has70x): curve axes write via derbase
Write* (adopt handshake) then RE-READ + compare (adopt_state=adopted only on readback match);
fixed PF/var convergence from measured PF/Var in observe(); energize via 703 ES + measured
cessation; legacy-12x per-series enable-rewrite for devices without 704+; corrective-write
backoff ≥ readback interval; interlock seniority (isTripped suppresses energize/connect-restoring
writes → InterlockHold); retained ReconcileReport(axis/adopt_state/curve_hash) per §2.2;
non-convergence feeds breachEpisodes (existing SubReconcileReport path); RvrtTms plumb: registry
exposes SetRvrtTms → derbase.Base.DefaultRvrtTms before 704 writes; `reconciler.adv`
off|shadow|active staged rollout.
**Files**: cmd/modbus/{reconcile_adv.go (new),main.go,config.go}, internal/reconcile (axis
extension or sibling advreconcile — prefer extending Field/Report kinds additively),
internal/southbound/registry.
**Deps**: WP-9. **Flags**: `reconciler.adv` default "off"; "shadow" = read doc + verdict logs +
metrics, zero writes (TASK-027 pattern); "active" flip is bench-gated.
**Tests**: reconcile_shell_test.go-style table tests per axis (adopt success/failed/ignored-ack;
one-sided convergence under volt-watt/droop; interlock suppression; 12x-series path); simulated
device via reconcileDriver fake; -race whole-package.
**Acceptance**: with sim inverter: **closes the execution half of BASIC-004/005/006/008/009/011/
012 (+007 via ramp defaults from WP-8/9) and CORE-012's "pass curve to inverter"** — lab-ready
pending bench soak; DefaultRvrtTms observed in writes (C3); no second writer introduced (grep
gate: only adv shell touches 703/705–712).

### WP-11 — GenLimW/LoadLimW enforcement (F-a) — Size M
**Scope**: GridState.GenLimitW/LoadLimitW; cmd/hub adoption from ActiveControl (WP-8 fields);
cascade rules behind `enforce_aus_limits` (gen cap: solar ceiling + battery-discharge cap with
meter-floor convergence backstop mirroring checkGenLimitConvergence; load cap: EVSE curtail +
battery-charge cap with NaN-hold counter mirroring checkImportConvergence); breach/CannotComply
integration (limit_type "generation-aus"/"load-aus"); mirrored shadow constraints in
orchestrator/constraint (TASK-061 pattern) running under constraint_shadow regardless of the
enforcement flag; planner StepConstraint gains gen/load lim fields (already carried on
DERScheduleSlot).
**Files**: internal/orchestrator/{model,optimizer,planner}.go,
internal/orchestrator/constraint/{genlimaus,loadlimaus}.go (new), cmd/hub/{state,main}.go.
**Deps**: WP-8. **Flags**: `enforce_aus_limits` default false; shadow-first (≥1 week bench),
then default-flip proposal.
**Tests**: optimizer_rules_test.go + convergence_test.go extensions (both cascade and shadow
copies — do not strip either side); planner_test constraint honoring; shadow 0-diff harness test.
**Acceptance**: CSIP-AUS dynamic-envelope axes (Exp/Imp/Gen/Load) all enforced with convergence
backstops; marked "verify against CSIP-AUS v1.1a" in WP-16's checklist (spec gap disclosed).

### WP-12 — OCPP 1.6J dual-stack (B1) — Size M/L
**Scope**: cmd/ocpp second listener via ocppserver16 (`port_16`, default 0=off; same cert/key +
basic-auth fields; same fail-closed gate: stations>0 && !bench && port_16>0 requires SP2 fields);
1.6 forwarders → SAME mqttBridge state (Boot/Start-StopTransaction→session lifecycle mapped to
the TransactionEvent-shaped stationState, MeterValues folding + plausibility gates reused,
StatusNotification→connector map); stationState gains proto tag; bridge.Apply dispatches
per-proto (2.0.1 SetChargingProfile | 1.6 TxDefaultProfile, chargingRateUnit=A, 10 s bound, L11
rejected-as-error verbatim); TriggerMessage analog.
**Files**: cmd/ocpp/{main.go,config.go,bridge16.go (new)}, configs/ocpp.json.
**Deps**: WP-1 (ocppserver16). **Flags**: port_16=0 default (1.6 fully off; zero regression
surface for the hardened 2.0.1 path).
**Tests**: bridge_test.go extended with 1.6 message fixtures; sp2_auth_test analog for 16;
reconcile_shell_test unchanged (protocol-agnostic contract is the assertion); harness repo:
evsim-1.6 loopback test (csip-tls-test follow-up, noted not blocking).
**Acceptance**: 1.6J charger connects, transacts, takes limits through the SAME desired-doc →
reconciler path; "2.0.1-native, 1.6J compatibility mode" positioning real; 2.0.1 tests
byte-identical.

### WP-13 — Pairing gate + ClearChargingProfile (B2, B3) — Size M
**Scope**: `pairing_mode` gated|open (D10): unknown-station Boot → Pending (both protos), no
plant, no transactions; retained pending surface (exists) + api approve/deny (POST
/devices/evse/{id}/pairing) → bus.PairingDecision on `lexa/ocpp/pairing` → persisted allowlist
(/var/lib/lexa, 0600, provision-dir discipline per V1RC finding D); configured stations
pre-approved. ClearChargingProfile both protos: RestoreCurrentA sentinel on desired doc
(bus/desired.go — LANE slice), reconciler maps MaxCurrentA ≥ rated → Clear; converge on
Clear-Accepted (release has no measurable target; under-limit one-sided rule makes post-release
state trivially compliant).
**Files**: cmd/ocpp/{pending.go,main.go,config.go,pairing.go (new)}, cmd/api/handlers.go,
internal/bus/{pairing.go,desired.go} (LANE slice), systemd/mosquitto-lexa.acl, apicontract
fixtures.
**Deps**: WP-12 (covers both stacks once); bus-lane slot after WP-9.
**Flags**: pairing_mode default "gated" product / "open" under bench profile.
**Tests**: pending_test.go extensions (gated Boot Pending, approve→Accepted flow, allowlist
persistence across restart); reconcile shell release-path tests; apicontract golden fixtures +
CI drift gate (hub-app contract Version bump per its rules if /devices shape changes).
**Acceptance**: closes roadmap R8 (unknown charger cannot become plant); release-semantics
integration-review gap closed; bench flows unchanged under bench profile.

### WP-14 — V2G type enablers (D1) — Size M
**Scope**: EVSECommand.SetpointW + EVSE desired-doc setpoint_w (signed, D8); bridge W→A
conversion + discharge clamp-to-0 A at bridge.Apply (single greppable seam, rate-limited log);
planner EV-as-storage DP asset behind `ev_storage:false` (departure/SoC/capacity honored — EVGoal
capacity plumb completed); EV counted as DER in export/limit math only when flag on;
breach attribution includes EV axis.
**Files**: internal/orchestrator/{model,planner,optimizer}.go, internal/bus/desired.go +
messages.go (LANE tail), cmd/hub/{desired,intent}.go, cmd/ocpp/{main,reconcile_shell}.go.
**Deps**: WP-9 lane order; WP-13 (desired.go slice ordering). **Flags**: ev_storage default
false — flag off ⇒ planner output byte-identical (pinned by test).
**Tests**: planner_test.go: flag-off identity test (golden plan equality) + flag-on discharge
scenarios (departure honored, reserve analog); bridge clamp tests; nan_reject/Finite updates.
**Acceptance**: charge-only construction dead in types/planner; actuation still provably
charge-only (clamp test); zero default-behavior change.

### WP-15 — lexa-openadr service (E1) — Size L
**Scope**: cmd/openadr + internal/openadr: OAuth2 client-credentials (token refresh on 401,
secret file 0600), 3.1 REST poll loop (programs/events, pagination, event lifecycle by poll
reconciliation, randomizeStart honored), CP-profile translation (PRICE/EXPORT_PRICE/GHG/ALERT →
`lexa/openadr/prices`; IMPORT/EXPORT_CAPACITY_LIMIT → `lexa/openadr/limits`), TELEMETRY_USAGE/
STORAGE_* report POSTs from bus subscriptions, opt-out from compliance alerts, retained
`lexa/openadr/status`; hub adoption: prices → SetPrices/SetDeliveryTariff seam, limits →
GridState arbitration per D9 (most-restrictive caps, CSIP-wins dispatch, price precedence);
uncommissioned idle (vtn_url:""); systemd unit Type=notify WatchdogSec=120; broker user + ACL
stanza; metrics :9108; deploy-hub-pi.sh awareness noted (creds + config + unit — script change
tracked separately).
**Files**: cmd/openadr/* (new), internal/openadr/* (new), internal/bus/openadr.go (+LANE slice),
cmd/hub/{openadr_adopt.go,state.go,tariff.go}, systemd/{lexa-openadr.service,mosquitto-lexa.acl},
configs/openadr.json.
**Deps**: WP-2 (report quality); bus-lane slot last. Hub arbitration piece serializes with
cmd/hub owners (after WP-11 preferably — same GridState assembly site).
**Flags**: whole service opt-in by config presence; pure Go (CGO_ENABLED=0 — verify in CI matrix).
**Tests**: -race unit tests: token flow (httptest VTN), event reconciliation (add/update/delete),
translation goldens, arbitration table (D9 cases: CSIP-tighter, OpenADR-tighter, dispatch
conflict), idle mode; tariff_test.go precedence extension.
**Acceptance**: CP-profile VEN feature-complete against httptest VTN; certification is a later
calendar item gated on a named program (expert #10); no wolfSSL import (grep gate in CI).

### WP-16 — CSIP-AUS checklist + verification-sweep docs (F-b, A7i/iii) — Size S
**Scope**: docs/extension/CSIP_AUS_CHECKLIST.md — every AUS-scoped behavior mapped to
evidence-or-UNVERIFIED (spec absent — F1 caveat, verify against CSIP-AUS v1.1a before cert);
COMM-004 D–G pcap procedure doc (wolfSSL verify-depth ≥4 config assertion + bench capture steps,
runs when bench frees); SunS 0/50000 probe noted closed by WP-1; cert-campaign mechanics note
(config freeze, TRR logs, dry-run before lab).
**Files**: docs/extension/*, csip-tls-test docs pointer. **Deps**: WP-11 (references axes).
**Tests**: n/a (docs). **Acceptance**: every unverifiable AUS item enumerated; A7 bench work
queued with exact commands.

### WP-17 — Integration & verification (final) — Size M
**Scope**: (a) ACL matrix re-derived from ALL Subscribe/Publish call sites (grep protocol per
acl header) — includes the pre-existing lexa-api certstatus gap (architecture §0.3); (b)
envelope/SupportedV audit (every new family has const + arm + version-gated subscriber); (c)
`make test` -race full sweep + `make build-arm64` cross-compile incl. cmd/openadr; (d) apicontract
golden fixtures + drift gate for /status (pin_ok, openadr status) and /devices (pairing) changes;
(e) FLASH_BUDGET review for every new log line (edges only); (f) bench validation queue doc:
ordered scenario list (adv-shell shadow soak, AUS shadow week, 1.6 evsim, PIN mismatch drill,
COMM-004 pcaps, dersite/PUT against gridsim — gridsim needs PUT/LogEvent endpoints: paired
csip-tls-test work item flagged); (g) CLAUDE.md topic-map/invariant updates.
**Deps**: all. **Acceptance**: releasable main; every WP's flags documented in configs/*.json
examples; zero ACL-drift (matrix diff clean); CI green both repos on the pinned proto SHA.

---

## Release order (tree releasable after each)

WP-1 → WP-3 → WP-2 → WP-7 → WP-5 → WP-6 → WP-4 → WP-8 → WP-9 → WP-12 → WP-13 → WP-10 → WP-11 →
WP-14 → WP-15 → WP-16 → WP-17.
(Lanes overlap in wall-clock per the DAG; the order above is the merge order for the serialized
files and "releasable-after" property.)

## Conformance closure map (SunSpec CSIP v1.3, DER Client [C] profile)

| Test | Closed by |
|---|---|
| CORE-009 / CORE-014 | WP-4 (+WP-7 PIN element) |
| CORE-003 / BASIC-001 (PIN) | WP-7 |
| CORE-012 (curve→inverter) | WP-8+9+10 |
| CORE-022/023 (codes) | WP-7 |
| BASIC-004/005/006/008/009/011/012 | WP-10 (with WP-8/9) |
| BASIC-007 (ramp defaults) | WP-8/9 (carriage) + WP-10 (write) |
| BASIC-027 | WP-6 |
| BASIC-028 | WP-4 |
| BASIC-029 (VAr) | WP-5 (+WP-2) |
| ERR-001 | WP-3 |
| COMM-004 D–G (pcap proof) | WP-16 procedure + bench queue |
| Already in hand (no WP) | COMM-002/003, CORE-005/010/011/013/021, BASIC-002/003/010/015–026 |
Not targeted: Aggregator-profile groups (CORE-018/019, UTIL/AGG/MAINT, ERR-002) — GFEMS [C]
strategy per expert review.
