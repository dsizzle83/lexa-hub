// Package sunspec — register layouts for the SunSpec DER Information Models
// (701-714), transcribed point-for-point from the SunSpec DER Information Model
// Specification v1.2 in exact spec order and data type.
//
// Fixed-shape models (701, 702, 703, 704, 713, 714 header) are single Layouts.
// Curve/control models (705-712) and the 714 port group have a fixed header
// Layout followed by a variable number of repeating sub-groups whose stride
// depends on NPt/NCrv/NCtl read at runtime; offset helpers compute those.
package sunspec

// ModelDERMeasureDC is model 714 (added — was previously unimplemented).
const ModelDERMeasureDC = uint16(714)

// ── Model 701: DER AC Measurement ────────────────────────────────────────────
// Spec Table 4. Scale factors are declared after the per-phase block but are
// resolved by name, so forward references are fine.
var L701 = NewLayout(
	F("ACType", Tenum16),
	F("St", Tenum16),
	F("InvSt", Tenum16),
	F("ConnSt", Tenum16),
	F("Alrm", Tbitfield32),
	F("DERMode", Tbitfield32),
	FS("W", Tint16, "W_SF"),
	FS("VA", Tint16, "VA_SF"),
	FS("Var", Tint16, "Var_SF"),
	FS("PF", Tuint16, "PF_SF"),
	FS("A", Tint16, "A_SF"),
	FS("LLV", Tuint16, "V_SF"),
	FS("LNV", Tuint16, "V_SF"),
	FS("Hz", Tuint32, "Hz_SF"),
	FS("TotWhInj", Tuint64, "TotWh_SF"),
	FS("TotWhAbs", Tuint64, "TotWh_SF"),
	FS("TotVarhInj", Tuint64, "TotVarh_SF"),
	FS("TotVarhAbs", Tuint64, "TotVarh_SF"),
	FS("TmpAmb", Tint16, "Tmp_SF"),
	FS("TmpCab", Tint16, "Tmp_SF"),
	FS("TmpSnk", Tint16, "Tmp_SF"),
	FS("TmpTrns", Tint16, "Tmp_SF"),
	FS("TmpSw", Tint16, "Tmp_SF"),
	FS("TmpOt", Tint16, "Tmp_SF"),
	// Phase L1
	FS("WL1", Tint16, "W_SF"), FS("VAL1", Tint16, "VA_SF"), FS("VarL1", Tint16, "Var_SF"),
	FS("PFL1", Tuint16, "PF_SF"), FS("AL1", Tint16, "A_SF"),
	FS("VL1L2", Tuint16, "V_SF"), FS("VL1", Tuint16, "V_SF"),
	FS("TotWhInjL1", Tuint64, "TotWh_SF"), FS("TotWhAbsL1", Tuint64, "TotWh_SF"),
	FS("TotVarhInjL1", Tuint64, "TotVarh_SF"), FS("TotVarhAbsL1", Tuint64, "TotVarh_SF"),
	// Phase L2
	FS("WL2", Tint16, "W_SF"), FS("VAL2", Tint16, "VA_SF"), FS("VarL2", Tint16, "Var_SF"),
	FS("PFL2", Tuint16, "PF_SF"), FS("AL2", Tint16, "A_SF"),
	FS("VL2L3", Tuint16, "V_SF"), FS("VL2", Tuint16, "V_SF"),
	FS("TotWhInjL2", Tuint64, "TotWh_SF"), FS("TotWhAbsL2", Tuint64, "TotWh_SF"),
	FS("TotVarhInjL2", Tuint64, "TotVarh_SF"), FS("TotVarhAbsL2", Tuint64, "TotVarh_SF"),
	// Phase L3
	FS("WL3", Tint16, "W_SF"), FS("VAL3", Tint16, "VA_SF"), FS("VarL3", Tint16, "Var_SF"),
	FS("PFL3", Tuint16, "PF_SF"), FS("AL3", Tint16, "A_SF"),
	FS("VL3L1", Tuint16, "V_SF"), FS("VL3", Tuint16, "V_SF"),
	FS("TotWhInjL3", Tuint64, "TotWh_SF"), FS("TotWhAbsL3", Tuint64, "TotWh_SF"),
	FS("TotVarhInjL3", Tuint64, "TotVarh_SF"), FS("TotVarhAbsL3", Tuint64, "TotVarh_SF"),
	FS("ThrotPct", Tuint16, ""),
	F("ThrotSrc", Tbitfield32),
	// Scale factors
	F("A_SF", Tsunssf), F("V_SF", Tsunssf), F("Hz_SF", Tsunssf), F("W_SF", Tsunssf),
	F("PF_SF", Tsunssf), F("VA_SF", Tsunssf), F("Var_SF", Tsunssf),
	F("TotWh_SF", Tsunssf), F("TotVarh_SF", Tsunssf), F("Tmp_SF", Tsunssf),
	FStr("MnAlrmInfo", 16),
)

// ── Model 702: DER Capacity ──────────────────────────────────────────────────
// Spec Table 5.
var L702 = NewLayout(
	FS("WMaxRtg", Tuint16, "W_SF"),
	FS("WOvrExtRtg", Tuint16, "W_SF"), FS("WOvrExtRtgPF", Tuint16, "PF_SF"),
	FS("WUndExtRtg", Tuint16, "W_SF"), FS("WUndExtRtgPF", Tuint16, "PF_SF"),
	FS("VAMaxRtg", Tuint16, "VA_SF"),
	FS("VarMaxInjRtg", Tuint16, "Var_SF"), FS("VarMaxAbsRtg", Tuint16, "Var_SF"),
	FS("WChaRteMaxRtg", Tuint16, "W_SF"), FS("WDisChaRteMaxRtg", Tuint16, "W_SF"),
	FS("VAChaRteMaxRtg", Tuint16, "VA_SF"), FS("VADisChaRteMaxRtg", Tuint16, "VA_SF"),
	FS("VNomRtg", Tuint16, "V_SF"), FS("VMaxRtg", Tuint16, "V_SF"), FS("VMinRtg", Tuint16, "V_SF"),
	FS("AMaxRtg", Tuint16, "A_SF"),
	FS("PFOvrExtRtg", Tuint16, "PF_SF"), FS("PFUndExtRtg", Tuint16, "PF_SF"),
	FS("ReactSusceptRtg", Tuint16, "S_SF"),
	F("NorOpCatRtg", Tenum16), F("AbnOpCatRtg", Tenum16),
	F("CtrlModes", Tbitfield32), F("IntIslandCatRtg", Tbitfield16),
	// Settings (RW)
	FS("WMax", Tuint16, "W_SF"),
	FS("WMaxOvrExt", Tuint16, "W_SF"), FS("WOvrExtPF", Tuint16, "PF_SF"),
	FS("WMaxUndExt", Tuint16, "W_SF"), FS("WUndExtPF", Tuint16, "PF_SF"),
	FS("VAMax", Tuint16, "VA_SF"),
	FS("VarMaxInj", Tuint16, "Var_SF"), FS("VarMaxAbs", Tuint16, "Var_SF"),
	FS("WChaRteMax", Tuint16, "W_SF"), FS("WDisChaRteMax", Tuint16, "W_SF"),
	FS("VAChaRteMax", Tuint16, "VA_SF"), FS("VADisChaRteMax", Tuint16, "VA_SF"),
	FS("VNom", Tuint16, "V_SF"), FS("VMax", Tuint16, "V_SF"), FS("VMin", Tuint16, "V_SF"),
	FS("AMax", Tuint16, "A_SF"),
	FS("PFOvrExt", Tuint16, "PF_SF"), FS("PFUndExt", Tuint16, "PF_SF"),
	F("IntIslandCat", Tbitfield16),
	// Scale factors
	F("W_SF", Tsunssf), F("PF_SF", Tsunssf), F("VA_SF", Tsunssf), F("Var_SF", Tsunssf),
	F("V_SF", Tsunssf), F("A_SF", Tsunssf), F("S_SF", Tsunssf),
)

// ── Model 703: DER Enter Service ─────────────────────────────────────────────
// Spec Table 6. NOTE the corrected widths: ESHzHi/Lo and all timers are uint32.
var L703 = NewLayout(
	F("ES", Tenum16),
	FS("ESVHi", Tuint16, "V_SF"), FS("ESVLo", Tuint16, "V_SF"),
	FS("ESHzHi", Tuint32, "Hz_SF"), FS("ESHzLo", Tuint32, "Hz_SF"),
	F("ESDlyTms", Tuint32), F("ESRndTms", Tuint32), F("ESRmpTms", Tuint32),
	F("ESDlyRemTms", Tuint32),
	F("V_SF", Tsunssf), F("Hz_SF", Tsunssf),
)

// ── Model 704: DER AC Controls ───────────────────────────────────────────────
// Spec Table 7 — full model including reversion timers, Set Active Power,
// ramp rates, anti-islanding, and the four PF sync groups.
var L704 = NewLayout(
	F("PFWInjEna", Tenum16), F("PFWInjEnaRvrt", Tenum16),
	F("PFWInjRvrtTms", Tuint32), F("PFWInjRvrtRem", Tuint32),
	F("PFWAbsEna", Tenum16), F("PFWAbsEnaRvrt", Tenum16),
	F("PFWAbsRvrtTms", Tuint32), F("PFWAbsRvrtRem", Tuint32),
	F("WMaxLimPctEna", Tenum16),
	FS("WMaxLimPct", Tuint16, "WMaxLimPct_SF"), FS("WMaxLimPctRvrt", Tuint16, "WMaxLimPct_SF"),
	F("WMaxLimPctEnaRvrt", Tenum16), F("WMaxLimPctRvrtTms", Tuint32), F("WMaxLimPctRvrtRem", Tuint32),
	F("WSetEna", Tenum16), F("WSetMod", Tenum16),
	FS("WSet", Tint32, "WSet_SF"), FS("WSetRvrt", Tint32, "WSet_SF"),
	FS("WSetPct", Tint16, "WSetPct_SF"), FS("WSetPctRvrt", Tint16, "WSetPct_SF"),
	F("WSetEnaRvrt", Tenum16), F("WSetRvrtTms", Tuint32), F("WSetRvrtRem", Tuint32),
	F("VarSetEna", Tenum16), F("VarSetMod", Tenum16), F("VarSetPri", Tenum16),
	FS("VarSet", Tint32, "VarSet_SF"), FS("VarSetRvrt", Tint32, "VarSet_SF"),
	FS("VarSetPct", Tint16, "VarSetPct_SF"), FS("VarSetPctRvrt", Tint16, "VarSetPct_SF"),
	F("VarSetEnaRvrt", Tenum16), F("VarSetRvrtTms", Tuint32), F("VarSetRvrtRem", Tuint32),
	F("WRmp", Tuint16), F("WRmpRef", Tenum16), F("VarRmp", Tuint16), F("AntiIslEna", Tenum16),
	// Scale factors
	F("PF_SF", Tsunssf), F("WMaxLimPct_SF", Tsunssf), F("WSet_SF", Tsunssf),
	F("WSetPct_SF", Tsunssf), F("VarSet_SF", Tsunssf), F("VarSetPct_SF", Tsunssf),
	// PF sync groups (PF + excitation pairs), processed atomically.
	FS("PFWInj_PF", Tuint16, "PF_SF"), F("PFWInj_Ext", Tenum16),
	FS("PFWInjRvrt_PF", Tuint16, "PF_SF"), F("PFWInjRvrt_Ext", Tenum16),
	FS("PFWAbs_PF", Tuint16, "PF_SF"), F("PFWAbs_Ext", Tenum16),
	FS("PFWAbsRvrt_PF", Tuint16, "PF_SF"), F("PFWAbsRvrt_Ext", Tenum16),
)

// 704 enum values (CSIP↔SunSpec mapping helpers).
const (
	M704_WSetMod_MaxPct = 0 // WSetMod: active power as % of max
	M704_WSetMod_Watts  = 1 // WSetMod: active power as watts
	M704_VarSetMod_WMaxPct  = 0
	M704_VarSetMod_VarMaxPct = 1
	M704_VarSetMod_VarAvailPct = 2
	M704_VarSetMod_VAMaxPct = 3
	M704_VarSetMod_Vars     = 4
	M704_VarSetPri_Active   = 0
	M704_VarSetPri_Reactive = 1
	M704_Ext_OverExcited  = 0
	M704_Ext_UnderExcited = 1
	M704_WRmpRef_AMax = 0
	M704_WRmpRef_WMax = 1
)

// ── Curve models 705-712: fixed header + repeating sub-groups ────────────────

// Model 705 (DER Volt-Var) — Q(V). Spec Table 8.
var L705Hdr = NewLayout(
	F("Ena", Tenum16), F("AdptCrvReq", Tuint16), F("AdptCrvRslt", Tenum16),
	F("NPt", Tuint16), F("NCrv", Tuint16),
	F("RvrtTms", Tuint32), F("RvrtRem", Tuint32), F("RvrtCrv", Tuint16),
	F("V_SF", Tsunssf), F("DeptRef_SF", Tsunssf), F("RspTms_SF", Tsunssf),
)
var L705Crv = NewLayout(
	F("ActPt", Tuint16), F("DeptRef", Tenum16), F("Pri", Tenum16),
	FS("VRef", Tuint16, "V_SF"), FS("VRefAuto", Tuint16, "V_SF"),
	F("VRefAutoEna", Tenum16), F("VRefAutoTms", Tuint16),
	FS("RspTms", Tuint32, "RspTms_SF"), F("ReadOnly", Tenum16),
) // followed by NPt × {V uint16 V_SF, Var int16 DeptRef_SF}

// Model 706 (DER Volt-Watt) — P(V). Spec Table 9.
var L706Hdr = NewLayout(
	F("Ena", Tenum16), F("AdptCrvReq", Tuint16), F("AdptCrvRslt", Tenum16),
	F("NPt", Tuint16), F("NCrv", Tuint16),
	F("RvrtTms", Tuint32), F("RvrtRem", Tuint32), F("RvrtCrv", Tuint16),
	F("V_SF", Tsunssf), F("DeptRef_SF", Tsunssf), F("RspTms_SF", Tsunssf),
)
var L706Crv = NewLayout(
	F("ActPt", Tuint16), F("DeptRef", Tenum16),
	FS("RspTms", Tuint32, "RspTms_SF"), F("ReadOnly", Tenum16),
) // followed by NPt × {V uint16 V_SF, W int16 DeptRef_SF}

// Models 707/708 (DER Trip LV/HV). Spec Table 10/11. Three curves per set:
// MustTrip, MayTrip, MomCess — each {ActPt + NPt×(V uint16 V_SF, Tms uint32 Tms_SF)}.
var L707Hdr = NewLayout(
	F("Ena", Tenum16), F("AdptCrvReq", Tuint16), F("AdptCrvRslt", Tenum16),
	F("NPt", Tuint16), F("NCrvSet", Tuint16),
	F("V_SF", Tsunssf), F("Tms_SF", Tsunssf),
)

// Models 709/710 (DER Trip LF/HF). Spec Table 12/13. Frequency points are
// uint32, so each point is {Hz uint32 Hz_SF, Tms uint32 Tms_SF}.
var L709Hdr = NewLayout(
	F("Ena", Tenum16), F("AdptCrvReq", Tuint16), F("AdptCrvRslt", Tenum16),
	F("NPt", Tuint16), F("NCrvSet", Tuint16),
	F("Hz_SF", Tsunssf), F("Tms_SF", Tsunssf),
)

// Model 711 (DER Frequency Droop). Spec Table 14.
var L711Hdr = NewLayout(
	F("Ena", Tenum16), F("AdptCtlReq", Tuint16), F("AdptCtlRslt", Tenum16),
	F("NCtl", Tuint16),
	F("RvrtTms", Tuint32), F("RvrtRem", Tuint32), F("RvrtCtl", Tuint16),
	F("Db_SF", Tsunssf), F("K_SF", Tsunssf), F("RspTms_SF", Tsunssf),
)
var L711Ctl = NewLayout(
	FS("DbOf", Tuint32, "Db_SF"), FS("DbUf", Tuint32, "Db_SF"),
	FS("KOf", Tuint16, "K_SF"), FS("KUf", Tuint16, "K_SF"),
	FS("RspTms", Tuint32, "RspTms_SF"), F("PMin", Tint16), F("ReadOnly", Tenum16),
)

// Model 712 (DER Watt-Var) — Q(P). Spec Table 15.
var L712Hdr = NewLayout(
	F("Ena", Tenum16), F("AdptCrvReq", Tuint16), F("AdptCrvRslt", Tenum16),
	F("NPt", Tuint16), F("NCrv", Tuint16),
	F("RvrtTms", Tuint32), F("RvrtRem", Tuint32), F("RvrtCrv", Tuint16),
	F("W_SF", Tsunssf), F("DeptRef_SF", Tsunssf),
)
var L712Crv = NewLayout(
	F("ActPt", Tuint16), F("DeptRef", Tenum16), F("Pri", Tenum16), F("ReadOnly", Tenum16),
) // followed by NPt × {W int16 W_SF, Var int16 DeptRef_SF}

// ── Model 713: DER Storage Capacity ──────────────────────────────────────────
// Spec Table 16 — the small operational-SoC model (corrected from the prior
// non-spec layout).
var L713 = NewLayout(
	FS("WHRtg", Tuint16, "WH_SF"), FS("WHAvail", Tuint16, "WH_SF"),
	FS("SoC", Tuint16, "Pct_SF"), FS("SoH", Tuint16, "Pct_SF"),
	F("Sta", Tenum16),
	F("WH_SF", Tsunssf), F("Pct_SF", Tsunssf),
)

// ── Model 714: DER DC Measurement ────────────────────────────────────────────
// Spec Table 17. Fixed header followed by NPrt repeating port groups.
var L714Hdr = NewLayout(
	F("PrtAlrms", Tbitfield32), F("NPrt", Tuint16),
	FS("DCA", Tint16, "DCA_SF"), FS("DCW", Tint16, "DCW_SF"),
	FS("DCWhInj", Tuint64, "DCWH_SF"), FS("DCWhAbs", Tuint64, "DCWH_SF"),
	F("DCA_SF", Tsunssf), F("DCV_SF", Tsunssf), F("DCW_SF", Tsunssf),
	F("DCWH_SF", Tsunssf), F("Tmp_SF", Tsunssf),
)
var L714Prt = NewLayout(
	F("PrtTyp", Tenum16), F("ID", Tuint16), FStr("IDStr", 8),
	FS("DCA", Tint16, "DCA_SF"), FS("DCV", Tuint16, "DCV_SF"), FS("DCW", Tint16, "DCW_SF"),
	FS("DCWhInj", Tuint64, "DCWH_SF"), FS("DCWhAbs", Tuint64, "DCWH_SF"),
	FS("Tmp", Tint16, "Tmp_SF"), F("DCSta", Tenum16), F("DCAlrm", Tbitfield32),
)

// ── Curve/port offset helpers ────────────────────────────────────────────────
// These return the absolute (0-based) register offset of repeating sub-groups
// given the device-reported NPt. Curve index is 0-based: 0 is the read-only
// active curve, 1+ are writable staging curves.

// CurveOffset705 returns the offset of curve i in a 705 register block.
func CurveOffset705(i, npt int) int { return L705Hdr.Len() + i*(L705Crv.Len()+2*npt) }

// PointOffset705 returns the offset of point j (V) within curve i of a 705 block.
func PointOffset705(i, j, npt int) int { return CurveOffset705(i, npt) + L705Crv.Len() + 2*j }

func CurveOffset706(i, npt int) int { return L706Hdr.Len() + i*(L706Crv.Len()+2*npt) }
func PointOffset706(i, j, npt int) int { return CurveOffset706(i, npt) + L706Crv.Len() + 2*j }

func CurveOffset712(i, npt int) int { return L712Hdr.Len() + i*(L712Crv.Len()+2*npt) }
func PointOffset712(i, j, npt int) int { return CurveOffset712(i, npt) + L712Crv.Len() + 2*j }

func CtlOffset711(i int) int { return L711Hdr.Len() + i*L711Ctl.Len() }

// Voltage trip (707/708): one curve-set = ReadOnly(1) + 3×(ActPt(1)+NPt×(V(1)+Tms(2))).
const tripVPtRegs = 3 // V(uint16)=1 + Tms(uint32)=2

func tripVSetSize(npt int) int { return 1 + 3*(1+npt*tripVPtRegs) }
func TripVSetOffset(i, npt int) int { return L707Hdr.Len() + i*tripVSetSize(npt) }

// SubCurveOffset707 returns the offset of sub-curve s (0=MustTrip,1=MayTrip,
// 2=MomCess) within curve-set i; the first register there is that sub-curve's ActPt.
func SubCurveOffset707(i, s, npt int) int {
	return TripVSetOffset(i, npt) + 1 + s*(1+npt*tripVPtRegs)
}

// Frequency trip (709/710): point = Hz(uint32)=2 + Tms(uint32)=2 = 4 regs.
const tripHzPtRegs = 4

func tripHzSetSize(npt int) int { return 1 + 3*(1+npt*tripHzPtRegs) }
func TripHzSetOffset(i, npt int) int { return L709Hdr.Len() + i*tripHzSetSize(npt) }
func SubCurveOffset709(i, s, npt int) int {
	return TripHzSetOffset(i, npt) + 1 + s*(1+npt*tripHzPtRegs)
}

// PortOffset714 returns the offset of DC port i within a 714 block.
func PortOffset714(i int) int { return L714Hdr.Len() + i*L714Prt.Len() }

// Trip sub-curve indices.
const (
	SubMustTrip = 0
	SubMayTrip  = 1
	SubMomCess  = 2
)
