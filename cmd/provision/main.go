// lexa-provision is the BLE commissioning service (ADR-0002, "LEXA Provision
// v1", unit B2). It exposes the sec1 handshake (unit B1) as a real Bluetooth LE
// GATT peripheral via BlueZ over D-Bus: a phone (GATT central) authenticates
// with the setup code printed on the device label, then — in later units —
// delivers WiFi credentials and receives the connection material for the hub's
// HTTPS API.
//
// This service advertises ONLY while the unit is uncommissioned (no
// /etc/lexa/commissioned marker); once commissioning completes it goes silent
// on the radio. It is the campaign's one new dependency: github.com/godbus/
// dbus/v5 (pure Go, CGO stays off).
//
// Scope of B2: the GATT server + advertising + the sec1 handshake and info
// read, end to end. WiFi scan and join are STUBBED (clear seams for unit B3);
// the PoP loading + re-provision window + real handoff are unit B4.
//
// Usage:
//
//	lexa-provision [-config /etc/lexa/provision.json]
package main

import (
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"

	"lexa-hub/internal/buildinfo"
	"lexa-hub/internal/logutil"
	"lexa-hub/internal/metrics"
	"lexa-hub/internal/provision/gatt"
	"lexa-hub/internal/provision/sec1"
	"lexa-hub/internal/watchdog"
)

func main() {
	cfgPath := flag.String("config", "/etc/lexa/provision.json", "path to JSON config")
	showVersion := flag.Bool("version", false, "print the build version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(buildinfo.Version)
		return
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("lexa-provision: load config: %v", err)
	}
	logutil.Setup("lexa-provision", logutil.ParseLevel(cfg.LogLevel))

	serial := cfg.resolveSerial()
	pop, fromFile := cfg.loadPoP()
	if !fromFile {
		slog.Warn("using dev-kit default PoP — a product image MUST provision a per-unit PoP",
			"pop_file", cfg.PopFile)
	}

	// Metrics (TASK-044 pattern): standard process gauges + the three
	// provision-specific series. No MQTT — this service is BLE/D-Bus only.
	reg := metrics.New()
	metrics.StandardGauges(reg)
	advertisingGauge := reg.Gauge("lexa_provision_advertising")
	sessionsCtr := reg.Counter("lexa_provision_sessions_total")
	popFailuresCtr := reg.Counter("lexa_provision_pop_failures_total")

	// Peripheral factory: a fresh sec1 peripheral per connection (Reset).
	// Info reports build truth (buildinfo.Version + resolved serial +
	// live commissioned marker). ScanResults/JoinBehavior are B2 stubs — the
	// B3 seam wires NetworkManager scan + join here.
	newPeripheral := func() *sec1.Peripheral {
		return sec1.NewPeripheral(sec1.PeripheralConfig{
			Pop:          pop,
			Serial:       serial,
			Fw:           buildinfo.Version,
			Commissioned: markerPresent(cfg.MarkerFile),
			ScanResults:  nil,        // B3: real NetworkManager scan
			JoinBehavior: stubJoin(), // B3: real NetworkManager join
		})
	}
	disp := gatt.NewDispatcher(newPeripheral, gatt.Observer{
		OnSessionEstablished: sessionsCtr.Inc,
		OnPopFailure:         popFailuresCtr.Inc,
	})

	// System bus + GATT application.
	conn, err := dbus.SystemBus()
	if err != nil {
		log.Fatalf("lexa-provision: connect system bus: %v", err)
	}
	defer conn.Close()

	adapter := gatt.AdapterPath(cfg.Adapter)
	server, err := gatt.NewServer(conn, adapter, disp, sec1.UUIDService, gatt.ADRCharDefs())
	if err != nil {
		log.Fatalf("lexa-provision: build GATT server: %v", err)
	}
	if err := server.Register(); err != nil {
		log.Fatalf("lexa-provision: register GATT application: %v", err)
	}
	defer func() { _ = server.Unregister() }()
	log.Printf("lexa-provision: GATT application registered on %s (serial=%s fw=%s)",
		adapter, serial, buildinfo.Version)

	// Advertising: real BlueZ AdManager, gated on the commissioned marker.
	// MarkerGate.Window is the B4 re-provision-window seam (nil for B2).
	adMgr := gatt.NewBluezAdManager(conn, adapter, gatt.LocalName(serial), sec1.UUIDService)
	gate := gatt.MarkerGate{MarkerPath: cfg.MarkerFile}
	adv := gatt.NewAdvertiser(adMgr, gate, func(on bool) {
		if on {
			advertisingGauge.Set(1)
			slog.Info("advertising started", "name", gatt.LocalName(serial))
		} else {
			advertisingGauge.Set(0)
			slog.Info("advertising stopped (commissioned or shutting down)")
		}
	})
	if err := adv.Reconcile(); err != nil {
		// Non-fatal: a failed initial advertise must not crash-loop the
		// service; the reconcile ticker retries, and the fault is logged.
		log.Printf("lexa-provision: initial advertising reconcile: %v", err)
	}

	metrics.Serve(cfg.MetricsAddr, reg)

	// sd_notify READY (TASK-008 pattern): the GATT app is registered and the
	// advertising state is reconciled — the service is doing its job.
	watchdog.Ready()

	// Watchdog kick: like lexa-ocpp/lexa-api, this service has no tight control
	// loop, so a 10s ticker gated on the D-Bus connection being alive is the
	// liveness proxy. A dead system bus (BlueZ/dbus-daemon gone) withholds the
	// kick and systemd restarts us — accepted crash-only behavior (AD-011).
	kick := time.NewTicker(10 * time.Second)
	defer kick.Stop()

	// Reconcile ticker: re-evaluate the advertising gate so a mid-run
	// commissioning commit (marker written) stops advertising within one tick.
	reconcile := time.NewTicker(cfg.ReconcileInterval())
	defer reconcile.Stop()

	// SIGHUP triggers an immediate reconcile (operator/tooling nudge).
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case <-quit:
			log.Println("lexa-provision: shutting down")
			if err := adv.Stop(); err != nil {
				log.Printf("lexa-provision: stop advertising: %v", err)
			}
			if err := server.Unregister(); err != nil {
				log.Printf("lexa-provision: unregister application: %v", err)
			}
			return

		case <-kick.C:
			if conn.Connected() {
				watchdog.Kick()
			}

		case <-reconcile.C:
			if err := adv.Reconcile(); err != nil {
				log.Printf("lexa-provision: advertising reconcile: %v", err)
			}
			// After a completed handoff the central sends done; recycle the
			// peripheral so the next central starts from a clean handshake.
			// (Advertising is separately gated by the marker; done alone does
			// not write the marker — see ADR-0002.)
			if disp.DoneReceived() {
				slog.Info("commissioning session done — recycling peripheral")
				disp.Reset()
			}

		case <-hup:
			if err := adv.Reconcile(); err != nil {
				log.Printf("lexa-provision: advertising reconcile (SIGHUP): %v", err)
			}
		}
	}
}

// stubJoin is the B2 placeholder JoinBehavior: it emits one "joining" then a
// "failed: internal" so a central driving the full flow gets a defined,
// end-to-end response (exercising the encrypted status-indication path) rather
// than hanging. Unit B3 replaces this with a real NetworkManager
// AddAndActivateConnection that emits joining → joined{handoff} / failed{reason}.
func stubJoin() sec1.JoinBehavior {
	return sec1.JoinFails{Reason: sec1.ReasonInternal, JoiningEvents: 1}
}

// markerPresent reports whether the commissioned marker exists (used to fill
// the info document's "commissioned" field with truth).
func markerPresent(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
