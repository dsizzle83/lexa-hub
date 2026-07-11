package sec1

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// Message is one op-discriminated provisioning message. A whole message is
// UTF-8 JSON (Encode), optionally sec1-encrypted, then framed. Mirrors the
// sealed ProvisionMessage hierarchy in messages.dart.
type Message interface {
	// Op is the "op" wire discriminator.
	Op() string
	// Encode returns the UTF-8 JSON bytes ready for encryption and/or framing.
	Encode() ([]byte, error)
}

// HelloApp is handshake step 1, app → hub (plaintext): the app's ephemeral
// public key.
type HelloApp struct {
	Pub []byte // raw 32-byte X25519 public key
}

func (*HelloApp) Op() string { return "hello" }
func (m *HelloApp) Encode() ([]byte, error) {
	return marshalJSON(struct {
		Op  string `json:"op"`
		Pub string `json:"pub"`
	}{"hello", b64(m.Pub)})
}

// HelloHub is handshake step 2, hub → app (plaintext): the hub public key plus
// an 8-byte random challenge the app must echo back encrypted.
type HelloHub struct {
	Pub       []byte
	Challenge []byte
}

func (*HelloHub) Op() string { return "hello" }
func (m *HelloHub) Encode() ([]byte, error) {
	return marshalJSON(struct {
		Op        string `json:"op"`
		Pub       string `json:"pub"`
		Challenge string `json:"challenge"`
	}{"hello", b64(m.Pub), b64(m.Challenge)})
}

// Confirm is handshake step 4, app → hub (encrypted): echoes the hub's
// challenge, proving the app derived K and therefore knows the PoP.
type Confirm struct {
	Challenge []byte
}

func (*Confirm) Op() string { return "confirm" }
func (m *Confirm) Encode() ([]byte, error) {
	return marshalJSON(struct {
		Op        string `json:"op"`
		Challenge string `json:"challenge"`
	}{"confirm", b64(m.Challenge)})
}

// Ok is handshake step 5, hub → app (encrypted): session established.
type Ok struct{}

func (*Ok) Op() string { return "ok" }
func (*Ok) Encode() ([]byte, error) {
	return marshalJSON(struct {
		Op string `json:"op"`
	}{"ok"})
}

// Err is a hub-reported error, e.g. {"op":"err","code":"pop_mismatch"}
// (plaintext when no session key exists).
type Err struct {
	Code string
}

func (*Err) Op() string { return "err" }
func (m *Err) Encode() ([]byte, error) {
	return marshalJSON(struct {
		Op   string `json:"op"`
		Code string `json:"code"`
	}{"err", m.Code})
}

// ScanRequest is app → hub on wifi (encrypted): request an AP scan.
type ScanRequest struct{}

func (*ScanRequest) Op() string { return "scan" }
func (*ScanRequest) Encode() ([]byte, error) {
	return marshalJSON(struct {
		Op string `json:"op"`
	}{"scan"})
}

// WifiAp is one access point in a scan_result.
type WifiAp struct {
	SSID string
	RSSI int
	Sec  string
}

type wifiApWire struct {
	SSID string `json:"ssid"`
	RSSI int    `json:"rssi"`
	Sec  string `json:"sec"`
}

// WifiScanResult is hub → app on wifi (encrypted): deduped scan results.
type WifiScanResult struct {
	APs []WifiAp
}

func (*WifiScanResult) Op() string { return "scan_result" }
func (m *WifiScanResult) Encode() ([]byte, error) {
	aps := make([]wifiApWire, 0, len(m.APs))
	for _, ap := range m.APs {
		aps = append(aps, wifiApWire{ap.SSID, ap.RSSI, ap.Sec})
	}
	return marshalJSON(struct {
		Op  string       `json:"op"`
		APs []wifiApWire `json:"aps"`
	}{"scan_result", aps})
}

// Join is app → hub on config (encrypted): join credentials. PSK is nil for
// open networks (the key is omitted on the wire).
type Join struct {
	SSID string
	PSK  *string
}

func (*Join) Op() string { return "join" }
func (m *Join) Encode() ([]byte, error) {
	if m.PSK == nil {
		return marshalJSON(struct {
			Op   string `json:"op"`
			SSID string `json:"ssid"`
		}{"join", m.SSID})
	}
	return marshalJSON(struct {
		Op   string `json:"op"`
		SSID string `json:"ssid"`
		PSK  string `json:"psk"`
	}{"join", m.SSID, *m.PSK})
}

// StateMessage is hub → app on status (encrypted): join progress. A joined
// state carries the HandoffInfo spread flat into the object; a failed state
// carries a WifiFailureReason. Mirrors StateMessage.toJson.
type StateMessage struct {
	State   ProvisioningState
	Reason  *WifiFailureReason // set (and emitted) when non-nil
	Handoff *HandoffInfo       // spread flat when non-nil
}

func (*StateMessage) Op() string { return "state" }
func (m *StateMessage) Encode() ([]byte, error) {
	// Manual, ordered assembly to reproduce {op, state, reason?, ...handoff?}
	// exactly — the handoff is spread flat, not nested.
	var b bytes.Buffer
	b.WriteString(`{"op":"state","state":`)
	writeJSONString(&b, m.State.Wire())
	if m.Reason != nil {
		b.WriteString(`,"reason":`)
		writeJSONString(&b, m.Reason.Wire())
	}
	if h := m.Handoff; h != nil {
		b.WriteString(`,"serial":`)
		writeJSONString(&b, h.Serial)
		b.WriteString(`,"ip":`)
		writeJSONString(&b, h.IP)
		fmt.Fprintf(&b, `,"port":%d`, h.Port)
		b.WriteString(`,"api_cert_fp":`)
		writeJSONString(&b, h.APICertFP)
		b.WriteString(`,"token":`)
		writeJSONString(&b, h.Token)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// Done is app → hub (encrypted): commissioning handoff complete; the hub stops
// advertising and closes the session.
type Done struct{}

func (*Done) Op() string { return "done" }
func (*Done) Encode() ([]byte, error) {
	return marshalJSON(struct {
		Op string `json:"op"`
	}{"done"})
}

// Decode parses one whole (already decrypted, reassembled) message. It mirrors
// ProvisionMessage.decode/fromJson: tolerant of missing optional fields but
// strict about the material the protocol cannot proceed without (op, keys,
// challenge). Both hello ops share "op":"hello"; the hub's is distinguished by
// the presence of a "challenge" key. Returns a pointer message.
func Decode(data []byte) (Message, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("provision: decode: %w", err)
	}
	op, _ := raw["op"].(string)
	switch op {
	case "hello":
		pub, err := requiredB64(raw["pub"], "pub")
		if err != nil {
			return nil, err
		}
		if _, hasChallenge := raw["challenge"]; hasChallenge {
			ch, err := requiredB64(raw["challenge"], "challenge")
			if err != nil {
				return nil, err
			}
			return &HelloHub{Pub: pub, Challenge: ch}, nil
		}
		return &HelloApp{Pub: pub}, nil
	case "confirm":
		ch, err := requiredB64(raw["challenge"], "challenge")
		if err != nil {
			return nil, err
		}
		return &Confirm{Challenge: ch}, nil
	case "ok":
		return &Ok{}, nil
	case "err":
		code, ok := raw["code"].(string)
		if !ok {
			code = "unknown"
		}
		return &Err{Code: code}, nil
	case "scan":
		return &ScanRequest{}, nil
	case "scan_result":
		return &WifiScanResult{APs: parseAPs(raw["aps"])}, nil
	case "join":
		ssid, _ := raw["ssid"].(string)
		var psk *string
		if v, ok := raw["psk"].(string); ok {
			psk = &v
		}
		return &Join{SSID: ssid, PSK: psk}, nil
	case "state":
		return decodeState(raw)
	case "done":
		return &Done{}, nil
	default:
		return nil, fmt.Errorf("provision: unknown op %q", op)
	}
}

func decodeState(raw map[string]any) (Message, error) {
	stateStr, _ := raw["state"].(string)
	st, ok := ParseProvisioningState(stateStr)
	if !ok {
		return nil, fmt.Errorf("provision: unknown state %q", stateStr)
	}
	msg := &StateMessage{State: st}
	if st == StateFailed {
		reasonStr, _ := raw["reason"].(string)
		r := ParseWifiFailureReason(reasonStr)
		msg.Reason = &r
	}
	if st == StateJoined {
		msg.Handoff = handoffFromMap(raw)
	}
	return msg, nil
}

func handoffFromMap(raw map[string]any) *HandoffInfo {
	return &HandoffInfo{
		Serial:    stringOr(raw["serial"], ""),
		IP:        stringOr(raw["ip"], ""),
		Port:      intOr(raw["port"], 9100),
		APICertFP: stringOr(raw["api_cert_fp"], ""),
		Token:     stringOr(raw["token"], ""),
	}
}

func parseAPs(v any) []WifiAp {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]WifiAp, 0, len(list))
	for _, e := range list {
		obj, ok := e.(map[string]any)
		if !ok {
			continue // objectList keeps only JSON objects
		}
		out = append(out, WifiAp{
			SSID: stringOr(obj["ssid"], ""),
			RSSI: intOr(obj["rssi"], -127),
			Sec:  stringOr(obj["sec"], ""),
		})
	}
	return out
}

// requiredB64 mirrors _requiredB64: rejects a missing/non-string/empty field,
// then standard-base64 decodes it.
func requiredB64(v any, field string) ([]byte, error) {
	s, ok := v.(string)
	if !ok || s == "" {
		return nil, fmt.Errorf("provision: missing or invalid %q", field)
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("provision: invalid base64 in %q: %w", field, err)
	}
	return b, nil
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func stringOr(v any, def string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return def
}

func intOr(v any, def int) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return def
}

// marshalJSON emits compact JSON without Go's default HTML escaping (so the
// bytes match Dart's jsonEncode for ASCII content) and without the trailing
// newline json.Encoder appends.
func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	return b[:len(b)-1], nil
}

func writeJSONString(b *bytes.Buffer, s string) {
	enc, _ := marshalJSON(s)
	b.Write(enc)
}
