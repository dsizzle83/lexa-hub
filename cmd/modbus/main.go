// lexa-modbus polls SunSpec/Modbus devices and bridges them to the MQTT bus.
//
// Northbound (MQTT):
//   - Publishes lexa/measurements/{device} on every poll.
//   - Publishes lexa/battery/{device}/metrics for battery-role devices.
//   - Subscribes lexa/desired/{battery,solar}/{device}: the device reconciler
//     executes the retained desired-state doc (AD-013). TASK-032 deleted the
//     legacy lexa/control/* command subscriptions; the reconciler is now the
//     sole write path (config must set reconciler battery/solar = "active").
//
// Southbound (Modbus):
//   - Supports roles: "inverter" (SunSpec model 103), "battery" (model 802),
//     "meter" (model 203).
//
// Usage:
//
//	lexa-modbus [-config /etc/lexa/modbus.json]
package main

import (
	"errors"
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

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/logutil"
	"lexa-hub/internal/metrics"
	"lexa-hub/internal/mqttutil"
	"lexa-hub/internal/reconcile"
	"lexa-hub/internal/southbound/battery"
	"lexa-hub/internal/southbound/device"
	"lexa-hub/internal/southbound/inverter"
	"lexa-hub/internal/southbound/meter"
	"lexa-hub/internal/southbound/registry"
	"lexa-hub/internal/watchdog"
	model "lexa-proto/csipmodel"
)

func main() {
	cfgPath := flag.String("config", "/etc/lexa/modbus.json", "path to JSON config")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("lexa-modbus: load config: %v", err)
	}
	logutil.Setup("lexa-modbus", logutil.ParseLevel(cfg.LogLevel)) // TASK-045

	// TASK-044: metrics registry (named mreg — "reg" below is already the
	// southbound device registry) + standard process gauges, wired before
	// the MQTT connect below so its instrumentation hooks have counters ready.
	mreg := metrics.New()
	metrics.StandardGauges(mreg)
	mqttFailCtr := mreg.Counter("lexa_mqtt_publish_failures_total")
	mqttReconnCtr := mreg.Counter("lexa_mqtt_reconnects_total")
	mreg.Collect(func(r *metrics.Registry) {
		var total uint64
		for _, n := range bus.VersionRejects() {
			total += n
		}
		for _, n := range bus.DecodeFailures() {
			total += n
		}
		r.Counter("lexa_bus_decode_failures_total").Set(total)
	})
	pollDurationGauge := mreg.Gauge("lexa_mb_poll_duration_seconds")
	deviceReconnectsCtr := mreg.Counter("lexa_mb_device_reconnects_total")
	writeFailuresCtr := mreg.Counter("lexa_mb_write_failures_total")
	interlockTripsCtr := mreg.Counter("lexa_mb_interlock_trips_total")

	mqttPass, err := mqttutil.LoadPassword(cfg.MQTTPassFile)
	if err != nil {
		log.Fatalf("lexa-modbus: %v", err)
	}
	mc, err := mqttutil.ConnectAuthInstrumented(cfg.MQTTBroker, cfg.MQTTClientID, cfg.MQTTUser, mqttPass, mqttutil.Instrumentation{
		OnPublishFail: mqttFailCtr.Inc,
		OnReconnect:   mqttReconnCtr.Inc,
	})
	if err != nil {
		log.Fatalf("lexa-modbus: %v", err)
	}
	defer mc.Disconnect(500)

	reg := registry.New(cfg.PollInterval())
	log.Printf("lexa-modbus: poll interval=%s (measurement-freshness ceiling; parallel per-device poll)", cfg.PollInterval())

	// Tier-0 edge safety interlock (ADR-0001): a local reflex that force-disconnects
	// a pack discharging below its reserve while commanded to charge, within one poll
	// and independent of the hub/broker.
	interlock := newBatterySafetyInterlock(reg, cfg)
	interlock.trips = interlockTripsCtr

	// Open each device; log failures but continue so other devices still poll.
	// retryDevices is kept so the active battery reconciler can attach its
	// reconnect hook to the exact wrapper the registry polls (TASK-028).
	retryDevices := map[string]*retryDevice{}
	for _, dc := range cfg.Devices {
		dev, err := openDevice(dc)
		if errors.Is(err, errUnknownRole) {
			log.Printf("lexa-modbus: device %s: %v — skipping", dc.Name, err)
			continue
		}
		// Wrap EVERY device so a mid-session drop reconnects, not just ones that
		// failed the initial open. A device that opened cleanly and later lost its
		// session would otherwise error forever.
		rd := &retryDevice{cfg: dc, pollDuration: pollDurationGauge, reconnects: deviceReconnectsCtr, writeFailures: writeFailuresCtr}
		if err != nil {
			log.Printf("lexa-modbus: device %s (%s): %v — will reconnect on next poll", dc.Name, dc.URL, err)
		} else {
			rd.live = dev
		}
		reg.Add(&registry.Entry{Name: dc.Name, Addr: dc.URL, Device: rd})
		retryDevices[dc.Name] = rd
		log.Printf("lexa-modbus: registered device %s role=%s url=%s", dc.Name, dc.Role, dc.URL)
	}

	// TASK-027/028: one reconciler shell per battery-role device when the
	// battery reconciler is in shadow or active mode. SHADOW is a recorder (no
	// hardware writes — see reconcile_shell.go's grep-proof no-write-path claim
	// for shadow mode). ACTIVE gives the reconciler write authority through the
	// SAME registry path legacy used, with Tier-0 interlock seniority, and makes
	// the reconciler the single reasserter-on-reconnect (retryDevice's lastCtrl
	// reassert suppressed for the device). Legacy battery commands keep flowing
	// either way (belt and braces for instant rollback).
	var battShells map[string]*batteryShell
	battMode := cfg.ReconcilerMode("battery")
	batteryActive := battMode == ReconcilerActive
	if battMode == ReconcilerShadow || battMode == ReconcilerActive {
		mode := modeShadow
		if batteryActive {
			mode = modeActive
		}
		battShells = make(map[string]*batteryShell)
		for _, dc := range cfg.Devices {
			if dc.Role != "battery" {
				continue
			}
			var drv reconcileDriver
			var ilg interlockGate
			var note func(bus.BattCommand)
			if mode == modeActive {
				name := dc.Name
				drv = registryDriver{reg: reg, dev: name}
				ilg = interlock
				note = func(cmd bus.BattCommand) { interlock.noteControl(name, cmd) }
			}
			shell := newBatteryShell(dc.Name, reconcile.Config{}, mreg, mode, drv, ilg, note)
			// TASK-031: forward device-level non-convergence to the hub's
			// breach-episode component (active mode only — shadow shells never
			// drive CannotComply).
			if mode == modeActive {
				shell.pub = newReconcileReportPublisher(mc)
			}
			battShells[dc.Name] = shell
			// Active: route reconnects to the reconciler (the SINGLE reasserter)
			// and suppress retryDevice's own lastCtrl reassert for this device —
			// avoiding the double-write race the task warns of (ledger L4).
			if mode == modeActive {
				if rd := retryDevices[dc.Name]; rd != nil {
					rd.reconciledActive = true
					rd.onReconnect = shell.markReconnected
				}
			}
		}
		go runBatteryShellTicker(battShells, 60*time.Second)
		log.Printf("lexa-modbus: battery reconciler %s mode active for %d device(s)", battMode, len(battShells))
	}

	// TASK-029: one solarShell per inverter-role device when the solar
	// reconciler is shadow or active. Shadow is a recorder; active gives the
	// reconciler write authority through the SAME registry path legacy solar
	// used, makes it the single reasserter-on-reconnect (retryDevice's
	// reassertLocked inverter branch suppressed via reconciledActive), and seeds
	// each inverter's initial standing desired to the restore ceiling
	// (Background case 3 — never both the seed and reassertLocked). Legacy solar
	// commands keep flowing either way (belt and braces).
	var solarShells map[string]*solarShell
	solarMode := cfg.ReconcilerMode("solar")
	solarActive := solarMode == ReconcilerActive
	if solarMode == ReconcilerShadow || solarMode == ReconcilerActive {
		mode := modeShadow
		if solarActive {
			mode = modeActive
		}
		solarShells = make(map[string]*solarShell)
		now := time.Now()
		for _, dc := range cfg.Devices {
			if dc.Role != "inverter" {
				continue
			}
			var drv reconcileDriver
			if mode == modeActive {
				drv = registryDriver{reg: reg, dev: dc.Name}
			}
			shell := newSolarShell(dc.Name, reconcile.Config{}, mreg, mode, drv)
			if mode == modeActive {
				shell.pub = newReconcileReportPublisher(mc) // TASK-031
			}
			solarShells[dc.Name] = shell
			if mode == modeActive {
				// Single reasserter: the shell's Reconnected() replaces
				// reassertLocked's inverter branch, and the seed replaces its
				// never-commanded stale-ceiling clear. Suppress reassertLocked
				// for this device and route reconnects to the shell.
				if rd := retryDevices[dc.Name]; rd != nil {
					rd.reconciledActive = true
					rd.onReconnect = shell.markReconnected
				}
				shell.seedRestoreCeiling(now)
			}
		}
		go runSolarShellTicker(solarShells, 60*time.Second)
		log.Printf("lexa-modbus: solar reconciler %s mode active for %d device(s)", solarMode, len(solarShells))
	}

	// Single retained-desired subscription routes each class to its shell map.
	if battShells != nil || solarShells != nil {
		if err := mqttutil.Subscribe(mc, bus.SubDesired, func(topic string, doc bus.DesiredState) {
			dev := bus.DeviceFromDesiredTopic(topic)
			switch bus.ClassFromDesiredTopic(topic) {
			case bus.DesiredClassBattery:
				if s, ok := battShells[dev]; ok {
					s.setDesired(doc, time.Now())
				}
			case bus.DesiredClassSolar:
				if s, ok := solarShells[dev]; ok {
					s.setDesired(doc, time.Now())
				}
			}
		}); err != nil {
			log.Printf("lexa-modbus: subscribe desired (reconciler): %v", err)
		}
	}

	// Subscribe to the registry and fan out to MQTT.
	updates, unsub := reg.Subscribe()
	defer unsub()

	reg.Start()
	defer reg.Stop()

	go publishMeasurements(mc, cfg, updates, interlock, battShells, solarShells)

	metrics.Serve(cfg.MetricsAddr, mreg)

	// sd_notify READY (TASK-008): the poll loop (reg.Start) and its MQTT
	// fan-out goroutine (publishMeasurements) are both running — the kick
	// site below covers registry, channel, and publish path in one place.
	watchdog.Ready()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("lexa-modbus: shutting down")
}

// errUnknownRole marks a device whose role is not recognized. It is a
// permanent configuration error, so the caller skips the device rather
// than wrapping it in a retry loop.
var errUnknownRole = errors.New("unknown device role")

// openDevice creates a live device connection based on role.
func openDevice(dc DeviceConfig) (device.Device, error) {
	switch dc.Role {
	case "inverter":
		return inverter.New(dc.URL, 5*time.Second, dc.UnitID)
	case "battery":
		return battery.New(dc.URL, 5*time.Second, dc.UnitID)
	case "meter":
		return meter.New(dc.URL, 5*time.Second, dc.UnitID)
	default:
		return nil, fmt.Errorf("%w %q for device %q (want inverter|battery|meter)", errUnknownRole, dc.Role, dc.Name)
	}
}

// publishMeasurements drains the registry subscription channel and publishes
// measurements (and battery metrics) to MQTT.
func publishMeasurements(mc mqtt.Client, cfg *Config, updates <-chan registry.MeasurementUpdate, interlock *batterySafetyInterlock, battShells map[string]*batteryShell, solarShells map[string]*solarShell) {
	deviceRole := map[string]string{}
	nameplate := map[string]float64{}
	for _, dc := range cfg.Devices {
		deviceRole[dc.Name] = dc.Role
		nameplate[dc.Name] = dc.MaxW
	}

	for upd := range updates {
		// TASK-008 watchdog kick: first statement of the update-drain body so
		// it fires on every registry update — including poll-error updates
		// (upd.Err below), since a device timing out still emits an update
		// each poll round. Only a wedged registry/channel (nothing arriving
		// at all) starves this kick, which is the failure mode WatchdogSec
		// exists to catch here.
		watchdog.Kick()
		if upd.Err != nil {
			log.Printf("lexa-modbus: device %s poll error: %v", upd.Name, upd.Err)
			continue
		}
		nowT := time.Now()
		now := nowT.Unix()
		m := upd.Measurements

		// Tier-0 edge safety interlock: evaluate every fresh poll BEFORE publishing,
		// on the raw measurement, so a mis-wired pack is disconnected locally within
		// one poll regardless of the hub. No-ops for non-battery devices.
		interlock.check(upd.Name, m)

		wPlausible := plausibleW(m.W, nameplate[upd.Name])
		msg := bus.Measurement{Envelope: bus.Envelope{V: bus.MeasurementV}, Device: upd.Name, Ts: now}
		if !math.IsNaN(m.W) {
			// Sanity-check decoded power against the nameplate before publishing.
			// A corrupted SunSpec scale factor (audit GS-1/MTR-1: solar-bad-scale)
			// decodes power ~10× the truth; withholding the suspect W leaves the
			// hub on its last-known-good rather than optimising against a value the
			// device physically cannot produce. Other fields (V/Hz) still flow.
			if wPlausible {
				msg.W = &m.W
			} else {
				log.Printf("lexa-modbus: REJECT implausible %s power %.0fW (nameplate %.0fW) — withholding W (suspect scale factor)",
					upd.Name, m.W, nameplate[upd.Name])
			}
		}

		// TASK-027/028: feed the battery reconciler shell this poll's readback.
		// Reuses the plausibleW verdict above (ledger L9's pattern) rather than
		// re-deriving it, so the reconciler and the published measurement never
		// disagree about whether this W was trustworthy. In active mode this is
		// also where a diverged read triggers a corrective write and a
		// just-reconnected pack reasserts desired.
		if s, ok := battShells[upd.Name]; ok {
			s.observe(m, wPlausible, nowT)
		}
		// TASK-029: feed the solar reconciler shell this poll's readback, reusing
		// the same plausibleW verdict (one-sided over-ceiling divergence lives in
		// the shell). In active mode this is where an over-ceiling inverter
		// triggers a corrective write and a just-reconnected inverter reasserts.
		if s, ok := solarShells[upd.Name]; ok {
			s.observe(m, wPlausible, nowT)
		}
		if !math.IsNaN(m.V) {
			msg.VoltageV = &m.V
		}
		if !math.IsNaN(m.Hz) {
			msg.Hz = &m.Hz
		}
		// Measurement plane is QoS 0 (bus.PubQoS): high-frequency, freshness-
		// gated by subscribers, so a dropped sample under broker congestion
		// is the documented design rather than a fault (review D5).
		measTopic := bus.MeasurementTopic(upd.Name)
		if err := mqttutil.PublishJSONQoS(mc, measTopic, bus.PubQoS(measTopic), msg); err != nil {
			log.Printf("lexa-modbus: publish measurement %s: %v", upd.Name, err)
		}

		// Batteries also publish on the metrics topic, which feeds both the
		// API's SoC display and the optimizer's storage model.
		if deviceRole[upd.Name] == "battery" && !math.IsNaN(m.SOC) {
			bm := bus.BattMetrics{Envelope: bus.Envelope{V: bus.BattMetricsV}, Device: upd.Name, SOC: &m.SOC, Ts: now}
			bmTopic := bus.BattMetricsTopic(upd.Name)
			if err := mqttutil.PublishJSONQoS(mc, bmTopic, bus.PubQoS(bmTopic), bm); err != nil {
				log.Printf("lexa-modbus: publish battery metrics %s: %v", upd.Name, err)
			}
		}
	}
}

// battCommandToControl converts a bus.BattCommand to the Modbus DERControlBase.
// Positive SetpointW = discharge (OpModExpLimW), negative = charge (OpModImpLimW).
func battCommandToControl(cmd bus.BattCommand) model.DERControlBase {
	ctrl := model.DERControlBase{OpModConnect: cmd.Connect}
	if cmd.SetpointW == nil {
		return ctrl
	}
	w := *cmd.SetpointW
	if w >= 0 {
		ap := activePowerFromWatts(w)
		ctrl.OpModExpLimW = &ap
	} else {
		ap := activePowerFromWatts(-w)
		ctrl.OpModImpLimW = &ap
	}
	return ctrl
}

// solarCommandToControl converts a bus.SolarCommand to a DERControlBase.
// Nil CurtailToW restores full nameplate generation.
func solarCommandToControl(cmd bus.SolarCommand) model.DERControlBase {
	if cmd.CurtailToW == nil {
		// Restore: command the ceiling far above any nameplate. The device clamps
		// the value to WMax, so WMaxLimPct → 100% (no effective curtailment). An
		// EMPTY control would be a silent no-op — Base.ApplyControl only ever
		// *sets* the ceiling — leaving the inverter stuck at its last curtailment.
		// Encoded via the multiplier so it stays above WMax even for systems
		// larger than the int16 watt range (a raw 32767 W ceiling would itself
		// curtail anything above 32.7 kW).
		ap := activePowerFromWatts(restoreCeilingW)
		return model.DERControlBase{OpModMaxLimW: &ap}
	}
	ap := activePowerFromWatts(math.Max(0, *cmd.CurtailToW))
	return model.DERControlBase{OpModMaxLimW: &ap}
}

// restoreCeilingW is the "no curtailment" ceiling — far above any
// residential/commercial nameplate so the device clamps it to WMax.
const restoreCeilingW = 1e9

// nameplateToleranceW is how far a decoded power reading may exceed the device
// nameplate before it is treated as a transport/scale-factor fault rather than a
// real measurement. Real inverters/meters do not sustain output meaningfully
// above their rating; a corrupted scale factor (solar-bad-scale) lands ~10× over.
const nameplateToleranceW = 1.2

// plausibleW reports whether a decoded power reading is physically plausible for
// a device of the given nameplate. A non-finite reading is never plausible; an
// unknown nameplate (≤ 0, not configured) cannot be judged and is accepted.
func plausibleW(w, maxW float64) bool {
	if math.IsNaN(w) || math.IsInf(w, 0) {
		return false
	}
	if maxW <= 0 {
		return true
	}
	return math.Abs(w) <= maxW*nameplateToleranceW
}

// activePowerFromWatts encodes a watt value as a SunSpec ActivePower,
// scaling into the decimal multiplier so values above the int16 range
// (> 32.767 kW) are represented faithfully instead of being clipped
// (audit GS-1/MTR-1: scale into the multiplier, never raw-cast).
func activePowerFromWatts(w float64) model.ActivePower {
	if w < 0 {
		w = 0
	}
	mult := int8(0)
	for w > math.MaxInt16 && mult < 9 {
		w /= 10
		mult++
	}
	if w > math.MaxInt16 {
		w = math.MaxInt16 // still over range at multiplier cap: clamp
	}
	return model.ActivePower{
		Value:      int16(math.Round(w)),
		Multiplier: mult,
	}
}

// retryDevice wraps a Modbus device so a connection that breaks mid-session
// (the device rebooted, the TCP session was severed, a poll timed out) is closed
// and reopened on the next poll, instead of erroring forever. EVERY device is
// wrapped — not just ones that failed to open at startup — because the common
// case is a device that opened fine and later dropped (e.g. a sim restart left
// the old session emitting "write: broken pipe" on every poll).
//
// The mutex serializes all operations on the single Modbus connection: the
// registry polls ReadMeasurements/Status on its poll goroutine while control
// writes arrive on the MQTT callback goroutine, and the simonvetter client is
// not safe for concurrent use — interleaved requests corrupt the stream (itself
// a likely source of broken-pipe errors).
type retryDevice struct {
	cfg DeviceConfig
	// open reconnects the device; nil means use the package openDevice(cfg).
	// Overridden in tests to inject a fake transport.
	open func() (device.Device, error)
	mu   sync.Mutex
	live device.Device

	// lastCtrl is the most recent control the orchestrator commanded for this
	// device — recorded even while disconnected (the DESIRED state, not the
	// delivered one). On reconnect it is re-asserted so a device that was dark
	// through a control transition converges to what the hub currently wants
	// instead of keeping whatever it latched before the drop (Phase 4; QA
	// 2026-07-02: release-while-rebooting — a cap released while the inverter
	// was rebooting left it clamped at the stale ceiling indefinitely).
	//
	// TASK-028: for an active-reconciled device, lastCtrl is NOT recorded and
	// its reconnect reassert is NOT fired — the reconciler is the single
	// reasserter (double-write races otherwise), signalled via onReconnect. The
	// never-commanded-inverter branch of reassertLocked stays intact for solar.
	lastCtrl *model.DERControlBase

	// reconciledActive marks a device whose writes are owned by the active
	// battery reconciler: suppresses lastCtrl recording/reassert (above) and
	// enables the onReconnect signal below.
	reconciledActive bool
	// onReconnect, when set (active-reconciled devices only), is invoked after a
	// successful reopen so the reconciler reasserts the standing desired (ledger
	// L4). It MUST NOT take any lock the apply path holds — it runs under r.mu,
	// and the reconciler's apply path is mu → registry → r.mu; the shell's
	// markReconnected only does an atomic store, satisfying that.
	onReconnect func()

	// TASK-044 metrics (all nil-safe; every registry_test.go/control_test.go
	// construction of retryDevice omits them and still works — see
	// metrics.Counter/Gauge's nil-receiver doc):
	//   pollDuration  — lexa_mb_poll_duration_seconds, last ReadMeasurements
	//                   call's wall time across ALL devices (no per-device
	//                   labels, per this task's metric inventory).
	//   reconnects    — lexa_mb_device_reconnects_total, one per completed
	//                   reopen (not the initial open at startup).
	//   writeFailures — lexa_mb_write_failures_total, one per failed
	//                   ApplyControl (including the reconnect-reconcile write).
	pollDuration  *metrics.Gauge
	reconnects    *metrics.Counter
	writeFailures *metrics.Counter
}

// reopen establishes a fresh device connection.
func (r *retryDevice) reopen() (device.Device, error) {
	if r.open != nil {
		return r.open()
	}
	return openDevice(r.cfg)
}

func (r *retryDevice) ReadMeasurements() (device.Measurements, error) {
	// lexa_mb_poll_duration_seconds (TASK-044): wall time of this call,
	// including a reconnect + reconcile when one happens — that IS the poll
	// cost on a poll where the device was dark, not something to exclude.
	pollStart := time.Now()
	defer func() { r.pollDuration.Set(time.Since(pollStart).Seconds()) }()

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.live == nil {
		dev, err := r.reopen()
		if err != nil || dev == nil {
			return device.Measurements{W: math.NaN(), V: math.NaN(), Hz: math.NaN()}, err
		}
		r.live = dev
		r.reconnects.Inc()
		slog.Info("lexa-modbus: reconnected device", "device", r.cfg.Name) // TASK-045
		if r.reconciledActive {
			// TASK-028: the active reconciler owns reassert-on-reconnect (ledger
			// L4). Signal it instead of replaying lastCtrl, so there is exactly
			// ONE reasserter and no double-write race. The callback only sets an
			// atomic flag (no lock), so calling it under r.mu is deadlock-safe;
			// the shell reasserts on this poll's Observe.
			if r.onReconnect != nil {
				r.onReconnect()
			}
		} else if ctrl, why, ok := r.reassertLocked(); ok {
			// Reconcile-on-reconnect (Phase 4): the device may have missed every
			// control transition while dark — including a release, which for an
			// inverter is a WRITE (the restore ceiling), not an absence of writes.
			// Bring its registers back to the hub's current desired state before
			// the first measurement is trusted.
			if err := r.live.ApplyControl(ctrl); err != nil {
				r.writeFailures.Inc()
				slog.Warn("lexa-modbus: device reconnect reconcile failed",
					"device", r.cfg.Name, "why", why, "err", err)
				r.dropLocked() // suspect session; retry whole sequence next poll
				return device.Measurements{W: math.NaN(), V: math.NaN(), Hz: math.NaN()}, err
			}
			slog.Info("lexa-modbus: device reconnected", "device", r.cfg.Name, "why", why)
		}
	}
	m, err := r.live.ReadMeasurements()
	if err != nil {
		r.dropLocked() // drop the dead session so the next poll reconnects
	}
	return m, err
}

// reassertLocked picks the control to reconcile a just-reconnected device to.
// Caller must hold r.mu.
//
//   - A control was commanded at some point (connected or not): re-assert it —
//     it is the hub's current desired state, and the device may hold something
//     older (or have reboot-reset to defaults; the orchestrator's periodic
//     re-command covers that case too, but only while a control is ACTIVE).
//   - Never commanded AND the device is an inverter: clear a possible stale
//     ceiling by asserting the restore ceiling. An idle inverter receives no
//     periodic commands, so a ceiling latched before this process started (or
//     released while the device was dark) would otherwise persist forever.
//   - Never commanded, battery: nothing — the orchestrator re-commands packs
//     every engine tick, and an unsolicited write could fight it.
//   - Meter: never — read-only device.
func (r *retryDevice) reassertLocked() (model.DERControlBase, string, bool) {
	if r.lastCtrl != nil {
		return *r.lastCtrl, "re-asserted the hub's current control", true
	}
	if r.cfg.Role == "inverter" {
		ap := activePowerFromWatts(restoreCeilingW)
		return model.DERControlBase{OpModMaxLimW: &ap}, "cleared possible stale ceiling (restore to full output)", true
	}
	return model.DERControlBase{}, "", false
}

func (r *retryDevice) ApplyControl(ctrl model.DERControlBase) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Record the DESIRED state even while disconnected — reconnect reconciles
	// to the newest command, so a release/re-command that happened while the
	// device was dark is not lost (Phase 4: release-while-rebooting).
	//
	// TASK-028: an active-reconciled device does NOT record lastCtrl — the
	// reconciler is the single reasserter (its own Reconnected write covers the
	// reboot case), and a lastCtrl replay here would be a competing second
	// reasserter (the double-write race the task forbids).
	if !r.reconciledActive {
		stored := ctrl
		r.lastCtrl = &stored
	}
	if r.live == nil {
		return nil // disconnected; the next ReadMeasurements poll reconnects and re-asserts
	}
	err := r.live.ApplyControl(ctrl)
	if err != nil {
		r.writeFailures.Inc()
		r.dropLocked()
	}
	return err
}

func (r *retryDevice) Status() (device.DeviceStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.live == nil {
		return device.DeviceStatus{}, nil
	}
	st, err := r.live.Status()
	if err != nil {
		r.dropLocked()
	}
	return st, err
}

func (r *retryDevice) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.live == nil {
		return nil
	}
	err := r.live.Close()
	r.live = nil
	return err
}

// dropLocked closes and clears the live device so the next poll reconnects.
// Caller must hold r.mu.
func (r *retryDevice) dropLocked() {
	if r.live != nil {
		_ = r.live.Close() // release the fd before clearing the reference
		r.live = nil
		slog.Warn("lexa-modbus: device session dropped — will reconnect on next poll", "device", r.cfg.Name) // TASK-045
	}
}
