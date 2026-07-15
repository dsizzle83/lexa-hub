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
	"lexa-hub/internal/northbound/derreport"
	"lexa-hub/internal/northbound/discovery"
	"lexa-hub/internal/northbound/egress"
	"lexa-hub/internal/northbound/flowres"
	"lexa-hub/internal/northbound/identity"
	"lexa-hub/internal/northbound/logevent"
	"lexa-hub/internal/northbound/responses"
	"lexa-hub/internal/northbound/run"
	"lexa-hub/internal/northbound/scheduler"
	"lexa-hub/internal/tlsclient"
	"lexa-hub/internal/utilitytime"
	"lexa-hub/internal/watchdog"
	"lexa-hub/internal/wolfssl"
	model "lexa-proto/csipmodel"
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
		RedirectMax:    cfg.RedirectMaxValue(),
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
	// WP-6 (§8 of the standards-buildout architecture): confirmed LogEvent
	// POSTs vs retry-exhausted/undeliverable drops — two flat names rather
	// than one labeled counter because internal/metrics has no label
	// dimension (the certmon.go precedent).
	logeventsPostedCtr := reg.Counter("lexa_nb_logevents_posted_total")
	logeventsDroppedCtr := reg.Counter("lexa_nb_logevents_dropped_total")
	// WP-4: successful DER* PUTs vs failed/skipped ones — registered
	// unconditionally (stable scrape surface even with der_report=false;
	// the registered-but-zero convention, TASK-044).
	derreportPutsCtr := reg.Counter("lexa_nb_derreport_puts_total")
	derreportErrsCtr := reg.Counter("lexa_nb_derreport_errors_total")
	// WP-8: control content the carriage could not represent (unknown
	// opModes, unresolvable curve hrefs, ...) — scrape-time snapshot of the
	// walker's process-global recorder.
	reg.Collect(func(r *metrics.Registry) {
		r.Counter("lexa_nb_ignored_control_content_total").Set(discovery.IgnoredContentTotal())
	})
	// WP-3/D3 (bench round 2 gap: named in architecture.md §8, never wired):
	// 301/302 hops followed across all three wolfSSL fetchers — scrape-time
	// snapshot of tlsclient's process-global recorder, same shape as
	// IgnoredContentTotal just above (internal/tlsclient stays
	// metrics-decoupled; see RedirectsTotal's doc).
	reg.Collect(func(r *metrics.Registry) {
		r.Counter("lexa_nb_redirects_total").Set(tlsclient.RedirectsTotal())
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
	// WP-7 (D5): Table 27 codes by default; legacy_cannotcomply_code=true
	// restores the pre-WP-7 0xF0 wire behavior byte-for-byte (bench compat).
	respTracker.SetLegacyCannotComplyCode(cfg.LegacyCannotComplyCode)
	frManager := flowres.New(fetcherFR, lfdi)

	// WP-7 (D4): one shared egress gate for everything this process sends
	// to the utility server — the PIN verifier suspends it on mismatch, and
	// every server-egress poster (Responses, flow reservations, LogEvents;
	// WP-4 DER* PUTs when they land) checks it before transmitting.
	egressGate := &egress.Gate{}
	respTracker.SetEgressGate(egressGate)
	frManager.SetEgressGate(egressGate)

	// WP-6 LogEvent poster: rides the RESPONSE fetcher's TLS session — a
	// LogEvent is the same egress plane as a Response POST (rare, small,
	// MQTT-goroutine-driven), the fetcher serializes callers under its own
	// mutex, and the three-session isolation above exists to keep POST churn
	// off the long-lived DISCOVERY session, which this preserves; a fourth
	// wolfSSL session on the SOM would buy no isolation for real memory.
	logevManager := logevent.New(fetcherResp, clk, logeventsPostedCtr, logeventsDroppedCtr)
	discovery := run.New(mc, fetcherDisc, lfdi, sched, clk, respTracker, frManager, nbm, run.PollRateConfig{Mode: cfg.PollRateMode()})
	discovery.SetLogEventSink(logevManager)

	// WP-4 DER* PUT reporter: rides the RESPONSE fetcher's TLS session, for
	// the same egress-plane reasoning that put the WP-6 LogEvent poster
	// there — DER* PUTs are periodic reporting egress (rare, small, driven
	// from the walk cadence and MQTT-goroutine content changes), the same
	// plane as Response/LogEvent POSTs, and keeping them off the DISCOVERY
	// session preserves the three-session isolation's point: the long-lived
	// walk session never serves two masters (an MQTT-driven capability PUT
	// contending with an in-flight walk fetch under the discovery mutex).
	// Rotation safety is identical either way — TASK-073's Reload swaps
	// sessions under the same per-fetcher mutex Put takes. (Architecture D3
	// sketched the discovery session; WP-4 chooses consistency with WP-6's
	// landed egress-plane split instead — deviation documented here and in
	// the WP-4 report.)
	var derReporter *derreport.Manager
	if cfg.DERReportEnabled() {
		derReporter = derreport.New(fetcherResp, clk, derreportPutsCtr, derreportErrsCtr)
		derReporter.SetEgressGate(egressGate)
		discovery.SetDERReporter(derReporter)
	} else {
		slog.Info("lexa-northbound: der_report disabled — DER* PUT reporting off (G28–G30 duty not served)")
	}

	// WP-7 (D5): the scheduler's receipt-reject hook — a plausibility-gate
	// rejection (malformed/implausible control, never adopted) posts Table 27
	// status 253 (invalid/out-of-range), deduped per mRID by the tracker.
	// 252 (parameter not applicable) is the documented WP-9 seam — no
	// capability/modesSupported classification exists northbound yet.
	sched.RejectHook = func(mrid, _ string) {
		respTracker.ReceiptReject(mrid, model.ResponseRejectedInvalid)
	}

	// TASK-072/§10.5 cert-expiry monitor, constructed BEFORE the walk loop
	// starts so the WP-7 PIN verifier's onChange can safely force a
	// certstatus republish from the very first walk; its own goroutine
	// (certMon.Run) still starts below, after the loop.
	certMon := NewMonitor(mc, cfg.ClientCert, cfg.CACert, cfg.CertExpiryWarnDays, reg)

	// WP-7 (D4): registration-PIN verification. 0 (the shipped default) =
	// disabled with one startup WARN (WS-8 disabled-default pattern); the
	// gauge is registered either way so dashboards see a stable 0, and the
	// certstatus pin_ok provider is nil-verifier-safe (reports nil = check
	// disabled).
	pinGauge := reg.Gauge("lexa_nb_pin_mismatch")
	var pinVerifier *run.PinVerifier
	if cfg.RegistrationPIN == 0 {
		slog.Warn("lexa-northbound: registration_pin not configured — Registration PIN verification disabled (WP-7/D4, CORE-003/BASIC-001); set registration_pin to the utility-issued PIN to enable the fail-closed mismatch posture")
	} else {
		// The LogEvent poster's suspend flag mirrors the gate at every verdict
		// transition (the only moments the verifier flips the gate), so PIN
		// freeze/heal covers LogEvent egress too (D4 item 2).
		pinVerifier = run.NewPinVerifier(cfg.RegistrationPIN, egressGate, pinGauge, func() {
			logevManager.SetEgressSuspended(egressGate.Suspended())
			certMon.CheckOnce()
		})
		discovery.SetPinVerifier(pinVerifier)
	}
	certMon.SetPinOK(pinVerifier.PinOK)

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

	// WP-6: LogEvent edges from the hub → POST to the EndDevice's
	// LogEventListLink (retry-once-then-drop; dedupe on LogEventMsg.DedupeKey).
	if err := mqttutil.Subscribe(mc, bus.TopicHubLogEvent, logevManager.HandleLogEvent); err != nil {
		log.Printf("lexa-northbound: subscribe logevent: %v", err)
	}

	// WP-4: the hub's retained GFEMS dersite aggregate → DERCapability/
	// DERSettings PUT on content change (the walk drives the DERStatus/
	// DERAvailability cadence via SetDERReporter above).
	if derReporter != nil {
		if err := mqttutil.Subscribe(mc, bus.TopicHubDERSite, derReporter.HandleDERSite); err != nil {
			log.Printf("lexa-northbound: subscribe dersite: %v", err)
		}
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
	// start rather than up to a day later. (Constructed above, before the
	// walk loop — WP-7 wiring order.)
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
