package bus

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

// wp9Doc is a fully-populated DesiredAdvanced fixture (every axis commanded),
// returned fresh each call so mutation in one test can't leak.
func wp9Doc() DesiredAdvanced {
	vv := wp8Entries()[0] // volt_var entry
	vw := wp8Entries()[1] // volt_watt entry
	f := func(v float64) *float64 { return &v }
	bTrue := true
	rvrt := int64(300)
	return DesiredAdvanced{
		Envelope:    Envelope{V: DesiredAdvancedV},
		DeviceClass: DesiredClassSolar,
		DeviceID:    "inverter-0",
		ReactiveMode: &AdvReactiveMode{
			Kind: AdvReactiveVoltVar,
			Curve: &AdvCurve{
				CurveType: vv.CurveType, XMult: vv.XMult, YMult: vv.YMult, YRefType: vv.YRefType,
				Points: vv.Points, Hash: CurveSetContentHash([]CurveSetEntry{vv}),
			},
		},
		VoltWatt: &AdvCurve{
			CurveType: vw.CurveType, Points: vw.Points,
			Hash: CurveSetContentHash([]CurveSetEntry{vw}),
		},
		FreqWatt:  &AdvCurve{CurveType: 1, Points: []CurvePoint{{X: 60200, Y: 10000}, {X: 62000, Y: 0}}, Hash: "fwhash"},
		FreqDroop: &AdvFreqDroop{DbOfHz: 0.036, DbUfHz: 0.036, KOf: 0.05, KUf: 0.05, OlrtS: 5},
		Trips: &AdvTrips{
			LV: &AdvTripSet{
				Curves: []AdvTripCurve{
					{Kind: AdvTripMustTrip, Curve: AdvCurve{CurveType: 9, Points: []CurvePoint{{X: 5000, Y: 200}}, Hash: "lvmust"}},
					{Kind: AdvTripMayTrip, Curve: AdvCurve{CurveType: 7, Points: []CurvePoint{{X: 7000, Y: 100}}, Hash: "lvmay"}},
				},
				Hash: "lvset",
			},
		},
		Energize:     &bTrue,
		SetGradW:     f(0.3),
		SetSoftGradW: f(0.1),
		RvrtTmsS:     &rvrt,
		Source:       "csip-event",
		MRID:         "EVT-1",
		IssuedAt:     1_750_000_000,
		Seq:          7,
	}
}

// TestDesiredAdvancedRoundTrip pins the wire shape (mirrors curves_test.go):
// "v" is emitted and a marshal/unmarshal round trip is lossless.
func TestDesiredAdvancedRoundTrip(t *testing.T) {
	in := wp9Doc()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"v":1`) {
		t.Fatalf("wire shape missing schema version: %s", b)
	}
	var out DesiredAdvanced
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round trip: got %+v, want %+v", out, in)
	}
}

// TestDesiredAdvancedExplicitNullAxes pins D6's explicit-null rule: the five
// mode axes are ALWAYS on the wire — an un-commanded axis is "<axis>":null
// (a release command), never an absent key — while the *T scalar opinions
// (energize, gradients, rvrt) follow DesiredState's omit-when-nil convention.
func TestDesiredAdvancedExplicitNullAxes(t *testing.T) {
	b, err := json.Marshal(DesiredAdvanced{
		Envelope:    Envelope{V: DesiredAdvancedV},
		DeviceClass: DesiredClassBattery,
		DeviceID:    "bat0",
		Source:      "none",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{
		`"reactive_mode":null`, `"volt_watt":null`, `"freq_watt":null`,
		`"freq_droop":null`, `"trips":null`,
	} {
		if !strings.Contains(string(b), key) {
			t.Errorf("axis must be an EXPLICIT null on the wire, missing %s: %s", key, b)
		}
	}
	for _, key := range []string{"energize", "set_grad_w", "set_soft_grad_w", "rvrt_tms_s", "mrid"} {
		if strings.Contains(string(b), key) {
			t.Errorf("nil scalar %q must be omitted, got %s", key, b)
		}
	}
}

// TestDesiredAdvancedTornStateImpossible pins the D6 structural guarantee:
// the reactive-power opinion is ONE field. (a) By reflection, no top-level
// field of DesiredAdvanced carries a reactive kind's wire key — the only
// reactive carrier is "reactive_mode", so two concurrent reactive modes are
// unrepresentable, not just un-authored. (b) On the wire, a full doc carries
// exactly one "reactive_mode" key with exactly one "kind".
func TestDesiredAdvancedTornStateImpossible(t *testing.T) {
	typ := reflect.TypeOf(DesiredAdvanced{})
	reactiveKeys := map[string]bool{
		AdvReactiveFixedPF: true, AdvReactiveFixedVar: true,
		AdvReactiveVoltVar: true, AdvReactiveWattVar: true,
		"fixed_var_pct": true, // AdvReactiveMode's payload key (fixed_pf == AdvReactiveFixedPF above)
	}
	for i := 0; i < typ.NumField(); i++ {
		tag := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if reactiveKeys[tag] {
			t.Errorf("top-level field %q (key %q) re-introduces a second reactive carrier — D6 forbids it", typ.Field(i).Name, tag)
		}
	}

	b, err := json.Marshal(wp9Doc())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if n := strings.Count(string(b), `"reactive_mode"`); n != 1 {
		t.Errorf("want exactly one reactive_mode key, got %d: %s", n, b)
	}
	if n := strings.Count(string(b), `"kind":"volt_var"`); n != 1 {
		t.Errorf("want exactly one reactive kind, got %d: %s", n, b)
	}
}

// TestDesiredAdvancedTopicPolicy mirrors curves_test.go's topic-policy test,
// plus the family-split pin: the adv arm must win over the broader
// lexa/desired/ prefix, and the scalar DesiredState family must be untouched.
func TestDesiredAdvancedTopicPolicy(t *testing.T) {
	topic := DesiredAdvTopic("inverter-0")
	if topic != "lexa/desired/adv/inverter-0" {
		t.Errorf("DesiredAdvTopic = %q", topic)
	}
	if got := PubQoS(topic); got != QoS1 {
		t.Errorf("PubQoS(%q) = %d, want QoS1", topic, got)
	}
	if got := SupportedV(topic); got != DesiredAdvancedV {
		t.Errorf("SupportedV(%q) = %d, want DesiredAdvancedV", topic, got)
	}
	// The scalar desired family still resolves to its own constant.
	if got := SupportedV(DesiredTopic(DesiredClassBattery, "bat0")); got != DesiredStateV {
		t.Errorf("SupportedV(battery desired) = %d, want DesiredStateV", got)
	}
	// "adv" rides the {class} segment, so the existing extraction helpers work.
	if got := DeviceFromDesiredTopic(topic); got != "inverter-0" {
		t.Errorf("DeviceFromDesiredTopic(%q) = %q", topic, got)
	}
	if got := ClassFromDesiredTopic(topic); got != "adv" {
		t.Errorf("ClassFromDesiredTopic(%q) = %q, want adv", topic, got)
	}
	if SubDesiredAdv != "lexa/desired/adv/+" {
		t.Errorf("SubDesiredAdv = %q", SubDesiredAdv)
	}
}

// TestDesiredAdvancedVersionGate mirrors curves_test.go's CheckVersion gate.
func TestDesiredAdvancedVersionGate(t *testing.T) {
	topic := DesiredAdvTopic("bat0")
	ok := []byte(`{"v":1,"device_class":"battery","device_id":"bat0"}`)
	if err := CheckVersion(topic, ok, SupportedV(topic)); err != nil {
		t.Fatalf("v=1 rejected: %v", err)
	}
	future := []byte(`{"v":2,"device_id":"bat0"}`)
	err := CheckVersion(topic, future, SupportedV(topic))
	if err == nil {
		t.Fatal("v=2 accepted, want rejection")
	}
	if _, isVE := err.(*VersionError); !isVE {
		t.Fatalf("want *VersionError, got %T", err)
	}
}

// TestDesiredAdvancedFinite: every float surface joins the GAP-09 check —
// gradients, the reactive payload (fixed_var_pct, fixed_pf.pf), and all five
// droop parameters.
func TestDesiredAdvancedFinite(t *testing.T) {
	nan := math.NaN()
	inf := math.Inf(1)
	cases := []struct {
		name string
		doc  DesiredAdvanced
	}{
		{"SetGradW", DesiredAdvanced{SetGradW: &nan}},
		{"SetSoftGradW", DesiredAdvanced{SetSoftGradW: &inf}},
		{"FixedVarPct", DesiredAdvanced{ReactiveMode: &AdvReactiveMode{Kind: AdvReactiveFixedVar, FixedVarPct: &nan}}},
		{"FixedPF.PF", DesiredAdvanced{ReactiveMode: &AdvReactiveMode{Kind: AdvReactiveFixedPF, FixedPF: &FixedPF{PF: inf}}}},
		{"Droop.DbOf", DesiredAdvanced{FreqDroop: &AdvFreqDroop{DbOfHz: nan}}},
		{"Droop.DbUf", DesiredAdvanced{FreqDroop: &AdvFreqDroop{DbUfHz: inf}}},
		{"Droop.KOf", DesiredAdvanced{FreqDroop: &AdvFreqDroop{KOf: nan}}},
		{"Droop.KUf", DesiredAdvanced{FreqDroop: &AdvFreqDroop{KUf: inf}}},
		{"Droop.OlrtS", DesiredAdvanced{FreqDroop: &AdvFreqDroop{OlrtS: nan}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.doc.Finite() == nil {
				t.Fatalf("Finite() = nil for a doc with a non-finite %s", tc.name)
			}
		})
	}
	if err := wp9Doc().Finite(); err != nil {
		t.Fatalf("Finite() on a finite doc = %v, want nil", err)
	}
}
