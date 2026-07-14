package discovery

// WP-8 walker tests: the DefaultDERControl setGradW/setSoftGradW carriage
// (parsed by the local XML wrapper — no proto pin bump) and the
// ignored-content alarm that replaced the extended→simple silent drop.

import (
	"context"
	"testing"

	model "lexa-proto/csipmodel"
)

// defaultWithGrad mirrors the walker's extendedDefaultDERControlDoc from the
// SERVING side: the mock fetcher xml.Marshal's it, producing a
// <DefaultDERControl> document with setGradW/setSoftGradW children exactly
// as a 2030.5 server would emit them.
type defaultWithGrad struct {
	model.ExtendedDefaultDERControl
	SetGradW     *uint16 `xml:"setGradW"`
	SetSoftGradW *uint16 `xml:"setSoftGradW"`
}

func uint16ptr(v uint16) *uint16 { return &v }

// TestDiscover_ParsesDefaultSetGradW: a DefaultDERControl carrying
// setGradW/setSoftGradW lands on ProgramState.DefaultSetGradW/-SoftGradW,
// while the extended base and the scheduler's scalar view are unaffected.
func TestDiscover_ParsesDefaultSetGradW(t *testing.T) {
	m := newMockFetcher()
	buildFullResourceTree(m)
	boolTrue := true
	m.serve("/derp/0/dderc", &defaultWithGrad{
		ExtendedDefaultDERControl: model.ExtendedDefaultDERControl{
			Resource: model.Resource{Href: "/derp/0/dderc"}, MRID: "DD01",
			DERControlBase: model.ExtendedDERControlBase{
				OpModExpLimW: &model.ActivePower{Multiplier: 0, Value: 5000},
				OpModConnect: &boolTrue,
			},
		},
		SetGradW:     uint16ptr(30),
		SetSoftGradW: uint16ptr(10),
	})

	tree, err := NewWalker(m, testLFDI).Discover(context.Background(), "/dcap")
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}
	if len(tree.Programs) != 1 {
		t.Fatalf("Programs = %d, want 1", len(tree.Programs))
	}
	ps := tree.Programs[0]
	if ps.DefaultSetGradW == nil || *ps.DefaultSetGradW != 30 {
		t.Errorf("DefaultSetGradW = %v, want 30", ps.DefaultSetGradW)
	}
	if ps.DefaultSetSoftGradW == nil || *ps.DefaultSetSoftGradW != 10 {
		t.Errorf("DefaultSetSoftGradW = %v, want 10", ps.DefaultSetSoftGradW)
	}
	if ps.ExtendedDefault == nil || ps.ExtendedDefault.MRID != "DD01" {
		t.Fatalf("ExtendedDefault = %+v, want DD01", ps.ExtendedDefault)
	}
	if ps.DefaultControl == nil || ps.DefaultControl.DERControlBase.OpModExpLimW == nil ||
		ps.DefaultControl.DERControlBase.OpModExpLimW.Value != 5000 {
		t.Errorf("scalar DefaultControl view regressed: %+v", ps.DefaultControl)
	}
}

// TestDiscover_NoGradFieldsStaysNil: a DefaultDERControl without the ramp
// fields (the pre-WP-8 fixture) leaves both carriers nil — absent, never
// fabricated.
func TestDiscover_NoGradFieldsStaysNil(t *testing.T) {
	m := newMockFetcher()
	buildFullResourceTree(m)
	tree, err := NewWalker(m, testLFDI).Discover(context.Background(), "/dcap")
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}
	ps := tree.Programs[0]
	if ps.DefaultSetGradW != nil || ps.DefaultSetSoftGradW != nil {
		t.Errorf("grad fields = %v/%v, want nil/nil", ps.DefaultSetGradW, ps.DefaultSetSoftGradW)
	}
}

// TestReportIgnoredContent covers the sweep directly: each unrepresentable
// content class counts (target_var, freq_droop, unresolvable curve hrefs),
// fully representable content does not.
func TestReportIgnoredContent(t *testing.T) {
	w := &Walker{}

	t.Run("unrepresentable modes count", func(t *testing.T) {
		ps := ProgramState{
			Program: model.DERProgram{MRID: "P1"},
			ExtendedControls: &model.ExtendedDERControlList{DERControl: []model.ExtendedDERControl{{
				MRID: "E1",
				DERControlBase: model.ExtendedDERControlBase{
					OpModTargetVar: &model.ReactivePower{Value: 100},
					OpModFreqDroop: &model.FreqDroop{DBuf: 36, DF: 100, DP: 5, OpenLoopTms: 100, TResponse: 100},
					OpModVoltVar:   &model.CurveLink{Href: "/dc/missing"}, // not in ps.Curves
				},
			}}},
		}
		before := IgnoredContentTotal()
		w.reportIgnoredContent(&ps)
		if got := IgnoredContentTotal() - before; got != 3 {
			t.Errorf("ignored-content delta = %d, want 3 (target_var + freq_droop + unresolvable volt_var)", got)
		}
	})

	t.Run("empty-href curve link counts", func(t *testing.T) {
		ps := ProgramState{
			Program: model.DERProgram{MRID: "P1"},
			ExtendedDefault: &model.ExtendedDefaultDERControl{
				MRID: "DD1",
				DERControlBase: model.ExtendedDERControlBase{
					OpModVoltWatt: &model.CurveLink{}, // present mode, empty href
				},
			},
		}
		before := IgnoredContentTotal()
		w.reportIgnoredContent(&ps)
		if got := IgnoredContentTotal() - before; got != 1 {
			t.Errorf("ignored-content delta = %d, want 1", got)
		}
	})

	t.Run("representable content does not count", func(t *testing.T) {
		energize := true
		ps := ProgramState{
			Program: model.DERProgram{MRID: "P1"},
			Curves: map[string]model.DERCurve{"/dc/vv": {
				MRID: "CRV-VV", CurveType: model.CurveTypeVoltVar,
				CurveData: []model.DERCurveData{{XValue: 9200, YValue: 3000}},
			}},
			ExtendedDefault: &model.ExtendedDefaultDERControl{
				MRID: "DD1",
				DERControlBase: model.ExtendedDERControlBase{
					OpModEnergize: &energize,
					OpModTargetW:  &model.ActivePower{Value: 2000},
					OpModVoltVar:  &model.CurveLink{Href: "/dc/vv"}, // resolves
				},
			},
		}
		before := IgnoredContentTotal()
		w.reportIgnoredContent(&ps)
		if got := IgnoredContentTotal() - before; got != 0 {
			t.Errorf("ignored-content delta = %d, want 0 (everything is representable)", got)
		}
	})
}

// TestDiscover_AlarmsUnresolvableCurveDuringWalk: the sweep runs inside the
// real Discover path — a served control whose curve href does not resolve
// (here: the program exposes no DERCurveList at all) raises the alarm during
// the walk, not just in the unit-level sweep.
func TestDiscover_AlarmsUnresolvableCurveDuringWalk(t *testing.T) {
	m := newMockFetcher()
	buildFullResourceTree(m)
	m.serve("/derp/0/derc", &model.ExtendedDERControlList{
		Resource: model.Resource{Href: "/derp/0/derc"}, All: 1, Results: 1,
		DERControl: []model.ExtendedDERControl{{
			Resource: model.Resource{Href: "/derp/0/derc/0"}, MRID: "C3D4",
			EventStatus: &model.EventStatus{CurrentStatus: 0},
			Interval:    model.DateTimeInterval{Duration: 7200, Start: 1700003600},
			DERControlBase: model.ExtendedDERControlBase{
				OpModExpLimW: &model.ActivePower{Value: 3000},
				OpModVoltVar: &model.CurveLink{Href: "/dc/nowhere"},
			},
		}},
	})

	before := IgnoredContentTotal()
	if _, err := NewWalker(m, testLFDI).Discover(context.Background(), "/dcap"); err != nil {
		t.Fatalf("Discover failed: %v", err)
	}
	if got := IgnoredContentTotal() - before; got != 1 {
		t.Errorf("ignored-content delta = %d, want 1 (unresolvable volt_var href on the served event)", got)
	}
}
