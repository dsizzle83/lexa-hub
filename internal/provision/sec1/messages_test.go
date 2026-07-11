package sec1

import (
	"bytes"
	"testing"
)

func TestParseQR(t *testing.T) {
	qr, ok := ParseQR("LEXA:v1;serial=LX93-000042;pop=8F3K2M9Q")
	if !ok {
		t.Fatal("valid QR should parse")
	}
	if qr.Serial != "LX93-000042" || qr.Pop != "8F3K2M9Q" {
		t.Fatalf("qr: %#v", qr)
	}
	// Leading/trailing whitespace is trimmed.
	if _, ok := ParseQR("  LEXA:v1;serial=s;pop=p\n"); !ok {
		t.Fatal("whitespace-padded QR should parse")
	}
}

func TestParseQR_Rejects(t *testing.T) {
	for _, bad := range []string{
		"OTHR:v1;serial=x;pop=y",     // wrong prefix
		"LEXA:v1;serial=LX93-000042", // missing pop
		"LEXA:v1;serial=;pop=y",      // empty serial
		"LEXA:v1;noequals",           // malformed pair
	} {
		if _, ok := ParseQR(bad); ok {
			t.Fatalf("should reject %q", bad)
		}
	}
}

// roundTrip encodes a message, decodes it back, and returns the decoded value.
func roundTrip(t *testing.T, m Message) Message {
	t.Helper()
	b, err := m.Encode()
	if err != nil {
		t.Fatalf("encode %T: %v", m, err)
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatalf("decode %T: %v", m, err)
	}
	return got
}

func TestMessages_WireShapes(t *testing.T) {
	cases := []struct {
		msg  Message
		wire string
	}{
		{&Ok{}, `{"op":"ok"}`},
		{&Done{}, `{"op":"done"}`},
		{&ScanRequest{}, `{"op":"scan"}`},
		{&Err{Code: "pop_mismatch"}, `{"op":"err","code":"pop_mismatch"}`},
		{&HelloApp{Pub: []byte{0, 1, 2}}, `{"op":"hello","pub":"AAEC"}`},
		{&HelloHub{Pub: []byte{0, 1, 2}, Challenge: []byte{9, 9}}, `{"op":"hello","pub":"AAEC","challenge":"CQk="}`},
		{&Confirm{Challenge: []byte{9, 9}}, `{"op":"confirm","challenge":"CQk="}`},
		{&Join{SSID: "HomeNet"}, `{"op":"join","ssid":"HomeNet"}`},
		{&StateMessage{State: StateJoining}, `{"op":"state","state":"joining"}`},
		{&StateMessage{State: StateFailed, Reason: ptr(ReasonAuthFailed)}, `{"op":"state","state":"failed","reason":"auth_failed"}`},
		{
			&StateMessage{State: StateJoined, Handoff: &HandoffInfo{
				Serial: "LX93-000042", IP: "192.168.1.42", Port: 9100,
				APICertFP: "abcd", Token: "tok",
			}},
			`{"op":"state","state":"joined","serial":"LX93-000042","ip":"192.168.1.42","port":9100,"api_cert_fp":"abcd","token":"tok"}`,
		},
		{&WifiScanResult{APs: nil}, `{"op":"scan_result","aps":[]}`},
		{
			&WifiScanResult{APs: []WifiAp{{SSID: "HomeNet", RSSI: -48, Sec: "wpa2"}}},
			`{"op":"scan_result","aps":[{"ssid":"HomeNet","rssi":-48,"sec":"wpa2"}]}`,
		},
	}
	for _, c := range cases {
		b, err := c.msg.Encode()
		if err != nil {
			t.Fatalf("encode %T: %v", c.msg, err)
		}
		if string(b) != c.wire {
			t.Fatalf("%T wire mismatch:\n got  %s\n want %s", c.msg, b, c.wire)
		}
	}
}

func TestMessages_JoinWithPSK(t *testing.T) {
	psk := "hunter22"
	b, err := (&Join{SSID: "HomeNet", PSK: &psk}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"op":"join","ssid":"HomeNet","psk":"hunter22"}` {
		t.Fatalf("join wire: %s", b)
	}
	got := roundTrip(t, &Join{SSID: "HomeNet", PSK: &psk}).(*Join)
	if got.PSK == nil || *got.PSK != "hunter22" {
		t.Fatalf("psk round trip: %#v", got)
	}
}

func TestMessages_HelloDistinguishedByChallenge(t *testing.T) {
	// HelloApp has no challenge; HelloHub does. Both are "op":"hello".
	if _, ok := roundTrip(t, &HelloApp{Pub: []byte{1, 2, 3}}).(*HelloApp); !ok {
		t.Fatal("HelloApp should decode as HelloApp")
	}
	if _, ok := roundTrip(t, &HelloHub{Pub: []byte{1}, Challenge: []byte{2}}).(*HelloHub); !ok {
		t.Fatal("HelloHub should decode as HelloHub")
	}
}

func TestMessages_DecodeDefaults(t *testing.T) {
	// Missing rssi/sec default to -127/"".
	res, err := Decode([]byte(`{"op":"scan_result","aps":[{"ssid":"X"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	ap := res.(*WifiScanResult).APs[0]
	if ap.SSID != "X" || ap.RSSI != -127 || ap.Sec != "" {
		t.Fatalf("ap defaults: %#v", ap)
	}

	// state:joined with a missing port defaults to 9100.
	st, err := Decode([]byte(`{"op":"state","state":"joined","serial":"s"}`))
	if err != nil {
		t.Fatal(err)
	}
	h := st.(*StateMessage).Handoff
	if h == nil || h.Port != 9100 || h.Serial != "s" {
		t.Fatalf("handoff defaults: %#v", h)
	}

	// err with no code defaults to "unknown".
	e, err := Decode([]byte(`{"op":"err"}`))
	if err != nil {
		t.Fatal(err)
	}
	if e.(*Err).Code != "unknown" {
		t.Fatalf("err code default: %q", e.(*Err).Code)
	}
}

func TestMessages_DecodeErrors(t *testing.T) {
	for _, bad := range []string{
		`{"op":"nope"}`,                  // unknown op
		`{"op":"state","state":"weird"}`, // unknown state
		`{"op":"hello"}`,                 // missing pub
		`{"op":"confirm"}`,               // missing challenge
		`[1,2,3]`,                        // not an object
		`not json`,                       // not json
	} {
		if _, err := Decode([]byte(bad)); err == nil {
			t.Fatalf("should reject %q", bad)
		}
	}
}

func TestMessages_UnknownReasonDefaultsInternal(t *testing.T) {
	st, err := Decode([]byte(`{"op":"state","state":"failed","reason":"weird"}`))
	if err != nil {
		t.Fatal(err)
	}
	r := st.(*StateMessage).Reason
	if r == nil || *r != ReasonInternal {
		t.Fatalf("unknown reason should map to internal, got %v", r)
	}
}

func TestMessages_ScanResultEmptyIsArray(t *testing.T) {
	b, _ := (&WifiScanResult{}).Encode()
	if !bytes.Contains(b, []byte(`"aps":[]`)) {
		t.Fatalf("empty scan result must emit an empty array: %s", b)
	}
}

func ptr[T any](v T) *T { return &v }
