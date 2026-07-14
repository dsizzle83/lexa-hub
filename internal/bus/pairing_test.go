package bus

import (
	"encoding/json"
	"testing"
)

// TestPairingDecision_FamilyConstants pins the WP-13 family wiring: the
// per-schema constant, the SupportedV arm, and the QoS policy (QoS 1 —
// lexa/ocpp/pairing is control-plane, not measurement-plane).
func TestPairingDecision_FamilyConstants(t *testing.T) {
	if PairingDecisionV != 1 {
		t.Errorf("PairingDecisionV = %d, want 1 (born at 1)", PairingDecisionV)
	}
	if got := SupportedV(TopicOCPPPairing); got != PairingDecisionV {
		t.Errorf("SupportedV(%q) = %d, want PairingDecisionV (%d)", TopicOCPPPairing, got, PairingDecisionV)
	}
	if got := PubQoS(TopicOCPPPairing); got != QoS1 {
		t.Errorf("PubQoS(%q) = %d, want QoS1 — pairing decisions are commands, not measurements", TopicOCPPPairing, got)
	}
}

// TestPairingDecision_WireShape pins the JSON wire shape (envelope "v" plus
// snake_case fields) so neither end drifts silently.
func TestPairingDecision_WireShape(t *testing.T) {
	d := PairingDecision{
		Envelope:  Envelope{V: PairingDecisionV},
		StationID: "cs-002",
		Action:    PairingActionApprove,
		Actor:     "local-api",
		Ts:        1752000000,
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"v", "station_id", "action", "actor", "ts"} {
		if _, ok := m[key]; !ok {
			t.Errorf("wire shape missing %q: %s", key, b)
		}
	}
	var back PairingDecision
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("round-trip: %v", err)
	}
	if back != d {
		t.Errorf("round-trip mismatch: got %+v want %+v", back, d)
	}
}

// TestPairingDecision_VersionGate pins the AD-006 subscriber gate for the
// family: v=1 accepted, a future v=2 rejected until the constant is bumped,
// absent v accepted as legacy v0 while the transition switch holds.
func TestPairingDecision_VersionGate(t *testing.T) {
	sup := SupportedV(TopicOCPPPairing)

	if err := CheckVersion(TopicOCPPPairing, []byte(`{"v":1,"station_id":"cs-2","action":"approve"}`), sup); err != nil {
		t.Errorf("v=1 must be accepted: %v", err)
	}
	if err := CheckVersion(TopicOCPPPairing, []byte(`{"v":2,"station_id":"cs-2","action":"approve"}`), sup); err == nil {
		t.Error("v=2 must be rejected while PairingDecisionV is 1")
	}
	if !LegacyV0Accepted {
		t.Skip("LegacyV0Accepted flipped — absent-v case no longer applies")
	}
	if err := CheckVersion(TopicOCPPPairing, []byte(`{"station_id":"cs-2","action":"approve"}`), sup); err != nil {
		t.Errorf("absent v (legacy v0) must be accepted during the transition: %v", err)
	}
}
