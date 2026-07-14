package bus

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestLogEventMsgRoundTrip pins the wire shape: the embedded Envelope's "v"
// key is actually emitted (no field-collision shadowing — the Measurement
// landmine documented in messages.go), and a marshal/unmarshal round trip is
// lossless.
func TestLogEventMsgRoundTrip(t *testing.T) {
	in := LogEventMsg{
		Envelope:     Envelope{V: LogEventV},
		Device:       "inverter-0",
		FunctionSet:  LogEventFunctionSetDER,
		LogEventCode: LogEventDEROverVoltage,
		Alarm:        true,
		LogEventID:   7,
		CreatedTs:    1750000000,
		DedupeKey:    "inverter-0/c2@1750000000#7",
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"v":1`) {
		t.Fatalf("wire shape missing schema version: %s", b)
	}
	var out LogEventMsg
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Fatalf("round trip: got %+v, want %+v", out, in)
	}
}

// TestLogEventTopicPolicy mirrors qos_test.go for the new edge topic: QoS 1
// (PubQoS's non-measurement default) and the LogEventV version constant.
func TestLogEventTopicPolicy(t *testing.T) {
	if got := PubQoS(TopicHubLogEvent); got != QoS1 {
		t.Errorf("PubQoS(%q) = %d, want QoS1", TopicHubLogEvent, got)
	}
	if got := SupportedV(TopicHubLogEvent); got != LogEventV {
		t.Errorf("SupportedV(%q) = %d, want %d", TopicHubLogEvent, got, LogEventV)
	}
}

// TestLogEventVersionGate mirrors the existing edge topics' CheckVersion
// discipline: v=1 accepted, v>supported rejected.
func TestLogEventVersionGate(t *testing.T) {
	ok := []byte(`{"v":1,"device":"d","function_set":11,"log_event_code":6}`)
	if err := CheckVersion(TopicHubLogEvent, ok, SupportedV(TopicHubLogEvent)); err != nil {
		t.Fatalf("v=1 rejected: %v", err)
	}
	future := []byte(`{"v":2,"device":"d"}`)
	err := CheckVersion(TopicHubLogEvent, future, SupportedV(TopicHubLogEvent))
	if err == nil {
		t.Fatal("v=2 accepted, want rejection")
	}
	if _, isVE := err.(*VersionError); !isVE {
		t.Fatalf("want *VersionError, got %T", err)
	}
}

// TestLogEventRTNPairing pins the Table 14 even/odd pairing over every alarm
// code, and LogEventCodeValid's bounds (0–21 in, 22 out).
func TestLogEventRTNPairing(t *testing.T) {
	alarms := []uint8{
		LogEventDEROverCurrent, LogEventDEROverVoltage, LogEventDERUnderVoltage,
		LogEventDEROverFrequency, LogEventDERUnderFrequency, LogEventDERVoltageImbalance,
		LogEventDERCurrentImbalance, LogEventDEREmergencyLocal, LogEventDEREmergencyRemote,
		LogEventDERLowPowerInput, LogEventDERPhaseRotation,
	}
	for _, a := range alarms {
		if a%2 != 0 {
			t.Errorf("alarm code %d is not even", a)
		}
		rtn := LogEventRTN(a)
		if rtn != a+1 {
			t.Errorf("LogEventRTN(%d) = %d, want %d", a, rtn, a+1)
		}
		if !LogEventCodeValid(a) || !LogEventCodeValid(rtn) {
			t.Errorf("codes %d/%d flagged invalid", a, rtn)
		}
	}
	if LogEventCodeValid(22) {
		t.Error("code 22 accepted, Table 14 tops out at 21")
	}
	// An RTN passed to LogEventRTN comes back unchanged (documented misuse guard).
	if got := LogEventRTN(3); got != 3 {
		t.Errorf("LogEventRTN(3) = %d, want 3", got)
	}
}
