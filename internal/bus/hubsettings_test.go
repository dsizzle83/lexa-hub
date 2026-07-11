package bus

import (
	"encoding/json"
	"math"
	"testing"
)

func f64(v float64) *float64 { return &v }

// TestHubSettings_RoundTrip pins the wire shape the app's /status reader
// consumes: reserve{effective_pct,floor_pct,source} + tariff{source,updated_at,
// spec} where spec is exactly TariffSpec's shape.
func TestHubSettings_RoundTrip(t *testing.T) {
	in := HubSettings{
		Envelope: Envelope{V: HubSettingsV},
		Reserve: ReserveSettings{
			EffectivePct: f64(35),
			FloorPct:     f64(20),
			Source:       "app",
		},
		Tariff: TariffSettings{
			Source:    "manual",
			UpdatedAt: 1_752_000_000,
			Spec: &TariffSpec{
				Currency: "USD",
				Periods: []TariffPeriod{
					{Label: "peak", Days: []int{0, 1, 2, 3, 4, 5, 6}, StartHH: 16, EndHH: 21, ImportPerKwh: 0.38, ExportPerKwh: f64(0)},
					{Label: "off-peak", Days: []int{0, 1, 2, 3, 4, 5, 6}, StartHH: 0, EndHH: 16, ImportPerKwh: 0.12},
				},
			},
		},
		Ts: 1_752_000_100,
	}

	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Verify the exact wire keys the app's TariffSpec.fromJson + /status map read.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	for _, k := range []string{"reserve", "tariff", "ts", "v"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("wire missing top-level key %q; got %s", k, b)
		}
	}
	var reserve map[string]json.RawMessage
	_ = json.Unmarshal(raw["reserve"], &reserve)
	for _, k := range []string{"effective_pct", "floor_pct", "source"} {
		if _, ok := reserve[k]; !ok {
			t.Errorf("reserve missing key %q; got %s", k, raw["reserve"])
		}
	}
	var tariff map[string]json.RawMessage
	_ = json.Unmarshal(raw["tariff"], &tariff)
	for _, k := range []string{"source", "updated_at", "spec"} {
		if _, ok := tariff[k]; !ok {
			t.Errorf("tariff missing key %q; got %s", k, raw["tariff"])
		}
	}
	var period map[string]json.RawMessage
	{
		var spec map[string]json.RawMessage
		_ = json.Unmarshal(tariff["spec"], &spec)
		var periods []json.RawMessage
		_ = json.Unmarshal(spec["periods"], &periods)
		_ = json.Unmarshal(periods[0], &period)
	}
	for _, k := range []string{"label", "days", "start_hh", "end_hh", "import_per_kwh", "export_per_kwh"} {
		if _, ok := period[k]; !ok {
			t.Errorf("tariff.spec.periods[0] missing key %q", k)
		}
	}

	var out HubSettings
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.V != HubSettingsV || out.Reserve.Source != "app" || out.Tariff.Source != "manual" {
		t.Errorf("round-trip mismatch: %+v", out)
	}
	if out.Reserve.EffectivePct == nil || *out.Reserve.EffectivePct != 35 {
		t.Errorf("effective_pct round-trip = %v, want 35", out.Reserve.EffectivePct)
	}
	if out.Tariff.Spec == nil || len(out.Tariff.Spec.Periods) != 2 {
		t.Fatalf("tariff spec round-trip lost periods: %+v", out.Tariff.Spec)
	}
	if out.Tariff.Spec.Periods[0].ImportPerKwh != 0.38 {
		t.Errorf("period[0].import_per_kwh = %v, want 0.38", out.Tariff.Spec.Periods[0].ImportPerKwh)
	}
}

// TestHubSettings_NilEffectiveOmitsValue verifies effective_pct serializes as
// JSON null (not omitted, not a bogus 0) before the first plan — the app must
// tell "no plan yet" from "reserve is 0%".
func TestHubSettings_NilEffectivePct(t *testing.T) {
	b, err := json.Marshal(HubSettings{Envelope: Envelope{V: HubSettingsV}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(b, &raw)
	var reserve map[string]json.RawMessage
	_ = json.Unmarshal(raw["reserve"], &reserve)
	if string(reserve["effective_pct"]) != "null" {
		t.Errorf("effective_pct = %s, want null when absent", reserve["effective_pct"])
	}
}

func TestHubSettings_Finite(t *testing.T) {
	ok := HubSettings{
		Reserve: ReserveSettings{EffectivePct: f64(30), FloorPct: f64(20)},
		Tariff:  TariffSettings{Spec: &TariffSpec{Periods: []TariffPeriod{{ImportPerKwh: 0.2}}}},
	}
	if err := ok.Finite(); err != nil {
		t.Errorf("Finite() on valid settings = %v, want nil", err)
	}

	// A non-finite reserve pct (a value only a lax decoder could smuggle in) is
	// caught.
	bad := HubSettings{Reserve: ReserveSettings{EffectivePct: f64(math.NaN())}}
	if err := bad.Finite(); err == nil {
		t.Error("Finite() on NaN effective_pct = nil, want error")
	}

	// A non-finite tariff import rate inside the spec is caught.
	badTariff := HubSettings{Tariff: TariffSettings{Spec: &TariffSpec{
		Periods: []TariffPeriod{{ImportPerKwh: math.Inf(1)}},
	}}}
	if err := badTariff.Finite(); err == nil {
		t.Error("Finite() on Inf import_per_kwh = nil, want error")
	}
}
