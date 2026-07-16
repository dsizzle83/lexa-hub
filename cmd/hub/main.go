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
//	lexa/openadr/prices      → Engine.SetPrices (WP-15, D9: below CSIP, above
//	                            app/cloud tariff intent — openadr_adopt.go)
//	lexa/openadr/limits      → MQTTSystemReader (WP-15, D9: most-restrictive
//	                            merge with CSIP into GridState)
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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/journal"
	"lexa-hub/internal/logutil"
	"lexa-hub/internal/metrics"
	"lexa-hub/internal/mqttutil"
	"lexa-hub/internal/orchestrator"
	"lexa-hub/internal/orchestrator/constraint"
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

	// TASK-044: metrics registry + standard process gauges. Constructed
	// before the journal below (WS-9.3: journal.Config.Metrics must be set
	// before journal.Open, and Open must run before anything that might
	// emit) so the journal's Writes/Rotations/Errors/Dropped counters can be
	// wired into the same reg every other counter in this file uses.
	reg := metrics.New()
	metrics.StandardGauges(reg)

	// TASK-040: the durable event journal. A nil cfg.Journal (no "journal"
	// block in hub.json) leaves jw nil — every emit call site below is
	// `if jw != nil`-guarded, so this is a true no-op rollout default, not a
	// degraded one. Opened before anything that might emit (MQTT connect,
	// the reader, actuators, breach episodes) so no early transition is lost.
	var jw *journal.Writer
	if cfg.Journal != nil {
		jcfg := cfg.Journal.ToLibrary()
		// WS-9.3: previously nil (log-only drops/writes/rotations/errors —
		// see journal.go's Metrics doc). Wires real counters using the same
		// reg every other cmd/hub/main.go metric uses.
		jcfg.Metrics = &journal.Metrics{
			Writes:    reg.Counter("lexa_hub_journal_writes_total"),
			Rotations: reg.Counter("lexa_hub_journal_rotations_total"),
			Errors:    reg.Counter("lexa_hub_journal_errors_total"),
			Dropped:   reg.Counter("lexa_hub_journal_dropped_total"),
		}
		jw, err = journal.Open(jcfg)
		if err != nil {
			log.Fatalf("lexa-hub: open journal: %v", err)
		}
		defer jw.Close()
		if ev, everr := journal.NewServiceStartEvent("hub", journal.NewServiceStart("", configFingerprint(cfg))); everr == nil {
			_ = jw.Append(ev)
		}
	}

	// WS-8 (V1.0 punch list, TASK-079/GAP-05): additive-only startup
	// assertion that this process's configured zone matches tariff_zone.
	// Logs loud + sets lexa_tariff_zone_mismatch on mismatch; never changes
	// control behavior.
	checkTariffZone(cfg, reg)

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
	// lexa_hub_tick_overruns_total (TASK-046): fires from planObserver below
	// when a pass's measured duration (this pass's own synchronous work plus
	// the PRIOR pass's actuator Apply* time — see tickTiming's doc) exceeds
	// tickBudget. Phase 4 exit criterion: zero under the mqtt-broker-latency
	// scenario in FAST mode, now that the tick path no longer waits on
	// PUBACKs.
	tickOverrunsCtr := reg.Counter("lexa_hub_tick_overruns_total")
	tickDurationGauge := reg.Gauge("lexa_hub_tick_duration_seconds")
	// lexa_hub_desired_publish_failures_total (TASK-046): every desired-doc
	// actuator's harvested async-publish failure or timeout (see
	// desired.go's harvestPending) — the async-era equivalent of the
	// synchronous publish error engine.go used to log straight from
	// ApplyBatteryCommand/ApplySolarCommand/ApplyEVSECommand's return.
	desiredPublishFailuresCtr := reg.Counter("lexa_hub_desired_publish_failures_total")
	// tickTiming accumulates actuator Apply* durations across one pass; see
	// its doc in desired.go for why planObserver reads it one pass lagged.
	tickTime := &tickTiming{}
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
	// lexa_hub_intents_{applied,rejected}_total (Unit 3.3, TASK-082/§3.1):
	// incremented by intentAdopter.adopt per lexa/intent/{kind} outcome — a
	// "duplicate" (retained-redelivery dedupe) increments neither.
	intentsAppliedCtr := reg.Counter("lexa_hub_intents_applied_total")
	intentsRejectedCtr := reg.Counter("lexa_hub_intents_rejected_total")
	// lexa_hub_mode_gateway (Unit 3.4/§3.7): 1 while the gateway plan author is
	// live, 0 in optimizer mode. Set by modeManager on every flip and at boot
	// re-seed, so a scrape always reflects which author is authoring plans.
	modeGatewayGauge := reg.Gauge("lexa_hub_mode_gateway")
	// lexa_hub_forecast_* (Unit 3.6/§3.1): solar-forecast provenance, stamped by
	// planObserver every pass from engine.ForecastSource()/ForecastAgeSeconds().
	//   forecast_external    — 1 while the planner uses a fresh EXTERNAL forecast,
	//                          0 on the diurnal clear-sky fallback.
	//   forecast_age_seconds — age (s) of the external forecast at the last plan;
	//                          -1 means none in effect (documented sentinel, not 0).
	//   forecast_stale       — 1 while a stale external forecast forced the diurnal
	//                          fallback (the edge-alarmed condition), else 0.
	forecastExternalGauge := reg.Gauge("lexa_hub_forecast_external")
	forecastAgeGauge := reg.Gauge("lexa_hub_forecast_age_seconds")
	forecastStaleGauge := reg.Gauge("lexa_hub_forecast_stale")
	staleAlarm := newForecastStaleAlarm(forecastStaleGauge)

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
	reader := newMQTTSystemReader(cfg.Devices, cfg.EngineInterval(), jw)
	// lexa_hub_control_adoption_age_seconds (TASK-044): computed at scrape
	// time from the reader's tracked last-change timestamp (see
	// MQTTSystemReader.ControlAdoptionAge's doc in state.go).
	reg.Collect(func(r *metrics.Registry) {
		r.Gauge("lexa_hub_control_adoption_age_seconds").Set(reader.ControlAdoptionAge(time.Now()).Seconds())
	})

	// TASK-042 (GAP-01/02): bound retained-control staleness at adoption and
	// wire the re-request path. The rewalk handler publishes non-retained,
	// QoS 1 (bus.PubQoS's default for anything outside the measurement
	// plane) — lexa-northbound is the sole subscriber (bus.TopicCSIPRewalk).
	reader.SetRetainedAdoptionMaxAge(cfg.RetainedAdoptionMaxAge())
	reader.SetRewalkHandler(func(reason string) {
		req := bus.RewalkRequest{
			Envelope: bus.Envelope{V: bus.RewalkRequestV},
			Reason:   reason,
			Ts:       time.Now().Unix(),
		}
		if err := mqttutil.PublishJSONQoS(mc, bus.TopicCSIPRewalk, bus.PubQoS(bus.TopicCSIPRewalk), req); err != nil {
			log.Printf("lexa-hub: publish rewalk request (%s): %v", reason, err)
		}
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
	// WP-11: CSIP-AUS dynamic-envelope enforcement (opModGenLimW/opModLoadLimW
	// cascade rules). Default false — the limits are adopted into GridState
	// and shadow-observed regardless; only enforcement is flagged. The gauge
	// makes the flag state scrapeable (the WS-8 lexa_tariff_zone_mismatch
	// flag-visibility pattern).
	opt.EnforceAusLimits = cfg.EnforceAusLimits
	if cfg.EnforceAusLimits {
		reg.Gauge("lexa_hub_aus_limits_enforced").Set(1)
		log.Printf("lexa-hub: CSIP-AUS envelope enforcement ENABLED (enforce_aus_limits=true): opModGenLimW/opModLoadLimW cascade rules live")
	} else {
		reg.Gauge("lexa_hub_aus_limits_enforced").Set(0)
	}

	// TASK-059/FIX-F: the constraint-stack shadow harness + per-constraint
	// off|shadow|active modes (TASK-060 §4 / TASK-061 §4). When
	// cfg.ConstraintShadow is false (the default) NOTHING here runs — the
	// engine drives opt directly and behaviour is byte-identical;
	// constraint_modes is irrelevant in that case (config.go's back-compat
	// rule). When true, modes (resolved below) decides, per constraint,
	// whether it is left out of the candidate Stack entirely ("off"),
	// constructed and observe-only ("shadow"), or constructed AND composed
	// into the actuated plan ("active", constraint/shadow.go Wrapper.compose).
	// shadowDivergences is nil when the flag is off, so the plan-log field
	// below stays absent.
	//
	// UNWIRED as of this task: nothing in configs/hub.json (bench or repo
	// default) sets any key to "active" yet — constraint_shadow ships false,
	// and even a hypothetical constraint_shadow=true default would resolve
	// every key to "shadow" (today's behaviour) absent an explicit
	// constraint_modes block. The first PR that flips an axis "active" pays
	// its own full targeted-scenario + campaign gate (03 §P5), same as every
	// other flip this program has done.
	modes, err := cfg.ResolveConstraintModes()
	if err != nil {
		// loadConfig already validated this; a second failure here would mean
		// cfg was mutated between load and here, which nothing in main does.
		log.Fatalf("lexa-hub: constraint modes: %v", err)
	}
	var optimizer orchestrator.Optimizer = opt
	var shadowDivergences func() uint64
	if cfg.ConstraintShadow {
		// The Stack carries the per-device plant models from hub.json (TASK-057)
		// and, as of TASK-063, the WHOLE controller across all three tiers:
		//   SAFETY     — BatterySafety (TASK-062), a post-arbitration force-disconnect
		//                that reads this tick's resolved setpoint (closing the ≤1-tick
		//                wrong-direction lag) and dominates every tier.
		//   COMPLIANCE — Export (TASK-060) + Gen + Import (TASK-061): the CSIP caps.
		//   ECONOMICS  — Economics (TASK-063): plan-following, self-consumption, TOU
		//                peak, EV allocation, emitted as PointDemands the arbiter
		//                clamps UNDER the caps (economics can never violate a limit).
		// Each is constructed only when its mode is not "off" (FIX-F) — an
		// "off" constraint is left out of the Stack entirely, exactly as if it
		// had never been ported (matching the per-constraint task docs' own
		// "off (default; stack has no export constraint)"). The economics
		// constraint takes the same config cmd/hub gives the legacy optimizer
		// (opt.* above), so the two layers are the same controller modulo the
		// tier seam.
		// One shared EV-resume cooldown (TASK-064): the import constraint writes it,
		// economics reads it — a single counter across the tier seam. Always
		// minted (cheap) even if neither constraint ends up wired.
		evCooldown := constraint.NewEVImportCooldown()

		active := map[string]bool{}
		var constraints []constraint.Constraint
		if m := modes["battery_safety"]; m != ModeOff {
			constraints = append(constraints, constraint.NewBatterySafetyConstraint(opt.SOCReserve))
			if m == ModeActive {
				active["battery-safety"] = true
			}
		}
		if m := modes["export"]; m != ModeOff {
			constraints = append(constraints, constraint.NewExportConstraint())
			if m == ModeActive {
				active["export"] = true
			}
		}
		if m := modes["gen"]; m != ModeOff {
			constraints = append(constraints, constraint.NewGenLimitConstraint())
			if m == ModeActive {
				active["gen"] = true
			}
		}
		if m := modes["import"]; m != ModeOff {
			constraints = append(constraints, constraint.NewImportLimitConstraint(evCooldown))
			if m == ModeActive {
				active["import"] = true
			}
		}
		// WP-11: the CSIP-AUS envelope mirrors. Shadow-observed whenever
		// constraint_shadow is on (an absent constraint_modes block resolves
		// every key to "shadow"), REGARDLESS of enforce_aus_limits — the
		// shadow watches the axis even while the cascade does not enforce it,
		// exactly how the export shadow ran before its flip (architecture §6).
		if m := modes["gen_aus"]; m != ModeOff {
			constraints = append(constraints, constraint.NewAusGenLimitConstraint())
			if m == ModeActive {
				active["gen-aus"] = true
			}
		}
		if m := modes["load_aus"]; m != ModeOff {
			constraints = append(constraints, constraint.NewAusLoadLimitConstraint())
			if m == ModeActive {
				active["load-aus"] = true
			}
		}
		if m := modes["economics"]; m != ModeOff {
			constraints = append(constraints, constraint.NewEconomicsConstraint(
				opt.CostModel,
				opt.SOCReserve,
				opt.SOCFullThreshold,
				opt.ExcessSolarThreshold,
				opt.ExportMarginFrac,
				opt.EVImportCooldownCycles,
				evCooldown,
			))
			if m == ModeActive {
				active["economics"] = true
			}
		}

		stack := constraint.NewStack(buildConstraintPlant(cfg), cfg.EngineInterval(), constraints...)
		stack.SetReserveFloor(opt.SOCReserve) // shared reserve latch tracks the same floor as the authors (audit B-1)
		reg.Counter("lexa_constraint_shadow_divergence_total")
		reg.Counter("lexa_constraint_active_fallback_total")
		wrapper := constraint.Wrap(opt, stack, constraint.Options{
			Now:               time.Now,
			ActiveConstraints: active,
			OnDiverge: func(d constraint.Divergence) {
				if b, err := json.Marshal(d); err == nil {
					// One structured JSON line per divergent tick; the wrapper
					// has already rate-limited to ≤1/min per signature (05 §9).
					slog.Warn("constraint-shadow divergence", "diff", string(b))
				}
			},
			// WS-5.1: a candidate panic must never kill the process
			// controlling hardware. The wrapper recovers, permanently
			// disables candidate observation (latch), and we alarm here —
			// a tripped latch FAILS the soak gate. FIX-F: when any axis is
			// active this latch also forces composition back to pure legacy
			// (ActiveFallbacks) — same alarm, now also a composition event.
			OnPanic: func(recovered any, stack []byte) {
				slog.Error("constraint-shadow candidate PANIC — shadow latched OFF (soak gate fails)",
					"panic", fmt.Sprint(recovered), "stack", string(stack))
			},
		})
		optimizer = wrapper
		shadowDivergences = wrapper.Divergences
		// Mirror the wrapper's running counts into metrics at scrape time
		// (Counter.Set mirrors an external monotonic source — metrics.go).
		// Per-axis counters (WS-5.2) make the flip gate's per-axis carve-outs
		// measurable; the safety counter (WS-5.3) tracks the Tier-1 fast-path
		// shadow diff, which carves out NOTHING. Axis names are sanitized to
		// metric-name charset (small fixed vocabulary).
		reg.Collect(func(r *metrics.Registry) {
			r.Counter("lexa_constraint_shadow_divergence_total").Set(wrapper.Divergences())
			r.Counter("lexa_constraint_shadow_safety_divergence_total").Set(wrapper.SafetyDivergences())
			r.Counter("lexa_constraint_shadow_panics_total").Set(wrapper.Panics())
			var latched uint64
			if wrapper.Latched() {
				latched = 1
			}
			r.Gauge("lexa_constraint_shadow_panic_latched").Set(float64(latched))
			for axis, n := range wrapper.AxisDivergences() {
				name := "lexa_constraint_shadow_divergence_axis_" +
					strings.NewReplacer("-", "_", ":", "_").Replace(axis) + "_total"
				r.Counter(name).Set(n)
			}
			// FIX-F: per-axis legacy-write drop count (composition, active
			// mode only — empty map while every constraint is off/shadow) and
			// the composition fail-safe fallback tally.
			for axis, n := range wrapper.LegacyOverrideDropped() {
				name := "lexa_constraint_legacy_override_dropped_axis_" +
					strings.NewReplacer("-", "_", ":", "_").Replace(axis) + "_total"
				r.Counter(name).Set(n)
			}
			r.Counter("lexa_constraint_active_fallback_total").Set(wrapper.ActiveFallbacks())
		})
		log.Printf("lexa-hub: constraint shadow ENABLED — modes: export=%s gen=%s import=%s economics=%s battery_safety=%s gen_aus=%s load_aus=%s (panic-latch + per-axis + Tier-1 safety diff armed)",
			modes["export"], modes["gen"], modes["import"], modes["economics"], modes["battery_safety"], modes["gen_aus"], modes["load_aus"])
		if len(active) > 0 {
			names := make([]string, 0, len(active))
			for n := range active {
				names = append(names, n)
			}
			sort.Strings(names)
			log.Printf("lexa-hub: *** ACTIVE CONSTRAINT AXES LIVE: %v — these axes are ACTUATED by the constraint stack; the legacy cascade's writes to them are DROPPED (lexa_constraint_legacy_override_dropped_axis_* metrics) ***", names)
		}
	} else if len(cfg.ConstraintModes) > 0 {
		log.Printf("lexa-hub: constraint_modes present but constraint_shadow=false — modes ignored, legacy cascade authoritative on every axis")
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
	//
	// TASK-041: snapPath ("" if cfg.Snapshot is nil/empty) makes the component
	// WRITE an atomic snapshot on every begin/end transition regardless of the
	// restore-enabled flag below — snapshot files appearing with restore still
	// off is the intended write-only soak for one full campaign.
	var snapPath string
	if cfg.Snapshot != nil {
		snapPath = cfg.Snapshot.Path
	}
	episodes := newBreachEpisodes(jw, snapPath)

	// WP-6 (BASIC-027/G31/G32): the CSIP Table 14 LogEvent alarm-edge
	// detector + its async publisher (cmd/hub/logevent.go). Fed from two
	// sources: alarm-bit transitions on the measurement subscription below,
	// and breach-episode edges inside emitAlerts. Publishes edge (never
	// retained) QoS 1 bus.LogEventMsg on lexa/hub/logevent for
	// lexa-northbound's poster; publishes are fire-then-harvest async
	// (TASK-046) so neither feed path ever blocks on a PUBACK.
	logEvents := newLogEventDetector(cfg.LogEventMinInterval())
	logEventsPub := newLogEventPublisher(mc, reg.Counter("lexa_hub_logevents_total"))

	// WP-4 (CORE-009/CORE-014/BASIC-028): the GFEMS dersite aggregator
	// (cmd/hub/dersite.go). Fed from the same measurement/battery-metrics/
	// EVSE-state subscriptions below plus the breach-episode level inside
	// emitAlerts; publishes the retained bus.DERSiteReport on
	// lexa/hub/dersite from its own fixed-cadence goroutine (change-detect +
	// 60 s min republish + heartbeat), async fire-then-harvest (TASK-046) —
	// no feed path or tick ever blocks on it.
	dersite := newDersiteAggregator(mc, cfg, reg.Counter("lexa_hub_dersite_publishes_total"))
	go dersite.loop()

	// WP-9 (standards-buildout C1/C3/C4): the advanced desired-doc author
	// (cmd/hub/adv.go). Constructed ONLY when advanced_der is "on" — flag off
	// means no author, no lexa/csip/curves subscription, and zero
	// bus.DesiredAdvanced publishes (byte-zero behavior change; the
	// constraint_shadow precedent). When on, it consumes the existing
	// lexa/csip/control and lexa/northbound/schedule subscriptions additively
	// (hooks below) plus its own lexa/csip/curves subscription, arbitrates
	// modes per D7, and publishes one retained lexa/desired/adv/{device} doc
	// per inverter/battery. Publishes are async fire-then-harvest (TASK-046)
	// on subscription goroutines and its own ticker — never the engine tick.
	adv := maybeNewAdvAuthor(mc, cfg,
		reg.Counter("lexa_hub_ignored_modes_total"),
		reg.Counter("lexa_hub_desired_adv_publishes_total"),
		desiredPublishFailuresCtr)
	if adv != nil {
		go adv.loop()
		log.Printf("lexa-hub: advanced DER author ENABLED for %d device(s) (retained lexa/desired/adv/+; no reconciler consumes these until WP-10)", len(adv.devices))
	}

	// TASK-041 restore-on-start: gated behind cfg.Snapshot.Enabled (WS-4.1,
	// 2026-07-09: ships true in configs/hub.json after the write-only soak
	// campaign — see SnapshotConfig's doc). This MUST run before the reconciler
	// report subscription and eng.Start() below, and it seeds identity ONLY
	// (activeMRID/episodeID/counter) — never a device command path (grep:
	// nothing here touches an actuator). No ordering assumption is made
	// against the retained-control MQTT re-seed; it arrives whenever it
	// arrives, same as any other restart.
	if cfg.Snapshot != nil && cfg.Snapshot.Enabled && snapPath != "" {
		snap, serr := loadHubSnapshot(snapPath, cfg.Snapshot.maxAge(), time.Now())
		switch {
		case serr == nil && snap.ActiveBreach != nil:
			episodes.Restore(snap.ActiveBreach.MRID, snap.ActiveBreach.EpisodeID, snap.ActiveBreach.Counter)
			log.Printf("lexa-hub: restored breach episode %s (mrid=%s) from snapshot %s",
				snap.ActiveBreach.EpisodeID, snap.ActiveBreach.MRID, snapPath)
			if jw != nil {
				if ev, everr := journal.NewSnapshotRestoredEvent("hub",
					journal.NewSnapshot(snapPath, snap.ActiveBreach.EpisodeID)); everr == nil {
					_ = jw.Append(ev)
					_ = jw.Flush()
				}
			}
		case serr == nil:
			// A valid, fresh snapshot with no active episode: nothing to seed,
			// but still worth journaling that restore ran (forensics parity
			// with the seeded case).
			if jw != nil {
				if ev, everr := journal.NewSnapshotRestoredEvent("hub", journal.NewSnapshot(snapPath, "")); everr == nil {
					_ = jw.Append(ev)
					_ = jw.Flush()
				}
			}
		case os.IsNotExist(serr):
			// First boot, or a fresh volume: not an error, nothing to log.
		default:
			// Corrupt, wrong version, or stale/future-dated: never trusted
			// (§8.3 stale-state hazard) — the hub starts as if no snapshot
			// existed at all.
			slog.Warn("lexa-hub: snapshot restore skipped", "path", snapPath, "err", serr)
		}
	}
	// TASK-041: refresh the breach snapshot's written_at every 60 s while an
	// episode is open, so a breach that legitimately outlasts max_age_s (the
	// 300 s default) doesn't go stale in the eyes of a LATER restart's
	// staleness check. Independent fixed-cadence goroutine, not a per-tick
	// hook (RSK-14) — ResaveIfActive itself is a cheap no-op whenever no
	// snapshot path is configured or no episode is open.
	if snapPath != "" {
		go func() {
			ticker := time.NewTicker(60 * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				episodes.ResaveIfActive()
			}
		}()
	}
	// emitAlerts publishes the component's edge alerts and updates the breach
	// observability (lexa_hub_breaches_total per Active edge, lexa_hub_breach_active
	// gauge). Shared by both feed paths (plan observer + report subscription), which
	// run on different goroutines; the component itself is internally locked, and
	// the mqtt client and metrics counters are goroutine-safe, so publishing here
	// (outside the component lock) is race-free.
	//
	// TASK-046: this publish stays SYNCHRONOUS, unlike the plan log below and
	// every actuator publish — but bounded at complianceAlertTimeout (1s), not
	// mqttutil's 5s default. A compliance alert is rare (one per breach EDGE)
	// and its ordering/latency against the CannotComply episode matters more
	// than sparing this pass's tick budget: the northbound service's Response
	// POST depends on seeing Active=true promptly and exactly once per
	// episode (see the "must NOT change" note on breach alert edge semantics
	// in TASK-046's task file). A 5s worst case on a rare, already-alerted
	// edge is an acceptable trade against reintroducing that stall on every
	// tick's actuator publishes; 1s bounds even that rare case tighter than
	// today's default did.
	const complianceAlertTimeout = 1 * time.Second
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
			if err := mqttutil.PublishJSONTimeout(mc, bus.TopicCSIPComplianceAlert, false, alert, complianceAlertTimeout); err != nil {
				log.Printf("lexa-hub: publish compliance alert: %v", err)
			}
			// WP-6: mirror the episode edge as a CSIP Table 14 LogEvent pair
			// (EMERGENCY_REMOTE alarm at onset, RTN at clear — see
			// logEventDetector.OnBreachAlert for the mapping rationale).
			// Async publish; never blocks the tick path.
			logEventsPub.publish(logEvents.OnBreachAlert(alert, time.Now()))
		}
		// Reflect the current episode state on EVERY call so the gauge tracks the
		// merged evidence, including ticks/reports that produced no edge.
		active := episodes.Active()
		if active {
			breachActiveGauge.Set(1)
		} else {
			breachActiveGauge.Set(0)
		}
		// WP-4: mirror the same merged level into the dersite status block's
		// alarm bitmap (EMERGENCY_REMOTE category — the LEVEL counterpart of
		// the WP-6 LogEvent edges emitted above).
		dersite.SetBreachActive(active)
	}

	// planLogPending is the previous pass's plan-log publish, harvested at
	// the top of the NEXT pass (TASK-046) — see the harvest call below.
	// Owned entirely by planObserver, which the engine only ever calls from
	// its single control goroutine (tick()/safetyTick()), so no lock is
	// needed even though it is closed over here.
	var planLogPending *mqttutil.PendingPub

	// openADRGateWasActive edge-tracks the WP-15 CannotComply gate below (D9's
	// last bullet) so its WARN log fires once per transition, not every tick a
	// gated breach persists — same single-writer-closure reasoning as
	// planLogPending above.
	var openADRGateWasActive bool

	// Plan-log enrichment seams (Unit 3.6). planObserver is defined here, before
	// modeMgr/eng exist, so it reaches them through these function values —
	// harmless no-op defaults until they are wired to modeMgr.Mode /
	// eng.ForecastSource / eng.ForecastAgeSeconds just after those are built.
	// The engine only calls PlanObserver from its control goroutine, which does
	// not start until eng.Start() (well after the wiring below), so the closure
	// always sees the real accessors — the happens-before is the goroutine spawn.
	modeOf := func() string { return "optimizer" }
	forecastSourceOf := func() string { return "" }
	forecastAgeOf := func() int64 { return -1 }

	// settingsRefresh is the GAP-8 read-back seam (settings.go): the plan
	// observer pokes it every pass so the retained lexa/hub/settings doc's
	// effective reserve pct tracks the plan. No-op until wired to the real
	// publisher below (same forward-declaration pattern as modeOf/forecast* —
	// the engine only calls PlanObserver after eng.Start(), well after wiring).
	settingsRefresh := func() {}

	// scheduleRefresh is the GAP-7 read-back seam (schedule.go): the plan
	// observer pokes it every pass so the retained lexa/hub/schedule doc tracks
	// the latest plan (deduped inside on the plan's build time). Same
	// forward-declaration/no-op-until-wired pattern as settingsRefresh.
	scheduleRefresh := func() {}

	// tickBudget is this task's Phase 4 exit-criterion threshold: half the
	// economic engine interval. Exceeding it — measured as this pass's own
	// synchronous work plus the PRIOR pass's actuator Apply* time, see
	// tickTiming's doc in desired.go — increments lexa_hub_tick_overruns_total.
	// 50% leaves headroom for the read/optimize/plan work internal/orchestrator
	// does BEFORE calling PlanObserver (unmeasured here — that package stays
	// I/O-free and unmodified, 05 §1) while still catching a publish path
	// that has started eating meaningfully into the tick.
	tickBudget := time.Duration(float64(cfg.EngineInterval()) * 0.5)

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

		// lexa_hub_tick_duration_seconds / lexa_hub_tick_overruns_total
		// (TASK-044/TASK-046): ownElapsed is this closure's own wall time —
		// ReadSystemState/Optimize/executePlan run inside internal/orchestrator
		// before PlanObserver is invoked, and that package stays I/O-free /
		// unmodified (05 §1), so their timing isn't observable from here.
		// actuatorElapsed is the PRIOR pass's total actuator Apply* time (this
		// pass's own executePlan — which calls Apply* — hasn't run yet; see
		// engine.go tick()/safetyTick() calling PlanObserver BEFORE
		// executePlan). The reported total is therefore one pass lagged
		// relative to a single contiguous measurement, which is unavoidable
		// without touching internal/orchestrator; a sustained wedge still
		// shows up within one extra pass, which an overrun COUNTER (as
		// opposed to a hard deadline) can tolerate.
		tickStart := time.Now()
		defer func() {
			ownElapsed := time.Since(tickStart)
			actuatorElapsed := tickTime.takeReset()
			total := ownElapsed + actuatorElapsed
			tickDurationGauge.Set(total.Seconds())
			if total > tickBudget {
				tickOverrunsCtr.Inc()
				slog.Warn("lexa-hub: tick budget exceeded",
					"duration", total, "budget", tickBudget,
					"own", ownElapsed, "actuator_prior_pass", actuatorElapsed)
			}
		}()

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
		// TASK-059: surface the running shadow-divergence count on every plan
		// publish so the dashboard/QA can watch the diff rate without scraping
		// /metrics. Nil closure (flag off) ⇒ field stays zero ⇒ omitted.
		if shadowDivergences != nil {
			pl.ShadowDivergences = shadowDivergences()
		}
		// Unit 3.6 observability: stamp the live plan author + solar-forecast
		// provenance onto the plan log, mirror them onto the forecast gauges, and
		// edge-alarm a stale external forecast (the 3.1 finding, surfaced here in
		// cmd/hub rather than per-tick inside the radioactive orchestrator).
		fSource := forecastSourceOf()
		fAge := forecastAgeOf()
		enrichPlanLog(&pl, modeOf(), fSource, fAge)
		forecastAgeGauge.Set(float64(fAge))
		if fSource == "external" {
			forecastExternalGauge.Set(1)
		} else {
			forecastExternalGauge.Set(0)
		}
		staleAlarm.observe(fSource, fAge)
		for _, dec := range plan.Decisions {
			pl.Decisions = append(pl.Decisions, bus.PlanDecision{
				Rule: dec.Rule, Reason: dec.Reason, Impact: dec.Impact,
			})
		}
		// TASK-046: async, harvested at the top of the NEXT pass — the plan
		// log carries no dedupe baseline to roll back (it publishes
		// unconditionally every pass, decisions or not), so a harvested
		// failure/timeout is just logged: a dropped/late plan log is
		// refreshed at most one pass later (retained topic, last write
		// wins), which is already the observability contract ("the
		// timestamp doubles as an engine heartbeat" above) — a single
		// missed heartbeat, not a missed command.
		if planLogPending != nil {
			if done, timedOut, err := planLogPending.Harvest(mqttutil.PublishTimeout); done || timedOut {
				switch {
				case err != nil:
					log.Printf("lexa-hub: publish plan log: %v (async)", err)
				case timedOut:
					log.Printf("lexa-hub: publish plan log: no ack after %s (async)", mqttutil.PublishTimeout)
				}
				planLogPending = nil
			}
		}
		if pp, err := mqttutil.PublishJSONRetainedAsync(mc, bus.TopicHubPlan, pl); err != nil {
			log.Printf("lexa-hub: publish plan log: %v", err)
		} else {
			planLogPending = pp
		}

		// GAP-8: refresh the retained lexa/hub/settings effective reserve pct if
		// this plan moved it (deduped inside — most passes are a no-op). Cheap
		// and off the actuator path; a failed publish only logs (the retained
		// doc is refreshed next change/pass).
		settingsRefresh()

		// GAP-7: refresh the retained lexa/hub/schedule series if the planner has
		// produced a NEW plan (deduped inside on build time — most passes are a
		// no-op). Same off-actuator-path, log-only-on-failure discipline.
		scheduleRefresh()

		// Feed this plan's meter-level breach evidence to the episode component
		// and publish whatever edge it produces. The component owns the edge
		// semantics (onset / new-mRID re-alert / clear) and the Safety-plan guard
		// (a safety plan's nil Breach is "not assessed", never a clear edge).
		// lexa_hub_breach_active is updated inside emitAlerts on every pass.
		//
		// WP-15 hub-adoption slice (D9's last bullet): a breach bound SOLELY by
		// an OpenADR-tighter capacity limit must never produce a 2030.5
		// CannotComply — reader.OpenADRBoundAxis reports that per axis from this
		// SAME tick's ReadSystemState pass (see openadr_adopt.go). Only the
		// optimizer's meter-level plan.Breach is gated here; breach.go itself is
		// untouched, and the reconciler's device-level NonConverged evidence
		// (fed separately via OnReport) is NOT gated — see openadr_adopt.go's
		// OpenADRBoundAxis doc and this task's report for why that path is left
		// as attribute-and-document rather than a clean gate.
		breachForEpisodes := plan
		if plan.Breach != nil && reader.OpenADRBoundAxis(plan.Breach.LimitType) {
			if !openADRGateWasActive {
				openADRGateWasActive = true
				slog.Warn("lexa-hub: breach bound solely by an OpenADR capacity limit — suppressing CannotComply episode evidence (CSIP is not the binding cap; OpenADR opt-out reporting owns this)",
					"limit_type", plan.Breach.LimitType, "limit_w", plan.Breach.LimitW, "measured_w", plan.Breach.MeasuredW)
			}
			breachForEpisodes.Breach = nil
		} else {
			openADRGateWasActive = false
		}
		emitAlerts(episodes.OnPlan(breachForEpisodes, time.Now()))
	}

	// Unit 3.4 (§3.5): the mode manager is the runtime switch between the two
	// plan authors. Its optimizer-mode author is EXACTLY what was passed to
	// orchestrator.New before this unit (`optimizer` above — the raw
	// DefaultOptimizer, or the TASK-059 shadow wrapper when constraint_shadow is
	// on). Its gateway-mode author is a constraint.Stack whose economics slot is
	// CSIPPassthrough (Unit 3.5), reusing the EXACT compliance constraints the
	// shadow wiring builds (BatterySafety/Export/GenLimit/ImportLimit) with
	// Economics swapped for the passthrough — so the site ExpLimW/ImpLimW/MaxLimW
	// envelopes are narrowed by the same shadow-validated code in both modes. The
	// gateway stack gets its OWN EV-import cooldown (the shadow stack's is a
	// separate instance); CSIPPassthrough does not read it — only ImportLimit
	// does. Built unconditionally (cheap) but only ever run when Mode()=="gateway".
	//
	// Tier-1 safety ALWAYS routes to the LEGACY safety evaluator, NEVER the
	// gateway stack: a protection relay is mode-invariant (ADR-0001; ecosystem
	// roadmap §14). See modeManager.EvaluateSafety.
	//
	// The gateway stack deliberately IGNORES constraint_modes (FIX-F): that map
	// governs the shadow/composition program's per-axis rollout against the
	// legacy cascade; in gateway mode the stack IS the CSIP author, so it always
	// carries the full constraint set — an axis can't be "off" for the author
	// of record.
	gatewayStack := constraint.NewStack(buildConstraintPlant(cfg), cfg.EngineInterval(),
		constraint.NewBatterySafetyConstraint(opt.SOCReserve),
		constraint.NewExportConstraint(),
		constraint.NewGenLimitConstraint(),
		constraint.NewImportLimitConstraint(constraint.NewEVImportCooldown()),
		constraint.NewCSIPPassthrough(cfg.GatewayPolicy()))
	gatewayStack.SetReserveFloor(opt.SOCReserve) // shared reserve latch tracks the same floor as the authors (audit B-1)
	// The safety delegate is whatever the engine's fast loop would have used
	// BEFORE the mode manager existed: the shadow wrapper when
	// constraint_shadow is on, else the raw optimizer (principal review,
	// unit 3.4). The wrapper's EvaluateSafety returns the LEGACY plan
	// unmodified by contract (TASK-059/shadow.go), so mode-invariance holds
	// through it — and wiring it here preserves WS-5.3's Tier-1 safety
	// shadow-diff telemetry during constraint-stack soaks, which routing to
	// the raw opt would have silently disconnected.
	safetyEval := orchestrator.SafetyEvaluator(opt)
	if se, ok := optimizer.(orchestrator.SafetyEvaluator); ok {
		safetyEval = se
	}
	modeMgr := newModeManager(cfg.Mode, optimizer, gatewayStack, safetyEval, jw, mc, modeGatewayGauge)

	// THE one structural touch (§3.5): the engine's optimizer argument is the
	// mode manager, not the bare optimizer. The engine type-asserts this arg to
	// orchestrator.SafetyEvaluator to wire its fast loop (engine.go); modeMgr
	// satisfies both Optimizer and SafetyEvaluator, and wraps whatever was passed
	// before — so this changes which AUTHOR runs (mode-gated) without changing the
	// engine, the reader wiring, or the fast-loop gating.
	eng := orchestrator.New(reader, modeMgr, orchestrator.Config{
		Interval:       cfg.EngineInterval(),
		SafetyInterval: cfg.SafetyInterval(),
		Debug:          cfg.Debug,
		Planner:        cfg.Planner,
		PlanObserver:   planObserver,
		EVStorage:      cfg.EVStorage,
	})
	modeMgr.setEngine(eng) // post-construction: modeMgr.request pokes eng.Wake on a flip

	// Wire the Unit 3.6 plan-log enrichment seams now that both authors exist
	// (see their forward declaration above planObserver). Reassigned before
	// eng.Start(), so planObserver reads the real accessors on its first pass.
	modeOf = modeMgr.Mode
	forecastSourceOf = eng.ForecastSource
	forecastAgeOf = eng.ForecastAgeSeconds

	// GAP-8: the retained lexa/hub/settings publisher. Reads the effective
	// reserve pct from the engine and the configured floor from cfg; the intent
	// adopter (wired below) feeds it the reserve source + tariff spec. Its
	// refresh is the seam the plan observer pokes above.
	settings := newSettingsPublisher(mc, eng, cfg.Planner.TerminalReservePct)
	settingsRefresh = settings.refreshFromPlan

	// GAP-7: the retained lexa/hub/schedule publisher. Reads the plan snapshot
	// (plan + the forecast it used + capacity/voltage) from the engine and keys
	// the ev_plan series by the first configured station id. Its refresh is the
	// seam the plan observer pokes above (deduped on the plan's build time).
	stationIDs := make([]string, 0, len(cfg.Stations))
	for _, sc := range cfg.Stations {
		stationIDs = append(stationIDs, sc.ID)
	}
	schedule := newSchedulePublisher(mc, eng, stationIDs)
	scheduleRefresh = schedule.refresh

	// lexa_hub_engine_cmd_dropped_total (WS-9.3): mirrors Engine.CmdDropped()
	// (internal/orchestrator stays decoupled from internal/metrics — same
	// stance as internal/mqttutil) — the same external-mirroring idiom
	// already used for lexa_hub_tick_overruns_total via planObserver above.
	reg.Collect(func(r *metrics.Registry) {
		r.Counter("lexa_hub_engine_cmd_dropped_total").Set(eng.CmdDropped())
	})

	// Subscribe to all state topics. The measurement handler also feeds the
	// WP-6 alarm-edge detector (alarm_bits transitions → lexa/hub/logevent);
	// the detector is a pure diff against its own baseline and the publish is
	// async, so this adds no blocking to the measurement path.
	if err := mqttutil.Subscribe(mc, bus.SubMeasurements, func(topic string, msg bus.Measurement) {
		reader.onMeasurement(topic, msg)
		logEventsPub.publish(logEvents.OnMeasurement(msg, time.Now()))
		// WP-4: the dersite aggregator stores the slice it needs (W + alarm
		// bits) under its own lock; no publish happens on this goroutine.
		dersite.OnMeasurement(msg)
	}); err != nil {
		log.Fatalf("lexa-hub: subscribe measurements: %v", err)
	}
	if err := mqttutil.Subscribe(mc, bus.SubBattMetrics, func(topic string, msg bus.BattMetrics) {
		reader.onBattMetrics(topic, msg)
		dersite.OnBattMetrics(msg) // WP-4: SOC/capacity/rate feed
	}); err != nil {
		log.Fatalf("lexa-hub: subscribe batt metrics: %v", err)
	}
	// TASK-042 (GAP-02): SubscribeDecodeErr instead of Subscribe on this one
	// topic so a corrupted retained payload — previously a silent
	// log-and-drop with no recovery until the next successful northbound
	// walk republished (potentially never, during a WAN outage) — alarms and
	// asks lexa-northbound to republish immediately via
	// reader.RequestRewalk("decode") (rate-limited jointly with the
	// stale-adoption path in state.go). The handler (successful-decode) path
	// is byte-identical to before.
	if err := mqttutil.SubscribeDecodeErr(mc, bus.TopicCSIPControl, func(topic string, msg bus.ActiveControl) {
		reader.onCSIPControl(topic, msg)
		// WP-9: feed the advanced desired-doc author additively (nil when
		// advanced_der is off). Its evaluation publishes async only — this
		// callback never blocks on a PUBACK.
		if adv != nil {
			adv.OnControl(msg)
		}
		// A disconnect order must not wait out the ticker interval: force an
		// immediate tick so cease-to-energize is applied within MQTT latency.
		if msg.Connect != nil && !*msg.Connect {
			eng.Wake()
		}
	}, func(_ string, _ []byte, err error) {
		log.Printf("[hub] retained CSIP control payload undecodable: %v — requesting re-publish", err)
		reader.RequestRewalk("decode")
	}); err != nil {
		log.Fatalf("lexa-hub: subscribe csip control: %v", err)
	}
	if err := mqttutil.Subscribe(mc, bus.SubEVSEState, func(topic string, msg bus.EVSEState) {
		reader.onEVSEState(topic, msg)
		dersite.OnEVSEState(msg) // WP-4: stored, inert until ev_storage (D2)
	}); err != nil {
		log.Fatalf("lexa-hub: subscribe evse state: %v", err)
	}

	// Subscribe to northbound DER schedule → extract DER constraints for planner.
	if err := mqttutil.Subscribe(mc, bus.TopicNorthboundSchedule, func(_ string, sched bus.DERScheduleMsg) {
		eng.SetDERConstraints(derConstraintsFromSchedule(sched, cfg.EnforceAusLimits))
		// WP-9: the schedule is the ONLY bus carriage for opModFreqDroop
		// parameters (WP-8's carriage seam — see droopFromSchedule in adv.go);
		// the adv author correlates them to the active control by MRID.
		if adv != nil {
			adv.OnSchedule(sched)
		}
	}); err != nil {
		log.Fatalf("lexa-hub: subscribe northbound schedule: %v", err)
	}

	// WP-9: the adv author's own curve-content subscription (retained
	// bus.CurveSet, correlated to the control by curve_set_id). Subscribed
	// only when the author exists — flag off leaves the hub's subscription
	// set byte-identical to pre-WP-9.
	if adv != nil {
		if err := mqttutil.Subscribe(mc, bus.TopicCSIPCurves, func(_ string, cs bus.CurveSet) {
			adv.OnCurves(cs)
		}); err != nil {
			log.Fatalf("lexa-hub: subscribe csip curves: %v", err)
		}
	}

	// WP-15 hub-adoption slice (D9 price precedence: CSIP tariff > OpenADR CP
	// prices > app/cloud tariff intent — cmd/hub/openadr_adopt.go). Built
	// unconditionally (cheap) so the CSIP handler below can always mark it;
	// only the lexa/openadr/* SUBSCRIPTIONS are gated on openadr_adopt.
	openadr := newOpenADRAdopter(cfg.OpenADRPriceMaxAge())

	// Subscribe to live pricing → extract per-step prices for planner.
	if err := mqttutil.Subscribe(mc, bus.TopicCSIPPricing, func(_ string, pricing bus.PricingUpdate) {
		imp, exp := pricesFromPricingUpdate(pricing, time.Now())
		if imp != nil {
			openadr.MarkCSIPPriceSeen() // D9: CSIP wins outright and keeps winning
			eng.SetPrices(imp, exp)
		}
	}); err != nil {
		log.Fatalf("lexa-hub: subscribe csip pricing: %v", err)
	}

	// WP-15 hub-adoption slice: lexa/openadr/prices and lexa/openadr/limits.
	// Gated on openadr_adopt (default true; a config-level off-switch, not a
	// safety concern — adopting a retained doc is passive consumption and
	// lexa-openadr ships uncommissioned) — mirrors the `if adv != nil` gating
	// pattern just above for the WP-9 curves subscription.
	if cfg.OpenADRAdoptEnabled() {
		if err := mqttutil.Subscribe(mc, bus.TopicOpenADRPrices, func(_ string, msg bus.OpenADRPrices) {
			if imp, exp, ok := openadr.AdoptPrices(msg, time.Now()); ok {
				eng.SetPrices(imp, exp)
			}
		}); err != nil {
			log.Fatalf("lexa-hub: subscribe openadr prices: %v", err)
		}
		if err := mqttutil.Subscribe(mc, bus.TopicOpenADRLimits, func(topic string, msg bus.OpenADRLimits) {
			reader.onOpenADRLimits(topic, msg)
		}); err != nil {
			log.Fatalf("lexa-hub: subscribe openadr limits: %v", err)
		}
	}

	// Intent adoption layer (Unit 3.3, TASK-082/DEVICE_ROADMAP.md §3.1): seven
	// explicit subscribe blocks — never a "lexa/intent/+" wildcard, since
	// that would also match lexa/intent/result (topics.go's doc). The seventh
	// kind, "mode" (Unit 3.4), funnels through the SAME adopter via applyMode →
	// modeMgr.request; wire adopter.modes here so applyMode can reach the manager.
	adopter := newIntentAdopter(eng, opt, jw, mc, cfg, intentsAppliedCtr, intentsRejectedCtr)
	adopter.modes = modeMgr
	adopter.settings = settings
	// GAP-8 seed: publish an initial retained lexa/hub/settings so lexa-api has a
	// value the moment it subscribes (effective pct is nil until the first plan;
	// source "default"/"csip"). A retained reserve/tariff intent redelivered
	// during the subscribe→Start window republishes it with the real source.
	settings.publish()
	if err := mqttutil.Subscribe(mc, bus.TopicIntentEVGoal, func(_ string, msg bus.EVGoalIntent) {
		adopter.adopt("evgoal", msg.IntentMeta, func() (string, string) { return adopter.applyEVGoal(msg) })
	}); err != nil {
		log.Fatalf("lexa-hub: subscribe intent evgoal: %v", err)
	}
	if err := mqttutil.Subscribe(mc, bus.TopicIntentReserve, func(_ string, msg bus.BackupReserveIntent) {
		adopter.adopt("reserve", msg.IntentMeta, func() (string, string) { return adopter.applyReserve(msg) })
	}); err != nil {
		log.Fatalf("lexa-hub: subscribe intent reserve: %v", err)
	}
	if err := mqttutil.Subscribe(mc, bus.TopicIntentTariff, func(_ string, msg bus.TariffIntent) {
		adopter.adopt("tariff", msg.IntentMeta, func() (string, string) { return adopter.applyTariff(msg) })
	}); err != nil {
		log.Fatalf("lexa-hub: subscribe intent tariff: %v", err)
	}
	if err := mqttutil.Subscribe(mc, bus.TopicIntentSolarForecast, func(_ string, msg bus.SolarForecastIntent) {
		adopter.adopt("solarforecast", msg.IntentMeta, func() (string, string) { return adopter.applySolarForecast(msg) })
	}); err != nil {
		log.Fatalf("lexa-hub: subscribe intent solarforecast: %v", err)
	}
	if err := mqttutil.Subscribe(mc, bus.TopicIntentLoadProfile, func(_ string, msg bus.LoadProfileIntent) {
		adopter.adopt("loadprofile", msg.IntentMeta, func() (string, string) { return adopter.applyLoadProfile(msg) })
	}); err != nil {
		log.Fatalf("lexa-hub: subscribe intent loadprofile: %v", err)
	}
	if err := mqttutil.Subscribe(mc, bus.TopicIntentChargeNow, func(_ string, msg bus.ChargeNowIntent) {
		adopter.adopt("chargenow", msg.IntentMeta, func() (string, string) { return adopter.applyChargeNow(msg) })
	}); err != nil {
		log.Fatalf("lexa-hub: subscribe intent chargenow: %v", err)
	}
	// Seventh intent kind, "mode" (Unit 3.4/§3.5): funnels through the SAME
	// adopter (ID-dedupe + IntentResult + generic intent_applied/rejected), but
	// applyMode delegates the transition to modeMgr.request (mode_change journal,
	// retained lexa/hub/mode status, eng.Wake).
	if err := mqttutil.Subscribe(mc, bus.TopicIntentMode, func(_ string, msg bus.ModeIntent) {
		adopter.adopt("mode", msg.IntentMeta, func() (string, string) { return adopter.applyMode(msg) })
	}); err != nil {
		log.Fatalf("lexa-hub: subscribe intent mode: %v", err)
	}
	// Boot re-seed (§3.5): subscribe the retained lexa/hub/mode status BEFORE
	// eng.Start() so the persisted mode is adopted (silently, mode.Store only)
	// before the control loop reads it. modeMgr.SealBoot() (after Start) then
	// closes this window — the hub is the sole writer of its own mode, so a
	// later/echoed message on this topic must not flip it (only the intent path
	// does). Precedence: this retained value ▸ cfg.Mode ▸ "optimizer".
	if err := mqttutil.Subscribe(mc, bus.TopicHubMode, func(_ string, msg bus.ModeStatus) {
		modeMgr.onModeStatus(msg)
	}); err != nil {
		log.Fatalf("lexa-hub: subscribe hub mode: %v", err)
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
			eng.RegisterBatteryActuator(dc.Name, newDesiredPublishingBatteryActuator(mc, dc.Name, desiredPublishesTotalCtr, desiredPublishFailuresCtr, tickTime, jw))
		case "inverter":
			eng.RegisterSolarActuator(dc.Name, newDesiredPublishingSolarActuator(mc, dc.Name, desiredPublishesTotalCtr, desiredPublishFailuresCtr, tickTime, jw))
		}
	}

	// Wire actuators for known EVSE stations (desired-doc publisher only).
	for _, sc := range cfg.Stations {
		evseAct := newDesiredPublishingEVSEActuator(mc, sc.ID, desiredPublishesTotalCtr, desiredPublishFailuresCtr, tickTime, jw)
		// WP-13 (B3): a command at/above the station's rated maximum is a
		// release and publishes the explicit bus.RestoreCurrentA sentinel —
		// see the actuator's ratedMaxA field doc.
		evseAct.ratedMaxA = sc.MaxCurrentA
		eng.RegisterEVSEActuator(sc.ID, evseAct)
	}

	// TASK-044: start serving /metrics before eng.Start() so a scrape during
	// the earliest ticks still sees the registry (StandardGauges/lexa_up are
	// already set regardless of engine state).
	metrics.Serve(cfg.MetricsAddr, reg)

	eng.Start()
	defer eng.Stop()

	// Boot re-seed window closes now (§3.5): the retained lexa/hub/mode message,
	// if any, was delivered on the subscription above during the subscribe→Start
	// window and has already re-seeded modeMgr's mode. After this, onModeStatus
	// ignores every lexa/hub/mode message — only the intent path flips the mode.
	modeMgr.SealBoot()

	// GAP-7 seed: publish the retained lexa/hub/schedule once now that the
	// planner goroutine has been started (its initial replan runs immediately),
	// so lexa-api has a series soon after subscribing. A no-op if the first plan
	// hasn't resolved yet — the plan observer's scheduleRefresh() then catches it
	// on an upcoming tick (deduped on build time so this seed never double-publishes).
	schedule.refresh()

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

// enrichPlanLog stamps the Unit 3.6 observability fields — the live plan author
// (mode) and the solar-forecast provenance (source + age) — onto a PlanLog.
// Pure and additive: the wire version is unchanged (PlanLogV) and every field
// is omitempty, so an optimizer-mode hub with no external forecast produces
// JSON byte-identical to pre-3.6 EXCEPT for the always-present "mode" key (a
// live hub always sets a non-empty mode). Kept a free function so plan_test.go
// can pin the stamping at a clean seam without standing up an Engine.
func enrichPlanLog(pl *bus.PlanLog, mode, forecastSource string, forecastAgeS int64) {
	pl.Mode = mode
	pl.ForecastSource = forecastSource
	pl.ForecastAgeS = forecastAgeS
}

// forecastStaleAlarm edge-triggers a single WARN when the planner falls back to
// the diurnal clear-sky curve while a (too-old) EXTERNAL solar forecast is still
// on the books — the Unit 3.1 finding, surfaced here in cmd/hub's planObserver
// rather than as a per-tick log inside the radioactive orchestrator package. It
// latches so the warn fires ONCE per stale episode and re-arms when the source
// recovers (a fresh external forecast, or no external forecast at all). It also
// mirrors the condition onto lexa_hub_forecast_stale (0/1).
type forecastStaleAlarm struct {
	latched bool
	gauge   *metrics.Gauge                // lexa_hub_forecast_stale; nil-safe
	warn    func(msg string, args ...any) // slog.Warn in production; a test seam
}

// newForecastStaleAlarm builds the alarm around a (nil-safe) stale gauge.
func newForecastStaleAlarm(gauge *metrics.Gauge) *forecastStaleAlarm {
	return &forecastStaleAlarm{gauge: gauge, warn: slog.Warn}
}

// observe applies this pass's forecast source/age. "stale" is a diurnal
// fallback WITH a recorded external-forecast age (>= 0): a real forecast exists
// but was rejected as too old. It returns whether it emitted a NEW edge warn
// this call (for tests; production ignores the return).
func (a *forecastStaleAlarm) observe(source string, ageS int64) bool {
	stale := source == "diurnal" && ageS >= 0
	if a.gauge != nil {
		if stale {
			a.gauge.Set(1)
		} else {
			a.gauge.Set(0)
		}
	}
	switch {
	case stale && !a.latched:
		a.warn("lexa-hub: solar forecast stale — planner using diurnal fallback",
			"forecast_source", source, "forecast_age_s", ageS)
		a.latched = true
		return true
	case !stale && a.latched:
		a.latched = false
	}
	return false
}

// configFingerprint returns a short, deterministic hash of cfg's JSON
// encoding for the journal's service_start ConfigHash field (TASK-040):
// there is no build-version string in this repo yet, so a hash of the
// effective (post-default) config is the cheapest "what was I running with"
// breadcrumb a journal reader can use to notice a config change between
// restarts, without needing to diff /etc/lexa/hub.json by hand.
func configFingerprint(cfg *Config) string {
	b, err := json.Marshal(cfg)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

// buildConstraintPlant assembles the constraint.Plant (TASK-057 plant models,
// keyed by device/station name) the shadow Stack reads, from the decoded plant
// blocks in hub.json. Devices without a plant block contribute a zero-value
// plant; the constraint layer applies its bench defaults at consume time
// (plantmodel withDefaults), so a partial or absent block is safe. The Stack is
// constraint-free today, so this map is unused until TASK-060 wires the export
// constraint — building it now keeps the shadow harness ready to carry real
// plant physics into that flip's bench session with no further wiring.
func buildConstraintPlant(cfg *Config) constraint.Plant {
	p := constraint.Plant{
		Inverters: map[string]orchestrator.InverterPlant{},
		Batteries: map[string]orchestrator.BatteryPlant{},
		EVSEs:     map[string]orchestrator.EVSEPlant{},
	}
	// Apply bench defaults at consume time (TASK-060): a partial/absent plant
	// block fills with the calibration that reproduces optimizer.go's constants,
	// so the ExportConstraint's adaptive detection window (control latency + meter
	// lag) lands on today's exportBreachTicks=3 instead of collapsing to the floor
	// of 2 on a zero-value plant.
	p.Meter = orchestrator.MeterPlant{}.WithDefaults()
	for _, d := range cfg.Devices {
		switch d.Role {
		case "inverter":
			p.Inverters[d.Name] = d.InverterPlant.WithDefaults()
		case "battery":
			p.Batteries[d.Name] = d.BatteryPlant.WithDefaults()
		case "meter":
			p.Meter = d.MeterPlant.WithDefaults()
		}
	}
	for _, s := range cfg.Stations {
		p.EVSEs[s.ID] = s.EVSEPlant.WithDefaults()
	}
	return p
}

// derConstraintsFromSchedule converts a DERScheduleMsg into per-step
// StepConstraints for the daily planner.  Missing steps are left unconstrained.
//
// ausLimits (WP-11, hub.json `enforce_aus_limits`) gates whether the slots'
// CSIP-AUS gen_lim_w/load_lim_w fields (carried since WP-8) are mapped into
// the planner envelope: with the flag off, daily plans stay byte-identical to
// pre-WP-11 even against a schedule that carries AUS limits, matching the
// optimizer cascade's own flag-off contract.
func derConstraintsFromSchedule(sched bus.DERScheduleMsg, ausLimits bool) []orchestrator.StepConstraint {
	const planStepSec = 5 * 60
	const planSteps = 288

	if len(sched.Slots) == 0 {
		return nil
	}
	ws := sched.WindowStart - (sched.WindowStart % planStepSec)
	out := make([]orchestrator.StepConstraint, planSteps)
	for i := range out {
		out[i] = orchestrator.StepConstraint{
			ExpLimW:  math.NaN(),
			ImpLimW:  math.NaN(),
			MaxLimW:  math.NaN(),
			FixedW:   math.NaN(),
			GenLimW:  math.NaN(),
			LoadLimW: math.NaN(),
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
		genLimW := math.NaN()
		loadLimW := math.NaN()
		if ausLimits {
			if slot.GenLimW != nil {
				genLimW = *slot.GenLimW
			}
			if slot.LoadLimW != nil {
				loadLimW = *slot.LoadLimW
			}
		}

		for i := startIdx; i < endIdx; i++ {
			out[i] = orchestrator.StepConstraint{
				Disconnect: disconnect,
				ExpLimW:    expLimW,
				ImpLimW:    impLimW,
				MaxLimW:    maxLimW,
				FixedW:     fixedW,
				GenLimW:    genLimW,
				LoadLimW:   loadLimW,
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
