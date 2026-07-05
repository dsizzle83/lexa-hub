// lexa-modbus polls SunSpec/Modbus devices and bridges them to the MQTT bus.
//
// Northbound (MQTT):
//   - Publishes lexa/measurements/{device} on every poll.
//   - Publishes lexa/battery/{device}/metrics for battery-role devices.
//   - Subscribes lexa/control/battery/{device} and applies battery setpoints.
//   - Subscribes lexa/control/solar/{device} and applies solar curtailment.
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
	"math"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/mqttutil"
	"lexa-hub/internal/northbound/model"
	"lexa-hub/internal/southbound/battery"
	"lexa-hub/internal/southbound/device"
	"lexa-hub/internal/southbound/inverter"
	"lexa-hub/internal/southbound/meter"
	"lexa-hub/internal/southbound/registry"
	"lexa-hub/internal/watchdog"
)

func main() {
	cfgPath := flag.String("config", "/etc/lexa/modbus.json", "path to JSON config")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("lexa-modbus: load config: %v", err)
	}

	mqttPass, err := mqttutil.LoadPassword(cfg.MQTTPassFile)
	if err != nil {
		log.Fatalf("lexa-modbus: %v", err)
	}
	mc, err := mqttutil.ConnectAuth(cfg.MQTTBroker, cfg.MQTTClientID, cfg.MQTTUser, mqttPass)
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

	// Open each device; log failures but continue so other devices still poll.
	for _, dc := range cfg.Devices {
		dev, err := openDevice(dc)
		if errors.Is(err, errUnknownRole) {
			log.Printf("lexa-modbus: device %s: %v — skipping", dc.Name, err)
			continue
		}
		// Wrap EVERY device so a mid-session drop reconnects, not just ones that
		// failed the initial open. A device that opened cleanly and later lost its
		// session would otherwise error forever.
		rd := &retryDevice{cfg: dc}
		if err != nil {
			log.Printf("lexa-modbus: device %s (%s): %v — will reconnect on next poll", dc.Name, dc.URL, err)
		} else {
			rd.live = dev
		}
		reg.Add(&registry.Entry{Name: dc.Name, Addr: dc.URL, Device: rd})
		log.Printf("lexa-modbus: registered device %s role=%s url=%s", dc.Name, dc.Role, dc.URL)
	}

	// Subscribe to control topics before starting the poll loop.
	subscribeControls(mc, cfg, reg, interlock)

	// Subscribe to the registry and fan out to MQTT.
	updates, unsub := reg.Subscribe()
	defer unsub()

	reg.Start()
	defer reg.Stop()

	go publishMeasurements(mc, cfg, updates, interlock)

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
func publishMeasurements(mc mqtt.Client, cfg *Config, updates <-chan registry.MeasurementUpdate, interlock *batterySafetyInterlock) {
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
		now := time.Now().Unix()
		m := upd.Measurements

		// Tier-0 edge safety interlock: evaluate every fresh poll BEFORE publishing,
		// on the raw measurement, so a mis-wired pack is disconnected locally within
		// one poll regardless of the hub. No-ops for non-battery devices.
		interlock.check(upd.Name, m)

		msg := bus.Measurement{Envelope: bus.Envelope{V: bus.MeasurementV}, Device: upd.Name, Ts: now}
		if !math.IsNaN(m.W) {
			// Sanity-check decoded power against the nameplate before publishing.
			// A corrupted SunSpec scale factor (audit GS-1/MTR-1: solar-bad-scale)
			// decodes power ~10× the truth; withholding the suspect W leaves the
			// hub on its last-known-good rather than optimising against a value the
			// device physically cannot produce. Other fields (V/Hz) still flow.
			if plausibleW(m.W, nameplate[upd.Name]) {
				msg.W = &m.W
			} else {
				log.Printf("lexa-modbus: REJECT implausible %s power %.0fW (nameplate %.0fW) — withholding W (suspect scale factor)",
					upd.Name, m.W, nameplate[upd.Name])
			}
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

// subscribeControls sets up MQTT subscriptions for battery and solar commands.
func subscribeControls(mc mqtt.Client, cfg *Config, reg *registry.Registry, interlock *batterySafetyInterlock) {
	// Build a map from device name → role for quick lookup.
	roleOf := map[string]string{}
	for _, dc := range cfg.Devices {
		roleOf[dc.Name] = dc.Role
	}

	if err := mqttutil.Subscribe(mc, bus.SubCtrlBattery, func(topic string, cmd bus.BattCommand) {
		dev := bus.DeviceFromCtrlBatteryTopic(topic)
		if roleOf[dev] != "battery" {
			return
		}
		// Record the hub's intent for the Tier-0 interlock before applying.
		interlock.noteControl(dev, cmd)
		ctrl := battCommandToControl(cmd)
		if err := reg.ApplyControlTo(dev, ctrl); err != nil {
			log.Printf("lexa-modbus: apply battery control %s: %v", dev, err)
		} else {
			log.Printf("lexa-modbus: battery %s control applied", dev)
		}
	}); err != nil {
		log.Printf("lexa-modbus: subscribe battery control: %v", err)
	}

	if err := mqttutil.Subscribe(mc, bus.SubCtrlSolar, func(topic string, cmd bus.SolarCommand) {
		dev := bus.DeviceFromCtrlSolarTopic(topic)
		if roleOf[dev] != "inverter" {
			return
		}
		ctrl := solarCommandToControl(cmd)
		if err := reg.ApplyControlTo(dev, ctrl); err != nil {
			log.Printf("lexa-modbus: apply solar control %s: %v", dev, err)
		} else {
			log.Printf("lexa-modbus: solar %s control applied", dev)
		}
	}); err != nil {
		log.Printf("lexa-modbus: subscribe solar control: %v", err)
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
	lastCtrl *model.DERControlBase
}

// reopen establishes a fresh device connection.
func (r *retryDevice) reopen() (device.Device, error) {
	if r.open != nil {
		return r.open()
	}
	return openDevice(r.cfg)
}

func (r *retryDevice) ReadMeasurements() (device.Measurements, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.live == nil {
		dev, err := r.reopen()
		if err != nil || dev == nil {
			return device.Measurements{W: math.NaN(), V: math.NaN(), Hz: math.NaN()}, err
		}
		r.live = dev
		log.Printf("lexa-modbus: reconnected device %s", r.cfg.Name)
		// Reconcile-on-reconnect (Phase 4): the device may have missed every
		// control transition while dark — including a release, which for an
		// inverter is a WRITE (the restore ceiling), not an absence of writes.
		// Bring its registers back to the hub's current desired state before
		// the first measurement is trusted.
		if ctrl, why, ok := r.reassertLocked(); ok {
			if err := r.live.ApplyControl(ctrl); err != nil {
				log.Printf("lexa-modbus: device %s reconnect reconcile (%s) failed: %v", r.cfg.Name, why, err)
				r.dropLocked() // suspect session; retry whole sequence next poll
				return device.Measurements{W: math.NaN(), V: math.NaN(), Hz: math.NaN()}, err
			}
			log.Printf("lexa-modbus: device %s reconnected — %s", r.cfg.Name, why)
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
	stored := ctrl
	r.lastCtrl = &stored
	if r.live == nil {
		return nil // disconnected; the next ReadMeasurements poll reconnects and re-asserts
	}
	err := r.live.ApplyControl(ctrl)
	if err != nil {
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
		log.Printf("lexa-modbus: device %s session dropped — will reconnect on next poll", r.cfg.Name)
	}
}
