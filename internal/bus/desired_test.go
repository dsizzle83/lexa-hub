package bus

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// TestDesiredStateRoundTrip verifies a fully-populated document for each class
// survives marshal → unmarshal unchanged, including the envelope version.
func TestDesiredStateRoundTrip(t *testing.T) {
	cases := []DesiredState{
		{
			Envelope:    Envelope{V: DesiredStateV},
			DeviceClass: DesiredClassBattery,
			DeviceID:    "bat0",
			SetpointW:   float64ptr(-2500), // charge
			Connect:     boolptr(true),
			Source:      "economic",
			IssuedAt:    1_700_000_000,
			Seq:         42,
		},
		{
			Envelope:    Envelope{V: DesiredStateV},
			DeviceClass: DesiredClassSolar,
			DeviceID:    "inverter-0",
			CeilingW:    float64ptr(4000),
			Source:      "csip-event",
			MRID:        "ABCD1234",
			IssuedAt:    1_700_000_001,
			Seq:         7,
		},
		{
			Envelope:    Envelope{V: DesiredStateV},
			DeviceClass: DesiredClassEVSE,
			DeviceID:    "station-1",
			MaxCurrentA: float64ptr(16),
			ConnectorID: 1,
			Source:      "csip-default",
			IssuedAt:    1_700_000_002,
			Seq:         3,
		},
	}
	for _, want := range cases {
		t.Run(want.DeviceClass, func(t *testing.T) {
			b, err := json.Marshal(want)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got DesiredState
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got.V != DesiredStateV {
				t.Errorf("version: got %d want %d (wire: %s)", got.V, DesiredStateV, b)
			}
			if !equalDesired(got, want) {
				t.Errorf("round-trip mismatch:\n got  %+v\n want %+v\n wire %s", got, want, b)
			}
		})
	}
}

// TestDesiredStateVersionOnWire verifies the embedded Envelope actually emits
// "v" (the shadowing bug that bit Measurement's voltage field must not recur:
// DesiredState has no other field keyed "v").
func TestDesiredStateVersionOnWire(t *testing.T) {
	b, err := json.Marshal(DesiredState{Envelope: Envelope{V: DesiredStateV}, DeviceClass: DesiredClassBattery, DeviceID: "bat0"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"v":1`) {
		t.Errorf(`expected "v":1 on the wire, got %s`, b)
	}
}

// TestDesiredStateNilVsZero verifies the AD-013 field-absence rule: a nil *T
// field is omitted from the wire ("no opinion"), while an explicit zero is
// present ("command": idle / suspend). Conflating the two is the silent-zero
// bug class this schema exists to prevent.
func TestDesiredStateNilVsZero(t *testing.T) {
	// nil: fields must be absent.
	noOpinion, err := json.Marshal(DesiredState{
		Envelope: Envelope{V: DesiredStateV}, DeviceClass: DesiredClassBattery, DeviceID: "bat0",
		SetpointW: nil, Connect: nil, CeilingW: nil, MaxCurrentA: nil,
	})
	if err != nil {
		t.Fatalf("marshal nil: %v", err)
	}
	for _, key := range []string{"setpoint_w", "connect", "ceiling_w", "max_current_a"} {
		if strings.Contains(string(noOpinion), key) {
			t.Errorf("nil field %q must be omitted, got %s", key, noOpinion)
		}
	}

	// explicit zero: fields must be present, and unmarshal back to a non-nil zero.
	zero := DesiredState{
		Envelope: Envelope{V: DesiredStateV}, DeviceClass: DesiredClassBattery, DeviceID: "bat0",
		SetpointW: float64ptr(0), Connect: boolptr(false),
	}
	b, err := json.Marshal(zero)
	if err != nil {
		t.Fatalf("marshal zero: %v", err)
	}
	if !strings.Contains(string(b), `"setpoint_w":0`) {
		t.Errorf(`explicit 0 setpoint must serialize as "setpoint_w":0, got %s`, b)
	}
	if !strings.Contains(string(b), `"connect":false`) {
		t.Errorf(`explicit false connect must serialize as "connect":false, got %s`, b)
	}
	var got DesiredState
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal zero: %v", err)
	}
	if got.SetpointW == nil || *got.SetpointW != 0 {
		t.Errorf("explicit-zero setpoint must round-trip to &0, got %v", got.SetpointW)
	}
	if got.Connect == nil || *got.Connect != false {
		t.Errorf("explicit-false connect must round-trip to &false, got %v", got.Connect)
	}
}

// TestDesiredStateNaNNeverSerialized mirrors nan_test.go: nil pointer fields
// never produce a NaN literal, and a *float64 pointing to NaN fails marshal
// (producers must pass nil, not &NaN).
func TestDesiredStateNaNNeverSerialized(t *testing.T) {
	b, err := json.Marshal(DesiredState{
		Envelope: Envelope{V: DesiredStateV}, DeviceClass: DesiredClassSolar, DeviceID: "inverter-0",
		CeilingW: nil, SetpointW: nil, MaxCurrentA: nil,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if containsNaN(string(b)) {
		t.Errorf("marshaled JSON contains NaN literal: %s", b)
	}

	nan := math.NaN()
	if _, err := json.Marshal(DesiredState{
		Envelope: Envelope{V: DesiredStateV}, DeviceClass: DesiredClassSolar, DeviceID: "inverter-0",
		CeilingW: &nan,
	}); err == nil {
		t.Error("expected marshal to fail for CeilingW pointing to NaN; producers must pass nil")
	}
}

// TestDesiredTopicHelpers verifies DesiredTopic composition and the
// class/device extraction helpers are exact inverses across all classes.
func TestDesiredTopicHelpers(t *testing.T) {
	cases := []struct{ class, device string }{
		{DesiredClassBattery, "bat0"},
		{DesiredClassSolar, "inverter-0"},
		{DesiredClassEVSE, "station-1"},
	}
	for _, c := range cases {
		topic := DesiredTopic(c.class, c.device)
		if want := "lexa/desired/" + c.class + "/" + c.device; topic != want {
			t.Errorf("DesiredTopic = %q, want %q", topic, want)
		}
		if got := ClassFromDesiredTopic(topic); got != c.class {
			t.Errorf("ClassFromDesiredTopic(%q) = %q, want %q", topic, got, c.class)
		}
		if got := DeviceFromDesiredTopic(topic); got != c.device {
			t.Errorf("DeviceFromDesiredTopic(%q) = %q, want %q", topic, got, c.device)
		}
	}
	// SubDesired must match the two-level wildcard shape.
	if SubDesired != "lexa/desired/+/+" {
		t.Errorf("SubDesired = %q, want lexa/desired/+/+", SubDesired)
	}
}

func boolptr(b bool) *bool { return &b }

func equalDesired(a, b DesiredState) bool {
	return a.V == b.V &&
		a.DeviceClass == b.DeviceClass && a.DeviceID == b.DeviceID &&
		eqF(a.CeilingW, b.CeilingW) && eqF(a.SetpointW, b.SetpointW) &&
		eqB(a.Connect, b.Connect) && eqF(a.MaxCurrentA, b.MaxCurrentA) &&
		a.ConnectorID == b.ConnectorID && a.Source == b.Source && a.MRID == b.MRID &&
		a.IssuedAt == b.IssuedAt && a.Seq == b.Seq
}

func eqF(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func eqB(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
