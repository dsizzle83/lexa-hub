package bus

import (
	"encoding/json"
	"math"
	"testing"
)

// TestHubSchedule_RoundTrip pins the wire shape lexa-api's GET /plan projection
// consumes: generated_at/window_start/slot_minutes/horizon_h plus the three
// per-slot series (solar_forecast_w, battery_setpoint_w + battery_soc_pct,
// ev_plan_w keyed by station).
func TestHubSchedule_RoundTrip(t *testing.T) {
	soc := f64(52.5)
	in := HubSchedule{
		Envelope:         Envelope{V: HubScheduleV},
		GeneratedAt:      1_752_000_000,
		WindowStart:      1_752_000_000,
		SlotMinutes:      5,
		HorizonH:         24,
		SolarForecastW:   []float64{0, 1500, 3200},
		BatterySetpointW: []float64{-1000, 0, 2000},
		BatterySocPct:    []*float64{soc, nil, f64(60)},
		EVPlanW:          map[string][]float64{"evse-001": {0, -7200, -7200}},
		Ts:               1_752_000_100,
	}

	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Exact top-level wire keys.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	for _, k := range []string{
		"v", "generated_at", "window_start", "slot_minutes", "horizon_h",
		"solar_forecast_w", "battery_setpoint_w", "battery_soc_pct", "ev_plan_w", "ts",
	} {
		if _, ok := raw[k]; !ok {
			t.Errorf("wire missing top-level key %q; got %s", k, b)
		}
	}

	// A nil SOC entry serializes as JSON null (the app tells "unknown" from 0%).
	var socRaw []json.RawMessage
	_ = json.Unmarshal(raw["battery_soc_pct"], &socRaw)
	if len(socRaw) != 3 || string(socRaw[1]) != "null" {
		t.Errorf("battery_soc_pct = %s, want [52.5,null,60]", raw["battery_soc_pct"])
	}

	var out HubSchedule
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.V != HubScheduleV || out.SlotMinutes != 5 || out.HorizonH != 24 {
		t.Errorf("round-trip header mismatch: %+v", out)
	}
	if len(out.SolarForecastW) != 3 || out.SolarForecastW[2] != 3200 {
		t.Errorf("solar_forecast_w round-trip = %v", out.SolarForecastW)
	}
	if out.BatterySocPct[1] != nil || out.BatterySocPct[0] == nil || *out.BatterySocPct[0] != 52.5 {
		t.Errorf("battery_soc_pct round-trip = %v", out.BatterySocPct)
	}
	ev, ok := out.EVPlanW["evse-001"]
	if !ok || len(ev) != 3 || ev[1] != -7200 {
		t.Errorf("ev_plan_w round-trip = %v", out.EVPlanW)
	}
}

// TestHubSchedule_Finite verifies the decode-side non-finite guard on every
// series (defense in depth for a value a lax decoder might smuggle in).
func TestHubSchedule_Finite(t *testing.T) {
	ok := HubSchedule{
		SolarForecastW:   []float64{0, 1500},
		BatterySetpointW: []float64{-1000, 500},
		BatterySocPct:    []*float64{f64(50), nil},
		EVPlanW:          map[string][]float64{"evse-001": {0, -7200}},
	}
	if err := ok.Finite(); err != nil {
		t.Errorf("Finite() on valid schedule = %v, want nil", err)
	}

	cases := map[string]HubSchedule{
		"solar":   {SolarForecastW: []float64{math.NaN()}},
		"battery": {BatterySetpointW: []float64{math.Inf(1)}},
		"soc":     {BatterySocPct: []*float64{f64(math.NaN())}},
		"ev":      {EVPlanW: map[string][]float64{"s": {math.Inf(-1)}}},
	}
	for name, bad := range cases {
		if err := bad.Finite(); err == nil {
			t.Errorf("Finite() on non-finite %s = nil, want error", name)
		}
	}
}
