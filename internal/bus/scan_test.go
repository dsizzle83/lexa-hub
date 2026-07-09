package bus

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// TestScanAndPendingTypesStampV1AndRoundTrip covers every scan.go type: marshal
// a fully-populated value stamped with its version constant, assert exactly
// one "v" key on the wire, and round-trip it back.
func TestScanAndPendingTypesStampV1AndRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		msg  any
	}{
		{"ScanRequest", ScanRequest{
			Envelope: Envelope{V: ScanRequestV},
			ID:       "s1", TCPCidr: "192.168.1.0/24", TCPPort: 502,
			RTUDev: "/dev/ttyUSB0", Bauds: []int{9600, 19200}, UnitIDs: []uint8{1, 2, 3, 126},
			Ts: 1,
		}},
		{"ScanStatus", ScanStatus{
			Envelope: Envelope{V: ScanStatusV},
			ID:       "s1", Phase: "tcp", Probed: 10, Found: 2, Detail: "scanning", Ts: 1,
		}},
		{"ScanResult", ScanResult{
			Envelope: Envelope{V: ScanResultV},
			ID:       "s1",
			Devices: []ScanHit{
				{URL: "tcp://192.168.1.40:502", UnitID: 1, Manufacturer: "Acme", Model: "Inv3000",
					Serial: "SN1", FwVersion: "1.2.3", Class: "inverter", Models: []uint16{1, 101},
					NameplateW: float64ptr(5000)},
			},
			Ts: 1,
		}},
		{"PendingStations", PendingStations{
			Envelope: Envelope{V: PendingStationsV},
			Stations: []PendingStation{
				{StationID: "ev-1", Vendor: "AcmeEV", ModelName: "Fast50", FirstSeenTs: 1, RemoteAddr: "10.0.0.5:5000"},
			},
			Ts: 1,
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.msg)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if count := strings.Count(string(b), `"v":`); count != 1 {
				t.Errorf("%s: wire JSON has %d \"v\" keys, want exactly 1: %s", tc.name, count, b)
			}
			var round map[string]any
			if err := json.Unmarshal(b, &round); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if round["v"] != float64(1) {
				t.Errorf("%s: v = %v, want 1", tc.name, round["v"])
			}
		})
	}
}

// TestScanRequestRoundTrip verifies a fully-populated ScanRequest survives
// marshal → unmarshal unchanged, including its slice fields (Bauds, UnitIDs).
func TestScanRequestRoundTrip(t *testing.T) {
	want := ScanRequest{
		Envelope: Envelope{V: ScanRequestV},
		ID:       "scan-42",
		TCPCidr:  "10.0.0.0/24",
		TCPPort:  502,
		RTUDev:   "/dev/ttyUSB0",
		Bauds:    []int{9600, 19200},
		UnitIDs:  []uint8{1, 2, 3, 126},
		Ts:       1_700_000_000,
	}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ScanRequest
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ID != want.ID || got.TCPCidr != want.TCPCidr || got.TCPPort != want.TCPPort ||
		got.RTUDev != want.RTUDev || got.Ts != want.Ts {
		t.Errorf("round-trip scalar mismatch: got %+v want %+v", got, want)
	}
	if len(got.Bauds) != len(want.Bauds) || got.Bauds[0] != want.Bauds[0] || got.Bauds[1] != want.Bauds[1] {
		t.Errorf("round-trip Bauds mismatch: got %v want %v", got.Bauds, want.Bauds)
	}
	if len(got.UnitIDs) != len(want.UnitIDs) {
		t.Errorf("round-trip UnitIDs mismatch: got %v want %v", got.UnitIDs, want.UnitIDs)
	}
}

// TestScanRequestOptionalFieldsOmitted verifies the zero-value defaults
// (empty TCPCidr, 0 TCPPort, empty RTUDev, nil Bauds/UnitIDs) are all
// omitted from the wire — §1.2's documented "empty = default" convention for
// ScanRequest (local /24, port 502, skip RTU, default baud/unit-id sets).
func TestScanRequestOptionalFieldsOmitted(t *testing.T) {
	b, err := json.Marshal(ScanRequest{Envelope: Envelope{V: ScanRequestV}, ID: "s1", Ts: 1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"tcp_cidr", "tcp_port", "rtu_dev", "bauds", "unit_ids"} {
		if strings.Contains(string(b), key) {
			t.Errorf("default-valued field %q must be omitted, got %s", key, b)
		}
	}
}

// TestScanHitOptionalFieldsOmitted verifies ScanHit's identification fields
// (unknown for an unresponsive/unidentified device) are omitted when empty,
// while URL/UnitID/Class (always meaningful) always appear.
func TestScanHitOptionalFieldsOmitted(t *testing.T) {
	hit := ScanHit{URL: "tcp://10.0.0.9:502", UnitID: 5, Class: "unknown-modbus"}
	b, err := json.Marshal(hit)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"manufacturer", "model", "serial", "fw_version", "models", "nameplate_w"} {
		if strings.Contains(string(b), key) {
			t.Errorf("unset field %q must be omitted for an unidentified hit, got %s", key, b)
		}
	}
	for _, key := range []string{`"url"`, `"unit_id"`, `"class"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("always-present field %q missing, got %s", key, b)
		}
	}
}

// TestFiniteRejectsNonFiniteScanResult is the Finite() rejection table for
// ScanResult's ScanHit.NameplateW — the one numeric field in this file's
// types — mirroring nan_reject_test.go's shape.
func TestFiniteRejectsNonFiniteScanResult(t *testing.T) {
	nan := math.NaN()
	posInf := math.Inf(1)

	cases := []struct {
		name string
		msg  ScanResult
	}{
		{"NameplateW=NaN", ScanResult{Devices: []ScanHit{{URL: "tcp://x", NameplateW: &nan}}}},
		{"second device NameplateW=+Inf", ScanResult{Devices: []ScanHit{
			{URL: "tcp://a", NameplateW: float64ptr(5000)},
			{URL: "tcp://b", NameplateW: &posInf},
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.msg.Finite()
			if err == nil {
				t.Fatalf("Finite() = nil, want an error naming field %q", "nameplate_w")
			}
			if !strings.Contains(err.Error(), "nameplate_w") {
				t.Errorf("Finite() error %q does not name field %q", err.Error(), "nameplate_w")
			}
		})
	}
}

// TestFiniteAcceptsValidScanResult is the accept-side complement: nil
// NameplateW and genuinely finite values must never be rejected.
func TestFiniteAcceptsValidScanResult(t *testing.T) {
	if err := (ScanResult{}).Finite(); err != nil {
		t.Errorf("ScanResult{} (no devices).Finite() = %v, want nil", err)
	}
	if err := (ScanResult{Devices: []ScanHit{{URL: "tcp://x"}}}).Finite(); err != nil {
		t.Errorf("ScanResult with nil NameplateW.Finite() = %v, want nil", err)
	}
	if err := (ScanResult{Devices: []ScanHit{{URL: "tcp://x", NameplateW: float64ptr(7500)}}}).Finite(); err != nil {
		t.Errorf("ScanResult with valid NameplateW.Finite() = %v, want nil", err)
	}
}

// TestSupportedVScanAndPendingTopics sweeps every new scan/ocpp-pending topic
// constant against its version constant.
func TestSupportedVScanAndPendingTopics(t *testing.T) {
	cases := []struct {
		name  string
		topic string
		want  int
	}{
		{"scan request", TopicScanRequest, ScanRequestV},
		{"scan status", TopicScanStatus, ScanStatusV},
		{"scan result", TopicScanResult, ScanResultV},
		{"ocpp pending", TopicOCPPPending, PendingStationsV},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SupportedV(tc.topic); got != tc.want {
				t.Errorf("SupportedV(%q) = %d, want %d", tc.topic, got, tc.want)
			}
		})
	}
}

// TestPubQoSScanAndPendingTopicsDefaultToQoS1 proves none of the new scan/
// pending topics match the QoS-0 measurement-plane rules, so all fall
// through to PubQoS's default (QoS 1) — no PubQoS change needed.
func TestPubQoSScanAndPendingTopicsDefaultToQoS1(t *testing.T) {
	for _, topic := range []string{TopicScanRequest, TopicScanStatus, TopicScanResult, TopicOCPPPending} {
		if got := PubQoS(topic); got != QoS1 {
			t.Errorf("PubQoS(%q) = %d, want %d (QoS1 default)", topic, got, QoS1)
		}
	}
}

// TestScanTopicConstantsExactStrings pins the literal wire topic strings
// (§1.1's topic map) — a typo here would silently create a new, unrelated
// topic instead of failing to compile.
func TestScanTopicConstantsExactStrings(t *testing.T) {
	cases := []struct{ got, want string }{
		{TopicScanRequest, "lexa/scan/request"},
		{TopicScanStatus, "lexa/scan/status"},
		{TopicScanResult, "lexa/scan/result"},
		{TopicOCPPPending, "lexa/ocpp/pending"},
		{TopicHubMode, "lexa/hub/mode"},
		{TopicCloudlinkStatus, "lexa/cloudlink/status"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("topic constant = %q, want %q", c.got, c.want)
		}
	}
}
