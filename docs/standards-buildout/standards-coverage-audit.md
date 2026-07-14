# LEXA Hub — Standards Coverage Audit

Date: 2026-07-14 · Repo: lexa-hub @ d6ac263 (main)
Method: 11 standards-reader agents digested the full ~/Documents/standards library (25 documents, ~2,200 pages);
2 codebase agents built capability inventories with file:line evidence. Digests: `scratchpad/digests/*.md`.

**Library gap disclosure**: the standards library contains NO OCPP documents (neither 1.6 nor 2.0.1/2.1),
no ISO 15118, no DNP3, and no CSIP-AUS. Findings on those are from domain knowledge, not document audit.

---

## 1. Coverage matrix

Legend: ✅ complete · 🟡 partial · ❌ absent. "Cert-required" = mandatory in the SunSpec CSIP
conformance test matrix (v1.3) for the DER Client profile ([C]) or Aggregator profile ([A]).

### 1.1 IEEE 2030.5-2018 + CSIP v2.1 client (primary conformance target)

| Capability | Status | Detail |
|---|---|---|
| TLS 1.2 + TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8, mutual auth | ✅ | wolfSSL pinned exactly; post-handshake verify; matches COMM-003. 2030.5-2023 changes nothing here. |
| LFDI/SFDI derivation (SHA-256 of DER cert; check digit) | ✅ | identity.go; matches §6.3; BASIC-001 self-computation requirement met. |
| Cert chain handling (2/3/4-length, reject bad MICA / self-signed) | 🟡 | Accept-path solid; **reject-paths (COMM-004 D–G) never proven** — needs pcap-grade verification vs the deepest test-PKI chain. |
| Cert expiry monitoring + probe-then-commit rotation | ✅ | Beyond spec (2030.5 certs are indefinite-lifetime, no OCSP/CRL — consistent with SunSpec CPS: renewal unsupported, revocation prohibited). |
| DeviceCapability walk, no hardcoded URIs, link-following | ✅ | walker.go; per-resource pollRate honored (TASK-071). |
| Time function set | 🟡 | currentTime consumed w/ monotonic anchoring (strong); TzOffset/DstOffset unused (delegated to SOM zone + WS-8 — acceptable); quality field unread. |
| EndDevice / FSA / DERProgram primacy | ✅/🟡 | Full for single-EndDevice GFEMS topology. Absolute-primacy interpretation (no cross-program merge) self-flagged vs BASIC-021..026; per-DER EndDevice (aggregator) absent. |
| Registration PIN verification | 🟡 | Helper exists, **never runs in production walk**. Exercised by CORE-003/BASIC-001. |
| DERControl event lifecycle (scalar): schedule, randomize, supersede, cancel, default-fallback, fail-closed | ✅ | scheduler.go; hardening beyond spec (plausibility gate, clock-regression guards, LKG hold). |
| Response posting 1/2/3/6/7 to replyTo per responseRequired | ✅ | responses.Tracker, durable persist. |
| **CannotComply as vendor status 0xF0** | 🟡 | **Not a 2030.5 Table 27 code** — only our gridsim understands it. Standard vocabulary: 252/253 (rejected), 4/8/10 (opt-out/partial), 13/14 (aborted). Real head-ends will drop it. |
| **DERCapability/DERSettings/DERStatus/DERAvailability reporting (PUT)** | ❌ | **tlsclient has no PUT verb at all.** Core CSIP duty (Table 8, G28–G30); cert-required [C] (CORE-009/014, BASIC-028, UTIL-002). Hub never tells the utility its nameplate, settings, status, or SoC. |
| ModesSupported assertion | ❌ | Constants exist, never asserted — pairs with DERCapability PUT. |
| **LogEvent alarm posting (G31/G32, functionSet=11, alarm+RTN pairs)** | ❌ | Cert-required [C] (BASIC-027). Alarm sources exist in-hub (701 Alrm parsed, breach episodes) but no poster. |
| MUP metering | 🟡 | W/V/Hz posted with correct ReadingType discipline + postRate honor. **Missing reactive power (VAr) — one of CSIP's four mandatory quantities** (Table 2). No Wh, no SOC mirror, no per-phase, no reading batching. |
| DERControl modes — enforced live | 🟡 | 5 of ~27: Connect, FixedW, MaxLimW, ExpLimW, ImpLimW. Note **ExpLimW/ImpLimW aren't CSIP v2.1** (they're CSIP-AUS; useful for AUS market, invisible to CA cert). |
| opModEnergize | 🟡 | Parsed, never enforced (Connect is enforced instead). CSIP Table 9 maps Connect/Disconnect to **opModEnergize**; BASIC-009 tests it. SunSpec 703 write support already exists in lexa-proto. |
| opModFixedPF / opModFixedVar / opModTargetW | 🟡 | Parsed + displayed only. FixedPF is CSIP-mandatory (BASIC-008); 704 sync-group write machinery already lib-complete. TargetW/FixedW tests exist but are not profile-required (BASIC-013/014). |
| **Curve modes: opModVoltVar/VoltWatt/FreqWatt/WattPF + FreqDroop + 10 ride-through trip modes** | ❌ (display-only) | Deliberate V1.0 de-scope (AD-010). But **cert-required [C]: BASIC-004/005/006/011/012** and CSIP Table 9 mandates them. Southbound 705–712 write machinery incl. curve-adoption handshake is **already lib-complete — the gap is only the middle plumbing** (bus.ActiveControl fields → desired docs → reconciler execution). |
| Subscription/Notification client | ❌ | MAY for direct/GFEMS client (poll-only certifiable — EPRI precedent); **MUST for Aggregator profile** (CORE-018/019, ERR-002, G16). Requires hosting a TLS notification endpoint. |
| FlowReservation | 🟡 | Read+publish wired; POST machinery wired; **no producer** — dormant. Optional per 2030.5; natural fit for EV energy-need scheduling later. |
| DNS-SD discovery | 🟡 | Package complete, unwired (static server config). COMM-001 is optional — fine. |
| EXI media type | ❌ | XML-only is compliant (client needs XML **or** EXI). |
| HTTP semantics | 🟡 | Hand-rolled HTTP/1.1 with hostile-server caps + chunked decode (fine). **301/302 redirect-following (ERR-001, cert-required) unverified.** No PUT/DELETE verbs (needed for DER* reporting / in-band dereg). |
| Pricing/Billing consume → tariff economics | ✅ | Full walk + retained publish + plan economics. |
| Aggregator profile (per-DER EndDevices, fan-out scoping, subscriptions, in-band add/delete) | ❌ | Not modeled. Only needed if certifying/deploying as Scenario-2 Aggregator rather than GFEMS. |

### 1.2 IEEE 2030.5-2023

❌ Not targeted (deliberate: CSIP v2.1 pins 2018; zero security delta so TLS stack carries over unchanged).
Highest-value 2023 items for this product when market pull arrives: absolute-watt site caps
(opModMaxLimWInject/WAbsorb), ProxiedDevice/AggregatedDevice function sets (sanctioned model for exactly
this gateway), storage-aware curve bases, CurrentDERControls reporting. Real migration cost is the event
engine (per-mode supersession, status 5 Completed, removal-as-cancellation).

### 1.3 IEEE 1547-2018 clause 10 (hub as plant-controller/local DER interface)

1547.2 §10.1.9 explicitly anticipates a single in-plant central controller as the interface for a
multi-component DER — the hub's architecture is the endorsed one, and its 4.7 priority order maps onto
the hub's safety > compliance > economics stack.

| 1547 §10 category | Status | Detail |
|---|---|---|
| Nameplate read (Table 28) | 🟡 | 702/713/121 read; performance categories (NorOpCatRtg/AbnOpCatRtg) + CtrlModes parsed at lib level but not surfaced/used; model 120 declared-unread. |
| Monitoring (Table 29: P, Q, V, f, state, conn, alarms, SoC) | 🟡 | P/V/f/SoC live on bus; **Q (reactive), operational state, alarm status not carried on bus.Measurement** (parsed at device layer, dropped). |
| Management — limit active power / connect-disconnect / set-W | ✅ | The workhorse path (704/123), reconciler-verified, convergence-checked. |
| Management — const PF, volt-var, watt-var, const Q, volt-watt, freq droop, V/f trip curves, enter service | ❌ actuation | Lib-complete (704–712 incl. adoption handshake), zero callers above device wrappers. |
| Protocol requirement (≥1 of 2030.5/DNP3/SunSpec Modbus) | ✅ | Two of three (2030.5 north, SunSpec Modbus south). DNP3 absent — not required. |
| Response times (reads ≤30 s; commands commence ≤30 s) | ✅ | Poll cadences and tick budget are far inside bounds. |

### 1.4 SunSpec Modbus client (Device Info Model v1.4 + Client Conformance v1.1)

| Obligation | Status | Detail |
|---|---|---|
| SunS marker + ID/L chain walk, unknown-model skip, duplicate tolerance | 🟡 | Walk solid; **only base 40000 probed** (spec/CLI-4: probe 0, 40000, 50000). |
| Scale factors, per-type sentinels, plausibility | ✅ | Strong; nameplate×1.2 gate beyond spec. |
| FC3/FC6/FC16, partial-response + TCP-segmentation recovery, exceptions 1–4 | 🟡 | Core paths exist; PROT-1/2 recovery behavior untested against the conformance procedures. |
| Model coverage — read | 🟡 | 1/101-103/201-203/701/702/713/802 live. **Absent: float families 111-113/211-214, meter 204, storage bank 803-805/807; 120/122 declared-unread; meter energy accumulators (TotWhImp/Exp) unparsed** — no revenue-grade energy anywhere. |
| Model coverage — write | 🟡 | 123 (Conn/WMaxLimPct) + 704 (WMaxLimPct/WSet) live; 703/705-712 lib-complete unwired; 702 WMax write no caller. |
| Legacy 12x enable-rewrite rule (Rule 21 guide) vs 7xx immediate-effect | 🟡 | Moot today (no settings writes); becomes load-bearing the day curve/PF writes ship — per-series handling required. |
| Reversion timers (DefaultRvrtTms) as loss-of-comms defense | ❌ | Never set; reassert-on-reconnect covers it, but device-side reversion is the belt to that suspender and 1547.2 explicitly blesses gateway-implemented defaults-on-comms-loss. |
| Secure SunSpec Modbus v1.0 (mTLS port 802, role certs) | ❌ | Brand-new (Dec 2025), nascent deployment. Hub's natural role: GridServiceSunSpec. Crypto reuses CCM-8 — wolfSSL/stack reusable. Watch, don't build yet. |

### 1.5 OCPP

| Item | Status | Detail |
|---|---|---|
| OCPP 2.0.1 core smart charging (B01-ish boot, heartbeat, status, TransactionEvent, MeterValues, K01 SetChargingProfile TxDefault, TriggerMessage) | 🟡 | Minimal-viable and hardened (plausibility gates, reassert-on-reconnect, accept≠convergence). |
| Security Profile 2 (TLS + Basic Auth), enforced fail-closed | ✅ | Product default with explicit bench opt-out. SP3 (mTLS) backlogged. |
| Authorize / local auth lists / pairing gate | ❌ | **Any charger that dials in transacts and becomes plant** (roadmap R8 flag). |
| ClearChargingProfile / GetCompositeSchedule / remote start-stop / Reset / device model / firmware / A05 cert mgmt / K15-K17 charging needs | ❌ | Release = re-set higher limit; no composite verification; no needs-based scheduling. |
| **OCPP 1.6J** | ❌ | No 1.6 anywhere; a 1.6-only charger cannot connect. Market reality: the overwhelming majority of deployed AC chargers still speak 1.6J. (Not in standards library — domain knowledge.) |
| OCPP 2.1 (V2X profiles, published 2025) | ❌ | The standardized OCPP carriage for bidirectional setpoints. |

### 1.6 V2G

❌ Absent end-to-end, and charge-only **by type construction** (EVSECommand.MaxCurrentA ≥ 0; planner has no
EV discharge term; no negative setpoints).

What the library's V2G-AC profile (SAE J3072 / SunSpec 2030.5 V2G-AC v1.0, Approved 2025-09) actually implies:
- Roles invert: **EVSE = 2030.5 server to the PEV; the car's onboard inverter is the DER.** The hub is the
  upstream "DME" the profile explicitly anticipates — but the **DME↔EVSE seam is deliberately unspecified**
  (no OCPP, no 2030.5 named).
- Dispatch surface is tiny: DefaultDERControl:opModEnergize (authorize) + signed **opModFixedW**
  (+discharge/−charge) + opModMaxLimW cap, plus full 1547 MI relay (curves/trips/droop!) and PEVInfo/SoC/
  availability aggregation.
- So V2G-AC for LEXA = (a) J3072-capable EVSE procurement, (b) an EVSE-relay leg (natural carrier: OCPP,
  which today can't carry 1547 MI → OCPP 2.1 V2X or vendor extension), (c) planner discharge term +
  breach/limits treatment of EV as storage DER, (d) northbound: EV-as-DER already fits existing DER function
  set (DERType 81/82; 2030.5-2023 adds nothing V2G-specific). ISO 15118-20 BPT (DC path) is a separate,
  bigger bet; not in the library.

### 1.7 OpenADR

❌ Absent entirely (all of 2.0a/2.0b/3.x). Assessment from digest:
- 3.1 VEN, Continuous-Pricing or Baseline profile: ~3–5 kLOC pure-Go new `lexa-openadr` service; OAuth2
  bearer + REST/JSON; **no wolfSSL involvement**; signals map cleanly onto existing levers
  (PRICE→SetPrices/tariff compile; IMPORT/EXPORT_CAPACITY_LIMIT→existing limit rules + convergence
  backstops; reports←existing bus telemetry; opt-out←breach episodes).
- 2.0b VEN: 15–25 kLOC XML/EI machinery — only if a specific utility program mandates it.
- Relevance: DR-program revenue (e.g., MA ConnectedSolutions-style programs) in territories that don't run CSIP.

### 1.8 Regulatory / market overlays

| Market driver | Status | Detail |
|---|---|---|
| CA Rule 21 Phase 2 comms (CSIP) | 🟡 | See §1.1 — scalar path strong, curve/PF/reporting gaps are exactly the Phase 3 function surface. |
| Rule 21 Phase 3 / UL 1741 SA functions via gateway | 🟡 | Autonomous functions live in the inverter (UL 1741 SA defaults); hub's job is provisioning/verification — the unwired 705-712 path. Legacy-inverter caveat: 127/132 can't express P1547 droop/volt-watt — per-device capability + CannotComply matter. |
| Massachusetts (TSRG) | ✅/n.a. | No DERMS protocol/cyber mandates (utility SCADA above ~250–500 kW). **Two real constraints**: >60 kW export limits need relay-grade enforcement (soft limits don't count); **BESS charge/discharge schedules are interconnection-binding — a free-running tariff optimizer can breach an ISA.** Hub lacks a "schedule-envelope" constraint concept. |
| IEEE 2030.2.1 BESS guidance | 🟡 | Architecture endorsed (2030.5 north / Modbus to BMS-PCS). Checklist gaps: event-sequence recording, comm-failure failover posture per function, auto-exit of grid functions on recovery. |

---

## 2. Strengths beyond spec (keep; they're differentiators)

- Fail-closed control adoption (LKG hold, plausibility gates, clock-regression guards) — exceeds 2030.5/CSIP.
- Trust-measurement convergence + meter-independent floors — validated by SunSpec v1.3's "readback ≠ effect"
  rule and the Rule 21 guide's explicit warning; most competitors trust ACKs.
- Reconciler/retained-desired-doc reassert — precisely the gateway-side compensation 1547.2 §10.4.3-4 anticipates.
- Cert lifecycle ops (monitor + probe-then-commit rotation) in a PKI with no renewal/revocation.
- Crash-only design, watchdogs, tick budget, journald caps — bench-hardened operational posture.

## 3. Questioned non-coverage (candidate roadmap, pre-expert-review)

**Certification blockers for SunSpec CSIP DER-Client cert (if that's the goal):**
1. HTTP PUT verb + DERCapability/DERSettings/DERStatus/DERAvailability reporting — no cert without it; also
   how the utility learns SoC/nameplate. Deliberate sequencing, but table stakes.
2. Curve/PF/energize control execution (BASIC-004..012): the expensive half (SunSpec 703-712 writes incl.
   adoption handshake) already exists; missing middle = bus.ActiveControl/desired-doc fields + reconciler
   per-axis convergence semantics + scheduler passthrough. AD-010's de-scope trigger ("cert-lab scope
   referencing curve function sets") fires the moment certification is pursued.
3. LogEvent alarm posting (BASIC-027) — sources exist, poster doesn't.
4. VAr in MUP telemetry (BASIC-029) — meter already parses VAR; plumb + post.
5. Registration PIN verify in production walk (CORE-003) — helper exists; wire it.
6. CannotComply 0xF0 → standard Response codes (252/253/4/8/10/13/14 mapping) — interop bug today.
7. Verify-only items: COMM-004 reject-paths (pcap), ERR-001 redirect following, SunS probe at 0/50000.

**Product-scope questions (for DERMS expert):**
8. OCPP 1.6J southbound — user instinct: required for fleet reality. Counterpoint: dual-stack cost;
   ocpp-go has 1.6 support; alternative is "2.0.1-certified hardware only" positioning.
9. V2G path choice — J3072/V2G-AC relay (AC, standards-complete at EVSE-PEV seam, DME seam open) vs
   OCPP 2.1 V2X (DC-capable, standardized CSMS seam, hardware nascent) vs wait. Planner needs an EV
   discharge term either way.
10. OpenADR 3.1 VEN — small, opens non-CSIP DR revenue; when?
11. Aggregator-profile capability (subscriptions + per-DER EndDevices) vs GFEMS-only positioning.
12. 2030.5-2023 adoption timing (no CSIP pull yet).
13. MA-style schedule-envelope constraint for the optimizer (interconnection-agreement compliance as a
    constraint tier).
14. Meter energy accumulators / revenue-grade Wh (needed for settlement/M&V in any DR/VPP program).
15. Secure SunSpec Modbus + OCPP SP3 — security roadmap sequencing.
16. Storage-bank models 803-805 (utility-scale/multi-string batteries) — market-dependent.
17. bus.Measurement lacks Q/state/alarms — blocks both 1547 monitoring completeness and richer telemetry.
18. DNP3 — third 1547 protocol, some utility DERMS RFPs ask for it; heavy; likely "no" but should be a
    conscious decision.

## 4. Bottom line

The hub's scalar-control CSIP client core, security posture, and southbound Modbus fundamentals are
genuinely strong — in several places beyond spec. The coverage cliff is concentrated in four places:
(1) client-initiated **reporting** (DER*/LogEvent/VAr/PUT) — cheap-to-moderate, cert-blocking;
(2) **advanced-function execution** (curves/PF/energize) — the southbound halves already exist, the middle
is missing; (3) **EV surface** (OCPP 1.6 fleet reality, needs-based charging, V2G absent by construction);
(4) **program adjacency** (OpenADR, aggregator profile, 2030.5-2023) — market-dependent bets.
