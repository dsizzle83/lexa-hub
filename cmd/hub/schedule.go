package main

// schedule.go publishes the retained lexa/hub/schedule document (bus.HubSchedule,
// GAP-7): the hub's most recent 24-hour plan projected into three per-slot series
// — the solar forecast the optimizer used, the planned battery setpoint + SOC,
// and the EV charge plan — so lexa-api can serve GET /plan and the app's
// forecast/plan charts have real backing data instead of placeholder cards.
//
// It mirrors settings.go's shape (a small named publisher poked by the plan
// observer) but with a different dedupe key: the plan's own build time. The
// planner re-runs on its own cadence (every replan_interval_s, or on any input
// change), independently of the engine tick that fires the plan observer, so
// republishing on every tick would spam a retained topic with an unchanged
// plan. Deduping on GeneratedAt (the DailyPlan.BuildTime) publishes exactly once
// per distinct plan.

import (
	"log/slog"
	"math"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/mqttutil"
	"lexa-hub/internal/orchestrator"
)

// planSnapshotReader is the engine accessor the publisher needs — an interface,
// not *orchestrator.Engine, so schedule_test.go can substitute a fake without a
// running engine (the same hubEngine/reserveReader split settings.go uses).
// *orchestrator.Engine satisfies it structurally.
type planSnapshotReader interface {
	DailyPlanSnapshot() orchestrator.PlanSnapshot
}

// schedulePublisher owns the retained lexa/hub/schedule document.
type schedulePublisher struct {
	mu sync.Mutex

	mc  mqtt.Client
	eng planSnapshotReader

	// stationID keys the ev_plan series. The daily planner models a single
	// logical EV, so the schedule carries one EV series keyed by the first
	// configured station id (empty ⇒ no station configured ⇒ ev_plan omitted).
	stationID string

	// Dedupe: publish once per distinct plan. lastGeneratedAt is the build time
	// (Unix s) of the plan last PUBLISHED; published guards the first publish.
	published       bool
	lastGeneratedAt int64

	now func() time.Time // seam for tests; time.Now in production
}

// newSchedulePublisher builds the publisher. stationIDs is cfg.Stations' ids
// (in config order); the first is used to key the ev_plan series.
func newSchedulePublisher(mc mqtt.Client, eng planSnapshotReader, stationIDs []string) *schedulePublisher {
	var station string
	if len(stationIDs) > 0 {
		station = stationIDs[0]
	}
	return &schedulePublisher{
		mc:        mc,
		eng:       eng,
		stationID: station,
		now:       time.Now,
	}
}

// slotMinutes / horizonHours describe the planner's fixed 288-slot 5-minute
// grid (orchestrator.planSteps / planStepSec) — surfaced on the wire so the app
// need not hardcode them.
const (
	scheduleSlotMinutes = 5
	scheduleHorizonH    = 24
)

// build projects a PlanSnapshot into a bus.HubSchedule. Returns ok=false before
// the first plan (Plan == nil), when the caller should not publish.
//
// Sign convention (bus house rule, + discharge/gen − charge/load): solar is
// +gen (forecast kW → +W), battery setpoint is + discharge / − charge already,
// and EV charging is a load → its power is NEGATIVE. Every value is forced
// finite (NaN/Inf → 0, or nil for an unknown SOC) so nothing non-finite ever
// reaches the wire.
func (p *schedulePublisher) build(snap orchestrator.PlanSnapshot) (bus.HubSchedule, bool) {
	plan := snap.Plan
	if plan == nil {
		return bus.HubSchedule{}, false
	}
	n := len(plan.Intervals)

	hs := bus.HubSchedule{
		Envelope:    bus.Envelope{V: bus.HubScheduleV},
		GeneratedAt: plan.BuildTime.Unix(),
		WindowStart: plan.WindowStart,
		SlotMinutes: scheduleSlotMinutes,
		HorizonH:    scheduleHorizonH,
		Ts:          p.now().Unix(),
	}

	// Solar forecast (kW → W, +gen). Missing/short forecast steps zero-fill,
	// same rule the planner itself applies to a short SolarForecastKw.
	hs.SolarForecastW = make([]float64, n)
	for i := 0; i < n; i++ {
		var kw float64
		if i < len(snap.ForecastKw) {
			kw = snap.ForecastKw[i]
		}
		hs.SolarForecastW[i] = finiteOrZero(kw) * 1000
	}

	// Load forecast (kW → W, +consumption). The home-load demand the DP planned
	// around — present whenever the snapshot carries it (load is always modelled,
	// unlike battery/EV). Guard the length defensively like solar above.
	if len(snap.LoadKw) > 0 {
		hs.LoadForecastW = make([]float64, n)
		for i := 0; i < n; i++ {
			var kw float64
			if i < len(snap.LoadKw) {
				kw = snap.LoadKw[i]
			}
			hs.LoadForecastW[i] = finiteOrZero(kw) * 1000
		}
	}

	// Battery plan (setpoint W + planned SOC pct) — only when a battery was
	// modelled (BattCapKwh > 0). BattSetpointW is NaN per-slot when there is no
	// battery; here BattCapKwh>0 guarantees real setpoints, but stay defensive.
	if snap.BattCapKwh > 0 {
		hs.BatterySetpointW = make([]float64, n)
		hs.BatterySocPct = make([]*float64, n)
		for i := 0; i < n; i++ {
			iv := plan.Intervals[i]
			hs.BatterySetpointW[i] = finiteOrZero(iv.BattSetpointW)
			if !math.IsNaN(iv.SocKwh) && !math.IsInf(iv.SocKwh, 0) {
				pct := iv.SocKwh / snap.BattCapKwh * 100
				hs.BatterySocPct[i] = &pct
			} // else nil (unknown SOC this slot)
		}
	}

	// EV charge plan (A → W via voltage, − charge/load), keyed by the configured
	// station. Only when an EV was modelled (EVVoltageV>0) AND a station id is
	// available to key it to.
	if snap.EVVoltageV > 0 && p.stationID != "" {
		series := make([]float64, n)
		for i := 0; i < n; i++ {
			a := finiteOrZero(plan.Intervals[i].EVMaxCurrentA)
			series[i] = -(a * snap.EVVoltageV) // charging draws power → negative
		}
		hs.EVPlanW = map[string][]float64{p.stationID: series}
	}

	// Plan economics (PR-C wire fields, PR-D population). Emitted whenever a plan
	// exists — these do NOT depend on a battery/EV being modelled. The per-slot
	// prices are copied straight from the snapshot's resolved arrays (the DP
	// costed against these exact values); GridW/MarginalCost are projected from
	// the plan intervals; TotalCost/FixedDailyCharge are the horizon scalars.
	// Every value is forced finite (finiteOrZero/finiteSlice) so nothing
	// non-finite ever reaches the wire — the same guard SolarForecastW uses.
	hs.Currency = snap.Currency
	hs.ImportPriceKwh = finiteSlice(snap.ImportPriceKwh)
	hs.DeliveryPriceKwh = finiteSlice(snap.DeliveryPriceKwh)
	hs.ExportPriceKwh = finiteSlice(snap.ExportPriceKwh)

	hs.GridW = make([]float64, n)
	hs.MarginalCost = make([]float64, n)
	for i := 0; i < n; i++ {
		hs.GridW[i] = finiteOrZero(plan.Intervals[i].ExpectedGridW)
		hs.MarginalCost[i] = finiteOrZero(plan.Intervals[i].MarginalCost)
	}
	hs.TotalCost = finiteOrZero(plan.TotalCost)
	hs.FixedDailyCharge = finiteOrZero(snap.FixedDailyCharge)

	return hs, true
}

// finiteOrZero maps a NaN/Inf to 0 so no non-finite value ever reaches the wire.
func finiteOrZero(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// finiteSlice copies src, forcing each element finite (NaN/Inf → 0). A nil src
// returns nil so an unpopulated economics field stays absent on the wire
// (omitempty), rather than becoming an empty non-nil slice.
func finiteSlice(src []float64) []float64 {
	if src == nil {
		return nil
	}
	out := make([]float64, len(src))
	for i, v := range src {
		out[i] = finiteOrZero(v)
	}
	return out
}

// refresh reads the current plan snapshot, builds the schedule, and publishes it
// retained — but only when the plan is NEW (dedupe on GeneratedAt) or nothing
// has been published yet. A no-op before the first plan (Plan == nil). It is the
// seam the plan observer pokes every pass, and doubles as the startup seed.
func (p *schedulePublisher) refresh() {
	hs, ok := p.build(p.eng.DailyPlanSnapshot())
	if !ok {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.published && hs.GeneratedAt == p.lastGeneratedAt {
		return
	}
	p.lastGeneratedAt = hs.GeneratedAt
	p.published = true
	if err := mqttutil.PublishJSONRetained(p.mc, bus.TopicHubSchedule, hs); err != nil {
		slog.Warn("lexa-hub: publish hub schedule", "err", err)
	}
}
