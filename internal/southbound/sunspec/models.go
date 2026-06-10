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
