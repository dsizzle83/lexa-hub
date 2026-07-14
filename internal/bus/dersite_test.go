package bus

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

// TestDERSiteReportRoundTrip pins the wire shape (mirrors logevent_test.go):
// the embedded Envelope's "v" key is actually emitted (no field-collision
// shadowing) and a marshal/unmarshal round trip is lossless, including the
// nested status/availability blocks and the optional pointers.
func TestDERSiteReportRoundTrip(t *testing.T) {
	soc := 63.4
	storage := uint8(1)
	wAvail := 12340.0
	availDur := uint32(7200)
	va := 15000.0
	in := DERSiteReport{
		Envelope:             Envelope{V: DERSiteReportV},
		DERType:              DERTypeStorage,
		ModesSupported:       0x01800007,
		RtgMaxW:              15000,
		RtgMaxChargeRateW:    5000,
		RtgMaxDischargeRateW: 5000,
		RtgMaxWh:             10000,
		RtgMaxVA:             &va,
		SetMaxW:              15000,
		SetMaxChargeRateW:    5000,
		SetMaxDischargeRateW: 5000,
		SetMaxWh:             10000,
		Status: DERSiteStatus{
			SocPct:           &soc,
			GenConnectStatus: 1,
			OperationalMode:  1,
			StorageMode:      &storage,
			AlarmBits:        DERAlarmOverFrequency | DERAlarmEmergencyRemote,
			ReadingTs:        1752480000,
		},
		Avail: &DERSiteAvailability{
			EstimatedWAvailW:      &wAvail,
			AvailabilityDurationS: &availDur,
		},
		ContentHash: "abc123def4567890",
		Ts:          1752480001,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"v":1`) {
		t.Fatalf("wire shape missing schema version: %s", b)
	}
	var out DERSiteReport
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round trip: got %+v, want %+v", out, in)
	}
}

// TestDERSiteReportOmitsAbsentOptionals pins G27's omission-over-fabrication
// on the wire: nil VA/Var ratings, SoC, storage mode, and availability block
// produce NO key at all, not a zero.
func TestDERSiteReportOmitsAbsentOptionals(t *testing.T) {
	in := DERSiteReport{Envelope: Envelope{V: DERSiteReportV}, RtgMaxW: 1000, SetMaxW: 1000}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"rtg_max_va", "rtg_max_var", "set_max_va", "set_max_var", "soc_pct", "storage_mode", `"avail"`} {
		if strings.Contains(string(b), key) {
			t.Errorf("absent optional %s serialized: %s", key, b)
		}
	}
}

// TestDERSiteTopicPolicy mirrors qos_test.go for the new retained state
// topic: QoS 1 (PubQoS's non-measurement default) and the version constant.
func TestDERSiteTopicPolicy(t *testing.T) {
	if got := PubQoS(TopicHubDERSite); got != QoS1 {
		t.Errorf("PubQoS(%q) = %d, want QoS1", TopicHubDERSite, got)
	}
	if got := SupportedV(TopicHubDERSite); got != DERSiteReportV {
		t.Errorf("SupportedV(%q) = %d, want %d", TopicHubDERSite, got, DERSiteReportV)
	}
}

// TestDERSiteVersionGate mirrors the existing topics' CheckVersion
// discipline: v=1 accepted, v>supported rejected with *VersionError.
func TestDERSiteVersionGate(t *testing.T) {
	ok := []byte(`{"v":1,"der_type":83,"rtg_max_w":15000}`)
	if err := CheckVersion(TopicHubDERSite, ok, SupportedV(TopicHubDERSite)); err != nil {
		t.Fatalf("v=1 rejected: %v", err)
	}
	future := []byte(`{"v":2,"der_type":83}`)
	err := CheckVersion(TopicHubDERSite, future, SupportedV(TopicHubDERSite))
	if err == nil {
		t.Fatal("v=2 accepted, want rejection")
	}
	if _, isVE := err.(*VersionError); !isVE {
		t.Fatalf("want *VersionError, got %T", err)
	}
}

// TestDERSiteReportFinite pins the GAP-09 defense-in-depth check over both
// the always-present and pointer-borne numeric fields.
func TestDERSiteReportFinite(t *testing.T) {
	good := DERSiteReport{RtgMaxW: 1000, SetMaxW: 1000}
	if err := good.Finite(); err != nil {
		t.Fatalf("finite report rejected: %v", err)
	}

	nan := math.NaN()
	cases := []struct {
		name string
		mut  func(*DERSiteReport)
	}{
		{"rtg_max_w", func(r *DERSiteReport) { r.RtgMaxW = math.Inf(1) }},
		{"set_max_charge_rate_w", func(r *DERSiteReport) { r.SetMaxChargeRateW = nan }},
		{"rtg_max_va", func(r *DERSiteReport) { r.RtgMaxVA = &nan }},
		{"status.soc_pct", func(r *DERSiteReport) { r.Status.SocPct = &nan }},
		{"avail.estimated_w_avail", func(r *DERSiteReport) {
			r.Avail = &DERSiteAvailability{EstimatedWAvailW: &nan}
		}},
	}
	for _, tc := range cases {
		r := good
		tc.mut(&r)
		if err := r.Finite(); err == nil {
			t.Errorf("%s: non-finite value accepted", tc.name)
		}
	}
}

// TestDERAlarmBitForCode pins the Table 14 code → DERStatus alarmStatus
// category-bit mapping (bit = code/2, RTN maps to the same category) and
// the out-of-vocabulary rejection.
func TestDERAlarmBitForCode(t *testing.T) {
	cases := []struct {
		code uint8
		want uint32
	}{
		{LogEventDEROverCurrent, DERAlarmOverCurrent},
		{LogEventDEROverVoltage, DERAlarmOverVoltage},
		{LogEventRTN(LogEventDEROverVoltage), DERAlarmOverVoltage}, // RTN → same category
		{LogEventDERUnderVoltage, DERAlarmUnderVoltage},
		{LogEventDEROverFrequency, DERAlarmOverFrequency},
		{LogEventDERUnderFrequency, DERAlarmUnderFrequency},
		{LogEventDERVoltageImbalance, DERAlarmVoltageImbalance},
		{LogEventDERCurrentImbalance, DERAlarmCurrentImbalance},
		{LogEventDEREmergencyLocal, DERAlarmEmergencyLocal},
		{LogEventDEREmergencyRemote, DERAlarmEmergencyRemote},
		{LogEventDERLowPowerInput, DERAlarmLowPowerInput},
		{LogEventDERPhaseRotation, DERAlarmPhaseRotation},
	}
	for _, tc := range cases {
		if got := DERAlarmBitForCode(tc.code); got != tc.want {
			t.Errorf("DERAlarmBitForCode(%d) = %#x, want %#x", tc.code, got, tc.want)
		}
	}
	if got := DERAlarmBitForCode(22); got != 0 {
		t.Errorf("DERAlarmBitForCode(22) = %#x, want 0 (outside Table 14)", got)
	}
}
