package main

// scan.go implements the lexa-modbus commissioning-scan controller
// (docs/DEVICE_ROADMAP.md §5.2, TASK-082 unit 5.2): the device-side half of
// the bus.ScanRequest/ScanStatus/ScanResult contract (internal/bus/scan.go).
// lexa-api's POST /scan (cmd/api/scan.go) publishes the request this
// subscribes; lexactl's `scan run`/`scan show` (cmd/lexactl/cmd_scan.go)
// drive it from the operator/wizard side.
//
// ARMING RULE (v1, deliberately blunt — matches the roadmap's own doc-comment
// sketch verbatim): a scan runs ONLY when cfg.Devices is empty (uncommissioned)
// OR every reconciler class actually present among cfg.Devices is "off"
// (scanArmed, below). Any other state is refused with a ScanStatus{Phase:
// "refused"} plus a scan_run journal/log line — never a partial or silent
// scan. This blunt config-state check is the WHOLE safety guarantee: a scan
// constructs its OWN transports via sunspec.SweepTCP/SweepRTU (never through
// the southbound registry — see runScan below), so it is capable of sharing
// a live TCP session or RS-485 line with normal polling/control if it were
// ever allowed to run alongside one. The arming rule exists precisely to make
// sure that never happens, by refusing to run at all unless the segment is
// known to be idle rather than trying to interleave safely with live traffic.
//
// In practice, because loadConfig (config.go, TASK-032) already REQUIRES
// reconciler["battery"/"solar"] = "active" whenever a battery/inverter
// device is configured, the "every present class is off" branch can only be
// reached today by a fleet containing ONLY meter-role devices (meters have
// no reconciler concept at all — see reconcilerClassByRole's doc in
// config.go). Meters are still live-polled by the registry even in that
// case, so this v1 rule does not fully close a meter-segment overlap; the
// roadmap flags this itself ("v2 may add an armed-pause handshake with the
// hub; not v1") and post-commissioning re-scans are meant to be an operator
// action (stop lexa-modbus, scan, restart) rather than a live re-scan.
//
// Post-sweep classification precedence (classifyHit, below) mirrors how
// internal/southbound's battery/inverter packages are actually distinguished
// in this codebase: model 801/802 (battery-only) wins first, then the meter
// models 201-203, then any inverter/DER AC model (legacy 101-103 or any of
// the IEEE 1547-2018 704-series 701-714). Legacy companion models 120-124
// (Nameplate/Basic Settings/Extended Status/Immediate Control) are NOT part
// of this precedence — see classifyHit's doc for why.
import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/journal"
	"lexa-hub/internal/metrics"
	"lexa-hub/internal/mqttutil"
	"lexa-proto/sunspec"
)

// Injectable seams (test-only overrides; production wires the vendored
// lexa-proto primitives / stdlib directly). Package-level vars, mirroring
// lexa-proto/sunspec/bussweep.go's own newTransport/newSerialTransport seam
// one layer up the stack, and this package's existing retryDevice.open
// per-instance-seam convention one layer down.
var (
	sweepTCP = sunspec.SweepTCP
	sweepRTU = sunspec.SweepRTU
	// interfaceAddrs backs deriveLocalCIDR; overridden in tests to simulate
	// a loopback-only host without depending on the test machine's real
	// network configuration.
	interfaceAddrs = net.InterfaceAddrs
)

const (
	scanDefaultTCPPort    = 502
	scanDefaultTCPTimeout = 2 * time.Second
	scanDefaultRTUTimeout = 500 * time.Millisecond
	// scanTotalBudget bounds ONE scan's TCP+RTU sweep time combined (spec
	// §5.2 item 4: "context bounded at 10min total") — a single context
	// shared across both sweep calls in runScan, not 10 minutes each.
	scanTotalBudget = 10 * time.Minute
	// scanStatusMinInterval rate-limits ScanStatus progress publishes to at
	// most once per this interval (spec item 4: "rate-limit ≥1s between
	// publishes") so a large/fast sweep doesn't flood the bus with one
	// publish per hit. The first observation in a phase always publishes
	// immediately (so a watcher sees the phase start without a 1s lag), and
	// a phase's true final count is always flushed regardless of the rate
	// limit — see scanProgress.
	scanStatusMinInterval = 1 * time.Second
)

// scanDefaultBauds is the RTU sweep's default baud list when
// ScanRequest.Bauds is empty (bus.ScanRequest's doc comment).
var scanDefaultBauds = []int{9600, 19200}

// scanDefaultTCPUnitIDs is the bus CONTRACT default for a TCP sweep with no
// explicit ScanRequest.UnitIDs (bus.ScanRequest's doc comment: "{1,2,3,126}
// (TCP)"). This is deliberately wider than lexa-proto/sunspec's OWN internal
// empty-unitIDs default for SweepTCP, which is just {1} (5.1 review ruling,
// docs/extension/00_PROGRESS.md: "unit 5.2 must pass the bus-contract default
// {1,2,3,126} explicitly — defaults belong at the caller"). RTU gets NO such
// override here: an empty ScanRequest.UnitIDs is passed straight through to
// sweepRTU, whose own default (the full 1..247 range) is what the bus
// contract wants for RTU.
var scanDefaultTCPUnitIDs = []uint8{1, 2, 3, 126}

// scanController owns the single in-flight commissioning scan for this
// lexa-modbus process (spec item 2: "One scan at a time"). Constructed once
// in main() and Subscribe'd to bus.TopicScanRequest.
type scanController struct {
	mc  mqtt.Client
	cfg *Config
	jw  *journal.Writer // nil ⇒ journaling disabled (no "journal" config block)

	// running gates concurrent scans: CompareAndSwap(false, true) claims the
	// single slot, restored to false when runScan returns (including via a
	// refusal that never actually started sweeping — see handleRequest). An
	// atomic bool rather than a mutex because the "reject if busy" check must
	// never block: a concurrent request should get an immediate refusal, not
	// wait behind whatever is already running.
	running atomic.Bool

	runsCtr    *metrics.Counter // lexa_mb_scan_runs_total
	refusedCtr *metrics.Counter // lexa_mb_scan_refused_total
	foundGauge *metrics.Gauge   // lexa_mb_scan_devices_found (last completed run)

	// now is a seam over time.Now so tests can drive the ≥1s progress
	// rate-limit deterministically with a fake clock instead of real sleeps.
	now func() time.Time
}

// newScanController builds a scanController with production defaults wired.
func newScanController(mc mqtt.Client, cfg *Config, jw *journal.Writer, mreg *metrics.Registry) *scanController {
	return &scanController{
		mc:         mc,
		cfg:        cfg,
		jw:         jw,
		runsCtr:    mreg.Counter("lexa_mb_scan_runs_total"),
		refusedCtr: mreg.Counter("lexa_mb_scan_refused_total"),
		foundGauge: mreg.Gauge("lexa_mb_scan_devices_found"),
		now:        time.Now,
	}
}

// subscribe wires sc to bus.TopicScanRequest. Each request is handled on its
// OWN goroutine (not the paho callback goroutine) for two reasons: (1) a scan
// can run up to scanTotalBudget (10 min) and must not stall processing of any
// other message on this same MQTT client for that long; (2) a legitimately
// concurrent second ScanRequest needs to be dispatched and refused promptly
// (spec's "concurrent request → refused" behavior) rather than queued behind
// the first request's handler for up to 10 minutes. The single-scan
// invariant itself is enforced by sc.running, not by serializing on the
// callback goroutine.
func (sc *scanController) subscribe() error {
	return mqttutil.Subscribe(sc.mc, bus.TopicScanRequest, func(_ string, req bus.ScanRequest) {
		go sc.handleRequest(req)
	})
}

// handleRequest claims the single scan slot (or refuses if one is already
// running), checks the arming rule, and either refuses or runs the sweep.
func (sc *scanController) handleRequest(req bus.ScanRequest) {
	if !sc.running.CompareAndSwap(false, true) {
		sc.refuse(req.ID, "scan already running")
		return
	}
	defer sc.running.Store(false)

	if armed, reason := scanArmed(sc.cfg); !armed {
		sc.refuse(req.ID, reason)
		return
	}

	sc.runScan(req)
}

// scanArmed implements the v1 arming rule (see this file's package doc): a
// scan is permitted when either the service is uncommissioned (no devices
// configured at all) or every reconciler class actually present among
// cfg.Devices is "off". It returns false plus a human-readable refusal
// reason otherwise.
func scanArmed(cfg *Config) (bool, string) {
	if len(cfg.Devices) == 0 {
		return true, ""
	}
	present := map[string]bool{}
	for _, dc := range cfg.Devices {
		if cls, ok := reconcilerClassByRole[dc.Role]; ok {
			present[cls] = true
		}
	}
	for cls := range present {
		if mode := cfg.ReconcilerMode(cls); mode != ReconcilerOff {
			return false, fmt.Sprintf("device(s) configured and reconciler[%s]=%q (not off)", cls, mode)
		}
	}
	return true, ""
}

// refuse publishes a refused ScanStatus, journals the refusal, and counts it.
func (sc *scanController) refuse(id, reason string) {
	sc.refusedCtr.Inc()
	slog.Warn("lexa-modbus: scan refused", "id", id, "reason", reason)
	sc.publishStatus(id, "refused", 0, 0, reason)
	sc.journalRun(id, "refused", 0, reason)
}

// runScan performs one armed, claimed scan end to end: resolve defaults,
// SweepTCP, SweepRTU (if configured), publish the identify status line,
// then the retained ScanResult + a final "done" status, journal, and record
// metrics. Every step is best-effort past this point — a sweep error only
// logs; the scan still reports whatever it found rather than aborting
// silently (mirrors SweepTCP/SweepRTU's own "never drop a device found so
// far because a LATER host/probe failed" contract).
func (sc *scanController) runScan(req bus.ScanRequest) {
	port := req.TCPPort
	if port == 0 {
		port = scanDefaultTCPPort
	}
	cidr := req.TCPCidr
	if cidr == "" {
		var err error
		cidr, err = deriveLocalCIDR()
		if err != nil {
			// Never reached sweepTCP at all — counts as a refusal, not a
			// started-then-failed run, so runsCtr/refusedCtr stay mutually
			// exclusive per scan attempt (see below: runsCtr only increments
			// once a sweep is actually about to happen).
			sc.refuse(req.ID, fmt.Sprintf("derive local /24: %v", err))
			return
		}
	}
	tcpUnitIDs := req.UnitIDs
	if len(tcpUnitIDs) == 0 {
		tcpUnitIDs = scanDefaultTCPUnitIDs
	}
	bauds := req.Bauds
	if len(bauds) == 0 {
		bauds = scanDefaultBauds
	}

	sc.runsCtr.Inc()
	slog.Info("lexa-modbus: scan starting", "id", req.ID, "cidr", cidr, "port", port)

	ctx, cancel := context.WithTimeout(context.Background(), scanTotalBudget)
	defer cancel()

	var hits []bus.ScanHit

	tcpProgress := newScanProgress(sc, req.ID, "tcp")
	if err := sweepTCP(ctx, cidr, port, tcpUnitIDs, scanDefaultTCPTimeout, func(h sunspec.SweepHit) {
		hits = append(hits, scanHitFromSweep(h))
		tcpProgress.observe(len(hits))
	}); err != nil {
		slog.Warn("lexa-modbus: scan tcp sweep ended early", "id", req.ID, "cidr", cidr, "err", err)
	}
	tcpProgress.flushFinal()

	// RTU UnitIDs are passed through UNMODIFIED (req.UnitIDs, not
	// tcpUnitIDs): an empty list must reach sweepRTU as empty so ITS OWN
	// default (the full 1..247 range) applies — only the TCP path gets the
	// bus-contract override above (see scanDefaultTCPUnitIDs's doc).
	if req.RTUDev != "" && ctx.Err() == nil {
		rtuProgress := newScanProgress(sc, req.ID, "rtu")
		before := len(hits)
		if err := sweepRTU(ctx, req.RTUDev, bauds, req.UnitIDs, scanDefaultRTUTimeout, func(h sunspec.SweepHit) {
			hits = append(hits, scanHitFromSweep(h))
			rtuProgress.observe(len(hits) - before)
		}); err != nil {
			slog.Warn("lexa-modbus: scan rtu sweep ended early", "id", req.ID, "dev", req.RTUDev, "err", err)
		}
		rtuProgress.flushFinal()
	}

	// Identify phase: every hit was already classified in scanHitFromSweep as
	// it arrived (there is no separate identify pass to run — classification
	// is cheap, pure, per-hit work), so this is purely the progress line the
	// spec asks for between the sweep phases and the final result.
	sc.publishStatus(req.ID, "identify", len(hits), len(hits), "")

	result := bus.ScanResult{Envelope: bus.Envelope{V: bus.ScanResultV}, ID: req.ID, Devices: hits, Ts: sc.now().Unix()}
	if err := mqttutil.PublishJSONRetained(sc.mc, bus.TopicScanResult, result); err != nil {
		slog.Error("lexa-modbus: publish scan result", "id", req.ID, "err", err)
	}
	sc.publishStatus(req.ID, "done", len(hits), len(hits), "")
	sc.foundGauge.Set(float64(len(hits)))
	sc.journalRun(req.ID, "done", len(hits), "")
	slog.Info("lexa-modbus: scan done", "id", req.ID, "devices_found", len(hits))
}

// publishStatus publishes one (non-retained, QoS per bus.PubQoS — currently
// QoS 1) bus.ScanStatus progress/terminal line.
func (sc *scanController) publishStatus(id, phase string, probed, found int, detail string) {
	st := bus.ScanStatus{
		Envelope: bus.Envelope{V: bus.ScanStatusV},
		ID:       id, Phase: phase, Probed: probed, Found: found, Detail: detail,
		Ts: sc.now().Unix(),
	}
	if err := mqttutil.PublishJSONQoS(sc.mc, bus.TopicScanStatus, bus.PubQoS(bus.TopicScanStatus), st); err != nil {
		slog.Error("lexa-modbus: publish scan status", "id", id, "phase", phase, "err", err)
	}
}

// journalRun appends a scan_run event (nil jw ⇒ no-op, matching cmd/hub's
// journal call-site convention).
func (sc *scanController) journalRun(id, phase string, devicesFound int, detail string) {
	if sc.jw == nil {
		return
	}
	ev, err := journal.NewScanRunEvent("modbus", journal.NewScanRun(id, phase, devicesFound, detail))
	if err != nil {
		slog.Error("lexa-modbus: build scan_run event", "id", id, "err", err)
		return
	}
	if err := sc.jw.Append(ev); err != nil {
		slog.Error("lexa-modbus: journal scan_run", "id", id, "err", err)
	}
}

// scanProgress rate-limits a phase's ScanStatus progress publishes to at
// most one per scanStatusMinInterval, per this file's package doc. Probed and
// Found are reported as the SAME cumulative hit count: sunspec.SweepTCP/
// SweepRTU only invoke their hit callback on a confirmed SunSpec marker
// match, never on a probed-but-silent host/unit id, so there is no
// finer-grained "attempted" count available from these primitives to report
// as Probed distinct from Found. A future proto version exposing a
// probe-attempt callback could refine this; documented here rather than
// left unexplained.
type scanProgress struct {
	sc    *scanController
	id    string
	phase string
	last  time.Time
	n     int
}

func newScanProgress(sc *scanController, id, phase string) *scanProgress {
	return &scanProgress{sc: sc, id: id, phase: phase}
}

// observe records the phase's new cumulative count and publishes immediately
// if this is the first observation or scanStatusMinInterval has elapsed
// since the last publish.
func (p *scanProgress) observe(n int) {
	p.n = n
	now := p.sc.now()
	if p.last.IsZero() || now.Sub(p.last) >= scanStatusMinInterval {
		p.publish()
		p.last = now
	}
}

func (p *scanProgress) publish() {
	p.sc.publishStatus(p.id, p.phase, p.n, p.n, "")
}

// flushFinal publishes the phase's final count unconditionally, so the rate
// limit never swallows the true end-of-phase state even if the last hit
// arrived less than scanStatusMinInterval after the previous publish.
func (p *scanProgress) flushFinal() {
	p.publish()
}

// scanHitFromSweep converts one sunspec.SweepHit into a bus.ScanHit,
// classifying its model list and copying identity fields when Common
// decoded.
//
// NameplateW is deliberately left nil in this v1: sunspec.SweepHit does not
// expose the transport it probed on (bussweep.go's probeHostTCP closes the
// per-host transport once that host is exhausted, and — more fundamentally —
// a TCP sweep's worker goroutine is already probing the NEXT unit id on that
// SAME transport concurrently with the hit callback's execution, per
// bussweep.go's "single-goroutine callback pump" design; there is no safe
// window to issue an extra WMax read through it). Reading a nameplate would
// require either changing the vendored lexa-proto module (out of scope for
// this unit — cmd/modbus/scan.go only consumes it) or opening a SECOND,
// redundant connection per hit purely to read one register block, which
// fights the "a scan never shares a session with anything else" posture this
// controller otherwise holds to. Left as an explicit, documented gap for a
// future unit rather than a silent omission or a guessed value.
func scanHitFromSweep(h sunspec.SweepHit) bus.ScanHit {
	models := make([]uint16, 0, len(h.Blocks))
	for _, b := range h.Blocks {
		models = append(models, b.ModelID)
	}
	hit := bus.ScanHit{
		URL:    h.URL,
		UnitID: h.UnitID,
		Class:  classifyHit(models),
		Models: models,
	}
	if h.Common != nil {
		hit.Manufacturer = h.Common.Manufacturer
		hit.Model = h.Common.Model
		hit.Serial = h.Common.Serial
		hit.FwVersion = h.Common.Version
	}
	return hit
}

// classifyHit maps a SunSpec-matched hit's model-ID list to a device class,
// following the SAME precedence this codebase's internal/southbound packages
// effectively encode by which models each device type actually carries (see
// internal/southbound/{battery,inverter,meter}.go and derbase.Init):
//
//  1. Model 801 (Battery Base) or 802 (Li-Ion Battery Detail) ⇒ "battery".
//     These are battery-only models in this codebase; nothing else declares
//     them. Checked FIRST because a battery may also carry legacy
//     inverter-shaped AC measurement models (see below), so battery must win
//     the tie.
//  2. Else any of models 201/202/203 (single/split/three-phase AC meter)
//     ⇒ "meter".
//  3. Else any of the legacy AC measurement models 101/102/103, OR any of
//     the IEEE 1547-2018 DER models 701-714 ⇒ "inverter". This is deliberately
//     the LAST AC-model check: derbase.Init treats 701 (DERMeasureAC) as the
//     preferred measurement model with 101/102/103 as its fallback (the same
//     precedence inverter.New/battery.New both inherit via derbase.Init), and
//     a battery ALSO carries one of these AC measurement models — that is
//     exactly why the battery check above must run first, not this one.
//  4. Otherwise (SunSpec marker matched — Blocks/Common decoded — but none of
//     the above model families present) ⇒ "unknown-sunspec".
//
// Legacy models 120-124 (Nameplate/Basic Settings/Extended Status/Immediate
// Control) are DELIBERATELY not part of this precedence at all: they are
// companion models common to BOTH inverters and batteries in the SunSpec
// model set (neither internal/southbound/battery.go nor inverter.go treats
// them as a class signal — only as ancillary WMax-source/control blocks), so
// on their own they identify nothing (5.1/5.2 review ruling).
//
// "unknown-modbus" is part of bus.ScanHit's documented Class vocabulary but
// is UNREACHABLE from sunspec.SweepTCP/SweepRTU as vendored: probeOne only
// ever invokes the hit callback once the SunSpec "SunS" marker itself has
// matched (lexa-proto/sunspec/bussweep.go) — a device that answers Modbus
// but never that marker is skipped silently before a SweepHit is ever
// constructed, so this controller can never actually classify anything as
// "unknown-modbus" today. Not handled specially below (there is nothing to
// route to it); flagged here and in this task's report as an open question
// for whoever owns non-SunSpec device discovery next.
func classifyHit(models []uint16) string {
	has := func(want ...uint16) bool {
		for _, m := range models {
			for _, w := range want {
				if m == w {
					return true
				}
			}
		}
		return false
	}
	switch {
	case has(sunspec.ModelBatteryBase, sunspec.ModelLithiumBattery): // 801, 802
		return "battery"
	case has(sunspec.ModelMeterSinglePh, sunspec.ModelMeterSplitPh, sunspec.ModelMeterThreePh): // 201-203
		return "meter"
	case has(
		sunspec.ModelInverterSinglePh, sunspec.ModelInverterSplitPh, sunspec.ModelInverterThreePh, // 101-103
		sunspec.ModelDERMeasureAC, sunspec.ModelDERCapacity, sunspec.ModelDEREnterService, sunspec.ModelDERCtlAC,
		sunspec.ModelDERVoltVar, sunspec.ModelDERVoltWatt, sunspec.ModelDERTripLV, sunspec.ModelDERTripHV,
		sunspec.ModelDERTripLF, sunspec.ModelDERTripHF, sunspec.ModelDERFreqDroop, sunspec.ModelDERWattVar,
		sunspec.ModelDERStorageCap, sunspec.ModelDERMeasureDC, // 701-714
	):
		return "inverter"
	default:
		return "unknown-sunspec"
	}
}

// deriveLocalCIDR returns the /24 CIDR containing the first non-loopback
// IPv4 address found via interfaceAddrs (net.InterfaceAddrs's own
// enumeration order — a heuristic default, not a guaranteed "the LAN
// segment," but a reasonable one for a single-NIC commissioning device).
// Returns an error when no such address exists (e.g. a loopback-only host),
// which the caller (runScan) turns into a refused ScanStatus rather than
// silently sweeping 127.0.0.0/24 or letting sweepTCP fail later with a less
// legible error.
func deriveLocalCIDR() (string, error) {
	addrs, err := interfaceAddrs()
	if err != nil {
		return "", fmt.Errorf("enumerate interfaces: %w", err)
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip4 := ipnet.IP.To4()
		if ip4 == nil || ip4.IsLoopback() {
			continue
		}
		return fmt.Sprintf("%d.%d.%d.0/24", ip4[0], ip4[1], ip4[2]), nil
	}
	return "", fmt.Errorf("no non-loopback IPv4 interface found")
}
