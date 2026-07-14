package main

// WP-9 (standards-buildout C1/C3/C4): the advanced desired-doc author.
// Constructed ONLY when hub.json's `advanced_der` is "on" (flag off ⇒ not
// even built, no lexa/csip/curves subscription, zero DesiredAdvanced
// publishes — the constraint_shadow precedent). It consumes the retained
// lexa/csip/control (bus.ActiveControl, WP-8 scalars) + lexa/csip/curves
// (bus.CurveSet, correlated by ActiveControl.CurveSetID == CurveSet.SetID) +
// the schedule's freq-droop parameters (see droopFromSchedule for why droop
// rides DERScheduleMsg), runs the D7 mode-priority arbitration, maps the
// resolved site axes onto each inverter/battery per its configured
// capability generation (DeviceConfig.DERGen), and publishes ONE retained
// bus.DesiredAdvanced per device on lexa/desired/adv/{device} (D6).
//
// Style mirror of cmd/hub/desired.go's actuators: content-key dedupe (the
// retained doc is standing intent, not a tick stream), the WS-2 unchanged-
// content heartbeat on the SAME desiredHeartbeatInterval (the adv reconciler
// will apply the same AD-013 staleness bound), and TASK-046 async
// fire-then-harvest publishes with rollback-on-failure — evaluation runs on
// MQTT subscription goroutines and this author's own ticker, NEVER the
// engine tick, and no path here ever blocks on a PUBACK.
//
// D7 rules implemented here:
//  1. Reactive modes are mutually exclusive: arbitrateReactive picks by
//     event-sourced > default-sourced, then voltVar > wattVar > fixedVar >
//     fixedPF; every dropped mode raises the ignored-mode alarm (edge slog
//     WARN + lexa_hub_ignored_modes_total).
//  2. volt-watt and freq-droop (and freq-watt) are concurrent overlays —
//     passed through untouched; the lesser-power rule is device physics.
//  3. Trip/ride-through sets always pass through (highest 1547 priority).
//  4. An axis the target device lacks a model generation for is left out of
//     that device's doc (explicit null) + ignored-mode alarm; WP-10's
//     reconciler reports adopt_state=unsupported for anything it still
//     cannot execute.

import (
	"encoding/json"
	"log"
	"log/slog"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/metrics"
	"lexa-hub/internal/mqttutil"
)

// advEvalInterval is the author's own ticker cadence: harvest outstanding
// publishes and re-evaluate (heartbeat republish, late-arriving curve sync).
// Input changes (control/curves/schedule arrival) evaluate immediately on
// their subscription goroutines; the ticker only bounds heartbeat latency,
// so it can be lazy relative to the engine tick.
const advEvalInterval = 15 * time.Second

// advNominalHz is the nominal grid frequency assumed by the 2030.5→SunSpec
// 711 droop-gain conversion (droopFromSchedule). CSIP is a North American
// profile; a 50 Hz deployment would make this a config key, not a constant.
const advNominalHz = 60.0

// Advanced axis vocabulary — the keys of a device's capability set and the
// site→device mapping below. advAxisRamp covers set_grad_w+set_soft_grad_w
// (one 704 write surface); advAxisRvrt is the rvrt_tms_s reversion plumb.
const (
	advAxisReactive  = "reactive_mode"
	advAxisVoltWatt  = "volt_watt"
	advAxisFreqWatt  = "freq_watt"
	advAxisFreqDroop = "freq_droop"
	advAxisTrips     = "trips"
	advAxisEnergize  = "energize"
	advAxisRamp      = "ramp"
	advAxisRvrt      = "rvrt"
)

// advCapabilityAxes is the DATA-DRIVEN device-capability → axis-set table
// (D7 rule 4; DeviceConfig.DERGen selects the row):
//
//   - "7xx": SunSpec IEEE-1547 models 701–712 — every axis (705 volt-var,
//     712 watt-var, 706 volt-watt, 711 droop, 707–710 trips, 703 enter-
//     service/energize, 704 ramp gradients + RvrtTms).
//   - "12x": legacy curve models (126 volt-var, 131 watt-PF, 132 volt-watt,
//     134 freq-watt, 129/130+135-138 ride-through, model-123 fixed PF/var) —
//     reactive/volt-watt/freq-watt/trips only. NO droop (no 711 shape), NO
//     energize (no 703 ES), NO ramp/rvrt (WP-10's write plumb is the 704
//     path; conservative omission — never command what won't execute).
//   - "" (unknown): NO axes — every commanded axis alarms as ignored,
//     the "unknown-capability device empty+alarmed" archetype.
//
// WP-10's reconciler is the execution-truth backstop: anything this table
// over-promises still comes back adopt_state=unsupported.
var advCapabilityAxes = map[string]map[string]bool{
	"7xx": {
		advAxisReactive: true, advAxisVoltWatt: true, advAxisFreqWatt: true,
		advAxisFreqDroop: true, advAxisTrips: true, advAxisEnergize: true,
		advAxisRamp: true, advAxisRvrt: true,
	},
	"12x": {
		advAxisReactive: true, advAxisVoltWatt: true, advAxisFreqWatt: true,
		advAxisTrips: true,
	},
	"": {},
}

// advTripAxes maps each D6 trip axis onto its CurveSet mode names + AdvTrip
// kinds, in the canonical doc order (must, may, momentary cessation). The
// LF/HF axes have no momentary-cessation mode in 2030.5.
var advTripAxes = []struct {
	axis  string // "lv" | "hv" | "lf" | "hf"
	kinds []struct{ mode, kind string }
}{
	{"lv", []struct{ mode, kind string }{
		{bus.CurveModeLVRTMustTrip, bus.AdvTripMustTrip},
		{bus.CurveModeLVRTMayTrip, bus.AdvTripMayTrip},
		{bus.CurveModeLVRTMomentaryCessation, bus.AdvTripMomentaryCessation},
	}},
	{"hv", []struct{ mode, kind string }{
		{bus.CurveModeHVRTMustTrip, bus.AdvTripMustTrip},
		{bus.CurveModeHVRTMayTrip, bus.AdvTripMayTrip},
		{bus.CurveModeHVRTMomentaryCessation, bus.AdvTripMomentaryCessation},
	}},
	{"lf", []struct{ mode, kind string }{
		{bus.CurveModeLFRTMustTrip, bus.AdvTripMustTrip},
		{bus.CurveModeLFRTMayTrip, bus.AdvTripMayTrip},
	}},
	{"hf", []struct{ mode, kind string }{
		{bus.CurveModeHFRTMustTrip, bus.AdvTripMustTrip},
		{bus.CurveModeHFRTMayTrip, bus.AdvTripMayTrip},
	}},
}

// ignoredMode is one dropped/unmappable mode occurrence — the D7 ignored-mode
// alarm's unit. Scope is "site" (arbitration/conversion drops, shared by all
// devices) or a device name (capability drops).
type ignoredMode struct {
	Scope  string
	Mode   string
	Reason string
	MRID   string
}

// key identifies an ignored-mode EPISODE for edge-triggered alarming: a new
// control (different MRID) re-dropping the same mode is a new episode and
// re-alarms; the same standing control's drop alarms exactly once.
func (i ignoredMode) key() string {
	return i.Scope + "/" + i.Mode + "@" + i.MRID + ":" + i.Reason
}

// reactiveCandidate is one reactive-power mode competing in D7 arbitration.
type reactiveCandidate struct {
	kind         string // bus.AdvReactive* vocabulary
	eventSourced bool
	mode         bus.AdvReactiveMode
}

// reactiveRank is D7's dynamic-over-static ordering within one source tier.
var reactiveRank = map[string]int{
	bus.AdvReactiveVoltVar:  3,
	bus.AdvReactiveWattVar:  2,
	bus.AdvReactiveFixedVar: 1,
	bus.AdvReactiveFixedPF:  0,
}

// arbitrateReactive picks the single winning reactive mode per D7: event-
// sourced beats default-sourced, then higher reactiveRank (voltVar > wattVar
// > fixedVar > fixedPF). Everything else is returned as dropped, for the
// ignored-mode alarm. nil winner ⇔ no candidates.
func arbitrateReactive(cands []reactiveCandidate) (winner *reactiveCandidate, dropped []reactiveCandidate) {
	best := -1
	for i := range cands {
		if best < 0 {
			best = i
			continue
		}
		b, c := cands[best], cands[i]
		if (c.eventSourced && !b.eventSourced) ||
			(c.eventSourced == b.eventSourced && reactiveRank[c.kind] > reactiveRank[b.kind]) {
			best = i
		}
	}
	if best < 0 {
		return nil, nil
	}
	w := cands[best]
	for i := range cands {
		if i != best {
			dropped = append(dropped, cands[i])
		}
	}
	return &w, dropped
}

// computeRvrtTmsS is the C3 reversion-window computation: ValidUntil−serverNow
// clamped to [60 s, 24 h]; nil when the control carries no ValidUntil. The
// floor keeps a nearly-expired control from commanding an instant reversion
// race; the ceiling bounds how long a device free-runs on stale intent if the
// hub dies right after publishing.
func computeRvrtTmsS(validUntil, serverNow int64) *int64 {
	if validUntil == 0 {
		return nil
	}
	d := validUntil - serverNow
	if d < 60 {
		d = 60
	}
	if d > 24*3600 {
		d = 24 * 3600
	}
	return &d
}

// droopFromSchedule converts 2030.5 opModFreqDroop parameters
// (bus.FreqDroopMsg — mHz dead band, mHz full-response deviation, hundredths-
// of-a-second open-loop time) into the SunSpec model 711 Ctl vocabulary the
// doc carries (bus.AdvFreqDroop):
//
//   - dbof = dbuf = dBuf/1000 Hz (2030.5's dead band is symmetric);
//   - kof = kuf = (dF/1000)/advNominalHz — the per-unit droop slope: full
//     (1.0 pu) power response at dF beyond the dead band, e.g. dF=3000 mHz
//     ⇒ 5 % droop at 60 Hz;
//   - olrt_s = openLoopTms/100.
//
// dP (absolute W/Hz — redundant with dF in per-unit form and device-rating-
// dependent) and tResponse (no 711 Ctl register) are deliberately not
// carried. A dF of 0 makes the droop gain underivable: the axis is dropped
// with an ignored-mode alarm rather than published with a zero (undefined)
// slope — fail closed, never fabricate.
func droopFromSchedule(fd bus.FreqDroopMsg) (*bus.AdvFreqDroop, bool) {
	if fd.DF == 0 {
		return nil, false
	}
	db := float64(fd.DBuf) / 1000
	k := (float64(fd.DF) / 1000) / advNominalHz
	return &bus.AdvFreqDroop{
		DbOfHz: db,
		DbUfHz: db,
		KOf:    k,
		KUf:    k,
		OlrtS:  float64(fd.OpenLoopTms) / 100,
	}, true
}

// advCurveFromEntry converts one CurveSetEntry into the doc's AdvCurve shape,
// stamping the per-curve content hash (CurveSetContentHash over the single
// canonical entry — the D6 no-op re-adoption key).
func advCurveFromEntry(e bus.CurveSetEntry) *bus.AdvCurve {
	return &bus.AdvCurve{
		CurveType: e.CurveType,
		XMult:     e.XMult,
		YMult:     e.YMult,
		YRefType:  e.YRefType,
		Points:    append([]bus.CurvePoint(nil), e.Points...),
		Hash:      bus.CurveSetContentHash([]bus.CurveSetEntry{e}),
	}
}

// curvePlausible is the author's defense-in-depth copy of the 2030.5 DERCurve
// point bound the northbound publisher already gates on (1..10 points).
func curvePlausible(e bus.CurveSetEntry) bool {
	return len(e.Points) >= 1 && len(e.Points) <= 10
}

// siteAxes is the site-level resolved advanced operating point — what D7
// arbitration produces from one (control, curves, droop) input set, before
// the per-device capability mapping.
type siteAxes struct {
	reactive     *bus.AdvReactiveMode
	voltWatt     *bus.AdvCurve
	freqWatt     *bus.AdvCurve
	freqDroop    *bus.AdvFreqDroop
	trips        *bus.AdvTrips
	energize     *bool
	setGradW     *float64
	setSoftGradW *float64
	rvrtTmsS     *int64
	source       string // "csip-event" | "csip-default" | "none"
	mrid         string
}

// advSourceOf maps ActiveControl.Source onto the desired-doc Source
// vocabulary.
func advSourceOf(ctrlSource string) string {
	switch ctrlSource {
	case "event":
		return "csip-event"
	case "default":
		return "csip-default"
	default:
		return "none"
	}
}

// buildSiteAxes resolves the site-level axes from the active control, its
// MATCHED curve entries (nil-able map keyed by CurveSet mode — the caller has
// already verified SetID == CurveSetID), and the schedule-carried droop
// parameters for the control's MRID (nil when none). Pure: all I/O-free, so
// the D7 table tests drive it directly.
func buildSiteAxes(ctrl *bus.ActiveControl, entries map[string]bus.CurveSetEntry, droop *bus.FreqDroopMsg, serverNow int64) (siteAxes, []ignoredMode) {
	s := siteAxes{source: advSourceOf(ctrl.Source), mrid: ctrl.MRID}
	if s.source == "none" {
		// No active control: every axis stays nil — the all-null release doc.
		return s, nil
	}
	var ignored []ignoredMode
	drop := func(mode, reason string) {
		ignored = append(ignored, ignoredMode{Scope: "site", Mode: mode, Reason: reason, MRID: ctrl.MRID})
	}
	curve := func(mode string) *bus.AdvCurve {
		e, ok := entries[mode]
		if !ok {
			return nil
		}
		if !curvePlausible(e) {
			drop(mode, "implausible-curve")
			return nil
		}
		return advCurveFromEntry(e)
	}

	// ── Reactive candidates (D7 rule 1). All candidates on one resolved
	// control share its source; arbitrateReactive still takes per-candidate
	// sourcing because 2030.5 servers may blend default-sourced reactive
	// modes under an event control in a future carriage — the rule is
	// implemented, not just the degenerate case.
	eventSourced := ctrl.Source == "event"
	var cands []reactiveCandidate
	if c := curve(bus.CurveModeVoltVar); c != nil {
		cands = append(cands, reactiveCandidate{kind: bus.AdvReactiveVoltVar, eventSourced: eventSourced,
			mode: bus.AdvReactiveMode{Kind: bus.AdvReactiveVoltVar, Curve: c}})
	}
	if c := curve(bus.CurveModeWattPF); c != nil {
		// D6/D7's "watt_var" axis, carried by 2030.5's opModWattPF curve —
		// see bus.AdvReactiveWattVar's doc for the naming decision.
		cands = append(cands, reactiveCandidate{kind: bus.AdvReactiveWattVar, eventSourced: eventSourced,
			mode: bus.AdvReactiveMode{Kind: bus.AdvReactiveWattVar, Curve: c}})
	}
	if ctrl.FixedVarPct != nil {
		pct := *ctrl.FixedVarPct
		cands = append(cands, reactiveCandidate{kind: bus.AdvReactiveFixedVar, eventSourced: eventSourced,
			mode: bus.AdvReactiveMode{Kind: bus.AdvReactiveFixedVar, FixedVarPct: &pct}})
	}
	if ctrl.FixedPFInject != nil || ctrl.FixedPFAbsorb != nil {
		pf := ctrl.FixedPFInject
		if pf == nil {
			pf = ctrl.FixedPFAbsorb
		} else if ctrl.FixedPFAbsorb != nil {
			// One fixed-PF slot on the doc (D6): keep the inject half, alarm
			// the absorb half — see bus.AdvReactiveMode.FixedPF's doc.
			drop("fixed_pf_absorb", "single-fixed-pf-slot")
		}
		cp := *pf
		cands = append(cands, reactiveCandidate{kind: bus.AdvReactiveFixedPF, eventSourced: eventSourced,
			mode: bus.AdvReactiveMode{Kind: bus.AdvReactiveFixedPF, FixedPF: &cp}})
	}
	winner, droppedCands := arbitrateReactive(cands)
	if winner != nil {
		m := winner.mode
		s.reactive = &m
	}
	for _, d := range droppedCands {
		drop(d.kind, "reactive-mode-conflict")
	}

	// ── Concurrent overlays (D7 rule 2) and trips (rule 3): pass through.
	s.voltWatt = curve(bus.CurveModeVoltWatt)
	s.freqWatt = curve(bus.CurveModeFreqWatt)
	trips := &bus.AdvTrips{}
	haveTrips := false
	for _, ax := range advTripAxes {
		var setEntries []bus.CurveSetEntry
		var curves []bus.AdvTripCurve
		for _, k := range ax.kinds {
			e, ok := entries[k.mode]
			if !ok {
				continue
			}
			if !curvePlausible(e) {
				drop(k.mode, "implausible-curve")
				continue
			}
			setEntries = append(setEntries, e)
			curves = append(curves, bus.AdvTripCurve{Kind: k.kind, Curve: *advCurveFromEntry(e)})
		}
		if len(curves) == 0 {
			continue
		}
		set := &bus.AdvTripSet{Curves: curves, Hash: bus.CurveSetContentHash(setEntries)}
		switch ax.axis {
		case "lv":
			trips.LV = set
		case "hv":
			trips.HV = set
		case "lf":
			trips.LF = set
		case "hf":
			trips.HF = set
		}
		haveTrips = true
	}
	if haveTrips {
		s.trips = trips
	}

	// ── Frequency droop: schedule-carried parameters for this control.
	if droop != nil {
		if fd, ok := droopFromSchedule(*droop); ok {
			s.freqDroop = fd
		} else {
			drop("freq_droop", "underivable-gain")
		}
	}

	// ── Scalars + the C3 reversion window.
	s.energize = ctrl.Energize
	s.setGradW = ctrl.SetGradW
	s.setSoftGradW = ctrl.SetSoftGradW
	s.rvrtTmsS = computeRvrtTmsS(ctrl.ValidUntil, serverNow)
	return s, ignored
}

// advDevice is one eligible target device (inverters + batteries; EVSEs are
// excluded — no adv axes exist for them yet) with its capability axis set.
type advDevice struct {
	name  string
	class string          // bus.DesiredClassSolar | bus.DesiredClassBattery
	axes  map[string]bool // advCapabilityAxes row for the device's DERGen
}

// buildDeviceDoc maps the site axes onto one device per its capability set
// (D7 rule 4): a commanded axis the device supports lands in the doc; one it
// does not stays an explicit null and raises the ignored-mode alarm. Pure.
func buildDeviceDoc(dev advDevice, s siteAxes) (bus.DesiredAdvanced, []ignoredMode) {
	doc := bus.DesiredAdvanced{
		Envelope:    bus.Envelope{V: bus.DesiredAdvancedV},
		DeviceClass: dev.class,
		DeviceID:    dev.name,
		Source:      s.source,
		MRID:        s.mrid,
	}
	var ignored []ignoredMode
	unsupported := func(mode string) {
		ignored = append(ignored, ignoredMode{Scope: dev.name, Mode: mode, Reason: "unsupported-by-device", MRID: s.mrid})
	}
	apply := func(axis, mode string, commanded bool, set func()) {
		if !commanded {
			return
		}
		if dev.axes[axis] {
			set()
			return
		}
		unsupported(mode)
	}

	reactiveMode := ""
	if s.reactive != nil {
		reactiveMode = s.reactive.Kind
	}
	apply(advAxisReactive, reactiveMode, s.reactive != nil, func() { doc.ReactiveMode = s.reactive })
	apply(advAxisVoltWatt, advAxisVoltWatt, s.voltWatt != nil, func() { doc.VoltWatt = s.voltWatt })
	apply(advAxisFreqWatt, advAxisFreqWatt, s.freqWatt != nil, func() { doc.FreqWatt = s.freqWatt })
	apply(advAxisFreqDroop, advAxisFreqDroop, s.freqDroop != nil, func() { doc.FreqDroop = s.freqDroop })
	apply(advAxisTrips, advAxisTrips, s.trips != nil, func() { doc.Trips = s.trips })
	apply(advAxisEnergize, advAxisEnergize, s.energize != nil, func() { doc.Energize = s.energize })
	apply(advAxisRamp, advAxisRamp, s.setGradW != nil || s.setSoftGradW != nil, func() {
		doc.SetGradW = s.setGradW
		doc.SetSoftGradW = s.setSoftGradW
	})
	apply(advAxisRvrt, advAxisRvrt, s.rvrtTmsS != nil, func() { doc.RvrtTmsS = s.rvrtTmsS })
	return doc, ignored
}

// advContentKey is the dedupe key for one device's doc: its JSON with the
// per-publish stamps zeroed — Envelope (constant), IssuedAt, Seq, and
// RvrtTmsS (a countdown that shrinks every evaluation and must not itself
// force republishes; each actual publish — content change or heartbeat —
// re-stamps it fresh).
func advContentKey(doc bus.DesiredAdvanced) string {
	doc.Envelope = bus.Envelope{}
	doc.IssuedAt = 0
	doc.Seq = 0
	doc.RvrtTmsS = nil
	b, err := json.Marshal(doc)
	if err != nil {
		return "" // cannot happen (finite by construction); forces a republish
	}
	return string(b)
}

// advDocState is one device's publish bookkeeping — the same optimistic-
// update / one-slot-pending / rollback-on-harvest-failure contract as
// desiredPublishingBatteryActuator (see its field docs for the reasoning).
type advDocState struct {
	published    bool
	lastKey      string
	lastIssuedAt int64
	seq          uint64

	pending              *mqttutil.PendingPub
	pendingPrevSet       bool
	pendingPrevPublished bool
	pendingPrevKey       string
	pendingPrevIssuedAt  int64
	pendingPrevSeq       uint64
}

// advAuthor owns the WP-9 authoring state. Concurrency mirrors
// dersiteAggregator: the On* feeds run on MQTT subscription goroutines and
// loop on its own ticker goroutine; mu serializes all of them.
type advAuthor struct {
	mu sync.Mutex

	mc      mqtt.Client
	devices []advDevice

	control     *bus.ActiveControl
	curves      *bus.CurveSet
	droopByMRID map[string]bus.FreqDroopMsg

	st      map[string]*advDocState
	alarmed map[string]bool // ignoredMode.key() → currently alarmed (edge)

	curveHold bool // edge for the curve-set-out-of-sync hold log

	ignoredModes  *metrics.Counter // lexa_hub_ignored_modes_total; nil-safe
	publishes     *metrics.Counter // lexa_hub_desired_adv_publishes_total; nil-safe
	asyncFailures *metrics.Counter // shared lexa_hub_desired_publish_failures_total; nil-safe

	now func() time.Time // test seam; time.Now in production
}

// maybeNewAdvAuthor is main.go's construction gate, factored out so the
// flag-off absence property is directly testable: nil unless advanced_der is
// "on". Flag off ⇒ the author is not even constructed — zero DesiredAdvanced
// publishes and no lexa/csip/curves subscription (every main.go hook is
// `if adv != nil`-guarded), the releasability property WP-9 ships under.
func maybeNewAdvAuthor(mc mqtt.Client, cfg *Config, ignoredModes, publishes, asyncFailures *metrics.Counter) *advAuthor {
	if cfg.AdvancedDER != "on" {
		return nil
	}
	return newAdvAuthor(mc, cfg, ignoredModes, publishes, asyncFailures)
}

// newAdvAuthor builds the author over the config's inverter/battery devices.
// Called only when cfg.AdvancedDER == "on" (maybeNewAdvAuthor's gate).
func newAdvAuthor(mc mqtt.Client, cfg *Config, ignoredModes, publishes, asyncFailures *metrics.Counter) *advAuthor {
	a := &advAuthor{
		mc:            mc,
		droopByMRID:   map[string]bus.FreqDroopMsg{},
		st:            map[string]*advDocState{},
		alarmed:       map[string]bool{},
		ignoredModes:  ignoredModes,
		publishes:     publishes,
		asyncFailures: asyncFailures,
		now:           time.Now,
	}
	for _, dc := range cfg.Devices {
		var class string
		switch dc.Role {
		case "inverter":
			class = bus.DesiredClassSolar
		case "battery":
			class = bus.DesiredClassBattery
		default:
			continue // meters get no doc; EVSEs are not devices[] entries
		}
		a.devices = append(a.devices, advDevice{name: dc.Name, class: class, axes: advCapabilityAxes[dc.DERGen]})
		a.st[dc.Name] = &advDocState{}
	}
	return a
}

// OnControl feeds the retained lexa/csip/control (hooked additively into the
// existing subscription in main.go) and re-evaluates immediately.
func (a *advAuthor) OnControl(msg bus.ActiveControl) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.control = &msg
	a.evaluateLocked()
}

// OnCurves feeds the retained lexa/csip/curves doc and re-evaluates.
func (a *advAuthor) OnCurves(msg bus.CurveSet) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.curves = &msg
	a.evaluateLocked()
}

// OnSchedule extracts the schedule-carried freq-droop parameters per control
// MRID (the WP-8 carriage seam — see droopFromSchedule) and re-evaluates.
func (a *advAuthor) OnSchedule(msg bus.DERScheduleMsg) {
	m := map[string]bus.FreqDroopMsg{}
	for _, s := range msg.Slots {
		if s.FreqDroop != nil {
			m[s.MRID] = *s.FreqDroop
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.droopByMRID = m
	a.evaluateLocked()
}

// loop is the author's owned goroutine: harvest + heartbeat on a fixed
// cadence, forever (crash-only, AD-011 — same shape as dersiteAggregator).
func (a *advAuthor) loop() {
	t := time.NewTicker(advEvalInterval)
	defer t.Stop()
	for range t.C {
		a.Evaluate()
	}
}

// Evaluate re-runs the full author pass (harvest, arbitrate, map, publish).
func (a *advAuthor) Evaluate() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.evaluateLocked()
}

func (a *advAuthor) evaluateLocked() {
	// Harvest every outstanding publish first, so a failed/timed-out one has
	// its baseline rolled back before this pass's dedupe comparisons.
	for i := range a.devices {
		a.harvestLocked(a.devices[i].name)
	}

	if a.control == nil {
		return // nothing ever adopted — no opinion to publish (or heartbeat)
	}

	// Correlate control ↔ curves by content hash. A mismatch means the two
	// retained docs are one walk apart: HOLD (publish nothing this pass)
	// rather than command a curve release the control never asked for —
	// both docs are retained, so the pair converges within one walk period.
	entries, inSync := a.matchedCurvesLocked()
	if !inSync {
		if !a.curveHold {
			slog.Warn("[hub] adv author holding: lexa/csip/curves does not match control's curve_set_id",
				"mrid", a.control.MRID, "curve_set_id", a.control.CurveSetID)
			a.curveHold = true
		}
		return
	}
	if a.curveHold {
		slog.Info("[hub] adv author curve set back in sync", "curve_set_id", a.control.CurveSetID)
		a.curveHold = false
	}

	now := a.now()
	serverNow := now.Unix() + a.control.ClockOffset
	var droop *bus.FreqDroopMsg
	if fd, ok := a.droopByMRID[a.control.MRID]; ok {
		droop = &fd
	}
	site, ignored := buildSiteAxes(a.control, entries, droop, serverNow)

	fresh := map[string]ignoredMode{}
	for _, ig := range ignored {
		fresh[ig.key()] = ig
	}
	for _, dev := range a.devices {
		doc, devIgnored := buildDeviceDoc(dev, site)
		for _, ig := range devIgnored {
			fresh[ig.key()] = ig
		}
		a.publishDeviceLocked(dev, doc, now)
	}
	a.updateAlarmsLocked(fresh)
}

// matchedCurvesLocked returns the curve entries by mode iff the retained
// curves doc matches the control's curve_set_id ("" = no curves ⇒ empty map,
// trivially in sync). mu held.
func (a *advAuthor) matchedCurvesLocked() (map[string]bus.CurveSetEntry, bool) {
	if a.control.CurveSetID == "" {
		return nil, true
	}
	if a.curves == nil || a.curves.SetID != a.control.CurveSetID {
		return nil, false
	}
	m := make(map[string]bus.CurveSetEntry, len(a.curves.Curves))
	for _, e := range a.curves.Curves {
		m[e.Mode] = e
	}
	return m, true
}

// updateAlarmsLocked edge-triggers the ignored-mode alarm: one WARN + one
// lexa_hub_ignored_modes_total increment per NEW ignored-mode episode
// (ignoredMode.key), re-armed when the episode clears. mu held.
func (a *advAuthor) updateAlarmsLocked(fresh map[string]ignoredMode) {
	for key, ig := range fresh {
		if a.alarmed[key] {
			continue
		}
		a.alarmed[key] = true
		a.ignoredModes.Inc()
		slog.Warn("[hub] advanced-DER mode ignored",
			"scope", ig.Scope, "mode", ig.Mode, "reason", ig.Reason, "mrid", ig.MRID)
	}
	for key := range a.alarmed {
		if _, still := fresh[key]; !still {
			delete(a.alarmed, key) // episode over — re-arm
		}
	}
}

// harvestLocked resolves device's pending publish if its outcome is known
// (or its timeout budget spent), rolling the dedupe baseline back on
// failure so the identical content republishes on the next evaluation —
// desiredPublishingBatteryActuator.harvestPending's contract, per device.
// Returns whether a rollback happened. mu held.
func (a *advAuthor) harvestLocked(device string) (rolledBack bool) {
	st := a.st[device]
	if st == nil || st.pending == nil {
		return false
	}
	done, timedOut, err := st.pending.Harvest(mqttutil.PublishTimeout)
	if !done && !timedOut {
		return false
	}
	if done && err == nil {
		st.pending = nil
		return false
	}
	if err != nil {
		log.Printf("lexa-hub: publish desired adv %s: %v (async)", device, err)
	} else {
		log.Printf("lexa-hub: publish desired adv %s: no ack after %s (async)", device, mqttutil.PublishTimeout)
	}
	if st.pendingPrevSet {
		st.published = st.pendingPrevPublished
		st.lastKey = st.pendingPrevKey
		st.lastIssuedAt = st.pendingPrevIssuedAt
		st.seq = st.pendingPrevSeq
	}
	st.pending = nil
	a.asyncFailures.Inc()
	return true
}

// publishDeviceLocked fires one device's retained doc when its content
// changed or the WS-2 heartbeat is due — async, one-slot pending,
// opportunistic immediate harvest (never blocks). mu held.
func (a *advAuthor) publishDeviceLocked(dev advDevice, doc bus.DesiredAdvanced, now time.Time) {
	st := a.st[dev.name]
	if st == nil {
		return
	}
	key := advContentKey(doc)
	changed := !st.published || key != st.lastKey
	heartbeat := st.published &&
		now.Unix()-st.lastIssuedAt >= int64(desiredHeartbeatInterval/time.Second)
	if !changed && !heartbeat {
		return
	}

	doc.IssuedAt = now.Unix()
	doc.Seq = st.seq

	pp, err := mqttutil.PublishJSONRetainedAsync(a.mc, bus.DesiredAdvTopic(dev.name), doc)
	if err != nil {
		// Marshal error only — nothing queued, nothing to harvest.
		log.Printf("lexa-hub: publish desired adv %s: %v", dev.name, err)
		return
	}

	st.pendingPrevSet = true
	st.pendingPrevPublished = st.published
	st.pendingPrevKey = st.lastKey
	st.pendingPrevIssuedAt = st.lastIssuedAt
	st.pendingPrevSeq = st.seq
	st.published = true
	st.lastKey = key
	st.lastIssuedAt = doc.IssuedAt
	st.seq++
	st.pending = pp

	// Opportunistic immediate check (free — Harvest never blocks): an
	// already-resolved failure rolls back right away instead of next pass.
	if a.harvestLocked(dev.name) {
		return
	}
	a.publishes.Inc()
}
