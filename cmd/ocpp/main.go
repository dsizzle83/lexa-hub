// lexa-ocpp runs the OCPP 2.0.1 Central System Management System (CSMS) and
// bridges EV charger state to the MQTT bus.
//
// MQTT northbound (publishes):
//
//	lexa/evse/{station}/state   — EVSEState on connect / disconnect / MeterValues
//
// MQTT southbound (subscribes):
//
//	lexa/desired/evse/{station} — retained AD-013 desired-state doc the EVSE
//	    reconciler executes (SetChargingProfile). TASK-032 deleted the legacy
//	    lexa/evse/{station}/command subscription; config must set reconciler
//	    = "active".
//
// Usage:
//
//	lexa-ocpp [-config /etc/lexa/ocpp.json]
package main

import (
	"context"
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

	core16 "github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	remotetrigger16 "github.com/lorenzodonini/ocpp-go/ocpp1.6/remotetrigger"
	ocpp2 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/availability"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/meter"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/provisioning"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/remotecontrol"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/smartcharging"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/transactions"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/types"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/logutil"
	"lexa-hub/internal/metrics"
	"lexa-hub/internal/mqttutil"
	"lexa-hub/internal/watchdog"
	"lexa-proto/ocppserver"
	"lexa-proto/ocppserver16"
)

func main() {
	cfgPath := flag.String("config", "/etc/lexa/ocpp.json", "path to JSON config")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("lexa-ocpp: load config: %v", err)
	}
	logutil.Setup("lexa-ocpp", logutil.ParseLevel(cfg.LogLevel)) // TASK-045

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
	transactionsTotalCtr := reg.Counter("lexa_ocpp_transactions_total")

	// Unit 6.1 (amended, docs/extension/00_PROGRESS.md "Scope amendments"):
	// both gauges are registered here — BEFORE the uncommissioned-idle
	// branch below decides whether the CSMS even starts — so both metric
	// names are always present in /metrics regardless of mode (the
	// "registered-but-zero counter is normal" convention, CLAUDE.md's
	// Metrics section), rather than appearing/disappearing with it.
	pendingGauge := reg.Gauge("lexa_ocpp_pending_stations")
	idleGauge := reg.Gauge("lexa_ocpp_uncommissioned_idle")

	// WS-9.1: tracks continuous-disconnected time so the watchdog kick below
	// can tell "genuinely connected" from "IsConnected() still true only
	// because paho's AutoReconnect is retrying" — see deaf's doc comment.
	deaf := watchdog.NewDeafTracker()
	reg.Collect(func(r *metrics.Registry) {
		r.Gauge("lexa_mqtt_deaf_seconds").Set(deaf.DeafFor(time.Now()).Seconds())
	})

	mqttPass, err := mqttutil.LoadPassword(cfg.MQTTPassFile)
	if err != nil {
		log.Fatalf("lexa-ocpp: %v", err)
	}
	mc, err := mqttutil.ConnectAuthInstrumented(cfg.MQTTBroker, cfg.MQTTClientID, cfg.MQTTUser, mqttPass, mqttutil.Instrumentation{
		OnPublishFail:    mqttFailCtr.Inc,
		OnReconnect:      func() { mqttReconnCtr.Inc(); deaf.OnReconnect() },
		OnConnectionLost: deaf.OnConnectionLost,
	})
	if err != nil {
		log.Fatalf("lexa-ocpp: %v", err)
	}
	defer mc.Disconnect(500)

	// Unit 6.1 (amended): "stations": [] and not bench is a VALID
	// uncommissioned-idle state (docs/DEVICE_ROADMAP.md §6/§9; the factory
	// profile configs/factory/ocpp.json) — loadConfig above already accepts
	// it (config.go's uncommissionedIdle/benchProfile). In that state, skip
	// building/starting the OCPP CSMS entirely: ocppserver.New/newMQTTBridge
	// are never called, so no socket is ever bound. Everything else — MQTT
	// connect (above), metrics, watchdog Ready, and the kick ticker below —
	// still runs unconditionally.
	if uncommissionedIdle(cfg) {
		idleGauge.Set(1)
		log.Printf("lexa-ocpp: uncommissioned idle — no stations configured; CSMS listener not started")
	} else {
		idleGauge.Set(0)

		srv := ocppserver.New(ocppserver.Config{
			Port:          cfg.Port,
			CertPath:      cfg.CertPath,
			KeyPath:       cfg.KeyPath,
			BasicAuthUser: cfg.BasicAuthUser,
			BasicAuthPass: cfg.BasicAuthPass,
		})

		bridge := newMQTTBridge(mc, srv.CSMS(), cfg.Stations, pendingGauge)
		bridge.transactionsTotal = transactionsTotalCtr

		// WP-13/D10: the pairing gate — one instance shared by BOTH stacks'
		// forwarders. Wired before either listener starts (read via its own
		// mutex thereafter). In gated mode, persisted decisions also seed the
		// pending surface's never-track set so an approved/denied station
		// doesn't re-surface as pending after a restart; open mode skips the
		// seeding so the bench's pending-surface behavior is byte-identical.
		stationIDs := make([]string, len(cfg.Stations))
		for i, sc := range cfg.Stations {
			stationIDs[i] = sc.ID
		}
		gate := newPairingGate(cfg.PairingMode, stationIDs, cfg.AllowlistPath,
			reg.Counter("lexa_ocpp_pairing_dropped_total"))
		bridge.gate = gate
		if cfg.PairingMode == PairingGated {
			for _, id := range gate.decidedStations() {
				bridge.pending.neverTrack(id)
			}
			log.Printf("lexa-ocpp: pairing gate GATED (allowlist %s) — unknown stations Boot as Pending until approved via lexa-api", cfg.AllowlistPath)
		}
		if err := mqttutil.Subscribe(mc, bus.TopicOCPPPairing, func(_ string, d bus.PairingDecision) {
			bridge.handlePairingDecision(d, time.Now())
		}); err != nil {
			log.Printf("lexa-ocpp: subscribe pairing decisions: %v", err)
		}

		// Unit 6.1: clear any stale retained lexa/ocpp/pending left over from
		// a prior run (since-approved or long-departed entries) even before
		// anything has connected this time around — see
		// pendingStations.publishStartup's doc.
		bridge.pending.publishStartup(time.Now())

		// lexa_ocpp_connected_stations (TASK-044): the connection-state gauge for
		// this service — computed at scrape time from the bridge's own station
		// map rather than incremented/decremented at each connect/disconnect
		// handler, so it can never drift from the bridge's actual state.
		reg.Collect(func(r *metrics.Registry) {
			r.Gauge("lexa_ocpp_connected_stations").Set(float64(bridge.connectedStationCount()))
		})

		// Pre-register known station limits.
		for _, sc := range cfg.Stations {
			bridge.setStationConfig(sc.ID, sc.MaxCurrentA, sc.VoltageV)
		}

		// TASK-030: one reconciler shell per configured station when the EVSE
		// reconciler is shadow or active. Shadow is a recorder (no SetChargingProfile
		// from the reconciler); active makes it own the profile via the SAME driver
		// (bridge.Apply) and adds reassert-on-reconnect. Legacy commands keep
		// flowing either way (belt and braces for instant rollback).
		evseMode := cfg.ReconcilerMode()
		evseActive := evseMode == ReconcilerActive
		if evseMode == ReconcilerShadow || evseMode == ReconcilerActive {
			mode := modeShadow
			var drv profileDriver
			if evseActive {
				mode = modeActive
				drv = bridge
			}
			shells := make(map[string]*evseShell)
			for _, sc := range cfg.Stations {
				sh := newEVSEShell(sc.ID, reg, mode, drv)
				// WP-13 (B3): the station's rated maximum — a desired current
				// at/above it releases via ClearChargingProfile (see
				// evseShell.releaseLimit). Set before any doc/observe arrives.
				sh.ratedMaxA = sc.MaxCurrentA
				// D8/WP-14: the station's rated voltage, for the setpoint-mode
				// W→A conversion (evseChargeAmpsFromSetpoint).
				sh.ratedVoltageV = sc.VoltageV
				// TASK-031: forward device-level non-convergence to the hub's
				// breach-episode component (active mode only).
				if mode == modeActive {
					// WS-4.5: heal a retained NonConvergedBegin left over from a
					// PREVIOUS process instance of this shell BEFORE this
					// instance's own reconciler starts publishing (sh.pub, right
					// below) — see healStaleRetainedReport's doc.
					healStaleRetainedReport(mc, bus.DesiredClassEVSE, sc.ID, time.Now())
					sh.pub = newReconcileReportPublisher(mc)
				}
				shells[sc.ID] = sh
			}
			bridge.shells = shells
			if err := mqttutil.Subscribe(mc, bus.SubDesired, func(topic string, doc bus.DesiredState) {
				if bus.ClassFromDesiredTopic(topic) != bus.DesiredClassEVSE {
					return
				}
				if sh, ok := shells[bus.DeviceFromDesiredTopic(topic)]; ok {
					sh.setDesired(doc, time.Now())
				}
			}); err != nil {
				log.Printf("lexa-ocpp: subscribe desired (reconciler): %v", err)
			}
			go runEVSEShellTicker(shells, 60*time.Second)
			log.Printf("lexa-ocpp: EVSE reconciler %s mode for %d station(s)", evseMode, len(shells))
		}

		// TASK-032: the legacy lexa/evse/{station}/command subscription was deleted.
		// The EVSE reconciler (above) owns SetChargingProfile via the retained
		// lexa/desired/evse/{station} doc, so config must set reconciler = "active".

		// WP-12: optional OCPP 1.6J compatibility listener — a SECOND
		// ws.WsServer on port_16 (0 = disabled, the product default) whose
		// forwarders feed the SAME bridge state map (bridge16.go); the same
		// cert/key + Basic Auth fields secure it, and the same lifecycle
		// applies (started here, stopped on shutdown; the watchdog ticker
		// below is untouched — process + MQTT stay the liveness definition,
		// OCPP listener health is not probed on either stack). Wired before
		// either listener starts: SetHandlers must precede Start
		// (ocppserver16 doc), and cs16 must be set before any 1.6 station
		// can connect and later take an Apply.
		if cfg.Port16 > 0 {
			srv16 := ocppserver16.New(ocppserver16.Config{
				Port:          cfg.Port16,
				CertPath:      cfg.CertPath,
				KeyPath:       cfg.KeyPath,
				BasicAuthUser: cfg.BasicAuthUser,
				BasicAuthPass: cfg.BasicAuthPass,
			})
			wireOCPP16(bridge, srv16)
			log.Printf("lexa-ocpp: OCPP 1.6J compatibility listener enabled on :%d (2.0.1 stays on :%d)", cfg.Port16, cfg.Port)
			go srv16.Start()
			defer srv16.Stop()
		}

		go srv.Start()
		defer srv.Stop()
	}

	metrics.Serve(cfg.MetricsAddr, reg)

	// sd_notify READY (TASK-008): process + MQTT are up and, when not
	// uncommissioned-idle, the OCPP WS listener goroutine has been started.
	// Weaker liveness than northbound/modbus — see the WatchdogSec comment in
	// lexa-ocpp.service: process + MQTT connectivity, not OCPP listener
	// health (follow-up TASK-044).
	watchdog.Ready()

	// TASK-008: no tight control loop exists here (srv.Start runs the OCPP
	// listener in its own goroutine, when one is started at all) — this
	// ticker is the liveness proxy, gated on MQTT connectivity so a genuinely
	// dead process (not just a disconnected broker) is what trips the
	// watchdog. Unaffected by the Unit 6.1 idle gate: an idle instance with
	// no stations is exactly as alive as one serving chargers from the
	// watchdog's point of view. WS-9.1: mc.IsConnected() alone stays true for
	// the ENTIRE duration of a broker outage as long as paho's AutoReconnect
	// keeps retrying, so it is paired with deaf.DeafFor — once a continuous
	// outage exceeds cfg.MQTTDeafRestartAfter (default 5 min), the kick gate
	// stops firing and systemd's WatchdogSec restarts this service; that is
	// accepted crash-only behavior (AD-011), matching the WatchdogSec comment
	// in systemd/lexa-ocpp.service.
	kick := time.NewTicker(10 * time.Second)
	defer kick.Stop()
	deafRestartAfter := cfg.MQTTDeafRestartAfter()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	for {
		select {
		case <-quit:
			log.Println("lexa-ocpp: shutting down")
			return
		case <-kick.C:
			if mc.IsConnected() && deaf.DeafFor(time.Now()) < deafRestartAfter {
				watchdog.Kick()
			}
		}
	}
}

// f64ptr returns a pointer to v, or nil if v is NaN.
func f64ptr(v float64) *float64 {
	if math.IsNaN(v) {
		return nil
	}
	return &v
}

// ── MQTT ↔ OCPP bridge ───────────────────────────────────────────────────────

type connectorStatus string

const (
	statusAvailable   connectorStatus = "Available"
	statusOccupied    connectorStatus = "Occupied"
	statusFaulted     connectorStatus = "Faulted"
	statusUnavailable connectorStatus = "Unavailable"
)

type connState struct {
	connectorID int
	status      connectorStatus
}

// Station protocol tags (WP-12): which OCPP stack a station last spoke, set at
// boot/connect by the respective listener's handlers. bridge.Apply dispatches
// its SetChargingProfile shape on this tag — the 2.0.1 path for protoOCPP201,
// the TxDefaultProfile/Amperes 1.6 path (bridge16.go) for protoOCPP16.
const (
	protoOCPP16  = "1.6"
	protoOCPP201 = "2.0.1"
)

type stationState struct {
	id        string
	connected bool
	// proto is the station's OCPP stack tag (protoOCPP16/protoOCPP201).
	// Defaults to protoOCPP201 at creation (getOrCreateLocked) — every
	// pre-WP-12 station is 2.0.1 by construction — and is (re)stamped by
	// each stack's connect/boot handlers, so a station that switches stacks
	// across reconnects (e.g. a firmware upgrade) dispatches correctly.
	proto       string
	connectors  map[int]*connState
	currentA    float64
	maxCurrentA float64
	voltageV    float64
	soc         float64
	energyWh    float64
}

// centralSystem201 is the slice of ocpp2.CSMS the bridge needs for
// CSMS-initiated 2.0.1 sends — the 2.0.1 sibling of centralSystem16
// (bridge16.go), narrowed to an interface so tests can record
// SetChargingProfile/ClearChargingProfile/TriggerMessage calls without a
// live WebSocket server. Production always wires the full ocpp2.CSMS
// (newMQTTBridge assigns the csms argument, which satisfies this by
// construction); handler REGISTRATION stays on the concrete csms field.
type centralSystem201 interface {
	SetChargingProfile(clientID string, callback func(*smartcharging.SetChargingProfileResponse, error), evseID int, chargingProfile *types.ChargingProfile, props ...func(request *smartcharging.SetChargingProfileRequest)) error
	ClearChargingProfile(clientID string, callback func(*smartcharging.ClearChargingProfileResponse, error), props ...func(request *smartcharging.ClearChargingProfileRequest)) error
	TriggerMessage(clientID string, callback func(*remotecontrol.TriggerMessageResponse, error), requestedMessage remotecontrol.MessageTrigger, props ...func(request *remotecontrol.TriggerMessageRequest)) error
}

// mqttBridge wraps the OCPP CSMS and publishes EVSE state changes to MQTT.
//
// mu protects stations and all stationState fields. Callers must hold mu
// for any read or write of stations or stationState. publishAll snapshots
// the required state under mu.RLock then publishes outside the lock so
// network I/O never runs while mu is held.
type mqttBridge struct {
	mu       sync.RWMutex
	mc       mqtt.Client
	csms     ocpp2.CSMS
	stations map[string]*stationState

	// cs201 is the 2.0.1 send seam (see centralSystem201). Set once by
	// newMQTTBridge (to csms) before any connection is accepted, then
	// read-only — same no-lock discipline as cs16/shells. Tests may override
	// it with a fake immediately after construction.
	cs201 centralSystem201

	// gate is the WP-13/D10 pairing gate — the single, stack-shared owner of
	// "may this station become plant?". Set once at startup (main), before
	// any listener starts, then only accessed through its own mutex. nil
	// (pre-WP-13 tests) behaves as OPEN mode — every gate method is
	// nil-receiver-safe, so all prior behavior is byte-identical.
	gate *pairingGate

	// transactionsTotal counts OCPP transaction Started events
	// (lexa_ocpp_transactions_total, TASK-044); nil-safe (metrics.Counter's
	// Inc is a no-op on a nil receiver).
	transactionsTotal *metrics.Counter

	// shells is the per-station TASK-030 reconciler map (nil when reconciler is
	// off). The meter/transaction forwarders tap it (after releasing mu) to feed
	// metered current as an Observe, and the connect handler signals reconnects.
	// Set once at startup before any OCPP connection is accepted, then read-only,
	// so it needs no lock of its own.
	shells map[string]*evseShell

	// pending is the Unit 6.1 pending-station surface: chargers seen but not
	// in cfg.Stations. Set once at construction (newMQTTBridge), then only
	// ever accessed through its own mutex — never bridge.mu — so nothing here
	// needs bridge's lock.
	pending *pendingStations

	// cs16 is the OCPP 1.6J send seam (WP-12): the CentralSystem the 1.6
	// apply/trigger paths issue SetChargingProfile/TriggerMessage through.
	// nil when port_16 is 0 (1.6 disabled) — no station can carry the
	// protoOCPP16 tag then, so the 1.6 dispatch branch is unreachable. Set
	// once at startup by wireOCPP16 (bridge16.go), before any connection is
	// accepted, then read-only — same no-lock discipline as shells above.
	cs16 centralSystem16

	// nextTxID16 assigns the CSMS-side transaction IDs 1.6 StartTransaction
	// confirmations carry (in 1.6 the Central System owns transaction IDs —
	// lexa-proto/ocppserver16 CLAUDE.md's session-lifecycle invariant).
	// Accessed atomically from the 1.6 forwarder only.
	nextTxID16 int32

	// dischargeClampMu/lastDischargeClampLog rate-limit Apply's D8/WP-14
	// discharge-clamp WARN (evseDischargeClampLogInterval floor), keyed per
	// station. Kept separate from mu (the station-state lock) since this is
	// purely a logging concern, not station state.
	dischargeClampMu      sync.Mutex
	lastDischargeClampLog map[string]time.Time
}

// connectedStationCount returns how many known stations currently have an
// open OCPP connection (lexa_ocpp_connected_stations, TASK-044). Computed
// fresh from b.stations rather than tracked incrementally, so it can never
// drift from the connect/disconnect handlers' own state.
func (b *mqttBridge) connectedStationCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	n := 0
	for _, s := range b.stations {
		if s.connected {
			n++
		}
	}
	return n
}

// newMQTTBridge builds a bridge and wires every CSMS handler, including the
// pending-station surface (Unit 6.1): configuredStations is cfg.Stations at
// startup (the set newPendingStations must never track as pending), and
// pendingGauge is lexa_ocpp_pending_stations (nil-safe — tests without a
// metrics.Registry pass nil).
func newMQTTBridge(mc mqtt.Client, csms ocpp2.CSMS, configuredStations []StationConfig, pendingGauge *metrics.Gauge) *mqttBridge {
	ids := make([]string, len(configuredStations))
	for i, sc := range configuredStations {
		ids[i] = sc.ID
	}
	b := &mqttBridge{
		mc:                    mc,
		csms:                  csms,
		cs201:                 csms,
		stations:              make(map[string]*stationState),
		lastDischargeClampLog: make(map[string]time.Time),
	}
	b.pending = newPendingStations(ids, func(doc bus.PendingStations) error {
		return mqttutil.PublishJSONRetained(mc, bus.TopicOCPPPending, doc)
	}, pendingGauge)

	csms.SetNewChargingStationHandler(b.onConnect)
	csms.SetChargingStationDisconnectedHandler(b.onDisconnect)
	csms.SetProvisioningHandler(&provisioningForwarder{bridge: b})
	csms.SetAvailabilityHandler(&availForwarder{bridge: b})
	csms.SetMeterHandler(&meterForwarder{bridge: b})
	csms.SetTransactionsHandler(&txForwarder{bridge: b})

	return b
}

// onConnect handles SetNewChargingStationHandler: a new OCPP WebSocket
// connection. Extracted to a named method (rather than an inline closure,
// as before Unit 6.1) so tests can drive it directly with a fake
// ocpp2.ChargingStationConnection, without a live WebSocket handshake.
func (b *mqttBridge) onConnect(cs ocpp2.ChargingStationConnection) {
	// WP-13/D10 pairing gate: a non-approved station in gated mode is NEVER
	// promoted to a stationState (no plant, no lexa/evse/{station}/state) —
	// it is only surfaced on the pending doc so an installer can decide.
	// Open mode (and every pre-WP-13 test, via the nil gate) takes the
	// unchanged path below.
	if !b.gate.allowed(cs.ID()) {
		log.Printf("[ocpp] connected (pairing-gated, not adopted): %s addr=%s", cs.ID(), cs.RemoteAddr())
		b.pending.upsert(cs.ID(), "", "", cs.RemoteAddr().String(), time.Now())
		return
	}
	b.mu.Lock()
	s, created := b.getOrCreateLocked(cs.ID())
	s.connected = true
	s.proto = protoOCPP201 // WP-12: (re)stamp at connect; see stationState.proto
	b.mu.Unlock()
	log.Printf("[ocpp] connected: %s addr=%s", cs.ID(), cs.RemoteAddr())
	// Unit 6.1: a station this bridge did not already know about is, by
	// construction, not in cfg.Stations (every configured station is
	// pre-registered via setStationConfig before any connection is
	// accepted) — surface it on the pending-station bus doc instead of
	// silently adopting it. Behavior of the station itself is unchanged:
	// it is still tracked below exactly as before, still gets its
	// measurements published, and gets no shell/driver either way.
	if created {
		b.pending.upsert(cs.ID(), "", "", cs.RemoteAddr().String(), time.Now())
	}
	// TASK-030: reassert-on-reconnect — signal the reconciler (outside mu) so
	// a charger that dropped and returned gets its standing current limit
	// re-sent immediately instead of waiting on the hub's 60 s watchdog. This
	// is the gap the legacy path never closed (it only publishes state here).
	if sh, ok := b.shells[cs.ID()]; ok {
		sh.markReconnected()
	}
	b.publishAll(cs.ID())
	go b.triggerStatusNotification(cs.ID())
}

// onDisconnect handles SetChargingStationDisconnectedHandler.
func (b *mqttBridge) onDisconnect(cs ocpp2.ChargingStationConnection) {
	b.mu.Lock()
	if s, ok := b.stations[cs.ID()]; ok {
		s.connected = false
	}
	b.mu.Unlock()
	log.Printf("[ocpp] disconnected: %s", cs.ID())
	b.publishAll(cs.ID())
}

// getOrCreateLocked returns the stationState for id, creating it if absent,
// and reports whether this call created a NEW entry. Since every station in
// cfg.Stations is pre-registered via setStationConfig before any OCPP
// connection is accepted, "created" here is equivalent to "not in
// cfg.Stations" — the signal the pending-station surface (Unit 6.1) upserts
// on. Caller must hold mu for writing.
func (b *mqttBridge) getOrCreateLocked(id string) (*stationState, bool) {
	if s, ok := b.stations[id]; ok {
		return s, false
	}
	s := &stationState{
		id:          id,
		proto:       protoOCPP201, // WP-12 default; 1.6 handlers re-stamp (bridge16.go)
		connectors:  make(map[int]*connState),
		maxCurrentA: 32,
		voltageV:    230,
		soc:         math.NaN(),
	}
	b.stations[id] = s
	return s, true
}

func (b *mqttBridge) setStationConfig(id string, maxCurrentA, voltageV float64) {
	b.mu.Lock()
	s, _ := b.getOrCreateLocked(id)
	s.maxCurrentA = maxCurrentA
	s.voltageV = voltageV
	b.mu.Unlock()
}

func (b *mqttBridge) publishAll(stationID string) {
	// Snapshot state under the read lock so we never hold the lock
	// during network I/O.
	b.mu.RLock()
	s, ok := b.stations[stationID]
	if !ok {
		b.mu.RUnlock()
		return
	}
	now := time.Now().Unix()
	var msgs []bus.EVSEState
	if len(s.connectors) == 0 {
		msg := bus.EVSEState{
			Envelope:    bus.Envelope{V: bus.EVSEStateV},
			StationID:   s.id,
			ConnectorID: 0,
			Connected:   s.connected,
			MaxCurrentA: f64ptr(s.maxCurrentA),
			VoltageV:    f64ptr(s.voltageV),
			Status:      string(statusAvailable),
			Ts:          now,
		}
		if !math.IsNaN(s.soc) {
			soc := s.soc
			msg.SOC = &soc
		}
		msgs = append(msgs, msg)
	} else {
		for _, c := range s.connectors {
			active := c.status == statusOccupied
			var powerW float64
			if active {
				powerW = s.currentA * s.voltageV
			}
			msg := bus.EVSEState{
				Envelope:      bus.Envelope{V: bus.EVSEStateV},
				StationID:     s.id,
				ConnectorID:   c.connectorID,
				Connected:     s.connected,
				SessionActive: active,
				CurrentA:      f64ptr(s.currentA),
				MaxCurrentA:   f64ptr(s.maxCurrentA),
				VoltageV:      f64ptr(s.voltageV),
				PowerW:        f64ptr(powerW),
				EnergyWh:      f64ptr(s.energyWh),
				Status:        string(c.status),
				Ts:            now,
			}
			if !math.IsNaN(s.soc) {
				soc := s.soc
				msg.SOC = &soc
			}
			msgs = append(msgs, msg)
		}
	}
	b.mu.RUnlock()

	// EVSE state is QoS 0 (bus.PubQoS): part of the measurement plane, same
	// freshness-gated best-effort policy as lexa/measurements and
	// lexa/battery/*/metrics (review D5).
	topic := bus.EVSEStateTopic(stationID)
	qos := bus.PubQoS(topic)
	for _, msg := range msgs {
		_ = mqttutil.PublishJSONQoS(b.mc, topic, qos, msg)
	}
}

// observeShell feeds one folded metered-current sample to the station's
// reconciler shell, if any. Called from the meter/transaction forwarders AFTER
// bridge.mu is released (the shell's own apply path takes bridge.mu). No-op when
// the reconciler is off or the station has no shell.
func (b *mqttBridge) observeShell(stationID string, currentA, maxA float64, connected bool) {
	sh, ok := b.shells[stationID]
	if !ok {
		return
	}
	sh.observe(currentA, !implausibleCurrent(currentA, maxA), connected, time.Now())
}

// Apply implements profileDriver (TASK-030): send a TxDefaultProfile capping the
// station's connector at limitA. Shared verbatim by the legacy path and the
// reconciler shell. A disconnected station is a silent no-op (nil) — the charger
// is not present to command, and the reconnect reassert re-sends when it returns
// (matching the legacy path's prior behavior). A delivered-but-REJECTED profile
// is an error, not success (ledger L11): the charger kept its previous limit.
// Each call is bounded at 10 s (OCPP timeout); an active-mode backoff must be ≥
// that so calls to one station never overlap.
//
// WP-12: Apply dispatches per station proto tag — a station stamped
// protoOCPP16 takes the 1.6 SetChargingProfile shape (apply16, bridge16.go)
// under the exact same contract (disconnected no-op, evseID 0→1, 10 s bound,
// rejected-as-error); everything else takes the 2.0.1 path below unchanged.
//
// D8/WP-14: setpointW is non-nil only in setpoint mode (evseShell.setDesired
// folded a desired doc's SetpointW into the reconciler's own MaxCurrentA
// tracking already, but that conversion clamps discharge silently for the
// core's bookkeeping — see that function's doc). THIS is the one seam
// (greppable: "discharge setpoint") where the actual write is derived and
// logged: setpointW overrides limitA entirely, converting W→A at the
// station's voltage and clamping a genuine discharge (negative A) to 0 A
// suspend with a rate-limited WARN — actuation stays charge-only until a
// V2X hardware path exists, even though the type system no longer is.
func (b *mqttBridge) Apply(stationID string, evseID int, limitA float64, setpointW *float64) error {
	b.mu.RLock()
	s, ok := b.stations[stationID]
	connected := ok && s.connected
	var proto string
	var voltageV float64
	if ok {
		proto = s.proto
		voltageV = s.voltageV
	}
	b.mu.RUnlock()

	if setpointW != nil {
		amps := evseChargeAmpsFromSetpoint(*setpointW, voltageV)
		if amps < 0 {
			b.logDischargeClamp(stationID, *setpointW)
			amps = 0
		}
		limitA = amps
	}

	if !connected {
		return nil
	}
	if evseID == 0 {
		evseID = 1
	}
	if proto == protoOCPP16 {
		return b.apply16(stationID, evseID, limitA)
	}
	limit := limitA
	period := types.NewChargingSchedulePeriod(0, limit)
	schedule := types.NewChargingSchedule(1, types.ChargingRateUnitAmperes, period)
	profile := types.NewChargingProfile(
		1, 0,
		types.ChargingProfilePurposeTxDefaultProfile,
		types.ChargingProfileKindAbsolute,
		[]types.ChargingSchedule{*schedule},
	)
	type spResult struct {
		status smartcharging.ChargingProfileStatus
		err    error
	}
	resCh := make(chan spResult, 1)
	callErr := b.cs201.SetChargingProfile(
		stationID,
		func(resp *smartcharging.SetChargingProfileResponse, err error) {
			r := spResult{err: err}
			if resp != nil {
				r.status = resp.Status
			}
			resCh <- r
		},
		evseID, profile,
	)
	if callErr != nil {
		return fmt.Errorf("SetChargingProfile %s evse=%d call failed: %w", stationID, evseID, callErr)
	}
	t := time.NewTimer(10 * time.Second)
	defer t.Stop()
	select {
	case r := <-resCh:
		if r.err != nil {
			return fmt.Errorf("SetChargingProfile %s evse=%d failed: %w", stationID, evseID, r.err)
		}
		// A delivered-but-rejected profile is a failure, not success: the EVSE
		// kept its previous limit. Surface it instead of assuming convergence.
		if r.status != smartcharging.ChargingProfileStatusAccepted {
			return fmt.Errorf("SetChargingProfile %s evse=%d rejected: status=%q", stationID, evseID, r.status)
		}
		return nil
	case <-t.C:
		return fmt.Errorf("SetChargingProfile %s evse=%d timed out after 10s", stationID, evseID)
	}
}

// evseDischargeClampLogInterval rate-limits Apply's discharge-clamp WARN —
// mirrors this repo's 10 s floor convention for edge-triggered alarms (e.g.
// cmd/hub's rewalkRateLimit) so a station stuck requesting a discharge
// setpoint every tick can't flood the journal (flash-budget discipline,
// CLAUDE.md).
const evseDischargeClampLogInterval = 10 * time.Second

// evseChargeAmpsFromSetpoint converts a signed EV power setpoint (W, battery
// sign convention: + discharge to site, − charge — matches
// orchestrator.EVSECommand.SetpointW/bus.DesiredState.SetpointW, D8/WP-14) to
// the equivalent CHARGING current (A) at voltageV. Pure arithmetic, no
// clamping: a charge setpoint (negative W) yields a positive amp value,
// flowing through exactly like today's MaxCurrentA limits; a discharge
// setpoint (positive W) yields a NEGATIVE amp value here, which has no
// SetChargingProfile equivalent — callers that actually write to hardware
// must clamp it (Apply, the one seam that does).
func evseChargeAmpsFromSetpoint(setpointW, voltageV float64) float64 {
	if voltageV <= 0 {
		voltageV = 230 // this package's station default (config.go)
	}
	return -setpointW / voltageV
}

// logDischargeClamp is the rate-limited WARN for a discharge setpoint
// clamped to 0 A suspend (D8/WP-14) — see Apply's doc.
func (b *mqttBridge) logDischargeClamp(stationID string, setpointW float64) {
	b.dischargeClampMu.Lock()
	now := time.Now()
	if last, ok := b.lastDischargeClampLog[stationID]; ok && now.Sub(last) < evseDischargeClampLogInterval {
		b.dischargeClampMu.Unlock()
		return
	}
	if b.lastDischargeClampLog == nil {
		b.lastDischargeClampLog = make(map[string]time.Time)
	}
	b.lastDischargeClampLog[stationID] = now
	b.dischargeClampMu.Unlock()
	log.Printf("lexa-ocpp: %s: discharge setpoint %.0fW clamped to 0A suspend (actuation is charge-only — D8/WP-14, no V2X hardware path yet)",
		stationID, setpointW)
}

func (b *mqttBridge) triggerStatusNotification(stationID string) {
	time.Sleep(500 * time.Millisecond)
	_ = b.cs201.TriggerMessage(
		stationID,
		func(_ *remotecontrol.TriggerMessageResponse, _ error) {},
		remotecontrol.MessageTriggerStatusNotification,
	)
}

// ApplyClear implements profileDriver's release path (WP-13, B3): remove the
// CSMS's standing TxDefaultProfile from the station's connector with an OCPP
// ClearChargingProfile, instead of re-setting a large numeric limit. Same
// per-proto dispatch and contract as Apply: a disconnected station is a
// silent no-op (the reconnect reassert re-issues the standing desired doc,
// which re-derives the release), evseID 0 → 1, each call bounded at 10 s.
//
// Response handling: Accepted is success. UNKNOWN — the only other status
// either stack's vocabulary defines, "no profile matched the clear criteria"
// — is ALSO success: a charger already carrying no CSMS profile IS the
// released state, and treating it as an error would retry a Clear forever
// against a charger with nothing to clear. Any other (nonconforming) status,
// a transport error, or a timeout is an error, L11-style — the release did
// not verifiably take, so the reconcile core retries on its backoff.
// Convergence after a successful Clear is trivial under the one-sided
// metered-current rule (release has no measurable target; any plausible
// under-limit sample converges) — see bus.RestoreCurrentA's doc.
func (b *mqttBridge) ApplyClear(stationID string, evseID int) error {
	b.mu.RLock()
	s, ok := b.stations[stationID]
	connected := ok && s.connected
	var proto string
	if ok {
		proto = s.proto
	}
	b.mu.RUnlock()
	if !connected {
		return nil
	}
	if evseID == 0 {
		evseID = 1
	}
	if proto == protoOCPP16 {
		return b.applyClear16(stationID, evseID)
	}
	type ccpResult struct {
		status smartcharging.ClearChargingProfileStatus
		err    error
	}
	resCh := make(chan ccpResult, 1)
	callErr := b.cs201.ClearChargingProfile(
		stationID,
		func(resp *smartcharging.ClearChargingProfileResponse, err error) {
			r := ccpResult{err: err}
			if resp != nil {
				r.status = resp.Status
			}
			resCh <- r
		},
		func(req *smartcharging.ClearChargingProfileRequest) {
			// Clear exactly what Apply installs: the TxDefaultProfile on this
			// EVSE — never a blanket clear of profiles some other authority
			// may have placed.
			id := evseID
			req.ChargingProfileCriteria = &smartcharging.ClearChargingProfileType{
				EvseID:                 &id,
				ChargingProfilePurpose: types.ChargingProfilePurposeTxDefaultProfile,
			}
		},
	)
	if callErr != nil {
		return fmt.Errorf("ClearChargingProfile %s evse=%d call failed: %w", stationID, evseID, callErr)
	}
	t := time.NewTimer(10 * time.Second)
	defer t.Stop()
	select {
	case r := <-resCh:
		if r.err != nil {
			return fmt.Errorf("ClearChargingProfile %s evse=%d failed: %w", stationID, evseID, r.err)
		}
		if r.status != smartcharging.ClearChargingProfileStatusAccepted &&
			r.status != smartcharging.ClearChargingProfileStatusUnknown {
			return fmt.Errorf("ClearChargingProfile %s evse=%d rejected: status=%q", stationID, evseID, r.status)
		}
		return nil
	case <-t.C:
		return fmt.Errorf("ClearChargingProfile %s evse=%d timed out after 10s", stationID, evseID)
	}
}

// handlePairingDecision consumes one bus.PairingDecision edge from
// lexa/ocpp/pairing (WP-13): apply it to the gate (validate + persist),
// resolve the station off the pending surface, and — on an approval — nudge
// the (possibly still-connected, Pending-registered) station to re-send its
// BootNotification immediately instead of waiting out its 60 s Boot-retry
// interval. A malformed decision is rejected loudly and changes nothing.
func (b *mqttBridge) handlePairingDecision(d bus.PairingDecision, now time.Time) {
	if err := b.gate.applyDecision(d); err != nil {
		log.Printf("[ocpp] REJECT pairing decision station=%q action=%q: %v", d.StationID, d.Action, err)
		return
	}
	log.Printf("[ocpp] pairing decision applied: station=%s action=%s actor=%s", d.StationID, d.Action, d.Actor)
	b.pending.resolve(d.StationID, now)
	if d.Action == bus.PairingActionApprove {
		b.nudgeBootNotification(d.StationID)
	}
}

// nudgeBootNotification fire-and-forgets a TriggerMessage(BootNotification)
// at stationID on BOTH stacks (a gated-pending station was never promoted to
// a stationState, so its proto tag is unknown; the stack it is not connected
// on errors harmlessly inside the library and is ignored). Best-effort by
// design — a station that misses the nudge re-Boots on its Pending-interval
// cadence anyway.
func (b *mqttBridge) nudgeBootNotification(stationID string) {
	go func() {
		_ = b.cs201.TriggerMessage(
			stationID,
			func(_ *remotecontrol.TriggerMessageResponse, _ error) {},
			remotecontrol.MessageTriggerBootNotification,
		)
		if b.cs16 != nil {
			_ = b.cs16.TriggerMessage(
				stationID,
				func(_ *remotetrigger16.TriggerMessageConfirmation, _ error) {},
				remotetrigger16.MessageTrigger(core16.BootNotificationFeatureName),
			)
		}
	}()
}

// ── OCPP handler forwarders ───────────────────────────────────────────────────

// provisioningForwarder overrides lexa-proto/ocppserver's default provisioning
// handler (which only logs) so BootNotification's vendor/model reach the
// pending-station surface (Unit 6.1). Registered via
// csms.SetProvisioningHandler in newMQTTBridge, exactly like
// availForwarder/meterForwarder/txForwarder below override their defaults.
type provisioningForwarder struct{ bridge *mqttBridge }

// OnBootNotification captures req.ChargingStation.VendorName/Model — the one
// place this information is ever offered by the OCPP protocol — and feeds it
// into the pending-station upsert (a no-op merge for an already-configured
// station; see pendingStations.upsert). Response shape mirrors
// lexa-proto/ocppserver/handlers.go's default handler (60s heartbeat
// interval); WP-13/D10 makes the RegistrationStatus the pairing gate's
// verdict — Accepted for configured/approved stations (and everything in
// open mode, today's behavior unchanged), Pending for an unknown station in
// gated mode (the protocol-sanctioned holding state; the 60 s interval is
// its Boot-retry cadence, so an approval takes effect within a minute even
// without the TriggerMessage nudge), Rejected for a persisted deny.
//
// An ACCEPTED boot also ensures the stationState exists and is marked
// connected (the request just arrived on a live socket by construction) —
// on a normal connect that is a no-op after onConnect, but it is what
// promotes a just-approved station whose onConnect ran while it was still
// gated (mirroring the 1.6 forwarder's getOrCreate16Locked-at-boot).
func (h *provisioningForwarder) OnBootNotification(csID string, req *provisioning.BootNotificationRequest) (*provisioning.BootNotificationResponse, error) {
	h.bridge.pending.upsert(csID, req.ChargingStation.VendorName, req.ChargingStation.Model, "", time.Now())
	status := provisioning.RegistrationStatusAccepted
	switch h.bridge.gate.verdict(csID) {
	case bootPending:
		status = provisioning.RegistrationStatusPending
	case bootReject:
		status = provisioning.RegistrationStatusRejected
	default:
		h.bridge.mu.Lock()
		s, created := h.bridge.getOrCreateLocked(csID)
		if created {
			// Only the just-approved case (onConnect ran while still gated,
			// so no stationState exists yet) takes this branch — a normal
			// boot after onConnect leaves state and the publish stream
			// byte-identical to pre-WP-13.
			s.connected = true
			h.bridge.mu.Unlock()
			h.bridge.publishAll(csID)
		} else {
			h.bridge.mu.Unlock()
		}
	}
	log.Printf("[ocpp] BootNotification cs=%s reason=%s model=%s vendor=%s status=%s",
		csID, req.Reason, req.ChargingStation.Model, req.ChargingStation.VendorName, status)
	resp := provisioning.NewBootNotificationResponse(
		types.NewDateTime(time.Now()),
		60, // heartbeat interval in seconds
		status,
	)
	return resp, nil
}

func (h *provisioningForwarder) OnNotifyReport(csID string, req *provisioning.NotifyReportRequest) (*provisioning.NotifyReportResponse, error) {
	log.Printf("[ocpp] NotifyReport cs=%s requestId=%d seqNo=%d", csID, req.RequestID, req.SeqNo)
	return &provisioning.NotifyReportResponse{}, nil
}

type availForwarder struct{ bridge *mqttBridge }

func (h *availForwarder) OnHeartbeat(csID string, _ *availability.HeartbeatRequest) (*availability.HeartbeatResponse, error) {
	now := types.NewDateTime(time.Now())
	return availability.NewHeartbeatResponse(*now), nil
}

func (h *availForwarder) OnStatusNotification(csID string, req *availability.StatusNotificationRequest) (*availability.StatusNotificationResponse, error) {
	// WP-13/D10: a StatusNotification from a non-approved station is RECORDED
	// on the pending surface (LastSeen refresh — the installer sees a live,
	// talking charger) but never folded into plant state.
	if !h.bridge.gate.allowed(csID) {
		h.bridge.pending.upsert(csID, "", "", "", time.Now())
		return &availability.StatusNotificationResponse{}, nil
	}
	status := connectorStatus(req.ConnectorStatus)
	h.bridge.mu.Lock()
	s, created := h.bridge.getOrCreateLocked(csID)
	s.connectors[req.ConnectorID] = &connState{connectorID: req.ConnectorID, status: status}
	h.bridge.mu.Unlock()
	// Unit 6.1: the "OnStatusNotification twin" of onConnect's auto-create
	// path (docs/DEVICE_ROADMAP.md §6) — defensive symmetry for the same
	// getOrCreateLocked auto-adoption, in case a StatusNotification is ever
	// the first this bridge hears of a station.
	if created {
		h.bridge.pending.upsert(csID, "", "", "", time.Now())
	}
	log.Printf("[ocpp] StatusNotification cs=%s connector=%d status=%s", csID, req.ConnectorID, status)
	h.bridge.publishAll(csID)
	return &availability.StatusNotificationResponse{}, nil
}

type meterForwarder struct{ bridge *mqttBridge }

// applySamplesLocked folds OCPP sampled values into the station state.
// Caller must hold bridge.mu for writing.
// evCurrentTolerance bounds a MeterValues current against the station's rated
// max. A real charging current cannot exceed the hardware rating by more than
// transient/measurement slack; a value beyond this is a mislabeled or corrupt
// reading (audit: ev-wrong-units — mA reported under an "A" label is ≈1000× the
// truth) and must not be ingested, or the optimizer plans against a fabricated
// site load.
const evCurrentTolerance = 1.25

// implausibleCurrent reports whether measuredA cannot be a real charging current
// for a station rated at maxA. A non-finite value is always implausible; an
// unknown rating (maxA ≤ 0) cannot be judged, so it is accepted.
func implausibleCurrent(measuredA, maxA float64) bool {
	if math.IsNaN(measuredA) || math.IsInf(measuredA, 0) {
		return true
	}
	if maxA <= 0 {
		return false
	}
	return math.Abs(measuredA) > maxA*evCurrentTolerance
}

func applySamplesLocked(s *stationState, meterValues []types.MeterValue) {
	for _, mv := range meterValues {
		for _, sv := range mv.SampledValue {
			v := sv.Value
			switch sv.Measurand {
			case types.MeasurandCurrentImport:
				if implausibleCurrent(v, s.maxCurrentA) {
					log.Printf("[ocpp] REJECT implausible MeterValues current %.1fA on %s (station max %.1fA) — keeping last good %.1fA",
						v, s.id, s.maxCurrentA, s.currentA)
					continue
				}
				s.currentA = v
			case types.MeasurandSoC:
				s.soc = v
			case types.MeasurandEnergyActiveImportRegister:
				mult := 0
				if sv.UnitOfMeasure != nil && sv.UnitOfMeasure.Multiplier != nil {
					mult = *sv.UnitOfMeasure.Multiplier
				}
				if mult != 0 {
					v *= math.Pow10(mult)
				}
				s.energyWh = v
			case types.MeasurandVoltage:
				if v > 0 {
					s.voltageV = v
				}
			}
		}
	}
}

func (h *meterForwarder) OnMeterValues(csID string, req *meter.MeterValuesRequest) (*meter.MeterValuesResponse, error) {
	// WP-13/D10: meter data from a non-approved station is dropped (edge
	// log + counter) — never folded, never published as plant telemetry.
	if !h.bridge.gate.permitOrLogDrop(csID, "MeterValues") {
		return meter.NewMeterValuesResponse(), nil
	}
	h.bridge.mu.Lock()
	s, _ := h.bridge.getOrCreateLocked(csID)
	applySamplesLocked(s, req.MeterValue)
	currentA, soc, energyWh := s.currentA, s.soc, s.energyWh
	connected, maxA := s.connected, s.maxCurrentA
	h.bridge.mu.Unlock()
	// TASK-030: feed the folded metered current to the reconciler (outside mu, so
	// the shell's apply path — which takes bridge.mu — never inverts lock order).
	// currentA is post-L11 (applySamplesLocked already rejected any implausible
	// sample and kept last-good), so it is by construction plausible.
	h.bridge.observeShell(csID, currentA, maxA, connected)
	// TASK-045 per-tick demotion: bare MeterValues arrives on every sample
	// (~10 s during an active session) — steady-state, not a transition (the
	// invariant per CLAUDE.md is the TransactionEvent lifecycle, not this).
	slog.Debug("[ocpp] MeterValues", "cs", csID, "evse", req.EvseID,
		"current_a", currentA, "soc_pct", soc, "energy_wh", energyWh)
	h.bridge.publishAll(csID)
	return meter.NewMeterValuesResponse(), nil
}

type txForwarder struct{ bridge *mqttBridge }

// OnTransactionEvent consumes the spec-correct carrier for in-transaction
// meter data (the station also sends legacy bare MeterValues during the
// transition). Ended events zero the current so site power drops immediately
// instead of holding the last sample.
func (h *txForwarder) OnTransactionEvent(csID string, req *transactions.TransactionEventRequest) (*transactions.TransactionEventResponse, error) {
	// WP-13/D10: no transactions folded from a non-approved station (edge
	// log + counter) — the session neither counts nor reaches the bus.
	if !h.bridge.gate.permitOrLogDrop(csID, "TransactionEvent") {
		return transactions.NewTransactionEventResponse(), nil
	}
	if req.EventType == transactions.TransactionEventStarted {
		h.bridge.transactionsTotal.Inc() // lexa_ocpp_transactions_total (TASK-044)
	}
	h.bridge.mu.Lock()
	s, _ := h.bridge.getOrCreateLocked(csID)
	applySamplesLocked(s, req.MeterValue)
	if req.EventType == transactions.TransactionEventEnded {
		s.currentA = 0
	}
	currentA, soc, energyWh := s.currentA, s.soc, s.energyWh
	connected, maxA := s.connected, s.maxCurrentA
	h.bridge.mu.Unlock()
	// TASK-030: feed the reconciler (outside mu). Ended has already zeroed
	// currentA above, so the 0 A suspend case converges via this same path.
	h.bridge.observeShell(csID, currentA, maxA, connected)
	// TASK-045: Started/Ended are real session lifecycle edges (CLAUDE.md's
	// OCPP-1 invariant) and stay at Info; Updated repeats through the whole
	// session (often on a periodic MeterValuePeriodic trigger) and is
	// steady-state, so it is demoted to Debug — the per-tick audit's other
	// OCPP demotion (bare MeterValues, above).
	level := slog.LevelInfo
	if req.EventType == transactions.TransactionEventUpdated {
		level = slog.LevelDebug
	}
	slog.Log(context.Background(), level, "[ocpp] TransactionEvent",
		"cs", csID, "type", req.EventType, "tx", req.TransactionInfo.TransactionID,
		"seq", req.SequenceNo, "trigger", req.TriggerReason,
		"state", req.TransactionInfo.ChargingState,
		"current_a", currentA, "soc_pct", soc, "energy_wh", energyWh)
	h.bridge.publishAll(csID)
	return transactions.NewTransactionEventResponse(), nil
}
