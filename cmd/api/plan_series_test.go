package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lexa-hub/internal/bus"
)

// appPlan mirrors the app-map target shape for GET /plan (lexa-app
// docs/HUB_API.md app-map): the exact keys lexa_core's plan models consume.
type appPlan struct {
	GeneratedAt   string `json:"generated_at"`
	HorizonH      int    `json:"horizon_h"`
	SlotMinutes   int    `json:"slot_minutes"`
	SolarForecast []struct {
		T      string  `json:"t"`
		SolarW float64 `json:"solar_W"`
	} `json:"solar_forecast"`
	LoadForecast []struct {
		T     string  `json:"t"`
		LoadW float64 `json:"load_W"`
	} `json:"load_forecast"`
	BatteryPlan []struct {
		T         string   `json:"t"`
		SetpointW float64  `json:"setpoint_W"`
		SocPct    *float64 `json:"soc_pct"`
	} `json:"battery_plan"`
	EVPlan map[string][]struct {
		T      string  `json:"t"`
		PowerW float64 `json:"power_W"`
	} `json:"ev_plan"`
}

// TestPlanHandler_503BeforeSchedule: GET /plan is 503 until the first
// HubSchedule arrives (like /mode).
func TestPlanHandler_503BeforeSchedule(t *testing.T) {
	store := newStateStore(nil, time.Minute)
	h := planHandler(store)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/plan", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 before any schedule", rec.Code)
	}
}

// TestPlanHandler_AppShape drives GET /plan end to end after a HubSchedule and
// pins the exact keys + RFC3339 timestamps + sign conventions the app reads.
func TestPlanHandler_AppShape(t *testing.T) {
	ws := int64(1_752_000_000) // 5-min aligned
	soc0 := 50.0
	store := newStateStore(nil, time.Minute)
	store.onHubSchedule(bus.TopicHubSchedule, bus.HubSchedule{
		Envelope:         bus.Envelope{V: bus.HubScheduleV},
		GeneratedAt:      ws,
		WindowStart:      ws,
		SlotMinutes:      5,
		HorizonH:         24,
		SolarForecastW:   []float64{0, 1500},
		LoadForecastW:    []float64{0, 800},
		BatterySetpointW: []float64{-1000, 2000},
		BatterySocPct:    []*float64{&soc0, nil},
		EVPlanW:          map[string][]float64{"evse-001": {0, -7200}},
		Ts:               ws + 10,
	})

	h := planHandler(store)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/plan", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp appPlan
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode /plan with app shape: %v", err)
	}

	wantGen := time.Unix(ws, 0).UTC().Format(time.RFC3339)
	if resp.GeneratedAt != wantGen || resp.HorizonH != 24 || resp.SlotMinutes != 5 {
		t.Errorf("header = {%s,%d,%d}, want {%s,24,5}", resp.GeneratedAt, resp.HorizonH, resp.SlotMinutes, wantGen)
	}

	// solar_forecast: RFC3339 t per slot, solar_W verbatim.
	if len(resp.SolarForecast) != 2 {
		t.Fatalf("solar_forecast len = %d, want 2", len(resp.SolarForecast))
	}
	if resp.SolarForecast[0].T != time.Unix(ws, 0).UTC().Format(time.RFC3339) {
		t.Errorf("solar_forecast[0].t = %s, want %s", resp.SolarForecast[0].T, wantGen)
	}
	if resp.SolarForecast[1].T != time.Unix(ws+300, 0).UTC().Format(time.RFC3339) {
		t.Errorf("solar_forecast[1].t = %s, want slot1 = ws+300", resp.SolarForecast[1].T)
	}
	if resp.SolarForecast[1].SolarW != 1500 {
		t.Errorf("solar_forecast[1].solar_W = %v, want 1500", resp.SolarForecast[1].SolarW)
	}

	// load_forecast: RFC3339 t per slot, load_W verbatim (+consumption magnitude).
	if len(resp.LoadForecast) != 2 {
		t.Fatalf("load_forecast len = %d, want 2", len(resp.LoadForecast))
	}
	if resp.LoadForecast[1].LoadW != 800 {
		t.Errorf("load_forecast[1].load_W = %v, want 800", resp.LoadForecast[1].LoadW)
	}
	if resp.LoadForecast[0].T != time.Unix(ws, 0).UTC().Format(time.RFC3339) {
		t.Errorf("load_forecast[0].t = %s, want slot0", resp.LoadForecast[0].T)
	}

	// battery_plan: setpoint verbatim (+dis/−chg), soc_pct nullable.
	if len(resp.BatteryPlan) != 2 {
		t.Fatalf("battery_plan len = %d, want 2", len(resp.BatteryPlan))
	}
	if resp.BatteryPlan[0].SetpointW != -1000 {
		t.Errorf("battery_plan[0].setpoint_W = %v, want -1000 (charge)", resp.BatteryPlan[0].SetpointW)
	}
	if resp.BatteryPlan[0].SocPct == nil || *resp.BatteryPlan[0].SocPct != 50 {
		t.Errorf("battery_plan[0].soc_pct = %v, want 50", resp.BatteryPlan[0].SocPct)
	}
	if resp.BatteryPlan[1].SocPct != nil {
		t.Errorf("battery_plan[1].soc_pct = %v, want null (unknown SOC)", *resp.BatteryPlan[1].SocPct)
	}

	// ev_plan: keyed by station, power_W negative (charge = load).
	ev, ok := resp.EVPlan["evse-001"]
	if !ok || len(ev) != 2 {
		t.Fatalf("ev_plan[evse-001] = %v, want 2 points", resp.EVPlan)
	}
	if ev[1].PowerW != -7200 {
		t.Errorf("ev_plan[evse-001][1].power_W = %v, want -7200", ev[1].PowerW)
	}
	if ev[0].T != time.Unix(ws, 0).UTC().Format(time.RFC3339) {
		t.Errorf("ev_plan[evse-001][0].t = %s, want slot0", ev[0].T)
	}
}
