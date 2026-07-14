package bus

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

// wp8Entries is the shared two-mode fixture the pinned-hash and permutation
// tests use. Returned fresh each call so mutation in one test can't leak.
func wp8Entries() []CurveSetEntry {
	return []CurveSetEntry{
		{
			Mode: CurveModeVoltVar, MRID: "CRV-VV", CurveType: 0, XMult: 0, YMult: 0, YRefType: 3,
			Points: []CurvePoint{{X: 9200, Y: 3000}, {X: 10800, Y: -3000}},
		},
		{
			Mode: CurveModeVoltWatt, MRID: "CRV-VW", CurveType: 3, XMult: 0, YMult: 0,
			Points: []CurvePoint{{X: 10600, Y: 10000}, {X: 11000, Y: 2000}},
		},
	}
}

// TestCurveSetRoundTrip pins the wire shape (mirrors logevent_test.go): the
// embedded Envelope's "v" key is actually emitted (no field-collision
// shadowing) and a marshal/unmarshal round trip is lossless.
func TestCurveSetRoundTrip(t *testing.T) {
	in := CurveSet{
		Envelope: Envelope{V: CurveSetV},
		SetID:    CurveSetContentHash(wp8Entries()),
		MRID:     "EVT-1",
		Program:  "/derp/0",
		Curves:   wp8Entries(),
		Ts:       1750000000,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"v":1`) {
		t.Fatalf("wire shape missing schema version: %s", b)
	}
	var out CurveSet
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round trip: got %+v, want %+v", out, in)
	}
}

// TestCurveSetTopicPolicy mirrors logevent_test.go's topic-policy test: the
// retained curve doc publishes at QoS 1 (PubQoS's non-measurement default)
// and version-checks against CurveSetV.
func TestCurveSetTopicPolicy(t *testing.T) {
	if got := PubQoS(TopicCSIPCurves); got != QoS1 {
		t.Errorf("PubQoS(%q) = %d, want QoS1", TopicCSIPCurves, got)
	}
	if got := SupportedV(TopicCSIPCurves); got != CurveSetV {
		t.Errorf("SupportedV(%q) = %d, want %d", TopicCSIPCurves, got, CurveSetV)
	}
}

// TestCurveSetVersionGate mirrors logevent_test.go's CheckVersion gate: v=1
// accepted, v>supported rejected with a *VersionError.
func TestCurveSetVersionGate(t *testing.T) {
	ok := []byte(`{"v":1,"set_id":"abc","curves":[]}`)
	if err := CheckVersion(TopicCSIPCurves, ok, SupportedV(TopicCSIPCurves)); err != nil {
		t.Fatalf("v=1 rejected: %v", err)
	}
	future := []byte(`{"v":2,"set_id":"abc"}`)
	err := CheckVersion(TopicCSIPCurves, future, SupportedV(TopicCSIPCurves))
	if err == nil {
		t.Fatal("v=2 accepted, want rejection")
	}
	if _, isVE := err.(*VersionError); !isVE {
		t.Fatalf("want *VersionError, got %T", err)
	}
}

// TestCurveSetContentHash_Pinned pins the canonicalization: this exact
// two-entry fixture must hash to this exact digest forever (any change to
// the canonical form is a CurveSetV bump — see CurveSetContentHash's doc).
func TestCurveSetContentHash_Pinned(t *testing.T) {
	const pinned = "9cd172a751cb79c865a431d54869e6da5c523486d8deb5487d89926bbd7d418a"
	if got := CurveSetContentHash(wp8Entries()); got != pinned {
		t.Fatalf("CurveSetContentHash = %s, want pinned %s (canonicalization drifted — that is a CurveSetV version bump, not a test update)", got, pinned)
	}
}

// TestCurveSetContentHash_Stability: the hash is invariant under entry-order
// permutation (canonicalization sorts by Mode) and under metadata-only
// changes (MRID is resource identity, not content), but sensitive to any
// content change (a moved breakpoint, a multiplier, the mode binding).
func TestCurveSetContentHash_Stability(t *testing.T) {
	base := CurveSetContentHash(wp8Entries())
	if base == "" {
		t.Fatal("non-empty entries hashed to the empty sentinel")
	}

	// Order permutation: reversed entries, same hash.
	rev := wp8Entries()
	rev[0], rev[1] = rev[1], rev[0]
	if got := CurveSetContentHash(rev); got != base {
		t.Errorf("entry-order permutation moved the hash: %s != %s", got, base)
	}

	// Metadata-only change: entry MRID is excluded from the hash.
	renamed := wp8Entries()
	renamed[0].MRID = "REISSUED-UNDER-NEW-MRID"
	if got := CurveSetContentHash(renamed); got != base {
		t.Errorf("MRID (metadata) change moved the content hash: %s != %s", got, base)
	}

	// Content changes: each must move the hash.
	for name, mutate := range map[string]func([]CurveSetEntry){
		"point":     func(e []CurveSetEntry) { e[0].Points[0].Y = 3001 },
		"mode":      func(e []CurveSetEntry) { e[0].Mode = CurveModeWattPF },
		"xmult":     func(e []CurveSetEntry) { e[1].XMult = 1 },
		"yreftype":  func(e []CurveSetEntry) { e[0].YRefType = 4 },
		"curvetype": func(e []CurveSetEntry) { e[1].CurveType = 1 },
	} {
		mut := wp8Entries()
		mutate(mut)
		if got := CurveSetContentHash(mut); got == base {
			t.Errorf("%s change did NOT move the hash", name)
		}
	}

	// Empty set: the "" sentinel (matches ActiveControl.CurveSetID's
	// "" = no curves convention, architecture §2.2).
	if got := CurveSetContentHash(nil); got != "" {
		t.Errorf("empty entries hash = %q, want \"\"", got)
	}
}

// TestActiveControlWP8RoundTrip pins the additive §2.2 fields' wire shape:
// all new keys emitted with their normative names, "v" still present, and a
// lossless round trip.
func TestActiveControlWP8RoundTrip(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	bTrue := true
	rvrt := int64(300)
	in := ActiveControl{
		Envelope:      Envelope{V: ActiveControlV},
		Source:        "event",
		MRID:          "EVT-1",
		Energize:      &bTrue,
		GenLimW:       f(4000),
		LoadLimW:      f(6000),
		TargetW:       f(-2000),
		FixedPFInject: &FixedPF{PF: 0.95, OverExcited: true},
		FixedPFAbsorb: &FixedPF{PF: 0.9, OverExcited: false},
		FixedVarPct:   f(-44),
		SetGradW:      f(0.3),
		SetSoftGradW:  f(0.1),
		RvrtTmsS:      &rvrt,
		CurveSetID:    "abc123",
		ClockOffset:   5,
		Ts:            1750000000,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{
		`"v":1`, `"energize":true`, `"gen_lim_w":4000`, `"load_lim_w":6000`,
		`"target_w":-2000`, `"fixed_pf_inject":{"pf":0.95,"over_excited":true}`,
		`"fixed_pf_absorb":{"pf":0.9,"over_excited":false}`, `"fixed_var_pct":-44`,
		`"set_grad_w":0.3`, `"set_soft_grad_w":0.1`, `"rvrt_tms_s":300`,
		`"curve_set_id":"abc123"`,
	} {
		if !strings.Contains(string(b), key) {
			t.Errorf("wire shape missing %s: %s", key, b)
		}
	}
	var out ActiveControl
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round trip: got %+v, want %+v", out, in)
	}
}

// TestActiveControlWP8FieldsOmittedWhenAbsent: a pre-WP-8-shaped publish
// (all new fields nil/"") emits none of the new keys — old golden payloads
// stay byte-compatible and old subscribers see nothing new.
func TestActiveControlWP8FieldsOmittedWhenAbsent(t *testing.T) {
	b, err := json.Marshal(ActiveControl{Envelope: Envelope{V: ActiveControlV}, Source: "none", Ts: 1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{
		"energize", "gen_lim_w", "load_lim_w", "target_w", "fixed_pf_inject",
		"fixed_pf_absorb", "fixed_var_pct", "set_grad_w", "set_soft_grad_w",
		"rvrt_tms_s", "curve_set_id",
	} {
		if strings.Contains(string(b), key) {
			t.Errorf("absent field %q leaked onto the wire: %s", key, b)
		}
	}
}

// TestActiveControlWP8Finite: every new *float64 (and FixedPF.PF) joins the
// GAP-09 Finite() check — same table shape as
// TestActiveControlNaNLimitNeverReachesOptimizer for the original limits.
func TestActiveControlWP8Finite(t *testing.T) {
	nan := math.NaN()
	inf := math.Inf(1)
	cases := []struct {
		name string
		ctrl ActiveControl
	}{
		{"GenLimW", ActiveControl{Source: "event", GenLimW: &nan, Ts: 1}},
		{"LoadLimW", ActiveControl{Source: "event", LoadLimW: &inf, Ts: 1}},
		{"TargetW", ActiveControl{Source: "event", TargetW: &nan, Ts: 1}},
		{"FixedVarPct", ActiveControl{Source: "event", FixedVarPct: &inf, Ts: 1}},
		{"SetGradW", ActiveControl{Source: "default", SetGradW: &nan, Ts: 1}},
		{"SetSoftGradW", ActiveControl{Source: "default", SetSoftGradW: &nan, Ts: 1}},
		{"FixedPFInject.PF", ActiveControl{Source: "event", FixedPFInject: &FixedPF{PF: nan}, Ts: 1}},
		{"FixedPFAbsorb.PF", ActiveControl{Source: "event", FixedPFAbsorb: &FixedPF{PF: inf}, Ts: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.ctrl.Finite() == nil {
				t.Fatalf("Finite() = nil for an ActiveControl with a non-finite %s", tc.name)
			}
		})
	}
	ok := ActiveControl{
		Source:        "event",
		GenLimW:       new(float64),
		FixedPFInject: &FixedPF{PF: 0.95, OverExcited: true},
		Ts:            1,
	}
	if err := ok.Finite(); err != nil {
		t.Fatalf("Finite() on a finite WP-8 control = %v, want nil", err)
	}
}
