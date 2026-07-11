package bus

import (
	"encoding/json"
	"math"
	"strings"
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

// TestHubSchedule_EconomicsRoundTrip pins the wire shape of the PR-C plan-
// economics fields (currency/import/delivery/export price series, grid_w,
// marginal_cost, total_cost, fixed_daily_charge) added alongside the
// original three per-slot series — same index-aligned 5-min grid.
func TestHubSchedule_EconomicsRoundTrip(t *testing.T) {
	in := HubSchedule{
		Envelope:         Envelope{V: HubScheduleV},
		GeneratedAt:      1_752_000_000,
		WindowStart:      1_752_000_000,
		SlotMinutes:      5,
		HorizonH:         24,
		Currency:         "USD",
		ImportPriceKwh:   []float64{0.30, 0.42},
		DeliveryPriceKwh: []float64{0.10, 0.10},
		ExportPriceKwh:   []float64{0.05, 0.05},
		GridW:            []float64{1200, -800},
		MarginalCost:     []float64{0.36, -0.336},
		TotalCost:        4.87,
		FixedDailyCharge: 0.75,
		Ts:               1_752_000_100,
	}

	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	for _, k := range []string{
		"currency", "import_price_kwh", "delivery_price_kwh", "export_price_kwh",
		"grid_w", "marginal_cost", "total_cost", "fixed_daily_charge",
	} {
		if _, ok := raw[k]; !ok {
			t.Errorf("wire missing top-level key %q; got %s", k, b)
		}
	}

	var out HubSchedule
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Currency != "USD" {
		t.Errorf("currency round-trip = %q, want USD", out.Currency)
	}
	if len(out.ImportPriceKwh) != 2 || out.ImportPriceKwh[1] != 0.42 {
		t.Errorf("import_price_kwh round-trip = %v", out.ImportPriceKwh)
	}
	if len(out.DeliveryPriceKwh) != 2 || out.DeliveryPriceKwh[0] != 0.10 {
		t.Errorf("delivery_price_kwh round-trip = %v", out.DeliveryPriceKwh)
	}
	if len(out.ExportPriceKwh) != 2 || out.ExportPriceKwh[0] != 0.05 {
		t.Errorf("export_price_kwh round-trip = %v", out.ExportPriceKwh)
	}
	if len(out.GridW) != 2 || out.GridW[1] != -800 {
		t.Errorf("grid_w round-trip = %v", out.GridW)
	}
	if len(out.MarginalCost) != 2 || out.MarginalCost[1] != -0.336 {
		t.Errorf("marginal_cost round-trip = %v", out.MarginalCost)
	}
	if out.TotalCost != 4.87 {
		t.Errorf("total_cost round-trip = %v, want 4.87", out.TotalCost)
	}
	if out.FixedDailyCharge != 0.75 {
		t.Errorf("fixed_daily_charge round-trip = %v, want 0.75", out.FixedDailyCharge)
	}
}

// TestHubSchedule_EconomicsFieldsOmittedWhenZero verifies the additive PR-C
// economics fields vanish from the wire when left at their zero value (an
// unpopulated HubSchedule, i.e. from a pre-PR-C publisher's code path, must
// serialize identically to before this change).
func TestHubSchedule_EconomicsFieldsOmittedWhenZero(t *testing.T) {
	b, err := json.Marshal(HubSchedule{Envelope: Envelope{V: HubScheduleV}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{
		`"currency"`, `"import_price_kwh"`, `"delivery_price_kwh"`, `"export_price_kwh"`,
		`"grid_w"`, `"marginal_cost"`, `"total_cost"`, `"fixed_daily_charge"`,
	} {
		if strings.Contains(string(b), key) {
			t.Errorf("zero-value economics field %s must be omitted, got %s", key, b)
		}
	}
}

// TestHubSchedule_EconomicsFinite extends TestHubSchedule_Finite to the PR-C
// series/scalars: a NaN/Inf anywhere in ImportPriceKwh/DeliveryPriceKwh/
// ExportPriceKwh/GridW/MarginalCost, or a non-finite TotalCost/
// FixedDailyCharge, must be rejected the same way the original three series
// already are.
func TestHubSchedule_EconomicsFinite(t *testing.T) {
	ok := HubSchedule{
		ImportPriceKwh:   []float64{0.30, 0.42},
		DeliveryPriceKwh: []float64{0.10},
		ExportPriceKwh:   []float64{0.05},
		GridW:            []float64{1200, -800},
		MarginalCost:     []float64{0.36},
		TotalCost:        4.87,
		FixedDailyCharge: 0.75,
	}
	if err := ok.Finite(); err != nil {
		t.Errorf("Finite() on valid economics = %v, want nil", err)
	}

	cases := map[string]HubSchedule{
		"import":     {ImportPriceKwh: []float64{math.NaN()}},
		"delivery":   {DeliveryPriceKwh: []float64{math.Inf(1)}},
		"export":     {ExportPriceKwh: []float64{math.Inf(-1)}},
		"grid":       {GridW: []float64{math.NaN()}},
		"marginal":   {MarginalCost: []float64{math.Inf(1)}},
		"total":      {TotalCost: math.NaN()},
		"fixeddaily": {FixedDailyCharge: math.Inf(1)},
	}
	for name, bad := range cases {
		if err := bad.Finite(); err == nil {
			t.Errorf("Finite() on non-finite %s = nil, want error", name)
		}
	}
}
