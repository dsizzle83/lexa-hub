# SunSpec Modbus Conformance + Security Digest (for LEXA hub standards-coverage audit)

Context: LEXA hub is a SunSpec Modbus **CLIENT** (southbound, `cmd/modbus` +
`internal/southbound`) and an IEEE 2030.5/CSIP **client** (northbound, SunSpec PKI).
Client-conformance tests treat the hub as the Client Under Test (CUT); device-side tests
describe the servers (inverters/batteries/meters) the hub polls.

## Document identities

| Doc | Version / status | Date | Scope |
|---|---|---|---|
| SunSpec Modbus IEEE 1547-2018 Profile Spec & Implementation Guide | v1.1 | 2024-02-15 | Normative profile: which SunSpec models/points implement each IEEE 1547-2018 clause-10 function; models 701-713 + Common (1) |
| SunSpec Modbus Client Conformance Test Procedures | v1.1, Approved | 2025-12-10 | Conformance tests where the Modbus CLIENT is the DUT — directly applicable to the hub |
| SunSpec Modbus Conformance Test Procedures | v1.4, Approved | 2025-10-15 | Device (server)-side conformance: discovery, model, curve, reversion, protocol, exception tests |
| SunSpec Modbus Conformance for IEEE 1547-2018 Test Procedures | v1.0, Approved | 2024-07-08 | 1547 add-on to device tests: MOD-4 mandatory-points check + scale-factor range test |
| SunSpec Modbus Conformance Test Procedures Results Reporting | v1.2, Approved | 2025-12-10 | TRR format (CSV summary + JSON hex message logs); authorized labs list |
| Secure SunSpec Modbus Specification | v1.0, Approved | 2025-12-10 | Modbus/TCP Security (mbaps): mutual TLS + X.509 role extension RBAC; TCP only (RTU deferred) |
| SunSpec Test PKI Certificates (App Note) | v1.0 | 2019-04-05 | Test-PKI cert packages for CSIP test procedures; SAN hwType/hwSerialNum conventions |
| SunSpec Alliance Certification Practice Statement | v1.0 | 2024-08-13 | Production 2030.5 PKI: SERCA/MCA/MICA hierarchy, no renewal, no revocation, indefinite lifetime |

## 1547-profile model/point mapping

The profile is implemented with SunSpec models 701-713 (DER "7xx" family), plus Common
Model 1. High-level mapping (Guide Table 1) and key points:

| IEEE 1547-2018 function | Model(s) | Key points (required unless noted) |
|---|---|---|
| Nameplate information | DERCapacity (702), DERStorageCapacity (713), Common (1) | 702.WMaxRtg, WOvrExtRtg(+PF), WUndExtRtg(+PF), VAMaxRtg, NorOpCatRtg, AbnOpCatRtg, VarMaxInjRtg, VarMaxAbsRtg, WChaRteMaxRtg, VAChaRteMaxRtg, VNomRtg, VMaxRtg, VMinRtg, CtrlModes, ReactSusceptRtg; 1.Mn/Md/SN/Vr; optional 702.IntIslandCatRtg |
| Configuration information (optional in 1547) | DERCapacity (702) | 702.WMax, WMaxOvrExt/WOvrExtPF, WMaxUndExt/WUndExtPF, VAMax, IntIslandCat, VarMaxInj, VarMaxAbs, WChaRteMax, VAChaRteMax, VNom (all "-AS" applied settings) |
| Monitoring information | DERMeasureAC (701), 713 | 701.W, 701.Var, voltage (LLV/LNV/VL1L2/VL1/VL2L3/VL2/VL3L1/VL3 — "applicable voltage points must be implemented"), 701.Hz, 701.St (operational state), 701.ConnSt, 701.Alrm; 713.SoC (storage; MUST be 0 if model present w/o storage) |
| Constant power factor | DERCtlAC (704) | 704.PFWInjEna [DISABLED/ENABLED], 704.PFWInj.PF, 704.PFWInj.Ext |
| Voltage-reactive power (volt-var) | DERVoltVar (705) | 705.Ena, VRef, VRefAutoEna, VRefAutoTms, RspTms, Crv.Pt[1-4].V/.Var; Crv.DeptRef [W_MAX_PCT, VAR_MAX_PCT, VA_MAX_PCT], Crv.Pri [REACTIVE] |
| Active power-reactive power (watt-var) | DERWattVar (712) | 712.Ena, Crv.Pt[1-6].W/.Var — Pt[6..4]=P3/P2/P1 gen, Pt[3..1]=P'1/P'2/P'3 load. **6-point curve MUST be supported; unused first 3 (charging-side) points ignored / set to 0** |
| Constant reactive power | DERCtlAC (704) | 704.VarSetEna, VarSetMod [W_MAX_PCT/VAR_MAX_PCT/VA_MAX_PCT], VarSetPri [REACTIVE], VarSetPct |
| Voltage-active power (volt-watt) | DERVoltWatt (706) | 706.Ena, RspTms, Crv.Pt[1-2].V/.W, Crv.DeptRef [W_MAX_PCT] |
| Voltage trip | DERTripLV (707), DERTripHV (708) | Crv.MustTrip.Pt[1-5].V/.Tms — 1547's 2-setting (UV1/UV2, OV1/OV2) trip maps onto a **5-point uniform curve**: Pt2 = inner setting (UV2/OV2 V+time), Pt4 = outer setting (UV1/OV1), Pt1/3/5 are slope/continuity fillers (V1 beyond V2, Tms1=Tms2, V3=V2, Tms3=Tms4, V5=V4, Tms5>Tms4) |
| Momentary cessation (optional) | 707/708 | Crv.MomCess.Pt[1].V/.Tms — horizontal 2-point curve (defaults LV 50%/0-2s, HV 110%/0-13s) |
| Frequency trip | DERTripLF (709), DERTripHF (710) | Crv.MustTrip.Pt[1-5].Hz/.Tms — same 5-point construction as voltage trip (Pt2=UF2/OF2, Pt4=UF1/OF1) |
| Frequency droop (freq-watt) | DERFreqDroop (711) | Ctl.DbOf, Ctl.DbUf, Ctl.KOf, Ctl.KUf, Ctl.RspTms, Ctl.ReadOnly |
| Enter service | DEREnterService (703) | 703.ES [DISABLED/ENABLED], ESVHi, ESVLo, ESHzHi, ESHzLo, ESDlyTms, ESRmpTms (+V_SF, Hz_SF); optional ESRndTms |
| Cease to energize and trip | DEREnterService (703) | No dedicated point: performed by **disabling 703.ES** (Permit Service); re-enable ⇒ DER returns per enter-service settings |
| Limit maximum active power | DERCtlAC (704) | 704.WMaxLimPctEna [DISABLED/ENABLED], 704.WMaxLimPct (+_SF) |

Profile-specific behaviors (Guide §3.1 General Requirements, normative):
- Scale-factor points (V_SF, Hz_SF, PF_SF, W_SF, Db_SF, K_SF, Tms_SF, DeptRef_SF,
  RspTms_SF...) are required alongside their data points — clients must apply them.
- **Curve handling**: curve/curve-set models MUST implement curve 1 as READ-ONLY (the live
  settings) and curve 2 as WRITABLE (the update staging area); additional curves MAY exist
  (read-only ones placed at the end). Adoption is via AdptCrvReq/AdptCrvRslt
  [IN_PROGRESS, COMPLETED, FAILED] (AdptCtlReq/Rslt for 711). So a client changes a curve
  by writing curve 2+ and requesting adoption, then verifying COMPLETED — not by writing
  curve 1.
- A writable point that supports only a single value MUST still accept a write of that
  value (e.g., trip/droop Ena where DISABLED isn't supported must still accept ENABLED;
  712 Crv.Pri must accept a write of REACTIVE).
- All enumerated values listed per model MUST be supported.
- Requirements of the SunSpec Device Information Model spec and DER Information Model
  spec remain in force underneath the profile.
- Priority: reactive-power functions carry Pri [REACTIVE] (reactive precedence);
  VarSetMod/DeptRef select the percentage base (W_MAX/VAR_MAX/VA_MAX).
- 1547.1-2020 results-reporting (RR) labels (NP_*, *-AS) are given per point — useful
  for mapping hub data to 1547.1 test artifacts.

**Note for LEXA**: the hub's southbound today speaks the legacy 1xx models
(inverter 10x + controls 121/123 style, per `internal/southbound`); this profile is the
7xx-series interface. A 1547-2018-certified inverter presents 701-713 — coverage gap to
track in the audit.

## Modbus CLIENT conformance test catalog (hub as DUT)

Test input: a Client PICS (models the client supports). Setup: two servers built from the
PICS model list in RANDOM order, with ≥5 extra models injected and ≥1 PICS model
DUPLICATED — the client must iterate the whole chain regardless of order/repetition, and
tolerate 100/200/700/800-series models it may not use. Common criteria for all CLI/READ/WR
tests: the client SHALL accurately log (screen/file/test-interface) and traffic is captured
to verify behavior.

| ID | Behavior validated | Pass criteria |
|---|---|---|
| CLI-1 | Discovery against servers at different IPv4 addresses (one may be localhost) | Logs ALL models present in each server + all Common Model point contents |
| CLI-2 | Discovery at arbitrary TCP ports (random 1-65535) | Same as CLI-1 — port must be configurable, not hardcoded 502 |
| CLI-3 | Discovery with different Modbus Unit IDs (1-247) | Same as CLI-1 — unit ID configurable |
| CLI-4 | Discovery with base register 0, 40000, AND 50000 | Same — client must probe all three standard SunSpec start addresses for the "SunS" marker |
| CLI-5 (optional) | RTU: discovery at each supported baud rate | Same criteria per baud rate in PICS |
| READ-1 | Single-point reads: every point of every PICS model read individually | All points logged as hex strings |
| READ-2 | Multi-point reads up to Modbus max (125 registers); models >125 regs read in multiple maximal reads (partial-point reads not required) | All points logged as hex; may be satisfied during CLI-1..4 discovery |
| WR-1 | Write single point (FC 0x06): every adjustable point at min, max, and 3 intermediate values; every supported enum value; RTU repeat with Unit ID 0 (broadcast) | Writes logged as hex; server-side effect validated by engineer |
| WR-2 | Write multiple (FC 0x10) used for each adjustable point (need not batch) — same value matrix as WR-1 | Same as WR-1 |
| INFO-1 | Type interpretation: human-readable rendering of every type — decimal for ints w/ **scale factors applied**; floats decimal; ipaddr dotted-decimal; ipv6addr hex-grouped; eui48 capitalized `B8:27:EB:...`; bitfield16/32 as hex; string as UTF-8; enum16/32 as base-10 value; pad/count/sunssf/acc as base-10. Duplicated models may be skipped | All values logged human-readable & correct |
| INFO-2 | **Unimplemented-value handling**: server sets ≥1 point per data type to its unimplemented sentinel (e.g., 0x8000 int16, 0xFFFF uint16, NaN for floats); client reads all models | Client accurately identifies/logs all unimplemented points (must not treat sentinel as data) |
| PROT-1 | Recovery from a partial (truncated) Modbus response | Client remains functional; subsequent normal read succeeds and logs expected values |
| PROT-2 | Modbus TCP response split across multiple TCP segments | Client reassembles and parses correctly |
| ERR-1 | Noncompliant server (SunSpec map at 40001 — no marker at 0/40000/50000) | Client logs the noncompliant server (fails gracefully, no crash/hang) |
| ERR-2 | Modbus exceptions 1-4 (illegal FC 0x88, write to RO register, write to unimplemented point, undefined enum value) | Client logs each exception and continues operating normally |
| ERR-3 | Unknown model ID present in server's model chain | Client connects without issues; discovered-model list MUST NOT include the unknown-ID model (skip via its L field, keep walking) |

Certification labels: tests reported as `Test <ID>` PASS/FAIL/NOT SUPPORTED in the TRR.

## Device-side test scope (summary) — and what a client should mirror

Device (server) conformance v1.4, DUT = inverter/battery/meter:
- **DEV-1/DEV-2**: discovery — "SunS" marker at 0/40000/40001-equivalent standard bases
  (0, 40000, 50000), end model ID 65535 with length 0, Model 1 present and matching PICS.
- **MOD-1/2/3** (per model): model present w/ correct length; whole-model read in one
  (or several ≤125-reg) reads; every adjustable point written at min/max/3-mid values,
  write-then-read must match (v1.3 REMOVED the previously allowed 1000 ms read-after-write
  delay — devices must reflect writes immediately).
- **CRV-1/2/3** (per 7xx curve model): curve 1 read-only; adopt-settings success path
  (AdptCrvReq → status success → curve 1 reflects new values); adopt with out-of-range
  curve index → status failure, curve 1 unchanged.
- **REV-1/2/3** (per reversion timer): settings revert on timeout (±2 s); time-remaining
  point counts down (±2 s); rewriting timer extends it; writing 0 cancels.
- **RTU-1..5**: RTU interface, baud rates, partial-request recovery, broadcast (Unit 0:
  no response but write applied; rejects writing 0 to Device Address), device-address
  change 1-247.
- **TCP-1..3**: TCP interface, partial-request recovery, request split across TCP frames.
- **MB-1/MB-2**: FC16 and FC06 writes both work; single-register FC03 reads work (note:
  a device MAY reject reading a 16-bit slice of a >16-bit value — not noncompliant).
- **EXC-1..3**: invalid value ⇒ exception 2/3/4 and value NOT applied; write to RO
  register ⇒ exception 2/3/4; illegal function code ⇒ exception 1.

1547 add-on test procedures (v1.0): all base device tests PLUS **MOD-4** (every model and
every mandatory point from the Profile Guide §3 tables present) and the **Scale Factor
Test** (every implemented scale factor within the range that meets IEEE 1547 accuracy for
the point's data type).

What the hub, as client, should mirror from device-side behavior:
1. Expect and honor curve-1-read-only / write-curve-2-then-adopt semantics (CRV tests);
   check AdptCrvRslt for COMPLETED/FAILED rather than assuming a write took.
2. Expect write-then-readback to match immediately (basis of the hub's
   verify-by-readback reconciler); a device needing settle time is noncompliant per v1.3.
3. If using reversion timers as a dead-man safety, refresh before timeout and expect
   ±2 s tolerance; writing 0 cancels.
4. Expect exceptions 2/3/4 on bad writes and treat the write as NOT applied; expect
   devices to reject single-register reads that split multi-register values (read
   aligned).
5. Broadcast writes (RTU Unit 0) elicit no response — don't treat silence as failure.
6. Devices may legitimately reject values outside their PICS range — the hub should
   clamp to nameplate/settings ranges before writing.

## Secure SunSpec Modbus summary + client obligations

What it is: v1.0 (Approved 2025-12-10) profile of the Modbus.org MODBUS/TCP Security
Protocol Spec ("mbaps"), hardened by SunSpec. **TCP only** — RTU security was deferred to
a future revision. Very new (drafted Aug-Dec 2025): maturity is "approved spec, nascent
deployment"; no conformance test procedure for it is in this document set.

Core mechanics:
- Mutual TLS wrapping standard Modbus/TCP (mbap unchanged inside the tunnel,
  SunSpecTCP-9), port 802 SHOULD be used (802 secure vs 502 plain may coexist on one
  device, Appendix B).
- TLS 1.2 MUST (RFC 5246), TLS 1.3 MAY. Mandatory 1.2 suites in this order:
  TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, ..._CHACHA20_POLY1305_SHA256,
  ..._AES_128_CCM_8 (CCM_8 deliberately included because IEEE 2030.5 mandates it — the
  hub's wolfSSL CCM-8 support is reusable). 1.3 suites (if supported): AES_128_GCM,
  CHACHA20_POLY1305, AES_128_CCM. HMAC-MD5/SHA-1/NULL prohibited; HMAC-SHA-256 required.
  ECC: P-256 minimum; Supported-Curves + Point-Format extensions required in ClientHello.
- Mutual authentication mandatory: server MUST send CertificateRequest; a client that
  doesn't present a cert gets a fatal alert; no resumption after fatal alert.
- **RBAC via X.509v3 role extension** (OID 1.3.6.1.4.1.50316.802.1, ASN.1 UTF8String,
  exactly ONE role per cert; extension REQUIRED on the CLIENT cert, not the server's).
  Mandatory roles every device must support: ReadOnlySunSpec, GridServiceSunSpec
  (read all + write commanded/autonomous functions, NO protection/network settings),
  NetworkAdministratorSunSpec, SuperAdministratorSunSpec (full R/W). IEC 62351-8 roles
  optional. Roles-to-rights map for the mandatory roles comes from
  github.com/sunspec/rbac. Authorization rejection = Modbus exception 01, no further
  detail; missing role in cert ⇒ exception 01.
- PKI: three-tier (root/intermediate/leaf) anticipated, aligned with 2030.5; self-signed
  allowed for local/LAN use, CA-signed MUST for anything internet-routed; devices MUST
  hold ≥10 root certs and have a secure add/remove mechanism (out-of-band portal or
  secured in-band Modbus push); full chain to root MUST be sent in the handshake.
- Session details: Max Fragment Length negotiation (RFC 6066) incl. 512-byte option
  MUST; compression NULL; secure renegotiation (RFC 5746) MUST.

Client obligations specifically (as opposed to server): present an X.509v3 cert
containing the role extension when asked (SunSpecTCP-12, -27, -28); send full chain
(-51); support the mandatory cipher suites/curves; and — explicitly called out in §3.5 —
**the client must be programmed to only read/write registers its role permits**, because
out-of-role requests just return exception 01 with no explanation. A GridServiceSunSpec
client (the natural role for the LEXA hub) must therefore avoid touching trip/protection
and network registers. VPP/aggregator use case (A.2) = GridServiceSunSpec; utility
DERMS (A.1) = SuperAdministratorSunSpec.

## PKI/cert practice requirements (SunSpec CA — the hub's CSIP identity)

Hierarchy (CPS §1.3.2, applies to production IEEE 2030.5 certs):
- **SERCA** (Smart Energy Root CA, one root: `O=SunSpec Alliance, CN=IEEE 2030.5 Root,
  SN=<n>`) → **MCA** (Manufacturer's CA, optional per-org tier for multi-line
  manufacturers) → **MICA** (Manufacturer Issuing CA, issues device certs). Direct
  SERCA→device issuance is allowed by 2030.5 but **NOT supported under this CPS**.
- Device cert Subject **MUST be empty** (2030.5 requirement); identity lives in
  SubjectAltName as RFC 4108 HardwareModuleName: hwType = manufacturer model OID
  (IANA PEN + model hierarchy, e.g. 1.3.6.1.4.1.99999.13.1), hwSerialNum = serial as
  DER OCTET STRING (utf-8 by convention). (hwType, hwSerialNum) pair MUST be unique;
  the MICA checks the CSR public key isn't reused across serials.
- Crypto: ECDSA P-256 + SHA-256 throughout; CA keys in FIPS 140-2 L3+ HSMs; subscriber
  (device) key pairs generated by the Authorized Applicant (manufacturer).
- Policy OID in certs: 1.3.6.1.4.1.53630.0.0.

Lifecycle facts that matter to a CSIP client device:
- **Renewal is NOT supported** (§4.6) and **revocation/suspension is PROHIBITED**
  (§4.9); no CRL, no OCSP (§4.9.9). §3.4: "CA and Subscriber [certs] have an indefinite
  lifetime and can not be revoked." I.e., device cert lifetime = device lifetime —
  matching 2030.5's long-lived device certs. Consequence: a compromised device key is
  handled operationally (replace the device cert = re-key/re-provision), not via PKI
  status checking; a client need not (cannot) do CRL/OCSP checks against this PKI.
- Re-key IS supported for CAs (new key pair, new cert, incremented SN); compromised
  MCA/MICA keys are replaced, never used to sign again (§5.7.1).
- **Relevance to LEXA cert rotation (TASK-073)**: rotation of the hub's own client cert
  is a re-provisioning event, not a renewal; and since LFDI hashes the full DER cert,
  ANY reissue changes the hub's LFDI — the runbook's "re-enrollment vs rotation" caveat
  is consistent with this CPS (no in-place renewal exists).
- Certs published to a public repository (pki.sunspec.org) within 1 h (subscriber) /
  48 h (CA); archive kept ≥3 years.

Test PKI (application note, used for CSIP conformance testing — matches the bench's
air-gapped cert setup):
- Same three CA types (SERCA/MCA/MICA); separate client vs server intermediate chains
  (a SunSpec test-PKI convention, not a 2030.5 distinction).
- Package = 3 valid device certs at chain depths dev / mica-dev / mca-mica-dev, the
  device private key (PKCS#8 PEM), root cert, plus deliberately-broken error certs; PEM,
  .p7b, .p12 (password `password`). Naming: `sat-<cli|svr|key>_<chain>_<model>_<serial>`.
- **DUT should test with the mca-mica-dev (deepest) chain** — best validation of
  compliant chain handling; the hub's wolfSSL client must verify a 3-intermediate-deep
  chain, not just root+leaf.
- Requesting a package requires device type (client/server), manufacturer model OID,
  serial; batch generation increments numeric serials.

## Numbered client obligations (the hub as SunSpec Modbus client / CSIP client)

1. Discover SunSpec maps at ALL THREE standard base addresses (0, 40000, 50000) via the
   "SunS" marker; log a noncompliant server (e.g., map at 40001) gracefully (CLI-4, ERR-1).
2. Support configurable IP, port, and Unit ID (1-247) per device (CLI-1..3).
3. Iterate the full model chain to end-marker 65535/len 0, tolerating random model order,
   duplicated models, and unrelated 100/200/700/800-series models (server setup rule).
4. SKIP models with unknown IDs using their length field — never abort discovery, and
   never report the unknown model as discovered (ERR-3).
5. Read efficiently: batch reads up to 125 registers, splitting larger models into
   maximal reads (READ-2); tolerate devices rejecting sub-point single-register reads.
6. Support BOTH FC 0x06 single-write and FC 0x10 multi-write for adjustable points
   (WR-1/WR-2).
7. Interpret every SunSpec type correctly, **applying scale factors** (sunssf) to numeric
   points, and render enums/bitfields/addresses per INFO-1 rules.
8. Treat unimplemented-value sentinels per type (0x8000/0xFFFF/NaN...) as ABSENT data,
   never as measurements (INFO-2) — maps to the hub's NaN-rejection/plausibility rules.
9. Survive partial/truncated responses and TCP-segmented responses without wedging
   (PROT-1/2); recover and continue on Modbus exceptions 1-4, logging them (ERR-2), and
   treat an exception on write as "not applied" (drives write-retry/reconcile logic).
10. Honor curve semantics on 7xx models: curve 1 is read-only truth; write a staging
    curve and adopt via AdptCrvReq, then confirm AdptCrvRslt=COMPLETED and re-read curve
    1 (Profile §3.1, CRV tests) — "trust measurement, not the command" applies to curve
    adoption too.
11. Respect reversion timers where used (refresh before timeout; 0 cancels; ±2 s
    tolerance) and expect settings to snap back on expiry (REV tests).
12. For 1547-2018 devices, expect the 701-713 model set and the profile's mandatory
    points (MOD-4); map trip settings through the 5-point curve construction and
    watt-var through the fixed 6-point curve (unused charge-side points = 0).
13. Under Secure SunSpec Modbus: do mutual TLS 1.2+ with the mandatory ECDHE-ECDSA
    suites (GCM/CHACHA20/CCM_8, P-256), present a client cert carrying exactly one role
    in OID 1.3.6.1.4.1.50316.802.1, send the full chain, and restrict reads/writes to
    registers the role permits (expect bare exception 01 otherwise); GridServiceSunSpec
    is the fitting role for a DERMS-style controller that must not touch protection
    settings.
14. As a CSIP/2030.5 client: use a device cert with EMPTY Subject and
    hwType(model OID)+hwSerialNum SAN, chain to SERCA via MICA (optionally MCA); do NOT
    rely on CRL/OCSP (none exist — revocation prohibited, indefinite lifetime); plan
    cert replacement as re-provisioning (renewal unsupported; new cert ⇒ new LFDI); and
    validate against the deepest (mca-mica-dev) test chain during conformance testing.
