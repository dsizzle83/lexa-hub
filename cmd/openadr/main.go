// lexa-openadr is the OpenADR 3.1 VEN service (WP-15, standards-buildout
// E1): it polls a VTN (Continuous Pricing profile — pull-only, no inbound
// listener), translates CP payloads into three retained bus documents
// (lexa/openadr/{prices,limits,status}), and POSTs usage reports built from
// the local measurement stream.
//
// Pure Go: stdlib net/http + crypto/tls only — NO wolfSSL, NO CGo
// (CGO_ENABLED=0 cross-compilable, unlike lexa-northbound/lexa-telemetry).
// OpenADR has no CSIP-style cipher pin, so internal/tlsclient is not needed.
//
// SCOPE (WP-15 service half): this service only PUBLISHES the retained
// docs. The cmd/hub adoption slice (prices → SetPrices/SetDeliveryTariff,
// limits → GridState arbitration per D9) lands separately after WP-11 —
// until then the retained docs sit harmless on the broker.
//
// Crash-only (AD-011): no recover() anywhere; a wedged process dies, systemd
// restarts it (Type=notify, WatchdogSec=120 — see the unit file), and the
// retained docs re-seed subscribers.
//
// Usage:
//
//	lexa-openadr [-config /etc/lexa/openadr.json]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/logutil"
	"lexa-hub/internal/metrics"
	"lexa-hub/internal/mqttutil"
	"lexa-hub/internal/openadr"
	"lexa-hub/internal/watchdog"
)

// vtnRequestTimeout bounds every single VTN HTTP round trip (token POST,
// each pagination page, report POST). A hung VTN therefore stalls one poll
// cycle by at most a few of these — comfortably inside WatchdogSec=120 for
// sane page counts; a pathological VTN that drags a cycle past the watchdog
// gets the accepted crash-only restart.
const vtnRequestTimeout = 15 * time.Second

// maxPollBackoff caps the failure backoff on the poll cadence (nextWait).
const maxPollBackoff = 15 * time.Minute

func main() {
	cfgPath := flag.String("config", "/etc/lexa/openadr.json", "path to JSON config")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("lexa-openadr: load config: %v", err)
	}
	logutil.Setup("lexa-openadr", logutil.ParseLevel(cfg.LogLevel)) // TASK-045

	// TASK-044 pattern: registry + standard process gauges + the summed
	// bus-decode-failure Collect hook, same shape as cmd/telemetry/main.go.
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
	pollErrCtr := reg.Counter("lexa_openadr_poll_errors_total")
	tokenRefreshCtr := reg.Counter("lexa_openadr_token_refresh_total")
	reportsPostedCtr := reg.Counter("lexa_openadr_reports_posted_total")
	eventsActiveGauge := reg.Gauge("lexa_openadr_events_active")

	mqttPass, err := mqttutil.LoadPassword(cfg.MQTTPassFile)
	if err != nil {
		log.Fatalf("lexa-openadr: %v", err)
	}
	mc, err := mqttutil.ConnectAuthInstrumented(cfg.MQTTBroker, cfg.MQTTClientID, cfg.MQTTUser, mqttPass, mqttutil.Instrumentation{
		OnPublishFail: mqttFailCtr.Inc,
		OnReconnect:   mqttReconnCtr.Inc,
	})
	if err != nil {
		log.Fatalf("lexa-openadr: %v", err)
	}
	defer mc.Disconnect(500)

	serveHTTP(cfg.MetricsAddr, reg)

	// Uncommissioned idle (telemetry pattern, Unit 1.7 discipline): no VTN
	// configured ⇒ nothing to poll, no secret files touched, no bus docs
	// published. Metrics/healthz, the MQTT session, and the watchdog all
	// keep running so the process stays alive and systemd-healthy until
	// commissioning writes a real config and restarts the service.
	if cfg.Uncommissioned() {
		runIdle(mc)
		return
	}

	secret, err := loadSecret(cfg.ClientSecretFile)
	if err != nil {
		// Configured-but-broken is a loud startup error, never masked
		// (the exact opposite of the uncommissioned branch above).
		log.Fatalf("lexa-openadr: %v", err)
	}

	httpc := &http.Client{Timeout: vtnRequestTimeout}

	var tokens *openadr.TokenSource
	if cfg.ClientID != "" {
		tokenURL := resolveTokenURL(cfg, httpc)
		tokens = openadr.NewTokenSource(httpc, tokenURL, cfg.ClientID, secret, tokenRefreshCtr.Inc)
	} else {
		slog.Info("lexa-openadr: no client_id configured — unauthenticated VEN (public-tariff VTN mode)")
	}

	client := &openadr.Client{Base: cfg.VTNURL, HTTP: httpc, Tokens: tokens}
	ven := openadr.New(client, cfg.ProgramIDs, cfg.VenName)

	// Measurement collector for USAGE reports (+ the battery-metrics
	// STORAGE_* seam — collected, not yet reported; see
	// internal/openadr/report.go). Subscribed regardless of report_enabled:
	// the window is cheap and a config flip to enabled needs no restart
	// semantics beyond the one systemd already provides.
	collector := newUsageCollector(time.Now())
	if err := mqttutil.Subscribe(mc, bus.SubMeasurements, func(_ string, m bus.Measurement) {
		collector.OnMeasurement(m)
	}); err != nil {
		log.Fatalf("lexa-openadr: subscribe measurements: %v", err)
	}
	if err := mqttutil.Subscribe(mc, bus.SubBattMetrics, func(_ string, b bus.BattMetrics) {
		collector.OnBattMetrics(b)
	}); err != nil {
		log.Fatalf("lexa-openadr: subscribe battery metrics: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-quit
		log.Println("lexa-openadr: shutting down")
		cancel()
	}()

	// sd_notify READY (TASK-008): all wiring is done and the poll loop is
	// about to take its first (immediate) tick. Nothing above can hang on
	// the VTN — the first VTN round trip happens inside the loop, bounded
	// by vtnRequestTimeout.
	watchdog.Ready()

	slog.Info("lexa-openadr: VEN starting", "vtn", cfg.VTNURL, "ven_name", cfg.VenName,
		"poll_interval_s", cfg.PollIntervalS, "programs_filter", len(cfg.ProgramIDs), "reports", cfg.ReportsEnabled())

	// Poll loop. Watchdog liveness has two sources in ONE select (so a
	// wedged poll starves both — the exact semantics WatchdogSec exists
	// for): the 10 s idle ticker gated on broker connectivity (telemetry/
	// ocpp pattern — covers arbitrarily long poll intervals), and a kick at
	// the poll body top (northbound run-loop pattern).
	pollTimer := time.NewTimer(0) // first poll immediately
	defer pollTimer.Stop()
	kick := time.NewTicker(10 * time.Second)
	defer kick.Stop()

	var (
		failures     int
		lastPollTs   int64
		lastPrograms int
		lastActive   int
		lastPrices   []byte
		lastLimits   []byte
	)

	for {
		select {
		case <-ctx.Done():
			return

		case <-kick.C:
			if mc.IsConnected() {
				watchdog.Kick()
			}

		case <-pollTimer.C:
			watchdog.Kick() // poll-loop body top (northbound pattern)
			now := time.Now()
			res, err := ven.PollOnce(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return // shutdown mid-poll, not a VTN failure
				}
				pollErrCtr.Inc()
				failures++
				slog.Warn("lexa-openadr: poll failed — retained docs hold last-known-good",
					"err", err, "consecutive_failures", failures)
			} else {
				failures = 0
				lastPollTs = now.Unix()
				lastPrograms = res.ProgramCount
				lastActive = res.ActiveEvents
				eventsActiveGauge.Set(float64(res.ActiveEvents))

				// Best-effort ven-object registration, once (idempotent
				// no-op after success or a permanent VTN refusal).
				ven.EnsureRegistered(ctx)

				lastPrices = publishIfChanged(mc, bus.TopicOpenADRPrices, res.Prices, lastPrices)
				lastLimits = publishIfChanged(mc, bus.TopicOpenADRLimits, res.Limits, lastLimits)

				if cfg.ReportsEnabled() {
					postDueReports(ctx, ven, client, collector, cfg, reportsPostedCtr, now)
				}
			}

			// Status doc EVERY cycle, success and failure alike. Program/
			// event counts hold their last-successful-poll values across a
			// failed cycle (VTNOK/LastErr carry the failure itself).
			status := bus.OpenADRStatus{
				Envelope:     bus.Envelope{V: bus.OpenADRStatusV},
				VTNOK:        err == nil,
				TokenOK:      tokens == nil || tokens.Healthy(),
				LastPollTs:   lastPollTs,
				Programs:     lastPrograms,
				ActiveEvents: lastActive,
				Ts:           time.Now().Unix(),
			}
			if err != nil {
				status.LastErr = err.Error()
			}
			if perr := mqttutil.PublishJSONRetained(mc, bus.TopicOpenADRStatus, status); perr != nil {
				log.Printf("lexa-openadr: publish status: %v", perr)
			}

			pollTimer.Reset(nextWait(cfg.PollInterval(), failures))
		}
	}
}

// publishIfChanged publishes v retained on topic only when its canonical
// JSON (Ts zeroed — see canonicalJSON) differs from prev: translation output
// is deterministically ordered, so unchanged VTN state produces zero
// retained-doc churn. Returns the canonical bytes for the next comparison.
func publishIfChanged(mc mqtt.Client, topic string, v any, prev []byte) []byte {
	canon, err := canonicalJSON(v)
	if err != nil {
		log.Printf("lexa-openadr: canonicalize %s: %v", topic, err)
		return prev
	}
	if prev != nil && string(canon) == string(prev) {
		return prev
	}
	if err := mqttutil.PublishJSONRetained(mc, topic, v); err != nil {
		log.Printf("lexa-openadr: publish %s: %v — will retry next cycle", topic, err)
		// Return prev unchanged so the next cycle re-detects the diff and
		// retries (the standard "late/dropped publishes are harmless
		// because they're re-issued" contract).
		return prev
	}
	slog.Info("lexa-openadr: retained doc updated", "topic", topic)
	return canon
}

// postDueReports builds+POSTs every due usage report. The collector window
// is snapshotted ONCE per cycle (all due streams see the same window) and
// only reset by that snapshot; MarkReported fires only on a successful POST
// so a failed one retries next cycle with a fresh window.
func postDueReports(ctx context.Context, ven *openadr.VEN, client *openadr.Client, collector *usageCollector, cfg *Config, posted *metrics.Counter, now time.Time) {
	due := ven.DueReports(now, cfg.PollInterval())
	if len(due) == 0 {
		return
	}
	snap := collector.Snapshot(now)
	for _, req := range due {
		rep, ok := openadr.BuildUsageReport(req, snap, cfg.VenName)
		if !ok {
			slog.Debug("lexa-openadr: report due but no usage data collected yet — skipped (never fabricate)",
				"event", req.Event.ID)
			continue
		}
		if err := client.PostReport(ctx, rep); err != nil {
			slog.Warn("lexa-openadr: report POST failed — will retry next cycle",
				"event", req.Event.ID, "err", err)
			continue
		}
		posted.Inc()
		ven.MarkReported(req, now)
		slog.Debug("lexa-openadr: report posted", "event", req.Event.ID, "resources", len(rep.Resources))
	}
}

// nextWait is the poll cadence with failure backoff: the configured interval
// while healthy; doubled per consecutive failure (capped at 8× and at
// maxPollBackoff) while the VTN is erroring — the northbound "keep iterating
// fail-closed, don't hammer" discipline. Pure function for table tests.
func nextWait(interval time.Duration, failures int) time.Duration {
	if failures <= 0 {
		return interval
	}
	shift := failures
	if shift > 3 {
		shift = 3
	}
	w := interval << shift
	if w > maxPollBackoff {
		w = maxPollBackoff
	}
	return w
}

// resolveTokenURL implements the token-endpoint precedence: configured
// token_url > 3.1 GET /auth/server discovery > {vtn_url}/auth/token (the
// spec's optional built-in endpoint). Discovery failure is non-fatal — the
// fallback keeps a bench VTN with no /auth/server working, and a wrong
// endpoint surfaces immediately as token-fetch errors on the status doc.
func resolveTokenURL(cfg *Config, httpc *http.Client) string {
	if cfg.TokenURL != "" {
		return cfg.TokenURL
	}
	ctx, cancel := context.WithTimeout(context.Background(), vtnRequestTimeout)
	defer cancel()
	if u, err := openadr.DiscoverTokenURL(ctx, httpc, cfg.VTNURL); err == nil {
		slog.Info("lexa-openadr: token endpoint discovered via /auth/server", "token_url", u)
		return u
	} else {
		slog.Warn("lexa-openadr: /auth/server discovery failed — falling back to {vtn_url}/auth/token", "err", err)
	}
	return cfg.VTNURL + "/auth/token"
}

// runIdle is the uncommissioned-idle loop (vtn_url "" — see the
// Uncommissioned doc in config.go): metrics/healthz and the MQTT session are
// already up; this just keeps the watchdog fed (10 s ticker gated on
// mc.IsConnected(), the lexa-ocpp/api idle-kick shape) until commissioning
// restarts the service with a live config. No bus docs are published — an
// uncommissioned VEN has no VTN state to report.
func runIdle(mc mqtt.Client) {
	slog.Info("lexa-openadr: uncommissioned idle — no vtn_url configured; commissioning will restart this service with a live config")
	watchdog.Ready()

	kick := time.NewTicker(10 * time.Second)
	defer kick.Stop()
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	for {
		select {
		case <-quit:
			log.Println("lexa-openadr: shutting down (uncommissioned idle)")
			return
		case <-kick.C:
			if mc.IsConnected() {
				watchdog.Kick()
			}
		}
	}
}

// serveHTTP serves /metrics AND /healthz on addr — metrics.Serve's contract
// ("" or "off" disables; bind failure logged, never fatal) plus the always-
// open /healthz the WP-15 idle requirement names. Hand-rolled here rather
// than extending internal/metrics: that package is a shared leaf and this is
// the only service that mounts healthz on its metrics listener.
func serveHTTP(addr string, reg *metrics.Registry) {
	if addr == "" || strings.EqualFold(addr, "off") {
		return
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", reg.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("lexa-openadr: serving /metrics and /healthz on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("lexa-openadr: serve %s: %v — metrics/healthz disabled for this run", addr, err)
		}
	}()
}

// canonicalJSON marshals v with its Ts field zeroed (typed copies — Ts is
// stamped fresh by the translator every poll) so retained-doc change
// detection ignores the publish timestamp: unchanged VTN state produces
// zero retained churn.
func canonicalJSON(v any) ([]byte, error) {
	switch t := v.(type) {
	case bus.OpenADRPrices:
		t.Ts = 0
		return json.Marshal(t)
	case bus.OpenADRLimits:
		t.Ts = 0
		return json.Marshal(t)
	default:
		return json.Marshal(v)
	}
}
