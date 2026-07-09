package main

import (
	"lexa-hub/internal/metrics"
	"lexa-hub/internal/spool"
)

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
	connected  *metrics.Gauge   // lexa_cloudlink_connected (0/1) — driven by the cloud session + statusPublisher
	spoolBytes *metrics.Gauge   // lexa_cloudlink_spool_bytes — driven by the spool (via spoolMetrics) when enabled
	spoolDrops *metrics.Counter // lexa_cloudlink_spool_drops_total — driven by the spool's eviction/over-budget path (§2.9)
	batchBytes *metrics.Gauge   // lexa_cloudlink_batch_bytes — last compressed uplink frame size, set by the batcher (2.2/§6)

	uplinkFrames *metrics.Counter // lexa_cloudlink_uplink_frames_total — batcher increments on a PUBACK'd frame
	uplinkFail   *metrics.Counter // lexa_cloudlink_uplink_fail_total — batcher increments on a failed/timed-out/over-budget frame

	cloudReconn *metrics.Counter // lexa_cloudlink_cloud_reconnects_total — the CLOUD session's reconnects (distinct from the local mqttReconn)

	intentsForwarded *metrics.Counter // lexa_cloudlink_intents_forwarded_total — wired by 2.4/2.6's downlink
	intentsRejected  *metrics.Counter // lexa_cloudlink_intents_rejected_total — wired by 2.4/2.6's downlink
	intentPubFail    *metrics.Counter // lexa_cloudlink_intent_pub_fail_total — downlink's local-bus forward failed AFTER a command was already accepted; distinct from a rejection (mirrors uplinkFail/uplinkFrames' "accepted vs delivered" split)

	certExpirySeconds *metrics.Gauge // lexa_cloudlink_cert_expiry_seconds — driven by cloudCertMon (2.7)
	certExpiring      *metrics.Gauge // lexa_cloudlink_cert_expiring — driven by cloudCertMon (2.7)

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
		connected:         reg.Gauge("lexa_cloudlink_connected"),
		spoolBytes:        reg.Gauge("lexa_cloudlink_spool_bytes"),
		spoolDrops:        reg.Counter("lexa_cloudlink_spool_drops_total"),
		batchBytes:        reg.Gauge("lexa_cloudlink_batch_bytes"),
		uplinkFrames:      reg.Counter("lexa_cloudlink_uplink_frames_total"),
		uplinkFail:        reg.Counter("lexa_cloudlink_uplink_fail_total"),
		cloudReconn:       reg.Counter("lexa_cloudlink_cloud_reconnects_total"),
		intentsForwarded:  reg.Counter("lexa_cloudlink_intents_forwarded_total"),
		intentsRejected:   reg.Counter("lexa_cloudlink_intents_rejected_total"),
		intentPubFail:     reg.Counter("lexa_cloudlink_intent_pub_fail_total"),
		certExpirySeconds: reg.Gauge("lexa_cloudlink_cert_expiry_seconds"),
		certExpiring:      reg.Gauge("lexa_cloudlink_cert_expiring"),
		mqttPubFail:       reg.Counter("lexa_mqtt_publish_failures_total"),
		mqttReconn:        reg.Counter("lexa_mqtt_reconnects_total"),
	}
}

// spoolMetrics returns the spool.Metrics view backed by this service's
// registered series, so the spool drives lexa_cloudlink_spool_bytes and
// lexa_cloudlink_spool_drops_total directly on every mutation (Append/Commit/
// eviction) — no scrape-time Collect hook needed, the gauge is always current.
// The remaining spool.Metrics fields (Appends/Commits/DropBytes/Errors) are
// left nil: spool.Metrics is fully nil-safe (each field's methods no-op on a
// nil receiver), so an unwired field costs nothing and keeps the exported
// metric surface to the §2.9 list. Constructed only on the enabled path (the
// spool is not opened for a local-only box), so a disabled unit's spool series
// stay a clean registered-but-zero.
func (m *cloudlinkMetrics) spoolMetrics() *spool.Metrics {
	return &spool.Metrics{
		Bytes: m.spoolBytes,
		Drops: m.spoolDrops,
	}
}
