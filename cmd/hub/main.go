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
	"lexa-hub/internal/watchdog"
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

	// Build the optimizer and engine (before the subscriptions, which need to
	// poke the engine on urgent controls).
	opt := orchestrator.NewDefaultOptimizer()
	opt.Debug = cfg.Debug
	// Tell the optimizer the engine cadence so its tick-denominated breach/debounce
	// thresholds keep a constant WALL-CLOCK meaning across cadences (the product ships
	// at 15 s but is QA'd at the 3 s fast tick). Without this, the shipped safety and
	// CannotComply latencies are 5× the ones validated in fast mode.
	opt.SetTickInterval(cfg.EngineInterval())
	// Activate the reactive TOU peak-discharge rule as a fallback. The daily
	// planner is the primary battery dispatcher; this rule only fires when no
	// plan target is available for the current interval (startup, or a gap in
	// the plan window), so the battery still shaves peak instead of idling.
	opt.CostModel = orchestrator.DefaultTOUCostModel()
	// The EV import cooldown is tick-denominated; size it to ~1 min of wall
	// clock from the configured interval (the optimizer's default of 20 is
	// calibrated for a 3 s demo tick and would mean 5 min at the 15 s default).
	if cycles := int(time.Minute / cfg.EngineInterval()); cycles >= 4 {
		opt.EVImportCooldownCycles = cycles
	} else {
		opt.EVImportCooldownCycles = 4
	}

	// Compliance-breach alerter. The optimizer flags limits it cannot meet
	// (e.g. an import cap with the battery at its SOC reserve) on plan.Breach.
	// Publish one ComplianceAlert when a control's breach begins and one when it
	// clears, so the northbound service POSTs one CannotComply Response per episode
	// rather than spamming one per tick.
	//
	// Keyed on the breaching mRID, NOT a single active flag: when one control is
	// already breaching and a DIFFERENT control then breaches without an
	// intervening clear (a higher-primacy event supersedes an unmeetable one, or a
	// held last-known-good control overlaps a fresh cap), an mRID-agnostic flag
	// stays latched and the new control's breach is never reported. That was the
	// reject-write/enable-gate-curtail flakiness: a prior episode's breach kept the
	// alerter latched, so a fresh gen-cap breach published no alert and gridsim
	// never saw the CannotComply.
	activeBreachMRID := "" // "" = no breach currently active
	// dedupeResets clears every actuator's command deduper; populated when the
	// actuators are wired below (before the engine starts, so no race — the
	// observer and executePlan both run on the engine's control goroutine).
	var dedupeResets []func()
	planObserver := func(plan orchestrator.Plan) {
		// systemd watchdog keepalive (TASK-007). Must stay the first thing
		// this closure does, before any publish: the engine calls
		// PlanObserver on its own control goroutine on every economic tick
		// and on every safety pass that produces commands (engine.go
		// tick()/safetyTick()), so a kick here rides the tick loop itself —
		// if ReadSystemState/Optimize/executePlan or this closure's own
		// publishes below wedge, the NEXT tick's kick simply never happens
		// and systemd's WatchdogSec fires. A goroutine-based timer kick
		// would defeat this entirely by staying alive after the loop dies.
		watchdog.Kick()

		// A compliance breach means the measured effect contradicts the
		// commanded state — the device may have reverted behind the hub's back
		// (reboot to defaults, installer override), which is exactly the case
		// the dedupers' "already sent" assumption gets wrong. Reset them so
		// this tick's commands publish unconditionally (the observer runs
		// before executePlan). Self-limiting: fires only while a breach
		// persists; without it a reverted device saw no corrective write for
		// up to reassertEvery even as the hub posted CannotComply about it
		// (QA 2026-07-03: a 0 W ceiling dedupe-suppressed for 30 s against an
		// uncurtailed inverter).
		if plan.Breach != nil {
			for _, reset := range dedupeResets {
				reset()
			}
		}
		// Surface the plan trace on the bus so lexa-api's /status last_plan is
		// real data instead of the historical empty stub (the QA harness's
		// decision introspection depends on it). Published on EVERY pass —
		// decisions or not — so the timestamp doubles as an engine heartbeat.
		// Retained: lexa-api restarting mid-episode still sees the latest plan.
		pl := bus.PlanLog{Envelope: bus.Envelope{V: bus.PlanLogV}, Ts: plan.Timestamp.Unix()}
		for _, dec := range plan.Decisions {
			pl.Decisions = append(pl.Decisions, bus.PlanDecision{
				Rule: dec.Rule, Reason: dec.Reason, Impact: dec.Impact,
			})
		}
		if err := mqttutil.PublishJSONRetained(mc, bus.TopicHubPlan, pl); err != nil {
			log.Printf("lexa-hub: publish plan log: %v", err)
		}

		alert, newMRID := breachAlert(activeBreachMRID, plan)
		activeBreachMRID = newMRID
		if alert == nil {
			return // no edge this tick
		}
		alert.Ts = time.Now().Unix()
		if alert.Active {
			log.Printf("lexa-hub: COMPLIANCE BREACH %s limit=%.0fW measured=%.0fW shortfall=%.0fW (%s) mrid=%s",
				alert.LimitType, alert.LimitW, alert.MeasuredW, alert.ShortfallW, alert.Reason, alert.MRID)
		} else {
			log.Printf("lexa-hub: compliance breach cleared")
		}
		if err := mqttutil.PublishJSON(mc, bus.TopicCSIPComplianceAlert, *alert); err != nil {
			log.Printf("lexa-hub: publish compliance alert: %v", err)
		}
	}

	eng := orchestrator.New(reader, opt, orchestrator.Config{
		Interval:       cfg.EngineInterval(),
		SafetyInterval: cfg.SafetyInterval(),
		Debug:          cfg.Debug,
		Planner:        cfg.Planner,
		PlanObserver:   planObserver,
	})

	// Subscribe to all state topics.
	if err := mqttutil.Subscribe(mc, bus.SubMeasurements, reader.onMeasurement); err != nil {
		log.Fatalf("lexa-hub: subscribe measurements: %v", err)
	}
	if err := mqttutil.Subscribe(mc, bus.SubBattMetrics, reader.onBattMetrics); err != nil {
		log.Fatalf("lexa-hub: subscribe batt metrics: %v", err)
	}
	if err := mqttutil.Subscribe(mc, bus.TopicCSIPControl, func(topic string, msg bus.ActiveControl) {
		reader.onCSIPControl(topic, msg)
		// A disconnect order must not wait out the ticker interval: force an
		// immediate tick so cease-to-energize is applied within MQTT latency.
		if msg.Connect != nil && !*msg.Connect {
			eng.Wake()
		}
	}); err != nil {
		log.Fatalf("lexa-hub: subscribe csip control: %v", err)
	}
	if err := mqttutil.Subscribe(mc, bus.SubEVSEState, reader.onEVSEState); err != nil {
		log.Fatalf("lexa-hub: subscribe evse state: %v", err)
	}

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
			a := &MQTTBatteryActuator{mc: mc, device: dc.Name}
			dedupeResets = append(dedupeResets, a.dedupe.reset)
			eng.RegisterBatteryActuator(dc.Name, a)
		case "inverter":
			a := &MQTTSolarActuator{mc: mc, device: dc.Name}
			dedupeResets = append(dedupeResets, a.dedupe.reset)
			eng.RegisterSolarActuator(dc.Name, a)
		}
	}

	// Wire MQTT actuators for known EVSE stations.
	for _, sc := range cfg.Stations {
		a := &MQTTEVSEActuator{mc: mc, stationID: sc.ID}
		dedupeResets = append(dedupeResets, a.dedupe.reset)
		eng.RegisterEVSEActuator(sc.ID, a)
	}

	eng.Start()
	defer eng.Stop()

	// sd_notify READY (TASK-007): tells systemd (Type=notify) the hub has
	// finished starting and is now ticking, so the watchdog deadline starts
	// counting from a point where PlanObserver's kicks are actually due. A
	// no-op when NOTIFY_SOCKET isn't set (dev/test, or a unit still on
	// Type=simple).
	watchdog.Ready()

	log.Printf("lexa-hub: running (engine interval=%s planner replan=%ds debug=%v)",
		cfg.EngineInterval(), cfg.Planner.ReplanIntervalS, cfg.Debug)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("lexa-hub: shutting down")
}

// breachAlert is the per-control compliance-alert edge logic, extracted from the
// plan observer so it is unit-testable. Given the mRID whose breach is currently
// being reported (prevMRID, "" if none) and the latest plan, it returns the alert
// to publish — Active when a control's breach begins OR the breaching control
// changes to a new mRID, !Active when the breach clears — or nil when nothing
// changed (so the same episode is reported once, not per tick). The returned
// string is the new active-breach mRID for the caller to carry forward. Ts is set
// by the caller.
func breachAlert(prevMRID string, plan orchestrator.Plan) (*bus.ComplianceAlert, string) {
	switch {
	// A fast-loop safety plan never evaluates CSIP limits: its nil Breach means
	// "not assessed", not "compliant". Treating it as a clear edge would publish
	// a spurious CannotComply-resolved between economic ticks mid-episode.
	case plan.Safety:
		return nil, prevMRID
	case plan.Breach != nil && plan.Breach.MRID != prevMRID:
		b := plan.Breach
		return &bus.ComplianceAlert{
			Envelope: bus.Envelope{V: bus.ComplianceAlertV},
			MRID:     b.MRID, LimitType: b.LimitType, LimitW: b.LimitW,
			MeasuredW: b.MeasuredW, ShortfallW: b.ShortfallW, Reason: b.Reason,
			Active: true,
		}, b.MRID
	case plan.Breach == nil && prevMRID != "":
		return &bus.ComplianceAlert{Envelope: bus.Envelope{V: bus.ComplianceAlertV}, Active: false}, ""
	default:
		return nil, prevMRID
	}
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
//
// Steps not covered by any tariff interval are filled with the default TOU
// rate for that time of day — never zero, which would tell the planner that
// uncovered hours are free electricity and make it schedule grid charging
// into them.  The export array is nil (no export remuneration data in the
// tariff feed yet); the planner prices exports at zero when the slice is nil.
func pricesFromPricingUpdate(pricing bus.PricingUpdate, now time.Time) (importPrices, exportPrices []float64) {
	const planStepSec = int64(5 * 60)
	const planSteps = 288

	if len(pricing.TariffProfiles) == 0 {
		return nil, nil
	}

	// Snap window start to the 5-min boundary covering now.
	ws := now.Unix() - (now.Unix() % planStepSec)

	importPrices = make([]float64, planSteps)
	fallback := orchestrator.DefaultTOUCostModel()
	for i := range importPrices {
		stepT := ws + int64(i)*planStepSec
		importPrices[i] = fallback.CurrentRate(time.Unix(stepT, 0).Local())
	}

	for _, tp := range pricing.TariffProfiles {
		mult := 1.0
		for i := int8(0); i < tp.PricePowerOfTenMultiplier; i++ {
			mult *= 10
		}
		for i := tp.PricePowerOfTenMultiplier; i < 0; i++ {
			mult /= 10
		}

		for _, rc := range tp.RateComponents {
			allIntervals := make([]bus.TimeTariffMsg, 0, len(rc.ActiveIntervals)+len(rc.ScheduledIntervals))
			allIntervals = append(allIntervals, rc.ActiveIntervals...)
			allIntervals = append(allIntervals, rc.ScheduledIntervals...)
			for _, ti := range allIntervals {
				if len(ti.Blocks) == 0 {
					continue
				}
				// TODO(conformance): verify the /1000 milli-currency assumption
				// against the utility Test Server. IEEE 2030.5 encodes the price
				// scale entirely in PricePowerOfTenMultiplier (already applied
				// via mult); if the server's multiplier yields whole currency
				// units, this extra /1000 understates every price 1000×.
				pricePerKwh := float64(ti.Blocks[0].Price) * mult / 1000
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

	return importPrices, nil
}
