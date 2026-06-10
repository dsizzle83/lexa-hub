package sunspec

import (
	"math"
	"testing"
)

func approx(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.IsNaN(want) {
		if !math.IsNaN(got) {
			t.Errorf("%s = %v, want NaN", name, got)
		}
		return
	}
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

// setSF writes a scale-factor register directly (sunssf is int16).
func setSF(regs []uint16, l *Layout, name string, sf int16) { regs[l.Offset(name)] = uint16(sf) }

// ── 701: measurement decode + sentinel handling ──────────────────────────────

func TestRoundTrip701(t *testing.T) {
	regs := make([]uint16, L701.Len())
	setSF(regs, L701, "W_SF", 0)
	setSF(regs, L701, "VA_SF", 0)
	setSF(regs, L701, "Var_SF", 0)
	setSF(regs, L701, "PF_SF", -3)
	setSF(regs, L701, "A_SF", -1)
	setSF(regs, L701, "V_SF", -1)
	setSF(regs, L701, "Hz_SF", -2)
	setSF(regs, L701, "Tmp_SF", -1)
	v := L701.View(regs)
	v.SetEnum("ACType", 2)
	v.SetEnum("St", 1)
	v.SetEnum("InvSt", 3)
	v.SetEnum("ConnSt", 1)
	v.SetFloat("W", 4200)
	v.SetFloat("VA", 4300)
	v.SetFloat("Var", -150)
	v.SetFloat("PF", 0.985) // power factor (engineering value)
	v.SetFloat("A", 18.3)
	v.SetFloat("LNV", 240.1)
	v.SetFloat("Hz", 60.01)
	v.SetFloat("TmpCab", 41.5)

	m := Parse701(regs)
	if m.ACType != 2 || m.St != 1 || m.InvSt != 3 || m.ConnSt != 1 {
		t.Errorf("enums: %+v", m)
	}
	approx(t, "W", m.W, 4200)
	approx(t, "VA", m.VA, 4300)
	approx(t, "Var", m.Var, -150)
	approx(t, "PF", m.PF, 0.985)
	approx(t, "A", m.A, 18.3)
	approx(t, "LNV", m.LNV, 240.1)
	approx(t, "Hz", m.Hz, 60.01)
	approx(t, "TmpCab", m.TmpCab, 41.5)

	// Sentinel: VA register set to the int16 not-implemented value → NaN.
	regs[L701.Offset("VA")] = sentI16
	approx(t, "VA(sentinel)", Parse701(regs).VA, math.NaN())
}

// ── 703: enter-service encode → parse round trip (uint32 fields) ─────────────

func TestRoundTrip703(t *testing.T) {
	regs := make([]uint16, L703.Len())
	setSF(regs, L703, "V_SF", 0)
	setSF(regs, L703, "Hz_SF", -2)
	in := EnterService{Enabled: true, VHi: 105, VLo: 88, HzHi: 60.5, HzLo: 59.5, DelayS: 300, RandomS: 60, RampS: 120}
	if err := Encode703(regs, in); err != nil {
		t.Fatal(err)
	}
	got := Parse703(regs)
	if !got.Enabled {
		t.Error("ES not enabled")
	}
	approx(t, "VHi", got.VHi, 105)
	approx(t, "HzHi", got.HzHi, 60.5)
	if got.DelayS != 300 || got.RandomS != 60 || got.RampS != 120 {
		t.Errorf("timers: %+v", got)
	}
}

// ── 704: control setters via View → parse, incl. WSet (int32) and PF group ───

func TestRoundTrip704(t *testing.T) {
	regs := make([]uint16, L704.Len())
	for _, sf := range []struct {
		n string
		v int16
	}{{"PF_SF", -3}, {"WMaxLimPct_SF", -2}, {"WSet_SF", 0}, {"WSetPct_SF", -2}, {"VarSet_SF", 0}, {"VarSetPct_SF", -2}} {
		setSF(regs, L704, sf.n, sf.v)
	}
	v := L704.View(regs)
	v.SetBool("PFWInjEna", true)
	v.SetFloat("PFWInj_PF", 0.95) // power factor (engineering value)
	v.SetEnum("PFWInj_Ext", M704_Ext_OverExcited)
	v.SetBool("WSetEna", true)
	v.SetEnum("WSetMod", M704_WSetMod_Watts)
	v.SetFloat("WSet", -3500) // charge 3.5 kW (negative int32)
	v.SetBool("WMaxLimPctEna", true)
	v.SetFloat("WMaxLimPct", 80.0)

	c := Parse704(regs)
	if !c.PFWInjEna || c.PFWInjExt != M704_Ext_OverExcited {
		t.Errorf("PFWInj: %+v", c)
	}
	approx(t, "PFWInjPF", c.PFWInjPF, 0.95)
	approx(t, "WSet", c.WSet, -3500)
	approx(t, "WMaxLimPct", c.WMaxLimPct, 80.0)
	if !c.WSetEna || c.WSetMod != M704_WSetMod_Watts {
		t.Errorf("WSet enable/mode: %+v", c)
	}
}

// ── 705: Volt-Var staging-curve encode → parse round trip ────────────────────

func TestRoundTrip705(t *testing.T) {
	npt, ncrv := 4, 2
	regs := make([]uint16, L705Hdr.Len()+ncrv*(L705Crv.Len()+2*npt))
	h := L705Hdr.View(regs)
	h.SetEnum("NPt", uint16(npt))
	h.SetEnum("NCrv", uint16(ncrv))
	setSF(regs, L705Hdr, "V_SF", -2)
	setSF(regs, L705Hdr, "DeptRef_SF", 0)
	setSF(regs, L705Hdr, "RspTms_SF", 0)

	in := VoltVarCurve{
		DeptRef: 1, Pri: 1, VRef: 240, VRefAutoEna: true, VRefAutoTms: 10, RspTms: 5,
		Points: []VVPoint{{V: 92, Var: 30}, {V: 98, Var: 0}, {V: 102, Var: 0}, {V: 108, Var: -30}},
	}
	start, end, err := Encode705Curve(regs, 1, in)
	if err != nil {
		t.Fatal(err)
	}
	if start <= L705Hdr.Len()-1 || end > len(regs) {
		t.Fatalf("encoded range [%d,%d) outside curve 1", start, end)
	}
	got, err := Parse705Curve(regs, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.DeptRef != 1 || got.Pri != 1 || !got.VRefAutoEna {
		t.Errorf("header: %+v", got)
	}
	approx(t, "VRef", got.VRef, 240)
	approx(t, "RspTms", got.RspTms, 5)
	if len(got.Points) != len(in.Points) {
		t.Fatalf("points len %d, want %d", len(got.Points), len(in.Points))
	}
	for i := range in.Points {
		approx(t, "V", got.Points[i].V, in.Points[i].V)
		approx(t, "Var", got.Points[i].Var, in.Points[i].Var)
	}
	// The active (read-only) curve at index 0 must be untouched by staging write.
	if c0, _ := Parse705Curve(regs, 0); len(c0.Points) != 0 {
		t.Errorf("active curve 0 mutated: %+v", c0)
	}
}

// ── 707/708: three-curve voltage trip set round trip (MustTrip/MayTrip/MomCess)

func TestRoundTrip707(t *testing.T) {
	npt := 5
	regs := make([]uint16, L707Hdr.Len()+2*tripVSetSize(npt))
	h := L707Hdr.View(regs)
	h.SetEnum("NPt", uint16(npt))
	h.SetEnum("NCrvSet", 2)
	setSF(regs, L707Hdr, "V_SF", -2)
	setSF(regs, L707Hdr, "Tms_SF", -2)

	in := VoltageTripSet{
		MustTrip: []TripVPoint{{V: 88, Tms: 0.16}, {V: 50, Tms: 0.16}, {V: 50, Tms: 2.0}},
		MayTrip:  []TripVPoint{{V: 88, Tms: 0.5}, {V: 70, Tms: 1.0}},
		MomCess:  []TripVPoint{{V: 50, Tms: 0}, {V: 88, Tms: 0}},
	}
	if _, _, err := Encode707Set(regs, 1, in); err != nil {
		t.Fatal(err)
	}
	got, err := Parse707Set(regs, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.MustTrip) != 3 || len(got.MayTrip) != 2 || len(got.MomCess) != 2 {
		t.Fatalf("curve lengths: must=%d may=%d mom=%d", len(got.MustTrip), len(got.MayTrip), len(got.MomCess))
	}
	approx(t, "MustTrip[0].V", got.MustTrip[0].V, 88)
	approx(t, "MustTrip[0].Tms", got.MustTrip[0].Tms, 0.16)
	approx(t, "MayTrip[1].Tms", got.MayTrip[1].Tms, 1.0)
	approx(t, "MomCess[1].V", got.MomCess[1].V, 88)
}

// ── 709/710: frequency trip (uint32 Hz points) round trip ────────────────────

func TestRoundTrip709(t *testing.T) {
	npt := 4
	regs := make([]uint16, L709Hdr.Len()+2*tripHzSetSize(npt))
	h := L709Hdr.View(regs)
	h.SetEnum("NPt", uint16(npt))
	h.SetEnum("NCrvSet", 2)
	setSF(regs, L709Hdr, "Hz_SF", -3)
	setSF(regs, L709Hdr, "Tms_SF", -2)
	in := FreqTripSet{
		MustTrip: []TripHzPoint{{Hz: 61.2, Tms: 0.16}, {Hz: 62.0, Tms: 0.16}},
		MayTrip:  []TripHzPoint{{Hz: 60.5, Tms: 299.0}},
	}
	if _, _, err := Encode709Set(regs, 1, in); err != nil {
		t.Fatal(err)
	}
	got, err := Parse709Set(regs, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.MustTrip) != 2 || len(got.MayTrip) != 1 {
		t.Fatalf("lengths must=%d may=%d", len(got.MustTrip), len(got.MayTrip))
	}
	approx(t, "MustTrip[0].Hz", got.MustTrip[0].Hz, 61.2)
	approx(t, "MayTrip[0].Tms", got.MayTrip[0].Tms, 299.0)
}

// ── 711: frequency droop round trip (uint32 deadbands) ───────────────────────

func TestRoundTrip711(t *testing.T) {
	regs := make([]uint16, L711Hdr.Len()+2*L711Ctl.Len())
	setSF(regs, L711Hdr, "Db_SF", -3)
	setSF(regs, L711Hdr, "K_SF", -2)
	setSF(regs, L711Hdr, "RspTms_SF", -2)
	in := FreqDroopCtl{DbOf: 0.036, DbUf: 0.036, KOf: 0.05, KUf: 0.05, RspTms: 5.0, PMin: -100}
	if _, _, err := Encode711Ctl(regs, 1, in); err != nil {
		t.Fatal(err)
	}
	got, err := Parse711Ctl(regs, 1)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "DbOf", got.DbOf, 0.036)
	approx(t, "KUf", got.KUf, 0.05)
	approx(t, "RspTms", got.RspTms, 5.0)
	approx(t, "PMin", got.PMin, -100)
}

// ── 712: Watt-Var round trip (signed W and Var points) ───────────────────────

func TestRoundTrip712(t *testing.T) {
	npt, ncrv := 6, 2
	regs := make([]uint16, L712Hdr.Len()+ncrv*(L712Crv.Len()+2*npt))
	h := L712Hdr.View(regs)
	h.SetEnum("NPt", uint16(npt))
	h.SetEnum("NCrv", uint16(ncrv))
	setSF(regs, L712Hdr, "W_SF", 0)
	setSF(regs, L712Hdr, "DeptRef_SF", 0)
	in := WattVarCurve{DeptRef: 1, Pri: 1, Points: []WVPoint{
		{W: -100, Var: 0}, {W: -50, Var: 0}, {W: 0, Var: 0}, {W: 50, Var: -10}, {W: 100, Var: -30}, {W: 100, Var: -44},
	}}
	if _, _, err := Encode712Curve(regs, 1, in); err != nil {
		t.Fatal(err)
	}
	got, err := Parse712Curve(regs, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Points) != 6 || got.DeptRef != 1 {
		t.Fatalf("got %+v", got)
	}
	approx(t, "W[0]", got.Points[0].W, -100)
	approx(t, "Var[4]", got.Points[4].Var, -30)
}

// ── 713: storage capacity decode (spec Table 16 shape) ───────────────────────

func TestRoundTrip713(t *testing.T) {
	regs := make([]uint16, L713.Len())
	setSF(regs, L713, "WH_SF", 0)
	setSF(regs, L713, "Pct_SF", -2)
	v := L713.View(regs)
	v.SetFloat("WHRtg", 13500)
	v.SetFloat("WHAvail", 9450)
	v.SetFloat("SoC", 72.5)
	v.SetFloat("SoH", 98.0)
	v.SetEnum("Sta", 0)
	s := Parse713(regs)
	approx(t, "WHRtg", s.WHRtg, 13500)
	approx(t, "WHAvail", s.WHAvail, 9450)
	approx(t, "SoC", s.SoC, 72.5)
	approx(t, "SoH", s.SoH, 98.0)
	if s.Sta != 0 {
		t.Errorf("Sta = %d", s.Sta)
	}
}

// ── 714: DC measurement with two ports ───────────────────────────────────────

func TestRoundTrip714(t *testing.T) {
	nprt := 2
	regs := make([]uint16, L714Hdr.Len()+nprt*L714Prt.Len())
	h := L714Hdr.View(regs)
	h.SetEnum("NPrt", uint16(nprt))
	setSF(regs, L714Hdr, "DCA_SF", -1)
	setSF(regs, L714Hdr, "DCV_SF", -1)
	setSF(regs, L714Hdr, "DCW_SF", 0)
	setSF(regs, L714Hdr, "DCWH_SF", 0)
	setSF(regs, L714Hdr, "Tmp_SF", -1)
	h.SetFloat("DCA", 12.4)
	h.SetFloat("DCW", 4200)
	// Port 0
	p0 := PortOffset714(0)
	h.SetU16At(p0+L714Prt.Offset("PrtTyp"), 0) // PV
	h.SetU16At(p0+L714Prt.Offset("ID"), 1)
	h.SetScaledSignedAt(p0+L714Prt.Offset("DCA"), 6.2, "DCA_SF")
	h.SetScaledUintAt(p0+L714Prt.Offset("DCV"), 400.0, "DCV_SF")
	h.SetScaledSignedAt(p0+L714Prt.Offset("DCW"), 2100, "DCW_SF")
	h.SetU16At(p0+L714Prt.Offset("DCSta"), 1)
	// Port 1
	p1 := PortOffset714(1)
	h.SetU16At(p1+L714Prt.Offset("PrtTyp"), 1) // ESS
	h.SetScaledSignedAt(p1+L714Prt.Offset("DCW"), 2100, "DCW_SF")

	m, err := Parse714(regs)
	if err != nil {
		t.Fatal(err)
	}
	if m.NPrt != 2 || len(m.Ports) != 2 {
		t.Fatalf("ports: NPrt=%d len=%d", m.NPrt, len(m.Ports))
	}
	approx(t, "total DCA", m.DCA, 12.4)
	if m.Ports[0].PrtTyp != 0 || m.Ports[0].ID != 1 {
		t.Errorf("port0: %+v", m.Ports[0])
	}
	approx(t, "port0 DCV", m.Ports[0].DCV, 400.0)
	if m.Ports[1].PrtTyp != 1 {
		t.Errorf("port1 type: %+v", m.Ports[1])
	}
}
