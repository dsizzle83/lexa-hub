package main

import "lexa-hub/internal/metrics"

// cloudlinkMetrics is the flat-name metrics inventory for lexa-cloudlink
// (docs/DEVICE_ROADMAP.md §2.9; internal/metrics has no label dimension, so
// every series here is its own flat counter/gauge, matching e.g. the cert
// monitor's lexa_cert_expiry_client_seconds/lexa_cert_expiry_ca_seconds
// pair rather than one labeled series).
//
// Every metric below is registered at startup regardless of whether
// anything increments it yet — "registered-but-zero is the house norm"
// (CLAUDE.md's Metrics section, citing lexa_hub_tick_overruns_total as the
// precedent) — so a Prometheus scrape sees the full, stable series list
// from this unit's first deploy, well before 2.2-2.4 wire their real
// sources.
type cloudlinkMetrics struct {
	connected  *metrics.Gauge // lexa_cloudlink_connected (0/1) — driven by THIS unit's statusPublisher
	spoolBytes *metrics.Gauge // lexa_cloudlink_spool_bytes — 0 until 2.2 wires a real spool

	uplinkFrames *metrics.Counter // lexa_cloudlink_uplink_frames_total — wired by 2.2's batcher
	uplinkFail   *metrics.Counter // lexa_cloudlink_uplink_fail_total — wired by 2.2's batcher

	intentsForwarded *metrics.Counter // lexa_cloudlink_intents_forwarded_total — wired by 2.4/2.6's downlink
	intentsRejected  *metrics.Counter // lexa_cloudlink_intents_rejected_total — wired by 2.4/2.6's downlink

	// mqttPubFail/mqttReconn are the standard local-MQTT-session pair every
	// service wires (see cmd/telemetry/main.go's mqttFailCtr/mqttReconnCtr)
	// — the LOCAL broker session, never the cloud one (2.3's cloud session
	// gets its own pair once it exists).
	mqttPubFail *metrics.Counter // lexa_mqtt_publish_failures_total
	mqttReconn  *metrics.Counter // lexa_mqtt_reconnects_total
}

// newCloudlinkMetrics registers every series in cloudlinkMetrics against reg
// and returns them bundled for main()/status.go to reference.
func newCloudlinkMetrics(reg *metrics.Registry) *cloudlinkMetrics {
	return &cloudlinkMetrics{
		connected:        reg.Gauge("lexa_cloudlink_connected"),
		spoolBytes:       reg.Gauge("lexa_cloudlink_spool_bytes"),
		uplinkFrames:     reg.Counter("lexa_cloudlink_uplink_frames_total"),
		uplinkFail:       reg.Counter("lexa_cloudlink_uplink_fail_total"),
		intentsForwarded: reg.Counter("lexa_cloudlink_intents_forwarded_total"),
		intentsRejected:  reg.Counter("lexa_cloudlink_intents_rejected_total"),
		mqttPubFail:      reg.Counter("lexa_mqtt_publish_failures_total"),
		mqttReconn:       reg.Counter("lexa_mqtt_reconnects_total"),
	}
}
