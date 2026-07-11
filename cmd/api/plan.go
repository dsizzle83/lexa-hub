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
//	  solar_forecast:[{t, solar_W}],
//	  battery_plan:[{t, setpoint_W, soc_pct}],
//	  ev_plan:{<station_id>:[{t, power_W}]} }
type planResp struct {
	GeneratedAt   string                    `json:"generated_at"` // RFC3339
	HorizonH      int                       `json:"horizon_h"`
	SlotMinutes   int                       `json:"slot_minutes"`
	SolarForecast []solarPoint              `json:"solar_forecast"`
	BatteryPlan   []batteryPoint            `json:"battery_plan"`
	EVPlan        map[string][]evPowerPoint `json:"ev_plan"`
}

type solarPoint struct {
	T      string  `json:"t"`       // RFC3339 slot start
	SolarW float64 `json:"solar_W"` // + generation
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
		GeneratedAt: time.Unix(s.GeneratedAt, 0).UTC().Format(time.RFC3339),
		HorizonH:    s.HorizonH,
		SlotMinutes: s.SlotMinutes,
		// Non-nil empty slices so the JSON always carries the arrays (the app's
		// chart code iterates them without a null check).
		SolarForecast: []solarPoint{},
		BatteryPlan:   []batteryPoint{},
		EVPlan:        map[string][]evPowerPoint{},
	}

	for i, v := range s.SolarForecastW {
		resp.SolarForecast = append(resp.SolarForecast, solarPoint{T: tAt(i), SolarW: v})
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

	return resp
}
