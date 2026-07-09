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

	// TASK-040: the durable event journal. A nil cfg.Journal (no "journal"
	// block in hub.json) leaves jw nil — every emit call site below is
	// `if jw != nil`-guarded, so this is a true no-op rollout default, not a
	// degraded one. Opened before anything that might emit (MQTT connect,
	// the reader, actuators, breach episodes) so no early transition is lost.
	var jw *journal.Writer
	if cfg.Journal != nil {
		jw, err = journal.Open(cfg.Journal.ToLibrary())
		if err != nil {
			log.Fatalf("lexa-hub: open journal: %v", err)
		}
		defer jw.Close()
		if ev, everr := journal.NewServiceStartEvent("hub", journal.NewServiceStart("", configFingerprint(cfg))); everr == nil {
			_ = jw.Append(ev)
		}
	}

	// TASK-044: metrics registry + standard process gauges, wired before the
	// MQTT connect below so its instrumentation hooks have counters ready.
	reg := metrics.New()
	metrics.StandardGauges(reg)

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
		log.Printf("lexa-hub: constraint shadow ENABLED — modes: export=%s gen=%s import=%s economics=%s battery_safety=%s (panic-latch + per-axis + Tier-1 safety diff armed)",
			modes["export"], modes["gen"], modes["import"], modes["economics"], modes["battery_safety"])
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

	// TASK-041 restore-on-start: gated behind cfg.Snapshot.Enabled (default
	// false — see SnapshotConfig's doc). This MUST run before the reconciler
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
		}
		// Reflect the current episode state on EVERY call so the gauge tracks the
		// merged evidence, including ticks/reports that produced no edge.
		if episodes.Active() {
			breachActiveGauge.Set(1)
		} else {
			breachActiveGauge.Set(0)
		}
	}

	// planLogPending is the previous pass's plan-log publish, harvested at
	// the top of the NEXT pass (TASK-046) — see the harvest call below.
	// Owned entirely by planObserver, which the engine only ever calls from
	// its single control goroutine (tick()/safetyTick()), so no lock is
	// needed even though it is closed over here.
	var planLogPending *mqttutil.PendingPub

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

		// Feed this plan's meter-level breach evidence to the episode component
		// and publish whatever edge it produces. The component owns the edge
		// semantics (onset / new-mRID re-alert / clear) and the Safety-plan guard
		// (a safety plan's nil Breach is "not assessed", never a clear edge).
		// lexa_hub_breach_active is updated inside emitAlerts on every pass.
		emitAlerts(episodes.OnPlan(plan, time.Now()))
	}

	eng := orchestrator.New(reader, optimizer, orchestrator.Config{
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
			eng.RegisterBatteryActuator(dc.Name, newDesiredPublishingBatteryActuator(mc, dc.Name, desiredPublishesTotalCtr, desiredPublishFailuresCtr, tickTime, jw))
		case "inverter":
			eng.RegisterSolarActuator(dc.Name, newDesiredPublishingSolarActuator(mc, dc.Name, desiredPublishesTotalCtr, desiredPublishFailuresCtr, tickTime, jw))
		}
	}

	// Wire actuators for known EVSE stations (desired-doc publisher only).
	for _, sc := range cfg.Stations {
		eng.RegisterEVSEActuator(sc.ID, newDesiredPublishingEVSEActuator(mc, sc.ID, desiredPublishesTotalCtr, desiredPublishFailuresCtr, tickTime, jw))
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
