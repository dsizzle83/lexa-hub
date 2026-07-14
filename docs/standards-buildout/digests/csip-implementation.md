# CSIP client requirement digest (CSIP v2.1 + EPRI 2030.5 Client User's Manual)

## Document identity

1. **CSIP Implementation Guide v2.1**, Common Smart Inverter Profile Working Group,
   March 2018 ("CSIPImplementationGuidev2.103-15-2018.txt", 3046 lines). Profiles
   **IEEE 2030.5-2018** (P1) for CA Rule 21 Phase 2/3 communications between CA IOUs
   and DER. Normative keywords per RFC 2119 (SHALL/SHOULD/MUST/MAY, §4 fn.3).
   Requirements are consolidated in **Appendix A**: General reqs **G1–G32** and IEEE
   2030.5 Protocol reqs **P1–P52** (reproduced condensed below). Explicit design
   principle: "eliminate optionality" (§2.5) — very few true options remain; most
   variability is delegated to each utility's Interconnection Handbook/contract.
2. **EPRI IEEE 2030.5 Client User's Manual**, EPRI report 3002014087, July 2018.
   Developer manual for EPRI's open-source C client (github epri-dev/IEEE-2030.5-Client
   v0.2.11), QualityLogic-certified against the SunSpec CA Rule 21/CSIP Test
   Procedures. Informative, but documents the certification test scope (PICS) and
   practical client behaviors (§6–8).

### Deployment topologies (CSIP §3.2, Fig 1–2)
- **Scenario 1 — Direct DER Communications**: utility talks to a "DER Client" =
  (a) SMCU (single DER, single EndDevice) or (b) **GFEMS** — a facility EMS mediating
  one or more local DERs behind one PCC, appearing as a **single IEEE 2030.5
  EndDevice** whose FSAs reflect the *aggregate* plant capability at the PCC (P28).
  A "DER" in CSIP is a logical concept: one or more inverters behind a single PCC
  (§3.2 ¶177-183). *LEXA hub as deployed (one gateway, one connection point, multiple
  local assets) maps to the GFEMS role.*
- **Scenario 2 — Aggregator Mediated**: utility talks to an Aggregator back-end; the
  Aggregator itself AND every DER it manages are each an EndDevice (§5.1.2.3).
- **G1**: each DER Client SHALL connect to the utility in one and only one scenario;
  the utility designates which.

## Required function sets for CSIP clients (Table 7, §5.1.2; P2)

| Function set | Utility server | Aggregator | DER Client (direct) |
|---|---|---|---|
| Time | MUST | MUST | MUST |
| DeviceCapability | MUST | MUST | MUST |
| EndDevice | MUST | MUST | MUST |
| FunctionSetAssignments (FSA) | MUST | MUST | MUST |
| DER | MUST | MUST | MUST |
| Response | MAY | **MUST** | **MUST** |
| Metering / Mirror Metering | MAY | **MUST** | **MUST** |
| Log Event | MUST | MUST | MUST |
| Subscription/Notification | MUST | **MUST** | **MAY** |
| Security | MUST | MUST | MUST |

- **EndDevice:DER sub-resources (Table 8; P4)** — client SHALL support **if the
  server makes them available**: `DERCapability`, `DERSettings`, `DERStatus`,
  `DERAvailability` (all four written by the client via HTTP PUT, §7.7 Fig 31).
- P5: clients SHALL also meet **all IEEE 2030.5-2018 mandatory requirements** for
  each required function set, unless the utility handbook says otherwise.
- Time: UTC; server-event timing keyed to the server Time resource; sync per
  2030.5-2018 (§5.1.2.1, P3).
- SunSpec/QualityLogic PICS (EPRI Table 6-2) marks mandatory for cert: DCAP, DER,
  DNS discovery, EDEV, EVENT rules, FSA, GEN networking, LOG, METER, MUP, **RAND
  (randomization)**, Response, SEC, TIME. Subscription listed M in the PICS table
  but EPRI certified WITHOUT it — all tests executed via polling (Table 6-1 note),
  i.e. poll-only DER clients are certifiable in practice.

## Required DER control modes (Table 9, §5.2.4; P29)

All CSIP DERs support Autonomous + Advanced functions (Table 1, §4.4.2). 2030.5
mapping — each exists as DERControl (event) and/or DefaultDERControl (setting):

| Grid function | DERControl (event) | DefaultDERControl |
|---|---|---|
| Low/High Voltage Ride-Through | opModLVRTMUSTTrip, opModLVRTMAYTrip, opModLVRTMomentaryCessation, opModHVRTMUSTTrip, opModHVRTMAYTrip, opModHVRTMomentaryCessation | same six |
| Low/High Frequency Ride-Through | opModLFRTMUSTTrip, opModLFRTMAYTrip, opModHFRTMUSTTrip, opModHFRTMAYTrip | same four |
| Ramp rate setting | — (default-only, cannot be scheduled §5.2.4 ¶602-604) | setGradW, setSoftGradW |
| Connect / Disconnect | opModEnergize | opModEnergize |
| Dynamic Volt-VAr | opModVoltVar (curve) | opModVoltVar |
| Fixed power factor | opModFixedPF | opModFixedPF |
| Real power output limit | **opModMaxLimW** | opModMaxLimW |
| Volt-Watt | opModVoltWatt (curve) | opModVoltWatt |
| Frequency-Watt | opModFreqWatt (curve) | opModFreqWatt |
| Set active power (% of max) | opModFixedW | opModFixedW |
| Set active power (Watts) | opModTargetW | opModTargetW |

- **Note for the audit**: `opModExpLimW`, `opModGenLimW`, `opModImpLimW`,
  `opModLoadLimW` do **not appear anywhere in CSIP v2.1** — export/import-limit
  controls of that form are CSIP-AUS extensions. CSIP v2.1's real-power lever is
  `opModMaxLimW` (+ opModFixedW/opModTargetW). Anti-islanding is autonomous
  (Table 1) with no op-mode mapping.
- **DefaultDERControl semantics** (§4.4.1, G6/G7): autonomous-function default
  settings SHALL be changeable via DefaultDERControl; modifications take effect
  **immediately on receipt** with indefinite duration; no status responses exist
  for defaults. Settings-with-status pattern = DERControl with start "now" +
  effectively-infinite duration, later superseded/cancelled (§4.4.1 ¶280-283).
- **Curves** (§5.1.2.5.4, §6.1.8.3): DERCurve = X-Y points (CurveData, curveType,
  x/yMultiplier, yRefType); referenced from DERControlBase by href (Fig 12).
  Only ONE curve per curve-type active at a time; different curve-types may be
  simultaneously active if not conflicting. Curve controls are schedulable events
  and may have a default curve.
- **Scheduling/eventing**: DERControls are 2030.5 events and SHALL conform to ALL
  event rules of IEEE 2030.5-2018 §12.1.3 (P30). Client SHALL store ≥**24 scheduled
  DER control events per DER** (G10) and process operations in the utility's time
  sequence (G9).
- **Loss of comms / no schedule** (G11/G12): with no scheduled controls, revert to
  default control setting (tariff/handbook or last DefaultDERControl); on comms
  loss, COMPLETE any scheduled event then revert to defaults.
- **Prioritization** (§4.4.3, §5.2.4.2, §7.10): priority = DERProgram `primacy`
  (lower value = higher priority); controls only conflict if they affect the SAME
  control type; same-priority conflict → more recently received SHOULD win (G14),
  and the client SHALL decide how to handle simultaneous controls (G13/G15). With
  no active events, execute the DefaultDERControl of the highest-priority (lowest
  primacy) program. A superseded event is NEVER resumed after the superseding
  event completes (§7.10 ¶1094-1095).

## Monitoring / telemetry requirements (§4.5, §4.6, §5.2.5)

**Quantities (Table 2 → Table 10 mapping; G23–G27, P41)** — via **MirrorUsagePoint /
MirrorMeterReading** (POST MUP to MirrorUsagePointListLink from DeviceCapability,
then POST MirrorMeterReadings to the returned /mup/N location):

| Quantity | ReadingType uom |
|---|---|
| Real (active) power | 38 (W) |
| Reactive power | 63 (VAr) |
| Frequency | 33 (Hz) |
| Voltage (per phase) | 29 (V) |

- Data qualifiers (Table 3/11, MAY): 0 not-specified/instantaneous, 2 average,
  8 maximum, 9 minimum. Instantaneous needs no dataQualifier.
- Every measurement SHALL carry a date-time stamp (G25). If the DER cannot
  provide a quantity, do NOT send it (G27).
- Posting cadence: **default every 5 minutes** (G19, both scenarios), or per
  `MirrorUsagePoint:postRate` (SHOULD, P42).

**Nameplate ratings & adjusted settings (Table 4 → Table 12; G28/G29, P43)** —
DERCapability (rtg*) / DERSettings (set*), reported **once at start-up and on any
change** (SHOULD):
rtg/setMaxChargeRateW, rtg/setMaxDischargeRateW, rtg/setMaxVA, rtg/setMaxVar,
rtg/setMaxVarNeg, rtg/setMaxW, rtg/setMinPFOverExcited, rtg/setMinPFUnderExcited,
rtg/setMaxWh. (DERCapability also carries modesSupported bitmap + type, Fig 20.)

**Operational status (Table 5 → Table 13; G30, P44)** — DERStatus, posted at the
rate in `DERList:pollRate` (§6.2.5.2/§6.3.5.2); reporting frequency otherwise
utility-specified:
- Operational state → `operationalModeStatus`, `inverterStatus`
- Connection status → `genConnectStatus`
- Alarm status → `alarmStatus` (bitmap of active alarms, §5.2.5.3 ¶669)
- Operational energy storage capacity → `stateOfChargeStatus`

**Alarms (Table 6 → Table 14; G31/G32, P45)** — LogEvent function set,
`functionSet=11` (DER), posted **as they occur**, each with date-time stamp; every
alarm has a paired return-to-normal (RTN) LogEvent. Codes 0–21:
OVER_CURRENT(0/1), OVER_VOLTAGE(2/3), UNDER_VOLTAGE(4/5), OVER_FREQUENCY(6/7),
UNDER_FREQUENCY(8/9), VOLTAGE_IMBALANCE(10/11), CURRENT_IMBALANCE(12/13),
EMERGENCY_LOCAL(14/15), EMERGENCY_REMOTE(16/17), LOW_POWER_INPUT(18/19),
PHASE_ROTATION(20/21) (even=alarm, odd=RTN). Low-level equipment health is
deliberately out of scope (§4.6.3 ¶379-380).

**Polling & control-latency cadences (§4.5; G17–G22, P34–P38)**

| Interaction | Default | Override |
|---|---|---|
| Poll DERControls + DefaultDERControls (Direct) | **10 min** | `DERProgramList:pollRate` (SHOULD honor, P37/38) |
| Post monitoring data (both scenarios) | **5 min** | utility handbook / postRate |
| SMCU applies received control to DER | ≤ 10 min (G20) | — |
| GFEMS applies received control to its DERs | ≤ 10 min (G21) | — |
| Aggregator transfers control to its DERs | ≤ 15 min (G22) | — |
| DER Client polls DERProgram (primacy changes) | per pollRate (P34) | subscriptions if handbook allows |
| Aggregator subscription renewal | ~every 24 h (P52 SHOULD) | — |

- **G17/G18**: Direct-scenario communications SHALL be client-initiated
  (client-side polling/posting; no inbound connections needed at the DER).

## Aggregator-topology requirements

- **EndDevice model**: Aggregator = special EndDevice with `SubscriptionListLink`
  and NO FSA link; each managed DER = its own EndDevice with FSA link + DERListLink
  (§6.1.7, Fig 9/11). Server presents each aggregator a filtered EndDeviceList of
  only its own DERs (P20); aggregator walks all instances to learn each DER's group
  assignments (§6.1.5).
- **Subscription/notification is SHALL** for aggregators (G16, §5.2.5, P39):
  subscribe to (a) EndDeviceList, (b) FunctionSetAssignmentsList of each DER,
  (c) DERProgramList of each DER, (d) DERControlList of each DER,
  (e) DefaultDERControl of each DER. MAY subscribe to more (P40). SHOULD renew
  subscriptions ~24 h and **fall back to polling on perceived comms errors** (P52).
  Subscription = POST {subscribedResource, encoding, level "+S1", limit,
  notificationURI} → 201 + Location; server POSTs Notification (may embed changed
  entries; client may need follow-up list GETs) (§6.2.3.3–6.2.3.4, §7.5–7.6).
- **Scaling**: aggregator must track overlapping resource interests across its
  fleet and subscribe to a shared resource only ONCE (§6.2.3.3 ¶909-912).
- **Group management (§4.3, §5.2.3; G5, P23–P27)**: DER in ≥1 and ≤**15 groups**;
  client SHALL track the DERProgram associated with each group, support up to
  **15 DERPrograms simultaneously per DER** (P24) and **15 FSAs** (P27); SHALL
  traverse EndDevice → FunctionSetAssignmentsListLink → FSAList → each FSA's
  DERProgramListLink → DERProgramList to discover ALL tracked DERPrograms (P25).
  Server uses one FSA for all topology-based DERPrograms + optional FSAs for
  non-topology programs (P26). Group tiers (Fig 3): System / Sub-transmission /
  Substation / Feeder / Segment / Service Transformer / Service Point +
  non-topology. Lower-level (more local) groups typically get lower primacy
  values → higher precedence (§4.4.3).
- **Direct-scenario contrast (P28)**: a DER Client/GFEMS receives FSAs for a
  SINGLE energy connection point reflecting aggregate plant capability at the PCC
  — i.e., the multi-asset gateway case does NOT become an aggregator unless the
  utility registers each asset as its own EndDevice.
- **Fleet maintenance (§5.3, §6.1.3–6.1.5)**: EndDevice add/remove is out-of-band
  (utility server edits list; aggregator notified via subscription) or **in-band**
  (aggregator POSTs new EndDevice → 201 Created, or DELETEs → 200 OK; 4XX on
  refusal). Aggregator SHOULD subscribe to EndDeviceList (P46), each EndDevice
  instance (P47), each FSA list (P48), DERControlLists (P49), DERPrograms (P50),
  DERProgramLists (P51).
- **Responses**: when responses are enabled on a DERControl, the aggregator posts
  the appropriate Responses **on behalf of each targeted DER** (§6.2.4 ¶929-930).
- **Monitoring**: one MUP per managed DER, keyed by `deviceLFDI`; DERStatus/
  DERCapability/DERSettings/DERAvailability PUT per DER (§6.2.5, §7.7–7.8).
- **Aggregator ciphers**: additionally TLS_RSA_WITH_AES_256_CBC_SHA256 or other
  utility-specified suites (P10).

## Registration & PKI

- **Registration is utility-specific** (§4.2); the process yields a GUID. For any
  client with an IEEE 2030.5 certificate the GUID SHALL be derived from that cert
  (G3): **GUID = LFDI = first 20 bytes (160 bits) of SHA-256 of the device
  certificate** (P14, §5.2.1.2/§5.2.2). SFDI = 36-bit SHA-256 hash rendered as 12
  decimal digits (§5.2.2). Direct scenario: DER's own LFDI (P21). Aggregator
  scenario: managed DERs may have NO cert — utility or aggregator produces the
  LFDI and returns it to the aggregator for routing (§5.2.2 ¶555-558). LFDI
  collision → utility replaces certs (¶561-565).
- **Registration models (§6.1.2–6.1.5)**: *Out-of-band* — server pre-creates
  EndDevice instances; authorized DER's EndDeviceList GET returns exactly 1 entry;
  in-band POST attempts get 403. *In-band* — authorized DER initially sees an
  EMPTY EndDeviceList and POSTs its EndDevice instance → 201 Created + Location
  (unauthorized → 403). Unregistered/unauthorized access → 404/403 (§6.1.2, P18).
- **PIN check (EPRI §8, DER Client Model step 4)**: per 2030.5, registration
  completes when the client retrieves its own EndDevice and verifies the linked
  `Registration` resource contains a **matching PIN**; if registered but absent
  from EndDeviceList, the client POSTs its EndDevice instance.
- **TLS**: HTTPS for ALL communications (P6; HTTP explicitly not permitted,
  §6.2.1.1/§6.3.1.1). **TLS 1.2** (P8). DER Clients SHALL support
  **TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8 on secp256r1** — the single 2030.5 suite
  (P9, §5.2.1.1); aggregators add the RSA suite (P10).
- **Certificates**: every server/aggregator/client SHALL have a valid certificate
  used in ALL 2030.5 TLS transactions (P11/P12); certs provisioned **only upon
  completion of conformance testing** (P13). The utility chooses the PKI: IEEE
  2030.5/CSIP CA, commercial third-party CA, private CA, or self-signed
  (§5.2.1 ¶496-501) — cert-profile details deferred to the utility handbook.
- **Mutual authentication** (§5.2.1.3, P15/P16): both sides verify cert integrity,
  expiry, and chain to the correct root CA; on failure SHOULD send TLS Alert –
  Bad Certificate and close.
- **Authorization/ACL** (§5.2.1.4–5.2.1.5, P17–P20): server authorization list
  keyed by **LFDI** (SFDI has insufficient collision protection at ~1M devices);
  per-resource ACLs; per-identity resource views.
- **Discovery (§6.2.1, §6.3.1)**: either out-of-band provisioning (IP/DNS name +
  HTTPS port + DeviceCapability path) or unicast DNS + **DNS-SD** for port/scheme/
  path. EPRI adds (§8): known address+port is the expected common method over the
  internet; DNS-SD subtypes include derp, tm, edev, mup, rsps, smartenergy etc.
  (EPRI §4).

## Response / event requirements

- Response function set is **MUST for both aggregator and DER client** (Table 7);
  it reports DERControl event status: "event reception, event start, event
  completion, event cancellation, etc." (§5.1.2.6).
- Trigger: DERControl carries `replyTo` (Response list URI) and `responseRequired`
  (bitmap; examples show `responseRequired="03"` = received+started/completed
  class, `"00"` = none) (Fig 12, §7.4/7.6). "If Responses are enabled for the
  DERControl, the client POSTs the appropriate Responses" (§6.3.4 ¶1013; aggregator
  per-DER §6.2.4 ¶929-930). DefaultDERControls have NO responses (§4.4.1 ¶278-279).
- **Response types shown in CSIP sequence figures (Fig 34/35)**: "Type 0: DERC
  Received", "Type 1: DERC Started", "Type 2: DERC Completed", "Type 13: DERC
  Superseded by Alternate Program or Provider". **EPRI's certified flow (§7)
  instead posts status 1 = Event Received, 2 = Event Started, 3 = Event
  Completed** — matching IEEE 2030.5-2018 ResponseType enumeration. The CSIP
  figure labels are off-by-one/informal; treat 2030.5-2018 Table 27 ResponseType
  codes as authoritative (audit cross-check: LEXA's response poster should use
  the 2030.5 enum values, incl. "cannot comply").
- **Timing** (Fig 34/35, EPRI §7): Received — on retrieval/notification of the
  event; Started — at (effective, randomized) start time; Completed — when the
  duration elapses; Superseded — when a higher-priority overlapping control
  displaces it (posted at supersede determination, even pre-start). Opt-out also
  requires an appropriate response message (EPRI §8 step 8). No CSIP-specific
  latency bound on response posting itself beyond these event edges.
- **Event state machine**: per IEEE 2030.5-2018 §12.1.3 (P30), including
  EventStatus.currentStatus (0=scheduled, 1=active), potentiallySuperseded,
  cancellation, randomizeStart/randomizeDuration handling (RAND mandatory in the
  SunSpec PICS; EPRI scheduler applies event randomization, §8 step 7).
- Overlap handling worked examples (§7.10): supersede-before-start (never run the
  low-priority event; post Superseded) and supersede-after-start (run until the
  higher-priority start, then switch; post Superseded + Started); never resume a
  superseded event.

## EPRI manual — practical client behaviors worth cross-checking

- **Poll-only operation is certifiable**: EPRI client has no subscription support;
  all QualityLogic CSIP tests ran with polling (Table 6-1). Default active-event
  poll = **300 s** (client_test `poll` command), example FSA list poll = 10 s.
- Test categories (Table 6-1, all Mandatory): COMM fundamentals, CORE function
  set, BASIC DER functions, AGG aggregator operation, ERR error handling, MAINT
  model maintenance — a ready-made outline for LEXA conformance-test scoping.
- **Retrieval model**: resources form a DAG from DeviceCapability; dependency
  tracking + completion routines gate local event scheduling on a *complete*
  EndDevice subtree; re-poll invalidates completion until re-retrieved — schedule
  only on model updates (§8 steps 5–7, 9).
- **Model maintenance**: poll must pick up group reassignment, program add/remove,
  primacy changes, cancels, supersedes (§8 step 9) — matches CSIP §5.3.
- **Time**: client sets local clock from the server Time resource on retrieval
  (`set_time` in csip_dep) — LEXA parallel: utilitytime ClockOffset.
- **Registration**: DNS-SD subtype query can match server by SFDI; real
  applications must still verify Registration PIN (§8 step 4).
- **Cipher**: OpenSSL `ECDHE-ECDSA-AES128-CCM8` pinned as the sole suite (§9);
  RSA suites are an optional recompile-time addition — mirrors LEXA's wolfSSL
  single-cipher posture.
- Applying a control "can mean sending a series of MODBUS commands to an
  inverter" (§8 step 8) — the gateway-translation pattern LEXA implements.

## Numbered requirement inventory (Appendix A, condensed; §-refs in body above)

General (G):
- G1 one-and-only-one scenario per DER client (§3.2)
- G2 security SHOULD extend to non-2030.5 legs (§4.1)
- G3 GUID derived from 2030.5 cert (§4.2) · G4 handbook governs identifier mgmt
- G5 support 2030.5 grouping + full lifecycle mgmt (§4.3/§5.2.3)
- G6 defaults changeable via DefaultDERControl · G7 default changes immediate,
  indefinite duration (§4.4.2)
- G8 scheduled ops via DERControl events · G9 process ops in utility time sequence
- G10 store ≥24 scheduled control events per DER
- G11 revert to default setting absent schedules · G12 on comms loss complete
  scheduled event then revert to defaults
- G13 highest-priority control wins · G14 same-priority: newer SHOULD win ·
  G15 client decides simultaneous-control handling (§4.4.3)
- G16 aggregators SHALL use subscription/notification · G17 direct scenario:
  client-initiated comms · G18 client polls/posts on predefined intervals
- G19 defaults: control polling 10 min (direct), monitoring posting 5 min (both)
- G20 SMCU ≤10 min control transfer · G21 GFEMS ≤10 min · G22 aggregator ≤15 min
- G23 report Table 2 monitoring data · G24 capability for Table 3 qualifiers ·
  G25 timestamp all measurements · G26 report Table 2, MAY qualifiers ·
  G27 never fabricate unavailable data
- G28 report Table 4 nameplate/settings · G29 at startup + on change (SHOULD)
- G30 report Table 5 operational status · G31 report Table 6 alarms as they occur ·
  G32 alarms + RTN timestamped with type

Protocol (P):
- P1 IEEE 2030.5-2018 exactly · P2 Table 7 function sets · P3 2030.5 time sync ·
  P4 Table 8 EndDevice:DER resources if offered · P5 all 2030.5 mandatory reqs
- P6 HTTPS everywhere · P7 support required security framework(s) · P8 TLS 1.2 ·
  P9 DER client: 2030.5 CCM-8 suite · P10 aggregator: +RSA suite
- P11 valid cert required · P12 valid cert in all TLS transactions · P13 certs
  only after conformance testing · P14 GUID = LFDI (20-byte SHA-256 of cert) ·
  P15 utility-specified certs for auth · P16 TLS Alert Bad Certificate on failure
- P17 authorization by LFDI · P18 404 for unauthorized (server) · P19 utility
  sets ACL permissions · P20 per-aggregator EndDeviceList filtering
- P21 direct-scenario GUID = DER's LFDI · P22 handbook for LFDI details
- P23 track DERProgram per group · P24 ≤15 DERPrograms per DER · P25 traverse
  FSA→DERProgramList links fully · P26 one topology FSA + optional non-topology
  FSAs (server) · P27 client supports 15 FSAs · P28 direct client: single
  connection point, aggregate at PCC
- P29 Table 9 control mappings · P30 conform to 2030.5 §12.1.3 event rules
- P31–P33 aggregator SHALL subscribe to DERProgramList/DERControlList/
  DefaultDERControl per DER · P34–P36 DER client SHALL poll same three (unless
  handbook allows subscription) · P37 server MAY set pollRate · P38 client SHOULD
  honor pollRate
- P39 aggregator subscription list (EndDeviceList, FSAList, DERProgramList,
  DERControlList, DefaultDERControls) · P40 MAY subscribe more
- P41 Mirror Metering for metrology · P42 SHOULD post per postRate · P43 Table 12
  nameplate/settings · P44 Table 13 status · P45 Table 14 alarms
- P46–P51 aggregator SHOULD-subscriptions (EndDeviceList, EndDevice instances,
  FSA lists, DERControlLists, DERPrograms, DERProgramLists)
- P52 renew subscriptions ~24 h; fall back to polling on comms errors

## Not applicable

- **Utility-server-side requirements** (most of §6.1: server registration
  handling, ACL enforcement, DERProgram/FSA construction, per-client list
  filtering) — LEXA is client-side; included above only where they define the
  behavior the client must expect (403/404, 201+Location, filtered lists).
- **opModExpLimW / opModGenLimW / opModImpLimW / opModLoadLimW** — not in CSIP
  v2.1 (CSIP-AUS/2030.5 later-profile controls); the CSIP-required real-power
  levers are opModMaxLimW, opModFixedW, opModTargetW, opModEnergize.
- **Aggregator-only SHALLs** (subscription/notification G16/P31–P33/P39,
  RSA cipher P10, per-DER EndDevice fleet handling, in-band EndDevice
  add/delete on behalf of a fleet) — inapplicable while LEXA registers as a
  single GFEMS-style EndDevice at one PCC; they become mandatory if LEXA is ever
  deployed as a Scenario-2 aggregator across multiple PCCs.
- **Rule 21 tariff/Interconnection-Handbook specifics** (which autonomous
  functions are active at deployment, reporting frequencies, cert profiles,
  registration paperwork) — explicitly delegated by CSIP to each utility;
  cannot be audited from these documents.
- **EPRI implementation internals** (build system, porting layer, epoll queue,
  OpenSSL specifics, C API) — informative only; no normative force.
- **Aggregator↔DER southbound links** (SMCU↔DER, GFEMS↔DER, Aggregator↔DER) —
  out of CSIP scope (§3.1); LEXA's Modbus/OCPP legs are unconstrained by CSIP
  except via G2's blanket "security SHOULD be used".
- Demand Response/Load Control, Pricing, Messaging, Prepayment, Billing, File
  Download function sets — present in 2030.5 but not required by CSIP (Table 7
  omits them).
