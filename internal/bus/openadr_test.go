package bus

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

// TestOpenADRPricesRoundTrip pins the wire shape (mirrors dersite_test.go):
// the embedded Envelope's "v" key is emitted and a marshal/unmarshal round
// trip is lossless, including nested series/intervals.
func TestOpenADRPricesRoundTrip(t *testing.T) {
	in := OpenADRPrices{
		Envelope: Envelope{V: OpenADRPricesV},
		Series: []OpenADRPriceSeries{{
			ProgramID: "prog-1",
			EventID:   "evt-1",
			Kind:      "PRICE",
			Currency:  "USD",
			Units:     "KWH",
			Intervals: []OpenADRPriceInterval{
				{StartTs: 1752480000, Start: "2026-07-14T08:00:00Z", DurationS: 900, Value: 0.17},
				{StartTs: 1752480900, Start: "2026-07-14T08:15:00Z", DurationS: 900, Value: 0.42},
			},
		}, {
			ProgramID: "prog-1",
			EventID:   "evt-2",
			Kind:      "GHG",
			Intervals: []OpenADRPriceInterval{{StartTs: 1752480000, DurationS: 0, Value: 120}},
		}},
		Ts: 1752480001,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"v":1`) {
		t.Fatalf("wire shape missing schema version: %s", b)
	}
	var out OpenADRPrices
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round trip: got %+v, want %+v", out, in)
	}
}

// TestOpenADRLimitsRoundTripAndOmission pins the limits wire shape plus the
// nil-axis omission (a released axis produces NO key, never a 0 — a 0 would
// read as "cap AT zero watts").
func TestOpenADRLimitsRoundTripAndOmission(t *testing.T) {
	imp := 5000.0
	in := OpenADRLimits{
		Envelope:   Envelope{V: OpenADRLimitsV},
		ImpLimW:    &imp,
		EventID:    "evt-7",
		ValidUntil: 1752483600,
		Ts:         1752480001,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "exp_lim_w") {
		t.Fatalf("absent exp_lim_w serialized: %s", b)
	}
	if !strings.Contains(string(b), `"v":1`) {
		t.Fatalf("wire shape missing schema version: %s", b)
	}
	var out OpenADRLimits
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round trip: got %+v, want %+v", out, in)
	}
}

// TestOpenADRStatusRoundTrip pins the status doc's wire shape.
func TestOpenADRStatusRoundTrip(t *testing.T) {
	in := OpenADRStatus{
		Envelope:     Envelope{V: OpenADRStatusV},
		VTNOK:        true,
		TokenOK:      true,
		LastPollTs:   1752480000,
		Programs:     2,
		ActiveEvents: 1,
		Ts:           1752480001,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "last_err") {
		t.Fatalf("empty last_err serialized: %s", b)
	}
	var out OpenADRStatus
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round trip: got %+v, want %+v", out, in)
	}
}

// TestOpenADRTopicPolicy mirrors qos_test.go for the three new retained
// state topics: QoS 1 (PubQoS's non-measurement default) and the per-family
// version constants via SupportedV.
func TestOpenADRTopicPolicy(t *testing.T) {
	cases := []struct {
		topic string
		wantV int
	}{
		{TopicOpenADRPrices, OpenADRPricesV},
		{TopicOpenADRLimits, OpenADRLimitsV},
		{TopicOpenADRStatus, OpenADRStatusV},
	}
	for _, tc := range cases {
		if got := PubQoS(tc.topic); got != QoS1 {
			t.Errorf("PubQoS(%q) = %d, want QoS1", tc.topic, got)
		}
		if got := SupportedV(tc.topic); got != tc.wantV {
			t.Errorf("SupportedV(%q) = %d, want %d", tc.topic, got, tc.wantV)
		}
	}
}

// TestOpenADRVersionGate mirrors the existing topics' CheckVersion
// discipline: v=1 accepted, v>supported rejected with *VersionError.
func TestOpenADRVersionGate(t *testing.T) {
	for _, topic := range []string{TopicOpenADRPrices, TopicOpenADRLimits, TopicOpenADRStatus} {
		ok := []byte(`{"v":1}`)
		if err := CheckVersion(topic, ok, SupportedV(topic)); err != nil {
			t.Fatalf("%s: v=1 rejected: %v", topic, err)
		}
		future := []byte(`{"v":2}`)
		err := CheckVersion(topic, future, SupportedV(topic))
		if err == nil {
			t.Fatalf("%s: v=2 accepted, want rejection", topic)
		}
		if _, isVE := err.(*VersionError); !isVE {
			t.Fatalf("%s: want *VersionError, got %T", topic, err)
		}
	}
}

// TestOpenADRFinite pins the GAP-09 defense-in-depth checks: a non-finite
// interval value or limit axis is rejected; nil axes pass.
func TestOpenADRFinite(t *testing.T) {
	nan := math.NaN()

	goodP := OpenADRPrices{Series: []OpenADRPriceSeries{{Intervals: []OpenADRPriceInterval{{Value: 0.2}}}}}
	if err := goodP.Finite(); err != nil {
		t.Fatalf("finite prices rejected: %v", err)
	}
	badP := OpenADRPrices{Series: []OpenADRPriceSeries{{Intervals: []OpenADRPriceInterval{{Value: math.Inf(1)}}}}}
	if err := badP.Finite(); err == nil {
		t.Error("non-finite price value accepted")
	}

	goodL := OpenADRLimits{}
	if err := goodL.Finite(); err != nil {
		t.Fatalf("nil-axes limits rejected: %v", err)
	}
	badImp := OpenADRLimits{ImpLimW: &nan}
	if err := badImp.Finite(); err == nil {
		t.Error("NaN imp_lim_w accepted")
	}
	badExp := OpenADRLimits{ExpLimW: &nan}
	if err := badExp.Finite(); err == nil {
		t.Error("NaN exp_lim_w accepted")
	}
}
