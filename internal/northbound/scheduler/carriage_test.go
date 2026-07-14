package scheduler

// WP-8 carriage + extended-plausibility tests (standards-buildout C1).
//
// These are ADDITIVE: every pre-WP-8 scheduler test (scheduler_test.go,
// failclosed_test.go, fuzz_test.go, utilitytime_equiv_test.go) runs
// unchanged — the acceptance gate that the fail-closed decision logic did
// not move. This file covers only what WP-8 added: the extended base /
// resolved-curve / ramp-default carriage through Evaluate, and the
// plausibility gates over the new numerics (PF, fixed var %, gen/load/
// target limits, curve content).

import (
	"testing"

	"lexa-hub/internal/northbound/discovery"
	model "lexa-proto/csipmodel"
)

func u16(v uint16) *uint16 { return &v }

// extProgram builds a primacy-1 ProgramState carrying BOTH the scalar
// evaluation view (Controls/DefaultControl — what the walker's
// extended→simple projection produces) and the full extended content
// (ExtendedControls/ExtendedDefault/Curves), the exact shape
// discovery.Walker.Discover emits post-WP-8.
func extProgram(curves map[string]model.DERCurve, def *model.ExtendedDefaultDERControl, defGradW, defSoftGradW *uint16, events ...model.ExtendedDERControl) []discovery.ProgramState {
	ps := discovery.ProgramState{
		Program:             model.DERProgram{Resource: model.Resource{Href: "/derp/0"}, MRID: "PROG", Primacy: 1},
		Curves:              curves,
		DefaultSetGradW:     defGradW,
		DefaultSetSoftGradW: defSoftGradW,
	}
	if def != nil {
		ps.ExtendedDefault = def
		ps.DefaultControl = &model.DefaultDERControl{
			Resource:       def.Resource,
			MRID:           def.MRID,
			DERControlBase: extBaseToSimple(def.DERControlBase),
		}
	}
	if len(events) > 0 {
		ext := &model.ExtendedDERControlList{DERControl: events}
		list := &model.DERControlList{}
		for _, e := range events {
			list.DERControl = append(list.DERControl, model.DERControl{
				Resource:       e.Resource,
				MRID:           e.MRID,
				CreationTime:   e.CreationTime,
				EventStatus:    e.EventStatus,
				Interval:       e.Interval,
				DERControlBase: extBaseToSimple(e.DERControlBase),
			})
		}
		ps.ExtendedControls = ext
		ps.Controls = list
	}
	return []discovery.ProgramState{ps}
}

// extBaseToSimple mirrors the walker's scalar projection for test fixtures.
func extBaseToSimple(e model.ExtendedDERControlBase) model.DERControlBase {
	return model.DERControlBase{
		OpModConnect:        e.OpModConnect,
		OpModEnergize:       e.OpModEnergize,
		OpModFixedPFAbsorbW: e.OpModFixedPFAbsorbW,
		OpModFixedPFInjectW: e.OpModFixedPFInjectW,
		OpModFixedVar:       e.OpModFixedVar,
		OpModFixedW:         e.OpModFixedW,
		OpModMaxLimW:        e.OpModMaxLimW,
		OpModExpLimW:        e.OpModExpLimW,
		OpModGenLimW:        e.OpModGenLimW,
		OpModImpLimW:        e.OpModImpLimW,
		OpModLoadLimW:       e.OpModLoadLimW,
		RampTms:             e.RampTms,
	}
}

func extEvent(mrid string, start int64, duration uint32, base model.ExtendedDERControlBase) model.ExtendedDERControl {
	return model.ExtendedDERControl{
		Resource:       model.Resource{Href: "/derp/0/derc/" + mrid},
		MRID:           mrid,
		CreationTime:   start,
		EventStatus:    &model.EventStatus{CurrentStatus: 0},
		Interval:       model.DateTimeInterval{Start: start, Duration: duration},
		DERControlBase: base,
	}
}

func voltVarCurve(points int) model.DERCurve {
	c := model.DERCurve{
		Resource:  model.Resource{Href: "/dc/vv"},
		MRID:      "CRV-VV",
		CurveType: model.CurveTypeVoltVar,
		YRefType:  3,
	}
	for i := 0; i < points; i++ {
		c.CurveData = append(c.CurveData, model.DERCurveData{XValue: int32(9000 + i*100), YValue: int32(3000 - i*100)})
	}
	return c
}

// TestEvaluate_CarriesExtendedEventContent: an active extended event rides
// through Evaluate with its full extended base, the program's curve map, the
// program href, and resolvable curves — and the decision output (Source/
// MRID/Base/ValidUntil) is exactly what the scalar path always produced.
func TestEvaluate_CarriesExtendedEventContent(t *testing.T) {
	curves := map[string]model.DERCurve{"/dc/vv": voltVarCurve(2)}
	base := model.ExtendedDERControlBase{
		OpModExpLimW: &model.ActivePower{Value: 1000},
		OpModTargetW: &model.ActivePower{Value: 2000},
		OpModVoltVar: &model.CurveLink{Href: "/dc/vv"},
	}
	programs := extProgram(curves, nil, nil, nil, extEvent("E1", epoch, 3600, base))

	s := New()
	ac := s.Evaluate(programs, epoch+10)
	if ac == nil || ac.Source != "event" || ac.MRID != "E1" || ac.Held {
		t.Fatalf("expected fresh event E1, got %+v", ac)
	}
	if ac.Base.OpModExpLimW == nil || ac.Base.OpModExpLimW.Value != 1000 {
		t.Errorf("scalar Base lost its export limit: %+v", ac.Base.OpModExpLimW)
	}
	if ac.Extended == nil {
		t.Fatal("Extended base not carried")
	}
	if ac.Extended.OpModTargetW == nil || ac.Extended.OpModTargetW.Value != 2000 {
		t.Errorf("Extended.OpModTargetW = %+v, want 2000", ac.Extended.OpModTargetW)
	}
	if ac.ProgramHref != "/derp/0" {
		t.Errorf("ProgramHref = %q, want /derp/0", ac.ProgramHref)
	}
	if ac.SetGradW != nil || ac.SetSoftGradW != nil {
		t.Errorf("event-sourced control must not carry the default-only ramp fields: %v/%v", ac.SetGradW, ac.SetSoftGradW)
	}
	mcs := ac.ResolvedModeCurves()
	if len(mcs) != 1 || mcs[0].Mode != "volt_var" || mcs[0].Curve.MRID != "CRV-VV" {
		t.Fatalf("ResolvedModeCurves = %+v, want one volt_var CRV-VV", mcs)
	}
}

// TestEvaluate_CarriesDefaultRampAndExtended: a default-sourced control
// carries the ExtendedDefault base plus the DefaultDERControl-only
// setGradW/setSoftGradW ramp defaults.
func TestEvaluate_CarriesDefaultRampAndExtended(t *testing.T) {
	def := &model.ExtendedDefaultDERControl{
		Resource: model.Resource{Href: "/derp/0/dderc"},
		MRID:     "DD1",
		DERControlBase: model.ExtendedDERControlBase{
			OpModExpLimW: &model.ActivePower{Value: 5000},
		},
	}
	programs := extProgram(nil, def, u16(30), u16(10))

	s := New()
	ac := s.Evaluate(programs, epoch)
	if ac == nil || ac.Source != "default" || ac.MRID != "DD1" {
		t.Fatalf("expected default DD1, got %+v", ac)
	}
	if ac.Extended == nil || ac.Extended.OpModExpLimW == nil || ac.Extended.OpModExpLimW.Value != 5000 {
		t.Errorf("ExtendedDefault base not carried: %+v", ac.Extended)
	}
	if ac.SetGradW == nil || *ac.SetGradW != 30 || ac.SetSoftGradW == nil || *ac.SetSoftGradW != 10 {
		t.Errorf("default ramp fields = %v/%v, want 30/10", ac.SetGradW, ac.SetSoftGradW)
	}
}

// TestEvaluate_HeldLKGKeepsCarriage: the fail-closed hold re-serves the
// last-known-good WITH its carriage — a WAN blip must not strip the curve/
// extended content off an enforced control any more than its scalar cap.
func TestEvaluate_HeldLKGKeepsCarriage(t *testing.T) {
	curves := map[string]model.DERCurve{"/dc/vv": voltVarCurve(2)}
	base := model.ExtendedDERControlBase{
		OpModExpLimW: &model.ActivePower{Value: 1000},
		OpModVoltVar: &model.CurveLink{Href: "/dc/vv"},
	}
	programs := extProgram(curves, nil, nil, nil, extEvent("E1", epoch, 3600, base))

	s := New()
	if ac := s.Evaluate(programs, epoch); ac == nil || ac.MRID != "E1" {
		t.Fatalf("expected fresh E1, got %+v", ac)
	}
	held := s.Evaluate(nil, epoch+10) // empty programs — fail-closed hold
	if held == nil || !held.Held || held.MRID != "E1" {
		t.Fatalf("expected held E1, got %+v", held)
	}
	if held.Extended == nil || len(held.ResolvedModeCurves()) != 1 {
		t.Errorf("held LKG lost its carriage: extended=%v curves=%d", held.Extended, len(held.ResolvedModeCurves()))
	}
}

// TestEvaluate_RejectsImplausibleNewNumerics: each new gated numeric —
// gen/load/target limit, fixed PF, fixed var %, curve content — rejects the
// whole control through the EXISTING fail-closed path (LKG held,
// ImplausibleReject set) with the RejectHook firing 253's reason.
func TestEvaluate_RejectsImplausibleNewNumerics(t *testing.T) {
	huge := &model.ActivePower{Value: 32767, Multiplier: 9} // ~3.3e13 W
	cases := []struct {
		name       string
		base       model.ExtendedDERControlBase
		curves     map[string]model.DERCurve
		wantReason string
	}{
		{"gen limit", model.ExtendedDERControlBase{OpModGenLimW: huge}, nil, "implausible-limit"},
		{"load limit", model.ExtendedDERControlBase{OpModLoadLimW: huge}, nil, "implausible-limit"},
		{"target W", model.ExtendedDERControlBase{OpModTargetW: huge}, nil, "implausible-limit"},
		{"pf zero", model.ExtendedDERControlBase{
			OpModFixedPFInjectW: &model.SignedPerCent{Value: 0}}, nil, "implausible-pf"},
		{"pf over unity", model.ExtendedDERControlBase{
			OpModFixedPFAbsorbW: &model.SignedPerCent{Value: 10001}}, nil, "implausible-pf"},
		{"var beyond 100%", model.ExtendedDERControlBase{
			OpModFixedVar: &model.FixedVar{Value: model.SignedPerCent{Value: -10001}}}, nil, "implausible-var"},
		{"curve 11 points", model.ExtendedDERControlBase{
			OpModVoltVar: &model.CurveLink{Href: "/dc/vv"}},
			map[string]model.DERCurve{"/dc/vv": voltVarCurve(11)}, "implausible-curve"},
		{"curve zero points", model.ExtendedDERControlBase{
			OpModVoltVar: &model.CurveLink{Href: "/dc/vv"}},
			map[string]model.DERCurve{"/dc/vv": voltVarCurve(0)}, "implausible-curve"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New()
			var gotMRID, gotReason string
			s.RejectHook = func(mrid, reason string) { gotMRID, gotReason = mrid, reason }

			// Adopt a good control first so the reject provably HOLDS it.
			good := progs(eventWithExpLim("GOOD", epoch, 3600, 1000, 0))
			if ac := s.Evaluate(good, epoch); ac == nil || ac.MRID != "GOOD" {
				t.Fatalf("expected GOOD control, got %+v", ac)
			}

			bad := extProgram(tc.curves, nil, nil, nil, extEvent("BAD", epoch, 3600, tc.base))
			held := s.Evaluate(bad, epoch+10)
			if held == nil || !held.Held || held.MRID != "GOOD" {
				t.Fatalf("expected held GOOD, got %+v", held)
			}
			if !held.ImplausibleReject {
				t.Error("ImplausibleReject not set on a garbage-value hold")
			}
			if gotMRID != "BAD" || gotReason != tc.wantReason {
				t.Errorf("RejectHook got (%q, %q), want (BAD, %q)", gotMRID, gotReason, tc.wantReason)
			}
		})
	}
}

// TestEvaluate_PlausibleNewNumericsAdopted: in-range values for every new
// numeric adopt normally — the gates reject garbage, not real commands.
func TestEvaluate_PlausibleNewNumericsAdopted(t *testing.T) {
	energize := true
	base := model.ExtendedDERControlBase{
		OpModEnergize:       &energize,
		OpModGenLimW:        &model.ActivePower{Value: 4000},
		OpModLoadLimW:       &model.ActivePower{Value: 6000},
		OpModTargetW:        &model.ActivePower{Value: -2000},
		OpModFixedPFInjectW: &model.SignedPerCent{Value: 9500},  // 0.95 over-excited
		OpModFixedPFAbsorbW: &model.SignedPerCent{Value: -9000}, // 0.90 under-excited
		OpModFixedVar:       &model.FixedVar{Value: model.SignedPerCent{Value: -4400}},
		OpModVoltVar:        &model.CurveLink{Href: "/dc/vv"},
	}
	curves := map[string]model.DERCurve{"/dc/vv": voltVarCurve(10)} // 10 points: the inclusive bound
	programs := extProgram(curves, nil, nil, nil, extEvent("E1", epoch, 3600, base))

	s := New()
	hookFired := false
	s.RejectHook = func(string, string) { hookFired = true }
	ac := s.Evaluate(programs, epoch+10)
	if ac == nil || ac.MRID != "E1" || ac.Held {
		t.Fatalf("expected E1 adopted, got %+v", ac)
	}
	if hookFired {
		t.Error("RejectHook fired on a fully plausible control")
	}
}

// TestEvaluate_UnresolvableCurveHrefDoesNotReject: a dangling curve href is
// ignored-content (alarmed at the walker), NOT a plausibility reject — it
// must never unseat adoption of the control's scalar caps.
func TestEvaluate_UnresolvableCurveHrefDoesNotReject(t *testing.T) {
	base := model.ExtendedDERControlBase{
		OpModExpLimW: &model.ActivePower{Value: 1000},
		OpModVoltVar: &model.CurveLink{Href: "/dc/missing"},
	}
	programs := extProgram(nil, nil, nil, nil, extEvent("E1", epoch, 3600, base))

	s := New()
	s.RejectHook = func(mrid, reason string) {
		t.Errorf("RejectHook fired (%s, %s) for an unresolvable href", mrid, reason)
	}
	ac := s.Evaluate(programs, epoch+10)
	if ac == nil || ac.MRID != "E1" || ac.Held {
		t.Fatalf("expected E1 adopted despite the dangling href, got %+v", ac)
	}
	if got := ac.ResolvedModeCurves(); len(got) != 0 {
		t.Errorf("ResolvedModeCurves = %+v, want none (href does not resolve)", got)
	}
}

// TestPlausibleCurve pins the exported curve content gate.
func TestPlausibleCurve(t *testing.T) {
	if !PlausibleCurve(voltVarCurve(1)) || !PlausibleCurve(voltVarCurve(10)) {
		t.Error("1- and 10-point curves must be plausible (inclusive bounds)")
	}
	if PlausibleCurve(voltVarCurve(0)) {
		t.Error("0-point curve accepted — nothing to adopt")
	}
	if PlausibleCurve(voltVarCurve(11)) {
		t.Error("11-point curve accepted — 2030.5 tops out at 10")
	}
	// Extreme-but-representable multipliers stay plausible: int32×10^127 is
	// ~2.1e136, finite in float64 — the finite check is defense-in-depth for
	// future wider types, not a live gate (see PlausibleCurve's doc).
	extreme := voltVarCurve(2)
	extreme.YMultiplier = 127
	extreme.XMultiplier = -128
	if !PlausibleCurve(extreme) {
		t.Error("max-magnitude multipliers rejected — every wire-representable point is finite")
	}
}
