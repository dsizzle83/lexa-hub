# Regulatory / market-requirements digest — UL1741SA·Rule 21, Massachusetts, IEEE 2030.2.1

Sources (all under `scratchpad/standards/txt/`):
- `SunSpecUL1741SARule21ImplemenationGuideD6.txt`
- `Massachusetts Technical Standards R.txt`
- `203021-2019.txt`

Provenance discipline: statements tagged **[doc]** come from the named document; rows tagged
**[context]** are well-established CPUC Rule 21 / SIWG program structure supplied for the audit's
phase framework (the SunSpec guide itself is Phase-1-centric and only *references* Phase 3).

## Document identities

1. **SunSpec UL1741 Supplement SA / Rule 21 Implementation Guide**, SunSpec Alliance Application
   Note, Draft 0.3, 2017-05-08. Companion to UL 1741 SA; maps each UL1741-SA grid-support function
   (SA8–SA15) to SunSpec information models, using CPUC Rule 21 parameter values in examples.
   Written pre-IEEE-1547-2018: it discusses then-*proposed* models 139/140 (momentary cessation),
   145 (extended ramp rates), 146 (P1547 freq-droop), 147 (P1547 volt-watt). Note for the audit:
   the modern IEEE 1547-2018 SunSpec profile has since moved to the 700-series DER models — this
   guide documents the legacy 1xx-model mapping still present in fielded UL1741-SA inverters.
2. **Massachusetts Technical Standards Review Group — Common Technical Standards Manual**
   (accompanies M.D.P.U. No. 1468), as of 2022-12-22. Joint interconnection-requirements reference
   of the three MA EDCs (National Grid, Eversource, Unitil). It is a *protection/interconnection*
   standard, NOT a DER-communications interop standard — no IEEE 2030.5, no DNP3-by-name, no
   cybersecurity clauses, no telemetry cadence anywhere in it.
3. **IEEE Std 2030.2.1-2019** — Guide for Design, Operation, and Maintenance of Battery Energy
   Storage Systems (stationary and mobile) integrated with EPS. A *guide* (should/recommend, not a
   conformance standard). Normative refs: IEEE 1547-2018, IEEE 2030-2011. Defines BESS =
   batteries + PCS + BMS, supervised by a **MIC** system (monitoring, information exchange,
   control) — the architectural slot a DERMS hub occupies.

## Rule 21 phase requirements (table)

| Phase | Functions required | 2030.5/CSIP DERControl (Phase 2/3 path) | SunSpec Modbus mapping (per the guide) | Gateway/aggregator obligation |
|---|---|---|---|---|
| **Phase 1** — autonomous grid-support (UL 1741 SA certification) **[doc for mappings; context for phase framing]** | SA8 anti-islanding; SA9 L/HVRT; SA10 L/HFRT; SA11 normal ramp rate + soft-start ramp rate; SA12 fixed power factor (SPF); SA13 volt-var; SA14 freq-watt (optional in UL1741 SA); SA15 volt-watt (optional) | n/a — autonomous, in-inverter; DEFAULT curves/settings, no comms needed | SA8: none (no SunSpec model applies). SA9: model 129 LV-trip, 130 HV-trip, proposed 139/140 LV/HV momentary-cessation curves. SA10: 135 LF-trip, 136 HF-trip. SA11: WGra (121) or AGra + Nom/Emg/ConnRmpUp/DnRte (145). SA12: model 123 `OutPFSet` + `_WinTms/_RvrtTms/_RmpTms/_Ena`. SA13: model 126 volt-var curves (fully sufficient). SA14: 127 (parameterized) / 134 (curve). SA15: 132 | None per se; a gateway that *writes* settings must honor the guide's control-enable semantics (below) |
| **Phase 2** — communications **[context]** | Inverter (or facility) must be *capable* of IEEE 2030.5 communication with the utility per the CA SIWG / CSIP Implementation Guide; three compliance paths: (a) direct inverter client, (b) cloud aggregator, (c) **on-site gateway/facility EMS presenting one 2030.5 client for the site — the LEXA architecture** | CSIP: mTLS client per §5.2.1.1 (ECDHE-ECDSA-AES128-CCM-8), DCAP discovery walk, EndDevice registration, DERCapability/DERSettings/DERStatus reporting, MUP telemetry, DefaultDERControl + DERControl event consumption, Response/CannotComply posting | Gateway translates 2030.5 controls to whatever the DER speaks — for SunSpec inverters, the Phase-1 model set above | Gateway IS the compliance unit: presents the site's DERs as EndDevices, enforces controls, reports status/telemetry, posts responses. CSIP conformance testing applies to the gateway |
| **Phase 3** — advanced functions **[context for list; doc confirms FW/VW gap]** | 1. Monitor key DER data; 2. remote disconnect/reconnect; 3. limit maximum real power; 4. set real power; 5. frequency-watt (P1547 freq-droop); 6. volt-watt; 7. dynamic reactive-current support; 8. scheduling of power values and modes | opModConnect / opModEnergize; opModMaxLimW; opModFixedW; opModFreqDroop (+FreqWatt curve); opModVoltWatt; opModVoltVar; DERControls with start/duration = scheduling; monitoring via MUP + DERStatus | **[doc]** P1547-style freq-droop is NOT fully supported by 127/134 (127 lacks under-frequency + open-loop response time; behavior snapshots pre-disturbance power) → proposed model 146. P1547 volt-watt not fully supported by 132 (no %WPreV pre-disturbance power reference, no sub-second open-loop TResp) → proposed model 147 (DeptRef 3=%WPreV, TResp/TResp_SF) | Gateway must map each DERControl base onto device writes and verify effect; where legacy inverters lack 146/147, gateway can only approximate P1547 FW/VW — a per-device capability matter to record |

**Rule 21 Phase 1 setpoints the guide fixes [doc]** (these are the utility-default curves a gateway
should expect as DefaultDERControl-equivalents):
- **HVRT/LVRT**: HV2 V≥120%: trip 0.16 s · HV1 110–120%: momentary cessation, ride-through 12 s,
  max trip 13 s · NN 88–110%: continuous · LV1 70–88%: mandatory operation 20 s, trip 21 s ·
  LV2 50–70%: mandatory 10 s, trip 11 s · LV3 <50%: momentary cessation 1 s, trip 1.5 s.
  Curve hierarchy: Trip > Momentary Cessation (crossing a higher-precedence curve wins).
- **HFRT/LFRT**: HF2 f>62 Hz: trip 0.16 s (adjustable 62.0–64.0) · HF1 60.5–62: mandatory
  ride-through 299 s, trip 300 s (adj. 60.1–62.0) · NN 58.5–60.5: continuous · LF1 57–58.5:
  mandatory 299 s, trip 300 s (adj. 57.0–59.9) · LF2 f<57: trip 0.16 s (adj. 53.0–57.0).

**SunSpec control-enable semantics [doc — binding on any Modbus-writing client, i.e. lexa-modbus]**:
- The enable point MUST be rewritten after ANY value change to a control, even if already enabled;
  new values take effect only on the enable write (multi-register atomicity mechanism).
- A read after writing returns the LAST-WRITTEN values, which can HIDE the currently-active values
  while a control is being staged — i.e., register readback of settings is not proof of active
  behavior. (Directly validates the hub's "trust measurement, not the command" invariant.)

## Massachusetts requirements (numbered)

All from the TSRG Common Technical Standards Manual [doc]. "Joint Utilities" = NG + Eversource + Unitil.

1. **Anti-islanding**: DER must trip within the 2-second limit prescribed by IEEE 1547 on island
   formation. Screening: line-section aggregate DER ≤33% of minimum load ⇒ negligible risk, no
   further screening; above that, Sandia screens (SAND2012-1365, positive-feedback SFS/SVS methods
   only), then dynamic ROI study, else utility-owned SCADA recloser at PCC ± reclose blocking ± DTT.
   National Grid: certified-DER screens require all inverters to have an **88% voltage trip within
   2 s**; utility recloser required at ≥300 kW aggregate where DER >33% min load on <5 kV EPS;
   rotating machines and *voltage-regulating inverters* ≥33% min-load require DTT.
2. **Direct Transfer Trip**: utility-signal (or loss-of-signal) driven trip of customer breakers;
   the definitive anti-islanding means. Comm medium installed and paid for by the customer.
3. **Remote Monitoring and Control (RMAC)** — the state's only DER "communications" requirement,
   and it is utility-SCADA, not DERMS interop: RTU or direct measurement-device comms + control of
   an interrupting device. Thresholds: Eversource recloser possible >500 kW; National Grid non-IPP
   RTU at ≥500 kW (≤5 kV) / ≥1,000 kW (5–15 kV) / ≥1,800 kW (15–69 kV), may require RMAC ≥250 kW;
   Unitil RMAC for all facilities ≥500 kVA **plus real-time monitoring of each individual unit
   ≥500 kVA**. Sites with both load and generation may need an RTU in addition to the recloser so
   each is monitored/controlled separately.
4. **RMAC data points** (typical, non-exhaustive): net kW, net kVA, net kVAR, per-phase current
   magnitudes, phase-to-phase voltages (all phases), frequency, facility breaker status, fault
   targets, PCC main/interconnect breaker status, individual generator breaker status, control
   input for the designated generator interrupting device (trip / block-close / permit-close),
   protective relay status, DC control system status. **No telemetry cadence is specified.**
5. **RMAC communications media**: National Grid — leased MPLS line; Eversource — MDS radio;
   Unitil — case-by-case. Customer bears install + maintenance cost. (No protocol named; this is
   conventional utility SCADA/RTU territory — IEEE 2030.5 appears nowhere in the manual.)
6. **Remote operation**: where RMAC/recloser required, remote tripping of the site must be enabled.
7. **PCC protection**: utility-owned recloser at ≥500 kW (≤5 kV) / ≥1,000 kW (≥15 kV). External,
   visible-break, lockable, 24/7-accessible disconnect: Eversource all sizes, NG ≥25 kW (waivable
   <25 kW for UL 1741 listed inverters with integrated NEC-compliant disconnect), Unitil >10 kW.
8. **Witness testing**: required for all facilities (waivable for simplified/expedited UL 1741
   certified); must include actual breaker trips; functions tested include anti-islanding,
   **non-export function**, synchronizing controls, proof of inability to energize de-energized
   lines, and the **5-minute reconnect time**. Relay points tested: 27, 59, 81O, 81U, 51/51N/51C,
   59N, breaker failure. Set-points per utility requirements and IEEE 1547; under-frequency
   ride-through modified per the **NPCC A.03** curve (ISO-NE may update).
9. **Utility-grade / redundant relaying**: redundant utility-grade relaying required ≥500 kW
   (possible ≥250 kW); utility-grade (ANSI C37.90) relaying required for all synchronous and/or
   non-UL1741-certified generators.
10. **Power factor**: all facilities hold a predetermined static PF, typically unity at generator
    terminals; active voltage regulation only case-by-case (and may trigger DTT).
11. **ESS export limiting (§16.3)**: a power-export-limit scheme requires a **utility-grade ANSI
    C37.90 protective relay** with sensing at the PCC; settings must use worst-case summed
    accuracy (RMS error combination explicitly insufficient). Exemption: Class I resources
    (≤60 kW) on radial feeders. Export limits count for thermal/voltage/flicker load-flow analyses
    but **full inverter nameplate is still used for fault and risk-of-islanding analyses**.
12. **ESS charging method (§16.4)**: grid-charging vs local-DG-only charging must be declared
    before study; DG-only designs must explicitly show the means preventing utility charging;
    grid-charging may be evaluated as a new-load request.
13. **ESS scheduling (§16.6)**: studies run Scheduled (EDC-standardized charge/discharge schedule
    only — unique schedules NOT permitted) vs Unscheduled (full capacity any time) scenarios; the
    customer's elected path becomes binding. **A change to the BESS charge/discharge schedule or
    charging method/source is a "Significant Change"** (§17) — grounds for re-study/withdrawal.
14. **Other**: balanced three-phase output required (no per-phase reverse/forward split through
    substation transformers, IEEE C57.91 aging analysis); GSU winding configs per Table 4;
    >2% voltage flicker in Expedited Review escalates to full SIS; minimum feeder load defaults to
    25% of previous-12-month peak when unmeasured.
15. **Standards incorporated by reference**: IEEE 1547 (trip times, relay set points), UL 1741
    (certified inverters), ANSI C37.90 (utility-grade relays incl. export-limit control), NPCC
    A.03 (UFRT curve), Sandia SAND2012-1365 (islanding screens), IEEE C57.91 (transformer
    insulation aging), NEC. **Not referenced: IEEE 2030.5, CSIP, DNP3, IEC 61850, any
    cybersecurity standard.**

## IEEE 2030.2.1 BESS interface guidance

What the guide recommends for the MIC layer — i.e., the checklist for a DERMS hub acting as (or
talking to) a BESS supervisory system. All [doc]; guide-level "should" unless noted.

1. **Protocol stack (5.5.1)**: **IEC 61850 or DNP 3.0 between EPS (utility/dispatch) and BESS**;
   **Modbus TCP/IP between BMS/PCS and the MIC system** (restated in 5.4.1.2 and 5.4.2); and
   "requirements of IEEE Std 2030.5 is recommended to be considered to facilitate the
   implementation of DR related requirements of IEEE Std 1547." — a 2030.5-northbound +
   Modbus-southbound hub matches the recommended split for DR-interconnected storage.
2. **MIC basic functions**: monitoring (operational params, equipment/communication status,
   alarms/faults); information exchange (operating params, switching info, alarms, protective
   action signals, control info); control — **stratified and graded by priority; on communication
   failure the control layer should automatically transfer to the adjacent higher priority**
   (fail-over hierarchy, not fail-open).
3. **Status to the utility**: BESS sends ON/OFF operating status to the EPS; the Area EPS operator
   may request status exchange + the ability to **directly trip the BESS breaker** where the BESS
   might keep operating after a feeder device opens (permissive for reclose etc.).
4. **Monitoring design (5.5.2.1)**: acquire analog + status quantities at PCC/interconnection
   points; data processing with out-of-limit alarming and **rationality (plausibility) checks**;
   data storage per configuration; **event-sequence recording and accident recall**; statistics of
   charge/discharge counts and energy per interval; **clock synchronization** — accept satellite
   time and synchronize PCS/BMS/protection clocks via messages.
5. **Information-exchange design**: alarm levels/queries/statistics/confirm-clear/storage; HMI;
   web publish under access control; interconnection with external systems (distribution
   management, automatic dispatch, marketing) exchanging charge/discharge power, energy, status.
6. **Control design**: open/close PCC and sub-system switches; control PCS operating status;
   regulate step-up transformer taps; command at least one 4.4 function: frequency regulation,
   voltage regulation, emergency back-up/black start, capacity optimization, **electric energy
   time-shift** (price-driven charge/discharge — the hub's optimizer function, named verbatim in
   6.1.5), power-quality/service-reliability improvement.
7. **Function-operation rules (6.1)**: BESS should **automatically exit** frequency/voltage
   regulation once the grid recovers; multiple BESSs regulating simultaneously need coordinated
   control to avoid oscillation; SCADA-allocated setpoints split by power ratio/available energy;
   response times (including communication time) per grid codes. P/Q mode for freq-reg, time-shift,
   capacity; V/f for black start/voltage support.
8. **BMS expectations (5.4.2)**: monitor voltage, current, temperature, **SOC and SOH** of cells/
   modules/system; warn on accident; protect OV/UV, overcurrent, low/over-temperature, dc
   insulation; balancing; functional safety per IEC 61508 / IEC 60730-1; Modbus TCP/IP upward.
9. **PCS expectations (5.4.1)**: continuous P and Q regulation; on-grid + off-grid capability;
   monitor connection status, active/reactive output, V and f at the interconnection point;
   protection per Table 1 (dc/ac OV/UV, OC, frequency abnormality, cooling failure, **communication
   failure**); power quality: harmonics per IEEE 519, dc injection ≤0.5% rated current, voltage
   fluctuation ≤±5%, negative-sequence unbalance ≤2% (4% short-time); LVRT when paired with
   wind/PV.
10. **Abnormal-condition handling (6.3)**: **communication failure/interruption** and PCS
    charge/discharge enable-signal failure are listed stop-and-inspect conditions; parameters out
    of normal range likewise; HVAC/cooling/fire-panel *self-failures* are monitor-don't-stop.
    Operations under EPS failure follow dispatching instructions.
11. **Battery ops relevant to an optimizer (6.2)**: SOC calibration via full discharge/recharge to
    correct BMS SOC accuracy; Li-ion idles best at relatively low SOC; supplementary charge at
    manufacturer-specified intervals in standby; smaller action thresholds / charge-discharge dead
    zones for freq-reg and load-leveling duty.

## Market-requirements matrix

| Jurisdiction / program | Interconnection cert | Comms / DERMS protocol | Required functions (gateway-relevant) | Storage-specific | Cadence / cyber |
|---|---|---|---|---|---|
| **California — CPUC Rule 21** (PG&E, SCE, SDG&E) | UL 1741 SA (now SB/1547-2018) smart inverter | **IEEE 2030.5 per CSIP — mandatory (Phase 2)**; gateway/EMS path explicitly allowed [context] | Phase 1 autonomous set (AI, L/HVRT, L/HFRT, RR/SS, SPF, VV; FW/VW optional) with Rule 21 default curves [doc]; Phase 3 DERControls: connect/energize, MaxLimW, FixedW, FreqDroop, VoltWatt, VoltVar, dynamic reactive current, scheduling, monitoring [context] | Storage participates via same DERControl set; export limits via opModMaxLimW / ExportLimitW | CSIP mTLS (ECDHE-ECDSA-AES128-CCM-8), per-device LFDI/SFDI PKI; poll/post cadences from server-supplied `pollRate`/`postRate` [context] |
| **Hawaii — HECO Rule 14H / SRD** (named in guide) | UL 1741 SA per HECO Source Requirements Document | (Out of these docs' scope) | Guide notes HECO SRD requires **P1547-style freq-watt and volt-watt** — the functions models 127/132 can't fully express (→146/147) [doc] | — | — |
| **Massachusetts — M.D.P.U. 1468 / TSRG manual** (NG, Eversource, Unitil) | IEEE 1547 + UL 1741 listing; ANSI C37.90 utility-grade relaying above thresholds / non-certified DER | **No DERMS interop protocol required.** RMAC = utility-SCADA RTU/recloser above ~250–500 kW thresholds; MPLS (NG) / MDS radio (Eversource); customer-funded. No 2030.5/DNP3/61850 named, no cyber standard, no cadence [doc] | Anti-islanding 2 s trip (+88%-V/2 s NG screen), DTT/reclose blocking per screens, remote trip, unity PF default, NPCC A.03 UFRT, witness-tested non-export + 5-min reconnect | Export limiting needs ANSI C37.90 relay at PCC (≤60 kW radial exempt); declared charging source; EDC-standardized schedules only; schedule/charging-source change = Significant Change re-study [doc] | None specified |
| **BESS anywhere (IEEE 2030.2.1 guidance, non-jurisdictional)** | IEEE 1547-2018 for DR interconnection (normative ref) | IEC 61850 or DNP3 EPS↔BESS; Modbus TCP/IP BMS/PCS↔MIC; **2030.5 recommended for 1547 DR requirements** [doc] | MIC monitoring/info-exchange/control; prioritized control with comm-failure failover; auto-exit grid functions; ON/OFF status + direct-trip hook for EPS operator; clock sync; event recording | SOC/SOH/V/I/T monitoring, SOC calibration, standby-SOC management, charge/discharge statistics; IEEE 519 power quality; functional-safety BMS | No cadence; cyber explicitly out of scope (1.3) but "cyber protection related requirement" nodded to for networked HMI (7.2.1) |

## Implications for the hub

1. **Rule 21/CA is the aligned market**: the hub's 2030.5/CSIP client + SunSpec Modbus southbound
   is exactly the Phase-2 "gateway" compliance path, and Phase-3 DERControl bases map onto the
   existing `lexa/csip/control` → desired-doc pipeline. Audit item: enumerate which DERControl
   bases the hub honors today (opModMaxLimW-equivalent exists; connect/energize, FixedW,
   FreqDroop, VoltWatt, VoltVar, scheduling need a coverage check) and which SunSpec models the
   southbound can write (guide's set: 121/123/126/127/132/134/145; modern 1547-2018 = 700-series).
2. **Control-enable semantics are a Modbus-client correctness rule [doc]**: after changing any
   control value, the enable point must be rewritten or the change never takes effect; and reads
   return last-written (possibly not active) values. The reconciler's verify-by-readback must
   treat *measured effect*, not settings readback, as convergence evidence — which the hub already
   does ("trust measurement, not the command"), but any future settings-write path (curves, PF,
   ramp rates) must include the enable-rewrite step.
3. **Legacy FW/VW gap [doc]**: fielded UL1741-SA inverters may only expose models 127/132, which
   cannot express P1547 freq-droop/volt-watt (no under-frequency, no pre-disturbance power
   reference, no sub-second open-loop response). If the hub relays such controls, per-device
   capability records must distinguish "can approximate" from "can execute" — a CannotComply
   trigger, not a silent best-effort.
4. **Massachusetts does not create a DERMS-protocol obligation** — RMAC is utility-owned SCADA at
   an RTU/recloser, separate from anything the hub does; the hub neither satisfies nor conflicts
   with it. Two real constraints: (a) a hub-enforced *soft* export limit is NOT acceptable as the
   MA export-limiting scheme above 60 kW (requires ANSI C37.90 relay at the PCC — position the
   hub's limit as supervisory, the relay as the compliance device); (b) the optimizer's battery
   schedule is an interconnection-binding artifact in MA — only EDC-standardized schedules are
   permitted for Scheduled sites, and changing schedule or charging source (grid vs PV) is a
   Significant Change. A tariff-driven optimizer that freely reshapes charge/discharge timing
   could put a MA site out of conformity with its ISA — needs a "schedule envelope" constraint
   concept if MA is a target market. Also: 5-minute reconnect and unity-PF defaults are witness-
   tested behaviors the hub must never command against.
5. **IEEE 2030.2.1 endorses the hub's architecture** (2030.5 north for DR, Modbus TCP south to
   PCS/BMS) and supplies a MIC feature checklist to audit against: prioritized control with
   comm-failure failover to the higher-priority layer (cf. AD-007 tiers + fail-closed retained
   controls), auto-exit of grid-support functions on recovery, event-sequence recording, clock
   synchronization of downstream devices, SOC/SOH acquisition + SOC-calibration awareness,
   charge/discharge cycle statistics, and an EPS-operator direct-trip/status hook. Utility-scale
   BESS markets would additionally expect IEC 61850/DNP3 northbound adapters — a portfolio gap to
   note, not a residential-product defect.
6. **Cadence/cyber takeaway**: none of these three documents impose telemetry cadence or
   cybersecurity requirements on a gateway — those come from CSIP (server-set poll/post rates,
   mTLS/PKI) and, for OCPP, its own security profiles; cite those documents for that audit axis.
