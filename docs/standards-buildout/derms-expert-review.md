# DERMS Expert Review — LEXA Hub Standards-Coverage Audit

Reviewer stance: 15+ yr DERMS/DER-interop (CA IOU procurement, SunSpec/IEEE WGs, VPP platforms,
EV charging networks). Review date 2026-07-14. Primary input: `audit/standards-coverage-audit.md`
@ lexa-hub d6ac263. Market claims below are mid-2026 state; hedges are marked.

## Executive verdict

The audit's diagnosis is correct and its four-cluster bottom line is the right map. My deltas are
about *weighting*: (1) the reporting cluster (PUT/DER*/LogEvent/VAr/Wh/bus-Q) is not just a cert
blocker — it is the single biggest commercial gap, because every utility program and VPP buyer in
2026 asks "can I see nameplate, SoC, and alarms?" before they ask about curves. (2) The user is
RIGHT about OCPP 1.6J — 2.0.1-only positioning forfeits the retrofit installed base, and dual-stack
is cheap here. (3) The user is WRONG about building V2G now — no path has procurable hardware +
a standardized seam this product can occupy before ~2027-28; do the cheap type-system enablers,
then wait for OCPP 2.1 V2X hardware. (4) Certify SunSpec CSIP **DER Client** (GFEMS,
single-EndDevice, poll-only — EPRI precedent); do NOT attempt the Aggregator profile. (5) The
audit's biggest miss is **CSIP-AUS/Australia**: the hub already enforces ExpLimW/ImpLimW — the
dynamic-operating-envelope levers of the one market with mandatory, at-scale 2030.5 pull today.

## Gap-by-gap classification

Verdicts: **TS** = table stakes · **WP** = worth pursuing (6-18 mo ROI) · **DEFER** (trigger named)
· **SKIP**. Sizes: S ≤1 wk · M 1-4 wk · L 1-2 mo · XL quarter+ (against the codebase inventories).

| # | Gap | Verdict | Size | Why — market evidence |
|---|---|---|---|---|
| 1 | HTTP PUT + DERCapability/Settings/Status/Availability reporting | **TS** | M | Cert-blocking (CORE-009/014, BASIC-028) AND the commercial front door: every CA IOU handbook, every VPP program (ConnectedSolutions-style, DSGS), and every head-end vendor (Oracle/GE/PXiSE-class) expects the client to declare nameplate and report SoC/status. A DERMS the utility can't see is a demo, not a product. PUT verb into the hand-rolled HTTP client is small; the real work is GFEMS aggregation semantics (what one EndDevice reports for solar+battery+EVSE behind one PCC — see critique). |
| 2 | Curve/PF/energize execution (BASIC-004..012 middle plumbing) | **TS** (for cert) / WP (for revenue) | L | No CSIP cert without it — the BASIC curve/PF/energize tests are M(C), not optional, and PICS "NOT SUPPORTED" doesn't apply. Market nuance: in real Rule 21 deployments the inverter's UL 1741 SB autonomous defaults do the physics; the gateway's job is provisioning + verification — exactly the unwired 705-712 path. Southbound is lib-complete including the adopt handshake; this is bus.ActiveControl fields + desired-doc fields + per-axis reconciler convergence + scheduler passthrough. AD-010's own trigger ("cert-lab scope referencing curve function sets") fires the day you book a lab. |
| 3 | LogEvent alarm poster (BASIC-027, G31/G32 alarm+RTN) | **TS** | S | Cert-required; sources (701 Alrm, breach episodes) already exist in-hub. Also what utility ops actually watch during pilots. Cheap — do it with #1. |
| 4 | VAr in MUP (BASIC-029) | **TS** | S | One of CSIP's four mandatory quantities; meter already parses VAr. Blocked only by #17. A telemetry feed missing reactive power reads as amateur to any utility reviewer. |
| 5 | Registration PIN verify in production walk (CORE-003/BASIC-001) | **TS** | S | Helper exists; wire it. Labs exercise it on day one (PIN 111115 is in the standard test config). |
| 6 | CannotComply 0xF0 → standard Table 27 codes | **TS** | S | Live interop bug, not a roadmap item: any non-LEXA head-end drops or mis-handles 0xF0. Map to 252/253 (reject), 4/8/10 (opt-out/partial), 13/14 (aborted). Keep 0xF0 as a bench alias if gridsim needs it. |
| 7 | Verify-only: COMM-004 reject-paths (pcap), ERR-001 redirects, SunS probe 0/50000 | **TS** | S | COMM-004 D-G demands packet-level proof of rejection — wolfSSL chain validation must be shown fail-closed to depth 4. Redirect-following may need a small code change in httpwire/tlsclient, not just a test. Cheap insurance against a failed (frozen-config) cert campaign. |
| 8 | OCPP 1.6J southbound | **TS** (residential retrofit) | M | See OCPP verdict below. User's instinct is right. |
| 9 | V2G path | **DEFER** (do S-size enablers) | S/M now, XL later | See V2G verdict below. Trigger: pilot LOI in hand, or ≥2 EVSE vendors shipping OCPP 2.1 V2X firmware on bidirectional hardware. |
| 10 | OpenADR 3.1 VEN (Continuous Pricing) | **WP** | M | 3-5 kLOC pure-Go, no wolfSSL, maps ~1:1 onto SetPrices/SetDeliveryTariff/limit rules the hub just built for plan economics. Opens DR revenue in the majority of US territories that will never run CSIP; CA's dynamic-pricing direction (MIDAS/CalFUSE-style) is converging on OpenADR 3 for price transport. Gate the build on ONE named program target; certify CP profile only. 2.0b: SKIP unless a specific program mandates it (15-25 kLOC of XML/EI pain). |
| 11 | Aggregator profile (per-DER EndDevices, subscriptions) | **DEFER** | XL | Wrong profile for a one-PCC GFEMS product. Mandatory subscription/notification means hosting a TLS listener on a NAT'd residential LAN — architecturally hostile. If a fleet business emerges, note 2030.5-2023's ProxiedDevice/AggregatedDevice is the sanctioned model for exactly this gateway — don't build 2018-style aggregator machinery that 2023 obsoletes. Trigger: a utility that insists on per-asset EndDevices, or a multi-site fleet contract. |
| 12 | 2030.5-2023 adoption | **DEFER** | L-XL | CSIP pins 2018; no US program demands schemaVer 2.2 yet; zero security delta so nothing rots. Watch items: absolute-watt site caps (opModMaxLimWInject/WAbsorb) and ProxiedDevice. Trigger: a CSIP revision or SIWG action referencing 2023. |
| 13 | Schedule-envelope / ISA-compliance constraint tier | **WP** | M | Generalize beyond MA: non-export agreements, ISA-bound BESS schedules, and demand-limit riders exist in most territories, and a free-running tariff optimizer that breaches an interconnection agreement is a product-liability event, not a missed feature. The constraint stack (AD-007 tiers) is the natural home; MA's "Scheduled vs Unscheduled is binding, change = re-study" is just the sharpest instance. Do before any Northeast storage deployment. |
| 14 | Meter energy accumulators / revenue-grade Wh | **TS** | S | TotWhImp/Exp offsets are already declared in the model tables — parse them and carry Wh on the bus. Without interval energy there is no settlement, no M&V, no OpenADR usage report, no honest validation of your own plan-economics numbers. Cheapest high-leverage item on the list. (True "revenue-grade" needs a certified meter — position hub Wh as operational/M&V data.) |
| 15 | Secure SunSpec Modbus + OCPP SP3 | **DEFER** | M each | Secure Modbus v1.0 is Dec 2025 paper — zero fielded device support; crypto reuses CCM-8 so the stack is ready when it matters. SP3: no residential charger fleet presents client certs in 2026; SP2 fail-closed already exceeds the field norm. Trigger: device availability (Modbus) / a utility cyber addendum or charger fleet support (SP3). |
| 16 | Storage-bank models 803-805/807 | **SKIP** (for now) | — | Multi-string/utility-scale BESS models; residential/light-commercial packs expose 802/713 or vendor registers. Revisit only if a light-commercial BESS design win presents string-level models. |
| 17 | bus.Measurement Q/state/alarms | **TS** | S/M | Not a standards gap per se — the enabler for #1, #3, #4 and 1547 Table 29 monitoring. Do it first; everything in the reporting cluster lands on top of it. |
| 18 | DNP3 | **SKIP** | — | No pull at this product class. DNP3 lives at utility SCADA/RTU (MA RMAC-style, >250-500 kW) and utility-scale sites — a different product tier and a certified-RTU procurement, not a Pi-class gateway feature. Make it a conscious "no" in positioning; partner with an RTU vendor if a >500 kW deal ever demands it. |

## OCPP hypothesis verdict — "we need 1.6J": **CORRECT**

- **Installed-base reality**: in mid-2026 the large majority of deployed, OCPP-speaking AC chargers
  — and a majority of what's still shipping at residential price points — are 1.6J. OCPP 2.0.1 is
  contractually forced in NEVI-corridor DC fast charging and new enterprise procurement, not in the
  home/light-commercial segment this product serves. A local CSMS that can't accept a 1.6J
  `ws://` connection cannot attach to the retrofit market, which is precisely where a
  bolt-on DERMS gateway gets sold.
- **Dual-stack cost is modest**: lorenzodonini/ocpp-go ships a 1.6 package alongside 2.0.1; the
  hub's OCPP surface is deliberately minimal (boot/heartbeat/status/transactions/MeterValues/
  SetChargingProfile/TriggerMessage), and every one of those has a near-1:1 1.6J equivalent
  (StartTransaction/StopTransaction/MeterValues/SetChargingProfile-TxDefaultProfile). The
  reconciler shell, plausibility gates, and "Accepted ≠ convergence" logic are protocol-agnostic.
  Size M: a version-dispatching listener + a 1.6 bridge behind the same `bridge.Apply` seam.
  Security: 1.6J + TLS + Basic Auth (the 1.6 security whitepaper's Profile 2 analog) under the
  same fail-closed config gate.
- **Keep 2.0.1 as the preferred/certified path** and market it that way ("2.0.1-native, 1.6J
  compatibility mode"). Do NOT invest in 1.6 feature depth — smart charging profile + metering
  is enough.
- **ISO 15118 / Plug&Charge: irrelevant here.** P&C solves roaming/billing identity at public
  chargers; a single-owner residential site behind a local CSMS has no such problem. Skip.
- **Sharper point than protocol version**: the missing Authorize/pairing gate (roadmap R8 — any
  charger that dials in transacts and becomes plant) is a worse commercial and security defect
  than the absent 1.6J. Fix both in the same milestone; add ClearChargingProfile while in there
  (it is trivial and its absence looks unfinished in any integration review).

## V2G hypothesis verdict — "we need V2G": **NOT YET — build the cheap enablers, then wait**

Path-by-path, 2026-2028:

- **J3072 / SunSpec V2G-AC relay**: standards-complete at the EVSE↔PEV seam (Approved 2025-09),
  but (a) essentially no J3072-certified onboard-inverter vehicles or A1 EVSEs are procurable in
  North America in 2026, and (b) the DME↔EVSE seam — exactly where LEXA lives — is explicitly
  unspecified (no OCPP, no 2030.5 named). Building here means inventing a proprietary relay leg
  for hardware you can't buy. SKIP until the ecosystem exists.
- **OCPP 2.1 V2X**: the right long-term seam for THIS product — the hub already is the CSMS, and
  OCPP 2.1 (published 2025) standardizes bidirectional setpoints and even 1547-style DER controls
  at the CSMS↔EVSE interface. Hardware is nascent; expect credible bidirectional AC/DC EVSEs with
  2.1 firmware ~2027. **This is the path to bet on** — WATCH, with the trigger named below.
- **ISO 15118-20 BPT (DC)**: real hardware is arriving (bidirectional DC wallboxes), but 15118-20
  terminates inside the EVSE; a Pi-class gateway never touches it. The gateway's leverage over a
  BPT EVSE is, again, OCPP 2.1. Not a LEXA workstream.
- **What actual 2026 V2X deployments look like**: OEM-proprietary V2H stacks (Ford HIS/Sunrun,
  GM Energy, Tesla Powershare) and school-bus/fleet DC pilots on bespoke integrations. None of
  these buy a third-party residential DERMS gateway a seat at the table today.
- **Do now (cheap, no market risk)**: kill the charge-only type construction while the codebase is
  fresh — signed `EVSECommand` setpoint, EV-as-storage discharge term in the DP planner, breach/
  limit treatment of EV as a DER (S/M). This is refactoring insurance so a 2027 pilot doesn't
  require type-system surgery. Northbound needs nothing: EV-as-DER already fits DERType 81/82 and
  2030.5-2023 adds nothing V2G-specific.
- **Trigger to build for real**: a signed pilot/LOI with a named utility + EVSE partner, or ≥2
  EVSE vendors shipping OCPP 2.1 V2X firmware on orderable bidirectional hardware.

## Certification strategy

- **Certify SunSpec CSIP DER Client (single EndDevice, GFEMS), poll-only.** CSIP §3.2 Scenario 1
  GFEMS is exactly this product (one gateway, one PCC, aggregate plant capability), and poll-only
  clients are certifiable in practice (EPRI's QualityLogic-certified client ran every test via
  polling). The Aggregator profile adds mandatory subscriptions (inbound TLS listener), per-DER
  EndDevice fan-out, and the whole UTIL/AGG/MAINT groups — a quarter-plus of work for a topology
  the product doesn't sell. Certify [C] first; revisit [A] only on a fleet contract.
- **Who asks for it**: CA Rule 21 Phase 2 interconnection paperwork (the gateway/EMS path is the
  sanctioned compliance unit — the cert applies to the gateway); utility DERMS pilots and
  munis/co-ops copying CA use "SunSpec CSIP certified" as procurement shorthand; it is the only
  third-party mark a small embedded vendor can wave against Enphase/SolarEdge/Tesla native
  clients. Caveat buyers won't tell you: SunSpec cert ≠ IOU acceptance — PG&E/SCE handbooks add
  utility-specific interop passes afterward. Budget both.
- **Minimum closure set to pass** (my reading of the v1.3 matrix): gaps #1 (PUT + all four DER*
  PUTs — CORE-009/014, BASIC-028), #2 (BASIC-004/005/006/008/009/011/012 curve/PF/energize/ramp
  execution — the long pole), #3 (BASIC-027), #4 (BASIC-029 VAr), #5 (CORE-003/BASIC-001 PIN),
  #6 (CORE-022 standard response codes), #7 (COMM-004 pcap-proven reject paths + ERR-001
  redirects). Already in hand: TLS/cipher (COMM-003), LFDI/SFDI, event lifecycle + randomization
  (CORE-021), supersession, DefaultDERControl fallback, ≥24-event storage discipline, pollRate
  honoring, MUP mechanics. Note BASIC-007 (setGradW/setSoftGradW ramp defaults) rides in with #2.
- **Campaign mechanics to respect**: configuration freeze (no software changes once testing
  starts — land everything, then soak on the bench against gridsim replicas of the test topology
  first), pcap deliverables for COMM-004, full unencrypted HTTP logs in the TRR. Plan one
  dry-run campaign on the bench harness before paying lab time.

## Phased roadmap (with sizes)

**Phase 1 — next quarter: "make the site visible + own the retrofit socket" (cert prerequisites,
all S/M):**
1. bus.Measurement Q/state/alarms enabler (#17, S/M) — unblocks everything below.
2. HTTP PUT verb + DERCapability/Settings/Status/Availability reporting incl. GFEMS aggregation
   semantics (#1, M).
3. VAr in MUP (#4, S) + Wh accumulators on meter/bus (#14, S).
4. LogEvent poster (#3, S).
5. Standard Response codes replacing 0xF0 (#6, S).
6. PIN verify in production walk (#5, S).
7. Verification sweep: COMM-004 reject pcaps, ERR-001 redirect support, SunS probe at 0/50000
   (#7, S).
8. OCPP 1.6J dual-stack + Authorize/pairing gate + ClearChargingProfile (#8 + R8, M).

**Phase 2 — 6-12 mo: "advanced functions + the cert campaign":**
9. Curve/PF/energize middle plumbing (#2, L): ActiveControl/desired-doc fields, scheduler
   passthrough, per-axis reconciler convergence, ignored-curve alarm, ramp defaults; wire
   DefaultRvrtTms as the device-side belt to the reconciler's suspender (S, inside this work).
10. CSIP DER-Client cert campaign: bench dry-run vs test-server topology, then ATL (calendar L,
    code ~0 if Phase 1/2 landed).
11. Schedule-envelope / ISA constraint tier (#13, M).
12. OpenADR 3.1 CP VEN as `lexa-openadr` (#10, M) — pull the trigger only with a named program.
13. CSIP-AUS market spike (S): gap-assess the already-enforced ExpLimW/ImpLimW path against
    CSIP-AUS/SAPN compliance requirements; decide on an AUS entry.

**Phase 3 — opportunistic / trigger-gated:**
14. V2G enablers: signed EVSE setpoint types + planner discharge term (S/M); OCPP 2.1 V2X build
    only on trigger (#9).
15. 2030.5-2023 watch (#12); Aggregator/ProxiedDevice only on fleet demand (#11).
16. OCPP SP3, Secure SunSpec Modbus on ecosystem triggers (#15).
17. Standing SKIPs: DNP3 (#18), 803-805 (#16), OpenADR 2.0b, EXI, ISO 15118 P&C.

## Audit critique

**Right and well-evidenced**: the coverage matrix is honest (including self-flagging ExpLimW/ImpLimW
as CSIP-AUS, and the 0xF0 interop bug — most internal audits hide those); the "southbound halves
exist, the middle is missing" framing of gap 2 matches the code inventory exactly; the strengths
section correctly identifies the convergence/fail-closed machinery as genuine differentiators —
most shipping clients trust ACKs.

**Missed**:
1. **CSIP-AUS / Australia** — the biggest omission. The one place on earth with mandatory,
   at-scale 2030.5 deployment pull (dynamic operating envelopes / flexible exports across SA,
   QLD, and spreading) runs on exactly the ExpLimW/ImpLimW levers LEXA already enforces with
   convergence backstops. The audit notes those modes are "invisible to CA cert" and stops; it
   should have flagged a near-term market where the hub's oddball strength is the core
   requirement.
2. **GFEMS aggregation semantics inside gap 1**: what DERCapability does one EndDevice PUT for
   solar+battery+EVSE behind one PCC (DERType 83? summed ratings? settings ≤ ratings invariants
   that CORE-014 operator-checks)? That design question is the actual work in #1 and it's
   invisible in the audit's "add PUT verb" framing.
3. **Cert campaign economics**: config freeze, pcap deliverables, lab fees, and the
   utility-handbook interop pass AFTER SunSpec cert. The audit treats certification as a gap
   list; it's also a calendar and budget.
4. **Competitive frame**: no mention that the comparables for this product are SwitchDin-style
   edge gateways and inverter-native CSIP(-AUS) clients, not the enterprise DERMS platforms —
   which is what makes the multi-vendor, one-PCC gateway niche defensible and the Aggregator
   profile a distraction.
5. **OCPP pairing gate** (R8) deserved gap-list status ahead of several protocol-completeness
   items; it's a live security/commercial defect, disclosed but buried.

**Over-weighted**: the 2030.5-2023 delta gets a detailed section for a standard with zero program
pull (correctly deferred, but the ink spent signals more urgency than exists). Storage-bank
models and Secure SunSpec Modbus are correctly parked but didn't need matrix rows this early.
The MA section is good analysis but frames schedule-envelope as an MA quirk when it generalizes
to ISA/non-export compliance everywhere — that generalization is what makes gap 13 worth building.

**Net**: adopt the audit's gap list with the reclassifications above; sequence reporting-cluster
first, 1.6J alongside, curves second, cert third, and spend one week validating the Australia
option before committing the OpenADR build.
