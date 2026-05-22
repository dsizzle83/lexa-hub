// lexa-hub is the central orchestrator for the LEXA energy hub.
//
// It subscribes to device measurements, battery metrics, CSIP controls, EVSE
// state, the northbound 24-hour DER schedule, and live pricing from the MQTT
// bus, runs a cost-optimal 24-hour planner plus a reactive rule engine on a
// configurable interval, and publishes control commands back to the bus.
//
// Data flow:
//
//	lexa/measurements/+       → MQTTSystemReader (battery/solar/meter state)
//	lexa/battery/+/metrics    → MQTTSystemReader (SOC/SOH/capacity)
//	lexa/csip/control         → MQTTSystemReader (active CSIP control)
//	lexa/evse/+/state         → MQTTSystemReader (EVSE connector state)
//	lexa/northbound/schedule  → Engine.SetDERConstraints (24h DER envelope)
//	lexa/csip/pricing         → Engine.SetPrices (TOU import/export rates)
//	                                      ↓
//	                      DailyPlanner.Plan (24h DP, every 15 min)
//	                      + Optimizer.Optimize (reactive rules, every 15 s)
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
	"math"
	"os"
	"os/signal"
	"syscall"
	"time"

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
		Planner:  cfg.Planner,
	})

	// Subscribe to northbound DER schedule → extract DER constraints for planner.
	if err := mqttutil.Subscribe(mc, bus.TopicNorthboundSchedule, func(_ string, sched bus.DERScheduleMsg) {
		eng.SetDERConstraints(derConstraintsFromSchedule(sched))
	}); err != nil {
		log.Fatalf("lexa-hub: subscribe northbound schedule: %v", err)
	}

	// Subscribe to live pricing → extract per-step prices for planner.
	if err := mqttutil.Subscribe(mc, bus.TopicCSIPPricing, func(_ string, pricing bus.PricingUpdate) {
		imp, exp := pricesFromPricingUpdate(pricing, time.Now())
		if imp != nil {
			eng.SetPrices(imp, exp)
		}
	}); err != nil {
		log.Fatalf("lexa-hub: subscribe csip pricing: %v", err)
	}

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

	log.Printf("lexa-hub: running (engine interval=%s planner replan=%ds debug=%v)",
		cfg.EngineInterval(), cfg.Planner.ReplanIntervalS, cfg.Debug)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("lexa-hub: shutting down")
}

// derConstraintsFromSchedule converts a DERScheduleMsg into per-step
// StepConstraints for the daily planner.  Missing steps are left unconstrained.
func derConstraintsFromSchedule(sched bus.DERScheduleMsg) []orchestrator.StepConstraint {
	const planStepSec = 5 * 60
	const planSteps = 288

	if len(sched.Slots) == 0 {
		return nil
	}
	ws := sched.WindowStart - (sched.WindowStart % planStepSec)
	out := make([]orchestrator.StepConstraint, planSteps)
	for i := range out {
		out[i] = orchestrator.StepConstraint{
			ExpLimW: math.NaN(),
			ImpLimW: math.NaN(),
			MaxLimW: math.NaN(),
			FixedW:  math.NaN(),
		}
	}

	for _, slot := range sched.Slots {
		startIdx := int((slot.Start - ws) / planStepSec)
		endIdx := int((slot.End - ws) / planStepSec)
		if startIdx >= planSteps || endIdx <= 0 {
			continue
		}
		if startIdx < 0 {
			startIdx = 0
		}
		if endIdx > planSteps {
			endIdx = planSteps
		}

		disconnect := slot.Connect != nil && !*slot.Connect
		expLimW := math.NaN()
		if slot.ExpLimW != nil {
			expLimW = *slot.ExpLimW
		}
		impLimW := math.NaN()
		if slot.ImpLimW != nil {
			impLimW = *slot.ImpLimW
		}
		maxLimW := math.NaN()
		if slot.MaxLimW != nil {
			maxLimW = *slot.MaxLimW
		}
		fixedW := math.NaN()
		if slot.FixedW != nil {
			fixedW = *slot.FixedW
		}

		for i := startIdx; i < endIdx; i++ {
			out[i] = orchestrator.StepConstraint{
				Disconnect: disconnect,
				ExpLimW:    expLimW,
				ImpLimW:    impLimW,
				MaxLimW:    maxLimW,
				FixedW:     fixedW,
			}
		}
	}
	return out
}

// pricesFromPricingUpdate converts a PricingUpdate into per-step import and
// export price arrays ($/kWh) for the daily planner.
// Returns nil, nil when the update contains no usable pricing data.
func pricesFromPricingUpdate(pricing bus.PricingUpdate, now time.Time) (importPrices, exportPrices []float64) {
	const planStepSec = int64(5 * 60)
	const planSteps = 288

	if len(pricing.TariffProfiles) == 0 {
		return nil, nil
	}

	// Snap window start to the 5-min boundary covering now.
	ws := now.Unix() - (now.Unix() % planStepSec)

	importPrices = make([]float64, planSteps)
	exportPrices = make([]float64, planSteps)

	for _, tp := range pricing.TariffProfiles {
		mult := 1.0
		for i := int8(0); i < tp.PricePowerOfTenMultiplier; i++ {
			mult *= 10
		}
		for i := tp.PricePowerOfTenMultiplier; i < 0; i++ {
			mult /= 10
		}

		for _, rc := range tp.RateComponents {
			allIntervals := append(rc.ActiveIntervals, rc.ScheduledIntervals...)
			for _, ti := range allIntervals {
				if len(ti.Blocks) == 0 {
					continue
				}
				pricePerKwh := float64(ti.Blocks[0].Price) * mult / 1000 // assume milli-currency
				tEnd := ti.IntervalStart + int64(ti.Duration)
				for i := 0; i < planSteps; i++ {
					stepT := ws + int64(i)*planStepSec
					if stepT >= ti.IntervalStart && stepT < tEnd {
						importPrices[i] = pricePerKwh
					}
				}
			}
		}
		break // first tariff profile only
	}

	return importPrices, exportPrices
}
