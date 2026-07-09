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
	"lexa-hub/internal/spool"
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Cloud session (unit 2.3). enabled:false ⇒ a stub that never dials the
	// WAN; enabled:true with an unreadable serial or bad cert/key/CA is FATAL
	// at startup (a misprovisioned unit fails loud, not silently offline).
	cloud, err := newCloudSession(cfg, m)
	if err != nil {
		log.Fatalf("lexa-cloudlink: cloud session: %v", err)
	}

	// spStats is the status seam; sp is the live spool. PRINCIPAL DECISION
	// (00_PROGRESS W3): when enabled:false the collectors do NOT run and the
	// spool is NOT opened — a local-only box must never consume flash for a
	// cloud that never comes. The spool + collectors + batcher exist only on
	// the enabled path (unit 2.2).
	var spStats spoolStats = stubSpoolStats{}
	var sp *spool.Spool
	lastUplink := func() int64 { return 0 }
	certDaysLeft := func() int { return 0 }

	if cfg.Enabled {
		sp, err = spool.Open(cfg.SpoolDir, cfg.SpoolMaxBytes, m.spoolMetrics())
		if err != nil {
			log.Fatalf("lexa-cloudlink: spool: %v", err)
		}
		defer sp.Close()
		spStats = sp

		// notify wakes the batcher on a P0 append so events drain within the
		// coalesce window rather than a whole telemetry interval.
		notify := make(chan struct{}, 1)
		up := newUplink(mc, sp, notify, m)
		up.subscribeAll()

		b, err := newBatcher(sp, cloud, cfg, m, notify)
		if err != nil {
			log.Fatalf("lexa-cloudlink: batcher: %v", err)
		}
		lastUplink = b.LastUplinkTs
		go b.run(ctx)

		// Downlink (unit 2.4/§2.6): cloud→intent validation chain, subscribed
		// on the CLOUD session. dlCloud's assertion always succeeds in
		// practice (*cloudMQTT and stubCloudSession both satisfy
		// cloudCmdSubscriber, downlink.go) — kept as a checked assertion
		// rather than a bare one so a future cloud.go change that drops the
		// interface fails loud at startup instead of panicking deep inside
		// runDownlink.
		dlCloud, ok := cloud.(cloudCmdSubscriber)
		if !ok {
			log.Fatalf("lexa-cloudlink: cloud session does not support downlink subscribe")
		}
		dl := newDownlink(mc, jw, m)
		go runDownlink(ctx, dlCloud, dl)

		// Cloud cert monitor (unit 2.5/§2.7): startup + daily inspection of
		// cloud_cert/cloud_ca, feeding CertDaysLeft into the status overlay
		// below. No MQTT publish of its own — CloudlinkStatus carries it.
		certMon := newCloudCertMon(cfg, m)
		go certMon.Run(ctx, cloudCertCheckInterval)
		certDaysLeft = certMon.CloudDaysLeft
	} else {
		// First-class local-only operation: everything else still runs (local
		// MQTT session, metrics, watchdog, retained status). Info, not Warn —
		// the safe shipped default, not a fault.
		log.Println("lexa-cloudlink: cloud link disabled by config — local-only operation (no spool, no collectors)")
	}

	// Publishes the retained CloudlinkStatus once now and every
	// cfg.HealthInterval() thereafter (spec item 4). lastUplink/certDaysLeft
	// are the batcher's/cert monitor's atomics when enabled, else constant
	// zero values.
	go statusPublisher(ctx, mc, cfg, cloud, spStats, lastUplink, certDaysLeft, m)

	watchdog.Ready()

	// TASK-008 pattern: no tight control loop of its own (the batcher rides one,
	// but on a slow cadence), so a 10s kick ticker is the liveness proxy — gated
	// on local MQTT connectivity AND, when enabled, spool health (§2.4: a wedged
	// or unwritable spool must not be reported healthy).
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
			spoolHealthy := true
			if cfg.Enabled {
				spoolHealthy = sp.Healthy()
			}
			if shouldKick(mc.IsConnected(), healthy() && spoolOK(cfg.Enabled, spoolHealthy)) {
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

// spoolOK folds the spool into the kick health gate (§2.4): a local-only box
// (enabled=false, no spool) is always spool-OK; an enabled box is spool-OK only
// while the spool reports Healthy() (dir writable + byte accounting consistent).
// Pure function of its two inputs so it is table-testable without a real spool,
// and kept SEPARATE from healthy() so the 2.1 healthy()==true baseline still
// holds (the combined gate is healthy() && spoolOK(...) at the call site).
func spoolOK(enabled, spoolHealthy bool) bool {
	return !enabled || spoolHealthy
}
