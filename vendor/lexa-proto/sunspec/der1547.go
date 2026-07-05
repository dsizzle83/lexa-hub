// Package sunspec — typed parse/encode for the SunSpec DER Information Models
// (701-714), built on the declarative layout engine (layout.go / derlayout.go).
//
// Reads return rich structs with engineering-unit values (NaN where a point is
// unimplemented on the device). Writes for single-function controls are done by
// callers manipulating a layout View directly (see internal/southbound/derbase);
// the curve models expose structured parse/encode here because their repeating
// point groups are non-trivial.
package sunspec

import (
	"fmt"
	"math"
	"strings"
)

// Adopt-curve / adopt-control result (AdptCrvRslt / AdptCtlRslt).
const (
	AdptInProgress = 0
	AdptCompleted  = 1
	AdptFailed     = 2
)

// readString decodes a SunSpec fixed-length string field (2 chars/register,
// NUL- or space-padded).
func readString(regs []uint16, off, regLen int) string {
	var b strings.Builder
	for i := 0; i < regLen && off+i < len(regs); i++ {
		r := regs[off+i]
		if hi := byte(r >> 8); hi != 0 {
			b.WriteByte(hi)
		}
		if lo := byte(r); lo != 0 {
			b.WriteByte(lo)
		}
	}
	return strings.TrimRight(b.String(), " \x00")
}

// ── Model 701: DER AC Measurement ────────────────────────────────────────────

// ACMeasurement is the full decoded DER AC measurement (model 701).
type ACMeasurement struct {
	ACType  uint16  // 0=single, 1=split, 2=three-phase
	St      uint16  // operating state (0=off,1=on)
	InvSt   uint16  // inverter state (0..7)
	ConnSt  uint16  // 0=disconnected, 1=connected
	Alrm    uint32  // alarm bitfield
	DERMode uint32  // operational characteristics bitfield
	W, VA, Var, PF, A   float64
	LLV, LNV, VL1, Hz   float64
	TotWhInj, TotWhAbs       float64
	TotVarhInj, TotVarhAbs   float64
	TmpCab  float64
	ThrotPct float64
	ThrotSrc uint32
	MnAlrmInfo string
}

// Parse701 decodes model 701 registers.
func Parse701(regs []uint16) ACMeasurement {
	v := L701.View(regs)
	e := func(n string) uint16 { x, _ := v.Enum(n); return x }
	return ACMeasurement{
		ACType:  e("ACType"),
		St:      e("St"),
		InvSt:   e("InvSt"),
		ConnSt:  e("ConnSt"),
		Alrm:    v.Bitfield32("Alrm"),
		DERMode: v.Bitfield32("DERMode"),
		W:       v.Float("W"),
		VA:      v.Float("VA"),
		Var:     v.Float("Var"),
		PF:      v.Float("PF"), // engineering value = power factor (raw × 10^SF)
		A:       v.Float("A"),
		LLV:     v.Float("LLV"),
		LNV:     v.Float("LNV"),
		VL1:     v.Float("VL1"),
		Hz:      v.Float("Hz"),
		TotWhInj:   v.Float("TotWhInj"),
		TotWhAbs:   v.Float("TotWhAbs"),
		TotVarhInj: v.Float("TotVarhInj"),
		TotVarhAbs: v.Float("TotVarhAbs"),
		TmpCab:     v.Float("TmpCab"),
		ThrotPct:   v.Float("ThrotPct"),
		ThrotSrc:   v.Bitfield32("ThrotSrc"),
		MnAlrmInfo: readString(regs, L701.Offset("MnAlrmInfo"), 16),
	}
}

// ── Model 702: DER Capacity ──────────────────────────────────────────────────

// Capacity holds the model 702 nameplate ratings and configuration setpoints.
// Setting fields are NaN when the device does not implement the optional block.
type Capacity struct {
	WMaxRtg, WOvrExtRtg, WOvrExtRtgPF, WUndExtRtg, WUndExtRtgPF float64
	VAMaxRtg, VarMaxInjRtg, VarMaxAbsRtg                        float64
	WChaRteMaxRtg, WDisChaRteMaxRtg, VAChaRteMaxRtg, VADisChaRteMaxRtg float64
	VNomRtg, VMaxRtg, VMinRtg, AMaxRtg                          float64
	PFOvrExtRtg, PFUndExtRtg, ReactSusceptRtg                   float64
	NorOpCatRtg, AbnOpCatRtg uint16
	CtrlModes        uint32
	IntIslandCatRtg  uint16
	// Settings (RW)
	WMax, WMaxOvrExt, WOvrExtPF, WMaxUndExt, WUndExtPF float64
	VAMax, VarMaxInj, VarMaxAbs                        float64
	WChaRteMax, WDisChaRteMax, VAChaRteMax, VADisChaRteMax float64
	VNom, VMax, VMin, AMax, PFOvrExt, PFUndExt         float64
}

// Parse702 decodes model 702 registers.
func Parse702(regs []uint16) Capacity {
	v := L702.View(regs)
	pf := func(n string) float64 { return v.Float(n) }
	e := func(n string) uint16 { x, _ := v.Enum(n); return x }
	return Capacity{
		WMaxRtg: v.Float("WMaxRtg"), WOvrExtRtg: v.Float("WOvrExtRtg"), WOvrExtRtgPF: pf("WOvrExtRtgPF"),
		WUndExtRtg: v.Float("WUndExtRtg"), WUndExtRtgPF: pf("WUndExtRtgPF"),
		VAMaxRtg: v.Float("VAMaxRtg"), VarMaxInjRtg: v.Float("VarMaxInjRtg"), VarMaxAbsRtg: v.Float("VarMaxAbsRtg"),
		WChaRteMaxRtg: v.Float("WChaRteMaxRtg"), WDisChaRteMaxRtg: v.Float("WDisChaRteMaxRtg"),
		VAChaRteMaxRtg: v.Float("VAChaRteMaxRtg"), VADisChaRteMaxRtg: v.Float("VADisChaRteMaxRtg"),
		VNomRtg: v.Float("VNomRtg"), VMaxRtg: v.Float("VMaxRtg"), VMinRtg: v.Float("VMinRtg"), AMaxRtg: v.Float("AMaxRtg"),
		PFOvrExtRtg: pf("PFOvrExtRtg"), PFUndExtRtg: pf("PFUndExtRtg"), ReactSusceptRtg: v.Float("ReactSusceptRtg"),
		NorOpCatRtg: e("NorOpCatRtg"), AbnOpCatRtg: e("AbnOpCatRtg"),
		CtrlModes: v.Bitfield32("CtrlModes"), IntIslandCatRtg: e("IntIslandCatRtg"),
		WMax: v.Float("WMax"), WMaxOvrExt: v.Float("WMaxOvrExt"), WOvrExtPF: pf("WOvrExtPF"),
		WMaxUndExt: v.Float("WMaxUndExt"), WUndExtPF: pf("WUndExtPF"), VAMax: v.Float("VAMax"),
		VarMaxInj: v.Float("VarMaxInj"), VarMaxAbs: v.Float("VarMaxAbs"),
		WChaRteMax: v.Float("WChaRteMax"), WDisChaRteMax: v.Float("WDisChaRteMax"),
		VAChaRteMax: v.Float("VAChaRteMax"), VADisChaRteMax: v.Float("VADisChaRteMax"),
		VNom: v.Float("VNom"), VMax: v.Float("VMax"), VMin: v.Float("VMin"), AMax: v.Float("AMax"),
		PFOvrExt: pf("PFOvrExt"), PFUndExt: pf("PFUndExt"),
	}
}

// ── Model 703: DER Enter Service ─────────────────────────────────────────────

// EnterService holds model 703 enter-service / cease-to-energize settings.
type EnterService struct {
	Enabled bool
	VHi, VLo, HzHi, HzLo float64
	DelayS, RandomS, RampS uint32
	DelayRemS uint32
}

// Parse703 decodes model 703.
func Parse703(regs []uint16) EnterService {
	v := L703.View(regs)
	u32 := func(n string) uint32 { x, _ := v.U32(n); return x }
	return EnterService{
		Enabled: v.Bool("ES"),
		VHi:     v.Float("ESVHi"), VLo: v.Float("ESVLo"),
		HzHi:    v.Float("ESHzHi"), HzLo: v.Float("ESHzLo"),
		DelayS:  u32("ESDlyTms"), RandomS: u32("ESRndTms"), RampS: u32("ESRmpTms"),
		DelayRemS: u32("ESDlyRemTms"),
	}
}

// Encode703 writes the writable model 703 fields into a freshly-read register
// slice. Scale factors must already be present in regs (read the model first).
func Encode703(regs []uint16, s EnterService) error {
	if len(regs) < L703.Len() {
		return fmt.Errorf("sunspec: M703 slice too short (%d < %d)", len(regs), L703.Len())
	}
	v := L703.View(regs)
	v.SetBool("ES", s.Enabled)
	v.SetFloat("ESVHi", s.VHi)
	v.SetFloat("ESVLo", s.VLo)
	v.SetFloat("ESHzHi", s.HzHi)
	v.SetFloat("ESHzLo", s.HzLo)
	v.SetU32("ESDlyTms", s.DelayS)
	v.SetU32("ESRndTms", s.RandomS)
	v.SetU32("ESRmpTms", s.RampS)
	return nil
}

// ── Model 704: DER AC Controls ───────────────────────────────────────────────

// ACControls is a read-back snapshot of model 704. Reversion-timer and ramp
// fields are included for completeness/diagnostics.
type ACControls struct {
	PFWInjEna  bool
	PFWInjPF   float64
	PFWInjExt  uint16
	PFWAbsEna  bool
	PFWAbsPF   float64
	PFWAbsExt  uint16
	WMaxLimPctEna bool
	WMaxLimPct float64
	WSetEna    bool
	WSetMod    uint16
	WSet       float64 // watts
	WSetPct    float64 // % of max
	VarSetEna  bool
	VarSetMod  uint16
	VarSetPri  uint16
	VarSet     float64
	VarSetPct  float64
	WRmp, VarRmp uint16
	WRmpRef    uint16
	AntiIslEna bool
}

// Parse704 decodes model 704.
func Parse704(regs []uint16) ACControls {
	v := L704.View(regs)
	e := func(n string) uint16 { x, _ := v.Enum(n); return x }
	return ACControls{
		PFWInjEna: v.Bool("PFWInjEna"), PFWInjPF: v.Float("PFWInj_PF"), PFWInjExt: e("PFWInj_Ext"),
		PFWAbsEna: v.Bool("PFWAbsEna"), PFWAbsPF: v.Float("PFWAbs_PF"), PFWAbsExt: e("PFWAbs_Ext"),
		WMaxLimPctEna: v.Bool("WMaxLimPctEna"), WMaxLimPct: v.Float("WMaxLimPct"),
		WSetEna: v.Bool("WSetEna"), WSetMod: e("WSetMod"), WSet: v.Float("WSet"), WSetPct: v.Float("WSetPct"),
		VarSetEna: v.Bool("VarSetEna"), VarSetMod: e("VarSetMod"), VarSetPri: e("VarSetPri"),
		VarSet: v.Float("VarSet"), VarSetPct: v.Float("VarSetPct"),
		WRmp: e("WRmp"), VarRmp: e("VarRmp"), WRmpRef: e("WRmpRef"), AntiIslEna: v.Bool("AntiIslEna"),
	}
}

// ── Curve helpers shared by 705/706/712 ──────────────────────────────────────

func curveNPt(hdr *Layout, regs []uint16, maxNPt int) (int, error) {
	if len(regs) < hdr.Len() {
		return 0, fmt.Errorf("sunspec: curve model too short for header (%d < %d)", len(regs), hdr.Len())
	}
	npt := int(hdr.View(regs).reg(hdr.Offset("NPt")))
	if npt == 0 || npt > maxNPt {
		return 0, fmt.Errorf("sunspec: curve NPt=%d out of range", npt)
	}
	return npt, nil
}

// ── Model 705: DER Volt-Var ──────────────────────────────────────────────────

type VVPoint struct{ V, Var float64 }

type VoltVarCurve struct {
	ReadOnly    bool
	DeptRef     uint16
	Pri         uint16
	VRef        float64
	VRefAutoEna bool
	VRefAutoTms float64
	RspTms      float64
	Points      []VVPoint
}

// Parse705Curve reads curve i (0=active read-only, 1+=staging) from a 705 block.
func Parse705Curve(regs []uint16, i int) (VoltVarCurve, error) {
	npt, err := curveNPt(L705Hdr, regs, 64)
	if err != nil {
		return VoltVarCurve{}, err
	}
	h := L705Hdr.View(regs)
	base := CurveOffset705(i, npt)
	co := func(p string) int { return base + L705Crv.Offset(p) }
	if base+L705Crv.Len()+2*npt > len(regs) {
		return VoltVarCurve{}, fmt.Errorf("sunspec: M705 too short for curve %d", i)
	}
	actPt := int(h.U16At(co("ActPt")))
	if actPt > npt {
		actPt = npt
	}
	c := VoltVarCurve{
		ReadOnly:    h.U16At(co("ReadOnly")) == 1,
		DeptRef:     h.U16At(co("DeptRef")),
		Pri:         h.U16At(co("Pri")),
		VRef:        h.ScaleUintAt(co("VRef"), "V_SF"),
		VRefAutoEna: h.U16At(co("VRefAutoEna")) == 1,
		VRefAutoTms: float64(h.U16At(co("VRefAutoTms"))),
		RspTms:      h.ScaleU32At(co("RspTms"), "RspTms_SF"),
		Points:      make([]VVPoint, actPt),
	}
	for j := 0; j < actPt; j++ {
		po := PointOffset705(i, j, npt)
		c.Points[j] = VVPoint{V: h.ScaleUintAt(po, "V_SF"), Var: h.ScaleSignedAt(po+1, "DeptRef_SF")}
	}
	return c, nil
}

// Encode705Curve writes c into staging curve index i (≥1) of regs, returning
// the modified register range [start,end). SFs must already be in regs.
func Encode705Curve(regs []uint16, i int, c VoltVarCurve) (start, end int, err error) {
	npt, err := curveNPt(L705Hdr, regs, 64)
	if err != nil {
		return 0, 0, err
	}
	if len(c.Points) > npt {
		return 0, 0, fmt.Errorf("sunspec: VoltVar curve has %d points, device NPt=%d", len(c.Points), npt)
	}
	h := L705Hdr.View(regs)
	base := CurveOffset705(i, npt)
	if base+L705Crv.Len()+2*npt > len(regs) {
		return 0, 0, fmt.Errorf("sunspec: M705 too short for curve %d", i)
	}
	co := func(p string) int { return base + L705Crv.Offset(p) }
	h.SetU16At(co("ActPt"), uint16(len(c.Points)))
	h.SetU16At(co("DeptRef"), c.DeptRef)
	h.SetU16At(co("Pri"), c.Pri)
	h.SetScaledUintAt(co("VRef"), c.VRef, "V_SF")
	if c.VRefAutoEna {
		h.SetU16At(co("VRefAutoEna"), 1)
	} else {
		h.SetU16At(co("VRefAutoEna"), 0)
	}
	h.SetU16At(co("VRefAutoTms"), uint16(c.VRefAutoTms))
	h.SetScaledU32At(co("RspTms"), c.RspTms, "RspTms_SF")
	h.SetU16At(co("ReadOnly"), 0)
	for j, p := range c.Points {
		po := PointOffset705(i, j, npt)
		h.SetScaledUintAt(po, p.V, "V_SF")
		h.SetScaledSignedAt(po+1, p.Var, "DeptRef_SF")
	}
	return base, base + L705Crv.Len() + 2*npt, nil
}

// ── Model 706: DER Volt-Watt ─────────────────────────────────────────────────

type VWPoint struct{ V, W float64 }

type VoltWattCurve struct {
	ReadOnly bool
	DeptRef  uint16
	RspTms   float64
	Points   []VWPoint
}

func Parse706Curve(regs []uint16, i int) (VoltWattCurve, error) {
	npt, err := curveNPt(L706Hdr, regs, 64)
	if err != nil {
		return VoltWattCurve{}, err
	}
	h := L706Hdr.View(regs)
	base := CurveOffset706(i, npt)
	if base+L706Crv.Len()+2*npt > len(regs) {
		return VoltWattCurve{}, fmt.Errorf("sunspec: M706 too short for curve %d", i)
	}
	co := func(p string) int { return base + L706Crv.Offset(p) }
	actPt := int(h.U16At(co("ActPt")))
	if actPt > npt {
		actPt = npt
	}
	c := VoltWattCurve{
		ReadOnly: h.U16At(co("ReadOnly")) == 1,
		DeptRef:  h.U16At(co("DeptRef")),
		RspTms:   h.ScaleU32At(co("RspTms"), "RspTms_SF"),
		Points:   make([]VWPoint, actPt),
	}
	for j := 0; j < actPt; j++ {
		po := PointOffset706(i, j, npt)
		c.Points[j] = VWPoint{V: h.ScaleUintAt(po, "V_SF"), W: h.ScaleSignedAt(po+1, "DeptRef_SF")}
	}
	return c, nil
}

func Encode706Curve(regs []uint16, i int, c VoltWattCurve) (start, end int, err error) {
	npt, err := curveNPt(L706Hdr, regs, 64)
	if err != nil {
		return 0, 0, err
	}
	if len(c.Points) > npt {
		return 0, 0, fmt.Errorf("sunspec: VoltWatt curve has %d points, device NPt=%d", len(c.Points), npt)
	}
	h := L706Hdr.View(regs)
	base := CurveOffset706(i, npt)
	if base+L706Crv.Len()+2*npt > len(regs) {
		return 0, 0, fmt.Errorf("sunspec: M706 too short for curve %d", i)
	}
	co := func(p string) int { return base + L706Crv.Offset(p) }
	h.SetU16At(co("ActPt"), uint16(len(c.Points)))
	h.SetU16At(co("DeptRef"), c.DeptRef)
	h.SetScaledU32At(co("RspTms"), c.RspTms, "RspTms_SF")
	h.SetU16At(co("ReadOnly"), 0)
	for j, p := range c.Points {
		po := PointOffset706(i, j, npt)
		h.SetScaledUintAt(po, p.V, "V_SF")
		h.SetScaledSignedAt(po+1, p.W, "DeptRef_SF")
	}
	return base, base + L706Crv.Len() + 2*npt, nil
}

// ── Models 707/708: Voltage trip (three curves per set) ──────────────────────

type TripVPoint struct{ V, Tms float64 }

// VoltageTripSet holds the must-trip, may-trip, and momentary-cessation curves
// of one curve-set in model 707 (LV) or 708 (HV).
type VoltageTripSet struct {
	ReadOnly bool
	MustTrip []TripVPoint
	MayTrip  []TripVPoint
	MomCess  []TripVPoint
}

func parseTripVSub(h View, i, sub, npt int) []TripVPoint {
	subOff := SubCurveOffset707(i, sub, npt)
	actPt := int(h.U16At(subOff))
	if actPt > npt {
		actPt = npt
	}
	pts := make([]TripVPoint, actPt)
	for j := 0; j < actPt; j++ {
		po := subOff + 1 + j*tripVPtRegs
		pts[j] = TripVPoint{V: h.ScaleUintAt(po, "V_SF"), Tms: h.ScaleU32At(po+1, "Tms_SF")}
	}
	return pts
}

// Parse707Set reads curve-set i from a 707/708 register block.
func Parse707Set(regs []uint16, i int) (VoltageTripSet, error) {
	npt, err := curveNPt(L707Hdr, regs, 64)
	if err != nil {
		return VoltageTripSet{}, err
	}
	if TripVSetOffset(i, npt)+tripVSetSize(npt) > len(regs) {
		return VoltageTripSet{}, fmt.Errorf("sunspec: M707/708 too short for set %d", i)
	}
	h := L707Hdr.View(regs)
	return VoltageTripSet{
		ReadOnly: h.U16At(TripVSetOffset(i, npt)) == 1,
		MustTrip: parseTripVSub(h, i, SubMustTrip, npt),
		MayTrip:  parseTripVSub(h, i, SubMayTrip, npt),
		MomCess:  parseTripVSub(h, i, SubMomCess, npt),
	}, nil
}

func encodeTripVSub(h View, i, sub, npt int, pts []TripVPoint) {
	subOff := SubCurveOffset707(i, sub, npt)
	h.SetU16At(subOff, uint16(len(pts)))
	for j, p := range pts {
		po := subOff + 1 + j*tripVPtRegs
		h.SetScaledUintAt(po, p.V, "V_SF")
		h.SetScaledU32At(po+1, p.Tms, "Tms_SF")
	}
}

// Encode707Set writes curve-set i into a 707/708 block.
func Encode707Set(regs []uint16, i int, s VoltageTripSet) (start, end int, err error) {
	npt, err := curveNPt(L707Hdr, regs, 64)
	if err != nil {
		return 0, 0, err
	}
	for _, c := range [][]TripVPoint{s.MustTrip, s.MayTrip, s.MomCess} {
		if len(c) > npt {
			return 0, 0, fmt.Errorf("sunspec: trip curve has %d points, device NPt=%d", len(c), npt)
		}
	}
	base := TripVSetOffset(i, npt)
	if base+tripVSetSize(npt) > len(regs) {
		return 0, 0, fmt.Errorf("sunspec: M707/708 too short for set %d", i)
	}
	h := L707Hdr.View(regs)
	h.SetU16At(base, 0) // ReadOnly = RW
	encodeTripVSub(h, i, SubMustTrip, npt, s.MustTrip)
	encodeTripVSub(h, i, SubMayTrip, npt, s.MayTrip)
	encodeTripVSub(h, i, SubMomCess, npt, s.MomCess)
	return base, base + tripVSetSize(npt), nil
}

// ── Models 709/710: Frequency trip (three curves per set) ────────────────────

type TripHzPoint struct{ Hz, Tms float64 }

type FreqTripSet struct {
	ReadOnly bool
	MustTrip []TripHzPoint
	MayTrip  []TripHzPoint
	MomCess  []TripHzPoint
}

func parseTripHzSub(h View, i, sub, npt int) []TripHzPoint {
	subOff := SubCurveOffset709(i, sub, npt)
	actPt := int(h.U16At(subOff))
	if actPt > npt {
		actPt = npt
	}
	pts := make([]TripHzPoint, actPt)
	for j := 0; j < actPt; j++ {
		po := subOff + 1 + j*tripHzPtRegs
		pts[j] = TripHzPoint{Hz: h.ScaleU32At(po, "Hz_SF"), Tms: h.ScaleU32At(po+2, "Tms_SF")}
	}
	return pts
}

func Parse709Set(regs []uint16, i int) (FreqTripSet, error) {
	npt, err := curveNPt(L709Hdr, regs, 64)
	if err != nil {
		return FreqTripSet{}, err
	}
	if TripHzSetOffset(i, npt)+tripHzSetSize(npt) > len(regs) {
		return FreqTripSet{}, fmt.Errorf("sunspec: M709/710 too short for set %d", i)
	}
	h := L709Hdr.View(regs)
	return FreqTripSet{
		ReadOnly: h.U16At(TripHzSetOffset(i, npt)) == 1,
		MustTrip: parseTripHzSub(h, i, SubMustTrip, npt),
		MayTrip:  parseTripHzSub(h, i, SubMayTrip, npt),
		MomCess:  parseTripHzSub(h, i, SubMomCess, npt),
	}, nil
}

func encodeTripHzSub(h View, i, sub, npt int, pts []TripHzPoint) {
	subOff := SubCurveOffset709(i, sub, npt)
	h.SetU16At(subOff, uint16(len(pts)))
	for j, p := range pts {
		po := subOff + 1 + j*tripHzPtRegs
		h.SetScaledU32At(po, p.Hz, "Hz_SF")
		h.SetScaledU32At(po+2, p.Tms, "Tms_SF")
	}
}

func Encode709Set(regs []uint16, i int, s FreqTripSet) (start, end int, err error) {
	npt, err := curveNPt(L709Hdr, regs, 64)
	if err != nil {
		return 0, 0, err
	}
	for _, c := range [][]TripHzPoint{s.MustTrip, s.MayTrip, s.MomCess} {
		if len(c) > npt {
			return 0, 0, fmt.Errorf("sunspec: freq trip curve has %d points, device NPt=%d", len(c), npt)
		}
	}
	base := TripHzSetOffset(i, npt)
	if base+tripHzSetSize(npt) > len(regs) {
		return 0, 0, fmt.Errorf("sunspec: M709/710 too short for set %d", i)
	}
	h := L709Hdr.View(regs)
	h.SetU16At(base, 0)
	encodeTripHzSub(h, i, SubMustTrip, npt, s.MustTrip)
	encodeTripHzSub(h, i, SubMayTrip, npt, s.MayTrip)
	encodeTripHzSub(h, i, SubMomCess, npt, s.MomCess)
	return base, base + tripHzSetSize(npt), nil
}

// ── Model 711: DER Frequency Droop ───────────────────────────────────────────

type FreqDroopCtl struct {
	ReadOnly bool
	DbOf, DbUf, KOf, KUf, RspTms float64
	PMin float64
}

func Parse711Ctl(regs []uint16, i int) (FreqDroopCtl, error) {
	if len(regs) < L711Hdr.Len() {
		return FreqDroopCtl{}, fmt.Errorf("sunspec: M711 too short for header")
	}
	h := L711Hdr.View(regs)
	base := CtlOffset711(i)
	if base+L711Ctl.Len() > len(regs) {
		return FreqDroopCtl{}, fmt.Errorf("sunspec: M711 too short for ctl %d", i)
	}
	co := func(p string) int { return base + L711Ctl.Offset(p) }
	return FreqDroopCtl{
		ReadOnly: h.U16At(co("ReadOnly")) == 1,
		DbOf:     h.ScaleU32At(co("DbOf"), "Db_SF"), DbUf: h.ScaleU32At(co("DbUf"), "Db_SF"),
		KOf:      h.ScaleUintAt(co("KOf"), "K_SF"), KUf: h.ScaleUintAt(co("KUf"), "K_SF"),
		RspTms:   h.ScaleU32At(co("RspTms"), "RspTms_SF"),
		PMin:     float64(h.I16At(co("PMin"))),
	}, nil
}

func Encode711Ctl(regs []uint16, i int, c FreqDroopCtl) (start, end int, err error) {
	if len(regs) < L711Hdr.Len() {
		return 0, 0, fmt.Errorf("sunspec: M711 too short for header")
	}
	h := L711Hdr.View(regs)
	base := CtlOffset711(i)
	if base+L711Ctl.Len() > len(regs) {
		return 0, 0, fmt.Errorf("sunspec: M711 too short for ctl %d", i)
	}
	co := func(p string) int { return base + L711Ctl.Offset(p) }
	h.SetScaledU32At(co("DbOf"), c.DbOf, "Db_SF")
	h.SetScaledU32At(co("DbUf"), c.DbUf, "Db_SF")
	h.SetScaledUintAt(co("KOf"), c.KOf, "K_SF")
	h.SetScaledUintAt(co("KUf"), c.KUf, "K_SF")
	h.SetScaledU32At(co("RspTms"), c.RspTms, "RspTms_SF")
	h.SetU16At(co("PMin"), uint16(int16(c.PMin)))
	h.SetU16At(co("ReadOnly"), 0)
	return base, base + L711Ctl.Len(), nil
}

// ── Model 712: DER Watt-Var ──────────────────────────────────────────────────

type WVPoint struct{ W, Var float64 }

type WattVarCurve struct {
	ReadOnly bool
	DeptRef  uint16
	Pri      uint16
	Points   []WVPoint
}

func Parse712Curve(regs []uint16, i int) (WattVarCurve, error) {
	npt, err := curveNPt(L712Hdr, regs, 64)
	if err != nil {
		return WattVarCurve{}, err
	}
	h := L712Hdr.View(regs)
	base := CurveOffset712(i, npt)
	if base+L712Crv.Len()+2*npt > len(regs) {
		return WattVarCurve{}, fmt.Errorf("sunspec: M712 too short for curve %d", i)
	}
	co := func(p string) int { return base + L712Crv.Offset(p) }
	actPt := int(h.U16At(co("ActPt")))
	if actPt > npt {
		actPt = npt
	}
	c := WattVarCurve{
		ReadOnly: h.U16At(co("ReadOnly")) == 1,
		DeptRef:  h.U16At(co("DeptRef")),
		Pri:      h.U16At(co("Pri")),
		Points:   make([]WVPoint, actPt),
	}
	for j := 0; j < actPt; j++ {
		po := PointOffset712(i, j, npt)
		c.Points[j] = WVPoint{W: h.ScaleSignedAt(po, "W_SF"), Var: h.ScaleSignedAt(po+1, "DeptRef_SF")}
	}
	return c, nil
}

func Encode712Curve(regs []uint16, i int, c WattVarCurve) (start, end int, err error) {
	npt, err := curveNPt(L712Hdr, regs, 64)
	if err != nil {
		return 0, 0, err
	}
	if len(c.Points) > npt {
		return 0, 0, fmt.Errorf("sunspec: WattVar curve has %d points, device NPt=%d", len(c.Points), npt)
	}
	h := L712Hdr.View(regs)
	base := CurveOffset712(i, npt)
	if base+L712Crv.Len()+2*npt > len(regs) {
		return 0, 0, fmt.Errorf("sunspec: M712 too short for curve %d", i)
	}
	co := func(p string) int { return base + L712Crv.Offset(p) }
	h.SetU16At(co("ActPt"), uint16(len(c.Points)))
	h.SetU16At(co("DeptRef"), c.DeptRef)
	h.SetU16At(co("Pri"), c.Pri)
	h.SetU16At(co("ReadOnly"), 0)
	for j, p := range c.Points {
		po := PointOffset712(i, j, npt)
		h.SetScaledSignedAt(po, p.W, "W_SF")
		h.SetScaledSignedAt(po+1, p.Var, "DeptRef_SF")
	}
	return base, base + L712Crv.Len() + 2*npt, nil
}

// ── Model 713: DER Storage Capacity ──────────────────────────────────────────

// StorageCapacity is the model 713 operational state-of-charge snapshot
// (spec Table 16).
type StorageCapacity struct {
	WHRtg   float64 // nameplate energy (Wh)
	WHAvail float64 // available energy (Wh) = WHRtg × SoC × SoH
	SoC     float64 // state of charge (%)
	SoH     float64 // state of health (%)
	Sta     uint16  // 0=OK, 1=warning, 2=error
}

func Parse713(regs []uint16) StorageCapacity {
	v := L713.View(regs)
	sta, _ := v.Enum("Sta")
	return StorageCapacity{
		WHRtg: v.Float("WHRtg"), WHAvail: v.Float("WHAvail"),
		SoC: v.Float("SoC"), SoH: v.Float("SoH"), Sta: sta,
	}
}

// ── Model 714: DER DC Measurement ────────────────────────────────────────────

type DCPort struct {
	PrtTyp uint16 // 0=PV,1=ESS,2=EV,3=INJ,4=ABS,5=BIDIR,6=DC_DC
	ID     uint16
	IDStr  string
	DCA, DCV, DCW    float64
	DCWhInj, DCWhAbs float64
	Tmp    float64
	DCSta  uint16 // 0=off,1=on,2=warning,3=error
	DCAlrm uint32
}

// DCMeasurement is the full model 714 decode (totals + per-port).
type DCMeasurement struct {
	PrtAlrms uint32
	NPrt     uint16
	DCA, DCW         float64
	DCWhInj, DCWhAbs float64
	Ports    []DCPort
}

func Parse714(regs []uint16) (DCMeasurement, error) {
	if len(regs) < L714Hdr.Len() {
		return DCMeasurement{}, fmt.Errorf("sunspec: M714 too short for header")
	}
	h := L714Hdr.View(regs)
	nprt, _ := h.Enum("NPrt")
	m := DCMeasurement{
		PrtAlrms: h.Bitfield32("PrtAlrms"), NPrt: nprt,
		DCA: h.Float("DCA"), DCW: h.Float("DCW"),
		DCWhInj: h.Float("DCWhInj"), DCWhAbs: h.Float("DCWhAbs"),
	}
	for i := 0; i < int(nprt); i++ {
		base := PortOffset714(i)
		if base+L714Prt.Len() > len(regs) {
			break
		}
		po := func(p string) int { return base + L714Prt.Offset(p) }
		m.Ports = append(m.Ports, DCPort{
			PrtTyp: h.U16At(po("PrtTyp")), ID: h.U16At(po("ID")),
			IDStr:  readString(regs, po("IDStr"), 8),
			DCA:    h.ScaleSignedAt(po("DCA"), "DCA_SF"),
			DCV:    h.ScaleUintAt(po("DCV"), "DCV_SF"),
			DCW:    h.ScaleSignedAt(po("DCW"), "DCW_SF"),
			DCWhInj: scaleU64At(h, po("DCWhInj"), "DCWH_SF"),
			DCWhAbs: scaleU64At(h, po("DCWhAbs"), "DCWH_SF"),
			Tmp:    h.ScaleSignedAt(po("Tmp"), "Tmp_SF"),
			DCSta:  h.U16At(po("DCSta")),
			DCAlrm: h.U32At(po("DCAlrm")),
		})
	}
	return m, nil
}

// scaleU64At reads a uint64 at an absolute offset and applies the named SF.
func scaleU64At(v View, o int, sf string) float64 {
	s, ok := v.SF(sf)
	if !ok {
		return math.NaN()
	}
	return float64(v.U64At(o)) * math.Pow10(int(s))
}
