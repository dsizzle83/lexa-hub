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
//
// Usage:
//
//	lexa-api [-config /etc/lexa/api.json]
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/mqttutil"
	"lexa-hub/internal/watchdog"
)

func main() {
	cfgPath := flag.String("config", "/etc/lexa/api.json", "path to JSON config")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("lexa-api: load config: %v", err)
	}

	mc, err := mqttutil.Connect(cfg.MQTTBroker, cfg.MQTTClientID)
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

	if err := mqttutil.Subscribe(mc, bus.TopicHubPlan, func(topic string, p bus.PlanLog) {
		store.onPlanLog(topic, p)
		// Emit only decision-bearing plans to /logs — the heartbeat cadence
		// (every engine tick) would drown the stream.
		for _, dec := range p.Decisions {
			lb.Emit(fmt.Sprintf("[plan] %s: %s → %s", dec.Rule, dec.Reason, dec.Impact))
		}
	}); err != nil {
		log.Fatalf("lexa-api: subscribe hub plan: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthzHandler)
	mux.HandleFunc("/status", statusHandler(store))
	mux.HandleFunc("/logs", logsHandler(lb))

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: mux}
	go func() {
		log.Printf("lexa-api: HTTP listening on %s", cfg.ListenAddr)
		lb.Emit(fmt.Sprintf("lexa-api: HTTP listening on %s", cfg.ListenAddr))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("lexa-api: listen: %v", err)
		}
	}()

	// TASK-008: cfg.ListenAddr is host:port form (default ":9100") — prefixing
	// the loopback host gives a same-process self-probe URL for /healthz.
	healthzURL := "http://127.0.0.1" + cfg.ListenAddr + "/healthz"

	// Give the ListenAndServe goroutine above a moment to bind before the
	// single startup probe (task spec: "probe once before Ready"). A failed
	// probe here does not block Ready — an HTTP server that never binds is a
	// real bug systemd's own TimeoutStartSec will catch via a missing Ready
	// altogether if this were made to block; instead we log loudly and let
	// the kick loop below (which gates on the same probe) be the ongoing
	// signal of whether /healthz is actually up.
	time.Sleep(100 * time.Millisecond)
	if !probeHealthz(healthzURL) {
		log.Printf("lexa-api: startup healthz probe failed (%s) — sending Ready anyway; watchdog kicks will gate on this same probe", healthzURL)
	}

	// sd_notify READY (TASK-008): the HTTP listener goroutine is up (probed
	// once, above) and all MQTT subscriptions were established earlier in
	// this function (each subscribe error is fatal, so reaching this line
	// means they succeeded).
	watchdog.Ready()

	// TASK-008: no tight control loop exists here — this ticker is the
	// liveness proxy, kicking only when BOTH MQTT is connected AND a fresh
	// loopback /healthz probe returns 200, so a wedged HTTP server (mux
	// handlers deadlocked, listener goroutine dead) withholds the kick even
	// though the process itself is still scheduling goroutines.
	kick := time.NewTicker(10 * time.Second)
	defer kick.Stop()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	for {
		select {
		case <-quit:
			log.Println("lexa-api: shutting down")
			_ = srv.Close()
			return
		case <-kick.C:
			if mc.IsConnected() && probeHealthz(healthzURL) {
				watchdog.Kick()
			}
		}
	}
}

// probeHealthz performs a single bounded GET against the local /healthz
// endpoint, returning true only on a 200 response. Used both for the
// startup probe and for gating each watchdog kick (TASK-008): the api
// service has no tight control loop, so an actual, current HTTP round trip
// is the strongest liveness signal available.
func probeHealthz(url string) bool {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
