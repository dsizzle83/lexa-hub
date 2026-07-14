package bus

// TopicOCPPPairing carries installer pairing decisions (PairingDecision) for
// OCPP charging stations from lexa-api's POST /devices/evse/{id}/pairing to
// lexa-ocpp's pairing gate (WP-13, architecture D10/§2.3). An EDGE, never
// retained — a retained decision would replay as a false edge after a broker
// reconnect/restart (the same discipline as TopicCSIPComplianceAlert and
// TopicHubLogEvent; topics.go's edge-vs-state rule). Durability of the
// decision itself lives on the consumer side: lexa-ocpp persists the
// resulting allowlist at allowlist_path (crash-only, AD-011 — a restart
// re-seeds from the file, never from this topic). QoS 1 via PubQoS's
// non-measurement default; at-least-once redelivery is harmless because
// applying the same decision twice is idempotent.
const TopicOCPPPairing = "lexa/ocpp/pairing"

// PairingDecision action values.
const (
	PairingActionApprove = "approve"
	PairingActionDeny    = "deny"
)

// PairingDecision — lexa/ocpp/pairing (edge, QoS 1). One installer decision
// about one pending OCPP station: approve (allowlist it; its next
// BootNotification — nudged via TriggerMessage where the station is still
// connected — is answered Accepted) or deny (persisted; every subsequent
// BootNotification is answered Rejected). lexa-api is the only writer,
// lexa-ocpp the only reader (mosquitto-lexa.acl).
type PairingDecision struct {
	Envelope
	// StationID is the OCPP station identity the decision applies to (the
	// {id} segment of the ws:///ocpp/{id} URL both stacks key on).
	StationID string `json:"station_id"`
	// Action is PairingActionApprove or PairingActionDeny; anything else is
	// rejected (logged + dropped) by the consumer.
	Action string `json:"action"`
	// Actor attributes the decision ("local-api" today), mirroring
	// IntentMeta.Actor's role.
	Actor string `json:"actor,omitempty"`
	// Ts is the publisher's wall clock (Unix seconds).
	Ts int64 `json:"ts"`
}
