package main

import (
	"encoding/json"
	"math"
	"sync"
	"testing"
	"time"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/orchestrator"
)

// fakeSnapReader is a settable stand-in for the engine's DailyPlanSnapshot
// accessor (the planSnapshotReader interface schedule.go depends on), so these
// tests need no running orchestrator.Engine — mirrors fakeReserveReader.
type fakeSnapReader struct {
	mu   sync.Mutex
	snap orchestrator.PlanSnapshot
}

func (f *fakeSnapReader) DailyPlanSnapshot() orchestrator.PlanSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap
}

func (f *fakeSnapReader) set(s orchestrator.PlanSnapshot) {
	f.mu.Lock()
	f.snap = s
	f.mu.Unlock()
}

// makeSnap builds a full 288-slot PlanSnapshot with a battery + EV, stamped with
// the given build time.
func makeSnap(buildTime time.Time) orchestrator.PlanSnapshot {
	plan := &orchestrator.DailyPlan{
		BuildTime:   buildTime,
		WindowStart: buildTime.Unix(),
		WindowEnd:   buildTime.Unix() + int64(288)*300,
	}
	forecast := make([]float64, 288)
	for i := range plan.Intervals {
		plan.Intervals[i] = orchestrator.PlanInterval{
			Start:         buildTime.Unix() + int64(i)*300,
			BattSetpointW: 100,
			EVMaxCurrentA: 16,
			SocKwh:        5, // 5 kWh of 10 kWh → 50%
		}
		forecast[i] = float64(i) // kW
	}
	return orchestrator.PlanSnapshot{Plan: plan, ForecastKw: forecast, BattCapKwh: 10, EVVoltageV: 240}
}

func lastSchedulePublish(t *testing.T, mc *fakeHubMQTTClient) (bus.HubSchedule, fakeHubPublish) {
	t.Helper()
	for i := len(mc.publishes) - 1; i >= 0; i-- {
		if mc.publishes[i].topic == bus.TopicHubSchedule {
			var hs bus.HubSchedule
			if err := json.Unmarshal(mc.publishes[i].payload, &hs); err != nil {
				t.Fatalf("unmarshal HubSchedule: %v", err)
			}
			return hs, mc.publishes[i]
		}
	}
	t.Fatal("no lexa/hub/schedule message published")
	return bus.HubSchedule{}, fakeHubPublish{}
}

func countSchedulePublishes(mc *fakeHubMQTTClient) int {
	n := 0
	for _, p := range mc.publishes {
		if p.topic == bus.TopicHubSchedule {
			n++
		}
	}
	return n
}

// TestSchedulePublisher_SeedNoPlan: refresh() before any plan (Plan == nil) is a
// no-op — nothing to publish yet.
func TestSchedulePublisher_SeedNoPlan(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	sp := newSchedulePublisher(mc, &fakeSnapReader{}, []string{"evse1"})
	sp.refresh()
	if n := countSchedulePublishes(mc); n != 0 {
		t.Errorf("published %d schedule messages before any plan, want 0", n)
	}
}

// TestSchedulePublisher_PublishesOnPlan pins the projected shape + sign
// conventions: solar kW→W (+gen), battery setpoint (+dis/−chg), SOC pct, EV
// power negated (charge = load), keyed by the configured station.
func TestSchedulePublisher_PublishesOnPlan(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	buildTime := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	r := &fakeSnapReader{}
	r.set(makeSnap(buildTime))
	sp := newSchedulePublisher(mc, r, []string{"evse1"})

	sp.refresh()

	hs, pub := lastSchedulePublish(t, mc)
	if !pub.retained {
		t.Error("schedule must be published RETAINED (lexa-api re-seed)")
	}
	if hs.V != bus.HubScheduleV {
		t.Errorf("envelope V = %d, want %d", hs.V, bus.HubScheduleV)
	}
	if hs.GeneratedAt != buildTime.Unix() {
		t.Errorf("generated_at = %d, want %d (plan build time)", hs.GeneratedAt, buildTime.Unix())
	}
	if hs.WindowStart != buildTime.Unix() || hs.SlotMinutes != 5 || hs.HorizonH != 24 {
		t.Errorf("grid header = window=%d slot=%d horizon=%d, want %d/5/24", hs.WindowStart, hs.SlotMinutes, hs.HorizonH, buildTime.Unix())
	}
	if len(hs.SolarForecastW) != 288 || hs.SolarForecastW[1] != 1000 { // 1 kW → 1000 W
		t.Errorf("solar_forecast_w[1] = %v (len %d), want 1000", hs.SolarForecastW[1], len(hs.SolarForecastW))
	}
	if len(hs.BatterySetpointW) != 288 || hs.BatterySetpointW[0] != 100 {
		t.Errorf("battery_setpoint_w[0] = %v, want 100", hs.BatterySetpointW[0])
	}
	if len(hs.BatterySocPct) != 288 || hs.BatterySocPct[0] == nil || *hs.BatterySocPct[0] != 50 {
		t.Errorf("battery_soc_pct[0] = %v, want 50 (5/10 kWh)", hs.BatterySocPct[0])
	}
	ev, ok := hs.EVPlanW["evse1"]
	if !ok || len(ev) != 288 || ev[0] != -3840 { // 16 A × 240 V, negated (charge = load)
		t.Errorf("ev_plan_w[evse1][0] = %v, want -3840", ev)
	}
}

// TestSchedulePublisher_Economics pins the PR-D plan-economics projection: a
// snapshot carrying resolved per-slot prices + a plan whose intervals carry
// ExpectedGridW/MarginalCost produces a HubSchedule with the price arrays copied
// through, GridW/MarginalCost projected from the intervals, and the horizon
// scalars (TotalCost/FixedDailyCharge/Currency). Non-finite inputs are forced to
// 0 so nothing non-finite reaches the wire.
func TestSchedulePublisher_Economics(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	buildTime := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	snap := makeSnap(buildTime)

	snap.Currency = "USD"
	snap.FixedDailyCharge = 0.35
	snap.Plan.TotalCost = 4.20
	imp := make([]float64, 288)
	del := make([]float64, 288)
	exp := make([]float64, 288)
	for i := range imp {
		imp[i] = 0.30
		del[i] = 0.05
		exp[i] = 0.10
		snap.Plan.Intervals[i].ExpectedGridW = 1000
		snap.Plan.Intervals[i].MarginalCost = 0.025
	}
	// Non-finite inputs must be scrubbed to 0 on the wire.
	imp[5] = math.NaN()
	snap.Plan.Intervals[6].ExpectedGridW = math.Inf(1)
	snap.ImportPriceKwh = imp
	snap.DeliveryPriceKwh = del
	snap.ExportPriceKwh = exp

	r := &fakeSnapReader{}
	r.set(snap)
	sp := newSchedulePublisher(mc, r, []string{"evse1"})
	sp.refresh()

	hs, _ := lastSchedulePublish(t, mc)
	if hs.Currency != "USD" {
		t.Errorf("currency = %q, want USD", hs.Currency)
	}
	if len(hs.ImportPriceKwh) != 288 || hs.ImportPriceKwh[0] != 0.30 {
		t.Errorf("import_price_kwh[0] = %v (len %d), want 0.30", hs.ImportPriceKwh[0], len(hs.ImportPriceKwh))
	}
	if hs.ImportPriceKwh[5] != 0 {
		t.Errorf("import_price_kwh[5] = %v, want 0 (NaN scrubbed)", hs.ImportPriceKwh[5])
	}
	if len(hs.DeliveryPriceKwh) != 288 || hs.DeliveryPriceKwh[0] != 0.05 {
		t.Errorf("delivery_price_kwh[0] = %v, want 0.05", hs.DeliveryPriceKwh[0])
	}
	if len(hs.ExportPriceKwh) != 288 || hs.ExportPriceKwh[0] != 0.10 {
		t.Errorf("export_price_kwh[0] = %v, want 0.10", hs.ExportPriceKwh[0])
	}
	if len(hs.GridW) != 288 || hs.GridW[0] != 1000 {
		t.Errorf("grid_w[0] = %v (len %d), want 1000", hs.GridW[0], len(hs.GridW))
	}
	if hs.GridW[6] != 0 {
		t.Errorf("grid_w[6] = %v, want 0 (Inf scrubbed)", hs.GridW[6])
	}
	if len(hs.MarginalCost) != 288 || hs.MarginalCost[0] != 0.025 {
		t.Errorf("marginal_cost[0] = %v, want 0.025", hs.MarginalCost[0])
	}
	if hs.TotalCost != 4.20 {
		t.Errorf("total_cost = %v, want 4.20", hs.TotalCost)
	}
	if hs.FixedDailyCharge != 0.35 {
		t.Errorf("fixed_daily_charge = %v, want 0.35", hs.FixedDailyCharge)
	}
	if err := hs.Finite(); err != nil {
		t.Errorf("HubSchedule.Finite() = %v, want nil", err)
	}
}

// TestSchedulePublisher_NoEconomics: a plan with no economics populated (nil
// price arrays) omits the price arrays (nil) but still emits GridW/MarginalCost
// projected from the zero-valued intervals — the fields are plan-driven, not
// economics-driven.
func TestSchedulePublisher_NoEconomics(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	r := &fakeSnapReader{}
	r.set(makeSnap(time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC))) // no economics set
	sp := newSchedulePublisher(mc, r, []string{"evse1"})
	sp.refresh()

	hs, _ := lastSchedulePublish(t, mc)
	if hs.ImportPriceKwh != nil || hs.DeliveryPriceKwh != nil || hs.ExportPriceKwh != nil {
		t.Errorf("price arrays should be nil when the snapshot has none, got imp=%v del=%v exp=%v",
			hs.ImportPriceKwh, hs.DeliveryPriceKwh, hs.ExportPriceKwh)
	}
	if len(hs.GridW) != 288 || len(hs.MarginalCost) != 288 {
		t.Errorf("grid_w/marginal_cost must still be 288-slot arrays, got %d/%d", len(hs.GridW), len(hs.MarginalCost))
	}
}

// TestSchedulePublisher_DedupeOnBuildTime: the same plan (unchanged build time)
// is published once; a new plan republishes.
func TestSchedulePublisher_DedupeOnBuildTime(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	t0 := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	r := &fakeSnapReader{}
	r.set(makeSnap(t0))
	sp := newSchedulePublisher(mc, r, []string{"evse1"})

	sp.refresh()
	sp.refresh() // same build time → deduped
	if n := countSchedulePublishes(mc); n != 1 {
		t.Fatalf("published %d times for one plan, want 1 (dedupe on build time)", n)
	}

	// A new plan (later build time) republishes.
	r.set(makeSnap(t0.Add(15 * time.Minute)))
	sp.refresh()
	if n := countSchedulePublishes(mc); n != 2 {
		t.Errorf("published %d times after a new plan, want 2", n)
	}
}

// TestSchedulePublisher_NoStation: with no configured station, the ev_plan is
// omitted (nothing to key it to) but the rest of the series still publish.
func TestSchedulePublisher_NoStation(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	r := &fakeSnapReader{}
	r.set(makeSnap(time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)))
	sp := newSchedulePublisher(mc, r, nil) // no stations

	sp.refresh()
	hs, _ := lastSchedulePublish(t, mc)
	if len(hs.EVPlanW) != 0 {
		t.Errorf("ev_plan_w = %v, want empty with no configured station", hs.EVPlanW)
	}
	if len(hs.SolarForecastW) != 288 {
		t.Errorf("solar_forecast_w len = %d, want 288 even without a station", len(hs.SolarForecastW))
	}
}
