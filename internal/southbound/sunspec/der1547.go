// Package sunspec — this file provides typed structs and parse/encode helpers
// for the IEEE 1547-2018 SunSpec Modbus profile (models 701-713).
//
// Each model is represented by a Go struct whose fields correspond 1:1 to the
// SunSpec Modbus points listed in the profile specification. Values are already
// scaled (watts, volts, Hz) unless otherwise noted — scale factor application
// is handled in the parse helpers using ApplyScaleSigned / ApplyScaleUint.
//
// Curve models (705, 706, 707/708, 709/710, 712) have a fixed-size header
// followed by variable per-curve data. The helpers CurveBase705 etc. compute
// the register offset of a given curve within the model's data block.
package sunspec

import (
	"fmt"
	"math"
)

// ── Common ────────────────────────────────────────────────────────────────────

// AdptResult is the enumerated result of an adapt-curve/control request.
type AdptResult uint16

const (
	AdptInProgress AdptResult = 0
	AdptCompleted  AdptResult = 1
	AdptFailed     AdptResult = 2
)

// ReadOnly indicates whether a curve or control set is writable.
type ReadOnly uint16

const (
	ReadWrite ReadOnly = 0
	ReadOnlyR ReadOnly = 1
)

// ── Model 701 (DERMeasureAC) ─────────────────────────────────────────────────

// M701St is the operating state reported by DERMeasureAC.
type M701St uint16

const (
	M701StOff         M701St = 0
	M701StSleeping    M701St = 1
	M701StStarting    M701St = 2
	M701StOn          M701St = 3
	M701StThrottled   M701St = 4
	M701StShuttingDwn M701St = 5
	M701StFault       M701St = 6
	M701StStandby     M701St = 7
)

// DERMeasureAC holds the scaled AC electrical measurements from Model 701.
// Fields requiring voltages that are not applicable should be NaN.
type DERMeasureAC struct {
	W      float64 // Active power (W); + export, - import
	Var    float64 // Reactive power (VAr)
	VA     float64 // Apparent power (VA)
	PF     float64 // Power factor (-1 to +1)
	A      float64 // AC current (A)
	LLV    float64 // Average L-L voltage (V)
	LNV    float64 // Average L-N voltage (V)
	VL1L2  float64 // L1-L2 (V)
	VL1    float64 // L1-N (V)
	VL2L3  float64 // L2-L3 (V)
	VL2    float64 // L2-N (V)
	VL3L1  float64 // L3-L1 (V)
	VL3    float64 // L3-N (V)
	Hz     float64 // Frequency (Hz)
	St     M701St  // Operating state
	ConnSt bool    // true = connected to grid
	Alrm   uint32  // Alarm bitfield
}

// ParseDERMeasureAC converts raw M701 register data into a DERMeasureAC.
func ParseDERMeasureAC(regs []uint16) (DERMeasureAC, error) {
	if len(regs) < M701_ConnSt+1 {
		return DERMeasureAC{}, fmt.Errorf("sunspec: M701 too short (%d regs)", len(regs))
	}
	get := func(i int) uint16 {
		if i < len(regs) {
			return regs[i]
		}
		return 0
	}
	sf := func(i int) int16 { return int16(get(i)) }

	vSF  := sf(M701_V_SF)
	wSF  := sf(M701_W_SF)
	varSF := sf(M701_Var_SF)
	vaSF := sf(M701_VA_SF)
	pfSF := sf(M701_PF_SF)
	hzSF := sf(M701_Hz_SF)

	m := DERMeasureAC{
		W:     ApplyScaleSigned(get(M701_W), wSF),
		Var:   ApplyScaleSigned(get(M701_Var), varSF),
		VA:    ApplyScaleSigned(get(M701_VA), vaSF),
		PF:    ApplyScaleSigned(get(M701_PF), pfSF) / 100.0,
		A:     ApplyScaleSigned(get(M701_A), sf(M701_A_SF)),
		LLV:   ApplyScaleSigned(get(M701_LLV), vSF),
		LNV:   ApplyScaleSigned(get(M701_LNV), vSF),
		VL1L2: ApplyScaleSigned(get(M701_VL1L2), vSF),
		VL1:   ApplyScaleSigned(get(M701_VL1), vSF),
		VL2L3: ApplyScaleSigned(get(M701_VL2L3), vSF),
		VL2:   ApplyScaleSigned(get(M701_VL2), vSF),
		VL3L1: ApplyScaleSigned(get(M701_VL3L1), vSF),
		VL3:   ApplyScaleSigned(get(M701_VL3), vSF),
		Hz:    ApplyScaleUint(get(M701_Hz), hzSF),
		St:    M701St(get(M701_St)),
		ConnSt: get(M701_ConnSt) == 1,
	}
	if len(regs) >= M701_Alrm+2 {
		m.Alrm = uint32(get(M701_Alrm))<<16 | uint32(get(M701_Alrm+1))
	}
	return m, nil
}

// ── Model 702 (DERCapacity) ───────────────────────────────────────────────────

// DERCapacity holds the nameplate and optional configuration data from M702.
// Charge-rate fields (WChaRteMaxRtg, VAChaRteMaxRtg) are storage-specific.
type DERCapacity struct {
	// Required nameplate ratings
	WMaxRtg         float64 // Active power max (W)
	WOvrExtRtg      float64 // Over-excited active power (W)
	WOvrExtRtgPF    float64 // Over-excited power factor
	WUndExtRtg      float64 // Under-excited active power (W)
	WUndExtRtgPF    float64 // Under-excited power factor
	VAMaxRtg        float64 // Max apparent power (VA)
	NorOpCatRtg     uint16  // Normal operating category: 0=A, 1=B
	AbnOpCatRtg     uint16  // Abnormal operating category: 0=I, 1=II, 2=III
	VarMaxInjRtg    float64 // Max reactive power injected (VAr)
	VarMaxAbsRtg    float64 // Max reactive power absorbed (VAr)
	WChaRteMaxRtg   float64 // Max charge rate (W); NaN if not storage
	VAChaRteMaxRtg  float64 // Max charge apparent power (VA); NaN if not storage
	VNomRtg         float64 // Nominal AC voltage (V)
	VMaxRtg         float64 // Max AC voltage (V)
	VMinRtg         float64 // Min AC voltage (V)
	CtrlModes       uint32  // Supported control modes bitfield
	ReactSusceptRtg float64 // Reactive susceptance (VAr)
	// Optional configuration setpoints (NaN if not present)
	WMax          float64 // Active power limit setpoint (W)
	WMaxOvrExt    float64 // Over-excited power limit (W)
	WOvrExtPF     float64 // Over-excited power factor setpoint
	WMaxUndExt    float64 // Under-excited power limit (W)
	WUndExtPF     float64 // Under-excited power factor setpoint
	VAMax         float64 // Apparent power limit (VA)
	VarMaxInj     float64 // Max reactive power inject setpoint (VAr)
	VarMaxAbs     float64 // Max reactive power absorb setpoint (VAr)
	WChaRteMax    float64 // Max charge rate setpoint (W)
	VAChaRteMax   float64 // Max charge apparent setpoint (VA)
	VNom          float64 // Nominal voltage setpoint (V)
}

// ParseDERCapacity converts raw M702 register data into a DERCapacity.
func ParseDERCapacity(regs []uint16) (DERCapacity, error) {
	if len(regs) < M702_V_SF+1 {
		return DERCapacity{}, fmt.Errorf("sunspec: M702 too short (%d regs)", len(regs))
	}
	get := func(i int) uint16 {
		if i < len(regs) {
			return regs[i]
		}
		return 0
	}
	wSF  := int16(get(M702_W_SF))
	pfSF := int16(get(M702_PF_SF))
	vaSF := int16(get(M702_VA_SF))
	varSF := int16(get(M702_Var_SF))
	vSF  := int16(get(M702_V_SF))

	nan := math.NaN()
	c := DERCapacity{
		WMaxRtg:         ApplyScaleUint(get(M702_WMaxRtg), wSF),
		WOvrExtRtg:      ApplyScaleUint(get(M702_WOvrExtRtg), wSF),
		WOvrExtRtgPF:    ApplyScaleUint(get(M702_WOvrExtRtgPF), pfSF) / 100.0,
		WUndExtRtg:      ApplyScaleUint(get(M702_WUndExtRtg), wSF),
		WUndExtRtgPF:    ApplyScaleUint(get(M702_WUndExtRtgPF), pfSF) / 100.0,
		VAMaxRtg:        ApplyScaleUint(get(M702_VAMaxRtg), vaSF),
		NorOpCatRtg:     get(M702_NorOpCatRtg),
		AbnOpCatRtg:     get(M702_AbnOpCatRtg),
		VarMaxInjRtg:    ApplyScaleUint(get(M702_VarMaxInjRtg), varSF),
		VarMaxAbsRtg:    ApplyScaleUint(get(M702_VarMaxAbsRtg), varSF),
		WChaRteMaxRtg:   ApplyScaleUint(get(M702_WChaRteMaxRtg), wSF),
		VAChaRteMaxRtg:  ApplyScaleUint(get(M702_VAChaRteMaxRtg), vaSF),
		VNomRtg:         ApplyScaleUint(get(M702_VNomRtg), vSF),
		VMaxRtg:         ApplyScaleUint(get(M702_VMaxRtg), vSF),
		VMinRtg:         ApplyScaleUint(get(M702_VMinRtg), vSF),
		CtrlModes:       uint32(get(M702_CtrlModes))<<16 | uint32(get(M702_CtrlModes+1)),
		ReactSusceptRtg: ApplyScaleUint(get(M702_ReactSusceptRtg), varSF),
		// Optional fields default to NaN; set below if registers are present.
		WMax: nan, WMaxOvrExt: nan, WOvrExtPF: nan, WMaxUndExt: nan, WUndExtPF: nan,
		VAMax: nan, VarMaxInj: nan, VarMaxAbs: nan, WChaRteMax: nan, VAChaRteMax: nan, VNom: nan,
	}
	if len(regs) > M702_WMax {
		c.WMax       = ApplyScaleUint(get(M702_WMax), wSF)
		c.WMaxOvrExt = ApplyScaleUint(get(M702_WMaxOvrExt), wSF)
		c.WOvrExtPF  = ApplyScaleUint(get(M702_WOvrExtPF), pfSF) / 100.0
		c.WMaxUndExt = ApplyScaleUint(get(M702_WMaxUndExt), wSF)
		c.WUndExtPF  = ApplyScaleUint(get(M702_WUndExtPF), pfSF) / 100.0
		c.VAMax      = ApplyScaleUint(get(M702_VAMax), vaSF)
		c.VarMaxInj  = ApplyScaleUint(get(M702_VarMaxInj), varSF)
		c.VarMaxAbs  = ApplyScaleUint(get(M702_VarMaxAbs), varSF)
		c.WChaRteMax = ApplyScaleUint(get(M702_WChaRteMax), wSF)
		c.VAChaRteMax= ApplyScaleUint(get(M702_VAChaRteMax), vaSF)
		c.VNom       = ApplyScaleUint(get(M702_VNom), vSF)
	}
	return c, nil
}

// ── Model 703 (DEREnterService) ───────────────────────────────────────────────

// DEREnterServiceSettings holds the enter-service / cease-to-energize config.
// Set Enabled=false to cease-to-energize the DER.
type DEREnterServiceSettings struct {
	Enabled    bool    // ES: false=cease-to-energize, true=permit service
	VHi        float64 // Voltage high limit (V)
	VLo        float64 // Voltage low limit (V)
	HzHi       float64 // Frequency high limit (Hz)
	HzLo       float64 // Frequency low limit (Hz)
	DelayS     uint16  // Entry delay (seconds)
	RampS      uint16  // Ramp time (seconds)
	RandomDelayS uint16 // Randomized delay (seconds; 0 = not used)
}

// ParseDEREnterService converts raw M703 register data.
func ParseDEREnterService(regs []uint16) (DEREnterServiceSettings, error) {
	if len(regs) < M703_Hz_SF+1 {
		return DEREnterServiceSettings{}, fmt.Errorf("sunspec: M703 too short (%d regs)", len(regs))
	}
	get := func(i int) uint16 {
		if i < len(regs) {
			return regs[i]
		}
		return 0
	}
	vSF  := int16(get(M703_V_SF))
	hzSF := int16(get(M703_Hz_SF))
	return DEREnterServiceSettings{
		Enabled:      get(M703_ES) == 1,
		VHi:          ApplyScaleUint(get(M703_ESVHi), vSF),
		VLo:          ApplyScaleUint(get(M703_ESVLo), vSF),
		HzHi:         ApplyScaleUint(get(M703_ESHzHi), hzSF),
		HzLo:         ApplyScaleUint(get(M703_ESHzLo), hzSF),
		DelayS:       get(M703_ESDlyTms),
		RampS:        get(M703_ESRmpTms),
		RandomDelayS: get(M703_ESRndTms),
	}, nil
}

// EncodeDEREnterService writes s into a pre-read register slice for M703.
// Only the writable fields are touched; the scale factors are read from regs.
// The caller must WriteModel the resulting slice.
func EncodeDEREnterService(regs []uint16, s DEREnterServiceSettings) error {
	if len(regs) < M703_Hz_SF+1 {
		return fmt.Errorf("sunspec: M703 encode: slice too short (%d)", len(regs))
	}
	vSF  := int16(regs[M703_V_SF])
	hzSF := int16(regs[M703_Hz_SF])
	if s.Enabled {
		regs[M703_ES] = 1
	} else {
		regs[M703_ES] = 0
	}
	regs[M703_ESVHi]    = RawFromScaleUint(s.VHi, vSF)
	regs[M703_ESVLo]    = RawFromScaleUint(s.VLo, vSF)
	regs[M703_ESHzHi]   = RawFromScaleUint(s.HzHi, hzSF)
	regs[M703_ESHzLo]   = RawFromScaleUint(s.HzLo, hzSF)
	regs[M703_ESDlyTms] = s.DelayS
	regs[M703_ESRmpTms] = s.RampS
	if len(regs) > M703_ESRndTms {
		regs[M703_ESRndTms] = s.RandomDelayS
	}
	return nil
}

// ── Model 704 (DERCtlAC) ─────────────────────────────────────────────────────

// DERCtlACSettings holds the constant PF, constant var, and power-limit
// settings from Model 704. A nil pointer means "leave unchanged" when encoding.
type DERCtlACSettings struct {
	// Constant power factor (§2.5)
	PFWInjEna  bool    // true=enabled
	PFWInj_PF  float64 // Power factor (0-1; positive only, sign from Ext)
	PFWInj_Ext bool    // true=injecting (over-excited), false=absorbing (under-excited)

	// Constant reactive power (§2.8)
	VarSetEna  bool    // true=enabled
	VarSetMod  uint16  // 0=W_MAX_PCT, 1=VAR_MAX_PCT, 2=VA_MAX_PCT
	VarSetPri  uint16  // 0=active_power, 1=reactive (spec requires REACTIVE)
	VarSetPct  float64 // Reactive power setpoint % of WMax (signed; + inject, - absorb)

	// Limit maximum active power (§2.16)
	WMaxLimPctEna bool    // true=enabled
	WMaxLimPct    float64 // Active power limit % of WMax (0-100)
}

// ParseDERCtlAC converts raw M704 registers into a DERCtlACSettings.
func ParseDERCtlAC(regs []uint16) (DERCtlACSettings, error) {
	if len(regs) < M704Len {
		return DERCtlACSettings{}, fmt.Errorf("sunspec: M704 too short (%d regs)", len(regs))
	}
	pfSF      := int16(regs[M704_PF_SF])
	varPctSF  := int16(regs[M704_VarSetPct_SF])
	wMaxPctSF := int16(regs[M704_WMaxLimPct_SF])
	return DERCtlACSettings{
		PFWInjEna:     regs[M704_PFWInjEna] == 1,
		PFWInj_PF:     ApplyScaleSigned(regs[M704_PFWInj_PF], pfSF) / 100.0,
		PFWInj_Ext:    regs[M704_PFWInj_Ext] == 1,
		VarSetEna:     regs[M704_VarSetEna] == 1,
		VarSetMod:     regs[M704_VarSetMod],
		VarSetPri:     regs[M704_VarSetPri],
		VarSetPct:     ApplyScaleSigned(regs[M704_VarSetPct], varPctSF),
		WMaxLimPctEna: regs[M704_WMaxLimPctEna] == 1,
		WMaxLimPct:    ApplyScaleUint(regs[M704_WMaxLimPct], wMaxPctSF),
	}, nil
}

// EncodeDERCtlAC writes s into a pre-read M704 register slice.
// The caller must WriteModel the result.
func EncodeDERCtlAC(regs []uint16, s DERCtlACSettings) error {
	if len(regs) < M704Len {
		return fmt.Errorf("sunspec: M704 encode: slice too short (%d)", len(regs))
	}
	pfSF      := int16(regs[M704_PF_SF])
	varPctSF  := int16(regs[M704_VarSetPct_SF])
	wMaxPctSF := int16(regs[M704_WMaxLimPct_SF])

	if s.PFWInjEna {
		regs[M704_PFWInjEna] = 1
	} else {
		regs[M704_PFWInjEna] = 0
	}
	regs[M704_PFWInj_PF] = RawFromScaleSigned(s.PFWInj_PF*100.0, pfSF)
	if s.PFWInj_Ext {
		regs[M704_PFWInj_Ext] = 1
	} else {
		regs[M704_PFWInj_Ext] = 0
	}
	if s.VarSetEna {
		regs[M704_VarSetEna] = 1
	} else {
		regs[M704_VarSetEna] = 0
	}
	regs[M704_VarSetMod] = s.VarSetMod
	regs[M704_VarSetPri] = s.VarSetPri
	regs[M704_VarSetPct] = RawFromScaleSigned(s.VarSetPct, varPctSF)
	if s.WMaxLimPctEna {
		regs[M704_WMaxLimPctEna] = 1
	} else {
		regs[M704_WMaxLimPctEna] = 0
	}
	regs[M704_WMaxLimPct] = RawFromScaleUint(s.WMaxLimPct, wMaxPctSF)
	return nil
}

// ── Curve model helpers ───────────────────────────────────────────────────────

// CurveBase705 returns the register offset of curve curveIdx (0-based) within
// a M705 (DERVoltVar) data block, given NPt points per curve.
func CurveBase705(curveIdx, npt int) int {
	return M705_CrvHdrSize + curveIdx*(M705_Crv_PtOffset+2*npt)
}

// CurveBase706 returns the register offset of curve curveIdx within M706 data.
func CurveBase706(curveIdx, npt int) int {
	return 8 + curveIdx*(M706_Crv_PtOffset+2*npt)
}

// CurveSetBase707 returns the offset of curve-set setIdx within M707/M708 data.
// Each set has: ReadOnly(1) + MustTrip.ActPt(1) + NPt pairs + optional MomCess.
// hasMomCess indicates whether optional momentary-cessation points are present.
func CurveSetBase707(setIdx, npt int, hasMomCess bool) int {
	momCessRegs := 0
	if hasMomCess {
		momCessRegs = 1 + 2 // ActPt + 1 point pair
	}
	setSize := 2 + 2*npt + momCessRegs
	return 7 + setIdx*setSize // 7 = fixed header size
}

// CurveSetBase709 returns the offset of curve-set setIdx within M709/M710 data.
// Same layout as 707/708 but Hz instead of V.
func CurveSetBase709(setIdx, npt int, hasMomCess bool) int {
	return CurveSetBase707(setIdx, npt, hasMomCess)
}

// CtlBase711 returns the register offset of control set ctlIdx within M711 data.
func CtlBase711(ctlIdx int) int {
	return M711_CtlStart + ctlIdx*M711_CtlSize
}

// CurveBase712 returns the register offset of curve curveIdx within M712 data.
func CurveBase712(curveIdx, npt int) int {
	return 7 + curveIdx*(M712_Crv_PtOffset+2*npt)
}

// ── Model 705 (DERVoltVar) curve types ────────────────────────────────────────

// VoltVarPoint is a (voltage, reactive power) point in a Q(V) curve.
// V is expressed as % of VRef; Var as % of WMax (or VAr or VA per DeptRef).
type VoltVarPoint struct {
	V   float64 // Voltage (%, scaled)
	Var float64 // Reactive power (%, signed; + inject, - absorb)
}

// VoltVarCurve is a Q(V) curve as used by Model 705.
// IEEE 1547-2018 requires exactly 4 active points (Pt[0]..Pt[3]).
type VoltVarCurve struct {
	DeptRef     uint16         // 0=W_MAX_PCT, 1=VAR_MAX_PCT, 2=VA_MAX_PCT
	Pri         uint16         // 1=REACTIVE
	VRef        float64        // Voltage reference (V, scaled)
	VRefAutoEna bool           // Autonomous VRef adjustment
	VRefAutoTms float64        // VRef time constant (s)
	RspTms      float64        // Open loop response time (s)
	Pts         []VoltVarPoint // Active points (4 for IEEE 1547-2018)
}

// ParseVoltVarCurve reads one curve from an M705 register block.
// curveIdx=0 is the read-only current-settings curve; curveIdx=1 is writable.
func ParseVoltVarCurve(regs []uint16, curveIdx int) (VoltVarCurve, error) {
	if len(regs) < M705_CrvHdrSize {
		return VoltVarCurve{}, fmt.Errorf("sunspec: M705 too short for header")
	}
	npt := int(regs[M705_NPt])
	if npt == 0 || npt > 20 {
		return VoltVarCurve{}, fmt.Errorf("sunspec: M705 NPt=%d out of range", npt)
	}
	base := CurveBase705(curveIdx, npt)
	if len(regs) < base+M705_Crv_PtOffset+2*npt {
		return VoltVarCurve{}, fmt.Errorf("sunspec: M705 too short for curve %d", curveIdx)
	}
	vSF      := int16(regs[M705_V_SF])
	deptRefSF := int16(regs[M705_DeptRef_SF])
	rspTmsSF  := int16(regs[M705_RspTms_SF])

	actPt := int(regs[base+M705_Crv_ActPt])
	if actPt > npt {
		actPt = npt
	}
	c := VoltVarCurve{
		DeptRef:     regs[base+M705_Crv_DeptRef],
		Pri:         regs[base+M705_Crv_Pri],
		VRef:        ApplyScaleUint(regs[base+M705_Crv_VRef], vSF),
		VRefAutoEna: regs[base+M705_Crv_VRefAutoEna] == 1,
		VRefAutoTms: ApplyScaleUint(regs[base+M705_Crv_VRefAutoTms], rspTmsSF),
		RspTms:      ApplyScaleUint(regs[base+M705_Crv_RspTms], rspTmsSF),
	}
	ptBase := base + M705_Crv_PtOffset
	c.Pts = make([]VoltVarPoint, actPt)
	for i := 0; i < actPt; i++ {
		c.Pts[i] = VoltVarPoint{
			V:   ApplyScaleSigned(regs[ptBase+2*i], vSF),
			Var: ApplyScaleSigned(regs[ptBase+2*i+1], deptRefSF),
		}
	}
	return c, nil
}

// EncodeVoltVarCurve writes c into the writable curve (curveIdx=1) of regs.
// Returns the range of registers modified so the caller can WriteModel the
// exact slice: [curveStart, curveStart+headerSize+2×len(c.Pts)).
func EncodeVoltVarCurve(regs []uint16, c VoltVarCurve) (start, end int, err error) {
	if len(regs) < M705_CrvHdrSize {
		return 0, 0, fmt.Errorf("sunspec: M705 too short for header")
	}
	npt := int(regs[M705_NPt])
	if npt == 0 {
		return 0, 0, fmt.Errorf("sunspec: M705 NPt=0")
	}
	if len(c.Pts) > npt {
		return 0, 0, fmt.Errorf("sunspec: VoltVar curve has %d points but device NPt=%d", len(c.Pts), npt)
	}
	vSF       := int16(regs[M705_V_SF])
	deptRefSF := int16(regs[M705_DeptRef_SF])
	rspTmsSF  := int16(regs[M705_RspTms_SF])
	base := CurveBase705(1, npt) // always write to writable curve (index 1)
	if len(regs) < base+M705_Crv_PtOffset+2*npt {
		return 0, 0, fmt.Errorf("sunspec: M705 too short for writable curve")
	}
	regs[base+M705_Crv_ActPt]       = uint16(len(c.Pts))
	regs[base+M705_Crv_DeptRef]     = c.DeptRef
	regs[base+M705_Crv_Pri]         = c.Pri
	regs[base+M705_Crv_VRef]        = RawFromScaleUint(c.VRef, vSF)
	if c.VRefAutoEna {
		regs[base+M705_Crv_VRefAutoEna] = 1
	} else {
		regs[base+M705_Crv_VRefAutoEna] = 0
	}
	regs[base+M705_Crv_VRefAutoTms] = RawFromScaleUint(c.VRefAutoTms, rspTmsSF)
	regs[base+M705_Crv_RspTms]      = RawFromScaleUint(c.RspTms, rspTmsSF)
	regs[base+M705_Crv_ReadOnly]    = 0 // writable
	ptBase := base + M705_Crv_PtOffset
	for i, pt := range c.Pts {
		regs[ptBase+2*i]   = RawFromScaleSigned(pt.V, vSF)
		regs[ptBase+2*i+1] = RawFromScaleSigned(pt.Var, deptRefSF)
	}
	return base, ptBase + 2*npt, nil
}

// ── Model 706 (DERVoltWatt) curve types ──────────────────────────────────────

// VoltWattPoint is a (voltage, active power) point in a P(V) curve.
type VoltWattPoint struct {
	V float64 // Voltage (%)
	W float64 // Active power (% of WMax)
}

// VoltWattCurve is a P(V) curve as used by Model 706.
// IEEE 1547-2018 requires exactly 2 active points.
type VoltWattCurve struct {
	DeptRef uint16          // 0=W_MAX_PCT
	RspTms  float64         // Open loop response time (s)
	Pts     []VoltWattPoint // Active points (2 for IEEE 1547-2018)
}

// ParseVoltWattCurve reads one curve from an M706 register block.
func ParseVoltWattCurve(regs []uint16, curveIdx int) (VoltWattCurve, error) {
	if len(regs) < 8 {
		return VoltWattCurve{}, fmt.Errorf("sunspec: M706 too short for header")
	}
	npt := int(regs[M706_NPt])
	if npt == 0 || npt > 20 {
		return VoltWattCurve{}, fmt.Errorf("sunspec: M706 NPt=%d out of range", npt)
	}
	base := CurveBase706(curveIdx, npt)
	if len(regs) < base+M706_Crv_PtOffset+2*npt {
		return VoltWattCurve{}, fmt.Errorf("sunspec: M706 too short for curve %d", curveIdx)
	}
	vSF       := int16(regs[M706_V_SF])
	deptRefSF := int16(regs[M706_DeptRef_SF])
	rspTmsSF  := int16(regs[M706_RspTms_SF])
	actPt := int(regs[base+M706_Crv_ActPt])
	if actPt > npt {
		actPt = npt
	}
	c := VoltWattCurve{
		DeptRef: regs[base+M706_Crv_DeptRef],
		RspTms:  ApplyScaleUint(regs[base+M706_Crv_RspTms], rspTmsSF),
	}
	ptBase := base + M706_Crv_PtOffset
	c.Pts = make([]VoltWattPoint, actPt)
	for i := 0; i < actPt; i++ {
		c.Pts[i] = VoltWattPoint{
			V: ApplyScaleSigned(regs[ptBase+2*i], vSF),
			W: ApplyScaleSigned(regs[ptBase+2*i+1], deptRefSF),
		}
	}
	return c, nil
}

// EncodeVoltWattCurve writes c to the writable curve (index 1) of regs.
func EncodeVoltWattCurve(regs []uint16, c VoltWattCurve) (start, end int, err error) {
	if len(regs) < 8 {
		return 0, 0, fmt.Errorf("sunspec: M706 too short for header")
	}
	npt := int(regs[M706_NPt])
	if npt == 0 {
		return 0, 0, fmt.Errorf("sunspec: M706 NPt=0")
	}
	if len(c.Pts) > npt {
		return 0, 0, fmt.Errorf("sunspec: VoltWatt curve has %d points, device NPt=%d", len(c.Pts), npt)
	}
	vSF       := int16(regs[M706_V_SF])
	deptRefSF := int16(regs[M706_DeptRef_SF])
	rspTmsSF  := int16(regs[M706_RspTms_SF])
	base := CurveBase706(1, npt)
	if len(regs) < base+M706_Crv_PtOffset+2*npt {
		return 0, 0, fmt.Errorf("sunspec: M706 too short for writable curve")
	}
	regs[base+M706_Crv_ActPt]    = uint16(len(c.Pts))
	regs[base+M706_Crv_DeptRef]  = c.DeptRef
	regs[base+M706_Crv_RspTms]   = RawFromScaleUint(c.RspTms, rspTmsSF)
	regs[base+M706_Crv_ReadOnly] = 0
	ptBase := base + M706_Crv_PtOffset
	for i, pt := range c.Pts {
		regs[ptBase+2*i]   = RawFromScaleSigned(pt.V, vSF)
		regs[ptBase+2*i+1] = RawFromScaleSigned(pt.W, deptRefSF)
	}
	return base, ptBase + 2*npt, nil
}

// ── Models 707/708 (DERTripLV/HV) trip curve types ───────────────────────────

// VoltageTripPoint is a (voltage, time) point on a must-trip or momentary-
// cessation curve. V is in % of nominal; Tms is in seconds.
type VoltageTripPoint struct {
	V   float64 // Voltage threshold (%)
	Tms float64 // Clearing / cessation time (s)
}

// VoltageTripCurve holds the must-trip and optional momentary-cessation curves
// from one curve-set in Model 707 (low-voltage) or 708 (high-voltage).
// IEEE 1547-2018 requires 5 active points for MustTrip.
type VoltageTripCurve struct {
	MustTrip []VoltageTripPoint // Must-trip curve (5 points for IEEE 1547)
	MomCess  []VoltageTripPoint // Momentary cessation curve (optional)
}

// ParseVoltageTripCurve reads curve-set setIdx from a M707/M708 register block.
// setIdx=0 is read-only; setIdx=1 is writable.
// hasMomCess must match whether the device populates optional MomCess points.
func ParseVoltageTripCurve(regs []uint16, setIdx int, hasMomCess bool) (VoltageTripCurve, error) {
	if len(regs) < 7 {
		return VoltageTripCurve{}, fmt.Errorf("sunspec: M707/708 too short for header")
	}
	npt := int(regs[M707_NPt])
	if npt == 0 || npt > 20 {
		return VoltageTripCurve{}, fmt.Errorf("sunspec: M707/708 NPt=%d out of range", npt)
	}
	vSF   := int16(regs[M707_V_SF])
	tmsSF := int16(regs[M707_Tms_SF])

	base := CurveSetBase707(setIdx, npt, hasMomCess)
	actPt := int(regs[base+M707_CrvSet_MustTripActPt])
	if actPt > npt {
		actPt = npt
	}
	c := VoltageTripCurve{
		MustTrip: make([]VoltageTripPoint, actPt),
	}
	ptBase := base + M707_CrvSet_MustTripPtV
	for i := 0; i < actPt; i++ {
		c.MustTrip[i] = VoltageTripPoint{
			V:   ApplyScaleSigned(regs[ptBase+2*i], vSF),
			Tms: ApplyScaleSigned(regs[ptBase+2*i+1], tmsSF),
		}
	}
	if hasMomCess {
		mcBase := ptBase + 2*npt
		if len(regs) > mcBase+2 {
			mcActPt := int(regs[mcBase])
			if mcActPt > 0 {
				c.MomCess = make([]VoltageTripPoint, 1)
				c.MomCess[0] = VoltageTripPoint{
					V:   ApplyScaleSigned(regs[mcBase+1], vSF),
					Tms: ApplyScaleSigned(regs[mcBase+2], tmsSF),
				}
			}
		}
	}
	return c, nil
}

// EncodeVoltageTripCurve writes c to the writable curve-set (index 1) of regs.
func EncodeVoltageTripCurve(regs []uint16, c VoltageTripCurve, hasMomCess bool) (start, end int, err error) {
	if len(regs) < 7 {
		return 0, 0, fmt.Errorf("sunspec: M707/708 too short for header")
	}
	npt := int(regs[M707_NPt])
	if npt == 0 {
		return 0, 0, fmt.Errorf("sunspec: M707/708 NPt=0")
	}
	if len(c.MustTrip) > npt {
		return 0, 0, fmt.Errorf("sunspec: VoltageTripCurve has %d points, device NPt=%d", len(c.MustTrip), npt)
	}
	vSF   := int16(regs[M707_V_SF])
	tmsSF := int16(regs[M707_Tms_SF])
	base  := CurveSetBase707(1, npt, hasMomCess)
	regs[base+M707_CrvSet_ReadOnly]      = 0
	regs[base+M707_CrvSet_MustTripActPt] = uint16(len(c.MustTrip))
	ptBase := base + M707_CrvSet_MustTripPtV
	for i, pt := range c.MustTrip {
		regs[ptBase+2*i]   = RawFromScaleSigned(pt.V, vSF)
		regs[ptBase+2*i+1] = RawFromScaleSigned(pt.Tms, tmsSF)
	}
	endOff := ptBase + 2*npt
	if hasMomCess && len(c.MomCess) > 0 {
		regs[endOff] = 1 // ActPt
		regs[endOff+1] = RawFromScaleSigned(c.MomCess[0].V, vSF)
		regs[endOff+2] = RawFromScaleSigned(c.MomCess[0].Tms, tmsSF)
		endOff += 3
	}
	return base, endOff, nil
}

// ── Models 709/710 (DERTripLF/HF) frequency trip curve types ─────────────────

// FreqTripPoint is a (frequency, time) point. Hz in cycles/s; Tms in seconds.
type FreqTripPoint struct {
	Hz  float64
	Tms float64
}

// FreqTripCurve holds must-trip (and optional momentary-cessation) points for
// one curve-set in Model 709 (low-frequency) or 710 (high-frequency).
type FreqTripCurve struct {
	MustTrip []FreqTripPoint
	MomCess  []FreqTripPoint
}

// ParseFreqTripCurve reads curve-set setIdx from a M709/M710 register block.
func ParseFreqTripCurve(regs []uint16, setIdx int, hasMomCess bool) (FreqTripCurve, error) {
	if len(regs) < 7 {
		return FreqTripCurve{}, fmt.Errorf("sunspec: M709/710 too short for header")
	}
	npt := int(regs[M709_NPt])
	if npt == 0 || npt > 20 {
		return FreqTripCurve{}, fmt.Errorf("sunspec: M709/710 NPt=%d out of range", npt)
	}
	hzSF  := int16(regs[M709_Hz_SF])
	tmsSF := int16(regs[M709_Tms_SF])
	base  := CurveSetBase709(setIdx, npt, hasMomCess)
	actPt := int(regs[base+M709_CrvSet_MustTripActPt])
	if actPt > npt {
		actPt = npt
	}
	c := FreqTripCurve{MustTrip: make([]FreqTripPoint, actPt)}
	ptBase := base + M709_CrvSet_MustTripPtHz
	for i := 0; i < actPt; i++ {
		c.MustTrip[i] = FreqTripPoint{
			Hz:  ApplyScaleSigned(regs[ptBase+2*i], hzSF),
			Tms: ApplyScaleSigned(regs[ptBase+2*i+1], tmsSF),
		}
	}
	if hasMomCess {
		mcBase := ptBase + 2*npt
		if len(regs) > mcBase+2 {
			if regs[mcBase] > 0 {
				c.MomCess = []FreqTripPoint{{
					Hz:  ApplyScaleSigned(regs[mcBase+1], hzSF),
					Tms: ApplyScaleSigned(regs[mcBase+2], tmsSF),
				}}
			}
		}
	}
	return c, nil
}

// EncodeFreqTripCurve writes c to the writable curve-set (index 1) of regs.
func EncodeFreqTripCurve(regs []uint16, c FreqTripCurve, hasMomCess bool) (start, end int, err error) {
	if len(regs) < 7 {
		return 0, 0, fmt.Errorf("sunspec: M709/710 too short for header")
	}
	npt := int(regs[M709_NPt])
	if npt == 0 {
		return 0, 0, fmt.Errorf("sunspec: M709/710 NPt=0")
	}
	if len(c.MustTrip) > npt {
		return 0, 0, fmt.Errorf("sunspec: FreqTripCurve has %d points, device NPt=%d", len(c.MustTrip), npt)
	}
	hzSF  := int16(regs[M709_Hz_SF])
	tmsSF := int16(regs[M709_Tms_SF])
	base  := CurveSetBase709(1, npt, hasMomCess)
	regs[base+M709_CrvSet_ReadOnly]      = 0
	regs[base+M709_CrvSet_MustTripActPt] = uint16(len(c.MustTrip))
	ptBase := base + M709_CrvSet_MustTripPtHz
	for i, pt := range c.MustTrip {
		regs[ptBase+2*i]   = RawFromScaleSigned(pt.Hz, hzSF)
		regs[ptBase+2*i+1] = RawFromScaleSigned(pt.Tms, tmsSF)
	}
	endOff := ptBase + 2*npt
	if hasMomCess && len(c.MomCess) > 0 {
		regs[endOff]   = 1
		regs[endOff+1] = RawFromScaleSigned(c.MomCess[0].Hz, hzSF)
		regs[endOff+2] = RawFromScaleSigned(c.MomCess[0].Tms, tmsSF)
		endOff += 3
	}
	return base, endOff, nil
}

// ── Model 711 (DERFreqDroop) control set types ────────────────────────────────

// FreqDroopCtl holds the frequency droop parameters from one control set.
// IEEE 1547-2018 §2.13. DbOf / DbUf are deadbands in Hz; KOf / KUf are
// dimensionless droop slopes (ΔP/ΔHz); RspTms is response time in seconds.
type FreqDroopCtl struct {
	DbOf   float64 // Over-frequency deadband (Hz)
	DbUf   float64 // Under-frequency deadband (Hz)
	KOf    float64 // Over-frequency droop gain
	KUf    float64 // Under-frequency droop gain
	RspTms float64 // Open loop response time (s)
}

// ParseFreqDroop reads control set ctlIdx from a M711 register block.
// ctlIdx=0 is read-only; ctlIdx=1 is writable.
func ParseFreqDroop(regs []uint16, ctlIdx int) (FreqDroopCtl, error) {
	if len(regs) < M711_CtlStart {
		return FreqDroopCtl{}, fmt.Errorf("sunspec: M711 too short for header")
	}
	dbSF     := int16(regs[M711_Db_SF])
	kSF      := int16(regs[M711_K_SF])
	rspTmsSF := int16(regs[M711_RspTms_SF])
	base := CtlBase711(ctlIdx)
	if len(regs) < base+M711_CtlSize {
		return FreqDroopCtl{}, fmt.Errorf("sunspec: M711 too short for control set %d", ctlIdx)
	}
	return FreqDroopCtl{
		DbOf:   ApplyScaleUint(regs[base+M711_Ctl_DbOf], dbSF),
		DbUf:   ApplyScaleUint(regs[base+M711_Ctl_DbUf], dbSF),
		KOf:    ApplyScaleSigned(regs[base+M711_Ctl_KOf], kSF),
		KUf:    ApplyScaleSigned(regs[base+M711_Ctl_KUf], kSF),
		RspTms: ApplyScaleUint(regs[base+M711_Ctl_RspTms], rspTmsSF),
	}, nil
}

// EncodeFreqDroop writes c to the writable control set (index 1) of regs.
func EncodeFreqDroop(regs []uint16, c FreqDroopCtl) (start, end int, err error) {
	if len(regs) < M711_CtlStart {
		return 0, 0, fmt.Errorf("sunspec: M711 too short for header")
	}
	dbSF     := int16(regs[M711_Db_SF])
	kSF      := int16(regs[M711_K_SF])
	rspTmsSF := int16(regs[M711_RspTms_SF])
	base := CtlBase711(1)
	if len(regs) < base+M711_CtlSize {
		return 0, 0, fmt.Errorf("sunspec: M711 too short for writable control set")
	}
	regs[base+M711_Ctl_DbOf]     = RawFromScaleUint(c.DbOf, dbSF)
	regs[base+M711_Ctl_DbUf]     = RawFromScaleUint(c.DbUf, dbSF)
	regs[base+M711_Ctl_KOf]      = RawFromScaleSigned(c.KOf, kSF)
	regs[base+M711_Ctl_KUf]      = RawFromScaleSigned(c.KUf, kSF)
	regs[base+M711_Ctl_RspTms]   = RawFromScaleUint(c.RspTms, rspTmsSF)
	regs[base+M711_Ctl_ReadOnly] = 0
	return base, base + M711_CtlSize, nil
}

// ── Model 712 (DERWattVar) curve types ───────────────────────────────────────

// WattVarPoint is a (active power, reactive power) point in a Q(P) curve.
// Both values are expressed as percentages.
type WattVarPoint struct {
	W   float64 // Active power (% of WMax; may be negative for load operation)
	Var float64 // Reactive power (%)
}

// WattVarCurve is a Q(P) curve as used by Model 712.
// IEEE 1547-2018 requires exactly 6 points. Points 1-3 (index 0-2) cover
// load operation (W<0) and may be zero if not used. Points 4-6 (index 3-5)
// cover generation operation (W>0).
type WattVarCurve struct {
	DeptRef uint16         // 0=W_MAX_PCT, 1=VAR_MAX_PCT, 2=VA_MAX_PCT
	Pri     uint16         // 1=REACTIVE
	Pts     []WattVarPoint // 6 points for IEEE 1547-2018
}

// ParseWattVarCurve reads one curve from a M712 register block.
func ParseWattVarCurve(regs []uint16, curveIdx int) (WattVarCurve, error) {
	if len(regs) < 7 {
		return WattVarCurve{}, fmt.Errorf("sunspec: M712 too short for header")
	}
	npt := int(regs[M712_NPt])
	if npt == 0 || npt > 20 {
		return WattVarCurve{}, fmt.Errorf("sunspec: M712 NPt=%d out of range", npt)
	}
	base := CurveBase712(curveIdx, npt)
	if len(regs) < base+M712_Crv_PtOffset+2*npt {
		return WattVarCurve{}, fmt.Errorf("sunspec: M712 too short for curve %d", curveIdx)
	}
	wSF       := int16(regs[M712_W_SF])
	deptRefSF := int16(regs[M712_DeptRef_SF])
	actPt := int(regs[base+M712_Crv_ActPt])
	if actPt > npt {
		actPt = npt
	}
	c := WattVarCurve{
		DeptRef: regs[base+M712_Crv_DeptRef],
		Pri:     regs[base+M712_Crv_Pri],
		Pts:     make([]WattVarPoint, actPt),
	}
	ptBase := base + M712_Crv_PtOffset
	for i := 0; i < actPt; i++ {
		c.Pts[i] = WattVarPoint{
			W:   ApplyScaleSigned(regs[ptBase+2*i], wSF),
			Var: ApplyScaleSigned(regs[ptBase+2*i+1], deptRefSF),
		}
	}
	return c, nil
}

// EncodeWattVarCurve writes c to the writable curve (index 1) of regs.
func EncodeWattVarCurve(regs []uint16, c WattVarCurve) (start, end int, err error) {
	if len(regs) < 7 {
		return 0, 0, fmt.Errorf("sunspec: M712 too short for header")
	}
	npt := int(regs[M712_NPt])
	if npt == 0 {
		return 0, 0, fmt.Errorf("sunspec: M712 NPt=0")
	}
	if len(c.Pts) > npt {
		return 0, 0, fmt.Errorf("sunspec: WattVar curve has %d points, device NPt=%d", len(c.Pts), npt)
	}
	wSF       := int16(regs[M712_W_SF])
	deptRefSF := int16(regs[M712_DeptRef_SF])
	base := CurveBase712(1, npt)
	if len(regs) < base+M712_Crv_PtOffset+2*npt {
		return 0, 0, fmt.Errorf("sunspec: M712 too short for writable curve")
	}
	regs[base+M712_Crv_ActPt]    = uint16(len(c.Pts))
	regs[base+M712_Crv_DeptRef]  = c.DeptRef
	regs[base+M712_Crv_Pri]      = c.Pri
	regs[base+M712_Crv_ReadOnly] = 0
	ptBase := base + M712_Crv_PtOffset
	for i, pt := range c.Pts {
		regs[ptBase+2*i]   = RawFromScaleSigned(pt.W, wSF)
		regs[ptBase+2*i+1] = RawFromScaleSigned(pt.Var, deptRefSF)
	}
	return base, ptBase + 2*npt, nil
}

// ── Model 713 (DERStorageCapacity) ───────────────────────────────────────────

// DERStorageCapacity holds operational and nameplate storage data from M713.
// SoC and SoH are percentages (0-100). NaN means not reported.
type DERStorageCapacity struct {
	WHRtg     float64 // Nameplate energy capacity (Wh)
	AHRtg     float64 // Nameplate amp-hour capacity (Ah)
	MaxChaSoC float64 // Max charge state of charge (%)
	MinChaSoC float64 // Min charge state of charge (%)
	MaxChaPct float64 // Max charge rate % of rated charge rate
	SoC       float64 // State of charge (%)
	SoH       float64 // State of health (%)
	NCyc      uint32  // Lifetime cycle count
}

// ParseDERStorageCapacity converts raw M713 register data.
func ParseDERStorageCapacity(regs []uint16) (DERStorageCapacity, error) {
	if len(regs) < M713_SoC+1 {
		return DERStorageCapacity{}, fmt.Errorf("sunspec: M713 too short (%d regs)", len(regs))
	}
	get := func(i int) uint16 {
		if i < len(regs) {
			return regs[i]
		}
		return 0
	}
	whSF  := int16(get(M713_WHRtg_SF))
	ahSF  := int16(get(M713_AHRtg_SF))
	socSF := int16(get(M713_SoC_SF))
	sohSF := int16(get(M713_SoH_SF))
	pctSF := int16(get(M713_Pct_SF))

	d := DERStorageCapacity{
		WHRtg:     ApplyScaleUint(get(M713_WHRtg), whSF),
		AHRtg:     ApplyScaleUint(get(M713_AHRtg), ahSF),
		MaxChaSoC: ApplyScaleUint(get(M713_MaxChaSoC), socSF),
		MinChaSoC: ApplyScaleUint(get(M713_MinChaSoC), socSF),
		MaxChaPct: ApplyScaleUint(get(M713_MaxChaPct), pctSF),
		SoC:       ApplyScaleUint(get(M713_SoC), socSF),
		SoH:       ApplyScaleUint(get(M713_SoH), sohSF),
	}
	if len(regs) > M713_NCyc+1 {
		d.NCyc = uint32(get(M713_NCyc))<<16 | uint32(get(M713_NCyc+1))
	}
	return d, nil
}
