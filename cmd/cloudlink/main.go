// lexa-cloudlink is the seventh lexa-hub service: the device's link to the
// cloud command and telemetry plane (docs/DEVICE_ROADMAP.md §2). Pure Go,
// CGO_ENABLED=0, standard crypto/tls (added in 2.3) — no wolfSSL anywhere in
// this service, unlike lexa-northbound/lexa-telemetry.
//
// Unit 2.1 (this file's scope) builds the chassis only: config, the house
// main() wiring, metrics, watchdog, journal, the local MQTT session, and a
// retained CloudlinkStatus publisher. NO cloud connection is made by this
// unit, regardless of cfg.Enabled — see status.go's cloudSession doc for
// why "always disconnected" is correct here even when Enabled is true. Unit
// 2.2 (internal/spool-backed uplink) and unit 2.3 (the actual cloud mTLS
// session) are what make this service do anything beyond idle and report
// its own local-only state; unit 2.4 adds the downlink (cloud → intent)
// path this unit's journal is opened ahead of time for.
//
// Usage:
//
//	lexa-cloudlink [-config /etc/lexa/cloudlink.json]
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/journal"
	"lexa-hub/internal/logutil"
	"lexa-hub/internal/metrics"
	"lexa-hub/internal/mqttutil"
	"lexa-hub/internal/watchdog"
)

func main() {
	cfgPath := flag.String("config", "/etc/lexa/cloudlink.json", "path to JSON config")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("lexa-cloudlink: load config: %v", err)
	}
	logutil.Setup("lexa-cloudlink", logutil.ParseLevel(cfg.LogLevel)) // TASK-045 pattern

	// Journal wired unconditionally, first thing after logging (READ FIRST
	// brief: "wire the journal NOW so the dir exists") — 2.4's downlink
	// intent audit trail depends on /var/lib/lexa/journal/cloudlink already
	// existing; nothing appends to it yet in this unit.
	jw, err := journal.Open(cfg.Journal.ToLibrary())
	if err != nil {
		log.Fatalf("lexa-cloudlink: journal: %v", err)
	}
	defer jw.Close()

	// TASK-044 pattern: registry + standard process gauges + the bus
	// decode-failure Collect hook, copied verbatim from
	// cmd/telemetry/main.go (see that file for why this exact shape — it
	// sums bus.VersionRejects()/bus.DecodeFailures() into one scrape-time
	// gauge rather than wiring each topic's counter by hand).
	reg := metrics.New()
	metrics.StandardGauges(reg)
	m := newCloudlinkMetrics(reg)
	reg.Collect(func(r *metrics.Registry) {
		var total uint64
		for _, n := range bus.VersionRejects() {
			total += n
		}
		for _, n := range bus.DecodeFailures() {
			total += n
		}
		r.Counter("lexa_bus_decode_failures_total").Set(total)
	})

	mqttPass, err := mqttutil.LoadPassword(cfg.MQTTPassFile)
	if err != nil {
		log.Fatalf("lexa-cloudlink: mqtt pass: %v", err)
	}
	mc, err := mqttutil.ConnectAuthInstrumented(cfg.MQTTBroker, cfg.MQTTClientID, cfg.MQTTUser, mqttPass, mqttutil.Instrumentation{
		OnPublishFail: m.mqttPubFail.Inc,
		OnReconnect:   m.mqttReconn.Inc,
	})
	if err != nil {
		log.Fatalf("lexa-cloudlink: mqtt: %v", err)
	}
	defer mc.Disconnect(500)

	metrics.Serve(cfg.MetricsAddr, reg)

	if !cfg.Enabled {
		// First-class local-only operation (spec item 3): everything above
		// still runs (local MQTT session, metrics, watchdog, retained
		// status) — only the cloud session stays a stub. This is an Info
		// line, not a Warn/Error: it is the safe shipped default, not a
		// fault.
		log.Println("lexa-cloudlink: cloud link disabled by config — local-only operation")
	}

	// session/spool are both stubs in this unit — see status.go's doc
	// comments on cloudSession/spoolStats for what 2.2/2.3 replace them
	// with. Neither one ever dials the WAN or touches disk beyond the
	// journal already opened above, regardless of cfg.Enabled.
	session := stubCloudSession{}
	spool := stubSpoolStats{}

	ctx, cancel := context.WithCancel(context.Background())

	// Publishes the retained CloudlinkStatus once now and every
	// cfg.HealthInterval() thereafter (spec item 4).
	go statusPublisher(ctx, mc, cfg, session, spool, m)

	watchdog.Ready()

	// TASK-008 pattern: like lexa-telemetry/lexa-ocpp, this unit has no
	// tight control loop to ride (no cloud session, no batcher yet), so a
	// 10s kick ticker is the liveness proxy, gated on shouldKick's two
	// inputs — see that function and healthy()'s doc comments for what
	// each currently checks and what 2.2 adds.
	kick := time.NewTicker(10 * time.Second)
	defer kick.Stop()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case <-quit:
			log.Println("lexa-cloudlink: shutting down")
			cancel()
			return
		case <-kick.C:
			if shouldKick(mc.IsConnected(), healthy()) {
				watchdog.Kick()
			}
		}
	}
}

// healthy reports every liveness signal the watchdog kick gates on BEYOND
// local MQTT connectivity (which the kick loop already checks separately
// via mc.IsConnected(), see shouldKick). Today there is nothing else to
// check — no spool, no cloud session — so this always returns true; unit
// 2.2 tightens it to also require the spool be writable/consistent
// (mirroring *spool.Spool's own Healthy() bool method,
// internal/spool/spool.go), matching lexa-api's probeHealthz gating pattern
// (cmd/api/main.go) of "a real, current signal, not just process
// liveness."
func healthy() bool {
	return true
}

// shouldKick reports whether the watchdog kick ticker should fire
// watchdog.Kick() this tick, given mqttConnected (mc.IsConnected()) and the
// current healthy() result. Extracted as a pure function of its two inputs
// — rather than inlined into the select's kick.C case — so the gating
// logic is table-testable without a real MQTT client (mirrors buildStatus's
// extraction rationale in status.go).
func shouldKick(mqttConnected, healthyNow bool) bool {
	return mqttConnected && healthyNow
}
