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
	"flag"
	"log"
	"math"
	"os"
	"os/signal"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/csip/model"
	"lexa-hub/internal/mqttutil"
	"lexa-hub/internal/southbound/battery"
	"lexa-hub/internal/southbound/device"
	"lexa-hub/internal/southbound/inverter"
	"lexa-hub/internal/southbound/meter"
	"lexa-hub/internal/southbound/registry"
)

func main() {
	cfgPath := flag.String("config", "/etc/lexa/modbus.json", "path to JSON config")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("lexa-modbus: load config: %v", err)
	}

	mc, err := mqttutil.Connect(cfg.MQTTBroker, cfg.MQTTClientID)
	if err != nil {
		log.Fatalf("lexa-modbus: %v", err)
	}
	defer mc.Disconnect(500)

	reg := registry.New(cfg.PollInterval())

	// Open each device; log failures but continue so other devices still poll.
	for _, dc := range cfg.Devices {
		dev, err := openDevice(dc)
		if err != nil {
			log.Printf("lexa-modbus: device %s (%s): %v — will retry", dc.Name, dc.URL, err)
			dev = &retryDevice{cfg: dc}
		}
		reg.Add(&registry.Entry{Name: dc.Name, Addr: dc.URL, Device: dev})
		log.Printf("lexa-modbus: registered device %s role=%s url=%s", dc.Name, dc.Role, dc.URL)
	}

	// Subscribe to control topics before starting the poll loop.
	subscribeControls(mc, cfg, reg)

	// Subscribe to the registry and fan out to MQTT.
	updates, unsub := reg.Subscribe()
	defer unsub()

	reg.Start()
	defer reg.Stop()

	go publishMeasurements(mc, cfg, updates)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("lexa-modbus: shutting down")
}

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
		return nil, nil // unknown role, will be skipped
	}
}

// publishMeasurements drains the registry subscription channel and publishes
// measurements (and battery metrics) to MQTT.
func publishMeasurements(mc mqtt.Client, cfg *Config, updates <-chan registry.MeasurementUpdate) {
	deviceRole := map[string]string{}
	for _, dc := range cfg.Devices {
		deviceRole[dc.Name] = dc.Role
	}

	for upd := range updates {
		if upd.Err != nil {
			log.Printf("lexa-modbus: device %s poll error: %v", upd.Name, upd.Err)
			continue
		}
		now := time.Now().Unix()
		m := upd.Measurements

		msg := bus.Measurement{Device: upd.Name, Ts: now}
		if !math.IsNaN(m.W) {
			msg.W = &m.W
		}
		if !math.IsNaN(m.V) {
			msg.V = &m.V
		}
		if !math.IsNaN(m.Hz) {
			msg.Hz = &m.Hz
		}

		if err := mqttutil.PublishJSON(mc, bus.MeasurementTopic(upd.Name), msg); err != nil {
			log.Printf("lexa-modbus: publish measurement %s: %v", upd.Name, err)
		}
	}
}

// subscribeControls sets up MQTT subscriptions for battery and solar commands.
func subscribeControls(mc mqtt.Client, cfg *Config, reg *registry.Registry) {
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
		v := clamp16(w)
		ctrl.OpModExpLimW = &model.ActivePower{Value: v, Multiplier: 0}
	} else {
		v := clamp16(-w)
		ctrl.OpModImpLimW = &model.ActivePower{Value: v, Multiplier: 0}
	}
	return ctrl
}

// solarCommandToControl converts a bus.SolarCommand to a DERControlBase.
// Nil CurtailToW restores full nameplate generation.
func solarCommandToControl(cmd bus.SolarCommand) model.DERControlBase {
	if cmd.CurtailToW == nil {
		// NaN sentinel in the SunSpec layer means "restore max".
		return model.DERControlBase{}
	}
	v := clamp16(math.Max(0, *cmd.CurtailToW))
	return model.DERControlBase{
		OpModMaxLimW: &model.ActivePower{Value: v, Multiplier: 0},
	}
}

func clamp16(v float64) int16 {
	if v > 32767 {
		return 32767
	}
	if v < 0 {
		return 0
	}
	return int16(v)
}

// retryDevice is a placeholder that auto-retries open on every ReadMeasurements call.
type retryDevice struct {
	cfg  DeviceConfig
	live device.Device
}

func (r *retryDevice) ReadMeasurements() (device.Measurements, error) {
	if r.live == nil {
		dev, err := openDevice(r.cfg)
		if err != nil || dev == nil {
			return device.Measurements{W: math.NaN(), V: math.NaN(), Hz: math.NaN()},
				err
		}
		r.live = dev
		log.Printf("lexa-modbus: reconnected device %s", r.cfg.Name)
	}
	m, err := r.live.ReadMeasurements()
	if err != nil {
		r.live = nil // reset so next poll retries
	}
	return m, err
}

func (r *retryDevice) ApplyControl(ctrl model.DERControlBase) error {
	if r.live == nil {
		return nil
	}
	return r.live.ApplyControl(ctrl)
}

func (r *retryDevice) Status() (device.DeviceStatus, error) {
	if r.live == nil {
		return device.DeviceStatus{}, nil
	}
	return r.live.Status()
}

func (r *retryDevice) Close() error {
	if r.live != nil {
		return r.live.Close()
	}
	return nil
}
