package bus

// LogEvent bus contract (WP-6, standards-buildout A4 / BASIC-027, G31/G32):
// the hub-side alarm-edge detector (cmd/hub/logevent.go) publishes one
// LogEventMsg per CSIP Table 14 alarm occurrence — an alarm onset and its
// return-to-normal (RTN) are two separate messages — on TopicHubLogEvent;
// lexa-northbound's logevent poster (internal/northbound/logevent) converts
// each into a csipmodel.LogEvent and POSTs it to the EndDevice's
// LogEventListLink.
//
// Edge-vs-state discipline (architecture.md §2.3): this topic is an EDGE,
// never retained — a retained edge replays as a false edge after a restart
// (the same rule TopicCSIPComplianceAlert follows; see
// ReconcileReportTopic's doc in topics.go). QoS 1 (PubQoS's default for
// anything outside the measurement plane) so a live broker delivers each
// edge at-least-once; the poster dedupes redelivery on DedupeKey.

// CSIP Table 14 DER alarm LogEvent codes (functionSet 11, IEEE 2030.5
// §10.10.6). Every alarm code is EVEN and its paired return-to-normal (RTN)
// code is the alarm code + 1 (G32: alarms and RTNs are posted as pairs, each
// timestamped). Defined here — not in cmd/hub — because both the hub-side
// producer and the northbound poster need the vocabulary, and internal/bus
// is their shared leaf.
const (
	LogEventDEROverCurrent      uint8 = 0  // RTN 1
	LogEventDEROverVoltage      uint8 = 2  // RTN 3
	LogEventDERUnderVoltage     uint8 = 4  // RTN 5
	LogEventDEROverFrequency    uint8 = 6  // RTN 7
	LogEventDERUnderFrequency   uint8 = 8  // RTN 9
	LogEventDERVoltageImbalance uint8 = 10 // RTN 11
	LogEventDERCurrentImbalance uint8 = 12 // RTN 13
	LogEventDEREmergencyLocal   uint8 = 14 // RTN 15
	LogEventDEREmergencyRemote  uint8 = 16 // RTN 17
	LogEventDERLowPowerInput    uint8 = 18 // RTN 19
	LogEventDERPhaseRotation    uint8 = 20 // RTN 21
	logEventDERMaxCode          uint8 = 21 // highest Table 14 code (RTN of PHASE_ROTATION)
	// LogEventFunctionSetDER mirrors csipmodel.LogFunctionSetDER (Table 24:
	// 11 = DER function set) so hub-side code does not need a csipmodel
	// import just to stamp FunctionSet.
	LogEventFunctionSetDER uint8 = 11
)

// LogEventRTN returns the return-to-normal code paired with alarmCode (the
// Table 14 even/odd pairing: RTN = alarm + 1). Calling it with an odd (RTN)
// code is a programming error; it returns the input unchanged in that case
// so a buggy caller re-posts the RTN rather than inventing a new code.
func LogEventRTN(alarmCode uint8) uint8 {
	if alarmCode%2 != 0 {
		return alarmCode
	}
	return alarmCode + 1
}

// LogEventCodeValid reports whether code is inside the CSIP Table 14 DER
// vocabulary (0–21). The northbound poster refuses to POST anything outside
// it — the code space is the standard's, not ours to extend (2030.5 §11.2's
// no-enum-extension rule).
func LogEventCodeValid(code uint8) bool {
	return code <= logEventDERMaxCode
}

// LogEventMsg is one DER alarm occurrence (or its return-to-normal) crossing
// the bus from the hub's alarm-edge detector to the northbound poster.
type LogEventMsg struct {
	Envelope

	// Device is the bus device name the event was observed on (the
	// lexa/measurements/{device} segment), or the literal "site" for
	// site-level events with no single device (the breach-episode source).
	Device string `json:"device"`

	// FunctionSet is the 2030.5 function set the code belongs to — always
	// LogEventFunctionSetDER (11) from this repo's producers.
	FunctionSet uint8 `json:"function_set"`

	// LogEventCode is the exact CSIP Table 14 code to post: even = alarm
	// onset, odd = the paired return-to-normal (RTN = alarm code + 1).
	LogEventCode uint8 `json:"log_event_code"`

	// Alarm distinguishes onset (true, even code) from RTN (false, odd code)
	// redundantly with LogEventCode's parity — carried explicitly so log
	// lines and subscribers never have to do parity arithmetic to read a
	// message.
	Alarm bool `json:"alarm"`

	// LogEventID disambiguates events created in the same second (the 2030.5
	// LogEvent.logEventID field) — a per-producer monotonic counter, wrapping
	// at uint16.
	LogEventID uint16 `json:"log_event_id"`

	// CreatedTs is when the hub observed the transition, LOCAL Unix seconds.
	// The poster converts it to utility/server time with the discovery
	// walk's accumulated clock offset (utilitytime.Clock.Offset) before
	// stamping csipmodel.LogEvent.CreatedDateTime — the hub does not own the
	// server clock offset, northbound does (AD-004).
	CreatedTs int64 `json:"created_ts"`

	// DedupeKey names this occurrence uniquely so the poster can make QoS 1
	// at-least-once redelivery idempotent: device + code + LogEventID for
	// alarm-bit events, the breach EpisodeID (+ direction) for episode
	// events. Same role as ComplianceAlert.EpisodeID for the responses
	// tracker.
	DedupeKey string `json:"dedupe_key"`
}
