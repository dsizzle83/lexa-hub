# LEXA Hub Standards Build-Out — Architecture

Date 2026-07-14. Basis: architect-brief.md (scope A–F), repo @ d6ac263, seams verified directly in
code (file:line cites below are verified, not digest-relayed). Companion docs: work-packages.md,
risks.md (same directory).

## 0. Verified deviations from the brief

1. **lexa-openadr metrics port is 9108, not 9106.** 9106 is taken by lexa-cloudlink
   (`cmd/cloudlink/config.go:150`), 9107 by lexa-provision (`cmd/provision/config.go:131`).
2. **OCPP dual-stack = two listener ports, not one version-dispatching socket.** ocpp-go v0.19.0
   (go.mod:9): `ocpp16.CentralSystem` and `ocpp2.CSMS` each own their `ws.WsServer` and bind in
   `Start(port, path)` — there is no shared-listener subprotocol dispatch seam. 2.0.1 stays :8887;
   1.6J gets :8886 (`port_16`, default 0 = disabled). The *bridge* is version-dispatching; the
   listener is not. Revisit only if ocpp-go grows a shared-server API.
3. **Live ACL gap found during verification** (evidence for the ACL-drift risk): cmd/api
   subscribes `lexa/northbound/certstatus` (`cmd/api` onCertStatus), but the only
   `topic read lexa/northbound/certstatus` grant in `systemd/mosquitto-lexa.acl` (line 271) is in
   the **lexa-cloudlink** stanza — the lexa-api stanza (lines 188–245) has none. Fix rides WP-17's
   ACL re-derivation.

## 1. Scope → landing map

| Scope | Lands in | New packages/files (indicative) |
|---|---|---|
| A1 Measurement enrichment | internal/bus, cmd/modbus, lexa-proto/derbase | messages.go additive fields; derbase.Measurements ext |
| A2 PUT + DER* reporting | internal/tlsclient, internal/northbound/derreport (new), cmd/hub, internal/bus | request.go PUT; fetcher.Put; hub `dersite` publisher |
| A3 VAr/Wh telemetry | cmd/telemetry | quantity table rows |
| A4 LogEvent poster | cmd/hub (alarm edge detector), internal/northbound/logevent (new) | new bus topic `lexa/hub/logevent` |
| A5 Table 27 codes | internal/northbound/responses, lexa-proto/csipmodel | code map + legacy flag |
| A6 PIN verify | internal/northbound/run + discovery (helper exists: discovery/helpers.go VerifyRegistration) | config `registration_pin` |
| A7 verify sweep | internal/tlsclient (redirects), lexa-proto/sunspec (scanner bases), bench docs | |
| B1 OCPP 1.6J | lexa-proto/ocppserver16 (new pkg), cmd/ocpp | second listener + 1.6 forwarders |
| B2 pairing gate | cmd/ocpp (pending.go exists), cmd/api, internal/bus | `lexa/ocpp/pairing`; allowlist persist |
| B3 ClearChargingProfile | cmd/ocpp, internal/bus/desired.go | RestoreCurrentA sentinel |
| C1 carriage | internal/northbound/{discovery,scheduler,publish}, internal/bus | ActiveControl additive; `lexa/csip/curves` |
| C2 execution | cmd/modbus (new adv shell), internal/reconcile | desired `lexa/desired/adv/{device}` |
| C3 RvrtTms | cmd/hub, cmd/modbus, southbound registry | plumb to derbase.Base.DefaultRvrtTms (exists, derbase.go:44) |
| C4 mode priority | cmd/hub adv-doc author | arbitration table (see D7) |
| C5 shadow discipline | internal/orchestrator/constraint | only for F-a; curve plumbing bypasses optimizer (see §6) |
| D1 V2G enablers | internal/bus, internal/orchestrator (planner), cmd/ocpp | signed SetpointW; `ev_storage` flag |
| E1 lexa-openadr | cmd/openadr (new service), internal/openadr (new), internal/bus | VEN core; 3 new topics |
| F CSIP-AUS | internal/orchestrator (+constraint), internal/bus, docs | GenLimW/LoadLimW axes; AUS checklist |

## 2. Bus: new/changed topics, message types, ACL impact

### 2.1 Envelope policy (applies to everything below)

All changes to existing families are **additive optional fields at the existing version constant**
(stay V=1). Per AD-006 (`internal/bus/envelope.go:36`), a version bump is only for changes old
subscribers cannot tolerate — and a bump is itself breaking (subscriber rejects v>supported,
`CheckVersion`, envelope.go:130). Additive `*T`/omitempty fields are ignored by old decoders and
`nil`-absent to new decoders reading old publishers. Every new `*float64` field joins the type's
`Finite()` check (GAP-09). New families are born at 1 with their own constants + `SupportedV` arms.

### 2.2 Changed message types (existing topics — no ACL change)

**bus.Measurement** (`lexa/measurements/{device}`, V=1, additive) — A1:
```
var_w        *float64  // reactive power (VAr), + = injecting/capacitive (device convention)
va           *float64  // apparent power (VA)
pf           *float64  // power factor [-1,1]
op_state     *uint16   // 701 St (or 103 St mapped); operational state enum
conn_state   *uint16   // 701 ConnSt bitfield
alarm_bits   *uint32   // 701 Alrm bitfield (raw; mapping to CSIP Table 14 happens hub-side)
wh_imp_total *float64  // lifetime import energy (Wh) — meter TotWhImp / 701 energy accumulators
wh_exp_total *float64  // lifetime export energy (Wh)
```
Sources: derbase.Measurements already carries Var/VA/PF (derbase.go:133-151); St/ConnSt/Alrm are
parsed by `sunspec.Parse701` but dropped by `ReadMeasurementsM701` (derbase.go:154-161) →
derbase.Measurements gains additive fields (lexa-proto change-set, §4). Meter TotWhImp/Exp offsets
are declared in lexa-proto/sunspec/models.go but parsed nowhere → parse in lexa-hub
internal/southbound/meter. Interval energy is derived hub/consumer-side from the monotonic totals
(never fabricate: absent stays nil, G27).

**bus.ActiveControl** (`lexa/csip/control`, V=1, additive) — C1/F-a:
```
energize        *bool      // opModEnergize (distinct from connect)
gen_lim_w       *float64   // opModGenLimW  (gross generation cap — CSIP-AUS)
load_lim_w      *float64   // opModLoadLimW (gross load cap — CSIP-AUS)
target_w        *float64   // opModTargetW (parse-through; enforcement TBD)
fixed_pf_inject *FixedPF   // {pf float64 [0,1], over_excited bool}  — opModFixedPFInjectW
fixed_pf_absorb *FixedPF   // opModFixedPFAbsorbW
fixed_var_pct   *float64   // opModFixedVar, signed % of setMaxVar
set_grad_w      *float64   // ramp default (default-control only)
set_soft_grad_w *float64
rvrt_tms_s      *int64     // computed device reversion window (see C3)
curve_set_id    string     // content hash of the matching lexa/csip/curves doc ("" = no curves)
```
Curve *content* does NOT ride ActiveControl (see D6/§2.3). The TASK-042 staleness/rewalk machinery
is untouched — same topic, same retained semantics, same size class.

**bus.DesiredState** (`lexa/desired/{class}/{device}`, V=1, additive) — B3/D1/C3:
```
setpoint_w (evse docs)  *float64  // signed EV setpoint: + discharge to site, − charge (D1; see D8)
rvrt_tms_s              *int64    // device reversion window for 704 writes (C3)
```
`RestoreCurrentA = 1e6` const added beside RestoreCeilingW (desired.go:71): an EVSE release is an
explicit large MaxCurrentA (reconciler maps ≥ rated → ClearChargingProfile, B3) — restore is a
value on the wire, never absence (AD-013).

**bus.EVSEState** — no change (already carries SOC/EnergyWh, messages.go:143-157).

**bus.ReconcileReport** (`lexa/reconcile/{class}/{device}/report`, V=1, additive) — C2:
```
axis        string  // "" (legacy scalar) | "volt_var"|"volt_watt"|"watt_var"|"freq_droop"|
                    // "trip_lv"|"trip_hv"|"trip_lf"|"trip_hf"|"fixed_pf"|"fixed_var"|"energize"
adopt_state string  // ""|"pending"|"adopted"|"diverged"|"unsupported"|"failed"
curve_hash  string  // content hash of the adopted curve (readback-verified)
```
Class "adv" joins the topic family (`lexa/reconcile/adv/{device}/report`). The hub's existing
`SubReconcileReport` wildcard (`lexa/reconcile/+/+/report`) already matches it.

### 2.3 New topics + ACL impact table

| Topic | Type (new family, V=1) | Retain/QoS | Writer → Readers | ACL rows to add (mosquitto-lexa.acl) |
|---|---|---|---|---|
| `lexa/hub/dersite` | bus.DERSiteReport | retained, QoS1 | hub → northbound (api optional later) | hub: write; northbound: read |
| `lexa/hub/logevent` | bus.LogEventMsg | NOT retained (edge), QoS1 | hub → northbound | hub: write; northbound: read |
| `lexa/csip/curves` | bus.CurveSet | retained, QoS1 | northbound → hub | northbound: write; hub: read |
| `lexa/desired/adv/{device}` | bus.DesiredAdvanced | retained, QoS1 | hub → modbus | hub: write `lexa/desired/adv/+`; modbus: read same |
| `lexa/reconcile/adv/{device}/report` | bus.ReconcileReport (ext) | retained, QoS1 | modbus → hub | modbus: write `lexa/reconcile/adv/+/report` (hub read exists via wildcard) |
| `lexa/ocpp/pairing` | bus.PairingDecision | NOT retained, QoS1 | api → ocpp | api: write; ocpp: read |
| `lexa/openadr/prices` | bus.OpenADRPrices | retained, QoS1 | openadr → hub | new user stanza (below) |
| `lexa/openadr/limits` | bus.OpenADRLimits | retained, QoS1 | openadr → hub | " |
| `lexa/openadr/status` | bus.OpenADRStatus | retained, QoS1 | openadr → api | " |

New broker user **lexa-openadr** (deploy-hub-pi.sh must provision `/etc/lexa/mqtt/openadr.pass` +
patch config — note for the deploy script, not written here):
```
user lexa-openadr
topic write lexa/openadr/prices
topic write lexa/openadr/limits
topic write lexa/openadr/status
topic read  lexa/measurements/+          # TELEMETRY_USAGE reports (E1)
topic read  lexa/battery/+/metrics       # STORAGE_* reports
topic read  lexa/csip/compliance/alert   # opt-out evidence (read-only, like cloudlink)
topic read  lexa/hub/plan                # forecast/dispatch reports (optional)
```
Hub stanza additions: read `lexa/openadr/prices`, `lexa/openadr/limits`. API stanza: read
`lexa/openadr/status`, write `lexa/ocpp/pairing`; **plus the missing
`read lexa/northbound/certstatus` fix (§0.3)**. Northbound stanza: read `lexa/hub/dersite`,
`lexa/hub/logevent`; write `lexa/csip/curves`. Re-derive the whole matrix from Subscribe/Publish
call sites in WP-17 (the ACL is an authorization boundary — CLAUDE.md).

Edge-vs-state discipline: `lexa/hub/logevent` and `lexa/ocpp/pairing` are edges (never retained —
a retained edge replays as a false edge after restart, topics.go:365). `dersite`/`curves`/adv
desired/adv report/openadr docs are state (retained, latest wins, re-seeds after crash — AD-011).

## 3. New/changed config keys (all additive; unknown keys warn-not-fail)

| File | Key | Default | Notes |
|---|---|---|---|
| northbound.json | `registration_pin` | 0 | 0 = check disabled + WARN (WS-8 pattern). Non-zero: verify each walk (D4) |
| northbound.json | `der_report` | true | PUT DER* per D2; auto-skips resources the server doesn't offer (404/405 tolerated once, logged) |
| northbound.json | `legacy_cannotcomply_code` | false | false = Table 27 codes (D5); true = 0xF0 (bench/gridsim compat) |
| northbound.json | `redirect_max` | 3 | ERR-001 following bound; 0 disables (A7ii) |
| hub.json | `advanced_der` | "off" | "off"\|"on": author+publish `lexa/desired/adv` docs (C1/C4). Flip after WP-10 bench soak |
| hub.json | `enforce_aus_limits` | false | GenLimW/LoadLimW cascade enforcement flip (F-a; shadow constraints always run when constraint_shadow) |
| hub.json | `ev_storage` | false | planner EV discharge term (D1); off = byte-identical plans |
| hub.json | `logevent_min_interval_s` | 10 | LogEvent edge rate floor (flash budget) |
| modbus.json | `reconciler.adv` | "off" | "off"\|"shadow"\|"active" — adv shell mode, staged like battery (TASK-027/028) |
| ocpp.json | `port_16` | 0 | 0 = 1.6J listener disabled (fail-closed). Same SP2-analog gate when enabled |
| ocpp.json | `pairing_mode` | "gated" | "gated"\|"open". bench profile ⇒ "open" (preserves bench flows). configured stations[] implicitly allowlisted |
| ocpp.json | `allowlist_path` | /var/lib/lexa/ocpp-allowlist.json | persisted approvals (B2) |
| telemetry.json | `post_var` | true | VAr MMR uom 63 (A3 — CSIP Table 2 mandatory) |
| telemetry.json | `post_wh` | true | Wh MMRs uom 72 imp/exp; NaN-skip preserves G27 |
| openadr.json | (new file) | — | `vtn_url` ("" = uncommissioned idle, telemetry pattern), `client_id`, `client_secret_file` (0600, like mqtt pass), `program_ids[]`, `poll_interval_s` (60), `ven_name`, `mqtt_user/mqtt_pass_file`, `metrics_addr` (127.0.0.1:9108), `log_level` |

Product defaults fail closed: 1.6 off, pairing gated, adv off/off, AUS enforcement off (status quo
preserved), EV storage off. Reporting defaults (der_report, post_var/wh) are ON because reporting
truthful data IS the compliance duty (G28–G31) and carries no control risk.

## 4. lexa-proto vs lexa-hub split — ONE proto change-set (one pin bump)

Everything below lands in a single lexa-proto commit, paired-PR'd with csip-tls-test and this repo
(proto.pin + vendor/ regenerated same session; CI check-proto-pin gate):

1. **derbase**: additive fields on `Measurements` (OpSt, InvSt, ConnSt, Alrm uint32, WhImpTotal,
   WhExpTotal — NaN/0 defaults); `ReadMeasurementsM701` populates them from Parse701 (which
   already parses all of it). No signature changes; every existing call site compiles.
2. **csipmodel**: `LogEvent` type (functionSet, logEventCode, logEventID, logEventPEN,
   createdDateTime, profileID — BASIC-027 shape); named Table 27 constants
   (ResponseEventReceived=1 … ResponsePartialOptOut=8, ResponseNoParticipation=10,
   ResponseAbortedServer=13, ResponseAbortedProgram=14, ResponseRejectedParam=252,
   ResponseRejectedInvalid=253, ResponseRejectedExpired=254) alongside the retained
   `ResponseCannotComply=0xF0`. Verify DERCapabilityFull/DERSettingsFull/DERStatusFull/
   DERAvailability marshal with xmlns roots (types exist, der.go:308-411) — add round-trip tests.
3. **sunspec**: `Scan` probes bases 0, 40000, 50000 (spec-permitted alternates; today 40000-only,
   scanner.go) — order 40000 first (fast path).
4. **ocppserver16** (new package, sibling of ocppserver): OCPP 1.6J CentralSystem wrapping
   ocpp-go's `ocpp1.6` package (Core + SmartCharging profiles), same Config shape (TLS cert/key +
   BasicAuth, constant-time compare), same handler-registration seam. ocpp-go stays at v0.19.0 —
   the 1.6 package already exists upstream; `go mod vendor` in consumers pulls it in.

Everything else — bus types, reporting, reconcilers, optimizer, VEN — is lexa-hub only. The
lexa-openadr service is pure lexa-hub (no proto involvement, no wolfSSL, CGO_ENABLED=0).

## 5. Data-flow diagrams

### (a) DER* reporting (A2)

```
cmd/modbus poll ──lexa/measurements/{dev} (V,VAr,W,alarms,Wh)──┐
cmd/modbus     ──lexa/battery/{dev}/metrics (SoC,cap,rates)────┤
cmd/ocpp       ──lexa/evse/{station}/state─────────────────────┤
                                                               ▼
              cmd/hub aggregator (GFEMS math, D2; change-detect + 60s min cadence)
                       │  retained lexa/hub/dersite (bus.DERSiteReport:
                       │   ratings Σ, settings, live status, SoC agg, alarm agg,
                       │   modesSupported truth-mask from enforcement flags)
                       ▼
        lexa-northbound internal/northbound/derreport
          ├─ startup + on dersite content change: PUT DERCapability, PUT DERSettings
          ├─ every DERList pollRate (run/pollrate seam): PUT DERStatus, PUT DERAvailability
          └─ via tlsclient.WolfSSLFetcher.Put (new verb; discovery session, existing mutex)
                       ▼
              utility server EndDevice → DERListLink → /der/1/dercap|dersettings|derstatus|deravail
```
Cadence sources: capability/settings on change (G29); status at DERList pollRate (G30, Table 13).
The walker already fetches the DER hrefs (walker.go:214-248) — derreport reuses those hrefs, never
hardcodes paths.

### (b) Curve-control path (C1/C2)

```
DERControl/DefaultDERControl (ExtendedDERControlBase + DERCurveLinks)
   │ walker: STOP dropping curve/extended fields (extendedListToSimple retired;
   │ ProgramState carries extended base + resolved Curves — walker.go:306-318 already fetches)
   ▼
scheduler.Evaluate → ActiveControl{Base + Extended + CurveRefs}   (same fail-closed LKG hold)
   │
   ├─ publish.ToActiveControl → retained lexa/csip/control  (scalars incl. PF/var/energize/
   │      gen/load lims + curve_set_id hash — small doc, TASK-042 semantics unchanged)
   └─ publish.Curves        → retained lexa/csip/curves     (bus.CurveSet: per curve-type the
          resolved DERCurve — ≤10 pts, multipliers, curveType, MRID, program, set hash)
   ▼
cmd/hub (advanced_der:"on"): consume both; arbitrate modes (D7); map site→device by
   capability (DERStatusSummary.ModesSupported + device class); compute rvrt_tms;
   author ONE retained lexa/desired/adv/{device} doc per inverter/battery (D6)
   ▼
cmd/modbus adv shell (reconciler.adv: shadow|active):
   curve axes:  derbase.WriteVoltVar/VoltWatt/WattVar/FreqDroop/Trip* (adopt handshake inside:
                stage → AdptCrvReq → poll AdptCrvRslt=COMPLETED → Ena=1) THEN re-read the live
                curve and compare — never trust the write (adopt_state=adopted only on readback
                match; ReconcileReport.axis/adopt_state/curve_hash retained)
   fixed PF/var: derbase.SetFixedPF/SetConstantVar; convergence from MEASURED PF/Var in the
                poll-loop observe() (device.Measurements already carries Var/PF locally)
   energize:    derbase.SetEnterServiceEnabled (703 ES) + measured cessation (W→0 / ConnSt)
   legacy 12x:  device without 704/705+ → per-series enable-rewrite handling; else 7xx immediate
   interlock:   Tier-0 isTripped suppresses energize/connect-restoring writes (InterlockHold) —
                interlock stays senior (cmd/modbus/interlock.go), unchanged
   ▼
non-convergence → ReconcileReport(adv) → hub breachEpisodes → ComplianceAlert → responses.Tracker
```
Single-writer preserved: hub is the only author of desired docs; the adv shell is the only writer
of 703/705–712 registers; scalar shells keep 123/704 W-path ownership (no register overlap —
WMaxLimPct/WSet vs curve/PF blocks; the one shared surface, 704 PF sync groups vs WSet, is owned
by the adv shell exclusively — scalar shells never touch PF fields since write704 is whole-block
RMW, adv and scalar writes for one device serialize on the per-device transport mutex).

### (c) OCPP dual-stack (B1)

```
1.6J charger ──ws/wss :8886 (subprotocol ocpp1.6)──► ocppserver16.CentralSystem ─┐
2.0.1 charger ──ws/wss :8887 (subprotocol ocpp2.0.1)──► ocppserver.Server ────────┤
                                                                                  ▼
                       cmd/ocpp mqttBridge (ONE state map; station tagged with proto version)
        1.6: BootNotification/Start-StopTransaction/MeterValues/StatusNotification
             mapped onto the SAME stationState/EVSEState the 2.0.1 forwarders feed
        pairing gate (B2, both stacks): unknown station + pairing_mode=gated →
             Boot response status=Pending, no plant, surfaced on retained lexa/ocpp/pending;
             approve via API → lexa/ocpp/pairing → allowlist persisted → next Boot Accepted
                                                                                  ▼
        reconcile shell (unchanged contract): desired doc → bridge.Apply(station, evse, limitA)
             Apply dispatches per station proto: 2.0.1 SetChargingProfile TxDefault |
             1.6 SetChargingProfile TxDefaultProfile (A units); release → ClearChargingProfile
             (B3, both protos); Accepted ≠ convergence — metered-current rule unchanged
```

### (d) lexa-openadr (E1)

```
VTN (OpenADR 3.1 REST/JSON, OAuth2 client-credentials, stdlib TLS — pure Go)
   ▲ poll loop (northbound run-loop shape: watchdog kick at loop top, single-flight,
   │ backoff, uncommissioned idle when vtn_url=="")
   │ GET /programs, /events?program&active; POST /reports; token refresh on 401
   ▼
cmd/openadr translator
   ├─ PRICE/EXPORT_PRICE/GHG/ALERT_* → retained lexa/openadr/prices (absolute RFC3339 intervals)
   ├─ IMPORT/EXPORT_CAPACITY_LIMIT   → retained lexa/openadr/limits {imp_lim_w, exp_lim_w,
   │        event_id, valid_until}  (event lifecycle: UPDATE/DELETE by poll reconciliation)
   ├─ reports ← lexa/measurements/+, lexa/battery/+/metrics (Wh/VAr need A1)
   ├─ opt-out ← lexa/csip/compliance/alert (breach episodes)
   └─ health  → retained lexa/openadr/status (token ok, last poll, program count) → lexa-api
   ▼
cmd/hub adoption (staleness-checked like TASK-042 pattern):
   prices  → SetPrices/SetDeliveryTariff seam (tariff.go compile path; precedence D9)
   limits  → GridState arbitration vs CSIP (D9: caps most-restrictive; CSIP wins dispatch)
```

## 6. Shadow-gate applicability (C5)

- **Curve/PF/energize provisioning (C1–C4) does NOT touch the optimizer cascade** — it flows
  northbound→hub-author→adv-reconciler, bypassing `Optimize()` entirely. Its staged rollout knob
  is the reconciler mode (off→shadow→active), the same discipline TASK-027/028 used, not the
  constraint-stack shadow harness.
- **GenLimW/LoadLimW enforcement (F-a) DOES**: new cascade rules (authoritative path, behind
  `enforce_aus_limits`) land together with mirrored shadow constraints
  (constraint/genlimit.go/importlimit.go pattern, TASK-061), each with convergence backstops and
  breach/CannotComply integration. ≥1-week bench shadow before default-on.
- **EV discharge term (D1)** is flag-off-by-default (byte-identical plans when off); when enabled
  it changes plan authorship → rides `constraint_shadow` divergence watch during bench enablement.

## 7. Design decisions

### D1 — Engine/bus integration for new inputs
OpenADR prices/limits and dersite aggregation enter the Engine only via existing async seams
(`SetPrices`/`SetDeliveryTariff`/`SetDERConstraints` → cmdCh; TASK-067). No new mutation paths; no
new locks. Hub-side publishers (dersite, logevent, adv docs) use the async
PublishJSON(Retained)Async + harvest pattern (TASK-046) — the tick never blocks on PUBACK.

### D2 — GFEMS aggregate DERCapability semantics
One EndDevice, one DERList entry, aggregate at the PCC (CSIP P28, Scenario-1 GFEMS).
- **DERType**: 83 (PV+storage) when the site is inverters+batteries (the product's common case);
  1 (virtual/mixed) when EVSEs are enrolled as DER (`ev_storage` on) or the mix is heterogeneous.
  Config override `der_type` for utility-handbook fiat.
- **Ratings (rtg*)**: physical sums — rtgMaxW = Σ inverter nameplate MaxW (+ Σ battery
  MaxDischargeW when the batteries can export); rtgMaxChargeRateW/rtgMaxDischargeRateW = Σ battery
  rates (EVSE excluded until ev_storage); rtgMaxWh = Σ battery CapacityWh. VA/Var ratings emitted
  only where device data exists — **never fabricate** (G27): a site with no VAr rating data omits
  rtgMaxVA/rtgMaxVar rather than guessing.
- **Settings (set*)**: operational caps ≤ ratings BY CONSTRUCTION (each setX = min(rtgX, site
  policy)); CORE-014's operator check passes structurally, pinned by unit test.
- **modesSupported**: a truth mask derived from what the hub ACTUALLY enforces end-to-end —
  starts at Connect/Energize/opModMaxLimW/opModFixedW (+ExpLimW/ImpLimW for AUS); PF/var/curve
  bits flip ON only when `advanced_der`+`reconciler.adv=active` are live for a device that has the
  model. Never advertise what a CannotComply would immediately contradict.
- **DERStatus**: stateOfChargeStatus = capacity-weighted battery SoC; genConnectStatus from
  aggregate connect state; alarmStatus = OR of per-device alarm categories; readingTime stamped.
- Rationale: sums are what the PCC physically presents; the truth-mask keeps CORE-014's "bits
  truthful" check and our own CannotComply story consistent; omission-over-fabrication follows G27
  and the repo's fail-closed posture.

### D3 — PUT verb shape
`buildPutRequest` mirrors buildPostRequest (request.go:35-45, CRLF-injection guard reused);
`WolfSSLFetcher.Put/PutContext` mirrors Post (expect 200/201/204; no Location dependency). Same
three-session model — DER* PUTs ride the discovery fetcher under its existing mutex (rotation via
Reload keeps working for free). Redirect-following (ERR-001): on 301/302 + Location, re-issue
against the SAME configured host (path-only rewrite; absolute-URL Locations must match host),
bounded by `redirect_max`, GET/PUT/POST alike, never downgrade scheme. Rationale: hostile-server
caps stay intact; CSIP uses 302 for rediscovery, not cross-host bouncing.

### D4 — PIN-mismatch posture (A6)
On `registration_pin` configured and Registration.pIN mismatch (or Registration fetch 404):
1. **Control plane fails CLOSED, not open**: currently-adopted control is HELD (scheduler LKG
   discipline, scheduler.go:144-160) but **no NEW control from this server is adopted**;
2. **Egress stops**: DER* PUTs, MUP posts, LogEvents, Responses to that server are suspended
   (don't feed data to a server we can't authenticate our registration against — 2030.5 §6.9.2(c)
   "stop using server");
3. **Loud**: slog Error edge + `lexa_nb_pin_mismatch` gauge=1 + surfaced on `lexa/openadr`— no,
   on `lexa/northbound/certstatus`-adjacent path: add `pin_ok` field to the retained certstatus
   doc (additive) so lexa-api /status shows it;
4. Walk keeps running (re-checks each cycle; self-heals when the server fixes registration).
Rationale: dropping enforcement on mismatch would fail OPEN (release caps because of a
provisioning error); enforcing forever a server we distrust is also wrong — hold-and-freeze is the
exact enforce-but-verify compromise TASK-042 established, and the loud surface makes it a
commissioning gate, not a silent brick. `registration_pin=0` (shipped default) = disabled + WARN
(WS-8 pattern) so existing deployments are untouched.

### D5 — CannotComply → Table 27 mapping (A5)
Two failure classes, mapped where 2030.5 vocabulary actually fits:
- **Rejected at receipt** (never adopted): plausibility-gate reject → **253** (invalid/
  out-of-range); mode the site cannot execute (modesSupported mismatch/no capable device) →
  **252** (parameter not applicable). Posted once per event mRID at rejection.
- **Adopted but breached during execution** (today's episode flow): at episode ONSET post
  **8** (partial completion) once per episode — the earliest standard signal that execution is
  degraded; at event END post **3** if no episode overlapped, **8** if partially breached, **10**
  (no participation) if breached throughout. Supersede/abort stay 7/13/14 (13/14 now emitted by
  the tracker where cross-program/server supersession is detected).
- LogEvent (A4) carries the real-time detail (alarm+RTN pairs) — Response codes are lifecycle
  states, not telemetry.
- `legacy_cannotcomply_code:true` restores 0xF0 for gridsim until the harness updates (paired
  session, MTR-4 discipline). Default false = standard codes.
Episode dedupe (tracker.go:120-176, episodeID) is unchanged — only the wire code and the
end-of-event reconciliation are new.

### D6 — Curve desired-doc schema (C1/C2)
**Separate retained doc per device**: `lexa/desired/adv/{device}` (bus.DesiredAdvanced), NOT new
fields on DesiredState. Shape:
```
{ v, device_class, device_id,
  reactive_mode: null | {kind:"fixed_pf"|"fixed_var"|"volt_var"|"watt_var", fixed_pf:{pf,over_excited},
                          fixed_var_pct, curve:{curve_type,x_mult,y_mult,points[≤10],hash}},
  volt_watt:  null | {curve...},        // concurrent overlay (1547 §4.7)
  freq_watt:  null | {curve...} , freq_droop: null | {dbuf,df,dp,olt,tresp},
  trips: null | {lv:{curves[3],hash}, hv:..., lf:..., hf:...},
  energize: *bool, set_grad_w/set_soft_grad_w: *float64, rvrt_tms_s: *int64,
  source, mrid, issued_at, seq }
```
Rationale: (1) the three scalar-shell contracts (battery/solar/evse) and their staleness/heartbeat
machinery (desired.go:19-57) stay byte-identical — zero risk to the workhorse path; (2) mutual
exclusivity is STRUCTURAL — `reactive_mode` is one field, so a reconciler can never see torn
multi-axis state, and the hub (single author) owns arbitration (D7); (3) curves are provisioning
state that changes rarely — its own doc gets its own slower reconcile cadence and its own
heartbeat, without inflating every battery-setpoint republish; (4) one doc per DEVICE (not per
axis) keeps the doc count bounded and adoption atomic per device. **Adoption state does NOT ride
the desired doc** (a desired doc is publisher-owned intent; readback state on it would create a
second writer) — it rides the retained per-device ReconcileReport extension (axis/adopt_state/
curve_hash, §2.2), which the hub re-seeds from after restart, exactly like NonConverged state.
Content hashes let both sides skip no-op re-adoptions after reconnect/restart.

### D7 — Mode-priority arbitration (C4)
Hub-side, at adv-doc authoring: (1) reactive modes are mutually exclusive (1547 §5.3) — when the
resolved control carries >1 of {fixedPF, fixedVar, voltVar, wattVar}, pick by: event-sourced over
default-sourced, then dynamic over static (voltVar > wattVar > fixedVar > fixedPF), and emit the
**ignored-mode alarm** (the backlogged ignored-curve alarm generalized: slog WARN edge +
`lexa_hub_ignored_modes_total` + Decision line) for every dropped mode. (2) volt-watt and
freq-droop are concurrent overlays; the lesser-power rule is DEVICE physics (704+ devices
implement 1547 §4.7) — the hub must not fight it, so active-power convergence checks for devices
with volt-watt/droop adopted are ONE-SIDED (under-target tolerated), reusing the solar one-sided
rule. (3) Trip/ride-through sets always pass through (highest 1547 priority, no arbitration). (4)
Anything the target device lacks a model for → adopt_state=unsupported → ignored-mode alarm +
(D5) Response 252 when responseRequired demands it.

### D8 — EVSE signed-setpoint representation (D1)
`SetpointW *float64` on EVSECommand (orchestrator) and the EVSE desired doc — sign matches the
battery convention (+ = discharge to site, − = charge; model.go:259-261): one site-wide DER sign
convention, no per-voltage A↔W ambiguity, planner symmetry with the battery DP asset. Semantics:
nil ⇒ ceiling mode (today's MaxCurrentA path, unchanged); non-nil ⇒ setpoint mode. The OCPP
bridge converts W→A at station voltage and **clamps discharge to 0 A (suspend) with a rate-limited
log** until a V2X path exists — the type system stops being charge-only now; the actuation stays
charge-only by an explicit, greppable clamp at exactly one seam (bridge.Apply). Planner: EV joins
the DP as a storage asset honoring EVGoal (departure/target SoC/capacity — capacity plumb
completed) behind `ev_storage:false`.

### D9 — CSIP vs OpenADR precedence (E1)
- **Capacity limits (imp/exp caps)**: combine MOST-RESTRICTIVE (min per axis). Both sources are
  obligations; honoring the tighter one satisfies both. Implemented at GridState assembly in
  cmd/hub (state.go), with per-source attribution kept for breach MRID/event-id reporting.
- **Dispatch/setpoint conflicts** (CSIP FixedW/TargetW vs OpenADR CHARGE_STATE/DISPATCH payloads):
  **CSIP wins outright** — DERControl is an interconnection-compliance instrument; OpenADR CP is a
  market program. The losing source gets its program's opt-out/report so the VTN learns.
- **Prices**: precedence CSIP tariff (§10.5 walk) > OpenADR CP prices > app/cloud tariff intent —
  utility-of-record first; each layer only fills seams the higher one left empty (SetPrices vs
  SetFallbackTOU split already models this, tariff.go:3-16).
- Breach while under a merged cap attributes CannotComply to the CSIP MRID only when the CSIP cap
  is the binding one; an OpenADR-only bind produces an OpenADR opt-out/report, never a 2030.5
  Response. Documented in code at the arbitration site.

### D10 — OCPP pairing gate posture (B2)
`pairing_mode:"gated"` (product default): unknown station's BootNotification → status **Pending**
(the protocol-sanctioned holding state in both 1.6 and 2.0.1), no stationState promotion to plant,
no transactions accepted (Authorize → Invalid where used), retained `lexa/ocpp/pending` surfaces
it (exists today), approval flows api→`lexa/ocpp/pairing`→persisted allowlist→next Boot Accepted.
Configured `stations[]` are pre-approved. Bench profile ⇒ "open" so every existing bench/Mayhem
flow is unchanged. Rationale: closes R8 ("any dialing charger becomes plant") with OCPP-native
signaling, fail-closed by default, zero bench churn.

## 8. Metrics & logging additions (flash-budget aware)

New gauges/counters (edge-triggered or scrape-computed, no per-tick Info lines):
`lexa_nb_derreport_puts_total{ok}`, `lexa_nb_pin_mismatch`, `lexa_nb_redirects_total`,
`lexa_nb_logevents_posted_total`, `lexa_hub_ignored_modes_total`, `lexa_mb_adv_adopts_total`,
`lexa_mb_adv_unsupported`, `lexa_ocpp_pending_stations` (exists), `lexa_ocpp_v16_stations`,
`lexa_openadr_poll_errors_total`, `lexa_openadr_token_refresh_total`, `lexa_openadr_events_active`.
lexa-openadr: Type=notify, WatchdogSec=120, kick at poll-loop body top (northbound pattern);
crash-only, retained docs re-seed.
