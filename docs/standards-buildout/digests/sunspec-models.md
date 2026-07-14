# SunSpec model catalog digest — for LEXA hub Modbus-client audit

Audience: audit of `lexa-proto/sunspec` (SunSpec Modbus CLIENT in lexa-modbus polling
inverters/batteries/meters, optimizer commanding them).

## Document identities

| Doc | Identity / status | Role |
|---|---|---|
| SunSpec-Device-Information-Model-Specificiation-V1-4.txt | Device Information Model Spec v1.4, Approved 12-10-2025 (v1.3 2025-10-15 added FC6 mandate, RTU broadcast, removed 1000 ms readback delay) | Base register-map rules: SunS marker, chaining, types, NaN, SF, M/O |
| SunSpec-DER-Information-Model-Specification-V1-2.txt | DER Information Model Spec v1.2 — defines the 7xx series, models **701–714** | New IEEE 1547-2018 model series |
| SunSpec-Alliance-Specification-Energy-Storage-ModelsD4rev0.txt | Energy Storage Models, Draft 4 rev 0 (models 802–805, 807 at TEST; 806/808/809 deferred to Draft 5; 801 was consolidated INTO 802 and no longer exists) | 80x battery models |
| SunSpec-Technology-Overview-20220301.txt | Technology Overview v1.6, Approved 2022-03-01 | Common→Standard→Vendor→End structure, vendor-model rules |
| SunSpec-Modbus-FactSheet-RevA-2019-07-web.txt | Fact sheet Rev A, July 2019 | SunSpec Modbus = one of three IEEE 1547-2018 communication interfaces (with IEEE 2030.5 and IEEE 1815/DNP3) |
| SunSpec Modbus Vendor Extensions.txt | Vendor-extension model-ID registry (informative web note) | Reserved vendor ID ranges 64001–64910 |
| Also drawn on (same folder, directly on-point): SunSpec-Modbus-IEEE-1547-2018-Profile-Specification-and-Implementation-Guide-v1.1-1.txt (v1.1, 2024-02-15) — the normative "which points a 1547 DER must implement" profile; SunSpec-Modbus-Client-Conformance-Test-Procedures-v1-1.txt — what a certified CLIENT must do; SunSpecUL1741SARule21ImplemenationGuideD6.txt (D6, 2017) — legacy 12x/13x usage. |

## Register-map/base rules a client must implement (Device Info Model v1.4 §6)

**Map location & marker**
- Map lives in the **holding-register** space only; base address is one of exactly three
  0-based addresses: **0, 40000 (0x9C40), or 50000 (0xC350)**. Client discovery: probe all
  three until the marker is found (spec §6.2 step 1; client cert test CLI-4 runs all three).
  A map starting at 40001 is NONCOMPLIANT and the client should detect/log that (ERR-1).
- First two registers at base = well-known marker **0x5375 0x6E53** ("SunS").

**Model chaining / discovery**
- Models are placed **contiguously** immediately after the marker: Common (1) first, then
  standard/vendor models in ANY order, terminated by the **End Model: ID=0xFFFF, L=0**
  (2 registers). No gap registers between points or between models.
- Every model starts with two uint16 registers: **ID** then **L** (length = number of
  registers REMAINING after the L register, i.e. excluding ID+L). Discovery loop: read
  ID+L, next model = addr(L-register)+1+L, repeat until ID==0xFFFF.
- L is per-INSTANCE and varies with repeating groups — never assume a fixed length for
  models with repeating blocks (703/705/707…/803/804/805/807); always advance by the
  read L, and use L (plus count points like NPt/NCrv/NStr) to size repeats.
- Unknown model IDs MUST be skipped gracefully via ID/L (client cert ERR-3: connect fine,
  exclude the unknown model from the discovered list). Duplicated model IDs can appear
  and are legal (client tests inject duplicates deliberately).
- A model instance MUST expose registers for **all** points in the definition, including
  optional/unsupported ones (filled with the unimplemented sentinel) — so offsets within
  a model are fixed by the definition; order of points/groups is preserved, points before
  groups, repeated instances consecutive.

**Modbus functions**
- Server MUST support FC **3** (read holding), FC **6** (write single), FC **16** (write
  multiple); RTU devices MUST support broadcast (Unit ID 0). A certified client is tested
  writing via BOTH FC6 and FC16 (WR-1/WR-2), reading points singly and in bulk, and
  splitting reads when a model exceeds the Modbus 125-register read limit.
- Big-endian register/byte order, two's-complement signed.

**Unimplemented / NaN sentinels (per point type)** — client MUST map these to "absent":
| Type | Not-implemented value |
|---|---|
| int16 | 0x8000 (range −32767…32767) |
| uint16 / enum16 / bitfield16 | 0xFFFF (bitfield16 valid range only to 0x7FFF) |
| acc16/32/64 | 0 = "not accumulated" (accumulators have NO 0xFFFF sentinel) |
| int32 | 0x80000000 |
| uint32 / enum32 / bitfield32 | 0xFFFFFFFF |
| int64 | 0x8000000000000000 · uint64: 0xFFFF…FF |
| ipaddr / ipv6addr | 0 = not configured |
| string | all-NULL registers (empty string recommended as first reg 0x0080) |
| float32 | 0x7FC00000 (NaN) · float64: 0x7FF8000000000000 |
| sunssf | 0x8000 (valid range −10…+10) |
| pad | always reads 0x8000 |
- A point may flip between valid and unimplemented **at any time** during operation.
- READING an unimplemented register returns the sentinel (never an exception); WRITING an
  unimplemented, invalid-value, or read-only register MUST raise Modbus exception 2, 3, or 4.

**Scale factors (sunssf)**
- Integer value × 10^SF; SF is either a constant or another point in the SAME model
  (e.g. 701.W scaled by 701.W_SF). Scale-factor point values MUST be **static** — read
  once per model discovery is legal; do not expect per-poll changes.

**Mandatory vs optional**
- Point attribute `mandatory` defaults to **optional**. Mandatory points MUST always hold
  a valid (non-sentinel) value; optional points may read as unimplemented. In the DER
  spec, "mandatory" = "model is non-functional without it" — jurisdictional requirements
  (IEEE 1547, Rule 21…) are layered on top via profiles (see 1547 profile column below).
- `access` defaults to read-only; RW marks writable points. If a writable point supports
  only ONE value, it must still accept writing that value (never be implemented as R-O).
- **sync groups** (e.g. 704's PF+Ext pairs) MUST be read/written atomically — the client
  must write PF and excitation in one FC16 transaction.

**Write verification**
- Read-after-write MUST return the values of the last successful write — but that only
  confirms information exchange, **not** that the device's operational behavior changed
  (v1.3 removed the old 1000 ms readback delay). This is the spec-level basis for the
  hub's "trust measurement, not the command" convergence checks: an echoed setpoint is
  NOT evidence of action.

## Model catalog (M/O column = required for a 1547 DER per the SunSpec Modbus IEEE 1547-2018 Profile v1.1)

DER Info Model v1.2 defines exactly **fourteen** models, 701–714. (Corrects the task
prompt's guesses: 708 = Trip **HV**, 709 = Trip **LF**; frequency-watt is **711**;
watt-var is **712**; constant-PF lives inside **704**, not a dedicated model.)

| ID | Name | Purpose | Key points | Spec-mandatory points | 1547-profile-required |
|---|---|---|---|---|---|
| 1 | Common | Identity — always first after SunS | Mn, Md, SN, Vr, DA | all per Device spec | Yes: ID,L,Mn,Md,SN,Vr |
| 701 | DERMeasureAC | AC measurements + status/alarms (not latched) | W, VA, Var, PF, A, LLV/LNV, per-phase V/W/A, Hz, TotWhInj/Abs, TotVarhInj/Abs, temps, St, InvSt, ConnSt, Alrm (bitfield32), DERMode, ThrotPct/ThrotSrc, 10 SF points, MnAlrmInfo | ID, L, **ACType** only | ID,L,W,Var,LLV,LNV,VL1L2,VL1,VL2L3,VL2,VL3L1,VL3,Hz,St,ConnSt,Alrm (applicable voltage points) |
| 702 | DERCapacity | Nameplate ratings (R, static) + adjustable settings (RW) that override them | WMaxRtg, VAMaxRtg, VarMaxInj/AbsRtg, W(Dis)ChaRteMaxRtg, VNom/VMax/VMinRtg, AMaxRtg, PFOvr/UndExtRtg, NorOpCatRtg (CAT_A/B), AbnOpCatRtg (CAT I/II/III), **CtrlModes** (supported-function bitfield), IntIslandCatRtg; RW twins WMax, VAMax, VarMaxInj/Abs, VNom… | ID, L only | Required: all *Rtg listed + CtrlModes + ReactSusceptRtg; Optional: the RW setting twins + IntIslandCat |
| 703 | DEREnterService | Permit-to-energize + re-energize conditions; ES=DISABLED while energized ⇒ cease + trip | ES (RW enum), ESVHi/Lo, ESHzHi/Lo, ESDlyTms, ESRndTms, ESRmpTms, ESDlyRemTms, V_SF, Hz_SF | ID, L only | Required: all except ESRndTms (optional) |
| 704 | DERCtlAC | Immediate controls: constant PF (inj/abs sync pairs), limit max W %, set W, set var, ramps, anti-islanding; per-control reversion timers | PFWInjEna + PFWInj{PF,Ext} sync, PFWAbs…, WMaxLimPctEna/WMaxLimPct, WSetEna/WSetMod/WSet(Pct), VarSetEna/Mod/Pri/VarSet(Pct), *Rvrt/*RvrtTms/*RvrtRem for each, WRmp/WRmpRef/VarRmp, AntiIslEna, 6 SFs | ID, L only | Required: PFWInjEna, PF_SF, PFWInj.PF/.Ext, VarSetEna/Mod/Pri/VarSetPct+SF, WMaxLimPctEna/WMaxLimPct+SF |
| 705 | DERVoltVar | Volt-var piece-wise curves (1547 voltage-reactive power) | Ena, AdptCrvReq/Rslt, NPt, NCrv, RvrtTms/Rem/Crv, V_SF, DeptRef_SF, RspTms_SF; per-curve: ActPt, DeptRef, Pri, VRef, VRefAuto(Ena/Tms), RspTms, ReadOnly, Pt.V/Pt.Var | ID,L,Ena,AdptCrvReq,AdptCrvRslt,NPt,NCrv,V_SF,DeptRef_SF,RspTms_SF,Crv.ActPt,Crv.DeptRef,Crv.ReadOnly | Yes — incl. Crv.Pt[1-4].V/.Var (≥4 curve points), VRef, VRefAutoEna/Tms |
| 706 | DERVoltWatt | Volt-watt curves (1547 voltage-active power) | as 705 minus VRef/Pri; Pt.V/Pt.W | same pattern as 705 | Yes — Crv.Pt[1-2].V/.W (≥2 points) |
| 707 | DERTripLV | LOW-voltage trip/may-trip/momentary-cessation curve SETS | Ena, AdptCrvReq/Rslt, NPt, NCrvSet, V_SF, Tms_SF; Crv.{MustTrip,MayTrip,MomCess}.{ActPt,Pt.V,Pt.Tms}, ReadOnly | ID,L,Ena,AdptCrvReq,AdptCrvRslt,NPt,NCrvSet,V_SF,Tms_SF,Crv.ReadOnly | Required: MustTrip.ActPt + MustTrip.Pt[1-5].V/.Tms; MomCess optional; Ena may support ENABLED-only |
| 708 | DERTripHV | HIGH-voltage trip; same structure as 707 | as 707 | as 707 | as 707 |
| 709 | DERTripLF | LOW-frequency trip; Hz instead of V (Hz_SF) | as 707 with Pt.Hz | as 707 (Hz_SF) | MustTrip.Pt[1-5].Hz/.Tms; no MomCess (freq stds have none) |
| 710 | DERTripHF | HIGH-frequency trip | as 709 | as 709 | as 709 |
| 711 | DERFreqDroop | Frequency-watt / frequency-droop per IEEE 1547 terms — stored CONTROLS (not curves) | Ena, AdptCtlReq/Rslt, NCtl, RvrtTms/Rem/Ctl, Db_SF, K_SF, RspTms_SF; Ctl.{DbOf,DbUf,KOf,KUf,RspTms,PMin,ReadOnly} | ID,L,Ena,AdptCtlReq,AdptCtlRslt,NCtl,Db_SF,K_SF,RspTms_SF,Ctl.DbOf/DbUf/KOf/KUf/RspTms,Ctl.ReadOnly | Yes (all mandatory points; PMin not required) |
| 712 | DERWattVar | Watt-var curves (1547 active power–reactive power) | Ena, AdptCrvReq/Rslt, NPt, NCrv, Rvrt*, W_SF, DeptRef_SF; Crv.{ActPt,DeptRef,Pri,ReadOnly}, Pt.W/Pt.Var | ID,L,Ena,AdptCrvReq,AdptCrvRslt,NPt,NCrv,W_SF,DeptRef_SF,Crv.ActPt,Crv.DeptRef,Crv.ReadOnly | Yes — MUST support a 6-point curve (Pt[1-6]); unused first 3 points set to 0/ignored; Crv.Pri must accept REACTIVE |
| 713 | DERStorageCapacity | High-level storage capacity/status | WHRtg, WHAvail (=WHRtg·SoC·SoH), SoC, SoH, Sta (OK/WARNING/ERROR), WH_SF, Pct_SF | ID, L only | Model optional if no storage; if present, ID,L,SoC required (SoC=0 if no storage) |
| 714 | DERMeasureDC | DC measurements, multi-port (PV/ESS/EV/DC-DC repeating Prt group) | PrtAlrms, NPrt, totals DCA/DCW/DCWhInj/DCWhAbs; per-port PrtTyp, ID, IDStr, DCA/DCV/DCW, DCWhInj/Abs, Tmp, DCSta, DCAlrm | ID, L only | not in 1547 profile |

**Curve mechanics (705/706/707–710/712, DER spec §3.1; 711 same via "controls"):**
≥2 curve instances required; **curve index 1 = read-only ACTIVE settings**; writable
curves start at index 2. Update flow: write points into curve ≥2 → write that index to
AdptCrvReq → poll AdptCrvRslt for IN_PROGRESS/COMPLETED/FAILED → on COMPLETED, curve 1
reflects the new settings; on FAILED the old settings stay in force uninterrupted.
1547 profile: curve 1 R-O, curve 2 writable are MUSTs; extra curves MAY exist.

**Enable semantics (7xx, DER spec §3.3):** setting registers can be written any time but
take effect only while `Ena`=1 (ENABLED); if Ena is ALREADY 1, changes apply immediately —
**re-writing the enable is NOT required** in the 7xx series. Trip/droop models under the
1547 profile need not support DISABLED, but Ena must stay writable for ENABLED.

**Reversion timers (§3.2):** RvrtTms=0 or function disabled ⇒ timer Disabled; any setting
change / re-enable reloads and restarts the timer; on expiry the alternate (Rvrt*)
settings apply. A client that owns a control must refresh before RvrtRem hits 0 or plan
for the reversion values.

## 1xx/12x/2xx legacy series vs 7xx migration

**Legacy catalog** (from the retired Inverter Models spec; corroborated in these docs by
the UL1741 SA/Rule 21 guide D6 and the client-conformance server lists, which exercise
models 101, 121, 122, 123, 201, 202, 203, 213 alongside 7xx/80x):
- **1xx inverters**: 101/102/103 = single/split/three-phase inverter, integer+scale-factor;
  111/112/113 = same as float32. Measurements: A, per-phase V, W, Hz, VA, VAr, PF, WH,
  DCW, temps, St/StVnd, Evt1/Evt2 bitfields.
- **12x settings/controls family** (paired with a 1xx model): **120** Nameplate ratings,
  **121** Basic Settings (WMax, VRef, VRefOfs, WGra ramp…), **122** Measurements/Status
  extended, **123** Immediate Controls (Conn, WMaxLimPct+_Ena, OutPFSet+_Ena,
  VArWMaxPct/VArPct+_Ena — the points lexa's legacy OpModMaxLimW-style control path maps
  to), **124** Basic Storage (StorCtl_Mod, InWRte/OutWRte, ChaState…), **125** Pricing,
  **126** Static Volt-VAr arrays. UL1741 SA guide additionally uses **127** Freq-Watt
  Param, **128** Dynamic Reactive Current, **129/130** LV/HV ride-through curves (trip),
  **131** Watt-PF, **132** Volt-Watt, **134** Freq-Watt Curve, **135/136** LF/HF
  ride-through, **139–145** (proposed, UL1741-era gap fills).
- **2xx meters**: 201/202/203/204 = single-phase / split-phase / wye / delta,
  integer+SF; 211–214 = float variants. Points: per-phase A/V/W/VA/VAR/PF/Hz, energy
  accumulators (TotWhExp/Imp…), Evt.
- **8xx storage**: see next section.

**Why 7xx exists / migration guidance:**
- The 7xx series is the DER Info Model's ground-up redesign whose explicit goal is to
  "support all DER interoperability functionality specified in IEEE 1547-2018" — the
  SunSpec Modbus IEEE 1547-2018 Profile (Table 1) maps every 1547 clause to 7xx models
  ONLY: nameplate→702(+713), configuration→702, monitoring→701, constant PF→704,
  volt-var→705, watt-var→712, constant var→704, volt-watt→706, voltage trip→707/708,
  momentary cessation→707/708, frequency trip→709/710, freq-droop→711, enter
  service→703, cease-to-energize→703, limit max active power→704. **A 1547-2018
  certification cannot be met with 1xx+12x** — the UL1741 D6 guide already documented the
  gaps (model 127 lacked under-frequency and open-loop response time; 132 lacked snapshot
  curve adoption and sub-second times; hence the proposed 139–145 stopgaps, superseded by
  7xx).
- Functional equivalences a client audit should track: 101–103/111–113 → **701** (+714
  for DC); 120 → **702**; 121 → 702 settings + **703/704**; 123 → **704** (Conn→
  704 controls / 802 SetOp for batteries; WMaxLimPct→704.WMaxLimPct(+Ena); OutPFSet→
  704.PFWInj sync group; VArPct→704.VarSet*); 126→**705**; 131→**712**; 132→**706**;
  127/134→**711**; 129/130→**707/708**; 135/136→**709/710**; 124 storage control has no
  1:1 — 7xx splits it across 704 (W setpoints incl. negative=charge via int32 WSet),
  702 (charge/discharge rate settings), 713 (SoC).
- Key behavioral deltas legacy→new: (1) legacy 12x required REWRITING the enable point
  after any value change to latch new values (UL1741 guide "SunSpec Model Control
  Enable"); 7xx applies changes immediately while enabled. (2) Legacy curve models used
  in-place curve edit; 7xx uses the read-only-active-curve + AdptCrvReq copy protocol.
  (3) 7xx numeric points widely reuse uint16/int16 + per-model SFs but add uint32 (Hz,
  timers) and uint64 (energies). (4) Trip settings are piece-wise-linear region curve
  SETS (must/may/momentary) rather than the fixed (V,t) point pairs of 129/130/135/136.
- Both generations coexist on real fleets; certified clients are explicitly tested
  against servers mixing 1xx, 2xx, 7xx, 8xx and must handle either. Practical rule:
  discover, prefer 7xx when present, fall back to 1xx+12x.

## Storage model catalog (Energy Storage Models D4 — 80x)

Prereq stack for any SunSpec battery: Common(1) → **802 (mandatory for ALL battery
devices)** → technology model(s) → End(0xFFFF). All 80x models are padded to 64-bit
boundaries; 32-bit values aligned. All models except 802 optional, but an implemented
model must expose ALL its registers (unimplemented ⇒ sentinel).

| ID | Name | Status (D4) | Purpose / key points |
|---|---|---|---|
| 802 | Battery Base | TEST | Common to all chemistries. Nameplate: **WHRtg**, WChaRteMax/WDisChaRteMax, DisChaRte (self-discharge), SoCMax/SoCMin. Reserves (RW): SoCRsvMax/**SoCRsvMin** (the reserve-floor point behind lexa's reserve logic). State: **SoC**, DoD, **SoH**, NCyc, **State** (Disconnected/Initializing/Connected/Standby/SoC-Protection/Fault), Typ, Evt1 bitfield + **AlmRst** (write 1 to clear latched alarms; device returns it to 0). Measurements: V, A, W, CellVMax/Min (+string/module locations). Dynamic limits: **AChaMax/ADisChaMax, VMax/VMin** — at least one pair must be implemented; controller MUST keep charge/discharge inside them (informational bounds, not the protection layer). Control: **SetOp** (connect/disconnect), SetInvState; battery→controller requests ReqInvState/ReqW during Initializing. LocRemCtl (1 ⇒ local mode, remote commands refused). Heartbeats Hb/CtrlHb (optional, 1 s increment). |
| 803 | Lithium-ion Battery Bank | TEST | Bank-level: ModTmpMax/Min (+locations), StrVMax/Min, StrAMax/Min; repeating **string block** (count NStr): StrSoC, StrSoH, StrCellVMax/Min, StrModTmpMax/Min, StrNMod, StrSt, StrConFail, StrDisRsn (NONE/FAULT/MAINTENANCE/EXTERNAL/OTHER), **StrSetEna** (1=enable/2=disable; readback = in-progress state), **StrSetCon** (connect/disconnect one string; must honor enable state). Disabled strings are excluded from bank SoC. |
| 804 | Lithium-ion Battery String | TEST | Per-string detail: Idx (1-based), A, V, SoC, DoD, SoH, NCyc, NCellBal, min/max cell V/temp/SoC/SoH; repeating **module block** (NMod): ModNCell, ModSoH, ModCellVMax/Min/Avg (+cell locations), ModCellTmpMax/Min/Avg. One 804 instance per string. |
| 805 | Lithium-ion Battery Module | TEST | Per-module: Idx, SN, NCellBal, NCyc, aggregates as 804; repeating **cell block**: CellV, CellTmp, CellSt — the only cell-level model. |
| 806 | Flow Battery Bank | Draft 5 — NOT defined in D4 | placeholder |
| 807 | Flow Battery String | TEST | NMod/NModCon, ModVMax/Min (+loc), CellVMax/Min (+module/stack loc), TmpMax/Min (electrolyte), StrEvt1 (superset of 802.Evt1 + flow alarms e.g. HIGH_PRESSURE_ALARM — read StrEvt1 as authoritative); repeating module block. |
| 808 | Flow Battery Module | Draft 5 — NOT defined in D4 | placeholder |
| 809 | Flow Battery Stack | Draft 5 — NOT defined in D4 | placeholder |
| 810+ | — | Not defined by this spec (no model 810 exists in D4) | — |

**Repeating-block math (803/804/805/807):** `L = fixed + (repeat_size × N_allocated)`;
the count register (NStr/NMod) may be **less** than the allocated repeats (space reserved
for expansion) — spare repeat instances read all-sentinel. Client must size by L, gate by
count, and tolerate sentinel-filled spares.
**Address-space caveat:** full 802+803+804+805 for a large bank can exceed the 65,535-
register Modbus map; vendors may split across multiple Unit IDs, each with its own
Common model — a client must not assume one Unit ID per physical battery.
**RW caveat:** storage-model "settings" marked RW are permitted to be read-only in a
given implementation (fixed or not settable) — write failures on 80x RW points are
conformant behavior, take the exception path, don't retry forever.
**Typical li-ion bank map (D4 Table 3):** 40001 SunS · 40003 Model 1 (L=66) · 40071
Model 802 (L=62) · 40135 Model 803 (L=26+28/string) · … · End.
(NOTE: 713 vs 802 — a 1547-profile inverter with storage exposes 713 SoC; a standalone
battery/BMS exposes 802+. lexa's battery poller should expect either, per device.)

## Vendor extension rules

- Vendor models MUST conform to the same model-definition rules (ID/L header, sentinel
  values, no interference with discovery of standard models) and require a
  **SunSpec-assigned model ID** (Technology Overview §5.3); they sit between the standard
  models and the End model in the chain (position not actually mandated — any order).
- Reserved vendor ID ranges (Vendor Extensions note): **64001–64899** assigned in blocks
  of ~10 to specific vendors (Veris 64001-, SunSpec 64010-, Mersen, LSIS, REFU, Ampt,
  Delta, Outback 64110–64139, ABB 64230-, Sungrow 64310-, SMA 64870-, LG 64880-, Goodwe
  64860-, Enphase 64430-, etc.); **64900–64910** = SunSpec private-experimentation range,
  never assigned. Model IDs are uint16 1–65535 overall; 0xFFFF reserved for End.
- Client tolerance obligations: ANY unknown model ID (vendor or future-standard) must be
  skipped via the ID/L chain without aborting discovery (cert test ERR-3); published
  vendor models should be usable if definitions are available, but certification only
  scores standardized models.

## Client-side obligations summary (what a compliant SunSpec Modbus client — i.e. lexa-modbus — must do)

1. **Discovery**: probe base 0/40000/50000 for "SunS"; walk ID/L chain to 0xFFFF; accept
   arbitrary model order, duplicate models, unknown models (skip, don't abort); log/flag a
   non-SunSpec server (e.g. map at 40001). Re-derive per-model point addresses from the
   model definition + discovered base; never hardcode absolute registers.
2. **Transport**: FC3/FC6/FC16; split reads >125 registers; survive partial Modbus
   responses and TCP-segmented responses (recover, stay functional); handle exceptions
   1–4 (log, continue) — expected on writes to R-O/unimplemented/invalid-value registers.
3. **Decoding**: big-endian multi-register assembly (u/int32/64, float32/64, strings
   NULL-padded UTF-8); apply sunssf scale factors (static — cacheable); treat every
   type-specific sentinel as "unimplemented", including mid-run transitions; acc* types:
   0 = not-accumulated, no 0xFFFF sentinel — accumulator rollover is the client's problem.
4. **Writes**: honor sync groups atomically (704 PF+Ext); write-then-readback confirms
   transport only — convergence must be judged from MEASUREMENTS (701/1xx/2xx telemetry),
   which is exactly the hub's checkGenLimitConvergence/reconciler posture; expect RW
   points that accept only one value or are effectively read-only (80x).
5. **Control protocols**: 7xx enable semantics (no enable-rewrite needed) vs legacy 12x
   (enable rewrite REQUIRED to latch) — per-series handling; curve adoption via
   AdptCrvReq/AdptCrvRslt polling with curve 1 read-only; respect reversion timers
   (refresh or accept fallback); battery: honor LocRemCtl, keep commands inside dynamic
   AChaMax/ADisChaMax/VMax/VMin, drive connect/disconnect through SetOp + State machine
   (Disconnected→Initializing→Connected; Fault cleared via AlmRst), honor ReqW during
   Initializing, exclude disabled strings from SoC reasoning.
6. **Mandatory-point posture**: only spec-M points are guaranteed valid; anything else can
   be sentinel on any given device even in a certified 1547 DER — the 1547 profile column
   above is the correct "should exist on a compliant inverter" checklist, the spec-M
   column the correct "may I hard-require this" floor (701: only ACType!).
