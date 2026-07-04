// Package bus defines MQTT topic names and JSON message types shared across
// all lexa-hub services.
//
// Topic layout:
//
//	lexa/measurements/{device}                modbus       → hub, telemetry
//	lexa/battery/{device}/metrics             modbus       → hub
//	lexa/csip/control                         northbound   → hub (retained)
//	lexa/csip/pricing                         northbound   → hub (retained)
//	lexa/csip/billing                         northbound   → hub (retained)
//	lexa/csip/flowreservation/status          northbound   → hub (retained)
//	lexa/csip/flowreservation/request         hub          → northbound (QoS 1)
//	lexa/northbound/schedule                  northbound   → hub (retained)
//	lexa/evse/{station}/state                 ocpp         → hub
//	lexa/control/battery/{device}             hub          → modbus
//	lexa/control/solar/{device}               hub          → modbus
//	lexa/evse/{station}/command               hub          → ocpp
//	lexa/hub/plan                             hub          → api (retained)
//
// Publish QoS is a per-topic policy owned here (PubQoS), not hardcoded by
// callers: the measurement plane (lexa/measurements/*, lexa/battery/*/metrics,
// lexa/evse/*/state) is QoS 0 — high-frequency, freshness-gated by
// subscribers, a dropped sample is the documented design, not a bug; every
// other topic (commands, CSIP control/pricing/billing/flow-reservation,
// compliance alert, schedule, plan log) is QoS 1 (bounded PUBACK wait,
// mqttutil.publishTimeout). See CLAUDE.md's MQTT topic map for the same table
// (D5 closure: doc and code now agree, enforced by bus.PubQoS).
//
// Every message type below is versioned (AD-006): it embeds Envelope by
// value, giving it an omitempty "v" field, and has a per-schema version
// constant in envelope.go (e.g. MeasurementV). A subscriber calls
// CheckVersion(topic, payload, supportedV) before unmarshalling; the decode
// policy is absent-v accepted as legacy v0 while LegacyV0Accepted is true (the
// transition default), 1..supported accepted, greater-than-supported or
// negative rejected via RejectAndAlarm (counted per topic, logged
// first-plus-every-Nth to stay inside the journald budget). Same-major
// unknown fields are simply ignored by json.Unmarshal, which is what keeps
// additive schema evolution cheap. For a retained control-plane topic, a
// rejected message means hold last-known-good (the scheduler's existing
// fail-closed discipline) rather than running on a zero-valued decode; a
// later task (TASK-042) adds an active re-request instead of waiting out the
// retained message's own expiry. This task (TASK-017) lands the type,
// constants, CheckVersion, and RejectAndAlarm only — nothing below is wired
// to call them yet; publishers stamping V and subscribers calling
// CheckVersion is TASK-018's rollout.
package bus

import (
	"fmt"
	"strings"
)

// QoS byte values, named for readability at PubQoS call sites.
const (
	QoS0 byte = 0
	QoS1 byte = 1
)

// PubQoS returns the publish QoS for topic per the documented policy: QoS 0
// for the measurement plane (device measurements, battery metrics, EVSE
// state), QoS 1 for everything else. Callers publish with
// mqttutil.PublishJSONQoS(client, topic, bus.PubQoS(topic), v) rather than
// hardcoding a QoS, so this function is the single place the policy lives
// (review D5: previously every publish hardcoded QoS 1).
//
// Subscribe QoS is untouched by this policy — Subscribe always requests QoS
// 1, and effective delivery QoS is min(publish, subscribe), so a QoS-0
// publish stays best-effort and a QoS-1 publish stays reliably delivered
// regardless of what a subscriber requests.
func PubQoS(topic string) byte {
	switch {
	case strings.HasPrefix(topic, "lexa/measurements/"):
		return QoS0
	case strings.HasPrefix(topic, "lexa/battery/") && strings.HasSuffix(topic, "/metrics"):
		return QoS0
	case strings.HasPrefix(topic, "lexa/evse/") && strings.HasSuffix(topic, "/state"):
		return QoS0
	default:
		return QoS1
	}
}

func MeasurementTopic(device string) string {
	return fmt.Sprintf("lexa/measurements/%s", device)
}

func BattMetricsTopic(device string) string {
	return fmt.Sprintf("lexa/battery/%s/metrics", device)
}

const TopicCSIPControl = "lexa/csip/control"

// TopicCSIPComplianceAlert is published by the hub (orchestrator) when the
// optimizer cannot meet an active CSIP control limit after exhausting every
// lever (e.g. an import cap with the battery at its SOC reserve). The
// northbound service consumes it and POSTs a 2030.5 CannotComply Response so
// the grid server is told the DER is resource-limited.
const TopicCSIPComplianceAlert = "lexa/csip/compliance/alert"

// Pricing, billing, and flow reservation topics (IEEE 2030.5 §10.5/10.7/10.9).
const (
	TopicCSIPPricing   = "lexa/csip/pricing"
	TopicCSIPBilling   = "lexa/csip/billing"
	TopicCSIPFRStatus  = "lexa/csip/flowreservation/status"
	TopicCSIPFRRequest = "lexa/csip/flowreservation/request"
)

// TopicNorthboundSchedule is published by lexa-northbound after each discovery
// walk. It carries the resolved 24-hour DER control schedule (retained, QoS 1).
const TopicNorthboundSchedule = "lexa/northbound/schedule"

// TopicHubPlan carries the optimizer's most recent plan trace (decision log +
// timestamp), published by lexa-hub on every engine pass — economic tick and
// safety tick alike — and retained so lexa-api serves the latest plan across
// its own restarts. This is pure observability (the /status last_plan field):
// before it existed, /status served a hardcoded empty decision list, so the QA
// harness's decision introspection silently never worked ("the hub's plan log
// was empty" appeared in every diagnosis that asked). The per-pass timestamp
// doubles as an engine heartbeat: a hub whose /status last_plan timestamp
// stops advancing has a wedged control loop (QA gaps doc, "wedge detection").
const TopicHubPlan = "lexa/hub/plan"

func EVSEStateTopic(stationID string) string {
	return fmt.Sprintf("lexa/evse/%s/state", stationID)
}

func EVSECommandTopic(stationID string) string {
	return fmt.Sprintf("lexa/evse/%s/command", stationID)
}

func CtrlBatteryTopic(device string) string {
	return fmt.Sprintf("lexa/control/battery/%s", device)
}

func CtrlSolarTopic(device string) string {
	return fmt.Sprintf("lexa/control/solar/%s", device)
}

// Wildcard subscription topics used by subscribers.
const (
	SubMeasurements = "lexa/measurements/+"
	SubBattMetrics  = "lexa/battery/+/metrics"
	SubEVSEState    = "lexa/evse/+/state"
	SubEVSECommand  = "lexa/evse/+/command"
	SubCtrlBattery  = "lexa/control/battery/+"
	SubCtrlSolar    = "lexa/control/solar/+"
)

// DeviceFromMeasurementTopic extracts the device name from a topic like
// "lexa/measurements/inverter-0".
func DeviceFromMeasurementTopic(topic string) string {
	return lastSegment(topic)
}

// DeviceFromBattMetricsTopic extracts the device name from
// "lexa/battery/{device}/metrics".
func DeviceFromBattMetricsTopic(topic string) string {
	// lexa/battery/<device>/metrics — 4 segments, device is index 2
	return nthSegment(topic, 2)
}

// StationFromEVSEStateTopic extracts the station ID from "lexa/evse/{id}/state".
func StationFromEVSEStateTopic(topic string) string {
	return nthSegment(topic, 2)
}

// StationFromEVSECommandTopic extracts the station ID from "lexa/evse/{id}/command".
func StationFromEVSECommandTopic(topic string) string {
	return nthSegment(topic, 2)
}

// DeviceFromCtrlBatteryTopic extracts the device name from
// "lexa/control/battery/{device}".
func DeviceFromCtrlBatteryTopic(topic string) string {
	return lastSegment(topic)
}

// DeviceFromCtrlSolarTopic extracts the device name from
// "lexa/control/solar/{device}".
func DeviceFromCtrlSolarTopic(topic string) string {
	return lastSegment(topic)
}

func lastSegment(topic string) string {
	for i := len(topic) - 1; i >= 0; i-- {
		if topic[i] == '/' {
			return topic[i+1:]
		}
	}
	return topic
}

func nthSegment(topic string, n int) string {
	seg := 0
	start := 0
	for i := 0; i <= len(topic); i++ {
		if i == len(topic) || topic[i] == '/' {
			if seg == n {
				return topic[start:i]
			}
			seg++
			start = i + 1
		}
	}
	return ""
}
