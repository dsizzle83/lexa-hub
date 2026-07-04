package bus

import "testing"

// TestPubQoS covers every topic constant/builder in this package against the
// documented policy (CLAUDE.md's MQTT topic map, review D5): the measurement
// plane (device measurements, battery metrics, EVSE state) is QoS 0;
// everything else — commands, CSIP control/pricing/billing/flow-reservation,
// compliance alert, resolved schedule, plan log — is QoS 1.
func TestPubQoS(t *testing.T) {
	cases := []struct {
		name  string
		topic string
		want  byte
	}{
		// Measurement plane — QoS 0.
		{"measurement", MeasurementTopic("inverter-0"), QoS0},
		{"batt metrics", BattMetricsTopic("bat0"), QoS0},
		{"evse state", EVSEStateTopic("ev0"), QoS0},

		// Control plane and CSIP — QoS 1.
		{"csip control", TopicCSIPControl, QoS1},
		{"csip compliance alert", TopicCSIPComplianceAlert, QoS1},
		{"csip pricing", TopicCSIPPricing, QoS1},
		{"csip billing", TopicCSIPBilling, QoS1},
		{"csip FR status", TopicCSIPFRStatus, QoS1},
		{"csip FR request", TopicCSIPFRRequest, QoS1},
		{"northbound schedule", TopicNorthboundSchedule, QoS1},
		{"hub plan", TopicHubPlan, QoS1},
		{"evse command", EVSECommandTopic("ev0"), QoS1},
		{"ctrl battery", CtrlBatteryTopic("bat0"), QoS1},
		{"ctrl solar", CtrlSolarTopic("solar0"), QoS1},

		// Wildcard subscribe topics: PubQoS is never called on these in
		// production (Subscribe hardcodes QoS 1, unaffected by this policy),
		// but they share the same prefixes/suffixes as their publish-side
		// counterparts, so the policy classifies them identically.
		{"sub measurements wildcard", SubMeasurements, QoS0},
		{"sub batt metrics wildcard", SubBattMetrics, QoS0},
		{"sub evse state wildcard", SubEVSEState, QoS0},
		{"sub evse command wildcard", SubEVSECommand, QoS1},
		{"sub ctrl battery wildcard", SubCtrlBattery, QoS1},
		{"sub ctrl solar wildcard", SubCtrlSolar, QoS1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PubQoS(tc.topic); got != tc.want {
				t.Errorf("PubQoS(%q) = %d, want %d", tc.topic, got, tc.want)
			}
		})
	}
}

// TestPubQoSDeviceNameCollisionSafe guards against a device/station name
// that happens to end the topic in "/metrics" or "/state" being misclassified
// as the measurement plane. PubQoS requires both the fixed prefix AND the
// fixed suffix to match (not suffix alone), so a control topic whose device
// name happens to be literally "metrics" or "state" must still land QoS 1.
func TestPubQoSDeviceNameCollisionSafe(t *testing.T) {
	// "lexa/control/battery/metrics" ends in "/metrics" but does not start
	// with "lexa/battery/" — must stay QoS 1.
	if topic, got := CtrlBatteryTopic("metrics"), PubQoS(CtrlBatteryTopic("metrics")); got != QoS1 {
		t.Errorf("PubQoS(%q) = %d, want %d (control topic must stay QoS 1 even when device name is \"metrics\")",
			topic, got, QoS1)
	}
	// "lexa/evse/state/command" starts with "lexa/evse/" but does not end in
	// "/state" — must stay QoS 1.
	if topic, got := EVSECommandTopic("state"), PubQoS(EVSECommandTopic("state")); got != QoS1 {
		t.Errorf("PubQoS(%q) = %d, want %d (command topic must stay QoS 1 even when station name is \"state\")",
			topic, got, QoS1)
	}
}
