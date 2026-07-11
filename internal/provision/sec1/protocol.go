package sec1

import "strings"

// GATT UUIDs for the provisioning service (ADR-0002). "4c455841" = ASCII
// "LEXA". Mirrors ProvisionUuids in protocol.dart.
const (
	UUIDService = "4c455841-0001-4870-b2d7-c36a53c1c9a1"
	UUIDInfo    = "4c455841-0002-4870-b2d7-c36a53c1c9a1"
	UUIDSession = "4c455841-0003-4870-b2d7-c36a53c1c9a1"
	UUIDWifi    = "4c455841-0004-4870-b2d7-c36a53c1c9a1"
	UUIDConfig  = "4c455841-0005-4870-b2d7-c36a53c1c9a1"
	UUIDStatus  = "4c455841-0006-4870-b2d7-c36a53c1c9a1"
)

// ProvisioningState is the join state machine reported on the status
// characteristic. Wire names equal the constant's lowercase name.
type ProvisioningState int

const (
	StateIdle ProvisioningState = iota
	StateJoining
	StateJoined
	StateFailed
)

// Wire returns the on-the-wire state string.
func (s ProvisioningState) Wire() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateJoining:
		return "joining"
	case StateJoined:
		return "joined"
	case StateFailed:
		return "failed"
	default:
		return ""
	}
}

// ParseProvisioningState parses a wire state string; ok is false for an
// unknown value (mirrors ProvisioningState.tryParse returning null).
func ParseProvisioningState(s string) (ProvisioningState, bool) {
	switch s {
	case "idle":
		return StateIdle, true
	case "joining":
		return StateJoining, true
	case "joined":
		return StateJoined, true
	case "failed":
		return StateFailed, true
	default:
		return 0, false
	}
}

// WifiFailureReason is the reason the hub reports for a failed join.
type WifiFailureReason int

const (
	ReasonNotFound WifiFailureReason = iota
	ReasonAuthFailed
	ReasonDHCPTimeout
	ReasonTimeout
	ReasonInternal
)

// AllWifiFailureReasons is every reason, for exhaustive table tests (mirrors
// WifiFailureReason.values).
var AllWifiFailureReasons = []WifiFailureReason{
	ReasonNotFound, ReasonAuthFailed, ReasonDHCPTimeout, ReasonTimeout, ReasonInternal,
}

// Wire returns the on-the-wire reason string as it appears in
// {"op":"state","reason":…}.
func (r WifiFailureReason) Wire() string {
	switch r {
	case ReasonNotFound:
		return "not_found"
	case ReasonAuthFailed:
		return "auth_failed"
	case ReasonDHCPTimeout:
		return "dhcp_timeout"
	case ReasonTimeout:
		return "timeout"
	default:
		return "internal"
	}
}

// ParseWifiFailureReason parses a wire reason string. An unknown or empty
// value maps to ReasonInternal, mirroring WifiFailureReason.parse.
func ParseWifiFailureReason(s string) WifiFailureReason {
	switch s {
	case "not_found":
		return ReasonNotFound
	case "auth_failed":
		return ReasonAuthFailed
	case "dhcp_timeout":
		return ReasonDHCPTimeout
	case "timeout":
		return ReasonTimeout
	default:
		return ReasonInternal
	}
}

// QrPayload is the parsed device-label QR code:
// LEXA:v1;serial=<serial>;pop=<setup code>.
type QrPayload struct {
	Serial string
	Pop    string
}

const qrPrefix = "LEXA:v1;"

// ParseQR parses the label QR format. It returns ok=false for a wrong prefix,
// a malformed field pair, or a missing/empty serial or pop — mirroring
// QrPayload.parse.
func ParseQR(raw string) (QrPayload, bool) {
	text := strings.TrimSpace(raw)
	if !strings.HasPrefix(text, qrPrefix) {
		return QrPayload{}, false
	}
	fields := map[string]string{}
	for _, part := range strings.Split(text[len(qrPrefix):], ";") {
		if part == "" {
			continue
		}
		eq := strings.IndexByte(part, '=')
		if eq <= 0 {
			return QrPayload{}, false
		}
		fields[part[:eq]] = part[eq+1:]
	}
	serial := fields["serial"]
	pop := fields["pop"]
	if serial == "" || pop == "" {
		return QrPayload{}, false
	}
	return QrPayload{Serial: serial, Pop: pop}, true
}

// HandoffInfo is the payload delivered on state:joined — everything the app
// needs to build a hub profile and reach the HTTPS API. On the wire it is
// spread flat into the state message (not nested), matching HandoffInfo.toJson
// spread by StateMessage.toJson.
type HandoffInfo struct {
	Serial    string
	IP        string
	Port      int
	APICertFP string
	Token     string
}
