# Digest: SunSpec IEEE 2030.5 V2G-AC Profile v1.0

Source: `/tmp/claude-1000/-home-dmitri-projects-lexa-hub/3fe1bb9d-5823-4092-ad8c-2497484ef88c/scratchpad/standards/txt/SunSpec-IEEE-2030.5-V2G-AC-Profile-v1.0.txt` (66 pp / 3044 lines).

## Document identity

- **Title**: "IEEE 2030.5 V2G-AC Profile — Implementation Guide for SAE J3072." SunSpec Alliance specification, **v1.0, status Approved 2025-09-15** (v1.0 TEST 2022-06-27; draft 0.7 2022-01-18). Once Approved, normative requirements are frozen.
- **Purpose**: an IEEE 2030.5-compliant profile for implementing **SAE J3072-2021** — the interconnection standard under which an EVSE grants a PEV's *onboard* inverter permission to discharge to the grid. Applies to **SAE J3072 System Type A1** only: SAE J1772 **AC Level 2**, point-to-point **PLC over the J1772 control pilot** (SAE J2931/4), IEEE 2030.5 at the application layer (§1, §1.1).
- **Supersedes SAE J2847/3** (the old "PEV as distributed energy source" comms recommended practice) (§1.1).
- It is more than a subset-profile: it *adds* normative requirements not present in J3072, IEEE 2030.5, or IEEE 1547 (§2.1). Guiding principles: complete profile, eliminate optionality, minimal spec, and **"strictly focus on EVSE-to-PEV communications — all other communications are out of scope"** (§2).

## Actor/topology model (where a hub fits)

- **The EVSE is the IEEE 2030.5 SERVER; the PEV (vehicle) is the IEEE 2030.5 CLIENT** (§3.4, §4.1.1). This inverts the usual utility-facing arrangement: the wall box hosts DeviceCapability/EndDevice/DERProgram resources; the car does discovery, GETs controls, and PUTs its DER data. "The EVSE MAY operate as an IEEE 2030.5 Client, but this functionality is beyond the scope of this profile" (§4.1.1) — the only nod to a northbound 2030.5 path.
- **The DER is the PEV's onboard inverter** (mobile storage DER). The EVSE is a *gatekeeper/authorizer*, not the inverter: it verifies the PEV's identity/certification/settings, grants or revokes authorization to discharge (`opModEnergize`), monitors compliance with its own metering hardware, and physically opens the contactor on violation (§3.4.5.1, §4.8.1.2).
- **Upstream "DME" (DER Managing Entity)**: the profile explicitly anticipates a hub above the EVSE — "The EVSE may be sent a control from an external DME. The EVSE will relay this control and related event parameters to the connected and approved PEV" (§3.4.5.2); PEV metrology mirrored to the EVSE "may be useful for upstream DMEs" (§3.4.3.3). **But the DME↔EVSE interface is entirely out of scope** — no protocol is named (not OCPP, not 2030.5-northbound).
- **Where LEXA sits**: LEXA is the DME. The J3072 link itself is physically PLC-on-control-pilot — a hub can never terminate it; only the EVSE can. A DERMS fronting V2G-AC EVSEs must (a) buy/require EVSEs implementing this profile (2030.5 *server* toward the car), (b) relay grid controls to the EVSE over some other channel (OCPP 2.0.1 is LEXA's natural one; unspecified here), and (c) aggregate the PEV DER data the EVSE collects (nameplate, settings, SoC, availability, energy need) northbound. The profile provides **no gateway/aggregation spec** for step (b)/(c). Note LEXA today is a 2030.5 *client* northbound; nothing here requires LEXA to become a 2030.5 server unless it wants to speak 2030.5 down to EVSE-as-client (the out-of-scope MAY).
- **Multi-port EVSE**: each port is a separate point-to-point PLC link / network interface (§3.3). Port-level re-apportioning of the site limit uses `DERControl:opModMaxLimW` against the first PEV when a second plugs in (§4.6.1.1).

## Profiled function sets & resources

Function sets (Table 2, §4.1.1) — near-total elimination of optionality:

| Function set | EVSE (2030.5 Server) | PEV (2030.5 Client) |
|---|---|---|
| DeviceCapability | MUST | MUST |
| Time | MUST | MUST |
| SelfDevice:DER (site limits) | MUST | MUST |
| EndDevice:DeviceInformation | MUST | MUST |
| EndDevice:PowerStatus:**PEVInfo** | MUST | MUST |
| EndDevice:DER (Capability/Settings/Availability/Status) | MUST | MUST |
| EndDevice:FunctionSetAssignments | MUST | MUST |
| MirrorUsagePoint (PEV metrology) | MUST | MUST |
| DER (Programs) | MUST | MUST |
| Response | MUST | MUST |
| Subscription/Notification | MUST | MAY (else poll) |
| LogEvent | optional | optional (§3.4.3.4) |
| Registration | **prohibited** (EndDevice SHALL NOT carry RegistrationLink, §4.5.3) | — |
| FlowReservation | **not profiled — absent entirely** | — |

- **Session-static resources** (Table 3, §4.1.2): DeviceCapability + top-level links, SelfDevice tree, EndDevice tree, MirrorUsagePoint URIs SHALL stay fixed for the charge session (an allowed 2030.5 optimization made mandatory).
- **Subscribable resources** (Table 4, §4.1.4): DERControlList, DefaultDERControl, FunctionSetAssignmentsList, DERProgramList. If not subscribed, PEV MUST poll at the server pollRate; both sides MUST support pollRate down to **1 s**; `DERProgramList:pollRate` SHALL be 1 s and unchangeable.
- **No public DERPrograms**: DeviceCapability SHALL NOT carry DERProgramListLink; the PEV finds its program only via its EndDevice's FSA (§4.5.1, §4.5.3.5). Exactly one FSA / one DERProgram / one DER instance is the expected shape (§4.5.3.4–.5, §5 examples).
- **Networking** (§4.2–4.3): IPv6-only, ULA fd00::/8 via SLAAC, EVSE advertises prefix via Router Advertisements; mDNS/DNS-SD (`_smartenergy._tcp.local`, ff02::fb) for discovery; EVSE SHALL prohibit bridging/routing at initial connect (EVSE is the only server visible), MAY bridge non-J3072 traffic post-authorization (§4.2.4).
- **Security** (§4.1.3, §4.4): both sides carry IEEE 2030.5 device certs; HTTPS only, HTTP prohibited; mutual TLS; EVSE MAY authenticate PEV LFDI/SFDI against an allow list and MAY use the PEV make/model, which SHALL be encoded as a manufacturer OID in the cert's `HardwareModuleName.hwType` (unique OID per vehicle model). **MITM risk on the PLC link is analyzed and deliberately accepted, not mitigated** (Registration-PIN mitigation considered and rejected, §4.1.3.1).

## Control modes for charge/discharge

Required Management Information functions (Table 5, §4.7.1) — the EVSE is the ONLY server the PEV may accept these from; each function locked to one control type; one opMod function per DERControl:

| Function | 2030.5 control | Type |
|---|---|---|
| Constant Power Factor | opModFixedPFInjectW | DERControl |
| Volt-Var / Watt-Var / Constant Var | opModVoltVar / opModWattVar / opModFixedVar | DERControl |
| Volt-Watt | opModVoltWatt | DERControl |
| HF/LF Trip | opModHFRTMustTrip / opModLFRTMustTrip | DERControl |
| HV/LV Trip + Momentary Cessation | opModHVRTMustTrip, opModHVRTMomentaryCessation, opModLVRTMustTrip, opModLVRTMomentaryCessation | DERControl |
| Frequency Droop (hi+lo) | opModFreqDroop | DERControl |
| **Limit Active Power** | **opModMaxLimW** | DERControl |
| Enter Service | setESDelay/HighFreq/HighVolt/LowFreq/LowVolt/RampTms/RandomDelay | DefaultDERControl |
| **Authorization to discharge** | **opModEnergize** (maps to IEEE 1547 "Permit Service") | DefaultDERControl (§4.6.4) |
| **Charge/discharge dispatch** | **opModFixedW** — positive = PEV discharging, negative = charging | DERControl (§4.8.2) |

- **The V2G dispatch lever is `DERControl:opModFixedW`** (coordinated charge/discharge, SAE J2836/3 Use Case U6, mandated by J3072): both sides MUST support it; sign is DER reference frame (discharge positive). There is **no opModTargetW, no storage-specific opModes, no FlowReservation** anywhere in the profile — a single active-power setpoint plus opModMaxLimW cap is the entire dispatch surface.
- MI DERControls: start = now, duration = 4294967295 (indefinite), responseRequired bits 0+1 (message received + event response); mid-session changes via a superseding DERControl with newer start (§4.6.3).
- **Absence ⇒ disabled** (§4.9.1): unlike base 2030.5 (falls back to device default), if no control is in effect for a function the PEV MUST disable that function. EVSE SHALL send ALL MI in effect at connect (no "assume IEEE 1547 defaults") and SHALL keep the set mutually compatible; reactive-power controls (const PF / volt-var / watt-var / const var) are mutually exclusive and the EVSE SHALL NOT enable more than one (§4.9.7).
- Site limit = `SelfDevice:DER:DERSettings:setMaxW` (**WMax, not VAMax**, per IEEE 1547 priority, §4.6.1.1/§4.9.6); nominal voltage via setVRef, never setVNom (§4.9.5). J1772 PWM ampacity remains a separate, senior electrical limit outside this profile (§4.6.1.1 note).
- Overrides of parent standards (§4.9): momentary cessation SHALL be supported (J3072 said no; IEEE 1547 wins); frequency droop default ON with 1547 defaults (J3072 said OFF).

## EV session lifecycle representation

1. **Plug-in**: PLC link up (SLAC associates PEV to the right EVSE) → SLAAC IPv6 → mDNS finds dcap → mutual-TLS (LFDI/allow-list + cert make/model checks) → resource discovery (EndDevice instance for this PEV; failure to find it = authorization failed, revert to plain charging) (§3.4.1–3.4.3, §4.5).
2. **Three-step info exchange** (§4.6, strict order 1→2): (1) PEV GETs site limits (SelfDevice:DER:DERSettings, J3072 Table C2); (2) PEV PUTs its site-adjusted **DERSettings** (MUST include `setMaxWh` — the SoC%-reference, §4.9.4), **DeviceInformation** (J3072 certification status/date, **VIN** in mfSerNum, **Inverter System Model (ISM) number**), nameplate **DERCapability** (incl. 2Q/4Q, normal/abnormal ratings categories per 1547), **DERAvailability**, **PEVInfo**; (3) PEV GETs + applies ALL Management Information, posting Event-Received and Event-Started Responses per control.
3. **Authorization** (§4.6.4): EVSE MUST verify — PEV certified; 2-/4-quadrant matches site; ISM on approved list; reported config within site limits; all responses received — then sets `DefaultDERControl:opModEnergize=true`. Authorization ≠ discharge command; start/stop dispatch is outside J3072 scope. One-shot: a denied PEV does NOT retry, it charges normally while monitoring opModEnergize at 1 s (poll or subscription).
4. **Arrival/energy-need/departure signaling** — informational only, via **PEVInfo** (PowerStatus): `energyRequestNow` (Wh), `targetStateOfCharge` (%), `timeChargeIsNeeded` (unix time = departure proxy), `minimumChargingDuration`, `chargingPowerNow`, `maxForwardPower`, `timeChargingStatusPEV`; plus **DERAvailability**: `availabilityDuration` (max-discharge duration), `maxChargeDuration`, reserve %s. **SoC** = `DERStatus:stateOfChargeStatus` (hundredths of %, referenced to setMaxWh). Posted as fast as 1 s capability, default postRate 15 s (Table 6, §4.7.2). No scheduling/reservation function consumes these — the EVSE/DME decides what to do with them.
5. **Steady state**: 1 s `DERStatus` heartbeat (irrespective of postRate); metrology (W/var/V/Hz MirrorMeterReadings, DER reference frame, roleFlags isPEV) at postRate; PEV polls EndDevice/MUP for postRate changes (§4.7.2, §4.8.1.1).
6. **Loss of comms** (§4.8.1.1): EVSE misses 10 consecutive heartbeats → revoke opModEnergize; 3 consecutive received → re-authorize if criteria still hold. Symmetric on the PEV: 10 failed sends → cease discharge; 3 successes → may resume if still authorized.
7. **Violation** (§3.4.5.1, §4.8.1.2): out-of-limit discharge → EVSE removes authorization within 1 s; PEV must cease within 3 s of revocation; discharging while unauthorized → EVSE opens the contactor.
8. **Sleep/wake** (§4.8.3): PEV posts `inverterStatus=2 (sleeping)` before sleep (no metrology while asleep) → EVSE revokes; on wake PEV re-acquires ALL MI, posts `inverterStatus=0` → EVSE may re-authorize. inverterStatus overloading: 0="N/A"=awake+authorized; 3="starting up/on but not producing"=awake, NOT authorized.
9. **Plug-out/departure**: represented only by physical link loss (heartbeat death → revocation). No explicit 2030.5 "departing" message.

## Dependencies on other standards

- **SAE J3072-2021** — the parent: defines WHAT must be exchanged (its Tables 2, 3, 14, 17 and mapping tables C2–C9, C19; sections 4.6.x/4.7.x are incorporated by reference for field lists); this profile defines HOW in 2030.5. J3072 in turn requires **IEEE 1547-2018 / 1547.1-2020** compliance (grid-support functions, Permit Service, normal/abnormal performance categories); profile is the comms layer over those device standards (§4 note).
- **UL 1741 (SC EVSE)**: the EVSE product-safety/certification standard this profile is used with (§2.2). **UL 9741 is not referenced.**
- **SAE J1772** (coupler, control pilot, PWM ampacity), **J2931/1 & /4** (PLC comms), **J2836/3** (Use Case U6 coordinated charge/discharge), **J2847/3** (superseded), **IEEE 802.3**, RFC 6762 mDNS.
- **ISO 15118: not referenced at all** — this is the North-American AC path (J3072/J1772 PLC), disjoint from ISO 15118-20 BPT V2G. **OCPP: not referenced** — the CSMS/DME side is fully out of scope.
- Delegated/out-of-scope: DME↔EVSE protocol; ISM approved-model database provisioning; LFDI allow-list provisioning; Enter Service parameter sourcing (utility); dispatch policy; EVSE-northbound anything.

## Numbered requirement inventory (cite sections)

Actors & platform:
1. EVSE MUST be a 2030.5 Server; PEV MUST be a 2030.5 Client (reverse roles out of scope) — §4.1.1.
2. Both MUST implement Table 2 function sets (all MUST/MUST; Sub/Notif PEV-MAY) — §4.1.1.
3. IPv6-only, ULA/SLAAC, EVSE RAs; mDNS `_smartenergy._tcp.local` — §4.2.3, §4.3.
4. EVSE SHALL prohibit bridging/routing at initial connect; MAY enable post-authorization — §4.2.4.
5. HTTPS only; both carry 2030.5 certs; PEV make/model as HardwareModuleName hwType OID (unique per model) — §4.1.3, §4.4.
6. TLS/auth failure ⇒ terminate session; PEV reverts to non-J3072 mode — §4.5.2.
7. Table 3 resources SHALL be session-static — §4.1.2.
8. Table 4 resources MUST be subscribable; 1 s pollRate capability both sides; DERProgramList pollRate locked at 1 s — §4.1.4.
9. No DERProgramListLink in DeviceCapability (no public programs); FSA-only program discovery — §4.5.1.
10. EndDevice SHALL NOT carry RegistrationLink; SHALL carry DeviceInformation/PowerStatus:PEVInfo/Subscription/DERList(Capability, Settings, Availability, Status)/FSA links — §4.5.3.
11. PEV not required to populate functionsImplemented, gpsLocation, pollRate in DeviceInformation (J3072 C4 correction) — §4.5.3.1.

Info exchange & authorization:
12. Step order: site limits before PEV config — §4.6.
13. EVSE MUST publish all J3072-C2 site limits in SelfDevice:DER:DERSettings; PEV MUST get them — §4.6.1.
14. Active-power site limit via setMaxW (WMax), never VAMax; setVRef not setVNom — §4.6.1.1, §4.9.5–4.9.6.
15. EVSE MAY dynamically reduce a PEV via DERControl:opModMaxLimW (does not move %setMaxW reference) — §4.6.1.1.
16. PEV MUST PUT DERSettings (C3, + setMaxWh mandatory §4.9.4), DeviceInformation (C4: cert status, VIN, ISM), DERCapability (C5), DERAvailability (C9), PEVInfo (C9) — §4.6.2.
17. EVSE SHALL provide ALL in-effect MI at connect (no default-assumption), mutually compatible set — §4.6.3, §4.9.1.
18. One opMod function per DERControl; responseRequired bits 0+1; start=now; duration=0xFFFFFFFF; supersede-by-newer-start for mid-session changes — §4.6.3.
19. Authorization preconditions (certified, 2Q/4Q match, ISM approved, within site limits, all Received+Started responses) — §4.6.4.
20. Grant/revoke = DefaultDERControl:opModEnergize true/false = IEEE 1547 Permit Service — §4.6.4.
21. Enter Service params (setES*) delivered via DefaultDERControl; EVSE MAY shorten ES delay if grid already nominal — §4.5.3.5, §4.6.5.

Operations:
22. Table 5 MI functions with fixed DERControl-vs-DefaultDERControl typing; EVSE is sole MI server; PEV MUST NOT accept MI from another server — §4.7.1.
23. Absence of a control ⇒ PEV MUST disable that function (overrides 2030.5 device-default fallback) — §4.9.1.
24. Momentary cessation SHALL be supported (overrides J3072); freq-droop default ON (overrides J3072) — §4.9.2–4.9.3.
25. EVSE SHALL NOT enable >1 reactive-power control — §4.9.7.
26. opModFixedW MUST be supported by both once authorized; +discharge/−charge; extra monitoring per J3072 4.7.4/T17/C19 — §4.8.2.
27. inverterStatus semantics 0/2/3 = authorized / sleeping / awake-not-authorized — §4.7.2.
28. Table 6 monitoring set (W, var, V, Hz MMRs; DERStatus op/conn/alarm/SoC; PEVInfo energy-need block; DERAvailability durations), timestamps mandatory, DER reference frame (export positive), MUP roleFlags isPEV only — §4.7.2.
29. postRate/pollRate defaults 15 s; PEV capable of 1 s posting for PEVInfo/DERAvailability — §4.7.2.
30. 1 s DERStatus heartbeat regardless of postRate; 10-miss revoke / 3-receive restore (EVSE); 10-fail cease / 3-success resume (PEV) — §4.8.1.1.
31. Contactor opens on unauthorized or out-of-limit discharge; revocation-to-cease ≤3 s; out-of-limit detection-to-revoke ≤1 s — §4.8.1.2, §3.4.5.1.
32. Sleep: post inverterStatus=2, silence, EVSE revokes; wake: re-acquire ALL MI, post 0, EVSE may re-authorize — §4.8.3.

## Conformance levels / PICS

**None.** No PICS proforma, no tiers/levels — deliberate ("eliminate optionality," §2). One flat normative section (§4) using MUST/SHALL / SHOULD / MAY conventions. §5 is informative wire examples only. The 2022 TEST note flagged it "not suitable for interoperability certification"; the 2025 Approved status clears that, but no certification program is defined in-document.

## Gaps the profile leaves open

1. **DME↔EVSE interface unspecified** — the exact seam where a DERMS hub (LEXA) lives. EVSE "relays" DME controls to the PEV, but over what northbound protocol (OCPP? 2030.5-client? proprietary) is out of scope. Integrating V2G-AC means specifying this yourself; OCPP 2.0.1 has no native carriage of J3072/1547 MI objects.
2. **No dispatch/scheduling semantics**: opModEnergize is permission, not a command; who computes opModFixedW, when, and against what objective (departure time, energy need, tariff) is out of scope. **FlowReservation is not used**; PEVInfo departure/energy fields are informational with no consuming function set.
3. **Provisioning black boxes**: ISM approved-model database, LFDI allow lists, Enter Service parameters ("normally provided by the utility") — all out of band.
4. **Forced authorization restart**: "by a TBD method" (§3.5.1) — literally unresolved in an Approved spec.
5. **PEV behavior undefined** if EVSE erroneously enables >1 reactive-power control (§4.9.7 note).
6. **Sleep/wake** not addressed by J3072 itself; profile's treat-as-disconnect is an interim recommendation pending a J3072 revision (§3.5.3).
7. **MITM accepted, unmitigated**; security of stored data and out-of-band channels to EVSE/PEV out of scope (§3.2, §4.1.3.1).
8. **AC Type A1 only**: DC V2G, ISO 15118-based V2G (incl. 15118-20 BPT), and any non-J1772/PLC coupling are outside this profile.
9. **Aggregation/multi-EVSE-site coordination** beyond the single multi-port opModMaxLimW example is absent; site-level economics/coordination belongs to the (out-of-scope) DME.
10. LogEvent optional; no alarm-forwarding path northbound defined.
