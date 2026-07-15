package main

// plan.go implements GET /plan (GAP-7): a projection of the hub's retained
// lexa/hub/schedule (bus.HubSchedule) into the app's forecast/plan-chart shape.
// The hub does the planning and publishes compact per-slot arrays on one uniform
// 5-minute grid; this handler renders each slot's Unix start as RFC3339 "t" and
// splits the arrays into the app's {t, ...} entry lists. Powers stay in the
// hub's house sign convention (+ discharge/generation, − charge/load).

import (
	"net/http"
	"time"

	"lexa-hub/internal/bus"
)

// planResp is GET /plan's JSON shape on success — the app-map target:
//
//	{ generated_at, horizon_h, slot_minutes,
//	  currency, total_cost, fixed_daily_charge,
//	  solar_forecast:[{t, solar_W}],
//	  load_forecast:[{t, load_W}],
//	  battery_plan:[{t, setpoint_W, soc_pct}],
//	  ev_plan:{<station_id>:[{t, power_W}]},
//	  price_forecast:[{t, import_per_kwh, delivery_per_kwh, export_per_kwh}],
//	  cost_plan:[{t, grid_W, marginal_cost}] }
//
// The economics fields (currency/total_cost/fixed_daily_charge and the
// price_forecast/cost_plan series) are ADDITIVE (PR-E): they surface the $
// economics HubSchedule now carries. They are emitted only when the source
// HubSchedule fields are populated — an unpopulated schedule yields the same
// wire shape as before this PR (empty Currency/zero totals omit, the two new
// series stay nil).
type planResp struct {
	GeneratedAt      string                    `json:"generated_at"` // RFC3339
	HorizonH         int                       `json:"horizon_h"`
	SlotMinutes      int                       `json:"slot_minutes"`
	Currency         string                    `json:"currency,omitempty"`
	TotalCost        float64                   `json:"total_cost,omitempty"`
	FixedDailyCharge float64                   `json:"fixed_daily_charge,omitempty"`
	SolarForecast    []solarPoint              `json:"solar_forecast"`
	LoadForecast     []loadPoint               `json:"load_forecast"`
	BatteryPlan      []batteryPoint            `json:"battery_plan"`
	EVPlan           map[string][]evPowerPoint `json:"ev_plan"`
	PriceForecast    []pricePoint              `json:"price_forecast"`
	CostPlan         []costPoint               `json:"cost_plan"`
}

type solarPoint struct {
	T      string  `json:"t"`       // RFC3339 slot start
	SolarW float64 `json:"solar_W"` // + generation
}

// loadPoint is one slot of the load_forecast series: the home-load demand the
// plan served, a POSITIVE consumption magnitude (the generation dual of
// solarPoint), not a signed flow.
type loadPoint struct {
	T     string  `json:"t"`      // RFC3339 slot start
	LoadW float64 `json:"load_W"` // + home consumption (demand magnitude)
}

type batteryPoint struct {
	T         string   `json:"t"`          // RFC3339 slot start
	SetpointW float64  `json:"setpoint_W"` // + discharge, − charge
	SocPct    *float64 `json:"soc_pct"`    // null when the planned SOC is unknown
}

type evPowerPoint struct {
	T      string  `json:"t"`       // RFC3339 slot start
	PowerW float64 `json:"power_W"` // − charge/load
}

// pricePoint is one slot of the price_forecast series: the all-in import price
// (supply + delivery), the delivery component alone (so the app can annotate
// how much of the import price is delivery vs. supply), and the export/feed-in
// credit — all $/kWh at that slot.
type pricePoint struct {
	T              string  `json:"t"`                // RFC3339 slot start
	ImportPerKwh   float64 `json:"import_per_kwh"`   // all-in supply + delivery
	DeliveryPerKwh float64 `json:"delivery_per_kwh"` // delivery component alone
	ExportPerKwh   float64 `json:"export_per_kwh"`   // feed-in credit
}

// costPoint is one slot of the cost_plan series: the planned grid flow (house
// sign convention, + import / − export) and the net $ cost that flow implies at
// the slot's price (negative = earning, e.g. net export at a positive credit).
type costPoint struct {
	T            string  `json:"t"`             // RFC3339 slot start
	GridW        float64 `json:"grid_W"`        // + import, − export
	MarginalCost float64 `json:"marginal_cost"` // net $ at this slot
}

// planHandler serves GET /plan: 503 {"error":"unknown"} until the first
// HubSchedule has arrived (the retained topic means this is normally the case
// within one broker round trip of either side's startup), 200 with the projected
// series otherwise. Mirrors modeHandler's staged shape.
func planHandler(store *stateStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.WriteHeader(http.StatusNoContent)
			return
		}

		sched := store.snapshot().hubSchedule
		if sched == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unknown"})
			return
		}
		writeJSON(w, http.StatusOK, buildPlan(*sched))
	}
}

// buildPlan projects a bus.HubSchedule into the app's /plan shape. The slot
// grid is uniform: slot i starts at WindowStart + i*SlotMinutes*60. Kept a pure
// function (no store/HTTP) so plan_test.go can pin the exact wire shape.
func buildPlan(s bus.HubSchedule) planResp {
	slotSec := int64(s.SlotMinutes) * 60
	// tAt renders slot i's RFC3339 UTC start. Guard against a zero SlotMinutes
	// on some malformed/legacy message so every entry doesn't collapse to t0.
	tAt := func(i int) string {
		return time.Unix(s.WindowStart+int64(i)*slotSec, 0).UTC().Format(time.RFC3339)
	}

	resp := planResp{
		GeneratedAt:      time.Unix(s.GeneratedAt, 0).UTC().Format(time.RFC3339),
		HorizonH:         s.HorizonH,
		SlotMinutes:      s.SlotMinutes,
		Currency:         s.Currency,
		TotalCost:        s.TotalCost,
		FixedDailyCharge: s.FixedDailyCharge,
		// Non-nil empty slices so the JSON always carries the arrays (the app's
		// chart code iterates them without a null check).
		SolarForecast: []solarPoint{},
		LoadForecast:  []loadPoint{},
		BatteryPlan:   []batteryPoint{},
		EVPlan:        map[string][]evPowerPoint{},
	}

	for i, v := range s.SolarForecastW {
		resp.SolarForecast = append(resp.SolarForecast, solarPoint{T: tAt(i), SolarW: v})
	}

	for i, v := range s.LoadForecastW {
		resp.LoadForecast = append(resp.LoadForecast, loadPoint{T: tAt(i), LoadW: v})
	}

	// battery_plan pairs setpoint with the parallel SOC slice; a shorter/absent
	// SOC slice yields null soc_pct for the uncovered slots.
	for i, v := range s.BatterySetpointW {
		bp := batteryPoint{T: tAt(i), SetpointW: v}
		if i < len(s.BatterySocPct) {
			bp.SocPct = s.BatterySocPct[i]
		}
		resp.BatteryPlan = append(resp.BatteryPlan, bp)
	}

	for station, series := range s.EVPlanW {
		pts := make([]evPowerPoint, 0, len(series))
		for i, v := range series {
			pts = append(pts, evPowerPoint{T: tAt(i), PowerW: v})
		}
		resp.EVPlan[station] = pts
	}

	// price_forecast is driven by ImportPriceKwh; the delivery/export components
	// are index-guarded against a shorter/absent slice (same rule as the
	// battery setpoint/SOC pairing above). Emitted only when the economics are
	// populated — an unpopulated schedule leaves the series nil (JSON null),
	// signalling "not planned" rather than an empty forecast.
	if len(s.ImportPriceKwh) > 0 {
		resp.PriceForecast = []pricePoint{}
		for i, imp := range s.ImportPriceKwh {
			pp := pricePoint{T: tAt(i), ImportPerKwh: imp}
			if i < len(s.DeliveryPriceKwh) {
				pp.DeliveryPerKwh = s.DeliveryPriceKwh[i]
			}
			if i < len(s.ExportPriceKwh) {
				pp.ExportPerKwh = s.ExportPriceKwh[i]
			}
			resp.PriceForecast = append(resp.PriceForecast, pp)
		}
	}

	// cost_plan is driven by GridW; marginal_cost is index-guarded the same way.
	if len(s.GridW) > 0 {
		resp.CostPlan = []costPoint{}
		for i, g := range s.GridW {
			cp := costPoint{T: tAt(i), GridW: g}
			if i < len(s.MarginalCost) {
				cp.MarginalCost = s.MarginalCost[i]
			}
			resp.CostPlan = append(resp.CostPlan, cp)
		}
	}

	return resp
}
