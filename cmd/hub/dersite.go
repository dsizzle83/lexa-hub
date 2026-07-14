package main

// WP-4 (standards-buildout A2, CORE-009/CORE-014/BASIC-028): the hub-side
// GFEMS dersite aggregator. It folds the hub's existing device state feeds —
// measurements (incl. the WP-2 alarm/op-state fields), battery metrics, EVSE
// state, the breach-episode level — together with the config nameplates into
// ONE site-level bus.DERSiteReport (architecture D2, NORMATIVE) and
// publishes it retained on bus.TopicHubDERSite for lexa-northbound's
// derreport manager to PUT upstream as DERCapability/DERSettings/DERStatus/
// DERAvailability.
//
// Style mirror of cmd/hub/logevent.go: the aggregator is pure state +
// report assembly (buildReportLocked and its helpers are exercised by tests
// without a broker); publishing rides its own fixed-cadence goroutine
// (loop), NEVER the engine tick or a subscription callback, using the
// TASK-046 async fire-then-harvest pattern so no feed path ever blocks on a
// PUBACK.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"log/slog"
	"math"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/metrics"
	"lexa-hub/internal/mqttutil"
	model "lexa-proto/csipmodel"
)

const (
	// dersiteEvalInterval is the aggregator goroutine's evaluation cadence:
	// how often it rebuilds the report and checks whether a publish is due.
	// Cheap (a few sums over a handful of devices), so 5 s keeps status
	// freshness tight without any per-tick coupling to the engine.
	dersiteEvalInterval = 5 * time.Second

	// dersiteMinRepublish is the minimum spacing between content-CHANGED
	// republishes (WP-4 spec: change-detect + 60 s min republish interval).
	// Live status (SoC, output) moves continuously; this bounds the retained
	// topic's write rate without a quantization scheme.
	dersiteMinRepublish = 60 * time.Second

	// dersiteHeartbeat re-publishes an UNCHANGED report (fresh Ts, identical
	// content and ContentHash) so subscribers can tell "nothing changed"
	// from "the hub stopped publishing" — the same staleness-vs-silence
	// reasoning as desiredHeartbeatInterval (WS-2), on a looser cadence
	// because nothing enforces from this doc.
	dersiteHeartbeat = 5 * time.Minute

	// dersiteNoiseW is the rounding grain for derived watt values in the
	// report (availability estimate, storage-mode threshold) so ADC jitter
	// doesn't count as content change — same 10 W noise floor as
	// meterFrozenNoiseW.
	dersiteNoiseW = 10.0
)

// enforcedModes names the 2030.5 DERControlType axes the hub enforces
// end-to-end today — the D2 truth mask's input. A field is true ONLY when
// the whole chain (northbound adoption → bus.ActiveControl → optimizer rule
// → actuator/reconciler → convergence backstop) exists for that axis; never
// advertise what a CannotComply would immediately contradict.
//
// The mask construction is data-driven (modeBitTable) so WP-9/10 flip a bit
// by setting one field from their config gates (advanced_der +
// reconciler.adv=active), with no mask arithmetic to touch.
type enforcedModes struct {
	// Connect: bus.ActiveControl.Connect → busToCSIPControl OpModConnect →
	// optimizer disconnect handling + main.go's eng.Wake fast-path on
	// Connect=false. Covers opModConnect/opModEnergize (one csipmodel bit).
	Connect bool
	// MaxLimW: ActiveControl.MaxLimW → gen-limit rule +
	// checkGenLimitConvergence (meter-independent floor — a hard preserve).
	MaxLimW bool
	// FixedW: ActiveControl.FixedW → fixed-dispatch rule.
	FixedW bool
	// ExpLimW: ActiveControl.ExpLimW → applyExportLimitRule + expGuard +
	// checkExportLimitConvergence.
	ExpLimW bool
	// ImpLimW: ActiveControl.ImpLimW → import cap + checkImportConvergence.
	ImpLimW bool

	// Advanced axes — OFF until the WP-9/10 adv path is live for a device
	// that has the model (advanced_der:"on" + reconciler.adv:"active").
	// Flipping one of these is those WPs' one-line change here.
	FixedVar  bool
	FixedPF   bool
	VoltVar   bool
	FreqWatt  bool
	VoltWatt  bool
	FreqDroop bool
}

// modeBitTable maps each enforcedModes field onto its csipmodel.Mode* bit(s).
// FixedPF covers both absorb and inject bits: the adv shell's
// SetFixedPF is one capability, two DERControlType bits.
var modeBitTable = []struct {
	bit uint32
	on  func(enforcedModes) bool
}{
	{model.ModeConnect, func(e enforcedModes) bool { return e.Connect }},
	{model.ModeMaxLimW, func(e enforcedModes) bool { return e.MaxLimW }},
	{model.ModeFixedW, func(e enforcedModes) bool { return e.FixedW }},
	{model.ModeExpLimW, func(e enforcedModes) bool { return e.ExpLimW }},
	{model.ModeImpLimW, func(e enforcedModes) bool { return e.ImpLimW }},
	{model.ModeFixedVar, func(e enforcedModes) bool { return e.FixedVar }},
	{model.ModeFixedPFAbsorb, func(e enforcedModes) bool { return e.FixedPF }},
	{model.ModeFixedPFInject, func(e enforcedModes) bool { return e.FixedPF }},
	{model.ModeVoltVar, func(e enforcedModes) bool { return e.VoltVar }},
	{model.ModeFreqWatt, func(e enforcedModes) bool { return e.FreqWatt }},
	{model.ModeVoltWatt, func(e enforcedModes) bool { return e.VoltWatt }},
	{model.ModeFreqDroop, func(e enforcedModes) bool { return e.FreqDroop }},
}

// mask folds the enabled fields into the csipmodel.Mode* bitmask.
func (e enforcedModes) mask() uint32 {
	var m uint32
	for _, row := range modeBitTable {
		if row.on(e) {
			m |= row.bit
		}
	}
	return m
}

// hubEnforcedModes is what cmd/hub enforces end-to-end TODAY: the five
// scalar axes (see each enforcedModes field's evidence note). Everything
// advanced stays off until WP-9/10.
func hubEnforcedModes() enforcedModes {
	return enforcedModes{
		Connect: true,
		MaxLimW: true,
		FixedW:  true,
		ExpLimW: true,
		ImpLimW: true,
	}
}

// dersiteAlarmBits maps one device's raw 701 Alrm bitfield onto the 2030.5
// DERStatus alarmStatus category bitmap, through the SAME defensible
// 701→Table 14 mapping the WP-6 LogEvent detector uses (alrm701ToTable14,
// logevent.go) — one mapping, two consumers, so the alarm LEVEL this report
// carries and the alarm EDGES the LogEvent pipeline posts can never
// disagree on vocabulary. Unmapped bits (equipment health, DC-side,
// connection state) contribute nothing, for the reasons documented on
// alrm701ToTable14.
func dersiteAlarmBits(devBits uint32) uint32 {
	var out uint32
	for mask, code := range alrm701ToTable14 {
		if devBits&mask != 0 {
			out |= bus.DERAlarmBitForCode(code)
		}
	}
	return out
}

// deriveDERType picks the DERCapability type code for the site mix (D2):
// a non-zero config override wins (utility-handbook fiat); otherwise
// inverters+batteries (the product's common case) and storage-only are 83,
// PV-only is 80, and anything else — no PV or storage at all — is 1
// (virtual/mixed). EVSEs never factor in until ev_storage exists.
func deriveDERType(devices []DeviceConfig, override uint8) uint8 {
	if override != 0 {
		return override
	}
	var hasPV, hasBatt bool
	for _, dc := range devices {
		switch dc.Role {
		case "inverter":
			hasPV = true
		case "battery":
			hasBatt = true
		}
	}
	switch {
	case hasBatt:
		return bus.DERTypeStorage
	case hasPV:
		return bus.DERTypePV
	default:
		return bus.DERTypeVirtualOrMixed
	}
}

// dersitePolicy is the site-level operational cap the settings computation
// clamps ratings against (D2: setX = min(rtgX, site policy)). No config key
// populates it today — a site without a policy runs settings == ratings —
// but the min() lives here, in one place, so the ≤-by-construction invariant
// (CORE-014) is structural, pinned by dersite_test.go, and a future policy
// source is a field assignment, not new arithmetic.
type dersitePolicy struct {
	MaxW              *float64
	MaxChargeRateW    *float64
	MaxDischargeRateW *float64
	MaxWh             *float64
}

// minCap clamps a rating under an optional policy cap; a negative policy
// value is ignored (a cap below zero is nonsense, not a tighter setting).
func minCap(rtg float64, cap *float64) float64 {
	if cap != nil && *cap >= 0 && *cap < rtg {
		return *cap
	}
	return rtg
}

// dersiteMeas is the slice of a device measurement the aggregator keeps.
type dersiteMeas struct {
	w         float64 // NaN when the device has not reported W
	alarmBits uint32  // 0 when the device reports no 701 Alrm bitfield
	at        time.Time
}

// dersiteAggregator folds device state into bus.DERSiteReport and owns its
// retained publish. Concurrency mirrors logEventDetector: the On* feeds run
// on MQTT subscription goroutines, SetBreachActive on whichever goroutine
// drives emitAlerts, and publishIfDue on the aggregator's own loop
// goroutine; mu serializes all of them.
type dersiteAggregator struct {
	mu sync.Mutex

	mc      mqtt.Client
	devices []DeviceConfig

	derTypeOverride uint8
	modes           enforcedModes
	policy          dersitePolicy

	meas   map[string]dersiteMeas
	batt   map[string]bus.BattMetrics
	battAt map[string]time.Time
	// evse holds the latest per-station EVSE state. Stored but INERT: D2
	// excludes EVSEs from every DER rating/status until `ev_storage`
	// exists — this feed is wired now so that flip is a consumption change
	// here, not new plumbing in main.go.
	evse map[string]bus.EVSEState

	breachActive bool

	// lastFullHash/lastPublishAt gate publishing: full-content change-detect
	// (Ts and ReadingTs excluded) with dersiteMinRepublish spacing, plus the
	// dersiteHeartbeat unchanged-content refresh. prev* are the rollback
	// targets when a harvested async publish turns out to have failed —
	// the same optimistic-update-then-roll-back contract as the desired-doc
	// actuators (TASK-046).
	lastFullHash  string
	lastPublishAt time.Time
	prevFullHash  string
	prevPublishAt time.Time
	pending       *mqttutil.PendingPub

	publishes *metrics.Counter // lexa_hub_dersite_publishes_total; nil-safe
	now       func() time.Time // test seam; time.Now in production
}

// newDersiteAggregator builds the aggregator from the hub config's device
// set (nameplates + plant blocks), der_type override, and the current
// enforcement truth (hubEnforcedModes).
func newDersiteAggregator(mc mqtt.Client, cfg *Config, publishes *metrics.Counter) *dersiteAggregator {
	return &dersiteAggregator{
		mc:              mc,
		devices:         cfg.Devices,
		derTypeOverride: uint8(cfg.DERType),
		modes:           hubEnforcedModes(),
		meas:            make(map[string]dersiteMeas),
		batt:            make(map[string]bus.BattMetrics),
		battAt:          make(map[string]time.Time),
		evse:            make(map[string]bus.EVSEState),
		publishes:       publishes,
		now:             time.Now,
	}
}

// OnMeasurement feeds one device measurement (MQTT subscription goroutine).
func (a *dersiteAggregator) OnMeasurement(msg bus.Measurement) {
	a.mu.Lock()
	defer a.mu.Unlock()
	m := dersiteMeas{w: math.NaN(), at: a.now()}
	if msg.W != nil {
		m.w = *msg.W
	}
	if msg.AlarmBits != nil {
		m.alarmBits = *msg.AlarmBits
	}
	a.meas[msg.Device] = m
}

// OnBattMetrics feeds one battery metrics message.
func (a *dersiteAggregator) OnBattMetrics(msg bus.BattMetrics) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.batt[msg.Device] = msg
	a.battAt[msg.Device] = a.now()
}

// OnEVSEState feeds one EVSE state message (stored, inert — see the evse
// field doc).
func (a *dersiteAggregator) OnEVSEState(msg bus.EVSEState) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.evse[msg.StationID] = msg
}

// SetBreachActive mirrors the breach-episode component's merged level (the
// same episodes.Active() emitAlerts already reads) into the site alarm
// bitmap as EMERGENCY_REMOTE — the identical condition WP-6's LogEvent
// pipeline posts edges for, carried here as LEVEL (Table 13) rather than
// occurrence (Table 14).
func (a *dersiteAggregator) SetBreachActive(active bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.breachActive = active
}

// loop is the aggregator's owned goroutine: evaluate + maybe-publish on a
// fixed cadence, forever (crash-only, AD-011 — no shutdown plumbing, same
// as the breach-snapshot resave goroutine in main.go).
func (a *dersiteAggregator) loop() {
	t := time.NewTicker(dersiteEvalInterval)
	defer t.Stop()
	for range t.C {
		a.publishIfDue()
	}
}

// buildReportLocked assembles the current bus.DERSiteReport per D2. mu held.
func (a *dersiteAggregator) buildReportLocked(now time.Time) bus.DERSiteReport {
	rep := bus.DERSiteReport{
		Envelope:       bus.Envelope{V: bus.DERSiteReportV},
		DERType:        deriveDERType(a.devices, a.derTypeOverride),
		ModesSupported: a.modes.mask(),
	}

	// ── Ratings: physical sums at the PCC (D2). Battery rate/capacity
	// sources, best first: live BattMetrics (device truth, held even when
	// stale — a nameplate does not expire), plant block, config max_w.
	var batteryDischargeSum float64
	for i := range a.devices {
		dc := &a.devices[i]
		switch dc.Role {
		case "inverter":
			rep.RtgMaxW += dc.MaxW
		case "battery":
			charge, discharge := dc.MaxW, dc.MaxW
			capWh := 0.0
			if dc.BatteryPlant.CapacityKWh > 0 {
				capWh = dc.BatteryPlant.CapacityKWh * 1000
			}
			if bm, ok := a.batt[dc.Name]; ok {
				if bm.MaxChargeW != nil && *bm.MaxChargeW > 0 {
					charge = *bm.MaxChargeW
				}
				if bm.MaxDischargeW != nil && *bm.MaxDischargeW > 0 {
					discharge = *bm.MaxDischargeW
				}
				if bm.CapacityWh != nil && *bm.CapacityWh > 0 {
					capWh = *bm.CapacityWh
				}
			}
			rep.RtgMaxChargeRateW += charge
			rep.RtgMaxDischargeRateW += discharge
			rep.RtgMaxWh += capWh
			batteryDischargeSum += discharge
		}
	}
	// The batteries in this product export at the PCC (peak-shift discharge
	// is the optimizer's whole economics tier), so their discharge rating
	// counts toward the site's max active power (D2).
	rep.RtgMaxW += batteryDischargeSum
	// VA/Var ratings: NO source exists today (no nameplate VA/Var in config,
	// plant blocks, or BattMetrics) — the fields stay nil rather than
	// echoing the W rating as VA (G27: never fabricate).

	// ── Settings: min(rtg, site policy) — ≤ ratings BY CONSTRUCTION.
	rep.SetMaxW = minCap(rep.RtgMaxW, a.policy.MaxW)
	rep.SetMaxChargeRateW = minCap(rep.RtgMaxChargeRateW, a.policy.MaxChargeRateW)
	rep.SetMaxDischargeRateW = minCap(rep.RtgMaxDischargeRateW, a.policy.MaxDischargeRateW)
	rep.SetMaxWh = minCap(rep.RtgMaxWh, a.policy.MaxWh)
	// SetMaxVA/SetMaxVar mirror the (absent) VA/Var ratings: nil.

	// ── Live status block.
	rep.Status = a.buildStatusLocked(now)

	// ── Availability block (only where derivable).
	rep.Avail = a.buildAvailLocked(now)

	// ── Capability/settings-scoped content hash (the G29 on-change trigger
	// northbound keys its DERCapability/DERSettings PUTs on).
	rep.ContentHash = dersiteCapHash(rep)
	return rep
}

// buildStatusLocked derives the aggregate status block. mu held.
func (a *dersiteAggregator) buildStatusLocked(now time.Time) bus.DERSiteStatus {
	st := bus.DERSiteStatus{ReadingTs: now.Unix()}

	var (
		genConnected  bool
		anyActive     bool
		battNetW      float64
		haveBattMeas  bool
		socWeightNum  float64
		socWeightDen  float64
		socEqualSum   float64
		socCount      int
		allSocWeighed = true
	)
	for i := range a.devices {
		dc := &a.devices[i]
		m, ok := a.meas[dc.Name]
		fresh := ok && now.Sub(m.at) <= measStaleAfter
		switch dc.Role {
		case "inverter", "battery":
			if fresh && !math.IsNaN(m.w) {
				genConnected = true
				if math.Abs(m.w) > dersiteNoiseW {
					anyActive = true
				}
				if dc.Role == "battery" {
					battNetW += m.w
					haveBattMeas = true
				}
			}
			if fresh {
				st.AlarmBits |= dersiteAlarmBits(m.alarmBits)
			}
		}
		if dc.Role == "battery" {
			if bm, ok := a.batt[dc.Name]; ok && bm.SOC != nil {
				socCount++
				socEqualSum += *bm.SOC
				capWh := 0.0
				if bm.CapacityWh != nil && *bm.CapacityWh > 0 {
					capWh = *bm.CapacityWh
				} else if dc.BatteryPlant.CapacityKWh > 0 {
					capWh = dc.BatteryPlant.CapacityKWh * 1000
				}
				if capWh > 0 {
					socWeightNum += *bm.SOC * capWh
					socWeightDen += capWh
				} else {
					allSocWeighed = false
				}
			}
		}
	}

	if socCount > 0 {
		// Capacity-weighted (D2) when every SOC-bearing pack has a known
		// capacity; plain average otherwise (a deterministic fallback beats
		// a partial weighting that silently over-counts the known packs).
		var soc float64
		if allSocWeighed && socWeightDen > 0 {
			soc = socWeightNum / socWeightDen
		} else {
			soc = socEqualSum / float64(socCount)
		}
		soc = math.Round(soc*10) / 10 // 0.1 % grain: real movement, not jitter
		st.SocPct = &soc
	}

	if genConnected {
		st.GenConnectStatus = model.GenConnectConnected
	} else {
		st.GenConnectStatus = model.GenConnectAvailable
	}
	if anyActive {
		st.OperationalMode = model.OpStatusOperating
	} else {
		st.OperationalMode = model.OpStatusIdle
	}
	if haveBattMeas {
		// Measurement sign convention: + discharge, − charge.
		mode := model.StorageIdle
		switch {
		case battNetW > dersiteNoiseW:
			mode = model.StorageDischarging
		case battNetW < -dersiteNoiseW:
			mode = model.StorageCharging
		}
		st.StorageMode = &mode
	}
	if a.breachActive {
		st.AlarmBits |= bus.DERAlarmEmergencyRemote
	}
	return st
}

// buildAvailLocked derives the availability block, or nil when nothing is
// derivable (G27). mu held.
func (a *dersiteAggregator) buildAvailLocked(now time.Time) *bus.DERSiteAvailability {
	var (
		wAvail     float64
		haveWAvail bool
		remainWh   float64
		headroomWh float64
		haveEnergy bool
		chargeSum  float64
		dischSum   float64
	)
	for i := range a.devices {
		dc := &a.devices[i]
		switch dc.Role {
		case "inverter":
			if m, ok := a.meas[dc.Name]; ok && now.Sub(m.at) <= measStaleAfter && !math.IsNaN(m.w) {
				// An inverter can promise no more than its current output —
				// the sun is not dispatchable.
				wAvail += math.Max(0, m.w)
				haveWAvail = true
			}
		case "battery":
			bm, ok := a.batt[dc.Name]
			if !ok || now.Sub(a.battAt[dc.Name]) > measStaleAfter {
				continue
			}
			discharge, charge := dc.MaxW, dc.MaxW
			if bm.MaxDischargeW != nil && *bm.MaxDischargeW > 0 {
				discharge = *bm.MaxDischargeW
			}
			if bm.MaxChargeW != nil && *bm.MaxChargeW > 0 {
				charge = *bm.MaxChargeW
			}
			if bm.SOC != nil && *bm.SOC > 0 {
				wAvail += discharge
				haveWAvail = true
			}
			if bm.SOC != nil && bm.CapacityWh != nil && *bm.CapacityWh > 0 {
				remainWh += *bm.CapacityWh * (*bm.SOC) / 100
				headroomWh += *bm.CapacityWh * (100 - *bm.SOC) / 100
				haveEnergy = true
				chargeSum += charge
				dischSum += discharge
			}
		}
	}

	av := &bus.DERSiteAvailability{}
	populated := false
	if haveWAvail {
		w := math.Round(wAvail/dersiteNoiseW) * dersiteNoiseW
		av.EstimatedWAvailW = &w
		populated = true
	}
	if haveEnergy && dischSum > 0 {
		d := uint32(remainWh / dischSum * 3600)
		av.AvailabilityDurationS = &d
		populated = true
	}
	if haveEnergy && chargeSum > 0 {
		d := uint32(headroomWh / chargeSum * 3600)
		av.MaxChargeDurationS = &d
		populated = true
	}
	if !populated {
		return nil
	}
	return av
}

// dersiteCapHash hashes the capability/settings-scoped content only —
// der_type, modes_supported, and every rtg_*/set_* field — by zeroing the
// live blocks and per-publish stamps before marshalling. This is
// bus.DERSiteReport.ContentHash's producer; see that field's doc for why
// status/availability are excluded (SoC jitter must never re-PUT nameplate
// data).
func dersiteCapHash(r bus.DERSiteReport) string {
	r.Status = bus.DERSiteStatus{}
	r.Avail = nil
	r.Ts = 0
	r.ContentHash = ""
	return dersiteHashJSON(r)
}

// dersiteFullHash hashes the WHOLE report content minus per-publish stamps
// (Ts, Status.ReadingTs, ContentHash — derived, changes with nothing else) —
// the hub-side publish gate's change detector, so a status-only change (SoC
// moved) still republishes the retained doc on the 60 s-bounded cadence.
func dersiteFullHash(r bus.DERSiteReport) string {
	r.Ts = 0
	r.Status.ReadingTs = 0
	r.ContentHash = ""
	return dersiteHashJSON(r)
}

func dersiteHashJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		// Cannot happen for this hand-built struct (no NaN pointers by
		// construction); an empty hash just forces a republish.
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// publishIfDue rebuilds the report and publishes it when (a) it has never
// been published, (b) its content changed and dersiteMinRepublish has
// elapsed, or (c) the unchanged-content heartbeat is due. Async publish,
// one-slot pending, harvested at the next call — TASK-046, same contract as
// the desired-doc actuators including the failed/timed-out rollback (the
// content republishes on the next evaluation).
func (a *dersiteAggregator) publishIfDue() {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.harvestLocked()
	if a.pending != nil {
		// Previous publish still in flight within its timeout budget: one
		// slot only — re-evaluate next tick rather than stacking publishes.
		return
	}

	now := a.now()
	rep := a.buildReportLocked(now)
	fullHash := dersiteFullHash(rep)
	changed := fullHash != a.lastFullHash

	switch {
	case a.lastPublishAt.IsZero():
		// First report: publish immediately so northbound has capability
		// data as soon as it subscribes.
	case changed:
		if now.Sub(a.lastPublishAt) < dersiteMinRepublish {
			return
		}
	default:
		if now.Sub(a.lastPublishAt) < dersiteHeartbeat {
			return
		}
	}

	rep.Ts = now.Unix()
	pp, err := mqttutil.PublishJSONRetainedAsync(a.mc, bus.TopicHubDERSite, rep)
	if err != nil {
		// Marshal error only (PublishJSONRetainedAsync's doc); nothing queued.
		log.Printf("lexa-hub: publish dersite: %v", err)
		return
	}

	first := a.lastPublishAt.IsZero()
	a.prevFullHash, a.prevPublishAt = a.lastFullHash, a.lastPublishAt
	a.lastFullHash, a.lastPublishAt = fullHash, now
	a.pending = pp

	// Opportunistic immediate harvest (free — Harvest never blocks): an
	// already-resolved error rolls back right away instead of next tick.
	if a.harvestLocked() {
		return
	}
	a.publishes.Inc()
	if first {
		// One edge line per process lifetime (TASK-045 discipline).
		slog.Info("lexa-hub: dersite site report published",
			"der_type", rep.DERType, "modes", rep.ModesSupported,
			"rtg_max_w", rep.RtgMaxW, "content_hash", rep.ContentHash)
	}
}

// harvestLocked resolves the pending publish if its outcome is known (or its
// timeout budget spent), rolling the change-detect baseline back on failure
// so the identical content republishes on the next evaluation. Returns
// whether a rollback happened. mu held.
func (a *dersiteAggregator) harvestLocked() (rolledBack bool) {
	if a.pending == nil {
		return false
	}
	done, timedOut, err := a.pending.Harvest(mqttutil.PublishTimeout)
	if !done && !timedOut {
		return false
	}
	if done && err == nil {
		a.pending = nil
		return false
	}
	if err != nil {
		log.Printf("lexa-hub: publish dersite: %v (async)", err)
	} else {
		log.Printf("lexa-hub: publish dersite: no ack after %s (async)", mqttutil.PublishTimeout)
	}
	a.lastFullHash, a.lastPublishAt = a.prevFullHash, a.prevPublishAt
	a.pending = nil
	return true
}
