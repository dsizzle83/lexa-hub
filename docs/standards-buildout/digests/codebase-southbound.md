# lexa-hub southbound + orchestrator capability inventory (standards-coverage audit)

Repo: /home/dmitri/projects/lexa-hub @ main (d6ac263, 2026-07-14). All paths below are repo-relative.

## Sources examined

- `vendor/lexa-proto/sunspec/` — models.go, scanner.go, reader.go, scale.go, identity.go,
  der1547.go, derlayout.go, layout.go, bussweep.go, sweep.go
- `vendor/lexa-proto/derbase/derbase.go` (CSIP→SunSpec write mapping, 785 lines)
- `vendor/lexa-proto/modbus/transport.go`; `vendor/lexa-proto/ocppserver/{server,handlers}.go` + CLAUDE.md
- `vendor/lexa-proto/csipmodel/der.go` (DERControlBase / ExtendedDERControlBase)
- `internal/southbound/{device,inverter,battery,meter,registry}` + southbound CLAUDE.md
- `cmd/modbus/` — main.go, interlock.go, scan.go, reconcile_shell.go, reconcile_solar.go, config.go
- `cmd/ocpp/` — main.go, config.go, reconcile_shell.go, pending.go
- `cmd/hub/` — desired.go, mode.go, intent.go, tariff.go, state.go (grep-level)
- `internal/orchestrator/` — model.go, interfaces.go, optimizer.go (grep), planner.go, costmodel.go,
  engine.go (grep), plantmodel.go; `internal/orchestrator/constraint/` (passthrough.go + file list)
- `internal/reconcile/reconcile.go`; `internal/bus/{messages,desired,intent,topics,scan}.go`
- `internal/northbound/{publish,schedule,discovery}` (grep for curve/extended-control flow)
- docs: `docs/extension/00_PROGRESS.md`, `docs/DEVICE_ROADMAP.md`, `docs/ECOSYSTEM_ROADMAP.md`,
  `docs/V1RC_FINDINGS.md`, repo CLAUDE.md. (`10_BACKLOG.md` referenced by CLAUDE.md does NOT exist in
  this repo — it lives in the planning corpus elsewhere; only its SP3 item is quoted in CLAUDE.md.)

## SunSpec model coverage table

Discovery: `SunS` marker at base 40000 + `[ID][Len]` chain walk to 0xFFFF end marker —
`vendor/lexa-proto/sunspec/scanner.go:22-53` (`Scan`, IDs/lengths only, no data burst; **only base
40000 is tried** — the spec-permitted 0/50000 alternates are noted but not probed,
`models.go:54-58`). Cached layout → `Reader.ReadModel/WriteModel` by 0-based offset
(`reader.go:41-70`). Bus-level commissioning sweep: `SweepTCP` (CIDR × unit IDs) / `SweepRTU`
(bauds × unit IDs) `bussweep.go:143,262`, driven by the cmd/modbus scan controller
(`cmd/modbus/scan.go:1-42`, armed only when the fleet is uncommissioned/reconcilers off; classify
precedence 801/802→battery, 201-203→meter, 101-103/701-714→inverter).

Scale factors: `ApplyScaleUint/Signed` with 0x8000 (int16 −32768) "not implemented" sentinel → NaN;
write-side `RawFromScale*` rounds + clamps to int16/uint16 range (`scale.go:7-63`). Register-wrap
class is regression-swept (`sweep.go:47-295`, mirrored in `internal/southbound/sunspecsweep/`).
Service-layer plausibility gate: decoded |W| > nameplate×1.2 is withheld from the bus
(`cmd/modbus/main.go:486-501`).

| Model | Read/Write | Status | Evidence |
|---|---|---|---|
| 1 Common | R | **Full** (Mn/Md/SN/Vr decode) | `sunspec/identity.go:9-70` (`ReadCommon`); used by bus sweep `bussweep.go:57` |
| 101/102 single/split-ph inverter | R | **Partial** — fallback measurement chain only; parsed with the M103 offset table | `derbase.go:86-94` (chain 103→102→101), `derbase.go:164-199` |
| 103 three-ph inverter | R | **Full read** of W/V/Hz/VA/VAr/PF/DC/Tmp/St | offsets `models.go:61-104`; parse `derbase.go:164-199`; status `inverter.go:87-94` |
| 111-113 (float inverter) | — | **Absent** — no constants, no parse | grep of `vendor/lexa-proto/sunspec` |
| 120 Nameplate | — | **Declared only** — full offset table exists but nothing reads it (WMax comes from 121/702) | `models.go:106-136`; no `ReadModel(ModelNameplate)` caller |
| 121 Basic Settings | R | **Partial** — WMax only | `models.go:152-156`, `derbase.go:759-773` (`ReadWMax`) |
| 122 Extended Status | — | **Declared only** — offsets (PVConn/StorConn/ECPConn/ActWh/WAval) named for the sim; hub never reads them | `models.go:138-150` |
| 123 Immediate Controls | R/W | **Partial write** — WMaxLimPct(+Ena,+RmpTms) and Conn only; OutPFSet / VArPct offsets declared but never written | `models.go:287-313`; writes `derbase.go:705-757` (`SetConnect`, `setLegacyWMaxLimPct`) |
| 124 Storage Controls | — | **Absent** (no constant) — battery dispatch rides 123 WMaxLimPct sign trick / 704 WSet instead | `derbase.go:724-757` |
| 125/126/127/128 (pricing, legacy curves) | — | **Absent**; roadmap names "legacy 126/127/128" as future curve work | `docs/ECOSYSTEM_ROADMAP.md:788-794` |
| 201 meter single-ph | R | **Full read** W/V/Hz/VA/VAR/PF (+TotWhExp/Imp offsets declared, not parsed) | `models.go:210-236`; `meter.go:131-196` |
| 202 meter split-ph | R | **Partial** — W/V/Hz only in parse | `models.go:238-253`; `meter.go:155-162` |
| 203 meter three-ph wye | R | **Full read** W/V/Hz/VA/VAR/PF; preferred model (203>202>201) | `models.go:255-285`; `meter.go:71-87` |
| 204 (delta) / 211-214 (float meters) | — | **Absent** | grep |
| 701 DER AC Measurement | R | **Full** — typed `Parse701` (St/InvSt/ConnSt/Alrm/DERMode/W/VA/Var/PF/A/V/Hz/energies/throttle) ; preferred over 103 | `der1547.go:40-89`; `derbase.go:83-95,153-161` |
| 702 DER Capacity | R + one W | **Read full** (ratings + RW settings); write = `SetCapacityWMax` demo only, no live caller | `der1547.go:91-134`; `derbase.go:410-433,775-785` |
| 703 Enter Service | R/W | **Implemented** — full block encode + ES-bit toggle; live path only via CSIP `opModEnergize` (which nothing sends today) | `der1547.go:136-175`; `derbase.go:279-311`; live gate `derbase.go:208-212` |
| 704 DER AC Controls | R/W | **Implemented at lib level**: WMaxLimPct%, WSet (±W, RvrtTms), PFWInj/PFWAbs sync groups, VarSet; whole-block RMW keeps PF groups atomic. **Live pipeline uses only WMaxLimPct + WSet-negative(import)** | `derbase.go:313-406`; `derlayout.go:113-158`; live mapping `derbase.go:207-266` |
| 705 Volt-Var | R/W | **Lib-complete, unwired** — curve parse/encode + §3.1.2 adopt handshake (write staging → AdptCrvReq=2 → poll AdptCrvRslt → Ena=1); no non-test caller | `derbase.go:442-511`; callers: only `inverter.go:115-120`/`battery.go:156-161` wrappers, nothing above them |
| 706 Volt-Watt | R/W | **Lib-complete, unwired** (same adopt flow) | `derbase.go:513-539` |
| 707/708 V-trip LV/HV | R/W | **Lib-complete, unwired** (3 curves/set) | `der1547.go:381-457`; `derbase.go:541-580` |
| 709/710 F-trip LF/HF | R/W | **Lib-complete, unwired** | `derbase.go:582-621` |
| 711 Freq Droop | R/W | **Lib-complete, unwired** (AdptCtlReq/Rslt variant) | `derbase.go:623-649` |
| 712 Watt-Var | R/W | **Lib-complete, unwired** | `derbase.go:651-677` |
| 713 Storage Capacity | R | **Implemented + live**: preferred SoC/SoH/capacity source, 802 fallback | `derbase.go:679-690`; `battery/metrics.go:14-46` |
| 714 DER DC Measurement | R | **Implemented, read-only, no live caller** (per-port decode) | `der1547.go:666-712`; `derbase.go:692-701` |
| 801 battery base | — | Constant only (classify hint in scan) | `models.go:23`; `cmd/modbus/scan.go:35-41` |
| 802 Li-Ion battery | R | **Full read** — SoC/SoH/DoD/rates/ChaSt/State; status source when no 701 | `models.go:158-189`; `battery.go:101-117`; `battery/metrics.go` |
| 803-810 (battery bank/string/flow) | — | **Absent** | grep |

Transports: Modbus **TCP, RTU, RTU-over-TCP** via URL scheme (`vendor/lexa-proto/modbus/transport.go:5-13`);
explicit-baud serial constructor for sweeps (`transport.go:65-78`). Device classes wired in cmd/modbus:
`inverter|battery|meter` only (`cmd/modbus/main.go:342-353`). Holding-register R/W + input-register R
(`transport.go:24-37`); no coils/discretes (SunSpec doesn't need them).

## IEEE 1547-2018 clause-10 mode coverage

Pipeline pinch point: the CSIP walker parses the full `ExtendedDERControlBase` (all scalar + curve
modes, `csipmodel/der.go:218-265`), but the retained actuated bus doc `bus.ActiveControl` carries
ONLY `Connect/ExpLimW/ImpLimW/MaxLimW/FixedW` (`internal/bus/messages.go:46-58`; adopted at
`cmd/hub/state.go:824-836`). Curve/PF/var content survives only as display metadata on
`lexa/northbound/schedule` (`DERScheduleMsg` slots: FixedVarPct, FixedPF*, curve summaries —
`internal/northbound/publish/publish.go:95-118`, `internal/bus/messages.go:303-386`), which
`cmd/hub/main.go:1065` distills back to scalar `StepConstraint{Disconnect,ExpLimW,ImpLimW,MaxLimW,FixedW}`
(`planner.go:101-108`) for the planner, and lexa-api shows read-only.

| 1547 §10 function | Read (monitor/nameplate) | Write path exists | Actuated live? |
|---|---|---|---|
| Nameplate / config (§10.4) | Yes — 702 full, 701 status, 713 storage, 121 WMax fallback | 702 WMax override (`SetCapacityWMax`) | Read: yes (WMax at init `derbase.go:97-105`); write: no caller |
| Monitoring (§10.5) | Yes — 701 W/Var/V/Hz/PF/state/alarms; 802/713 SoC; meter 201-203 | n/a | Yes (poll → `lexa/measurements`) |
| Constant power factor | 704 PFWInj/PFWAbs parse | `SetFixedPF` (704, atomic sync group) via CSIP `opModFixedPF*W` in `ApplyControl` `derbase.go:221-232` | **No** — desired-doc layer never emits it; ActiveControl can't carry it |
| Volt-var (curve) | `ReadVoltVar` 705 | `WriteVoltVar` + adopt handshake | **No** — no caller above device wrapper; CSIP CurveLinks stop at schedule display |
| Watt-var | 712 R/W | `WriteWattVar` | **No** |
| Constant reactive power | 704 VarSet parse | `SetConstantVar` (VarMaxPct mode) via `opModFixedVar` `derbase.go:233-237` | **No** (same pinch point) |
| Volt-watt | 706 R/W | `WriteVoltWatt` | **No** |
| Freq-watt / freq droop | 711 R/W | `WriteFreqDroop` | **No** |
| Voltage/freq trip & ride-through | 707-710 R/W | `writeVoltageTrip`/`writeFreqTrip` | **No** — HFRT/LVRT etc. CurveLinks parsed (`csipmodel/der.go:246-259`) but never forwarded |
| Limit active power | 704 WMaxLimPct read-back | `SetWMaxLimPctW` (704) / M123 WMaxLimPct legacy — **the workhorse** | **Yes** — solar ceiling + battery exp/imp limit path (`cmd/modbus/main.go:446-478` → `derbase.go:244-264`) |
| Set active power (charge/discharge) | 704 WSet | `SetActivePowerWatts` ±WSet w/ RvrtTms clamp to ±WMax | **Yes** on 704 devices (battery import→ −WSet, `derbase.go:256-263`); legacy path uses signed 123 WMaxLimPct (`derbase.go:724-757`) |
| Connect / disconnect | 701 ConnSt, 103 St, 802 State | 123 Conn (`SetConnect`) | **Yes** — battery Connect in desired doc; Tier-0 interlock force-disconnect (`cmd/modbus/interlock.go:122-156`). Solar/EVSE Connect: doc-complete, **execution done for solar** (Unit 6.2 `reconcile_solar.go:41-48` OpModConnect write) but readback-blind; EVSE folds Connect→0 A (`cmd/ocpp/reconcile_shell.go:51-63`) |
| Enter service / cease-to-energize (§10.10.3) | 703 parse | `SetEnterService(Enabled)` | **No live sender** of `opModEnergize`; energize rides `opModConnect`/123 today |
| Scheduling (§10.9-adjacent) | DERSchedule → planner StepConstraints incl. per-slot Disconnect/FixedW | n/a | Yes (planner honors envelope) |

Reversion safety: `Base.DefaultRvrtTms` written on 704 WSet/WMaxLimPct writes (`derbase.go:44-47,
374,393`) — but no caller sets it (grep: zero non-test assignments), so autonomous device reversion
timers are effectively unused; loss-of-comms fallback relies on reconciler reassert + CSIP defaults.

## OCPP 2.0.1 message/feature coverage

CSMS = `vendor/lexa-proto/ocppserver` (wraps lorenzodonini/ocpp-go) + `cmd/ocpp` bridge/forwarders.

| Message / feature | Status | Evidence |
|---|---|---|
| BootNotification | **Implemented** — Accepted, 60 s heartbeat; vendor/model feeds pending-station surface | `cmd/ocpp/main.go:598-608` |
| Heartbeat | Implemented | `cmd/ocpp/main.go:617-620` |
| StatusNotification | Implemented → connector map → `lexa/evse/{station}/state` | `cmd/ocpp/main.go:622-638` |
| NotifyReport (B07) | Ack-only stub (log + empty response); no GetBaseReport requester | `cmd/ocpp/main.go:610-613` |
| MeterValues | Implemented; folds Current.Import / SoC / Energy.Active.Import.Register (unit multiplier) / Voltage; plausibility gate current > rating×1.25 rejected keep-last-good | `cmd/ocpp/main.go:640-716` |
| TransactionEvent Started/Updated/Ended | Implemented — lifecycle is the carrier (OCPP-1 invariant); Ended zeroes current (suspend convergence) | `cmd/ocpp/main.go:724-756`; `vendor/lexa-proto/ocppserver/CLAUDE.md` |
| SetChargingProfile (K01) | **Implemented (CSMS→CS)** — TxDefaultProfile, ChargingRateUnit=A, single period, 10 s bounded, delivered-but-Rejected = error (L11) | `cmd/ocpp/main.go:516-571` |
| TriggerMessage | Implemented — StatusNotification re-trigger on connect only | `cmd/ocpp/main.go:573-580` |
| ClearChargingProfile | **Absent** — release = re-Set a higher limit; no clear ever sent | grep cmd/ocpp + ocppserver |
| GetCompositeSchedule | **Absent** | grep |
| RequestStart/StopTransaction (F01/F02) | **Absent** — hub never remote-starts; suspend is 0 A profile | grep |
| Reset / UnlockConnector / ChangeAvailability | **Absent** | grep |
| Authorize / local auth list (C, D01-D02) | **Absent** — no authorization handler registered; any charger that connects transacts | `ocppserver/server.go:80-90` (only provisioning/availability/transactions/meter handlers set) |
| Reservation (H01 ReserveNow) | **Absent** (only mentioned in a doc comment) | `ocppserver/server.go:107` |
| Firmware mgmt (L01-L05) / diagnostics / GetLog | **Absent** | grep |
| Get/SetVariables, device model (B05-B06) | **Absent** | grep |
| Security profile 1 (HTTP Basic over ws) | Supported implicitly (BasicAuth w/o TLS possible) | `ocppserver/server.go:55-75` |
| **Security profile 2** (TLS + Basic Auth) | **Implemented & enforced as product default** — config refuses blank cert/key/user/pass unless `bench:true`/`OCPP_PROFILE=bench` (TASK-074/WS-1); constant-time credential compare | `ocppserver/server.go:55-75`; `cmd/ocpp/config.go:34-59` |
| Security profile 3 (mTLS client certs) | **Absent** — explicitly backlogged ("AD-008 scopes this task to ≥2, see 10_BACKLOG.md") | repo CLAUDE.md OCPP invariant |
| A05 CertificateSigned / InstallCertificate / security events (A, N) | **Absent** | grep |
| Smart-charging beyond K01: charging needs (K15-K17 NotifyEVChargingNeeds/Schedule), station-max/Tx profiles, composite | **Absent** — only TxDefaultProfile purpose used | `cmd/ocpp/main.go:532` |
| Offline behavior | CSMS side only: disconnect tracked, reassert-on-reconnect re-sends standing limit (`onConnect` → `sh.markReconnected` `cmd/ocpp/main.go:382-386`); no station-side offline auth semantics (no auth at all) |
| Pending/unknown stations | Auto-accepted but surfaced retained on `lexa/ocpp/pending` (Unit 6.1); **no pairing gate** — roadmap R8 flags any dialing charger becomes a plant | `cmd/ocpp/main.go:362-387`; `docs/ECOSYSTEM_ROADMAP.md:87` |

## OCPP 1.6 / ISO 15118 / V2G status

- **OCPP 1.6: absent.** Only `github.com/lorenzodonini/ocpp-go/ocpp2.0.1` imports exist
  (`cmd/ocpp/main.go:33-40`); grep for `ocpp1.6|ocpp16|v16` across repo returns nothing. A 1.6-only
  charger cannot connect.
- **ISO 15118: absent.** No Get15118EVCertificate / GetCertificateStatus / Plug&Charge / contract
  cert handling anywhere (grep). EV SoC arrives only via OCPP MeterValues `SoC` measurand
  (`cmd/ocpp/main.go:677-678`), i.e. depends on the charger reporting it.
- **V2G/V2X (bidirectional EVSE): absent by construction.** `EVSECommand.MaxCurrentA: 0 = suspend,
  >0 = limit` (`orchestrator/model.go:298-315`); charging profile always positive amperes
  (`cmd/ocpp/main.go:527-535`); planner models EV as load only (`planner.go:41-48` EVMaxChargeKw,
  no discharge term; `discretizeEVCurrents` `planner.go:782`). No OCPP 2.1, no negative setpoints.

## EVSE control path

Optimizer levers: per-connector current ceiling (A) incl. 0 A suspend; `Connect` opinion (gateway
fan-out, folded to effective 0 A at the reconciler — `cmd/ocpp/reconcile_shell.go:51-63`). Desired
doc `lexa/desired/evse/{station}` is the sole command path (TASK-032). Reconciler: convergence
judged ONLY from metered current, one-sided (under-limit compliant; Accepted ≠ convergence —
"ev-accept-but-ignore", `reconcile_shell.go:28-32,99-101`); corrective re-write backoff ≥15 s;
reassert-on-reconnect closes the legacy gap; non-convergence feeds CannotComply episodes
(retained `lexa/reconcile/evse/{station}/report`).

## Optimizer / orchestrator control-lever inventory

Levers actually actuated (all via retained AD-013 desired docs, `internal/bus/desired.go:27-65`):
1. **Battery setpoint W** (+discharge/−charge; explicit 0 = idle/reserve-hold) + **Connect** —
   `BatteryCommand` `model.go:256-270`; executed as 123 WMaxLimPct-signed / 704 WSet via
   `battCommandToControl` (`cmd/modbus/main.go:446-460`).
2. **Solar generation ceiling W** (explicit restore = 1e9 clamp-to-WMax, never absence) —
   `SolarCommand` `model.go:273-295`; `solarCommandToControl` → OpModMaxLimW
   (`cmd/modbus/main.go:464-482`); one-sided over-ceiling divergence only.
3. **EVSE current limit A / 0 A suspend** (above).
4. **Grid caps honored, not commanded**: CSIP ExpLimW/ImpLimW/MaxLimW/FixedW/Connect distilled to
   `GridState` limits (`model.go:157-172`) and enforced by cascade rules.

Rule cascade (authoritative live path, `optimizer.go:325-440`): csipDisconnectRule (grid-event
shed: disconnect batteries, curtail solar, suspend EVSEs, `optimizer.go:488-533`) →
applyFixedDispatchRule (CSIP FixedW) → applyPlanRule (follow 24 h DP target) → applyExportLimitRule
(battery absorb → solar curtail; convergence checks) → applyGenLimitRule + meter-floor
checkGenLimitConvergence (`optimizer.go:1349,1397` — HARD preserve) → applyImportLimitRule
(+checkImportConvergence `optimizer.go:1499,2027`) → applySelfConsumptionRule → peak
applyDemandResponseRule (TOU discharge) → applyEVChargingRule (solar-surplus boost / import caps)
→ applyRestoreRule. Breach → `ComplianceBreach` → CannotComply episode (`cmd/hub/breach.go`).

Safety tiers (ADR-0001): Tier-0 cmd/modbus interlock (charge-commanded pack discharging at/below
reserve+5% → local force-disconnect within one poll, senior to reconciler,
`cmd/modbus/interlock.go:20-156`); Tier-1 `EvaluateSafety` fast loop (criticalBatteryInversion
only, `optimizer.go:1567-1618`), mode-invariant even in gateway mode (`cmd/hub/mode.go:15-24`);
Tier-2 economic-tick `checkBatterySafety` debounced.

Constraint stack (`internal/orchestrator/constraint/`): Export/GenLimit/ImportLimit/BatterySafety
constraints ported and running SHADOW-only behind `constraint_shadow` (TASK-059/060/061/062);
legacy cascade stays authoritative until per-axis `active` flips. Gateway mode (Unit 3.4/3.5):
`modeManager` can swap the plan author to `CSIPPassthrough` — pure CSIP forwarding + scheduled/full
EVSE window policy (`constraint/passthrough.go:9-60`) — the seed of grid-services-first operation.

Economics: TOU cost model (hour-of-local-zone, DST-correct; zone-mismatch startup assertion WS-8
`cmd/hub/tariffzone.go`) `costmodel.go:8-137`; 24 h DP planner, 288×5-min slots, SOC-discretised,
battery + EV assets, per-step import/export prices, volumetric delivery adder + fixed daily charge
(`planner.go:14-108`), receding-horizon replan; DER schedule envelope per slot. App/cloud intents:
mode, evgoal (target kWh + departure), reserve (raise-only), tariff (compiled to supply+delivery
TOU, USD-only, `cmd/hub/tariff.go:49-76`), solarforecast, loadprofile, chargenow (TTL edge)
(`internal/bus/intent.go`, `internal/bus/topics.go:290-318`).

Room for grid-services additions (judgment): reactive-power/PF levers, volt-var/volt-watt/freq-watt
curve passthrough, and trip-curve provisioning all have finished SunSpec write machinery
(704/705-712 + adopt handshake) and CSIP parse; what's missing is (a) fields on `bus.ActiveControl`/
`DesiredState`, (b) optimizer/gateway rules to author them, (c) reconciler execution + convergence
semantics per axis. Frequency droop for FFR-style services likewise lib-ready.

## Known-deferred items (from docs/boards)

- **Grid-support curve passthrough** (CSIP DERCurves → SunSpec 705/706/711 writes): explicitly
  "genuinely new southbound work… gateway-mode-for-power-envelopes first, curves second (a milestone
  of its own)" — `docs/ECOSYSTEM_ROADMAP.md:786-794,996`.
- **OCPP Security Profile 3 (mTLS)**: backlogged; AD-008 scoped TASK-074 to "≥2" — repo CLAUDE.md.
- **OCPP pairing gate (R8)**: unknown chargers auto-adopted (surfaced but accepted) —
  `docs/ECOSYSTEM_ROADMAP.md:87`.
- **Solar/EVSE hard disconnect execution**: desired docs carry Connect for all classes; solar
  OpModConnect write landed Unit 6.2 but is readback-blind (no Status() plumb —
  `cmd/modbus/reconcile_solar.go:52-70`); EVSE deliberately folds Connect→0 A rather than a
  device-level disconnect — `cmd/hub/mode.go:33-47`, `docs/extension/00_PROGRESS.md:66`.
- **Plant model unwired**: per-device `"plant"` physical-response params parse but nothing reads
  them until TASK-064 — repo CLAUDE.md (`plantmodel.go`).
- **modbus.json ships no journal block** → scan_run journaling no-op —
  `docs/extension/00_PROGRESS.md:445-446`.
- **Cert-rotation 24 h soak** pending (northbound, not southbound) — repo CLAUDE.md TASK-073.
- **Meter-segment scan overlap** (v1 arming rule doesn't pause live meter polling) —
  `cmd/modbus/scan.go:24-36`.
- **EVGoal capacity field**: `orchestrator.EVGoal` has no capacity plumb (user-stated pack size
  accepted but partially unused) — `cmd/hub/intent.go:249`.

## Notable absences (standards-coverage holes)

1. **No reactive-power or PF control is ever actuated** — 1547 volt-var, watt-var, constant-PF,
   constant-Q all dead-end at `bus.ActiveControl`'s 5 scalar fields
   (`internal/bus/messages.go:46-58`), despite complete 704/705/712 write machinery.
2. **No curve/trip provisioning path** (705-712 unwired); no legacy 126-128 either — a Rule-21/
   1547 full-function DER gateway claim rests today on the inverter's own UL1741 defaults.
3. **OCPP surface is minimal-viable smart charging**: no Authorize/auth lists, no
   ClearChargingProfile/GetCompositeSchedule, no remote start/stop, no Reset, no device model
   (Get/SetVariables), no firmware/security-event handling, single TxDefaultProfile purpose.
4. **No OCPP 1.6, no ISO 15118, no V2G/bidirectional EVSE** (planner and command types are
   charge-only by construction).
5. **SunSpec float-model families (111-113, 211-214) and battery-bank models (803-810) absent**;
   header only probed at base 40000; model 120/122 declared but unread; meter energy accumulators
   (TotWhImp/Exp) parsed nowhere → no revenue-grade energy from the meter, only power.
6. **No reversion-timer usage** (DefaultRvrtTms never set) — loss-of-comms device autonomy relies
   on reconciler reassert rather than device-side RvrtTms.
7. **Meters are read-only and reconciler-less** (correct per spec but means no meter-level
   plausibility beyond nameplate gate; no per-phase data surfaced onto the bus — `bus.Measurement`
   carries W/V/Hz(/SOC) only, `internal/bus/messages.go:21-30`).
8. **Frequency/voltage from the meter are read but no autonomous freq-watt/volt-watt response
   exists in the hub** — grid frequency rides `GridState.FrequencyHz` into snapshots and is never
   consulted by any rule (grep `FrequencyHz` in optimizer.go: no hits).
