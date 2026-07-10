// lexa-northbound is the IEEE 2030.5 northbound client service.
//
// It maintains a wolfSSL mTLS connection to the utility server, walks the
// resource tree on a configurable interval, resolves the active DER control via
// the scheduler, builds a 24-hour DER control schedule, and publishes all
// results to MQTT (retained).
//
// The walk loop, publishers, response tracker, and flow-reservation manager
// live in internal/northbound/{run,publish,responses,flowres} (TASK-068,
// D12/R5) — this file is wiring only: config, TLS fetchers, MQTT connect,
// subscriptions, and signal handling. See internal/northbound/CLAUDE.md for
// topics, walk order, and the packages' own docs for the mechanisms wired
// here (fail-closed holdover, CannotComply dedupe, TASK-042 rewalk).
//
// Usage:
//
//	lexa-northbound [-config /etc/lexa/northbound.json]
package main

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/journal"
	"lexa-hub/internal/logutil"
	"lexa-hub/internal/metrics"
	"lexa-hub/internal/mqttutil"
	"lexa-hub/internal/northbound/flowres"
	"lexa-hub/internal/northbound/identity"
	"lexa-hub/internal/northbound/responses"
	"lexa-hub/internal/northbound/run"
	"lexa-hub/internal/northbound/scheduler"
	"lexa-hub/internal/tlsclient"
	"lexa-hub/internal/utilitytime"
	"lexa-hub/internal/watchdog"
	"lexa-hub/internal/wolfssl"
)

func main() {
	cfgPath := flag.String("config", "/etc/lexa/northbound.json", "path to JSON config")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("lexa-northbound: load config: %v", err)
	}
	logutil.Setup("lexa-northbound", logutil.ParseLevel(cfg.LogLevel)) // TASK-045

	// TASK-040: durable event journal; nil cfg.Journal ⇒ jw stays nil and
	// every emit site is jw != nil-guarded (true no-op rollout default).
	var jw *journal.Writer
	if cfg.Journal != nil {
		jw, err = journal.Open(cfg.Journal.ToLibrary())
		if err != nil {
			log.Fatalf("lexa-northbound: open journal: %v", err)
		}
		defer jw.Close()
		if ev, everr := journal.NewServiceStartEvent("northbound", journal.NewServiceStart("", configFingerprint(cfg))); everr == nil {
			_ = jw.Append(ev)
		}
	}

	// Unit 1.7 (DEVICE_ROADMAP.md §9, closing a gap found in unit 1.6): an
	// uncommissioned unit has no server to walk, so wolfSSL init, TLS
	// fetcher construction (which loads cert/key files that may not exist
	// yet on a factory-fresh/-reset unit), LFDI derivation from the client
	// cert, and the discovery/certmon/rotation goroutines all stay
	// unstarted — see runIdle's doc comment for exactly what still runs.
	if cfg.Uncommissioned() {
		runIdle(cfg)
		return
	}

	wolfssl.Init()
	defer wolfssl.Cleanup()

	tlsCfg := tlsclient.Config{
		ServerAddr:     cfg.Server,
		CACertPath:     cfg.CACert,
		ClientCertPath: cfg.ClientCert,
		ClientKeyPath:  cfg.ClientKey,
	}
	// Three independent wolfSSL sessions: discovery (long-lived keep-alive
	// walk), response (CORE-022 Response POSTs), flow-reservation (§10.9
	// POSTs). Never shared — each fetcher owns its own TLS state.
	fetcherDisc := mustFetcher(tlsCfg, "discovery")
	defer fetcherDisc.Free()
	fetcherResp := mustFetcher(tlsCfg, "response")
	defer fetcherResp.Free()
	fetcherFR := mustFetcher(tlsCfg, "flow reservation")
	defer fetcherFR.Free()

	lfdi := cfg.LFDI
	if lfdi == "" {
		lfdi, err = lfdiFromCert(cfg.ClientCert)
		if err != nil {
			log.Fatalf("lexa-northbound: derive LFDI: %v", err)
		}
	}
	log.Printf("lexa-northbound: LFDI=%s server=%s", lfdi, cfg.Server)

	// TASK-044: metrics registry + standard process gauges, wired before the
	// MQTT connect below so its instrumentation hooks have counters ready.
	reg := metrics.New()
	metrics.StandardGauges(reg)
	mqttFailCtr := reg.Counter("lexa_mqtt_publish_failures_total")
	mqttReconnCtr := reg.Counter("lexa_mqtt_reconnects_total")
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
	nbm := run.Metrics{
		WalkDuration:       reg.Gauge("lexa_nb_walk_duration_seconds"),
		WalkFailures:       reg.Counter("lexa_nb_walk_failures_total"),
		ClockOffset:        reg.Gauge("lexa_nb_clock_offset_seconds"),
		ImplausibleRejects: reg.Counter("lexa_nb_implausible_rejects_total"),
	}
	responsesPostedCtr := reg.Counter("lexa_nb_responses_posted_total")

	mqttPass, err := mqttutil.LoadPassword(cfg.MQTTPassFile)
	if err != nil {
		log.Fatalf("lexa-northbound: %v", err)
	}
	mc, err := mqttutil.ConnectAuthInstrumented(cfg.MQTTBroker, cfg.MQTTClientID, cfg.MQTTUser, mqttPass, mqttutil.Instrumentation{
		OnPublishFail: mqttFailCtr.Inc,
		OnReconnect:   mqttReconnCtr.Inc,
	})
	if err != nil {
		log.Fatalf("lexa-northbound: %v", err)
	}
	defer mc.Disconnect(500)

	metrics.Serve(cfg.MetricsAddr, reg)

	ctx, cancel := context.WithCancel(context.Background())

	sched := scheduler.New()
	// clk: single-owner accumulated utility-time offset (AD-004, TASK-035),
	// shared by run.Discovery's walk loop and responses.Tracker.
	clk := utilitytime.New(utilitytime.Config{})

	// WS-4.2: response-state persistence (TASK-041's northbound half).
	// Failure to load or open is a WARN, never fatal (AD-011) — this store
	// is restart-survivability for the dedupe maps, not a source of truth
	// the service can't run without; a fresh/missing/corrupt file starts
	// exactly as the pre-WS-4.2 RAM-only tracker always did: empty.
	var respStore *responses.Store
	var initialState responses.State
	if !cfg.ResponseStateDisabled() {
		var lerr error
		initialState, lerr = responses.LoadState(cfg.ResponseStatePath)
		if lerr != nil {
			if os.IsNotExist(lerr) {
				log.Printf("lexa-northbound: no persisted response state at %s; starting empty", cfg.ResponseStatePath)
			} else {
				log.Printf("lexa-northbound: WARN response-state load failed (%v); starting empty", lerr)
			}
			initialState = responses.State{}
		} else {
			log.Printf("lexa-northbound: restored response state from %s (posted=%d alerted=%d)",
				cfg.ResponseStatePath, len(initialState.Posted), len(initialState.Alerted))
		}
		var serr error
		respStore, serr = responses.OpenStore(cfg.ResponseStatePath)
		if serr != nil {
			log.Printf("lexa-northbound: WARN could not open response-state store (%v); persistence disabled for this run", serr)
			respStore = nil
		} else {
			defer respStore.Close()
		}
	}

	respTracker := responses.New(fetcherResp, lfdi, cfg.ResponseSetPath, clk, responsesPostedCtr, jw, respStore, initialState)
	frManager := flowres.New(fetcherFR, lfdi)
	discovery := run.New(mc, fetcherDisc, lfdi, sched, clk, respTracker, frManager, nbm, run.PollRateConfig{Mode: cfg.PollRateMode()})

	// FlowReservationRequest from the hub. Bypasses mqttutil.Subscribe (needs
	// the raw payload for HandleRequest, not a JSON-decoded T) so it carries
	// its own bus.CheckVersion/RejectAndAlarm gate (TASK-018).
	if token := mc.Subscribe(bus.TopicCSIPFRRequest, 1, func(_ mqtt.Client, msg mqtt.Message) {
		if verr := bus.CheckVersion(msg.Topic(), msg.Payload(), bus.SupportedV(msg.Topic())); verr != nil {
			if ve, ok := verr.(*bus.VersionError); ok {
				bus.RejectAndAlarm(ve)
			}
			return
		}
		frManager.HandleRequest(msg.Payload())
	}); token.Wait() && token.Error() != nil {
		log.Printf("lexa-northbound: subscribe flowreservation/request: %v", token.Error())
	}

	// Compliance-breach alerts from the hub → CannotComply Response
	// (alertCannotComply dedupes per breach episode; clear re-arms it).
	if err := mqttutil.Subscribe(mc, bus.TopicCSIPComplianceAlert, func(_ string, alert bus.ComplianceAlert) {
		if alert.Active {
			log.Printf("lexa-northbound: compliance breach %s limit=%.0fW measured=%.0fW (%s) → CannotComply mrid=%s episode=%s",
				alert.LimitType, alert.LimitW, alert.MeasuredW, alert.Reason, alert.MRID, alert.EpisodeID)
			respTracker.AlertCannotComply(alert.MRID, alert.EpisodeID)
		} else {
			respTracker.ClearAlerts()
		}
	}); err != nil {
		log.Printf("lexa-northbound: subscribe compliance alert: %v", err)
	}

	// TASK-042 rewalk request → immediate cache republish + out-of-cadence walk.
	if err := mqttutil.Subscribe(mc, bus.TopicCSIPRewalk, func(_ string, req bus.RewalkRequest) {
		discovery.HandleRewalk(req)
	}); err != nil {
		log.Printf("lexa-northbound: subscribe rewalk request: %v", err)
	}

	// sd_notify READY (TASK-008): subscriptions registered; only the walk
	// loop remains to start. A slow/unreachable utility server must not
	// itself cause a systemd start timeout — run.Discovery's fail-closed
	// discipline and watchdog kicks prove liveness from here on.
	watchdog.Ready()

	go discovery.Loop(ctx, cfg.DiscoveryInterval())

	// TASK-072/§10.5: cert-expiry monitor — its own owned goroutine, no
	// shared state with the discovery walk loop (05 §4). Run performs its
	// startup inspection immediately (before its first 24h tick), so the
	// very first WARN/ERROR alarm (if any) lands within moments of process
	// start rather than up to a day later.
	certMon := NewMonitor(mc, cfg.ClientCert, cfg.CACert, cfg.CertExpiryWarnDays, reg)
	go certMon.Run(ctx, certCheckInterval)

	// TASK-073/§10.5/§8.6/RSK-07: staged cert-rotation controller — its own
	// owned goroutine (05 §4), watching cfg.CertRotateSentinel for an
	// operator-staged rotation request (scripts/rotate-cert.sh) and, on
	// finding one, rotating all three fetchers off-tick via each one's own
	// probe-then-commit Reload (internal/tlsclient/fetcher.go). onCommit
	// forces an immediate certstatus re-check (TASK-072) so the new NotAfter
	// is visible without waiting for the next scheduled 24h check.
	rotator := NewRotationController(cfg.CertRotateSentinel, tlsCfg, lfdi, fetcherDisc, fetcherResp, fetcherFR,
		func() { certMon.CheckOnce() }, reg)
	go rotator.Run(ctx, cfg.CertRotatePollInterval())

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("lexa-northbound: shutting down")
	cancel()
}

// runIdle is the uncommissioned-idle path (Unit 1.7, DEVICE_ROADMAP.md §9):
// no server is configured, so there is nothing to walk and — critically —
// nothing that requires the cert/key files at cfg.CACert/ClientCert/
// ClientKey to exist yet. wolfSSL init, TLS fetcher construction, LFDI
// derivation from the client cert (lfdiFromCert), the cert-expiry monitor,
// and the cert-rotation controller all touch those files (directly or via
// wolfSSL) and are skipped entirely, not deferred — a factory-fresh or
// factory-reset unit (no certs on disk) must idle cleanly instead of
// crash-looping into systemd's StartLimit (V1RC FINDING A;
// configs/factory/README.md "Known gaps" #1 names this exact failure).
// Skipping the cert monitor/rotator entirely is acceptable here: neither
// has anything to report on (no certs configured to expire or rotate).
//
// Everything that does NOT depend on a server or certs still runs: the
// metrics registry + /metrics listener (standard gauges, MQTT fail/reconnect
// counters, the bus-decode-failure gauge — all keep working), MQTT connect
// (the broker credentials exist even on an uncommissioned unit; if the
// broker itself is unreachable, ordinary crash-only behavior applies,
// AD-011), and the watchdog: Ready() plus the same "10s ticker gated on
// mc.IsConnected()" idle-kick shape lexa-ocpp/lexa-api already use (see
// CLAUDE.md's watchdog table), so the process stays alive and
// systemd-healthy (healthcheck check #1) indefinitely until commissioning
// replaces this config and restarts the service.
func runIdle(cfg *Config) {
	slog.Info("uncommissioned idle — no server configured; commissioning will restart this service with a live config")

	// TASK-044: metrics registry + standard process gauges, wired before the
	// MQTT connect below so its instrumentation hooks have counters ready —
	// same ordering as the configured path above.
	reg := metrics.New()
	metrics.StandardGauges(reg)
	mqttFailCtr := reg.Counter("lexa_mqtt_publish_failures_total")
	mqttReconnCtr := reg.Counter("lexa_mqtt_reconnects_total")
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
		log.Fatalf("lexa-northbound: %v", err)
	}
	mc, err := mqttutil.ConnectAuthInstrumented(cfg.MQTTBroker, cfg.MQTTClientID, cfg.MQTTUser, mqttPass, mqttutil.Instrumentation{
		OnPublishFail: mqttFailCtr.Inc,
		OnReconnect:   mqttReconnCtr.Inc,
	})
	if err != nil {
		log.Fatalf("lexa-northbound: %v", err)
	}
	defer mc.Disconnect(500)

	metrics.Serve(cfg.MetricsAddr, reg)

	// sd_notify READY (TASK-008): there is no discovery walk to wait on —
	// idle is the terminal state until a config/restart from commissioning.
	watchdog.Ready()

	kick := time.NewTicker(10 * time.Second)
	defer kick.Stop()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	for {
		select {
		case <-quit:
			log.Println("lexa-northbound: shutting down (uncommissioned idle)")
			return
		case <-kick.C:
			if mc.IsConnected() {
				watchdog.Kick()
			}
		}
	}
}

// configFingerprint returns a short, deterministic hash of cfg's JSON
// encoding for the journal's service_start ConfigHash field (TASK-040); see
// cmd/hub/main.go's copy for the shared rationale (cmd/* packages don't
// import each other, 05 §1).
func configFingerprint(cfg *Config) string {
	b, err := json.Marshal(cfg)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

// mustFetcher constructs a wolfSSL fetcher or Fatals with a label identifying
// which of the three independent TLS sessions failed to init.
func mustFetcher(tlsCfg tlsclient.Config, label string) *tlsclient.WolfSSLFetcher {
	f, err := tlsclient.NewWolfSSLFetcher(tlsCfg)
	if err != nil {
		log.Fatalf("lexa-northbound: init TLS fetcher (%s): %v", label, err)
	}
	return f
}

func lfdiFromCert(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return "", fmt.Errorf("no PEM block found in %s", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}
	l, _ := identity.FromCertificate(cert)
	return l.String(), nil
}
