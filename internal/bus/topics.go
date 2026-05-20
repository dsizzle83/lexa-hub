// Package bus defines MQTT topic names and JSON message types shared across
// all lexa-hub services.
//
// Topic layout:
//
//	lexa/measurements/{device}       modbus → hub, telemetry
//	lexa/battery/{device}/metrics    modbus → hub
//	lexa/csip/control                csip   → hub
//	lexa/evse/{station}/state        ocpp   → hub
//	lexa/control/battery/{device}    hub    → modbus
//	lexa/control/solar/{device}      hub    → modbus
//	lexa/evse/{station}/command      hub    → ocpp
package bus

import "fmt"

func MeasurementTopic(device string) string {
	return fmt.Sprintf("lexa/measurements/%s", device)
}

func BattMetricsTopic(device string) string {
	return fmt.Sprintf("lexa/battery/%s/metrics", device)
}

const TopicCSIPControl = "lexa/csip/control"

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
