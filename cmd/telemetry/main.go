// lexa-telemetry subscribes to device measurements on the MQTT bus and
// forwards them to the CSIP server as MirrorUsagePoint readings (MUP POST).
//
// On startup it registers one MUP per configured device with the server, then
// posts batched meter readings every mup_post_rate_s seconds.
//
// Usage:
//
//	lexa-telemetry [-config /etc/lexa/telemetry.json]
package main

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"encoding/xml"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/logutil"
	"lexa-hub/internal/metrics"
	"lexa-hub/internal/mqttutil"
	"lexa-hub/internal/northbound/identity"
	"lexa-hub/internal/southbound/device"
	"lexa-hub/internal/tlsclient"
	"lexa-hub/internal/watchdog"
	"lexa-hub/internal/wolfssl"
	model "lexa-proto/csipmodel"
)

type mupEntry struct {
	device string
	path   string
	fails  int
}

func main() {
	cfgPath := flag.String("config", "/etc/lexa/telemetry.json", "path to JSON config")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("lexa-telemetry: load config: %v", err)
	}
	logutil.Setup("lexa-telemetry", logutil.ParseLevel(cfg.LogLevel)) // TASK-045

	// Unit 1.7 (DEVICE_ROADMAP.md §9, closing a gap found in unit 1.6): an
	// uncommissioned unit has no server to post to, so wolfSSL init, TLS
	// fetcher construction (which loads cert/key files that may not exist
	// yet on a factory-fresh/-reset unit), LFDI derivation from the client
	// cert, and MUP registration/posting all stay unstarted — see runIdle's
	// doc comment for exactly what still runs.
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
	fetcher, err := tlsclient.NewWolfSSLFetcher(tlsCfg)
	if err != nil {
		log.Fatalf("lexa-telemetry: init TLS fetcher: %v", err)
	}
	defer fetcher.Free()

	lfdi := cfg.LFDI
	if lfdi == "" {
		lfdi, err = lfdiFromCert(cfg.ClientCert)
		if err != nil {
			log.Fatalf("lexa-telemetry: derive LFDI: %v", err)
		}
	}
	log.Printf("lexa-telemetry: LFDI=%s server=%s", lfdi, cfg.Server)

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
	mupPostsTotalCtr := reg.Counter("lexa_telemetry_mup_posts_total")
	postFailuresTotalCtr := reg.Counter("lexa_telemetry_post_failures_total")
	connectedGauge := reg.Gauge("lexa_telemetry_connected")

	mqttPass, err := mqttutil.LoadPassword(cfg.MQTTPassFile)
	if err != nil {
		log.Fatalf("lexa-telemetry: %v", err)
	}
	mc, err := mqttutil.ConnectAuthInstrumented(cfg.MQTTBroker, cfg.MQTTClientID, cfg.MQTTUser, mqttPass, mqttutil.Instrumentation{
		OnPublishFail: mqttFailCtr.Inc,
		OnReconnect:   mqttReconnCtr.Inc,
	})
	if err != nil {
		log.Fatalf("lexa-telemetry: %v", err)
	}
	defer mc.Disconnect(500)

	metrics.Serve(cfg.MetricsAddr, reg)

	// ctx (TASK-070, R5): canceled by the signal-bridge goroutine below once
	// SIGINT/SIGTERM arrives. Threaded into every PostContext call in the
	// per-tick loop so a shutdown mid-tick stops making new MUP POSTs between
	// devices/quantities instead of finishing the whole tick first — the
	// same "check ctx before the next request, don't try to interrupt one in
	// flight" contract as lexa-northbound's walker (see
	// tlsclient.WolfSSLFetcher.PostContext's doc comment).
	ctx, cancel := context.WithCancel(context.Background())

	// Register MUPs for each configured device.
	var mups []mupEntry
	for _, dev := range cfg.Devices {
		path, err := registerMUP(ctx, fetcher, lfdi, dev, cfg.MUPPostRateS)
		if err != nil {
			log.Printf("lexa-telemetry: MUP register %s: %v — skipping", dev, err)
			continue
		}
		mups = append(mups, mupEntry{device: dev, path: path})
	}
	if len(mups) == 0 {
		log.Fatal("lexa-telemetry: no MUPs registered — exiting")
	}

	// sd_notify READY (TASK-008): registerMUP makes exactly one bounded POST
	// attempt per device (no internal retry loop — a per-device failure is
	// logged and skipped, above), so this loop cannot hang on an unreachable
	// server; it either finishes fast or the process has already exited via
	// the len(mups)==0 Fatal. Placed before the (fast, local) MQTT
	// subscriptions below since MUP registration — the network round trip to
	// the utility server — is the part of startup that could plausibly be
	// slow, and it has just completed.
	watchdog.Ready()

	// mu guards both latest measurements and clockOffset so snapshots are
	// always from the same lock epoch (no clock/data skew between locks).
	var mu sync.RWMutex
	latest := make(map[string]device.Measurements)
	var clockOffset int64

	// Initialise to NaN so we don't post zeros before the first poll.
	for _, dev := range cfg.Devices {
		latest[dev] = device.Measurements{W: math.NaN(), V: math.NaN(), Hz: math.NaN()}
	}

	// Subscribe to measurements from the modbus service.
	if err := mqttutil.Subscribe(mc, bus.SubMeasurements, func(_ string, msg bus.Measurement) {
		mu.Lock()
		m := latest[msg.Device]
		if msg.W != nil {
			m.W = *msg.W
		}
		if msg.VoltageV != nil {
			m.V = *msg.VoltageV
		}
		if msg.Hz != nil {
			m.Hz = *msg.Hz
		}
		latest[msg.Device] = m
		mu.Unlock()
	}); err != nil {
		log.Fatalf("lexa-telemetry: subscribe measurements: %v", err)
	}

	// Subscribe to clock offset updates from the CSIP service.
	if err := mqttutil.Subscribe(mc, bus.TopicCSIPControl, func(_ string, msg bus.ActiveControl) {
		mu.Lock()
		clockOffset = msg.ClockOffset
		mu.Unlock()
	}); err != nil {
		log.Printf("lexa-telemetry: subscribe csip control: %v", err)
	}

	ticker := time.NewTicker(cfg.MUPPostRate())
	defer ticker.Stop()

	// TASK-008 watchdog kick ticker: added as a case in the SAME select as
	// the post loop below (not a free-running goroutine) so a wedged
	// postMeasurements blocks this kick too — telemetry has no tight control
	// loop like northbound/modbus, so riding the post-loop select is the
	// closest available liveness signal. 10 s cadence gives ample headroom
	// under WatchdogSec=60 even at the slowest configured MUPPostRate.
	kick := time.NewTicker(10 * time.Second)
	defer kick.Stop()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	// TASK-070 (R5): bridge the OS signal to ctx cancellation in its own
	// goroutine, rather than a `case <-quit:` in the select below. A signal
	// arriving while the ticker case is mid-flight (looping over every
	// device's POSTs) cannot be observed by a select until that case
	// returns — bridging to ctx instead lets the code INSIDE the loop below
	// (via PostContext's preflight check) notice cancellation between
	// individual POSTs, not just between ticks.
	go func() {
		<-quit
		log.Println("lexa-telemetry: shutting down")
		cancel()
	}()

	for {
		select {
		case <-ctx.Done():
			return

		case <-kick.C:
			watchdog.Kick()

		case <-ticker.C:
			mu.RLock()
			snap := make(map[string]device.Measurements, len(latest))
			for k, v := range latest {
				snap[k] = v
			}
			offset := clockOffset
			mu.RUnlock()

			for i := range mups {
				if ctx.Err() != nil {
					// Shutdown mid-tick: stop starting new device POSTs.
					// Between-request granularity only — see PostContext's
					// doc comment for why a POST already in flight isn't
					// interrupted early.
					break
				}
				ep := &mups[i]
				m := snap[ep.device]
				err := postMeasurements(ctx, fetcher, ep.device, ep.path, m, offset, cfg.MUPPostRateS)
				// TASK-044: lexa_telemetry_connected is this service's
				// "connection state" gauge — telemetry has no persistent
				// session like OCPP's WS connections, so the closest
				// equivalent is whether the last POST round to the utility
				// server succeeded. Set per-device-loop-iteration; the last
				// device posted each tick wins, matching this task's
				// no-per-device-labels metric inventory.
				if err != nil {
					postFailuresTotalCtr.Inc()
					connectedGauge.Set(0)
					ep.fails++
					if ep.fails >= 3 {
						log.Printf("lexa-telemetry: re-registering MUP for %s after %d failures", ep.device, ep.fails)
						newPath, rerr := registerMUP(ctx, fetcher, lfdi, ep.device, cfg.MUPPostRateS)
						if rerr == nil {
							ep.path = newPath
							ep.fails = 0
						}
					}
				} else {
					mupPostsTotalCtr.Inc()
					connectedGauge.Set(1)
					ep.fails = 0
				}
			}
		}
	}
}

// runIdle is the uncommissioned-idle path (Unit 1.7, DEVICE_ROADMAP.md §9):
// no server is configured, so there is nothing to post to and — critically
// — nothing that requires the cert/key files at cfg.CACert/ClientCert/
// ClientKey to exist yet. wolfSSL init, TLS fetcher construction, LFDI
// derivation from the client cert (lfdiFromCert), and MUP registration/
// posting all touch those files (directly or via wolfSSL) and are skipped
// entirely, not deferred — a factory-fresh or factory-reset unit (no certs
// on disk) must idle cleanly instead of crash-looping into systemd's
// StartLimit (V1RC FINDING A; configs/factory/README.md "Known gaps" #1
// names this exact failure — it also notes the "no MUPs registered" Fatal
// this codepath would otherwise hit immediately with zero configured
// devices, independent of the cert bug).
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
		log.Fatalf("lexa-telemetry: %v", err)
	}
	mc, err := mqttutil.ConnectAuthInstrumented(cfg.MQTTBroker, cfg.MQTTClientID, cfg.MQTTUser, mqttPass, mqttutil.Instrumentation{
		OnPublishFail: mqttFailCtr.Inc,
		OnReconnect:   mqttReconnCtr.Inc,
	})
	if err != nil {
		log.Fatalf("lexa-telemetry: %v", err)
	}
	defer mc.Disconnect(500)

	metrics.Serve(cfg.MetricsAddr, reg)

	// sd_notify READY (TASK-008): there is no MUP registration to wait on —
	// idle is the terminal state until a config/restart from commissioning.
	watchdog.Ready()

	kick := time.NewTicker(10 * time.Second)
	defer kick.Stop()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	for {
		select {
		case <-quit:
			log.Println("lexa-telemetry: shutting down (uncommissioned idle)")
			return
		case <-kick.C:
			if mc.IsConnected() {
				watchdog.Kick()
			}
		}
	}
}

func registerMUP(ctx context.Context, fetcher *tlsclient.WolfSSLFetcher, lfdi, devName string, rateS int) (string, error) {
	prefix := lfdi
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	mup := model.MirrorUsagePoint{
		MRID:                prefix + "-" + devName,
		Description:         devName + " Measurements (W/V/Hz)",
		RoleFlags:           0x0002,
		ServiceCategoryKind: 0,
		Status:              1,
		DeviceLFDI:          lfdi,
		PostRate:            uint32(rateS),
	}
	body, err := xml.Marshal(&mup)
	if err != nil {
		return "", err
	}
	_, loc, err := fetcher.PostContext(ctx, "/mup", body, "application/sep+xml")
	if err != nil {
		return "", err
	}
	log.Printf("lexa-telemetry: MUP registered: %s → %s", devName, loc)
	return loc, nil
}

// quantity describes one measured value and how to encode it as a
// self-describing IEEE 2030.5 MirrorMeterReading. A reading is only meaningful
// to the server if its ReadingType declares the unit (uom) and scale
// (powerOfTenMultiplier) — without them V×100 is just an opaque integer
// (audit finding S-2).
type quantity struct {
	value      float64
	scale      float64 // multiply the SI value by this before rounding to int
	uom        uint8
	kind       uint8
	multiplier int8 // powerOfTenMultiplier: value = encoded × 10^multiplier
	suffix     string
}

func postMeasurements(
	ctx context.Context,
	fetcher *tlsclient.WolfSSLFetcher,
	devName, mupPath string,
	m device.Measurements,
	clockOffset int64,
	intervalS int,
) error {
	now := time.Now().Unix() + clockOffset
	dur := uint32(intervalS)
	start := now - int64(dur)

	// One MirrorMeterReading per quantity, each carrying its own ReadingType.
	// V and Hz are scaled ×100 (multiplier −2); W is sent as whole watts.
	quantities := []quantity{
		{m.W, 1, model.UomWatts, model.KindPower, 0, "W"},
		{m.V, 100, model.UomVolts, model.KindVoltage, -2, "V"},
		{m.Hz, 100, model.UomHertz, model.KindFreq, -2, "Hz"},
	}

	posted := 0
	for _, q := range quantities {
		if math.IsNaN(q.value) {
			continue
		}
		mmr := model.MirrorMeterReading{
			MRID:        devName + "-" + q.suffix,
			Description: devName + " " + q.suffix,
			ReadingType: &model.ReadingType{
				DataQualifier:        model.DataQualifierAverage,
				Kind:                 q.kind,
				PowerOfTenMultiplier: q.multiplier,
				Uom:                  q.uom,
				IntervalLength:       dur,
			},
			MirrorReadingSet: []model.MirrorReadingSet{{
				StartTime: start,
				Duration:  dur,
				Reading: []model.Reading{{
					Value:      int64(math.Round(q.value * q.scale)),
					TimePeriod: &model.DateTimeInterval{Start: start, Duration: dur},
				}},
			}},
		}
		body, err := xml.Marshal(&mmr)
		if err != nil {
			return err
		}
		if _, _, err = fetcher.PostContext(ctx, mupPath, body, "application/sep+xml"); err != nil {
			log.Printf("lexa-telemetry: POST %s %s: %v", devName, q.suffix, err)
			return err
		}
		posted++
	}
	if posted == 0 {
		return nil
	}
	// TASK-045 per-tick demotion: fires every mup_post_rate_s (default 300 s,
	// but bench-tunable much faster) for every device — steady-state
	// success, not a transition. The per-service TASK-044 counters
	// (lexa_telemetry_mup_posts_total, lexa_telemetry_post_failures_total)
	// already cover "is posting happening"; the POST-error path above stays
	// at Info (it is an edge, not steady-state).
	slog.Debug("lexa-telemetry: posted", "device", devName, "w", m.W, "v", m.V, "hz", m.Hz)
	return nil
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
