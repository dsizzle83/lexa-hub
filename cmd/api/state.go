package main

import (
	"sort"
	"sync"
	"time"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/utilitytime"
)

// deviceSnap is the per-device state aggregated from MQTT.
type deviceSnap struct {
	Name      string
	Role      string  // from config: inverter | battery | meter
	MaxW      float64 // from config
	W         *float64
	V         *float64
	Hz        *float64
	SOC       *float64 // batteries only
	UpdatedAt time.Time
	// WChangedAt is when W last took a NEW value. Messages keep arriving (so
	// UpdatedAt stays fresh) even when a device's reading is frozen — a hung meter
	// serving a cached register, the INV-STALE hazard — so value-freshness needs
	// this separate from arrival-freshness.
	WChangedAt time.Time
}

// evseSnap is the per-EVSE state aggregated from MQTT.
type evseSnap struct {
	State     bus.EVSEState
	UpdatedAt time.Time
}

// scanStatusRingCap bounds stateStore.scanStatusRing (DEVICE_ROADMAP.md
// §4.3): ScanStatus is a transient progress line, not state — a commissioning
// sweep emits a handful of them (one per TCP/RTU phase transition plus
// per-hit progress), so 50 is generous headroom for one scan's whole
// lifetime without the ring growing unbounded across repeated scans.
const scanStatusRingCap = 50

// stateStore is the thread-safe aggregator that backs /status.
//
// It subscribes to MQTT and keeps the latest snapshot per topic, plus enough
// derived fields (CSIP control, DER schedule program count, clock offset) to
// reproduce the JSON shape the demo dashboard expects.
type stateStore struct {
	mu sync.RWMutex

	devices map[string]*deviceSnap // keyed by device name
	evses   map[string]*evseSnap   // keyed by "stationID:connectorID"

	csipControl  *bus.ActiveControl
	csipPrograms int
	clockOffsetS int64

	// utclk anchors utility (server) time to a monotonic instant on every
	// bus.ActiveControl arrival (onCSIPControl), mirroring cmd/hub's
	// MQTTSystemReader.utclk (TASK-037/AD-004 extension): a LOCAL wall-clock
	// step between control arrivals must not move /status's reporting-grace
	// evaluation (buildStatus's utilitytime.ReportGrace check) any more than
	// it may move the hub's own enforcement. clockOffsetS above is still
	// updated on every message (unchanged, cheap fallback for before the
	// first control/schedule ever arrives); snapshot() overrides it with the
	// anchored derivation once utclk has anchored at least once.
	utclk *utilitytime.Clock

	// lastPlan is the hub's most recent plan trace (TopicHubPlan, retained).
	// nil until the first message — /status then reports an empty decision
	// list, which after this field's introduction genuinely means "no plan
	// received", not the historical always-empty stub.
	lastPlan *bus.PlanLog

	// certStatus is lexa-northbound's latest cert-expiry check
	// (TopicNorthboundCertStatus, retained, TASK-072/§10.5). nil until the
	// first message arrives (older northbound builds, or the topic not yet
	// retained on a fresh broker) — /status omits "cert_status" entirely in
	// that case rather than reporting a fabricated OK.
	certStatus *bus.CertStatus

	// hubSettings is the hub's latest effective reserve + active tariff
	// (TopicHubSettings, retained, GAP-8). nil until the first message arrives —
	// /status omits "reserve"/"tariff" in that case rather than fabricating a
	// value (the hub seeds one at startup, so it's normally present within one
	// broker round trip). Folded into /status's "reserve" + "tariff" objects.
	hubSettings *bus.HubSettings

	// hubSchedule is the hub's latest 24-hour plan/forecast series
	// (TopicHubSchedule, retained, GAP-7). nil until the first message arrives —
	// GET /plan reports 503 in that case rather than fabricating a series (the
	// hub publishes it on each replan). Projected into GET /plan's app shape.
	hubSchedule *bus.HubSchedule

	// modeStatus is the hub's latest authoritative plan-author mode
	// (TopicHubMode, retained, DEVICE_ROADMAP.md §3.5/§4.3). nil until the
	// first message arrives — GET /mode reports 503 {"error":"unknown"} in
	// that case rather than guessing a default (the retained topic means
	// this is normally present within one broker round trip of either
	// side's startup).
	modeStatus *bus.ModeStatus

	// cloudLinkStatus is lexa-cloudlink's latest retained status
	// (TopicCloudlinkStatus). nil until the first message arrives — no
	// cloudlink service exists in this repo yet (TASK-085+), so today this
	// stays nil forever on every deployment; folded into /status as
	// "cloud_link" only once non-nil.
	cloudLinkStatus *bus.CloudlinkStatus

	// openADRStatus is lexa-openadr's latest retained VEN health doc
	// (TopicOpenADRStatus, WP-15). nil until the first message arrives — the
	// VEN is idle until openadr.json's vtn_url is set, so on an uncommissioned
	// deployment this stays nil; folded into /status as "openadr" only once
	// non-nil, same additive discipline as cloudLinkStatus above.
	openADRStatus *bus.OpenADRStatus

	// scanStatusRing holds the last scanStatusRingCap ScanStatus progress
	// lines (TopicScanStatus, not retained — a transient one-shot command's
	// progress, not state) across however many scans have run this process
	// lifetime, oldest evicted first.
	scanStatusRing []bus.ScanStatus

	// scanResult is the latest completed commissioning scan
	// (TopicScanResult, retained until commissioning supersedes it). nil
	// until the first scan ever completes.
	scanResult *bus.ScanResult

	// ocppPending is the current set of OCPP stations that have dialed the
	// CSMS but are not yet in ocpp.json (TopicOCPPPending, retained). nil
	// until lexa-ocpp has ever published (including the "no pending
	// stations" empty-slice case, which IS a message — nil here specifically
	// means "never heard from lexa-ocpp's pending-station surface at all").
	ocppPending *bus.PendingStations

	// telemetry is the bounded in-memory ring of recent Measurement/
	// BattMetrics/EVSEState samples backing GET /telemetry/recent
	// (DEVICE_ROADMAP.md §4.3). See telemetry.go's telemetryRingCap doc for
	// the RAM-bound rationale. Always non-nil once newStateStore returns.
	telemetry *telemetryRing

	staleAfter time.Duration
}

func newStateStore(devices []DeviceConfig, staleAfter time.Duration) *stateStore {
	s := &stateStore{
		devices:    make(map[string]*deviceSnap),
		evses:      make(map[string]*evseSnap),
		staleAfter: staleAfter,
		utclk:      utilitytime.New(utilitytime.Config{}),
		telemetry:  newTelemetryRing(telemetryRingCap),
	}
	for _, d := range devices {
		s.devices[d.Name] = &deviceSnap{Name: d.Name, Role: d.Role, MaxW: d.MaxW}
	}
	return s
}

// device returns (and lazily creates) the snapshot for a named device.
// Caller must hold mu for writing.
func (s *stateStore) deviceLocked(name string) *deviceSnap {
	d, ok := s.devices[name]
	if !ok {
		d = &deviceSnap{Name: name, Role: "meter"} // unknown → meter
		s.devices[name] = d
	}
	return d
}

func (s *stateStore) onMeasurement(_ string, m bus.Measurement) {
	now := time.Now()
	s.mu.Lock()
	d := s.deviceLocked(m.Device)
	if m.W != nil {
		v := *m.W
		if d.W == nil || *d.W != v {
			d.WChangedAt = now
		}
		d.W = &v
	}
	if m.VoltageV != nil {
		v := *m.VoltageV
		d.V = &v
	}
	if m.Hz != nil {
		v := *m.Hz
		d.Hz = &v
	}
	d.UpdatedAt = now
	s.mu.Unlock()

	// TASK-088 telemetry ring: ARRIVAL-stamped (not m.Ts — same clock-warp-safe
	// discipline as planHeartbeat/WChangedAt), independent of stateStore's own
	// mutex (telemetryRing has its own).
	s.telemetry.add(telemetrySample{
		Kind: telemetryKindMeasurement, Device: m.Device, ArrivedAt: now,
		W: m.W, VoltageV: m.VoltageV, Hz: m.Hz,
	})
}

func (s *stateStore) onBattMetrics(_ string, m bus.BattMetrics) {
	now := time.Now()
	s.mu.Lock()
	d := s.deviceLocked(m.Device)
	if d.Role == "" {
		d.Role = "battery"
	}
	if m.SOC != nil {
		v := *m.SOC
		d.SOC = &v
	}
	d.UpdatedAt = now
	s.mu.Unlock()

	s.telemetry.add(telemetrySample{
		Kind: telemetryKindBattMetrics, Device: m.Device, ArrivedAt: now,
		SOC: m.SOC, SOH: m.SOH, CapacityWh: m.CapacityWh,
		MaxChargeW: m.MaxChargeW, MaxDischargeW: m.MaxDischargeW,
	})
}

func (s *stateStore) onPlanLog(_ string, p bus.PlanLog) {
	s.mu.Lock()
	s.lastPlan = &p
	s.mu.Unlock()
}

func (s *stateStore) onCertStatus(_ string, c bus.CertStatus) {
	s.mu.Lock()
	s.certStatus = &c
	s.mu.Unlock()
}

func (s *stateStore) onCSIPControl(_ string, c bus.ActiveControl) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.Source == "" || c.Source == "none" {
		s.csipControl = nil
	} else {
		cc := c
		s.csipControl = &cc
	}
	s.clockOffsetS = c.ClockOffset
	// Anchor utility time at this message's arrival (TASK-037), same
	// same-host assumption and rationale as cmd/hub's onCSIPControl: c.Ts is
	// stamped by lexa-northbound with time.Now().Unix() at publish
	// (toActiveControl), so c.Ts+c.ClockOffset is server time AT PUBLISH —
	// valid because lexa-api and lexa-northbound share the hub Pi/SOM clock.
	s.utclk.Anchor(c.Ts + c.ClockOffset)
}

func (s *stateStore) onEVSEState(_ string, e bus.EVSEState) {
	now := time.Now()
	s.mu.Lock()
	key := evseKey(e.StationID, e.ConnectorID)
	s.evses[key] = &evseSnap{State: e, UpdatedAt: now}
	// The ocpp service publishes a synthetic connector-0 entry while a station
	// has no connectors yet (pre-StatusNotification). Once a real connector
	// reports, drop the placeholder so /status doesn't carry a phantom idle
	// EVSE forever (the dashboard was rendering it as "EV idle, 0 W").
	if e.ConnectorID > 0 {
		delete(s.evses, evseKey(e.StationID, 0))
	}
	s.mu.Unlock()

	s.telemetry.add(telemetrySample{
		Kind: telemetryKindEVSE, Device: key, ArrivedAt: now,
		CurrentA: e.CurrentA, MaxCurrentA: e.MaxCurrentA, VoltageV: e.VoltageV,
		PowerW: e.PowerW, EnergyWh: e.EnergyWh, Status: e.Status,
	})
}

func (s *stateStore) onSchedule(_ string, sched bus.DERScheduleMsg) {
	s.mu.Lock()
	defer s.mu.Unlock()
	progs := map[string]struct{}{}
	for _, slot := range sched.Slots {
		if slot.ProgramMRID != "" {
			progs[slot.ProgramMRID] = struct{}{}
		}
	}
	s.csipPrograms = len(progs)
	if sched.ClockOffset != 0 {
		s.clockOffsetS = sched.ClockOffset
	}
}

// onModeStatus records the hub's latest authoritative plan-author mode
// (TopicHubMode, retained, DEVICE_ROADMAP.md §3.5/§4.3).
func (s *stateStore) onModeStatus(_ string, m bus.ModeStatus) {
	s.mu.Lock()
	s.modeStatus = &m
	s.mu.Unlock()
}

// onHubSettings records the hub's latest effective reserve + active tariff
// (TopicHubSettings, retained, GAP-8).
func (s *stateStore) onHubSettings(_ string, h bus.HubSettings) {
	s.mu.Lock()
	s.hubSettings = &h
	s.mu.Unlock()
}

// onHubSchedule records the hub's latest 24-hour plan/forecast series
// (TopicHubSchedule, retained, GAP-7).
func (s *stateStore) onHubSchedule(_ string, h bus.HubSchedule) {
	s.mu.Lock()
	s.hubSchedule = &h
	s.mu.Unlock()
}

// onCloudlinkStatus records lexa-cloudlink's latest retained status
// (TopicCloudlinkStatus).
func (s *stateStore) onCloudlinkStatus(_ string, c bus.CloudlinkStatus) {
	s.mu.Lock()
	s.cloudLinkStatus = &c
	s.mu.Unlock()
}

// onOpenADRStatus records lexa-openadr's latest retained VEN health doc
// (TopicOpenADRStatus, WP-15).
func (s *stateStore) onOpenADRStatus(_ string, o bus.OpenADRStatus) {
	s.mu.Lock()
	s.openADRStatus = &o
	s.mu.Unlock()
}

// onScanStatus appends one commissioning-scan progress line
// (TopicScanStatus, not retained) to the bounded ring, evicting the oldest
// entry once scanStatusRingCap is reached.
func (s *stateStore) onScanStatus(_ string, st bus.ScanStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scanStatusRing = append(s.scanStatusRing, st)
	if len(s.scanStatusRing) > scanStatusRingCap {
		s.scanStatusRing = s.scanStatusRing[len(s.scanStatusRing)-scanStatusRingCap:]
	}
}

// onScanResult records the latest completed commissioning scan
// (TopicScanResult, retained until commissioning supersedes it).
func (s *stateStore) onScanResult(_ string, r bus.ScanResult) {
	s.mu.Lock()
	s.scanResult = &r
	s.mu.Unlock()
}

// onOCPPPending records the current set of unapproved OCPP stations
// (TopicOCPPPending, retained).
func (s *stateStore) onOCPPPending(_ string, p bus.PendingStations) {
	s.mu.Lock()
	s.ocppPending = &p
	s.mu.Unlock()
}

// snapshot returns a deep-copy view safe to render without holding the lock.
type snapshot struct {
	devices      map[string]deviceSnap
	evses        []evseSnap
	csipControl  *bus.ActiveControl
	csipPrograms int
	clockOffsetS int64
	lastPlan     *bus.PlanLog
	certStatus   *bus.CertStatus
	staleAfter   time.Duration
	now          time.Time

	// TASK-088 additions (DEVICE_ROADMAP.md §4.3): all nil/empty until the
	// first corresponding message arrives, exactly like certStatus above.
	modeStatus      *bus.ModeStatus
	cloudLinkStatus *bus.CloudlinkStatus
	scanStatusRing  []bus.ScanStatus
	scanResult      *bus.ScanResult
	ocppPending     *bus.PendingStations

	// WP-15: nil until the first OpenADRStatus arrives, same discipline as above.
	openADRStatus *bus.OpenADRStatus

	// GAP-8: nil until the first HubSettings arrives, same discipline as above.
	hubSettings *bus.HubSettings

	// GAP-7: nil until the first HubSchedule arrives; GET /plan 503s until then.
	hubSchedule *bus.HubSchedule
}

func (s *stateStore) snapshot() snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := snapshot{
		devices:      make(map[string]deviceSnap, len(s.devices)),
		csipPrograms: s.csipPrograms,
		clockOffsetS: s.clockOffsetS,
		staleAfter:   s.staleAfter,
		now:          time.Now(),
	}
	// TASK-037: once a control has ever anchored utclk, prefer the DERIVED
	// offset (utclk.ServerNow() minus THIS snapshot's now) over the raw
	// s.clockOffsetS — bit-identical under a stable local clock (both equal
	// server-minus-local), but immune to a local wall-clock step occurring
	// between control arrivals, matching cmd/hub's ReadSystemState. Before
	// the first control ever arrives, utclk is unanchored and this is
	// skipped, leaving the pre-existing s.clockOffsetS fallback (0, or
	// whatever onSchedule last set) untouched.
	if s.utclk.Anchored() {
		out.clockOffsetS = s.utclk.ServerNow() - out.now.Unix()
	}
	for name, d := range s.devices {
		out.devices[name] = *d
	}
	if s.csipControl != nil {
		cc := *s.csipControl
		out.csipControl = &cc
	}
	if s.lastPlan != nil {
		pl := *s.lastPlan
		out.lastPlan = &pl
	}
	if s.certStatus != nil {
		cs := *s.certStatus
		out.certStatus = &cs
	}
	if s.modeStatus != nil {
		ms := *s.modeStatus
		out.modeStatus = &ms
	}
	if s.hubSettings != nil {
		hs := *s.hubSettings
		out.hubSettings = &hs
	}
	if s.hubSchedule != nil {
		hsc := *s.hubSchedule
		out.hubSchedule = &hsc
	}
	if s.cloudLinkStatus != nil {
		cl := *s.cloudLinkStatus
		out.cloudLinkStatus = &cl
	}
	if s.openADRStatus != nil {
		oa := *s.openADRStatus
		out.openADRStatus = &oa
	}
	if s.scanResult != nil {
		sr := *s.scanResult
		out.scanResult = &sr
	}
	if s.ocppPending != nil {
		op := *s.ocppPending
		out.ocppPending = &op
	}
	if len(s.scanStatusRing) > 0 {
		out.scanStatusRing = make([]bus.ScanStatus, len(s.scanStatusRing))
		copy(out.scanStatusRing, s.scanStatusRing)
	}
	// Stable order: sort by stationID, then connector.
	keys := make([]string, 0, len(s.evses))
	for k := range s.evses {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out.evses = append(out.evses, *s.evses[k])
	}
	return out
}

// Staleness windows for surfacing a frozen/silent source in /status (INV-STALE /
// INV-EVBLIND). These detect what arrival-time freshness alone cannot: a hung
// device still answering with a cached value, or a charger whose session is open
// but whose telemetry has gone silent.
const (
	// meterFrozenAfter: a meter still receiving fresh publishes but whose W has not
	// changed for this long is suspect — but only flagged WHILE the world is
	// demonstrably moving (solarMovingWindow), so a legitimately steady grid is not
	// a false alarm. (Load is unmeasured, so a pure energy-balance check cannot tell
	// frozen from a real load swing; this cross-sensor gate is the safe compromise.)
	meterFrozenAfter  = 18 * time.Second
	solarMovingWindow = 15 * time.Second
	// activeEvseStaleAfter: an EVSE with an active session whose state has not
	// updated for this long has gone silent (MeterValues stopped) — the hub is
	// blind to that charger even though the session reads "active".
	activeEvseStaleAfter = 30 * time.Second
)

// staleMeters returns the names of meter sources whose reading appears frozen:
// fresh arrivals (the publisher is alive) but a W unchanged for meterFrozenAfter
// while some inverter's W changed within solarMovingWindow.
func (s snapshot) staleMeters() []string {
	worldMoving := false
	for _, d := range s.devices {
		if d.Role == "inverter" && !d.WChangedAt.IsZero() && s.now.Sub(d.WChangedAt) < solarMovingWindow {
			worldMoving = true
			break
		}
	}
	if !worldMoving {
		return nil
	}
	var out []string
	for name, d := range s.devices {
		if d.Role != "meter" || d.W == nil {
			continue
		}
		freshArrival := !d.UpdatedAt.IsZero() && s.now.Sub(d.UpdatedAt) <= s.staleAfter
		valueFrozen := !d.WChangedAt.IsZero() && s.now.Sub(d.WChangedAt) > meterFrozenAfter
		if freshArrival && valueFrozen {
			out = append(out, name)
		}
	}
	return out
}

// stale reports whether an EVSE with an active session has gone silent (its
// MeterValues/Updated stopped, so the published state stopped refreshing).
func (e evseSnap) stale(now time.Time) bool {
	return e.State.SessionActive && !e.UpdatedAt.IsZero() && now.Sub(e.UpdatedAt) > activeEvseStaleAfter
}

func evseKey(stationID string, connectorID int) string {
	return stationID + ":" + itoa(connectorID)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
