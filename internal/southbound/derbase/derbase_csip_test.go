package derbase

import (
	"math"
	"testing"

	"lexa-hub/internal/northbound/model"
	"lexa-hub/internal/southbound/sunspec"
)

// memDev is an in-memory SunSpec holding-register device implementing
// modbus.Transport, used to validate the CSIP → SunSpec write mapping without
// hardware. It hosts models 702 (for WMax), 703, and 704.
type memDev struct{ regs map[uint16]uint16 }

func (d *memDev) Open() error            { return nil }
func (d *memDev) Close() error           { return nil }
func (d *memDev) SetUnitID(uint8) error  { return nil }
func (d *memDev) ReadInput(a, q uint16) ([]uint16, error) { return d.ReadHolding(a, q) }
func (d *memDev) ReadHolding(addr, qty uint16) ([]uint16, error) {
	out := make([]uint16, qty)
	for i := range out {
		out[i] = d.regs[addr+uint16(i)]
	}
	return out, nil
}
func (d *memDev) WriteHolding(addr uint16, vals []uint16) error {
	for i, v := range vals {
		d.regs[addr+uint16(i)] = v
	}
	return nil
}

func putModel(d *memDev, addr, id uint16, l int, init func([]uint16)) uint16 {
	d.regs[addr] = id
	d.regs[addr+1] = uint16(l)
	data := make([]uint16, l)
	if init != nil {
		init(data)
	}
	for i, v := range data {
		d.regs[addr+2+uint16(i)] = v
	}
	return addr + 2 + uint16(l)
}

func setSF(regs []uint16, l *sunspec.Layout, name string, sf int16) {
	regs[l.Offset(name)] = uint16(sf)
}

// newCSIPDevice builds a SunSpec device with 702/703/704 and a 10 kW nameplate.
func newCSIPDevice(t *testing.T) Base {
	t.Helper()
	d := &memDev{regs: map[uint16]uint16{}}
	addr := sunspec.SunSpecBase
	d.regs[addr] = 0x5375 // "Su"
	d.regs[addr+1] = 0x6E53
	addr += 2
	addr = putModel(d, addr, 701, sunspec.L701.Len(), nil) // present so Init selects M701
	addr = putModel(d, addr, 702, sunspec.L702.Len(), func(r []uint16) {
		setSF(r, sunspec.L702, "W_SF", 0)
		sunspec.L702.View(r).SetFloat("WMaxRtg", 10000)
	})
	addr = putModel(d, addr, 703, sunspec.L703.Len(), func(r []uint16) {
		setSF(r, sunspec.L703, "V_SF", 0)
		setSF(r, sunspec.L703, "Hz_SF", -2)
	})
	addr = putModel(d, addr, 704, sunspec.L704.Len(), func(r []uint16) {
		setSF(r, sunspec.L704, "PF_SF", -3)
		setSF(r, sunspec.L704, "WMaxLimPct_SF", -2)
		setSF(r, sunspec.L704, "WSet_SF", 0)
		setSF(r, sunspec.L704, "WSetPct_SF", -2)
		setSF(r, sunspec.L704, "VarSet_SF", 0)
		setSF(r, sunspec.L704, "VarSetPct_SF", -2)
	})
	n705 := sunspec.L705Hdr.Len() + 2*(sunspec.L705Crv.Len()+2*4)
	addr = putModel(d, addr, 705, n705, func(r []uint16) {
		h := sunspec.L705Hdr.View(r)
		h.SetEnum("NPt", 4)
		h.SetEnum("NCrv", 2)
		h.SetEnum("AdptCrvRslt", sunspec.AdptCompleted) // device reports adoption done
		setSF(r, sunspec.L705Hdr, "V_SF", -2)
		setSF(r, sunspec.L705Hdr, "DeptRef_SF", 0)
		setSF(r, sunspec.L705Hdr, "RspTms_SF", 0)
	})
	d.regs[addr] = 0xFFFF // end marker

	r, err := sunspec.NewReader(d)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	b, err := Init(r, "test")
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if math.Abs(b.Wmax-10000) > 1 {
		t.Fatalf("Wmax = %v, want 10000 (702 WMaxRtg)", b.Wmax)
	}
	return b
}

func read704(t *testing.T, b *Base) sunspec.ACControls {
	t.Helper()
	regs, err := b.Reader.ReadModel(sunspec.ModelDERCtlAC)
	if err != nil {
		t.Fatal(err)
	}
	return sunspec.Parse704(regs)
}

func ap(value int16, mult int8) *model.ActivePower { return &model.ActivePower{Value: value, Multiplier: mult} }
func spc(v int16) *model.SignedPerCent             { x := model.SignedPerCent{Value: v}; return &x }

// TestCSIP_ExportLimit_To_WMaxLimPct: opModExpLimW (a ceiling) → 704 WMaxLimPct %.
func TestCSIP_ExportLimit_To_WMaxLimPct(t *testing.T) {
	b := newCSIPDevice(t)
	if err := b.ApplyControl(model.DERControlBase{OpModExpLimW: ap(5000, 0)}, "test"); err != nil {
		t.Fatal(err)
	}
	c := read704(t, &b)
	if !c.WMaxLimPctEna {
		t.Error("WMaxLimPctEna not set")
	}
	if math.Abs(c.WMaxLimPct-50) > 0.5 {
		t.Errorf("WMaxLimPct = %v, want 50 (5kW of 10kW)", c.WMaxLimPct)
	}
}

// TestCSIP_FixedW_To_WSet: opModFixedW (a setpoint) → 704 WSet in watts.
func TestCSIP_FixedW_To_WSet(t *testing.T) {
	b := newCSIPDevice(t)
	if err := b.ApplyControl(model.DERControlBase{OpModFixedW: ap(3000, 0)}, "test"); err != nil {
		t.Fatal(err)
	}
	c := read704(t, &b)
	if !c.WSetEna || c.WSetMod != sunspec.M704_WSetMod_Watts {
		t.Errorf("WSet enable/mode: ena=%v mod=%d", c.WSetEna, c.WSetMod)
	}
	if math.Abs(c.WSet-3000) > 1 {
		t.Errorf("WSet = %v, want 3000", c.WSet)
	}
}

// TestCSIP_FixedPF_Inject_And_Absorb maps to the two distinct PF sync groups.
func TestCSIP_FixedPF_Inject_And_Absorb(t *testing.T) {
	b := newCSIPDevice(t)
	// Inject, over-excited (positive sign).
	if err := b.ApplyControl(model.DERControlBase{OpModFixedPFInjectW: spc(9500)}, "test"); err != nil {
		t.Fatal(err)
	}
	c := read704(t, &b)
	if !c.PFWInjEna || c.PFWInjExt != sunspec.M704_Ext_OverExcited {
		t.Errorf("PFWInj: ena=%v ext=%d", c.PFWInjEna, c.PFWInjExt)
	}
	if math.Abs(c.PFWInjPF-0.95) > 1e-3 {
		t.Errorf("PFWInjPF = %v, want 0.95", c.PFWInjPF)
	}

	// Absorb, under-excited (negative sign) — distinct PFWAbs group.
	if err := b.ApplyControl(model.DERControlBase{OpModFixedPFAbsorbW: spc(-9000)}, "test"); err != nil {
		t.Fatal(err)
	}
	c = read704(t, &b)
	if !c.PFWAbsEna || c.PFWAbsExt != sunspec.M704_Ext_UnderExcited {
		t.Errorf("PFWAbs: ena=%v ext=%d", c.PFWAbsEna, c.PFWAbsExt)
	}
	if math.Abs(c.PFWAbsPF-0.90) > 1e-3 {
		t.Errorf("PFWAbsPF = %v, want 0.90", c.PFWAbsPF)
	}
}

// TestCSIP_ImportLimit_To_NegativeWSet: opModImpLimW → charge (negative WSet).
func TestCSIP_ImportLimit_To_NegativeWSet(t *testing.T) {
	b := newCSIPDevice(t)
	if err := b.ApplyControl(model.DERControlBase{OpModImpLimW: ap(2000, 0)}, "test"); err != nil {
		t.Fatal(err)
	}
	c := read704(t, &b)
	if math.Abs(c.WSet-(-2000)) > 1 {
		t.Errorf("WSet = %v, want -2000 (charge)", c.WSet)
	}
}

// TestCSIP_Energize_To_EnterService: opModEnergize=false ceases to energize (703 ES=0).
func TestCSIP_Energize_To_EnterService(t *testing.T) {
	b := newCSIPDevice(t)
	on := true
	if err := b.ApplyControl(model.DERControlBase{OpModEnergize: &on}, "test"); err != nil {
		t.Fatal(err)
	}
	regs, _ := b.Reader.ReadModel(sunspec.ModelDEREnterService)
	if !sunspec.Parse703(regs).Enabled {
		t.Error("expected ES enabled")
	}
	off := false
	if err := b.ApplyControl(model.DERControlBase{OpModEnergize: &off}, "test"); err != nil {
		t.Fatal(err)
	}
	regs, _ = b.Reader.ReadModel(sunspec.ModelDEREnterService)
	if sunspec.Parse703(regs).Enabled {
		t.Error("expected ES disabled (cease to energize)")
	}
}

// TestCSIP_VoltVarAdoptWorkflow verifies the §3.1.2/§3.3 curve-update handshake:
// the staging curve is written, adoption is requested with a 1-based index >1
// (=2), and the function is enabled (Ena=1).
func TestCSIP_VoltVarAdoptWorkflow(t *testing.T) {
	b := newCSIPDevice(t)
	if !b.Has705 {
		t.Fatal("device missing M705")
	}
	c := sunspec.VoltVarCurve{
		DeptRef: 1, Pri: 1, VRef: 240,
		Points: []sunspec.VVPoint{{V: 92, Var: 30}, {V: 98, Var: 0}, {V: 102, Var: 0}, {V: 108, Var: -30}},
	}
	if err := b.WriteVoltVar(c, "test"); err != nil {
		t.Fatalf("WriteVoltVar: %v", err)
	}
	regs, _ := b.Reader.ReadModel(sunspec.ModelDERVoltVar)
	if got := regs[sunspec.L705Hdr.Offset("AdptCrvReq")]; got != 2 {
		t.Errorf("AdptCrvReq = %d, want 2 (1-based staging index, >1 per spec)", got)
	}
	if got := regs[sunspec.L705Hdr.Offset("Ena")]; got != 1 {
		t.Errorf("Ena = %d, want 1 (function must be enabled after adopt)", got)
	}
	got, _ := sunspec.Parse705Curve(regs, 1) // staging curve holds the new points
	if len(got.Points) != 4 {
		t.Fatalf("staging curve points = %d, want 4", len(got.Points))
	}
	approxEq(t, "staging V[0]", got.Points[0].V, 92)
	// Active read-only curve (index 0) must remain empty until the device promotes it.
	if c0, _ := sunspec.Parse705Curve(regs, 0); len(c0.Points) != 0 {
		t.Errorf("active curve 0 mutated by staging write: %+v", c0)
	}
}

func approxEq(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}
