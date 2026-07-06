// lexa-northbound is the IEEE 2030.5 northbound client service.
//
// It maintains a wolfSSL mTLS connection to the utility server, walks the
// resource tree on a configurable interval, resolves the active DER control via
// the scheduler, builds a 24-hour DER control schedule, and publishes all
// results to MQTT (retained).
//
// Published topics:
//
//	lexa/csip/control           — current active DER control (scalar modes)
//	lexa/northbound/schedule    — resolved 24-hour schedule with curves
//	lexa/csip/pricing           — tariff profile intervals
//	lexa/csip/billing           — billing period summaries
//	lexa/csip/flowreservation/status — granted flow reservation responses
//
// Usage:
//
//	lexa-northbound [-config /etc/lexa/northbound.json]
package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"encoding/xml"
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
	"lexa-hub/internal/northbound/discovery"
	"lexa-hub/internal/northbound/identity"
	"lexa-hub/internal/northbound/schedule"
	"lexa-hub/internal/northbound/scheduler"
	"lexa-hub/internal/tlsclient"
	"lexa-hub/internal/utilitytime"
	"lexa-hub/internal/watchdog"
	"lexa-hub/internal/wolfssl"
	model "lexa-proto/csipmodel"
)

// ── Flow reservation manager ──────────────────────────────────────────────────

// flowReservationManager handles the client side of the Flow Reservation
// function set (IEEE 2030.5 §10.9). It receives FlowReservationRequest
// messages from the hub via MQTT and POSTs them to the utility server, tracking
// the path to use for each end device.
type flowReservationManager struct {
	mu          sync.RWMutex
	fetcher     *tlsclient.WolfSSLFetcher
	lfdi        string
	requestPath string // EndDevice FlowReservationRequestListLink.Href; guarded by mu
}

func newFlowReservationManager(f *tlsclient.WolfSSLFetcher, lfdi string) *flowReservationManager {
	return &flowReservationManager{fetcher: f, lfdi: lfdi}
}

// setRequestPath updates the server path to POST new requests to. Called after
// each discovery walk with the path from the EndDevice resource.
func (m *flowReservationManager) setRequestPath(path string) {
	m.mu.Lock()
	m.requestPath = path
	m.mu.Unlock()
}

// handleRequest is the MQTT message handler for TopicCSIPFRRequest. It
// decodes the hub's FlowReservationRequestMsg and POSTs a corresponding
// FlowReservationRequest to the utility server.
func (m *flowReservationManager) handleRequest(payload []byte) {
	var msg bus.FlowReservationRequestMsg
	if err := json.Unmarshal(payload, &msg); err != nil {
		log.Printf("lexa-northbound: flowreservation decode: %v", err)
		return
	}
	m.mu.RLock()
	requestPath := m.requestPath
	m.mu.RUnlock()
	if requestPath == "" {
		log.Printf("lexa-northbound: flowreservation: no request path yet — server may not support FR")
		return
	}

	req := model.FlowReservationRequest{
		MRID:              msg.MRID,
		Description:       msg.Description,
		CreationTime:      msg.Ts,
		DurationRequested: msg.DurationRequested,
		EnergyRequested: &model.UnitValue{
			Multiplier: 0,
			Value:      int64(derefF64(msg.EnergyRequestedWh)),
		},
		IntervalRequested: model.DateTimeInterval{
			Start:    msg.IntervalStart,
			Duration: msg.IntervalDuration,
		},
		PowerRequested: &model.UnitValue{
			Multiplier: 0,
			Value:      int64(derefF64(msg.PowerRequestedW)),
		},
		RequestStatus: model.RequestStatus{
			DateTime:      msg.Ts,
			RequestStatus: model.FlowReqStatusRequested,
		},
	}

	body, err := xml.Marshal(&req)
	if err != nil {
		log.Printf("lexa-northbound: flowreservation marshal: %v", err)
		return
	}
	_, location, err := m.fetcher.Post(requestPath, body, "application/sep+xml")
	if err != nil {
		log.Printf("lexa-northbound: flowreservation POST %s: %v", requestPath, err)
		return
	}
	log.Printf("lexa-northbound: flowreservation POSTed mrid=%s location=%s", msg.MRID, location)
}

func main() {
	cfgPath := flag.String("config", "/etc/lexa/northbound.json", "path to JSON config")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("lexa-northbound: load config: %v", err)
	}
	logutil.Setup("lexa-northbound", logutil.ParseLevel(cfg.LogLevel)) // TASK-045

	wolfssl.Init()
	defer wolfssl.Cleanup()

	tlsCfg := tlsclient.Config{
		ServerAddr:     cfg.Server,
		CACertPath:     cfg.CACert,
		ClientCertPath: cfg.ClientCert,
		ClientKeyPath:  cfg.ClientKey,
	}
	fetcherDisc, err := tlsclient.NewWolfSSLFetcher(tlsCfg)
	if err != nil {
		log.Fatalf("lexa-northbound: init TLS fetcher (discovery): %v", err)
	}
	defer fetcherDisc.Free()

	fetcherResp, err := tlsclient.NewWolfSSLFetcher(tlsCfg)
	if err != nil {
		log.Fatalf("lexa-northbound: init TLS fetcher (response): %v", err)
	}
	defer fetcherResp.Free()

	lfdi := cfg.LFDI
	if lfdi == "" {
		lfdi, err = lfdiFromCert(cfg.ClientCert)
		if err != nil {
			log.Fatalf("lexa-northbound: derive LFDI: %v", err)
		}
	}
	log.Printf("lexa-northbound: LFDI=%s server=%s", lfdi, cfg.Server)

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
		r.Counter("lexa_bus_decode_failures_total").Set(total)
	})
	walkDurationGauge := reg.Gauge("lexa_nb_walk_duration_seconds")
	walkFailuresCtr := reg.Counter("lexa_nb_walk_failures_total")
	responsesPostedCtr := reg.Counter("lexa_nb_responses_posted_total")
	clockOffsetGauge := reg.Gauge("lexa_nb_clock_offset_seconds")

	mqttPass, err := mqttutil.LoadPassword(cfg.MQTTPassFile)
	if err != nil {
		log.Fatalf("lexa-northbound: %v", err)
	}
	mc, err := mqttutil.ConnectAuthInstrumented(cfg.MQTTBroker, cfg.MQTTClientID, cfg.MQTTUser, mqttPass, mqttutil.Instrumentation{
		OnPublishFail: mqttFailCtr.Inc,
		OnReconnect:   mqttReconnCtr.Inc,
	})
	if err != nil {
		log.Fatalf("lexa-northbound: %v", err)
	}
	defer mc.Disconnect(500)

	metrics.Serve(cfg.MetricsAddr, reg)

	ctx, cancel := context.WithCancel(context.Background())

	sched := scheduler.New()
	// clk is the single owner of the accumulated utility-time offset (AD-004,
	// TASK-035). The walker still acquires the raw offset per walk
	// (tree.ClockOffset); runDiscovery feeds it here via clk.SetOffset, and every
	// serverNow computation reads it back through clk.ServerNow — the same
	// arithmetic as scheduler.ServerNow(tree.ClockOffset), now single-owned.
	clk := utilitytime.New(utilitytime.Config{})
	respTracker := newResponseTracker(fetcherResp, lfdi, cfg.ResponseSetPath, clk, responsesPostedCtr)

	// Flow reservation: a third TLS session dedicated to POSTing reservation
	// requests received from the hub via MQTT. This keeps it isolated from the
	// discovery fetcher (which holds long-lived keep-alive sessions).
	fetcherFR, err := tlsclient.NewWolfSSLFetcher(tlsCfg)
	if err != nil {
		log.Fatalf("lexa-northbound: init TLS fetcher (flow reservation): %v", err)
	}
	defer fetcherFR.Free()

	frManager := newFlowReservationManager(fetcherFR, lfdi)

	// Subscribe to FlowReservationRequest messages from the hub.
	// These arrive when the hub wants to schedule a charging/discharging window
	// on the utility server.
	//
	// This is the one subscribe in the codebase that bypasses mqttutil.Subscribe
	// (it needs the raw payload for handleRequest, not a JSON-decoded T), so it
	// carries its own bus.CheckVersion/RejectAndAlarm gate (TASK-018) instead of
	// getting it for free the way every mqttutil.Subscribe caller does. Same
	// policy as the central gate: absent-v (legacy v0) accepted while
	// bus.LegacyV0Accepted is true, unknown-major dropped and counted.
	if token := mc.Subscribe(bus.TopicCSIPFRRequest, 1, func(_ mqtt.Client, msg mqtt.Message) {
		if verr := bus.CheckVersion(msg.Topic(), msg.Payload(), bus.SupportedV(msg.Topic())); verr != nil {
			if ve, ok := verr.(*bus.VersionError); ok {
				bus.RejectAndAlarm(ve)
			}
			return
		}
		frManager.handleRequest(msg.Payload())
	}); token.Wait() && token.Error() != nil {
		log.Printf("lexa-northbound: subscribe flowreservation/request: %v", token.Error())
	}

	// Subscribe to compliance-breach alerts from the hub. On the onset of a
	// breach the hub cannot meet an active control limit (e.g. an import cap
	// with the battery drained); post a CannotComply Response so the grid
	// server knows the DER is resource-limited. On clear, reset the episode
	// guard so a future breach re-alerts.
	if err := mqttutil.Subscribe(mc, bus.TopicCSIPComplianceAlert, func(_ string, alert bus.ComplianceAlert) {
		if alert.Active {
			log.Printf("lexa-northbound: compliance breach %s limit=%.0fW measured=%.0fW (%s) → CannotComply mrid=%s",
				alert.LimitType, alert.LimitW, alert.MeasuredW, alert.Reason, alert.MRID)
			respTracker.alertCannotComply(alert.MRID)
		} else {
			respTracker.clearAlerts()
		}
	}); err != nil {
		log.Printf("lexa-northbound: subscribe compliance alert: %v", err)
	}

	// sd_notify READY (TASK-008): MQTT is connected and both subscriptions
	// (flow reservation request, compliance alert) are registered — only the
	// discovery walk goroutine remains to start. Sending Ready here, before
	// the first walk, matters: a slow or unreachable utility server must not
	// itself cause a systemd start timeout — runDiscovery's fail-closed
	// discipline (see below) already handles that case once the process is
	// up, and the walk loop's own watchdog kicks (also below) are what prove
	// liveness from here on.
	watchdog.Ready()

	nbm := nbMetrics{walkDuration: walkDurationGauge, walkFailures: walkFailuresCtr, clockOffset: clockOffsetGauge}

	// Run the first discovery immediately, then loop.
	go func() {
		runDiscovery(mc, fetcherDisc, lfdi, sched, clk, respTracker, frManager, cfg, nbm)
		// TASK-008: kick once the initial walk returns, success or fail-closed
		// — a walk that erred and held last-known-good is still a live,
		// iterating loop (QA 2026-07-02 northbound-hang/wan-outage-hold: a
		// server that stops responding must NOT starve this kick, only a
		// wedged walker/registry should).
		watchdog.Kick()
		ticker := time.NewTicker(cfg.DiscoveryInterval())
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// TASK-008: kick at the top of the loop body — every tick the
				// loop wakes up at all is itself the liveness signal;
				// runDiscovery's internal errors are handled by its own
				// fail-closed logging and never prevent reaching this line.
				watchdog.Kick()
				runDiscovery(mc, fetcherDisc, lfdi, sched, clk, respTracker, frManager, cfg, nbm)
			}
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("lexa-northbound: shutting down")
	cancel()
}

// discoveryFailures counts consecutive failed walks, for the fail-closed log
// line and operator triage. Touched only from the single discovery goroutine.
var discoveryFailures int

// nbMetrics bundles the TASK-044 Prometheus instruments runDiscovery updates
// on every walk cycle. A small struct rather than three more positional
// parameters on runDiscovery, which already takes six.
type nbMetrics struct {
	walkDuration *metrics.Gauge   // lexa_nb_walk_duration_seconds
	walkFailures *metrics.Counter // lexa_nb_walk_failures_total (monotonic total; contrast discoveryFailures, which is consecutive-and-resets)
	clockOffset  *metrics.Gauge   // lexa_nb_clock_offset_seconds
}

func runDiscovery(
	mc mqtt.Client,
	fetcher *tlsclient.WolfSSLFetcher,
	lfdi string,
	sched *scheduler.Scheduler,
	clk *utilitytime.Clock,
	rt *responseTracker,
	frm *flowReservationManager,
	cfg *Config,
	nbm nbMetrics,
) {
	walkStart := time.Now()
	defer func() { nbm.walkDuration.Set(time.Since(walkStart).Seconds()) }()

	walker := discovery.NewWalker(fetcher, lfdi)
	tree, err := walker.Discover("/dcap")
	if err != nil {
		// FAIL CLOSED on a walk error: publish NOTHING. "Server unreachable /
		// walk failed" is not "server says there are no controls" — publishing
		// a retained no-control here actively wiped the enforced cap the moment
		// the WAN dropped or the head-end wedged (QA 2026-07-02: northbound-hang
		// FAIL, wan-outage-hold DEGRADED — ~9.4 kW exported over a 0 W cap until
		// the server returned). The retained last-good control stays on the bus,
		// lexa-hub keeps enforcing it, and the hub's own local clock discipline
		// (csipExpiredTicks in cmd/hub/state.go) still releases it at ValidUntil
		// if the outage outlives the control. Only a SUCCESSFUL walk that
		// resolves no valid control may release — and that path already holds
		// last-known-good via the scheduler's fail-closed Evaluate.
		discoveryFailures++
		nbm.walkFailures.Inc()
		// TASK-045: migrated to slog. "holding last-published control (fail-closed)"
		// kept intact (WAN-outage vocabulary; grep-verified unquoted today).
		slog.Warn("lexa-northbound: discovery error — holding last-published control (fail-closed)",
			"consecutive", discoveryFailures, "err", err)
		return
	}
	discoveryFailures = 0
	nbm.clockOffset.Set(float64(tree.ClockOffset))

	// Feed the walk's raw offset to the single-owner Clock (AD-004, TASK-035).
	// The Clock accumulates ownership of the accepted offset; SetOffset never
	// alters the value ServerNow returns (still local + raw offset), so this is
	// behavior-preserving. Log only a real Step transition (05 §9: transition
	// logs, not per-tick) — a stepped clock is the class the clock-jitter saga
	// hardened against, worth an operator breadcrumb; Wobble/First are silent.
	if class := clk.SetOffset(tree.ClockOffset); class == utilitytime.Step {
		slog.Info("lexa-northbound: utility clock stepped", "offset_s", tree.ClockOffset)
	}

	// Re-anchor the Clock's monotonic reference at this successful walk
	// (TASK-037/AD-004 extension): utilitytime.ServerNowAt(time.Now(), ...)
	// is the raw formula (local + this walk's fresh offset), computed once
	// here and locked in as the new anchor. Between now and the NEXT
	// successful walk, clk.ServerNow() derives purely from monotonic elapsed
	// time since this instant, making the responseTracker's CreatedDateTime
	// arithmetic (and every other ServerNow reader sharing this Clock) immune
	// to a LOCAL wall-clock step during a WAN outage/discovery gap — the
	// exposure GAP-04 identified (today's fallback holds last-known-good
	// indefinitely on a walk failure; a local step during that holdover used
	// to shift every subsequent ServerNow read by the step size). A stable
	// local clock makes this bit-identical to the pre-TASK-037 formula: the
	// anchor is reset to the same raw value every walk, so nothing changes
	// under the common case this task's "must not change" list protects.
	clk.Anchor(utilitytime.ServerNowAt(time.Now(), tree.ClockOffset))

	// serverNow now reads from the single-owner, now-anchored Clock (AD-004,
	// TASK-035/037). Immediately after Anchor above this is arithmetically
	// identical to the former scheduler.ServerNow(tree.ClockOffset); it only
	// diverges from that raw formula between walks, and only under a local
	// wall-clock step (see the Anchor call's comment). Computed ONCE per walk
	// and shared across Evaluate/Build/SupersededMRIDs, exactly as before.
	serverNow := clk.ServerNow()
	active := sched.Evaluate(tree.Programs, serverNow)

	if active != nil && active.Held {
		// TASK-045: migrated to slog. "holding last-known-good" kept intact.
		slog.Warn("lexa-northbound: discovery resolved no valid control (empty/malformed resource); holding last-known-good (fail-closed)",
			"mrid", active.MRID, "valid_until", active.ValidUntil)
	}

	msg := toActiveControl(active, tree.ClockOffset)
	if err := mqttutil.PublishJSONRetained(mc, bus.TopicCSIPControl, msg); err != nil {
		log.Printf("lexa-northbound: publish control: %v", err)
	}
	// 24-hour DER schedule — built from all discovered programs, curves, and
	// DER resource data. Published retained so lexa-hub always has the full plan.
	der24h := schedule.Build(tree, serverNow)
	publishSchedule(mc, der24h)

	// TASK-045 per-tick demotion: a successful walk logs on EVERY discovery
	// cycle (discovery_interval_s, 60 s STOCK / faster FAST) — steady-state,
	// not a transition. walkDuration (lexa-northbound's TASK-044 gauge)
	// already covers "is discovery alive", so this drops to Debug rather than
	// Info; the fail-closed WARN/error paths above stay at Warn.
	slog.Debug("lexa-northbound: discovery OK",
		"programs", len(tree.Programs), "curves_programs", countProgramsWithCurves(tree.Programs),
		"pricing", len(tree.PricingProfiles), "billing", len(tree.BillingAccounts),
		"source", msg.Source, "mrid", msg.MRID, "clock_offset_s", tree.ClockOffset, "slots", len(der24h.Slots))

	rt.update(tree, active, sched.SupersededMRIDs(tree.Programs, serverNow))

	// Pricing (§10.5): publish if we discovered any tariff profiles.
	if len(tree.PricingProfiles) > 0 {
		publishPricing(mc, tree, serverNow)
	}

	// Billing (§10.7): publish if we discovered any customer accounts.
	if len(tree.BillingAccounts) > 0 {
		publishBilling(mc, tree)
	}

	// Flow Reservation (§10.9): update the manager's request path and publish
	// current reservation statuses.
	frm.setRequestPath(tree.FlowReservationRequestPath)
	publishFlowReservations(mc, tree)
}

func countProgramsWithCurves(programs []discovery.ProgramState) int {
	n := 0
	for _, p := range programs {
		if len(p.Curves) > 0 {
			n++
		}
	}
	return n
}

// publishSchedule converts a DER24hSchedule to a DERScheduleMsg and publishes
// it retained so lexa-hub always has the current 24-hour DER plan.
func publishSchedule(mc mqtt.Client, sched *schedule.DER24hSchedule) {
	msg := bus.DERScheduleMsg{
		Envelope:    bus.Envelope{V: bus.DERScheduleV},
		WindowStart: sched.WindowStart,
		WindowEnd:   sched.WindowEnd,
		BuildTime:   sched.BuildTime,
		ClockOffset: sched.ClockOffset,
		Ts:          time.Now().Unix(),
	}
	for _, s := range sched.Slots {
		slot := bus.DERScheduleSlot{
			Start:       s.Start,
			End:         s.End,
			Source:      s.Source,
			MRID:        s.MRID,
			Description: s.Description,
			ProgramMRID: s.ProgramMRID,
			Primacy:     s.Primacy,
			RampTms:     s.Base.RampTms,
		}
		// Scalar modes.
		if s.Base.OpModConnect != nil {
			slot.Connect = s.Base.OpModConnect
		}
		if s.Base.OpModEnergize != nil {
			slot.Energize = s.Base.OpModEnergize
		}
		if s.Base.OpModMaxLimW != nil {
			w := apW(s.Base.OpModMaxLimW)
			slot.MaxLimW = &w
		}
		if s.Base.OpModFixedW != nil {
			w := apW(s.Base.OpModFixedW)
			slot.FixedW = &w
		}
		if s.Base.OpModExpLimW != nil {
			w := apW(s.Base.OpModExpLimW)
			slot.ExpLimW = &w
		}
		if s.Base.OpModImpLimW != nil {
			w := apW(s.Base.OpModImpLimW)
			slot.ImpLimW = &w
		}
		if s.Base.OpModGenLimW != nil {
			w := apW(s.Base.OpModGenLimW)
			slot.GenLimW = &w
		}
		if s.Base.OpModLoadLimW != nil {
			w := apW(s.Base.OpModLoadLimW)
			slot.LoadLimW = &w
		}
		if s.Extended != nil {
			if s.Extended.OpModTargetW != nil {
				w := apW(s.Extended.OpModTargetW)
				slot.TargetW = &w
			}
			if s.Extended.OpModFixedVar != nil {
				v := float64(s.Extended.OpModFixedVar.Value.Value) / 100.0
				slot.FixedVarPct = &v
			}
			if s.Extended.OpModFixedPFAbsorbW != nil {
				pf := float64(s.Extended.OpModFixedPFAbsorbW.Value) / 100.0
				slot.FixedPFAbsorb = &pf
			}
			if s.Extended.OpModFixedPFInjectW != nil {
				pf := float64(s.Extended.OpModFixedPFInjectW.Value) / 100.0
				slot.FixedPFInject = &pf
			}
			if fd := s.Extended.OpModFreqDroop; fd != nil {
				slot.FreqDroop = &bus.FreqDroopMsg{
					DBuf: fd.DBuf, DF: fd.DF, DP: fd.DP,
					OpenLoopTms: fd.OpenLoopTms, TResponse: fd.TResponse,
				}
			}
			// Curve-linked modes.
			slot.VoltVar = curveSummary(s.Curves.VoltVar)
			slot.FreqWatt = curveSummary(s.Curves.FreqWatt)
			slot.WattPF = curveSummary(s.Curves.WattPF)
			slot.VoltWatt = curveSummary(s.Curves.VoltWatt)
			slot.HFRTMayTrip = curveSummary(s.Curves.HFRTMayTrip)
			slot.HFRTMustTrip = curveSummary(s.Curves.HFRTMustTrip)
			slot.HVRTMayTrip = curveSummary(s.Curves.HVRTMayTrip)
			slot.HVRTMomentaryCessation = curveSummary(s.Curves.HVRTMomentaryCessation)
			slot.HVRTMustTrip = curveSummary(s.Curves.HVRTMustTrip)
			slot.LFRTMayTrip = curveSummary(s.Curves.LFRTMayTrip)
			slot.LFRTMustTrip = curveSummary(s.Curves.LFRTMustTrip)
			slot.LVRTMayTrip = curveSummary(s.Curves.LVRTMayTrip)
			slot.LVRTMomentaryCessation = curveSummary(s.Curves.LVRTMomentaryCessation)
			slot.LVRTMustTrip = curveSummary(s.Curves.LVRTMustTrip)
		}
		msg.Slots = append(msg.Slots, slot)
	}

	// DER device status summaries.
	for _, rs := range sched.DERResources {
		sum := bus.DERStatusSummary{DERHref: rs.DER.Href}
		if rs.Capability != nil {
			sum.ModesSupported = rs.Capability.ModesSupported
		}
		if rs.Status != nil {
			if rs.Status.GenConnectStatus != nil {
				sum.GenConnectStatus = &rs.Status.GenConnectStatus.Value
			}
			if rs.Status.InverterStatus != nil {
				sum.InverterStatus = &rs.Status.InverterStatus.Value
			}
			if rs.Status.OperationalModeStatus != nil {
				sum.OperationalMode = &rs.Status.OperationalModeStatus.Value
			}
			if rs.Status.StorageModeStatus != nil {
				sum.StorageMode = &rs.Status.StorageModeStatus.Value
			}
			if rs.Status.StateOfChargeStatus != nil {
				pct := float64(rs.Status.StateOfChargeStatus.Value) / 100.0
				sum.StateOfChargePct = &pct
			}
		}
		if rs.Availability != nil && rs.Availability.EstimatedWAvail != nil {
			w := apW(rs.Availability.EstimatedWAvail)
			sum.EstimatedWAvail = &w
		}
		msg.DERStatus = append(msg.DERStatus, sum)
	}

	if err := mqttutil.PublishJSONRetained(mc, bus.TopicNorthboundSchedule, msg); err != nil {
		log.Printf("lexa-northbound: publish schedule: %v", err)
	}
}

// curveSummary converts a DERCurve pointer to a DERCurveSummary for the bus message.
func curveSummary(c *model.DERCurve) *bus.DERCurveSummary {
	if c == nil {
		return nil
	}
	s := &bus.DERCurveSummary{
		MRID:        c.MRID,
		Description: c.Description,
		CurveType:   c.CurveType,
		XMultiplier: c.XMultiplier,
		YMultiplier: c.YMultiplier,
	}
	for _, pt := range c.CurveData {
		s.Points = append(s.Points, bus.CurvePoint{X: pt.XValue, Y: pt.YValue})
	}
	return s
}

// publishPricing converts the discovered pricing state to a PricingUpdate
// message and publishes it retained so the hub always has the latest rates.
func publishPricing(mc mqtt.Client, tree *discovery.ResourceTree, serverNow int64) {
	msg := bus.PricingUpdate{Envelope: bus.Envelope{V: bus.PricingUpdateV}, Ts: time.Now().Unix()}

	for _, ts := range tree.PricingProfiles {
		pm := bus.TariffProfileMsg{
			MRID:                      ts.Profile.MRID,
			Description:               ts.Profile.Description,
			Currency:                  ts.Profile.Currency,
			PricePowerOfTenMultiplier: ts.Profile.PricePowerOfTenMultiplier,
			Primacy:                   ts.Profile.Primacy,
			RateCode:                  ts.Profile.RateCode,
		}
		for _, rcs := range ts.RateComponents {
			rcm := bus.RateComponentMsg{
				MRID:        rcs.Component.MRID,
				Description: rcs.Component.Description,
			}
			for _, tti := range rcs.ActiveTimeTariffIntervals {
				rcm.ActiveIntervals = append(rcm.ActiveIntervals, toTimeTariffMsg(tti))
			}
			// Include scheduled intervals that haven't ended yet.
			for _, tti := range rcs.TimeTariffIntervals {
				end := tti.Interval.Start + int64(tti.Interval.Duration)
				if end > serverNow {
					rcm.ScheduledIntervals = append(rcm.ScheduledIntervals, toTimeTariffMsg(tti))
				}
			}
			pm.RateComponents = append(pm.RateComponents, rcm)
		}
		msg.TariffProfiles = append(msg.TariffProfiles, pm)
	}

	if err := mqttutil.PublishJSONRetained(mc, bus.TopicCSIPPricing, msg); err != nil {
		log.Printf("lexa-northbound: publish pricing: %v", err)
	}
}

func toTimeTariffMsg(tti model.TimeTariffInterval) bus.TimeTariffMsg {
	m := bus.TimeTariffMsg{
		MRID:          tti.MRID,
		Description:   tti.Description,
		TouTier:       tti.TouTier,
		IntervalStart: tti.Interval.Start,
		Duration:      tti.Interval.Duration,
	}
	return m
}

// publishBilling converts the discovered billing state to a BillingUpdate
// message and publishes it retained.
func publishBilling(mc mqtt.Client, tree *discovery.ResourceTree) {
	msg := bus.BillingUpdate{Envelope: bus.Envelope{V: bus.BillingUpdateV}, Ts: time.Now().Unix()}

	for _, cas := range tree.BillingAccounts {
		cam := bus.CustomerAccountMsg{
			MRID:         cas.Account.MRID,
			CustomerName: cas.Account.CustomerName,
			Currency:     cas.Account.Currency,
		}
		for _, ags := range cas.Agreements {
			agm := bus.CustomerAgreementMsg{
				MRID:            ags.Agreement.MRID,
				Description:     ags.Agreement.Description,
				ServiceLocation: ags.Agreement.ServiceLocation,
			}
			for _, bp := range ags.BillingPeriods {
				agm.BillingPeriods = append(agm.BillingPeriods, bus.BillingPeriodMsg{
					IntervalStart:  bp.Interval.Start,
					Duration:       bp.Interval.Duration,
					BillLastPeriod: bp.BillLastPeriod,
					BillToDate:     bp.BillToDate,
				})
			}
			cam.Agreements = append(cam.Agreements, agm)
		}
		msg.CustomerAccounts = append(msg.CustomerAccounts, cam)
	}

	if err := mqttutil.PublishJSONRetained(mc, bus.TopicCSIPBilling, msg); err != nil {
		log.Printf("lexa-northbound: publish billing: %v", err)
	}
}

// publishFlowReservations converts discovered FlowReservationResponses to a
// FlowReservationStatusMsg and publishes it retained.
func publishFlowReservations(mc mqtt.Client, tree *discovery.ResourceTree) {
	msg := bus.FlowReservationStatusMsg{Envelope: bus.Envelope{V: bus.FlowReservationStatusV}, Ts: time.Now().Unix()}

	if tree.FlowReservations != nil {
		for _, frr := range tree.FlowReservations.FlowReservationResponse {
			status := uint8(0)
			if frr.EventStatus != nil {
				status = frr.EventStatus.CurrentStatus
			}
			rm := bus.ReservationMsg{
				MRID:          frr.MRID,
				Subject:       frr.Subject,
				CurrentStatus: status,
				IntervalStart: frr.Interval.Start,
				Duration:      frr.Interval.Duration,
			}
			if frr.EnergyAvailable != nil {
				v := unitValueToFloat(frr.EnergyAvailable)
				rm.EnergyAvailWh = &v
			}
			if frr.PowerAvailable != nil {
				v := unitValueToFloat(frr.PowerAvailable)
				rm.PowerAvailW = &v
			}
			msg.Reservations = append(msg.Reservations, rm)
		}
	}

	if err := mqttutil.PublishJSONRetained(mc, bus.TopicCSIPFRStatus, msg); err != nil {
		log.Printf("lexa-northbound: publish flow reservation status: %v", err)
	}
}

func unitValueToFloat(uv *model.UnitValue) float64 {
	if uv == nil {
		return 0
	}
	return float64(uv.Value) * math.Pow10(int(uv.Multiplier))
}

func derefF64(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// toActiveControl converts a scheduler.ActiveControl to the MQTT bus message.
func toActiveControl(ac *scheduler.ActiveControl, clockOffset int64) bus.ActiveControl {
	msg := bus.ActiveControl{
		Envelope:    bus.Envelope{V: bus.ActiveControlV},
		Source:      "none",
		ClockOffset: clockOffset,
		Ts:          time.Now().Unix(),
	}
	if ac == nil {
		return msg
	}
	msg.Source = ac.Source
	msg.MRID = ac.MRID
	msg.ValidUntil = ac.ValidUntil
	msg.Connect = ac.Base.OpModConnect
	if ac.Base.OpModExpLimW != nil {
		w := apW(ac.Base.OpModExpLimW)
		msg.ExpLimW = &w
	}
	if ac.Base.OpModImpLimW != nil {
		w := apW(ac.Base.OpModImpLimW)
		msg.ImpLimW = &w
	}
	if ac.Base.OpModMaxLimW != nil {
		w := apW(ac.Base.OpModMaxLimW)
		msg.MaxLimW = &w
	}
	if ac.Base.OpModFixedW != nil {
		w := apW(ac.Base.OpModFixedW)
		msg.FixedW = &w
	}
	return msg
}

func apW(ap *model.ActivePower) float64 {
	return float64(ap.Value) * math.Pow10(int(ap.Multiplier))
}

func lfdiFromCert(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return "", fmt.Errorf("no PEM block found in %s", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}
	l, _ := identity.FromCertificate(cert)
	return l.String(), nil
}

// ── Response tracker (GEN.044 / CORE-022) ────────────────────────────────────

// responsePoster is the subset of tlsclient.WolfSSLFetcher the response state
// machine needs. Narrowing it to an interface keeps the CORE-022 logic unit
// testable without a live TLS session.
type responsePoster interface {
	Post(path string, body []byte, contentType string) ([]byte, string, error)
}

type responseTracker struct {
	poster          responsePoster
	lfdi            string
	responseSetPath string
	// clk is the single-owner Clock (AD-004, TASK-035); Response CreatedDateTime
	// values read serverNow from it. It shares the same Clock instance the
	// discovery loop feeds via SetOffset, so postResponse sees the same
	// accumulated offset the last walk accepted — identical arithmetic to the
	// former scheduler.ServerNow(rt.clockOffset).
	clk *utilitytime.Clock
	// posted records the last Response status sent for each event mRID, so we
	// never re-post a transition and can tell whether an event has already
	// reached a terminal state (Completed/Cancelled/Superseded).
	posted     map[string]uint8
	activeMRID string
	// alerted records mRIDs for which a CannotComply Response has been posted
	// in the current breach episode, so a redelivered MQTT alert does not
	// double-post. Cleared when the hub signals the breach has cleared.
	alerted map[string]bool
	// mu guards the tracker: update() runs on the discovery goroutine while
	// alertCannotComply()/clearAlerts() run on the MQTT subscription goroutine.
	mu sync.Mutex
	// responsesPosted counts every successful POST (lexa_nb_responses_posted_total,
	// TASK-044); nil-safe (metrics.Counter's Inc is a no-op on a nil receiver),
	// so tests constructing a responseTracker without wiring metrics need no change.
	responsesPosted *metrics.Counter
}

func newResponseTracker(p responsePoster, lfdi, path string, clk *utilitytime.Clock, responsesPosted *metrics.Counter) *responseTracker {
	return &responseTracker{
		poster:          p,
		lfdi:            lfdi,
		responseSetPath: path,
		clk:             clk,
		posted:          make(map[string]uint8),
		alerted:         make(map[string]bool),
		responsesPosted: responsesPosted,
	}
}

// alertCannotComply posts a single CannotComply Response for mrid per breach
// episode. The hub already edge-triggers the alert, but the per-mRID guard
// makes a redelivered MQTT message idempotent.
func (rt *responseTracker) alertCannotComply(mrid string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.alerted[mrid] {
		return
	}
	rt.alerted[mrid] = true
	rt.postResponse(mrid, model.ResponseCannotComply)
}

// clearAlerts ends the current breach episode so a future breach re-alerts.
func (rt *responseTracker) clearAlerts() {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.alerted = make(map[string]bool)
}

// terminalResponse reports whether a response status ends an event's lifecycle:
// no further responses are sent for an mRID once it reaches one of these.
func terminalResponse(status uint8) bool {
	switch status {
	case model.ResponseEventCompleted, model.ResponseEventCancelled, model.ResponseEventSuperseded:
		return true
	default:
		return false
	}
}

// update drives the GEN.044 / CORE-022 response state machine for one poll
// cycle: Received(1) on first sighting, Started(2)/Completed(3) as the active
// event begins/ends, Cancelled(6) when the server cancels a received event
// (CORE-022 step 7), and Superseded(7) when an overlapping event wins
// (CORE-023). superseded is the set of currently-superseded event mRIDs from
// scheduler.SupersededMRIDs.
func (rt *responseTracker) update(tree *discovery.ResourceTree, active *scheduler.ActiveControl, superseded map[string]bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	serverNow := rt.clk.ServerNow()

	// Pass 1 — receipt, cancellation, and supersession for every event.
	for _, ps := range tree.Programs {
		if ps.Controls == nil {
			continue
		}
		for _, ctrl := range ps.Controls.DERControl {
			mrid := ctrl.MRID
			last, seen := rt.posted[mrid]

			if ctrl.EventStatus != nil && ctrl.EventStatus.CurrentStatus == 6 {
				// Cancelled. Acknowledge only events we previously received;
				// events that arrive already-cancelled are dropped silently.
				if seen && !terminalResponse(last) {
					rt.set(mrid, model.ResponseEventCancelled)
					if rt.activeMRID == mrid {
						rt.activeMRID = ""
					}
				}
				continue
			}

			if !seen {
				rt.set(mrid, model.ResponseEventReceived)
				last = model.ResponseEventReceived
			}

			if superseded[mrid] && !terminalResponse(last) {
				rt.set(mrid, model.ResponseEventSuperseded)
				if rt.activeMRID == mrid {
					rt.activeMRID = ""
				}
			}
		}
	}

	// Pass 2 — start/complete transitions for the active event.
	if active == nil || active.Source == "default" {
		rt.completeActive()
		return
	}

	if active.MRID != rt.activeMRID {
		rt.completeActive()
		rt.set(active.MRID, model.ResponseEventStarted)
		rt.activeMRID = active.MRID
	}

	if active.ValidUntil > 0 && serverNow >= active.ValidUntil {
		rt.completeActive()
	}
}

// completeActive posts Completed(3) for the current active event unless it has
// already reached a terminal state (e.g. it was just cancelled or superseded),
// then clears the active mRID.
func (rt *responseTracker) completeActive() {
	if rt.activeMRID == "" {
		return
	}
	if !terminalResponse(rt.posted[rt.activeMRID]) {
		rt.set(rt.activeMRID, model.ResponseEventCompleted)
	}
	rt.activeMRID = ""
}

// set posts a Response and records it as the latest status for mrid.
func (rt *responseTracker) set(mrid string, status uint8) {
	rt.postResponse(mrid, status)
	rt.posted[mrid] = status
}

func (rt *responseTracker) postResponse(mrid string, status uint8) {
	resp := model.Response{
		CreatedDateTime: rt.clk.ServerNow(),
		EndDeviceLFDI:   rt.lfdi,
		Status:          status,
		Subject:         mrid,
	}
	body, err := xml.Marshal(&resp)
	if err != nil {
		log.Printf("lexa-northbound: marshal Response: %v", err)
		return
	}
	if _, _, err = rt.poster.Post(rt.responseSetPath, body, "application/sep+xml"); err != nil {
		log.Printf("lexa-northbound: POST response (mrid=%s status=%d): %v", mrid, status, err)
		return
	}
	rt.responsesPosted.Inc()
	names := map[uint8]string{1: "Received", 2: "Started", 3: "Completed", 6: "Cancelled", 7: "Superseded", model.ResponseCannotComply: "CannotComply"}
	name := names[status]
	if name == "" {
		name = fmt.Sprintf("status=%d", status)
	}
	// TASK-045: migrated to slog. Each call is already a lifecycle edge (this
	// function is only reached from update()'s transition logic, never per-tick).
	slog.Info("lexa-northbound: response posted", "status_name", name, "status", status, "mrid", mrid)
}
