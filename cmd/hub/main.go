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
//	lexa/desired/battery/+    ← desiredPublishingBatteryActuator (reconciler)
//	lexa/desired/solar/+      ← desiredPublishingSolarActuator (reconciler)
//	lexa/desired/evse/+       ← desiredPublishingEVSEActuator (reconciler)
//
// The retained desired-state documents (AD-013) are the authoritative command
// path; the legacy lexa/control/* and lexa/evse/+/command topics were deleted in
// TASK-032 once every device class ran on its reconciler.
//
// Usage:
//
//	lexa-hub [-config /etc/lexa/hub.json]
package main

import (
	"flag"
	"log"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"syscall"
	"time"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/logutil"
	"lexa-hub/internal/metrics"
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

	// TASK-045: install the slog default first, before anything else that
	// might log — every other line in this main() after this point (and
	// every migrated transition site in state.go/actuators.go) goes through
	// this handler. log.Printf call sites this task does not migrate keep
	// working unchanged (slog does not touch the "log" package's output).
	logutil.Setup("lexa-hub", logutil.ParseLevel(cfg.LogLevel))

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
	// lexa_hub_tick_overruns_total: registered now with its real source
	// landing in TASK-046 (no tick-budget accounting exists yet in cmd/hub
	// or internal/orchestrator) — a registered-but-zero counter is fine per
	// this task's own prerequisites note.
	reg.Counter("lexa_hub_tick_overruns_total")
	tickDurationGauge := reg.Gauge("lexa_hub_tick_duration_seconds")
	breachActiveGauge := reg.Gauge("lexa_hub_breach_active")
	breachesTotalCtr := reg.Counter("lexa_hub_breaches_total")
	// lexa_hub_dispatches_total counted the legacy lexa/control/* command
	// publishes, which TASK-032 deleted. Kept registered-but-zero (like
	// lexa_hub_tick_overruns_total) so the scrape surface is stable;
	// lexa_hub_desired_publishes_total is now the live command-dispatch counter.
	reg.Counter("lexa_hub_dispatches_total")
	// desiredPublishesTotalCtr (TASK-027): retained lexa/desired/battery/{device}
	// publishes actually sent by desiredPublishingBatteryActuator (content-change
	// gated, not per-tick — see its doc).
	desiredPublishesTotalCtr := reg.Counter("lexa_hub_desired_publishes_total")

	mqttPass, err := mqttutil.LoadPassword(cfg.MQTTPassFile)
	if err != nil {
		log.Fatalf("lexa-hub: %v", err)
	}
	mc, err := mqttutil.ConnectAuthInstrumented(cfg.MQTTBroker, cfg.MQTTClientID, cfg.MQTTUser, mqttPass, mqttutil.Instrumentation{
		OnPublishFail: mqttFailCtr.Inc,
		OnReconnect:   mqttReconnCtr.Inc,
	})
	if err != nil {
		log.Fatalf("lexa-hub: %v", err)
	}
	defer mc.Disconnect(500)

	// Build the MQTT-backed system reader. The engine interval sizes the CSIP
	// expiry debounce (AD-004/TASK-036: utilitytime.DebouncedExpiry) so it means
	// the same wall-clock seconds at any cadence — see confirmTicksFor in state.go.
	reader := newMQTTSystemReader(cfg.Devices, cfg.EngineInterval())
	// lexa_hub_control_adoption_age_seconds (TASK-044): computed at scrape
	// time from the reader's tracked last-change timestamp (see
	// MQTTSystemReader.ControlAdoptionAge's doc in state.go).
	reg.Collect(func(r *metrics.Registry) {
		r.Gauge("lexa_hub_control_adoption_age_seconds").Set(reader.ControlAdoptionAge(time.Now()).Seconds())
	})

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

	// Compliance-breach reporting (TASK-031). The named breachEpisodes component
	// is the single owner of CannotComply episode state: it merges the
	// optimizer's meter-level breaches (plan.Breach, fed by the plan observer
	// below) AND the reconcilers' device-level non-convergence (bus.ReconcileReport,
	// fed by the retained lexa/reconcile/+/+/report subscription) into ONE episode
	// stream, and emits one Active ComplianceAlert per episode onset (or new-mRID
	// switch) and one !Active on full clear — so the northbound service POSTs
	// exactly one CannotComply Response per real episode rather than one per source
	// or one per tick. It replaces the former activeBreachMRID closure variable +
	// standalone breachAlert func (05 §4: named, testable episode state).
	episodes := newBreachEpisodes()
	// emitAlerts publishes the component's edge alerts and updates the breach
	// observability (lexa_hub_breaches_total per Active edge, lexa_hub_breach_active
	// gauge). Shared by both feed paths (plan observer + report subscription), which
	// run on different goroutines; the component itself is internally locked, and
	// the mqtt client and metrics counters are goroutine-safe, so publishing here
	// (outside the component lock) is race-free.
	emitAlerts := func(alerts []bus.ComplianceAlert) {
		for i := range alerts {
			alert := alerts[i]
			alert.Ts = time.Now().Unix()
			if alert.Active {
				breachesTotalCtr.Inc() // one per breach EDGE, not per tick
				slog.Info("COMPLIANCE BREACH",
					"limit_type", alert.LimitType, "limit_w", alert.LimitW,
					"measured_w", alert.MeasuredW, "shortfall_w", alert.ShortfallW,
					"reason", alert.Reason, "mrid", alert.MRID, "episode", alert.EpisodeID)
			} else {
				slog.Info("compliance breach cleared", "mrid", alert.MRID, "episode", alert.EpisodeID)
			}
			if err := mqttutil.PublishJSON(mc, bus.TopicCSIPComplianceAlert, alert); err != nil {
				log.Printf("lexa-hub: publish compliance alert: %v", err)
			}
		}
		// Reflect the current episode state on EVERY call so the gauge tracks the
		// merged evidence, including ticks/reports that produced no edge.
		if episodes.Active() {
			breachActiveGauge.Set(1)
		} else {
			breachActiveGauge.Set(0)
		}
	}
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

		// lexa_hub_tick_duration_seconds (TASK-044): this closure's own wall
		// time, NOT the full tick — ReadSystemState/Optimize/executePlan run
		// inside internal/orchestrator before PlanObserver is invoked, and
		// that package stays I/O-free / unmodified (05 §1), so their timing
		// isn't observable from here. This still captures a real synchronous
		// cost of every tick (the retained TopicHubPlan publish below, plus
		// the compliance-alert publish on a breach edge) as a proxy pending
		// TASK-046's full-tick timing.
		tickStart := time.Now()
		defer func() { tickDurationGauge.Set(time.Since(tickStart).Seconds()) }()

		// TASK-032: the breach-triggered actuator dedupe reset (ledger L3) was
		// deleted with cmdDeduper. A device that reverts while the commanded value
		// is unchanged is now corrected by the reconciler's verify-by-readback +
		// write-on-diff (bounded by the poll/readback interval, not a 60 s
		// watchdog), and device-level non-convergence rides the breachEpisodes
		// component via lexa/reconcile/+/+/report.

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

		// Feed this plan's meter-level breach evidence to the episode component
		// and publish whatever edge it produces. The component owns the edge
		// semantics (onset / new-mRID re-alert / clear) and the Safety-plan guard
		// (a safety plan's nil Breach is "not assessed", never a clear edge).
		// lexa_hub_breach_active is updated inside emitAlerts on every pass.
		emitAlerts(episodes.OnPlan(plan, time.Now()))
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

	// Subscribe to reconciler reports (TASK-031): device-level non-convergence
	// evidence from the lexa-modbus / lexa-ocpp reconciler shells, RETAINED per
	// device so this feeds the current convergence state after a hub restart.
	// Fed to the same breach-episode component the plan observer feeds, so a
	// device that won't converge under an active control opens/closes the SAME
	// CannotComply episode the optimizer would — one episode per real fault, not
	// one per source. Non-fatal on error: the optimizer's meter-level breach path
	// still functions if this subscription fails to register.
	if err := mqttutil.Subscribe(mc, bus.SubReconcileReport, func(_ string, rep bus.ReconcileReport) {
		emitAlerts(episodes.OnReport(rep, time.Now()))
	}); err != nil {
		log.Printf("lexa-hub: subscribe reconcile reports: %v", err)
	}

	// Wire actuators for each device. TASK-032: the retained desired-doc
	// publisher is the ONLY actuator implementation — the legacy lexa/control/*
	// command path (and its cmdDeduper) was deleted once every class ran on its
	// reconciler. Each command publishes a retained bus.DesiredState the
	// lexa-modbus/lexa-ocpp reconcilers execute.
	for _, dc := range cfg.Devices {
		switch dc.Role {
		case "battery":
			eng.RegisterBatteryActuator(dc.Name, newDesiredPublishingBatteryActuator(mc, dc.Name, desiredPublishesTotalCtr))
		case "inverter":
			eng.RegisterSolarActuator(dc.Name, newDesiredPublishingSolarActuator(mc, dc.Name, desiredPublishesTotalCtr))
		}
	}

	// Wire actuators for known EVSE stations (desired-doc publisher only).
	for _, sc := range cfg.Stations {
		eng.RegisterEVSEActuator(sc.ID, newDesiredPublishingEVSEActuator(mc, sc.ID, desiredPublishesTotalCtr))
	}

	// TASK-044: start serving /metrics before eng.Start() so a scrape during
	// the earliest ticks still sees the registry (StandardGauges/lexa_up are
	// already set regardless of engine state).
	metrics.Serve(cfg.MetricsAddr, reg)

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
