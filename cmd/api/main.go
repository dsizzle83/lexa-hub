// lexa-api exposes the lexa-hub MQTT bus state over HTTP, in the JSON shape
// expected by the legacy demo dashboard.
//
// It subscribes to the same topics as lexa-hub (measurements, battery metrics,
// CSIP control, EVSE state, northbound schedule) and aggregates them into a
// thread-safe snapshot served at:
//
//	GET  /status     — JSON system snapshot (devices, power, EVSE, CSIP control)
//	GET  /logs       — text/event-stream of MQTT events seen by the API
//	GET  /healthz    — liveness probe
//	GET  /metrics    — Prometheus text exposition (TASK-044)
//
// Usage:
//
//	lexa-api [-config /etc/lexa/api.json]
package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"lexa-hub/internal/buildinfo"
	"lexa-hub/internal/bus"
	"lexa-hub/internal/journal"
	"lexa-hub/internal/logutil"
	"lexa-hub/internal/metrics"
	"lexa-hub/internal/mqttutil"
	"lexa-hub/internal/watchdog"
)

func main() {
	cfgPath := flag.String("config", "/etc/lexa/api.json", "path to JSON config")
	showVersion := flag.Bool("version", false, "print the build version (internal/buildinfo.Version) and exit")
	flag.Parse()

	// Handled before config load: -version is a build-verification/ops
	// utility (GAP-5), not a service start — it must work even with no
	// /etc/lexa/api.json present (e.g. right after `make build-arm64
	// VERSION=...`, before the binary is ever installed).
	if *showVersion {
		fmt.Println(buildinfo.Version)
		return
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("lexa-api: load config: %v", err)
	}
	logutil.Setup("lexa-api", logutil.ParseLevel(cfg.LogLevel)) // TASK-045

	// TASK-090/DEVICE_ROADMAP.md §4.5: the config_write audit journal, wired
	// unconditionally right after logging (same ordering cmd/cloudlink/main.go
	// uses) — POST /config/{service} below depends on this being open before
	// the mux is even built, and cfg.Journal is never optional for this
	// service (see config.go's JournalConfig doc).
	jw, err := journal.Open(cfg.Journal.ToLibrary())
	if err != nil {
		log.Fatalf("lexa-api: journal: %v", err)
	}
	defer jw.Close()

	apiToken, err := cfg.LoadAPIToken()
	if err != nil {
		log.Fatalf("lexa-api: %v", err)
	}
	if apiToken != "" {
		log.Printf("lexa-api: bearer-token auth ENABLED on /status,/logs (api_token_file=%s)", cfg.APITokenFile)
	} else {
		log.Printf("lexa-api: bearer-token auth disabled (api_token_file unset) — /status,/logs open, staged-rollout default")
	}

	// TASK-044: metrics registry + standard process gauges (lexa_up,
	// goroutines, fds, RSS), wired before the MQTT connect below so the
	// connect's instrumentation hooks have counters to increment into.
	reg := metrics.New()
	metrics.StandardGauges(reg)
	mqttFailCtr := reg.Counter("lexa_mqtt_publish_failures_total")
	mqttReconnCtr := reg.Counter("lexa_mqtt_reconnects_total")
	// TASK-090/DEVICE_ROADMAP.md §4.5 point 6: one counter for a committed
	// config_write, one for every rejection (gate/schema/write/restart-setup
	// failure) — see configwrite.go's call sites.
	configWritesCtr := reg.Counter("lexa_api_config_writes_total")
	configWriteRejectsCtr := reg.Counter("lexa_api_config_write_rejects_total")
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

	// TASK-045: the plan heartbeat consumes the retained lexa/hub/plan
	// topic (bus.PlanLog) — see cmd/api/heartbeat.go's doc for why arrival
	// time, not the plan's own Ts, drives stall detection.
	planHB := newPlanHeartbeat(cfg.PlanStallAfter())
	planHB.stalledGauge = reg.Gauge("lexa_api_plan_heartbeat_stalled")
	planHB.ageGauge = reg.Gauge("lexa_api_plan_heartbeat_age_seconds")

	// WS-9.1: tracks continuous-disconnected time so the watchdog kick below
	// can tell "genuinely connected" from "IsConnected() still true only
	// because paho's AutoReconnect is retrying" — see deaf's doc comment.
	deaf := watchdog.NewDeafTracker()
	reg.Collect(func(r *metrics.Registry) {
		r.Gauge("lexa_mqtt_deaf_seconds").Set(deaf.DeafFor(time.Now()).Seconds())
	})

	mqttPass, err := mqttutil.LoadPassword(cfg.MQTTPassFile)
	if err != nil {
		log.Fatalf("lexa-api: %v", err)
	}
	mc, err := mqttutil.ConnectAuthInstrumented(cfg.MQTTBroker, cfg.MQTTClientID, cfg.MQTTUser, mqttPass, mqttutil.Instrumentation{
		OnPublishFail:    mqttFailCtr.Inc,
		OnReconnect:      func() { mqttReconnCtr.Inc(); deaf.OnReconnect() },
		OnConnectionLost: deaf.OnConnectionLost,
	})
	if err != nil {
		log.Fatalf("lexa-api: %v", err)
	}
	defer mc.Disconnect(500)

	lb := newLogBroadcaster(cfg.LogBufferSize)
	store := newStateStore(cfg.Devices, cfg.StaleAfter())

	if err := mqttutil.Subscribe(mc, bus.SubMeasurements, func(topic string, m bus.Measurement) {
		store.onMeasurement(topic, m)
		w := "·"
		if m.W != nil {
			w = fmt.Sprintf("%.0f W", *m.W)
		}
		lb.Emit(fmt.Sprintf("[meas] %s %s", m.Device, w))
	}); err != nil {
		log.Fatalf("lexa-api: subscribe measurements: %v", err)
	}

	if err := mqttutil.Subscribe(mc, bus.SubBattMetrics, func(topic string, m bus.BattMetrics) {
		store.onBattMetrics(topic, m)
		if m.SOC != nil {
			lb.Emit(fmt.Sprintf("[batt] %s SOC=%.1f%%", m.Device, *m.SOC))
		}
	}); err != nil {
		log.Fatalf("lexa-api: subscribe batt metrics: %v", err)
	}

	if err := mqttutil.Subscribe(mc, bus.TopicCSIPControl, func(topic string, c bus.ActiveControl) {
		store.onCSIPControl(topic, c)
		lb.Emit(fmt.Sprintf("[csip] control source=%s mrid=%s offset=%ds", c.Source, c.MRID, c.ClockOffset))
	}); err != nil {
		log.Fatalf("lexa-api: subscribe csip control: %v", err)
	}

	if err := mqttutil.Subscribe(mc, bus.SubEVSEState, func(topic string, e bus.EVSEState) {
		store.onEVSEState(topic, e)
		pw := 0.0
		if e.PowerW != nil {
			pw = *e.PowerW
		}
		lb.Emit(fmt.Sprintf("[evse] %s/%d status=%s power=%.0f W", e.StationID, e.ConnectorID, e.Status, pw))
	}); err != nil {
		log.Fatalf("lexa-api: subscribe evse state: %v", err)
	}

	if err := mqttutil.Subscribe(mc, bus.TopicNorthboundSchedule, func(topic string, s bus.DERScheduleMsg) {
		store.onSchedule(topic, s)
		lb.Emit(fmt.Sprintf("[sched] slots=%d offset=%ds", len(s.Slots), s.ClockOffset))
	}); err != nil {
		log.Fatalf("lexa-api: subscribe schedule: %v", err)
	}

	if err := mqttutil.Subscribe(mc, bus.TopicNorthboundCertStatus, func(topic string, c bus.CertStatus) {
		store.onCertStatus(topic, c)
		lb.Emit(fmt.Sprintf("[cert] days_left=%d client_err=%q ca_err=%q", c.DaysLeft, c.ClientErr, c.CAErr))
	}); err != nil {
		log.Fatalf("lexa-api: subscribe cert status: %v", err)
	}

	if err := mqttutil.Subscribe(mc, bus.TopicHubPlan, func(topic string, p bus.PlanLog) {
		store.onPlanLog(topic, p)
		// TASK-045: arrival stamping — time.Now() here, not p.Ts. See
		// cmd/api/heartbeat.go's planHeartbeat doc.
		planHB.onPlanLog(p.Ts, time.Now())
		// Emit only decision-bearing plans to /logs — the heartbeat cadence
		// (every engine tick) would drown the stream.
		for _, dec := range p.Decisions {
			lb.Emit(fmt.Sprintf("[plan] %s: %s → %s", dec.Rule, dec.Reason, dec.Impact))
		}
	}); err != nil {
		log.Fatalf("lexa-api: subscribe hub plan: %v", err)
	}

	// DEVICE_ROADMAP.md §3.5/§4.3: the hub's authoritative plan-author mode.
	if err := mqttutil.Subscribe(mc, bus.TopicHubMode, func(topic string, m bus.ModeStatus) {
		store.onModeStatus(topic, m)
		lb.Emit(fmt.Sprintf("[mode] %s (actor=%s)", m.Mode, m.Actor))
	}); err != nil {
		log.Fatalf("lexa-api: subscribe hub mode: %v", err)
	}

	// GAP-8: the hub's effective reserve + active tariff, folded into /status's
	// "reserve"/"tariff" so the app reads hub truth (retained — re-served to a
	// restarting lexa-api).
	if err := mqttutil.Subscribe(mc, bus.TopicHubSettings, func(topic string, h bus.HubSettings) {
		store.onHubSettings(topic, h)
		src := "-"
		if h.Reserve.EffectivePct != nil {
			src = fmt.Sprintf("%.0f%%", *h.Reserve.EffectivePct)
		}
		lb.Emit(fmt.Sprintf("[settings] reserve=%s src=%s tariff=%s", src, h.Reserve.Source, h.Tariff.Source))
	}); err != nil {
		log.Fatalf("lexa-api: subscribe hub settings: %v", err)
	}

	// GAP-7: the hub's 24-hour plan/forecast series, projected into GET /plan
	// (retained — re-served to a restarting lexa-api).
	if err := mqttutil.Subscribe(mc, bus.TopicHubSchedule, func(topic string, h bus.HubSchedule) {
		store.onHubSchedule(topic, h)
		lb.Emit(fmt.Sprintf("[schedule] slots=%d ev_series=%d gen_at=%d",
			len(h.SolarForecastW), len(h.EVPlanW), h.GeneratedAt))
	}); err != nil {
		log.Fatalf("lexa-api: subscribe hub schedule: %v", err)
	}

	// DEVICE_ROADMAP.md §2/§4.3: lexa-cloudlink's retained status. No such
	// service exists in this repo yet (TASK-085+) — this subscribe is inert
	// (never delivers) until it lands, exactly like every other forward-
	// looking subscribe in this file.
	if err := mqttutil.Subscribe(mc, bus.TopicCloudlinkStatus, func(topic string, c bus.CloudlinkStatus) {
		store.onCloudlinkStatus(topic, c)
		lb.Emit(fmt.Sprintf("[cloudlink] connected=%v endpoint=%s", c.Connected, c.Endpoint))
	}); err != nil {
		log.Fatalf("lexa-api: subscribe cloudlink status: %v", err)
	}

	// DEVICE_ROADMAP.md §5/§4.3: commissioning-scan progress + result.
	if err := mqttutil.Subscribe(mc, bus.TopicScanStatus, func(topic string, s bus.ScanStatus) {
		store.onScanStatus(topic, s)
		lb.Emit(fmt.Sprintf("[scan] %s phase=%s probed=%d found=%d", s.ID, s.Phase, s.Probed, s.Found))
	}); err != nil {
		log.Fatalf("lexa-api: subscribe scan status: %v", err)
	}

	if err := mqttutil.Subscribe(mc, bus.TopicScanResult, func(topic string, r bus.ScanResult) {
		store.onScanResult(topic, r)
		lb.Emit(fmt.Sprintf("[scan] result id=%s devices=%d", r.ID, len(r.Devices)))
	}); err != nil {
		log.Fatalf("lexa-api: subscribe scan result: %v", err)
	}

	// DEVICE_ROADMAP.md §6/§4.3: OCPP stations awaiting installer approval.
	if err := mqttutil.Subscribe(mc, bus.TopicOCPPPending, func(topic string, p bus.PendingStations) {
		store.onOCPPPending(topic, p)
		lb.Emit(fmt.Sprintf("[ocpp] pending stations=%d", len(p.Stations)))
	}); err != nil {
		log.Fatalf("lexa-api: subscribe ocpp pending: %v", err)
	}

	// DEVICE_ROADMAP.md §4.3: resWaiter subscribes lexa/intent/result exactly
	// once for the process lifetime — see resultwaiter.go's doc. (Named
	// resWaiter, not resultWaiter, to avoid shadowing the resultWaiter TYPE
	// in this same function's scope.)
	resWaiter, err := newResultWaiter(mc)
	if err != nil {
		log.Fatalf("lexa-api: subscribe intent result: %v", err)
	}

	// resolveSerial mirrors the same serial-file/hostname resolution the TLS
	// cert (below) and mDNS advertisement (further down) both use, so the
	// cert's CN, the mDNS TXT "serial=" field, and /site's "serial" field
	// always agree. Computed here (rather than only at the mDNS call site,
	// as before DEVICE_ROADMAP.md §4.3) so siteHandler can also close over it.
	serial := resolveSerial(cfg.SerialFile)

	// DEVICE_ROADMAP.md §4.1: the HTTPS server cert. Generated once on
	// first boot and persisted (cmd/api/tlscert.go); a load/generate
	// failure is FATAL here — a misprovisioned unit must not silently fall
	// back to serving plaintext. certFP is "" when TLS is disabled.
	var tlsConfig *tls.Config
	var certFP string
	if cfg.TLSEnabled() {
		cert, fp, err := ensureServerCertFor(cfg.CertDir, cfg.SerialFile)
		if err != nil {
			log.Fatalf("lexa-api: TLS cert (cert_dir=%s): %v", cfg.CertDir, err)
		}
		certFP = fp
		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
		// Installers/TOFU consumers (lexactl, the mobile app) read this
		// line to learn the fingerprint they should pin — one INFO line,
		// not a repeated log source.
		log.Printf("lexa-api: TLS enabled — server cert fingerprint sha256:%s (cert_dir=%s)", fp, cfg.CertDir)
	} else {
		log.Printf("lexa-api: TLS disabled (tls:false) — serving plain HTTP; product deploys must not ship this")
	}

	mux := http.NewServeMux()
	// /healthz is NEVER wrapped — TASK-008's api watchdog self-probe (and any
	// future load-balancer check) needs an unauthenticated liveness endpoint.
	mux.HandleFunc("/healthz", healthzHandler)
	mux.HandleFunc("/status", requireBearer(apiToken, statusHandler(store, planHB, certFP)))
	mux.HandleFunc("/logs", requireBearer(apiToken, logsHandler(lb)))
	// DEVICE_ROADMAP.md §4.3: read routes (staged-rollout requireBearer, same
	// semantics as /status and /logs above) plus the two write routes
	// (requireBearerStrict — an empty/unconfigured token fails CLOSED here,
	// never open; see auth.go's doc).
	mux.HandleFunc("/site", requireBearer(apiToken, siteHandler(serial, cfg.SiteCacheFile)))
	mux.HandleFunc("/devices", requireBearer(apiToken, devicesHandler(store)))
	mux.HandleFunc("/telemetry/recent", requireBearer(apiToken, telemetryRecentHandler(store.telemetry)))
	mux.HandleFunc("/mode", requireBearer(apiToken, modeHandler(store)))
	// GAP-7: the 24-hour plan/forecast series (staged-rollout requireBearer,
	// same semantics as /status and /mode). 503 until the first HubSchedule.
	mux.HandleFunc("/plan", requireBearer(apiToken, planHandler(store)))
	mux.HandleFunc("/intent", requireBearerStrict(apiToken, intentHandler(mc, resWaiter)))
	// /scan dispatches GET vs POST to their own auth wrapper internally
	// (scan.go's scanHandler) since both methods share this one path.
	mux.HandleFunc("/scan", scanHandler(mc, store, apiToken))
	// DEVICE_ROADMAP.md §4.5/TASK-090: the commissioning config-write path.
	// "/config/" (trailing slash) is a classic http.ServeMux SUBTREE pattern —
	// it matches "/config/hub", "/config/api-secret", etc.; configwrite.go's
	// handler parses the service name itself from r.URL.Path, same style as
	// this file's other handlers doing their own method dispatch rather than
	// relying on Go 1.22+ pattern-based routing. requireBearerStrict applies
	// here too: the bearer token is required even while uncommissioned (the
	// per-unit label secret, §4.2) — commissioned-gate enforcement happens
	// INSIDE the handler (configwrite.go), after auth, not instead of it.
	mux.HandleFunc("/config/", requireBearerStrict(apiToken,
		configWriteHandler(cfg.APITokenFile, jw, defaultRestartRunner, configWritesCtr, configWriteRejectsCtr)))
	// /metrics is NEVER wrapped either (TASK-044): same reasoning as
	// /healthz — a Prometheus scraper is infra, not a dashboard consumer of
	// this API's data, and AD-008's bearer-token rollout is scoped to
	// /status and /logs (see LoadAPIToken's doc and the startup log line
	// above). This mirrors lexa-api's :9100/metrics being an "existing
	// listener, new route" per the task: no new auth surface, no new port.
	mux.Handle("/metrics", reg.Handler())

	// withContractVersion stamps the X-Lexa-Contract-Version header
	// (apicontract.Version) on every response — see version.go. Wrapping the
	// whole mux (rather than each route) keeps the header on /healthz and
	// /metrics too, so the app can read the contract version off any route.
	srv := &http.Server{Addr: cfg.ListenAddr, Handler: withContractVersion(mux), TLSConfig: tlsConfig}
	scheme := "http"
	if cfg.TLSEnabled() {
		scheme = "https"
	}
	go func() {
		log.Printf("lexa-api: %s listening on %s", scheme, cfg.ListenAddr)
		lb.Emit(fmt.Sprintf("lexa-api: %s listening on %s", scheme, cfg.ListenAddr))
		var err error
		if cfg.TLSEnabled() {
			// Cert/key already loaded into srv.TLSConfig above — passing
			// empty paths here tells ListenAndServeTLS to use that config
			// rather than re-reading from disk.
			err = srv.ListenAndServeTLS("", "")
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("lexa-api: listen: %v", err)
		}
	}()

	// TASK-008: cfg.ListenAddr is host:port form — extract just the port so
	// the self-probe always targets 127.0.0.1 regardless of what host
	// ListenAddr itself binds (loopback, wildcard, or a LAN address on the
	// bench). The previous "http://127.0.0.1" + cfg.ListenAddr
	// concatenation was only correct for a host-less ListenAddr like
	// ":9100" — since WS-1 defaulted ListenAddr to "127.0.0.1:9100" it
	// produced a malformed "http://127.0.0.1127.0.0.1:9100/healthz" that
	// probeHealthz would always fail to reach, silently starving every
	// watchdog kick gated on it. Fixed here as part of wiring the
	// TLS-conditional scheme (DEVICE_ROADMAP.md §4.1's "keep http when
	// off, https when on" — both need a clean host:port join either way).
	_, healthzPort, err := net.SplitHostPort(cfg.ListenAddr)
	if err != nil {
		log.Fatalf("lexa-api: parse listen_addr %q: %v", cfg.ListenAddr, err)
	}
	healthzURL := scheme + "://127.0.0.1:" + healthzPort + "/healthz"

	// healthzClient probes the loopback /healthz above. When TLS is on this
	// is a same-process LIVENESS check, not an identity check — the whole
	// point is "does this process's own HTTP server answer", so
	// InsecureSkipVerify is correct here even though it would not be for a
	// real client (see tlscert.go's fingerprint-pinning doc for how a real
	// client is expected to verify this cert instead).
	healthzClient := &http.Client{Timeout: 2 * time.Second}
	if cfg.TLSEnabled() {
		healthzClient = &http.Client{
			Timeout: 2 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // loopback liveness only, not identity
			},
		}
	}

	// Give the ListenAndServe goroutine above a moment to bind before the
	// single startup probe (task spec: "probe once before Ready"). A failed
	// probe here does not block Ready — an HTTP server that never binds is a
	// real bug systemd's own TimeoutStartSec will catch via a missing Ready
	// altogether if this were made to block; instead we log loudly and let
	// the kick loop below (which gates on the same probe) be the ongoing
	// signal of whether /healthz is actually up.
	time.Sleep(100 * time.Millisecond)
	if !probeHealthz(healthzClient, healthzURL) {
		log.Printf("lexa-api: startup healthz probe failed (%s) — sending Ready anyway; watchdog kicks will gate on this same probe", healthzURL)
	}

	// DEVICE_ROADMAP.md §4.4: mDNS advertisement. serial was already resolved
	// above (before the mux was built, so /site could close over it too) —
	// resolveSerial is a cheap file read, and reusing the same value here
	// (rather than re-deriving it) keeps the mDNS TXT "serial=" field, the
	// cert identity, and /site's "serial" field all provably in agreement.
	// Non-fatal by construction (startMDNS logs and returns nil on any
	// failure); refreshLoop and Shutdown are both nil-receiver-safe so no
	// "if configured" guard is needed below.
	var mdnsAdv *mdnsAdvertiser
	if cfg.MDNSEnabled() {
		mdnsAdv = startMDNS(serial, cfg.ListenAddr, cfg.TLSEnabled())
	}
	mdnsStop := make(chan struct{})
	go mdnsAdv.refreshLoop(mdnsStop)

	// sd_notify READY (TASK-008): the HTTP listener goroutine is up (probed
	// once, above) and all MQTT subscriptions were established earlier in
	// this function (each subscribe error is fatal, so reaching this line
	// means they succeeded).
	watchdog.Ready()

	// TASK-008: no tight control loop exists here — this ticker is the
	// liveness proxy, kicking only when BOTH MQTT is connected AND a fresh
	// loopback /healthz probe returns 200, so a wedged HTTP server (mux
	// handlers deadlocked, listener goroutine dead) withholds the kick even
	// though the process itself is still scheduling goroutines. WS-9.1:
	// mc.IsConnected() alone stays true for the ENTIRE duration of a broker
	// outage as long as paho's AutoReconnect keeps retrying, so it is paired
	// with deaf.DeafFor — once a continuous outage exceeds
	// cfg.MQTTDeafRestartAfter (default 5 min), the kick gate stops firing
	// and systemd's WatchdogSec restarts this service.
	kick := time.NewTicker(10 * time.Second)
	defer kick.Stop()
	deafRestartAfter := cfg.MQTTDeafRestartAfter()

	// TASK-045: heartbeat evaluation cadence — independent of the watchdog
	// kick ticker above (different purpose: this one drives the edge-triggered
	// stall alarm + metric gauges, not liveness).
	hbTicker := time.NewTicker(5 * time.Second)
	defer hbTicker.Stop()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	for {
		select {
		case <-quit:
			log.Println("lexa-api: shutting down")
			close(mdnsStop)
			mdnsAdv.Shutdown()
			_ = srv.Close()
			return
		case <-kick.C:
			if mc.IsConnected() && deaf.DeafFor(time.Now()) < deafRestartAfter && probeHealthz(healthzClient, healthzURL) {
				watchdog.Kick()
			}
		case <-hbTicker.C:
			planHB.tick(time.Now())
		}
	}
}

// probeHealthz performs a single bounded GET against the local /healthz
// endpoint using client, returning true only on a 200 response. Used both
// for the startup probe and for gating each watchdog kick (TASK-008): the
// api service has no tight control loop, so an actual, current HTTP round
// trip is the strongest liveness signal available. client is caller-
// supplied (rather than constructed here) so main() can hand in a TLS
// transport with InsecureSkipVerify when the listener is HTTPS — see the
// call site's doc for why that's the right trust model for a loopback
// liveness probe.
func probeHealthz(client *http.Client, url string) bool {
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
