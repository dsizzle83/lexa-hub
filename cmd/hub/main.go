// lexa-hub is the central orchestrator for the LEXA energy hub.
//
// It subscribes to device measurements, battery metrics, CSIP controls, and
// EVSE state from the MQTT bus, runs the energy optimizer on a configurable
// interval, and publishes control commands back to the bus.
//
// Data flow:
//
//	lexa/measurements/+       → MQTTSystemReader (battery/solar/meter state)
//	lexa/battery/+/metrics    → MQTTSystemReader (SOC/SOH/capacity)
//	lexa/csip/control         → MQTTSystemReader (active CSIP control)
//	lexa/evse/+/state         → MQTTSystemReader (EVSE connector state)
//	                                      ↓
//	                             Optimizer.Optimize(SystemState)
//	                                      ↓
//	lexa/control/battery/+    ← MQTTBatteryActuator
//	lexa/control/solar/+      ← MQTTSolarActuator
//	lexa/evse/+/command       ← MQTTEVSEActuator
//
// Usage:
//
//	lexa-hub [-config /etc/lexa/hub.json]
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/mqttutil"
	"lexa-hub/internal/orchestrator"
)

func main() {
	cfgPath := flag.String("config", "/etc/lexa/hub.json", "path to JSON config")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("lexa-hub: load config: %v", err)
	}

	mc, err := mqttutil.Connect(cfg.MQTTBroker, cfg.MQTTClientID)
	if err != nil {
		log.Fatalf("lexa-hub: %v", err)
	}
	defer mc.Disconnect(500)

	// Build the MQTT-backed system reader.
	reader := newMQTTSystemReader(cfg.Devices)

	// Subscribe to all state topics.
	if err := mqttutil.Subscribe(mc, bus.SubMeasurements, reader.onMeasurement); err != nil {
		log.Fatalf("lexa-hub: subscribe measurements: %v", err)
	}
	if err := mqttutil.Subscribe(mc, bus.SubBattMetrics, reader.onBattMetrics); err != nil {
		log.Fatalf("lexa-hub: subscribe batt metrics: %v", err)
	}
	if err := mqttutil.Subscribe(mc, bus.TopicCSIPControl, reader.onCSIPControl); err != nil {
		log.Fatalf("lexa-hub: subscribe csip control: %v", err)
	}
	if err := mqttutil.Subscribe(mc, bus.SubEVSEState, reader.onEVSEState); err != nil {
		log.Fatalf("lexa-hub: subscribe evse state: %v", err)
	}

	// Build the optimizer and engine.
	opt := orchestrator.NewDefaultOptimizer()
	opt.Debug = cfg.Debug

	eng := orchestrator.New(reader, opt, orchestrator.Config{
		Interval: cfg.EngineInterval(),
		Debug:    cfg.Debug,
	})

	// Wire MQTT actuators for each device.
	for _, dc := range cfg.Devices {
		switch dc.Role {
		case "battery":
			eng.RegisterBatteryActuator(dc.Name, &MQTTBatteryActuator{mc: mc, device: dc.Name})
		case "inverter":
			eng.RegisterSolarActuator(dc.Name, &MQTTSolarActuator{mc: mc, device: dc.Name})
		}
	}

	// Wire MQTT actuators for known EVSE stations.
	for _, sc := range cfg.Stations {
		eng.RegisterEVSEActuator(sc.ID, &MQTTEVSEActuator{mc: mc, stationID: sc.ID})
	}

	eng.Start()
	defer eng.Stop()

	log.Printf("lexa-hub: running (engine interval=%s debug=%v)", cfg.EngineInterval(), cfg.Debug)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("lexa-hub: shutting down")
}
