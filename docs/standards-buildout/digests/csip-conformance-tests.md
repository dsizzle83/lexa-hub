# Digest: SunSpec CSIP Conformance Test Procedures v1.3 (+ Results Reporting Spec 1.1) — CLIENT/AGGREGATOR view

## Document identity

- **SunSpec CSIP Conformance Test Procedures, Version 1.3, Approved, 2023-10-24** (234 pp). Conformance tests for the Common Smart Inverter Profile (CSIP) over IEEE 2030.5-2018. Source txt: `scratchpad/standards/txt/SunSpecCSIPConformanceTestProceduresV1.3-1.txt`.
- **SunSpec IEEE 2030.5/CSIP Conformance Test Procedures Results Reporting Specification, Version 1.1, Final, 2019-11-10** (18 pp). Defines the Test Results Report (TRR) certification labs must submit. Source txt: `scratchpad/standards/txt/SunSpec-CSIP-Conformance-Test-Procedures-Results-Reporting-Specification-1.1.txt`.
- Roles: **[C]** Device Client (single DER inverter), **[A]** Aggregator Client (acts on DER controls for downstream inverters), **[S]** Server (DER head-end). **[TS]/[TC]** = test-harness server/client (do things a production peer wouldn't, to provoke the DUT). **[T]** = test instrumentation, **[U]** = the unit under test. LEXA hub = **[A] DER Aggregator Client** DUT; the utility head-end is simulated by a Test Server.
- §4 "Profile Test Conformance" is the normative required-test matrix per profile (Server / DER Client / DER Aggregator Client). "M(A)" below = X in the DER Aggregator Client column (mandatory for our profile); "M(C,A)" = required for both client profiles; "—" = no X in any column of the v1.3 matrix (kept in the doc but not required; mostly server-only or optional).
- §3.2.2 blanket requirements riding on EVERY test: HTTPS/TLS 1.2 only, `application/sep+xml` (or sep-exi), schema-valid payloads, WADL conformance, list paging (`s`/`a`/`l` query params) semantics, client MUST send Accept header (GEN.056), clients support XML or EXI (GEN.051), UTF-8 XML 1.0, HTTP response-code discipline incl. "treat unrecognized code as x00 of its class" (GEN.049), TLS per RFC 5246 with server Device Certificate (SEC.009/SEC.014).
- Standard test config: server at HTTPS `/dcap`, Registration PIN **111115**. On power-up/restart the Client SHALL GET DeviceCapability first.

## Test catalog by group

### 1. Security / TLS / certificate handling (COMM)

| ID | Purpose | DUT-observable pass behavior | Req |
|---|---|---|---|
| COMM-002 | Out-of-band discovery: connect using configured IP/port/dcap path | Client GETs DeviceCapability at the configured URI over HTTPS, then GETs at least one resource linked from it | M(C,A) |
| COMM-003 | Basic security: TLS 1.2 + mandatory cipher | Client establishes TLS 1.2 session using **TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8** (verified by packet capture per RFC 5246 §7.4) and GETs DeviceCapability over it | M(C,A) |
| COMM-004 (A–G) | Advanced security: accept valid chains, reject invalid certs. 7 standalone sub-tests | Client completes TLS+GET with server cert chains of length 2 (SERCA→Device, 004A), 3 (SERCA→MICA→Device, 004B), 4 (SERCA→MCA→MICA→Device, 004C); Client **refuses** the connection (TLS Alert; TCP disconnect or HTTP 403 acceptable) for: invalid MICA Extended-Key-Critical (004D), invalid MICA Name Non-Critical (004E), invalid MICA Policy-Mapping Non-Critical (004F), self-signed device cert (004G) | M(C,A) |
| COMM-001 | xmDNS/DNS-SD discovery of local server (subtype queries e.g. `derp.sub.smartenergy.tcp.site`, QU bit on/off, main service query) | Client emits conformant xmDNS queries, parses PTR/SRV/A/AAAA response, GETs the discovered URI | **Optional** for all device types; — in profile matrix |

### 2. Core function set (CORE) — REST, polling, time, EndDevice, FSA, DER program, subscription, randomization, responses

| ID | Purpose | DUT-observable pass behavior | Req |
|---|---|---|---|
| CORE-003 | Polling interaction: observe per-resource `pollRate` | Client walks dcap→EndDeviceList→its EndDevice (SFDI/LFDI match)→Registration (PIN=111115 verified)→FSAList→DERProgramList; selects highest-priority DERProgram by list-ordering rules; schedules any active event; **polls each resource at its advertised pollRate** | M(C,A) |
| CORE-005 | Basic time: Time resource discovery + sync | Client finds TimeLink in dcap, GETs Time, verifies quality=7 (intentionally uncoordinated), synchronizes its clock to it | M(C,A) |
| CORE-009 | Advanced EndDevice: DER info upkeep | Client finds its EndDevice, validates PIN, finds DERListLink, **PUTs updated DERCapability/DERSettings/DERStatus/DERAvailability**; re-GETs to confirm; (if not pre-registered, Client POSTs its own EndDevice instance) | M(C,A) |
| CORE-010 | FSA: 7-level group hierarchy | Client follows EndDevice→FSAListLink→FSAList→FSA→DERProgramListLink→DERProgramList and GETs **all seven DERPrograms** (all 200 OK) | M(C,A) |
| CORE-011 | Advanced FSA: fifteen FSAs | Client GETs all 15 FSA instances, sorts by primary/secondary key (mRID), picks highest priority, follows its DERProgramListLink (falls back to next FSA if absent), applies DERProgram priority rules (primacy, then mRID), schedules found event | M(C,A) |
| CORE-012 | Basic DERProgram/DERControl processing | Client selects highest-priority DERProgram/DERControl by list-ordering; GETs subordinate resources incl. DERCurveList and all **10 curve points**; passes curve to inverter system; **applies DefaultDERControl until event start**; polls DERControl status; completes on status and/or start+duration | M(C,A) |
| CORE-013 | Advanced: 10 DERPrograms × 10 DERControls (primacy N, staggered starts, opModFixedPFInjectW) | Client schedules and executes each in priority order, DefaultDERControl between events, completes every event | M(C,A) |
| CORE-014 | Basic DER settings (power-generating) | Client GETs its DERList and **PUTs DERCapability + DERSettings** with consistency invariants checkable by operator: settings ≤ ratings (setMaxW≤rtgMaxW, setMaxVA≤rtgMaxVA, setMaxVAr≤rtgMaxVAr, setMinPFOverExcited ≥ rtgMinPFOverExcited etc.), modesSupported bits truthful (opModMaxLimW, opModVoltWatt, opModFixedVAr, opModFixedPFInjectW, opModVoltVar, opModWattPF) | M(C,A) |
| CORE-018 | Basic subscription | Client confirms SubscriptionListLink on its EndDevice, **POSTs subscription** for its FSAList (server 201 + Location); on server-pushed Notification, Client responds **201 Created**, processes payload, GETs the notified href and confirms identical content | M(A only) |
| CORE-019 | Advanced subscription + cancellation | As CORE-018 plus subscription to the EndDevice itself; on Notification with status=1 (subscription cancelled) Client updates internal state and stops expecting notifications on that resource | M(A only) |
| CORE-021 | Randomized events | Three DERControls with randomizeStart/randomizeDuration = 0 / +30s / −30s; Client schedules & executes with randomization applied (effective window = start+duration±randomization); default control between events | M(C,A) |
| CORE-022 | Responses (event-state reporting) | DERControls carry replyTo + **responseRequired=7** (all states). Client POSTs Response to replyTo URI: **status=1 Received, 2 Started, 3 Completed** at the correct times; when the server cancels an event via currentStatus=2 the Client detects it and POSTs **status=6 Cancelled**; polls currentStatus before/at each state | M(C,A) |
| CORE-023 | Superseding events (added in v1.3 body; not in the §4 profile matrix) | 3 same-start overlapping DERControls at different primacy: Client runs the highest-priority one (1 Received, 2 Started, 3 Completed) and POSTs **status=7 Superseded or 14 Aborted-due-to-alternate-program** for the two it does not run; checks EventStatus before acting | (not in matrix) |
| CORE-001/002/004 | Server-side HTTP request/response/list-handling tests | Server-DUT only ([S]) | — |

### 3. Basic functions (BASIC) — identity, groups, inverter controls, event rules, alarms, status, metering

| ID | Purpose | DUT-observable pass behavior | Req |
|---|---|---|---|
| BASIC-001 | DER identification: SFDI/LFDI/PIN | Client validates server-presented SFDI (36-bit truncated SHA-256 of cert, 11 decimal digits w/ check digit) and LFDI (160-bit, 40 hex) against values it computes from **its own TLS cert**; validates PIN; **aggregator: iterates every EndDevice instance in the list and validates SFDI/LFDI/PIN of each managed device** | M(C,A) |
| BASIC-002 | Basic group management: 7-level topology | Client retrieves all 7 FSAs, verifies Time resource in FSA, GETs DERProgramList with `l=255`, picks highest-priority program (IEEE 2030.5 event-priority rules), activates its DefaultDERControl until the scheduled DERControl starts, keeps polling FSA | M(C,A) |
| BASIC-003 | Advanced group management: topology changed mid-test | Server swaps a feeder subtree in the FSAList before the scheduled event; Client detects updated FSAList by polling, re-GETs subordinate resources, and schedules the **new** DefaultDERControl/DERControl | M(C,A) |
| BASIC-004 | L/HVRT curves (opModLVRT/HVRTMustTrip + MomentaryCessation; curveType 4,5,9,10) | Common control-test skeleton: apply DefaultDERControl while idle → at event start GET DERControl & verify currentStatus → activate the specified mode on the inverter → POST Responses per responseRequired=7 → complete at effective end + final response → revert to default | M(C,A) |
| BASIC-005 | L/HFRT curves (opModLFRT/HFRTMustTrip; curveType 2,7) | Same skeleton, frequency ride-through curves | M(C,A) |
| BASIC-006 | Volt-VAr curve (opModVoltVar; curveType 11, openLoopTms) | Same skeleton, volt-var curve executed | M(C,A) |
| BASIC-007 | Ramp rates (setGradW / setSoftGradW) — **Default-only control** | Client detects & activates the DefaultDERControl carrying ramp-rate settings (no timed event) | M(C,A) |
| BASIC-008 | Fixed power factor (opModFixedPFInjectW, immediate; displacement/excitation/multiplier) | Same skeleton, fixed-PF applied | M(C,A) |
| BASIC-009 | Connect/Disconnect (opModEnergize; opModConnect optional) | Same skeleton, energize/connect state applied | M(C,A) |
| BASIC-010 | Limit max active power (opModMaxLimW, immediate, % of max) | Same skeleton, generation cap applied | M(C,A) |
| BASIC-011 | Volt-Watt curve (opModVoltWatt; curveType 12) | Same skeleton | M(C,A) |
| BASIC-012 | Freq-Watt curve + Freq-Droop (opModFreqWatt curve; opModFreqDroop immediate: dBOF/dBUF/kOF/kUF/openLoopTms) | Same skeleton | M(C,A) |
| BASIC-013 | Set active power % (opModFixedW) | Same skeleton | — (no profile requires it) |
| BASIC-014 | Set active power watts (opModTargetW) | Same skeleton | — (no profile requires it) |
| BASIC-015 | Advanced control: **24 successive DERControls** + cancellation | Client schedules all 24 Service-Point events plus 1 Transformer event; executes first; when server cancels the remainder (currentStatus=2) the Client detects it, ignores the cancelled events, reverts to DefaultDERControl (backs CSIP G10: ≥24 stored scheduled events per DER) | M(C,A) |
| BASIC-016 | Event: 2 DERP, 2 DefaultDERC, 0 DERC | Client GETs both programs' DefaultDERControls and **applies the Service-Point (lower-primacy) default** — verified out-of-band (no protocol ack for defaults) | M(C,A) |
| BASIC-017 | Event: 1 DERP, 0 DDERC, 1 DERC | Single scheduled event: Response 1 on receipt, apply at start (Response 2), Response 3 after duration | M(C,A) |
| BASIC-018 | Event: 1 DERP, 1 DDERC, 1 DERC | Default applied while idle; event executed 1/2/3; default re-applied after completion | M(C,A) |
| BASIC-019 | 2 non-overlapping similar events, 1 program | Both events executed in order, default applied in the gap and after each; responses 1/2/3 per event | M(C,A) |
| BASIC-020 | 2 programs, 2 defaults, 2 non-overlapping similar events | SP default wins while idle (lower primacy); each event executed with responses; SP default re-applied after each | M(C,A) |
| BASIC-021 | Overlapping SIMILAR controls, SY then SP, both known before start | Client executes **only the SP (higher-priority) event**; POSTs **status=7 Superseded for the SY event**, never starts it | M(C,A) |
| BASIC-022 | Overlapping SIMILAR, SP scheduled first, SY overlaps | Same outcome: SP executed; SY superseded (status 7), never executed | M(C,A) |
| BASIC-023 | Overlapping SIMILAR, SP created AFTER SY already started | Client runs SY from its start (Responses 1,2); at SP start POSTs **status=7 for the in-flight SY**, switches to SP, completes SP (2,3), reverts to SP default | M(C,A) |
| BASIC-024 | Overlapping INDEPENDENT controls (different opMods), scheduled ahead | Client executes **both** controls concurrently, full 1/2/3 response set for each, per-program defaults after each completes. (Note: CurrentDERProgram deprecated — do not use) | M(C,A) |
| BASIC-025 | Overlapping INDEPENDENT, SP first then SY | Both executed with correct start times; responses per event; defaults restored per program | M(C,A) |
| BASIC-026 | Overlapping INDEPENDENT, SP created after SY started | Both executed; responses per event; defaults restored | M(C,A) |
| BASIC-027 | Alarms / LogEvents | Client finds LogEventListLink on its EndDevice and **POSTs a LogEvent** (LE_GEN_SOFTWARE; createdDateTime, functionSet=0, logEventCode, logEventID, logEventPEN, profileID) → server 201+Location; repeated ×5; list retrievable time-ordered | M(C,A) |
| BASIC-028 | Inverter status reporting | Client GETs DERList subordinates then **PUTs DERStatus** reflecting current state — inverter: genConnectStatus, inverterStatus, localControlModeStatus, manufacturerStatus, operationalModeStatus, readingTime; storage adds stateOfChargeStatus, storageModeStatus, storConnectStatus — server replies 204 | M(C,A) |
| BASIC-029 | Inverter meter reading via MUP | Client POSTs **MirrorUsagePoint** + MirrorMeterReading/ReadingType definitions (own LFDI; unique mRIDs) for **Real Power, Reactive Power, Frequency, Voltage** (201+Location each), then POSTs Readings whose mRID matches the ReadingType setup (204); GETs MirrorUsagePoint first to honor server **postRate**; posted data must read back identical. **Aggregator sums values across its managed DERs** | M(C,A) |

### 4. Utility-server / aggregator model (UTIL) — aggregator commissioning against the utility server

Topology for UTIL/AGG/MAINT: aggregator manages 4 EndDevices — EDA1/EDA2 (service points SPA1/SPA2 under transformer TFA / feeder FDA) and EDB1/EDB2 (SPB1/SPB2 under TFB/FDB), all under substation/system (SY) nodes; a DERProgram per topology node.

| ID | Purpose | DUT-observable pass behavior | Req |
|---|---|---|---|
| UTIL-001 | Server startup group assignment (up to 9 levels) | Server-DUT only | — |
| UTIL-002 | Aggregator commissioning | Aggregator GETs dcap→EndDeviceList→**all** EndDevices; verifies its OWN EndDevice by SFDI/LFDI; validates its Registration PIN; for each managed EndDevice (EDA1/EDA2/EDB1/EDB2) finds DERListLink and **PUTs DERCapability/DERSettings/DERStatus/DERAvailability** | M(A) |
| UTIL-003 | Group-assignment retrieval | For each managed EndDevice: GET all FSAs and all DERPrograms; **subscribe to each DERProgramList** (primacy-change notify) and to each program's **DERControlList** (new-control notify) | M(A) |
| UTIL-004 | DER retrieval + event fan-out | Server creates DERControl at each topology level (SP, feeder, SY) and notifies; aggregator GETs the new control and **POSTs received/started/completed Responses for exactly the EndDevices under that node** (SP event→1 device; feeder→2; SY→all 4) at correct times | M(A) |

### 5. Aggregator operation (AGG) — multi-DER event scoping and conflict resolution

All M(A). Same event-rule logic as BASIC-016..026 but the pass criterion is per-END-DEVICE scoping: responses/actuation only for devices under the targeted node; **explicit fail if EDB1/EDB2 POST any response to a TFA event**.

| ID | Purpose | DUT-observable pass behavior |
|---|---|---|
| AGG-001 | Aggregator subscription machinery | Subscribes to EndDeviceList; on server adding EDA1X, receives Notification (responds 201), GETs list + new instance |
| AGG-002 | 2 DERP, 2 DDERC, 0 DERC | Applies TFA default to EDA1/EDA2 and SY default to EDB1/EDB2 (out-of-band verification) |
| AGG-003 | 1 DERP, 0 DDERC, 1 DERC (TFA) | Executes event for EDA1/EDA2 only, POSTs 1/2/3 **per device**; fails if EDB* responds |
| AGG-004 | 1 DERP, 1 DDERC, 1 DERC | TFA default before/after; event 1/2/3 for EDA1/EDA2 only |
| AGG-005 | 2 non-overlapping similar DERC, one program | Both TFA events executed in order for EDA1/EDA2, defaults between; per-device responses |
| AGG-006 | 2 programs, 2 defaults, 2 non-overlapping similar DERC | TFA event → EDA*, SY event → EDB* handling with correct per-group defaults & responses |
| AGG-007 | Overlapping similar: SY then TFA, both known before start | EDA1/EDA2: TFA executed, **status=14 (superseded by other program) POSTed for SY**; EDB1/EDB2: SY executed normally (1/2/3) |
| AGG-008 | Overlapping similar: TFA first, SY overlaps | Same split outcome: EDA* run TFA + supersede SY (14); EDB* run SY |
| AGG-009 | Overlapping similar: TFA created after SY started | All 4 start SY; at TFA start EDA* POST **status=7** for in-flight SY and switch to TFA; EDB* finish SY |
| AGG-010 | Overlapping independent: SY then TFA before start | EDA* execute **both** (SY + TFA); EDB* execute SY; per-device 1/2/3 for each event; per-program defaults after |
| AGG-011 | Overlapping independent: TFA first then SY | Both executed on EDA*; SY on all 4 |
| AGG-012 | Overlapping independent: TFA created after SY started | Both executed on EDA*; SY on all 4 |

### 6. Error handling (ERR)

| ID | Purpose | DUT-observable pass behavior | Req |
|---|---|---|---|
| ERR-001 | HTTP redirect handling | On GET dcap the Test Server returns **301/302 + Location**; Client follows to the new URI over TLS, GETs dcap (200), continues walking resources | M(C,A) |
| ERR-002 | Subscription error handling | After server restart (subscriptions persist server-side) Client processes Notification normally (201/204, GET href, content identical); on cancellation Notification (status=1) removes the subscription from its own list; on an **invalid Notification** (wrong resource type in body) Client responds **HTTP 400** (server then silently deletes that subscription) | M(A only) |

### 7. Maintenance of the model (MAINT)

| ID | Purpose | DUT-observable pass behavior | Req |
|---|---|---|---|
| MAINT-001 | Inverter population change (out-of-band delete) | Aggregator subscribes to EndDeviceList; when server deletes EDA1X it processes the Notification, GETs remaining list, **stops managing the deleted device**; GET on deleted href yields 404 | M(A) |
| MAINT-002 | Inverter population change (in-band: aggregator issues HTTP DELETE on the EndDevice) | Aggregator DELETEs EDA1X, processes resulting notifications, updates internal state | — (removed from requirements in v1.3) |
| MAINT-003 | Group/topology maintenance | Server moves EDB1 SPB1→SPA1; aggregator receives FSAList notification, GETs new FSAList + DERProgramList for EDB1, re-subscribes (cancels old subs if needed), applies the correct new default | M(A) |
| MAINT-004 | Controls maintenance | Aggregator subscribed to DERProgramList + DERControlList (limit = `all`); on server adding a DERControl (+15 min) it GETs subordinates, applies event-processing rules across ALL controls per EndDevice, schedules, sends required responses | M(A) |
| MAINT-005 | Programs maintenance (primacy swap) | Server swaps primacy of TFA/TFB vs SGA/SGB programs and notifies; aggregator re-evaluates priorities and **activates the DefaultDERControl of the new highest-priority program** (if no active event) | M(A) |

## PICS / capability-claim structure

- **§4 Profile Test Conformance matrix** is the top-level claim: you certify as one of three profiles — Server, DER Client, **DER Aggregator Client** — and every X'ed test for that profile is mandatory. For the DER Aggregator Client that is: COMM-002/003/004; CORE-003/005/009/010/011/012/013/014/**018/019**/021/022; BASIC-001–012, 015–029; UTIL-002/003/004; AGG-001–012; ERR-001/**002**; MAINT-001/003/004/005. (Aggregator = DER Client set **plus** subscription (CORE-018/019), ERR-002, UTIL, AGG, MAINT.)
- Not required for any profile in v1.3: BASIC-013/014 (opModFixedW/opModTargetW), COMM-001 (xmDNS optional), CORE-001/002/004 & UTIL-001 (server-side), MAINT-002. CORE-023 exists in the body (errata addition) but is absent from the matrix.
- **CSIP Requirements Matrix** (Annex): ~32 "G" (general) + ~52 "P" (protocol) CSIP requirements, each mapped to a test group or marked "(optional) No test; utility handbook policy" / "Out-of-scope". Untested-but-claimed obligations relevant to us: G10 store ≥24 scheduled events per DER; G12 on comms loss complete scheduled event then revert to default; G19 default poll 10 min for (Default)DERControls, post monitoring every 5 min; G22 aggregator transfers control to DERs within 15 min; P24 support 15 simultaneous DERPrograms per DER; P10 aggregator SHOULD also support TLS_RSA_WITH_AES_256_CBC_SHA256 if utility requires; P13 certs provisioned only after conformance testing; P14/P17 LFDI = SHA-256 of device cert, authorization keyed on LFDI.
- **IEEE 2030.5 Requirements Matrix + "Requirements Tested" appendix**: every atomic 2030.5 requirement (prefixes BASE/DNS/GEN/SEC/TIME/DER/EVENT/RAND/METER/MUP/LOG/MULTI…) is cross-referenced to the test IDs exercising it (e.g. COMM-003 ⇐ SEC.015–.027; CORE-012 ⇐ DER.001–.013/.034/.041; CORE-004 ⇐ GEN.014–.036). This is the traceability layer behind each test.
- The TRR requires a **Protocol Implementation Conformance Statement (PICS)** as a URL field — a per-product capability declaration document hosted alongside the report; per-test results may be marked **NOT SUPPORTED** (the capability-claim escape hatch for optional features, e.g. modes the DER doesn't implement).

## Certification / reporting requirements

- Certification = SunSpec Authorized Test Laboratory runs the profile's required tests and submits a **Test Results Report** to SunSpec; SunSpec evaluates it and grants the **SunSpec Certified** mark. Summary results are published publicly; detailed logs are archived privately.
- **Configuration freeze**: no software/hardware changes during the campaign; a Client config may not change at all once testing starts (an Aggregator Client config may change **only as a test specifies**); any other change restarts the whole test process.
- **TRR contents** (CSV key/value): certificate type/number, company identity, test lab + supervising engineer, software name/version/**checksum**, OS/version, operating environment (Cloud vs Hardware Device), hardware manufacturer/model, PICS URL, test date/description/comments, then one `Test <ID>` row per test with value **PASS / FAIL / NOT SUPPORTED**. Certification implies PASS on every test required by the claimed profile.
- **Detailed Test Logs**: JSON — TestLogs{logs[],cid} → TestLog{tests[],cid,messages[]} → Message{time (sub-second), type req/resp, method, uri, vers, headers{}, body, code/reason} — containing **every HTTP(S) message in unencrypted form** for every test.
- **COMM-004**: additionally submit a **Libpcap/PcapNg packet trace of the TLS handshake for each certificate scenario** (A–G) — the cert-rejection behavior must be provable at packet level.

## Notes for a client that is an AGGREGATOR (multi-DER) — LEXA hub relevance

1. **Aggregator = superset profile.** Everything a DER Client must do, PLUS subscription/notification (CORE-018/019, AGG-001, UTIL-003/004, MAINT-*, ERR-002) is **mandatory** — polling alone does not certify an aggregator (CSIP G16: aggregators SHALL use subscription/notification to limit polling). LEXA's poll-only discovery walker would need a notification-server endpoint (client hosts an HTTP listener for server-POSTed Notifications, answering 201/204, and 400 on malformed bodies).
2. **Per-end-device identity and fan-out.** The aggregator manages a population of EndDevice instances (one per DER) under one aggregator EndDevice: it must validate SFDI/LFDI/PIN for **every** managed device (BASIC-001 step 6), and scope every event application and every Response POST to exactly the devices under the targeted topology node — an over-broad response is an explicit FAIL (AGG-003/004/005: "fails if EDB1/EDB2 POSTs"). LEXA currently presents one aggregate; a cert campaign needs per-DER EndDevice handling.
3. **Response discipline is the heartbeat of every event test**: status 1 (received) on discovery, 2 (started) at effective start, 3 (completed) at effective end, 6 (cancelled), 7 (superseded within program), 14 (superseded by other program) — POSTed to replyTo, gated by responseRequired, **per end device**. LEXA's responses.Tracker covers CannotComply-style posting; the full 1/2/3/6/7/14 lifecycle per event per device is the certification surface.
4. **Event rules engine**: primacy-based supersession only between **similar** (same opMod) controls; **independent** controls run concurrently; late-arriving higher-priority events preempt in-flight ones (status 7 mid-event); cancellation detected by polling currentStatus; randomizeStart/randomizeDuration applied (CORE-021); ≥24 queued events per DER (BASIC-015/G10); 15 FSAs deep (CORE-011/P24); revert-to-DefaultDERControl whenever no event is active is checked in nearly every test.
5. **DER info reporting is client-initiated writes**: PUT DERCapability/DERSettings (settings ≤ nameplate invariants), PUT DERStatus (storage adds SoC/storConnectStatus), POST LogEvents for alarms, and MUP metering: POST MirrorUsagePoint + ReadingType per quantity (W, VAr, Hz, V) then Readings at the server's postRate, with **aggregated (summed) values across managed DERs**. LEXA's telemetry MUP poster aligns; ReadingType mRID discipline and postRate honoring are the pass criteria.
6. **TLS posture matches LEXA**: TLS 1.2 + ECDHE-ECDSA-AES128-CCM-8 only, client cert (LFDI = SHA-256 over full DER cert — LEXA already implements this), and COMM-004 requires **rejecting** bad chains (bad MICA extensions, self-signed) with a TLS alert/disconnect, provable by pcap. wolfSSL chain-validation config must be fail-closed for chains up to length 4.
7. **Fail-closed / error handling**: follow 301/302 redirects (ERR-001, also httpwire relevance), stop repeating requests that drew 4xx/5xx (CORE-001/002 client criteria), 400 on invalid Notification bodies, treat unrecognized HTTP codes as x00 of class (GEN.049).
8. **Timing defaults to design against**: poll (Default)DERControls every 10 min, post monitoring every 5 min (G19), aggregator control transfer to DER ≤15 min (G22), honor per-resource pollRate and MUP postRate when advertised.
