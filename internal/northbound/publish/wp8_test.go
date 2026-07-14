package publish

// WP-8 tests: advanced-control scalars on ToActiveControl, the retained
// lexa/csip/curves publisher, and golden wire fixtures for both docs.
// Additive alongside publish_test.go (which must pass unchanged) and
// reusing its fakeClient/apw helpers.

import (
	"encoding/json"
	"reflect"
	"testing"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/northbound/discovery"
	"lexa-hub/internal/northbound/scheduler"
	model "lexa-proto/csipmodel"
)

func u16p(v uint16) *uint16 { return &v }

func wp8VoltVarCurve() model.DERCurve {
	return model.DERCurve{
		Resource:  model.Resource{Href: "/dc/vv"},
		MRID:      "CRV-VV",
		CurveType: model.CurveTypeVoltVar,
		YRefType:  3,
		CurveData: []model.DERCurveData{
			{XValue: 9200, YValue: 3000},
			{XValue: 10800, YValue: -3000},
		},
	}
}

// wp8SingleCurveHash is the pinned CurveSetContentHash of wp8VoltVarCurve
// bound to volt_var (see bus.TestCurveSetContentHash_Pinned for the
// canonicalization pin; this constant just saves recomputing it here).
const wp8SingleCurveHash = "5adc9f30b89b162db83ec36d38458435cd35a83ad75b000ab78f383b57d03fd0"

// wp8EventControl is a fully-loaded extended event control, the shape the
// scheduler carries post-WP-8.
func wp8EventControl() *scheduler.ActiveControl {
	energize := true
	connect := true
	ext := &model.ExtendedDERControlBase{
		OpModTargetW: apw(0, -2000),
		OpModVoltVar: &model.CurveLink{Href: "/dc/vv"},
	}
	return &scheduler.ActiveControl{
		Source:     "event",
		MRID:       "EVT-1",
		ValidUntil: 1700003600,
		Base: model.DERControlBase{
			OpModConnect:        &connect,
			OpModEnergize:       &energize,
			OpModExpLimW:        apw(0, 1500),
			OpModGenLimW:        apw(0, 4000),
			OpModLoadLimW:       apw(0, 6000),
			OpModFixedPFInjectW: &model.SignedPerCent{Value: 9500},  // 0.95 over-excited
			OpModFixedPFAbsorbW: &model.SignedPerCent{Value: -9000}, // 0.90 under-excited
			OpModFixedVar:       &model.FixedVar{Value: model.SignedPerCent{Value: -4400}},
		},
		Extended:    ext,
		Curves:      map[string]model.DERCurve{"/dc/vv": wp8VoltVarCurve()},
		ProgramHref: "/derp/0",
	}
}

// TestToActiveControl_WP8FieldMapping is the extended-event → ActiveControl
// passthrough table: every §2.2 scalar decodes with the right units, signs,
// and excitation convention, and curve_set_id names the resolved content.
func TestToActiveControl_WP8FieldMapping(t *testing.T) {
	msg := ToActiveControl(wp8EventControl(), 7)

	if msg.Energize == nil || !*msg.Energize {
		t.Errorf("Energize = %v, want true", msg.Energize)
	}
	if msg.GenLimW == nil || *msg.GenLimW != 4000 {
		t.Errorf("GenLimW = %v, want 4000", msg.GenLimW)
	}
	if msg.LoadLimW == nil || *msg.LoadLimW != 6000 {
		t.Errorf("LoadLimW = %v, want 6000", msg.LoadLimW)
	}
	if msg.TargetW == nil || *msg.TargetW != -2000 {
		t.Errorf("TargetW = %v, want -2000", msg.TargetW)
	}
	if msg.FixedPFInject == nil || *msg.FixedPFInject != (bus.FixedPF{PF: 0.95, OverExcited: true}) {
		t.Errorf("FixedPFInject = %+v, want {0.95 true}", msg.FixedPFInject)
	}
	if msg.FixedPFAbsorb == nil || *msg.FixedPFAbsorb != (bus.FixedPF{PF: 0.9, OverExcited: false}) {
		t.Errorf("FixedPFAbsorb = %+v, want {0.9 false}", msg.FixedPFAbsorb)
	}
	if msg.FixedVarPct == nil || *msg.FixedVarPct != -44 {
		t.Errorf("FixedVarPct = %v, want -44", msg.FixedVarPct)
	}
	if msg.CurveSetID != wp8SingleCurveHash {
		t.Errorf("CurveSetID = %q, want %q", msg.CurveSetID, wp8SingleCurveHash)
	}
	if msg.SetGradW != nil || msg.SetSoftGradW != nil {
		t.Errorf("event control emitted default-only ramp fields: %v/%v", msg.SetGradW, msg.SetSoftGradW)
	}
	if msg.RvrtTmsS != nil {
		t.Errorf("RvrtTmsS = %v, want nil (computed hub-side, WP-9/C3)", msg.RvrtTmsS)
	}
	// The pre-WP-8 scalars are untouched by the new mapping.
	if msg.ExpLimW == nil || *msg.ExpLimW != 1500 || msg.Connect == nil || !*msg.Connect {
		t.Errorf("legacy scalars regressed: exp=%v connect=%v", msg.ExpLimW, msg.Connect)
	}
}

// TestToActiveControl_DefaultRampFields: the DefaultDERControl-only
// setGradW/setSoftGradW decode from hundredths-of-a-percent to percent of
// setMaxW per second, and only when the scheduler carried them (default
// source).
func TestToActiveControl_DefaultRampFields(t *testing.T) {
	ac := &scheduler.ActiveControl{
		Source:       "default",
		MRID:         "DD1",
		Base:         model.DERControlBase{OpModExpLimW: apw(0, 5000)},
		SetGradW:     u16p(30), // 30 hundredths of a % per s = 0.3 %/s
		SetSoftGradW: u16p(10),
	}
	msg := ToActiveControl(ac, 0)
	if msg.SetGradW == nil || *msg.SetGradW != 0.3 {
		t.Errorf("SetGradW = %v, want 0.3", msg.SetGradW)
	}
	if msg.SetSoftGradW == nil || *msg.SetSoftGradW != 0.1 {
		t.Errorf("SetSoftGradW = %v, want 0.1", msg.SetSoftGradW)
	}
	if msg.CurveSetID != "" {
		t.Errorf("CurveSetID = %q, want \"\" (no curves)", msg.CurveSetID)
	}
}

// TestToActiveControl_WP8GoldenFixture pins the full wire bytes for the
// loaded event control (Ts pinned post-conversion — it is the only
// wall-clock field).
func TestToActiveControl_WP8GoldenFixture(t *testing.T) {
	msg := ToActiveControl(wp8EventControl(), 7)
	msg.Ts = 1700000000
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	golden := `{"v":1,"source":"event","mrid":"EVT-1","connect":true,"exp_lim_w":1500,"clock_offset":7,"valid_until":1700003600,"energize":true,"gen_lim_w":4000,"load_lim_w":6000,"target_w":-2000,"fixed_pf_inject":{"pf":0.95,"over_excited":true},"fixed_pf_absorb":{"pf":0.9,"over_excited":false},"fixed_var_pct":-44,"curve_set_id":"` + wp8SingleCurveHash + `","ts":1700000000}`
	if string(b) != golden {
		t.Errorf("ActiveControl wire drift:\n got %s\nwant %s", b, golden)
	}
}

// TestCurves_PublishRetainedDedupeAndChange covers the Curves publisher's
// whole contract: first publish (retained, QoS 1, correct entries + set id),
// dedupe on unchanged content, republish on content change, and the
// explicit empty-set publish when curves are released.
func TestCurves_PublishRetainedDedupeAndChange(t *testing.T) {
	fc := &fakeClient{}
	var cp Curves
	ac := wp8EventControl()

	cp.Publish(fc, ac)
	if len(fc.publishes) != 1 {
		t.Fatalf("first Publish: %d publishes, want 1", len(fc.publishes))
	}
	p := fc.publishes[0]
	if p.topic != bus.TopicCSIPCurves || p.qos != 1 || !p.retained {
		t.Fatalf("publish = topic=%s qos=%d retained=%v, want %s/1/true", p.topic, p.qos, p.retained, bus.TopicCSIPCurves)
	}
	var msg bus.CurveSet
	if err := json.Unmarshal(p.payload, &msg); err != nil {
		t.Fatalf("unmarshal CurveSet: %v", err)
	}
	if msg.V != bus.CurveSetV || msg.SetID != wp8SingleCurveHash || msg.MRID != "EVT-1" || msg.Program != "/derp/0" {
		t.Errorf("CurveSet identity = v=%d set_id=%q mrid=%q program=%q", msg.V, msg.SetID, msg.MRID, msg.Program)
	}
	wantEntry := bus.CurveSetEntry{
		Mode: bus.CurveModeVoltVar, MRID: "CRV-VV", CurveType: 0, YRefType: 3,
		Points: []bus.CurvePoint{{X: 9200, Y: 3000}, {X: 10800, Y: -3000}},
	}
	if len(msg.Curves) != 1 || !reflect.DeepEqual(msg.Curves[0], wantEntry) {
		t.Errorf("Curves = %+v, want [%+v]", msg.Curves, wantEntry)
	}

	// Same content again: dedupe — no second publish.
	cp.Publish(fc, ac)
	if len(fc.publishes) != 1 {
		t.Fatalf("unchanged content republished: %d publishes, want 1", len(fc.publishes))
	}

	// Content change (one breakpoint moves): republish with a new set id.
	changed := wp8EventControl()
	c := changed.Curves["/dc/vv"]
	c.CurveData[0].YValue = 3001
	changed.Curves["/dc/vv"] = c
	cp.Publish(fc, changed)
	if len(fc.publishes) != 2 {
		t.Fatalf("changed content not republished: %d publishes, want 2", len(fc.publishes))
	}
	var msg2 bus.CurveSet
	if err := json.Unmarshal(fc.publishes[1].payload, &msg2); err != nil {
		t.Fatalf("unmarshal changed CurveSet: %v", err)
	}
	if msg2.SetID == wp8SingleCurveHash || msg2.SetID == "" {
		t.Errorf("changed content SetID = %q, want a new non-empty hash", msg2.SetID)
	}

	// Curves released (control without curves): explicit empty set with the
	// "" sentinel supersedes the retained content.
	bare := &scheduler.ActiveControl{Source: "default", MRID: "DD1"}
	cp.Publish(fc, bare)
	if len(fc.publishes) != 3 {
		t.Fatalf("released curves not republished: %d publishes, want 3", len(fc.publishes))
	}
	var msg3 bus.CurveSet
	if err := json.Unmarshal(fc.publishes[2].payload, &msg3); err != nil {
		t.Fatalf("unmarshal empty CurveSet: %v", err)
	}
	if msg3.SetID != "" || len(msg3.Curves) != 0 {
		t.Errorf("released set = set_id=%q curves=%d, want \"\"/0", msg3.SetID, len(msg3.Curves))
	}

	// nil active control (source none): same empty content — deduped against
	// the empty set just published.
	cp.Publish(fc, nil)
	if len(fc.publishes) != 3 {
		t.Fatalf("nil control republished an unchanged empty set: %d publishes, want 3", len(fc.publishes))
	}
}

// TestCurves_FirstPublishAlwaysFires: a fresh process with an empty curve
// set still publishes once, superseding whatever stale retained set a
// previous run left on the broker.
func TestCurves_FirstPublishAlwaysFires(t *testing.T) {
	fc := &fakeClient{}
	var cp Curves
	cp.Publish(fc, nil)
	if len(fc.publishes) != 1 {
		t.Fatalf("first empty publish suppressed: %d publishes, want 1", len(fc.publishes))
	}
	var msg bus.CurveSet
	if err := json.Unmarshal(fc.publishes[0].payload, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.SetID != "" || len(msg.Curves) != 0 {
		t.Errorf("empty set = %+v, want set_id \"\" and no curves", msg)
	}
}

// TestCurves_MalformedEntryNeverPublished: a resolved curve failing the
// content gate (here: 11 breakpoints) must not produce a CurveSet entry —
// it is dropped, counted as ignored content, and the set id reflects only
// the published (valid) content. The scheduler normally rejects such a
// control outright; this pins the publisher's own defensive line.
func TestCurves_MalformedEntryNeverPublished(t *testing.T) {
	ac := wp8EventControl()
	bad := wp8VoltVarCurve()
	bad.CurveData = nil
	for i := 0; i < 11; i++ {
		bad.CurveData = append(bad.CurveData, model.DERCurveData{XValue: int32(i), YValue: int32(i)})
	}
	bad.MRID = "CRV-BAD"
	ac.Extended.OpModVoltWatt = &model.CurveLink{Href: "/dc/bad"}
	ac.Curves["/dc/bad"] = bad

	before := discovery.IgnoredContentTotal()
	fc := &fakeClient{}
	var cp Curves
	cp.Publish(fc, ac)

	if got := discovery.IgnoredContentTotal(); got != before+1 {
		t.Errorf("IgnoredContentTotal = %d, want %d (malformed entry must be alarmed, not silent)", got, before+1)
	}
	var msg bus.CurveSet
	if err := json.Unmarshal(fc.only(t).payload, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(msg.Curves) != 1 || msg.Curves[0].MRID != "CRV-VV" {
		t.Fatalf("Curves = %+v, want only the valid CRV-VV entry", msg.Curves)
	}
	if msg.SetID != wp8SingleCurveHash {
		t.Errorf("SetID = %q, want the valid-content hash %q", msg.SetID, wp8SingleCurveHash)
	}
}

// TestCurveSet_GoldenFixture pins the CurveSet wire bytes (Ts pinned).
func TestCurveSet_GoldenFixture(t *testing.T) {
	fc := &fakeClient{}
	var cp Curves
	cp.Publish(fc, wp8EventControl())

	var msg bus.CurveSet
	if err := json.Unmarshal(fc.only(t).payload, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msg.Ts = 1700000000
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	golden := `{"v":1,"set_id":"` + wp8SingleCurveHash + `","mrid":"EVT-1","program":"/derp/0","curves":[{"mode":"volt_var","mrid":"CRV-VV","curve_type":0,"y_ref_type":3,"points":[{"x":9200,"y":3000},{"x":10800,"y":-3000}]}],"ts":1700000000}`
	if string(b) != golden {
		t.Errorf("CurveSet wire drift:\n got %s\nwant %s", b, golden)
	}
}
