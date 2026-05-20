// Package sunspec provides SunSpec model discovery and register access on
// top of a Modbus transport. The SunSpec Alliance defines standardized
// Modbus information models (github.com/sunspec/models) supported by most
// grid-tied inverters and batteries manufactured after ~2018.
//
// Entry point: NewReader(transport) scans the device's SunSpec block layout
// then provides ReadModel / WriteModel helpers so callers work with 0-based
// offsets within a named model, not raw Modbus addresses.
package sunspec

// Well-known SunSpec model IDs referenced by this package.
// See the SunSpec Alliance model registry for the full list.
const (
	// ── Legacy / transitional models (pre-IEEE 1547-2018) ────────────────
	ModelCommon           = uint16(1)   // manufacturer, serial number, model, version
	ModelInverterSinglePh = uint16(101) // single-phase inverter measurements
	ModelInverterSplitPh  = uint16(102) // split-phase inverter measurements
	ModelInverterThreePh  = uint16(103) // three-phase inverter measurements
	ModelNameplate        = uint16(120) // DER nameplate ratings
	ModelBasicSettings    = uint16(121) // DER operational settings (WMax, etc.)
	ModelExtendedStatus   = uint16(122) // extended measurements & connection status
	ModelImmediateCtrl    = uint16(123) // immediate controls: WMaxLimPct, Conn, etc.
	ModelBatteryBase      = uint16(801) // battery base model
	ModelLithiumBattery   = uint16(802) // lithium battery detail

	// ── IEEE 1547-2018 SunSpec Modbus Profile (models 701-713) ───────────
	// Reference: SunSpec Modbus IEEE 1547-2018 Profile Specification §2.1
	// These supersede the legacy models for compliant implementations.
	ModelDERMeasureAC      = uint16(701) // DER AC measurements (replaces M103 for 1547)
	ModelDERCapacity       = uint16(702) // DER nameplate + configuration (replaces M120/121)
	ModelDEREnterService   = uint16(703) // enter-service / cease-to-energize settings
	ModelDERCtlAC          = uint16(704) // constant PF, constant var, active power limit
	ModelDERVoltVar        = uint16(705) // voltage-reactive power curve (Q(V))
	ModelDERVoltWatt       = uint16(706) // voltage-active power curve (P(V))
	ModelDERTripLV         = uint16(707) // low-voltage trip curve
	ModelDERTripHV         = uint16(708) // high-voltage trip curve
	ModelDERTripLF         = uint16(709) // low-frequency trip curve
	ModelDERTripHF         = uint16(710) // high-frequency trip curve
	ModelDERFreqDroop      = uint16(711) // frequency droop (P(f))
	ModelDERWattVar        = uint16(712) // active power-reactive power curve (Q(P))
	ModelDERStorageCap     = uint16(713) // storage state-of-charge and capacity
)

// SunSpec binary header constants.
const (
	// SunSMagic0 and SunSMagic1 are the two registers that form the ASCII
	// string "SunS" at the start of the SunSpec address space.
	SunSMagic0 = uint16(0x5375) // 'S','u'
	SunSMagic1 = uint16(0x6E53) // 'n','S'

	// EndMarker is the model ID that terminates the SunSpec model list.
	EndMarker = uint16(0xFFFF)

	// SunSpecBase is the most common 0-based Modbus starting address for the
	// SunSpec header (corresponds to Modbus register 40001 in 1-based notation).
	// Per the SunSpec spec, devices may also start at 0 or 50000; the vast
	// majority of commercial hardware uses 40000.
	SunSpecBase = uint16(40000)
)

// ── Model 103 (Three-Phase Inverter) register offsets ────────────────────────
// 0-based within the model data block (i.e. after the model ID and length regs).
// Source: SunSpec Model 103 specification.
const (
	M103_A     = 0  // AC total current (int16, A_SF)
	M103_AphA  = 1  // Phase A current (int16, A_SF)
	M103_AphB  = 2  // Phase B current (int16, A_SF)
	M103_AphC  = 3  // Phase C current (int16, A_SF)
	M103_A_SF  = 4  // current scale factor (int16, power of 10)
	M103_PPVphAB = 5  // Phase A-B voltage (uint16, V_SF)
	M103_PPVphBC = 6  // Phase B-C voltage (uint16, V_SF)
	M103_PPVphCA = 7  // Phase C-A voltage (uint16, V_SF)
	M103_PhVphA  = 8  // Phase A-N voltage (uint16, V_SF)
	M103_PhVphB  = 9  // Phase B-N voltage (uint16, V_SF)
	M103_PhVphC  = 10 // Phase C-N voltage (uint16, V_SF)
	M103_V_SF    = 11 // voltage scale factor (int16)
	M103_W       = 12 // AC power (int16, W_SF)
	M103_W_SF    = 13 // power scale factor (int16)
	M103_Hz      = 14 // frequency (uint16, Hz_SF)
	M103_Hz_SF   = 15 // frequency scale factor (int16)
	M103_VA      = 16 // apparent power (int16, VA_SF)
	M103_VA_SF   = 17 // apparent power scale factor (int16)
	M103_VAr     = 18 // reactive power (int16, VAr_SF)
	M103_VAr_SF  = 19 // reactive power scale factor (int16)
	M103_PF      = 20 // power factor ×100 (int16, PF_SF)
	M103_PF_SF   = 21 // power factor scale factor (int16)
	// WH occupies two registers (uint32) at offsets 22-23
	M103_WH_SF  = 24 // energy scale factor (int16)
	M103_DCA    = 25 // DC current (int16, DCA_SF)
	M103_DCA_SF = 26 // DC current scale factor (int16)
	M103_DCV    = 27 // DC voltage (uint16, DCV_SF)
	M103_DCV_SF = 28 // DC voltage scale factor (int16)
	M103_DCW    = 29 // DC power (int16, DCW_SF)
	M103_DCW_SF = 30 // DC power scale factor (int16)
	M103_TmpCab  = 31 // cabinet temperature (int16, Tmp_SF)
	M103_TmpSnk  = 32 // heat sink temperature (int16, Tmp_SF)
	M103_TmpTrns = 33 // transformer temperature (int16, Tmp_SF)
	M103_TmpOt   = 34 // other temperature (int16, Tmp_SF)
	M103_Tmp_SF  = 35 // temperature scale factor (int16)
	M103_St      = 36 // operating state (uint16): 1=Off 2=Sleeping 3=Starting
	              //   4=MPPT 5=Throttled 6=ShuttingDown 7=Fault 8=Standby
	M103_StVnd   = 37 // vendor status (uint16)
	// Evt1/Evt2 at 38-41 (two uint32s each spanning two registers)
)

// ── Model 120 (Nameplate Ratings) register offsets ───────────────────────────
// Source: SunSpec Model 120 specification.
// DERTyp values: 4=PV, 80=storage, 82=storage+PV.
const (
	M120Len            = 26  // data registers
	M120_DERTyp        = 0   // DER type (uint16)
	M120_WRtg          = 1   // nameplate real power (uint16, M120_W_SF)
	M120_VARtg         = 2   // nameplate apparent power (uint16, M120_VARtg_SF)
	M120_VArRtgQ1      = 3   // max reactive power Q1 (int16, M120_VArRtg_SF)
	M120_VArRtgQ2      = 4   // Q2 (int16)
	M120_VArRtgQ3      = 5   // Q3 (int16)
	M120_VArRtgQ4      = 6   // Q4 (int16)
	M120_ARtg          = 7   // nameplate current (uint16, M120_ARtg_SF)
	M120_PFRtgQ1       = 8   // min power factor Q1 ×100 (int16, M120_PFRtg_SF)
	M120_PFRtgQ2       = 9
	M120_PFRtgQ3       = 10
	M120_PFRtgQ4       = 11
	M120_WHRtg         = 12  // energy storage rating (uint16, M120_WHRtg_SF) — storage
	M120_AhrRtg        = 13  // amp-hour rating (uint16, M120_AhrRtg_SF)
	M120_MaxChaRte     = 14  // max charge rate (uint16, M120_MaxChaRte_SF) — storage
	M120_MaxDisChaRte  = 15  // max discharge rate (uint16, M120_MaxDisChaRte_SF)
	M120_W_SF          = 16  // power scale factor (int16)
	M120_VARtg_SF      = 17
	M120_VArRtg_SF     = 18
	M120_ARtg_SF       = 19
	M120_PFRtg_SF      = 20
	M120_WHRtg_SF      = 21
	M120_AhrRtg_SF     = 22
	M120_MaxChaRte_SF  = 23
	M120_MaxDisChaRte_SF = 24
)

// ── Model 122 (Extended Measurements & Status) register offsets ──────────────
// Only the registers this codebase reads or writes are named; the full model
// is 44 registers and the sim populates unused registers as zero.
const (
	M122Len       = 44  // full model length per SunSpec spec
	M122_PVConn   = 0   // PV connection status bitfield (uint16): bit 0 = connected
	M122_StorConn = 1   // storage connection status bitfield
	M122_ECPConn  = 2   // ECP / grid connection bitfield: bit 0 = grid-connected
	// ActWh: accumulated exported Wh, uint64 spread across offsets 3–6 (4 × uint16)
	M122_ActWh    = 3   // high word of upper 32 bits
	M122_WAval    = 21  // available real power (uint16, M122_WAval_SF)
	M122_WAval_SF = 22  // scale factor (int16)
)

// ── Model 121 (Basic Settings) register offsets ───────────────────────────────
const (
	M121_WMax    = 0  // max active power setpoint (uint16, WMax_SF)
	M121_WMax_SF = 20 // WMax scale factor (int16)
)

// ── Model 802 (Li-Ion Battery Base) register offsets ─────────────────────────
// Source: SunSpec Model 802 specification.
// ChaSt values: 1=off, 2=empty, 3=discharging, 4=charging, 5=full, 6=holding.
// State values: 0=disconnected, 2=connected, 3=standby, 4=SoC-protection.
const (
	M802Len             = 26  // data registers
	M802_WHRtg          = 0   // energy rating (uint16, M802_WHRtg_SF) — Wh
	M802_WHRtg_SF       = 1   // scale factor (int16)
	M802_AHRtg          = 2   // capacity (uint16, M802_AHRtg_SF) — Ah
	M802_AHRtg_SF       = 3
	M802_WChaRteMax     = 4   // max charge rate (uint16, M802_W_SF) — W
	M802_WDisChaRteMax  = 5   // max discharge rate (uint16, M802_W_SF)
	M802_W_SF           = 6   // power scale factor (int16)
	M802_DisChaRte      = 7   // self-discharge rate (uint16, M802_DisChaRte_SF) — %/day
	M802_DisChaRte_SF   = 8
	M802_SoCMax         = 9   // max allowed SoC (uint16, M802_SoC_SF)
	M802_SoCMin         = 10  // min allowed SoC
	M802_SoCRsvMax      = 11  // reserve max SoC
	M802_SoCRsvMin      = 12  // reserve min SoC
	M802_SoC_SF         = 13  // SoC scale factor (int16): use -2 → register × 0.01 = %
	M802_SoC            = 14  // state of charge (uint16 × SoC_SF)
	M802_DoD            = 15  // depth of discharge (uint16, M802_DoD_SF)
	M802_DoD_SF         = 16
	M802_SoH            = 17  // state of health (uint16, M802_SoH_SF) — %
	M802_SoH_SF         = 18
	// NCyc: uint32 at offsets 19–20
	M802_ChaSt          = 21  // charge status enum (uint16)
	M802_LocRemCtl      = 22  // 0=local, 1=remote (uint16)
	M802_HeatCool       = 23  // thermal management enum (uint16)
	M802_Typ            = 24  // battery chemistry: 4=Li-Ion (uint16)
	M802_State          = 25  // operational state enum (uint16)
)

// ── Model 201/202/203 (AC Meter) register offsets ────────────────────────────
// SunSpec meters sit at the main service entrance and measure net grid power.
// Sign convention (all three models): W positive = site importing from grid,
// W negative = site exporting to grid.  This is opposite to the inverter sign
// convention (positive = export).

// Model IDs for the three meter variants.
const (
	ModelMeterSinglePh = uint16(201) // single-phase AC meter
	ModelMeterSplitPh  = uint16(202) // split-phase (US 240 V) AC meter
	ModelMeterThreePh  = uint16(203) // three-phase wye AC meter
)

// Model 201 (Single-Phase AC Meter) — 105 data registers.
// Source: SunSpec Alliance smdx_00201.xml
const (
	M201Len    = 105
	M201_A     = 0  // Total AC current (int16, A_SF)
	M201_AphA  = 1  // Phase A current (int16, A_SF)
	M201_A_SF  = 2  // Current scale factor (int16)
	M201_PhV   = 3  // Average L-N voltage (int16, V_SF)
	M201_PhVphA = 4 // Phase A L-N voltage (int16, V_SF)
	M201_V_SF  = 5  // Voltage scale factor (int16)
	M201_Hz    = 6  // AC frequency (int16, Hz_SF)
	M201_Hz_SF = 7  // Frequency scale factor (int16)
	M201_W     = 8  // Total real power (int16, W_SF); +import −export
	M201_W_SF  = 9  // Power scale factor (int16)
	M201_VA    = 10 // Apparent power (int16, VA_SF)
	M201_VA_SF = 11
	M201_VAR   = 12 // Reactive power (int16, VAR_SF)
	M201_VAR_SF = 13
	M201_PF    = 14 // Avg power factor ×100 (int16, PF_SF)
	M201_PF_SF = 15
)

// Model 202 (Split-Phase AC Meter) — 106 data registers.
const (
	M202Len     = 106
	M202_A      = 0  // Total AC current (int16, A_SF)
	M202_AphA   = 1
	M202_AphB   = 2
	M202_A_SF   = 3
	M202_PhVphA = 4  // Phase A L-N voltage (int16, V_SF)
	M202_PhVphB = 5
	M202_V_SF   = 6
	M202_PPVphAB = 7 // Phase A-B L-L voltage (int16, PPV_SF)
	M202_PPV_SF = 8
	M202_Hz     = 9
	M202_Hz_SF  = 10
	M202_W      = 11 // Total real power (int16, W_SF); +import −export
	M202_W_SF   = 12
)

// Model 203 (Three-Phase Wye AC Meter) — 105 data registers.
const (
	M203Len      = 105
	M203_A       = 0  // Total AC current (int16, A_SF)
	M203_AphA    = 1
	M203_AphB    = 2
	M203_AphC    = 3
	M203_A_SF    = 4
	M203_PhV     = 5  // Average L-N voltage (int16, V_SF)
	M203_PhVphA  = 6
	M203_PhVphB  = 7
	M203_PhVphC  = 8
	M203_V_SF    = 9
	M203_PPV     = 10 // Average L-L voltage (int16, PPV_SF)
	M203_PPVphAB = 11
	M203_PPVphBC = 12
	M203_PPVphCA = 13
	M203_PPV_SF  = 14
	M203_Hz      = 15
	M203_Hz_SF   = 16
	M203_W       = 17 // Total real power (int16, W_SF); +import −export
	M203_W_SF    = 18
	M203_WphA    = 19
	M203_WphB    = 20
	M203_WphC    = 21
	M203_VA      = 22 // Apparent power (int16, VA_SF)
	M203_VA_SF   = 23
	M203_VAR     = 24 // Reactive power (int16, VAR_SF)
	M203_VAR_SF  = 25
	M203_PF      = 26 // Avg power factor ×100 (int16, PF_SF)
	M203_PF_SF   = 27
)

// ── Model 123 (Immediate Controls) register offsets ───────────────────────────
// Writes to these registers take immediate effect on the inverter.
const (
	M123_WMaxLimPct      = 0  // active power limit as % of WMax (uint16, WMaxLimPct_SF)
	M123_WMaxLimPct_WinTms  = 1  // ramp window (uint16, seconds)
	M123_WMaxLimPct_RvrtTms = 2  // revert time (uint16, seconds)
	M123_WMaxLimPct_RmpTms  = 3  // ramp time (uint16, seconds)
	M123_WMaxLimPct_Ena  = 4  // enable WMaxLimPct (uint16: 0=disabled 1=enabled)
	M123_OutPFSet        = 5  // output power factor (int16, OutPFSet_SF)
	M123_OutPFSet_WinTms = 6
	M123_OutPFSet_RvrtTms = 7
	M123_OutPFSet_RmpTms = 8
	M123_OutPFSet_Ena    = 9  // enable OutPFSet (uint16)
	M123_VArPct_Mod      = 10 // VAr percent mode (uint16)
	M123_VArPct          = 11 // VAr command as % of nameplate (int16, VArPct_SF)
	M123_VArPct_WinTms   = 12
	M123_VArPct_RvrtTms  = 13
	M123_VArPct_RmpTms   = 14
	M123_VArPct_Ena      = 15 // enable VArPct (uint16)
	M123_Conn            = 16 // connect/disconnect (uint16: 0=disconnect 1=connect)
	M123_Conn_WinTms     = 17 // connect window time (uint16, seconds)
	M123_Conn_RvrtTms    = 18 // revert time (uint16, seconds)
	M123_Conn_RmpTms     = 19 // ramp time (uint16, seconds)
	M123_WMaxLimPct_SF   = 20 // WMaxLimPct scale factor (int16)
	M123_OutPFSet_SF     = 21 // OutPFSet scale factor (int16)
	M123_VArPct_SF       = 22 // VArPct scale factor (int16)
)

// ═══════════════════════════════════════════════════════════════════════════════
// IEEE 1547-2018 SunSpec Modbus Profile — Models 701-713
// Reference: SunSpec Modbus IEEE 1547-2018 Profile Specification and
// Implementation Guide (sunspec.org).
// All offsets are 0-based from the start of the model's data block.
// ═══════════════════════════════════════════════════════════════════════════════

// ── Model 701 (DERMeasureAC) — 28 data registers ─────────────────────────────
// IEEE 1547-2018 Monitoring Information. Replaces Model 103 for 1547-compliant
// devices. Required points per Table 17 of the 1547 SunSpec profile spec.
//
// St values: 0=off, 1=sleeping, 2=starting, 3=on, 4=throttled,
//
//	5=shutting_down, 6=fault, 7=standby.
//
// ConnSt: 0=disconnected, 1=connected.
// Alrm: bitfield (uint32 spanning 2 registers).
const (
	M701Len        = 28
	M701_W         = 0  // Active power (int16, W_SF)
	M701_Var       = 1  // Reactive power (int16, Var_SF)
	M701_VA        = 2  // Apparent power (int16, VA_SF)
	M701_PF        = 3  // Power factor ×100 (int16, PF_SF)
	M701_A         = 4  // AC current (int16, A_SF)
	M701_LLV       = 5  // Average L-L voltage (int16, V_SF)
	M701_LNV       = 6  // Average L-N voltage (int16, V_SF)
	M701_VL1L2     = 7  // L1-L2 voltage (int16, V_SF)
	M701_VL1       = 8  // L1-N voltage (int16, V_SF)
	M701_VL2L3     = 9  // L2-L3 voltage (int16, V_SF)
	M701_VL2       = 10 // L2-N voltage (int16, V_SF)
	M701_VL3L1     = 11 // L3-L1 voltage (int16, V_SF)
	M701_VL3       = 12 // L3-N voltage (int16, V_SF)
	M701_Hz        = 13 // Frequency (uint16, Hz_SF)
	M701_W_SF      = 14 // Power scale factor (int16)
	M701_Var_SF    = 15 // Reactive power scale factor (int16)
	M701_VA_SF     = 16 // Apparent power scale factor (int16)
	M701_PF_SF     = 17 // Power factor scale factor (int16)
	M701_A_SF      = 18 // Current scale factor (int16)
	M701_V_SF      = 19 // Voltage scale factor (int16)
	M701_Hz_SF     = 20 // Frequency scale factor (int16)
	M701_St        = 21 // Operating state enum (uint16)
	M701_StVnd     = 22 // Vendor-defined state (uint16)
	M701_ConnSt    = 23 // Connection status (uint16)
	M701_Alrm      = 24 // Alarm bitfield high word (uint32 lo at 25)
	M701_AlrmVnd   = 26 // Vendor alarm bitfield high word (uint32 lo at 27)
)

// ── Model 702 (DERCapacity) — 23 required + 13 optional data registers ────────
// IEEE 1547-2018 Nameplate and Configuration Information. Replaces M120/M121.
// CtrlModes is a uint32 bitmask spanning two consecutive registers (15, 16).
// Required fields per Table 18; optional configuration fields per Table 19.
const (
	// Required nameplate ratings
	M702_WMaxRtg        = 0  // Active power max rating (uint16, W_SF)
	M702_WOvrExtRtg     = 1  // Over-excited active power rating (uint16, W_SF)
	M702_WOvrExtRtgPF   = 2  // Over-excited power factor ×100 (uint16, PF_SF)
	M702_WUndExtRtg     = 3  // Under-excited active power rating (uint16, W_SF)
	M702_WUndExtRtgPF   = 4  // Under-excited power factor ×100 (uint16, PF_SF)
	M702_VAMaxRtg       = 5  // Max apparent power rating (uint16, VA_SF)
	M702_NorOpCatRtg    = 6  // Normal operating category: 0=A, 1=B (uint16)
	M702_AbnOpCatRtg    = 7  // Abnormal operating category: 0=I,1=II,2=III (uint16)
	M702_VarMaxInjRtg   = 8  // Max reactive power injected (uint16, Var_SF)
	M702_VarMaxAbsRtg   = 9  // Max reactive power absorbed (uint16, Var_SF)
	M702_WChaRteMaxRtg  = 10 // Max active power charge rate (uint16, W_SF) — storage
	M702_VAChaRteMaxRtg = 11 // Max apparent power charge rate (uint16, VA_SF) — storage
	M702_VNomRtg        = 12 // Nominal AC voltage (uint16, V_SF)
	M702_VMaxRtg        = 13 // Max AC voltage rating (uint16, V_SF)
	M702_VMinRtg        = 14 // Min AC voltage rating (uint16, V_SF)
	M702_CtrlModes      = 15 // Supported control mode bitfield (uint32, regs 15-16)
	M702_ReactSusceptRtg= 17 // Reactive susceptance (uint16, Var_SF)
	// Scale factors
	M702_W_SF           = 18 // Power scale factor (int16)
	M702_PF_SF          = 19 // Power factor scale factor (int16)
	M702_VA_SF          = 20 // Apparent power scale factor (int16)
	M702_Var_SF         = 21 // Reactive power scale factor (int16)
	M702_V_SF           = 22 // Voltage scale factor (int16)
	// Optional configuration fields (Table 19)
	M702_IntIslandCatRtg = 23 // Intentional island category rating (uint16)
	M702_WMax            = 24 // Active power limit setpoint (uint16, W_SF)
	M702_WMaxOvrExt      = 25 // Over-excited power limit (uint16, W_SF)
	M702_WOvrExtPF       = 26 // Over-excited power factor ×100 (uint16, PF_SF)
	M702_WMaxUndExt      = 27 // Under-excited power limit (uint16, W_SF)
	M702_WUndExtPF       = 28 // Under-excited power factor ×100 (uint16, PF_SF)
	M702_VAMax           = 29 // Apparent power limit (uint16, VA_SF)
	M702_IntIslandCat    = 30 // Intentional island category setpoint (uint16)
	M702_VarMaxInj       = 31 // Max reactive power inject setpoint (uint16, Var_SF)
	M702_VarMaxAbs       = 32 // Max reactive power absorb setpoint (uint16, Var_SF)
	M702_WChaRteMax      = 33 // Max charge rate setpoint (uint16, W_SF)
	M702_VAChaRteMax     = 34 // Max apparent charge rate setpoint (uint16, VA_SF)
	M702_VNom            = 35 // Nominal voltage setpoint (uint16, V_SF)
)

// ── Model 703 (DEREnterService) — 10 data registers ──────────────────────────
// IEEE 1547-2018 Enter Service and Cease-to-Energize settings.
// Cease-to-Energize is performed by setting ES=0 (disabled).
// Required points per Table 20; ESRndTms is optional (Table 21).
// ES values: 0=DISABLED, 1=ENABLED.
const (
	M703Len       = 10
	M703_ES       = 0 // Permit service: 0=disabled, 1=enabled (uint16)
	M703_ESVHi    = 1 // Voltage high threshold (uint16, V_SF)
	M703_ESVLo    = 2 // Voltage low threshold (uint16, V_SF)
	M703_ESHzHi   = 3 // Frequency high threshold (uint16, Hz_SF)
	M703_ESHzLo   = 4 // Frequency low threshold (uint16, Hz_SF)
	M703_ESDlyTms = 5 // Enter-service delay (uint16, seconds)
	M703_ESRmpTms = 6 // Enter-service ramp time (uint16, seconds)
	M703_ESRndTms = 7 // Randomized delay (uint16, seconds; optional)
	M703_V_SF     = 8 // Voltage scale factor (int16)
	M703_Hz_SF    = 9 // Frequency scale factor (int16)
)

// ── Model 704 (DERCtlAC) — 12 data registers ─────────────────────────────────
// IEEE 1547-2018 Constant Power Factor, Constant Reactive Power, and
// Limit Maximum Active Power. Required points per Table 22.
//
// PFWInjEna / VarSetEna / WMaxLimPctEna: 0=DISABLED, 1=ENABLED.
// VarSetMod: 0=W_MAX_PCT, 1=VAR_MAX_PCT, 2=VA_MAX_PCT.
// VarSetPri: 0=active_power_priority, 1=REACTIVE.
// PFWInj_Ext: 0=absorbing (under-excited), 1=injecting (over-excited).
const (
	M704Len            = 12
	M704_PFWInjEna     = 0  // Constant PF mode enable (uint16)
	M704_PFWInj_PF     = 1  // Power factor setpoint ×100 (int16, PF_SF)
	M704_PFWInj_Ext    = 2  // Excitation direction (uint16)
	M704_PF_SF         = 3  // Power factor scale factor (int16)
	M704_VarSetEna     = 4  // Constant reactive power enable (uint16)
	M704_VarSetMod     = 5  // Reactive power mode (uint16)
	M704_VarSetPri     = 6  // Priority (uint16)
	M704_VarSetPct     = 7  // Reactive power setpoint % of WMax (int16, VarSetPct_SF)
	M704_VarSetPct_SF  = 8  // Reactive power setpoint scale factor (int16)
	M704_WMaxLimPctEna = 9  // Active power limit enable (uint16)
	M704_WMaxLimPct    = 10 // Active power limit % of WMax (uint16, WMaxLimPct_SF)
	M704_WMaxLimPct_SF = 11 // Active power limit scale factor (int16)
)

// ── Curve-based models (705-712): fixed-header offsets ───────────────────────
// These models use a fixed header followed by variable-length per-curve or
// per-control data. The total length depends on NPt (points per curve) and
// NCrv / NCtl (number of curves or control sets).
//
// Use CurveOffset705/706/707/708/709/710/712 or CtlOffset711 helper functions
// (defined in der1547.go) to compute offsets into curve/control data.

// Model 705 (DERVoltVar) — voltage-reactive power Q(V) curve.
// IEEE 1547-2018 §2.6. Requires 4 curve points per IEEE 1547.
// Required points per Table 23.
const (
	M705_Ena        = 0 // Enable: 0=DISABLED, 1=ENABLED (uint16)
	M705_AdptCrvReq = 1 // Adapt curve request: write curve index to activate (uint16)
	M705_AdptCrvRslt= 2 // Result: 0=IN_PROGRESS, 1=COMPLETED, 2=FAILED (uint16)
	M705_NPt        = 3 // Points per curve (uint16)
	M705_NCrv       = 4 // Number of curves (uint16)
	M705_V_SF       = 5 // Voltage scale factor (int16)
	M705_DeptRef_SF = 6 // Dependent variable scale factor (int16)
	M705_RspTms_SF  = 7 // Response time scale factor (int16)
	M705_CrvHdrSize = 8 // Curve header size (registers before point data)
	// Per-curve header offsets (relative to curve start):
	M705_Crv_ActPt       = 0 // Number of active points (uint16)
	M705_Crv_DeptRef     = 1 // Dependent ref: 0=W_MAX_PCT,1=VAR_MAX_PCT,2=VA_MAX_PCT (uint16)
	M705_Crv_Pri         = 2 // Priority: 1=REACTIVE (uint16)
	M705_Crv_VRef        = 3 // Voltage reference (uint16, V_SF)
	M705_Crv_VRefAutoEna = 4 // Autonomous VRef: 0=DISABLED,1=ENABLED (uint16)
	M705_Crv_VRefAutoTms = 5 // VRef time constant (uint16, RspTms_SF)
	M705_Crv_RspTms      = 6 // Open loop response time (uint16, RspTms_SF)
	M705_Crv_ReadOnly    = 7 // 0=RW, 1=R (uint16)
	M705_Crv_PtOffset    = 8 // Offset to first point pair within curve
	// Each point: V (int16, V_SF), Var (int16, DeptRef_SF)
)

// Model 706 (DERVoltWatt) — voltage-active power P(V) curve.
// IEEE 1547-2018 §2.9. Requires 2 curve points per IEEE 1547.
// Required points per Table 24.
const (
	M706_Ena        = 0 // Enable: 0=DISABLED, 1=ENABLED (uint16)
	M706_AdptCrvReq = 1
	M706_AdptCrvRslt= 2
	M706_NPt        = 3 // Points per curve (uint16)
	M706_NCrv       = 4
	M706_V_SF       = 5
	M706_DeptRef_SF = 6
	M706_RspTms_SF  = 7
	M706_CrvHdrSize = 4 // Per-curve header size (registers before point data)
	// Per-curve header offsets:
	M706_Crv_ActPt    = 0
	M706_Crv_DeptRef  = 1 // 0=W_MAX_PCT (uint16)
	M706_Crv_RspTms   = 2
	M706_Crv_ReadOnly = 3
	M706_Crv_PtOffset = 4
	// Each point: V (int16, V_SF), W (int16, DeptRef_SF)
)

// Model 707 (DERTripLV) — low-voltage must-trip and momentary-cessation curves.
// Model 708 (DERTripHV) — high-voltage must-trip and momentary-cessation curves.
// IEEE 1547-2018 §2.10-2.11. Both models use identical register layouts.
// Required: 5 curve points per IEEE 1547 (NPt=5). Required per Tables 25-26.
//
// Curve-set layout (per NCrvSet curve-sets):
//
//	ReadOnly (1) | MustTrip.ActPt (1) | MustTrip.Pt[0..NPt-1].V, .Tms (2×NPt)
//	             | optional MomCess.ActPt (1) | MomCess.Pt.V, .Tms (2×1)
const (
	M707_Ena        = 0 // Enable: 1=ENABLED (uint16; DISABLED may not be supported)
	M707_AdptCrvReq = 1
	M707_AdptCrvRslt= 2
	M707_NPt        = 3 // Points per must-trip curve (uint16; IEEE 1547 = 5)
	M707_NCrvSet    = 4 // Number of curve sets (uint16)
	M707_V_SF       = 5 // Voltage scale factor (int16)
	M707_Tms_SF     = 6 // Time scale factor (int16)
	// Per-curve-set offsets (relative to curve-set start):
	M707_CrvSet_ReadOnly    = 0 // 0=RW, 1=R
	M707_CrvSet_MustTripActPt = 1
	M707_CrvSet_MustTripPtV   = 2 // V for point i: +2+2i; Tms for i: +3+2i
	// MomCess (optional) follows MustTrip points
)

// M708 shares M707 layout; use the same M707_* constants with ModelDERTripHV.
// Model 709/710 (DERTripLF/DERTripHF) — frequency trip curves.
// Same structure as 707/708 but V_SF→Hz_SF and V→Hz in curve points.
const (
	M709_Ena        = 0
	M709_AdptCrvReq = 1
	M709_AdptCrvRslt= 2
	M709_NPt        = 3
	M709_NCrvSet    = 4
	M709_Hz_SF      = 5 // Frequency scale factor (int16)
	M709_Tms_SF     = 6
	// Per-curve-set: same layout as M707 but Hz instead of V
	M709_CrvSet_ReadOnly      = 0
	M709_CrvSet_MustTripActPt = 1
	M709_CrvSet_MustTripPtHz  = 2 // Hz for point i: +2+2i; Tms: +3+2i
)

// M710 shares M709 layout; use the same M709_* constants with ModelDERTripHF.

// Model 711 (DERFreqDroop) — frequency droop P(f).
// IEEE 1547-2018 §2.13. Required per Table 28.
// NCtl control-sets: first is read-only (current), second is writable.
const (
	M711_Ena        = 0 // Enable: 1=ENABLED (uint16)
	M711_AdptCtlReq = 1 // Adapt control request (uint16)
	M711_AdptCtlRslt= 2 // Result (uint16)
	M711_NCtl       = 3 // Number of control sets (uint16)
	M711_Db_SF      = 4 // Deadband scale factor (int16)
	M711_K_SF       = 5 // Droop gain scale factor (int16)
	M711_RspTms_SF  = 6 // Response time scale factor (int16)
	// Per-control-set offsets (relative to control start):
	M711_Ctl_DbOf     = 0 // Over-frequency deadband (uint16, Db_SF)
	M711_Ctl_DbUf     = 1 // Under-frequency deadband (uint16, Db_SF)
	M711_Ctl_KOf      = 2 // Over-frequency droop gain (int16, K_SF)
	M711_Ctl_KUf      = 3 // Under-frequency droop gain (int16, K_SF)
	M711_Ctl_RspTms   = 4 // Response time (uint16, RspTms_SF)
	M711_Ctl_ReadOnly = 5 // 0=RW, 1=R
	M711_CtlSize      = 6 // Registers per control set
	M711_CtlStart     = 7 // Offset to first control set from model start
)

// Model 712 (DERWattVar) — active power-reactive power Q(P) curve.
// IEEE 1547-2018 §2.7. Must support 6-point curve; first 3 points for
// load operation may be set to 0 if not used. Required per Table 29.
const (
	M712_Ena        = 0 // Enable: 0=DISABLED, 1=ENABLED (uint16)
	M712_AdptCrvReq = 1
	M712_AdptCrvRslt= 2
	M712_NPt        = 3 // Points per curve (uint16; IEEE 1547 = 6)
	M712_NCrv       = 4
	M712_W_SF       = 5 // Active power scale factor (int16)
	M712_DeptRef_SF = 6 // Reactive power scale factor (int16)
	M712_CrvHdrSize = 4 // Per-curve header size
	// Per-curve header offsets:
	M712_Crv_ActPt    = 0
	M712_Crv_DeptRef  = 1 // 0=W_MAX_PCT,1=VAR_MAX_PCT,2=VA_MAX_PCT (uint16)
	M712_Crv_Pri      = 2 // 1=REACTIVE (uint16)
	M712_Crv_ReadOnly = 3
	M712_Crv_PtOffset = 4
	// Each point: W (int16, W_SF), Var (int16, DeptRef_SF)
)

// ── Model 713 (DERStorageCapacity) — 14 data registers ───────────────────────
// IEEE 1547-2018 §3.13: Operational State of Charge (SoC). Required per
// Table 30. If the device has no storage, SoC must be 0.
// SoC / SoH are scaled percentages: actual = raw × 10^SF.
const (
	M713Len          = 14
	M713_WHRtg       = 0  // Rated energy (uint16, WHRtg_SF) — Wh
	M713_AHRtg       = 1  // Rated capacity (uint16, AHRtg_SF) — Ah
	M713_MaxChaSoC   = 2  // Max charge state of charge % (uint16, SoC_SF)
	M713_MinChaSoC   = 3  // Min charge state of charge % (uint16, SoC_SF)
	M713_MaxChaPct   = 4  // Max charge rate % of WChaRteMax (uint16, Pct_SF)
	M713_SoC         = 5  // State of charge % (uint16, SoC_SF)
	M713_SoH         = 6  // State of health % (uint16, SoH_SF)
	M713_NCyc        = 7  // Lifetime cycle count (uint32, regs 7-8)
	M713_WHRtg_SF    = 9  // Energy capacity scale factor (int16)
	M713_AHRtg_SF    = 10 // Amp-hour capacity scale factor (int16)
	M713_SoC_SF      = 11 // State of charge scale factor (int16)
	M713_SoH_SF      = 12 // State of health scale factor (int16)
	M713_Pct_SF      = 13 // Percentage scale factor (int16)
)
