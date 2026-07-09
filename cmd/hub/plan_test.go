package main

import (
	"encoding/json"
	"strings"
	"testing"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/metrics"
)

// TestEnrichPlanLog_StampsFields pins the Unit 3.6 plan-log enrichment: mode +
// solar-forecast provenance land on the PlanLog exactly as planObserver hands
// them in (from modeMgr.Mode / eng.ForecastSource / eng.ForecastAgeSeconds).
func TestEnrichPlanLog_StampsFields(t *testing.T) {
	pl := bus.PlanLog{Envelope: bus.Envelope{V: bus.PlanLogV}, Ts: 10}
	enrichPlanLog(&pl, "gateway", "external", 42)
	if pl.Mode != "gateway" || pl.ForecastSource != "external" || pl.ForecastAgeS != 42 {
		t.Fatalf("enrichPlanLog stamped %+v, want mode=gateway source=external age=42", pl)
	}
}

// TestEnrichPlanLog_JSONAdditiveShape verifies the fields are additive (wire
// version unchanged at PlanLogV, omitempty semantics), and that an unstamped
// legacy PlanLog carries none of them — so cmd/api's passthrough relay and any
// legacy decoder are unaffected.
func TestEnrichPlanLog_JSONAdditiveShape(t *testing.T) {
	// Optimizer mode, no external forecast: mode present, source omitted (empty),
	// age -1 present (documented sentinel — not omitted), version still 1.
	pl := bus.PlanLog{Envelope: bus.Envelope{V: bus.PlanLogV}, Ts: 10}
	enrichPlanLog(&pl, "optimizer", "", -1)
	m := marshalToMap(t, pl)
	if m["v"] != float64(1) {
		t.Fatalf("wire version changed: v=%v, want 1", m["v"])
	}
	if m["mode"] != "optimizer" {
		t.Fatalf("mode = %v, want optimizer", m["mode"])
	}
	if _, ok := m["forecast_source"]; ok {
		t.Fatalf("empty forecast_source must be omitted, got %v", m["forecast_source"])
	}
	if m["forecast_age_s"] != float64(-1) {
		t.Fatalf("forecast_age_s = %v, want -1 (sentinel serialized, not omitted)", m["forecast_age_s"])
	}

	// A pristine (unstamped) PlanLog omits every new key — the additive contract.
	legacy := marshalToMap(t, bus.PlanLog{Envelope: bus.Envelope{V: bus.PlanLogV}, Ts: 5})
	for _, k := range []string{"mode", "forecast_source", "forecast_age_s"} {
		if _, ok := legacy[k]; ok {
			t.Fatalf("unstamped PlanLog must omit %q, got %v", k, legacy[k])
		}
	}
}

// TestForecastStaleAlarm_EdgeLatch pins the Unit 3.1 stale-forecast edge alarm:
// ONE warn on the diurnal-with-external-age onset, silent while it persists,
// cleared when the source recovers, and re-armed to warn again on a second
// episode. The lexa_hub_forecast_stale gauge tracks the raw condition (0/1).
func TestForecastStaleAlarm_EdgeLatch(t *testing.T) {
	reg := metrics.New()
	g := reg.Gauge("lexa_hub_forecast_stale")
	var warns int
	a := &forecastStaleAlarm{gauge: g, warn: func(string, ...any) { warns++ }}

	gaugeIs := func(want string) {
		t.Helper()
		if !strings.Contains(reg.Format(), "lexa_hub_forecast_stale "+want+"\n") {
			t.Fatalf("gauge render %q lacks lexa_hub_forecast_stale %s", reg.Format(), want)
		}
	}

	// Onset: diurnal fallback while a (too-old) external forecast exists.
	if !a.observe("diurnal", 100) {
		t.Fatal("stale onset must edge-warn")
	}
	if warns != 1 {
		t.Fatalf("warns = %d, want 1 on onset", warns)
	}
	gaugeIs("1")

	// Persisting stale: no new warn (latched).
	if a.observe("diurnal", 150) {
		t.Fatal("a still-stale tick must not re-warn")
	}
	if warns != 1 {
		t.Fatalf("warns = %d, want still 1 while latched", warns)
	}
	gaugeIs("1")

	// Recover: a fresh external forecast is back — latch clears, no warn.
	a.observe("external", 5)
	if warns != 1 {
		t.Fatalf("warns = %d, want still 1 on recovery", warns)
	}
	gaugeIs("0")

	// Re-arm: a second stale episode warns again.
	if !a.observe("diurnal", 200) {
		t.Fatal("a re-armed stale onset must warn again")
	}
	if warns != 2 {
		t.Fatalf("warns = %d, want 2 after re-arm", warns)
	}
	gaugeIs("1")

	// Diurnal with NO external forecast (age -1) is not stale → clears.
	a.observe("diurnal", -1)
	if warns != 2 {
		t.Fatalf("warns = %d, want still 2 (diurnal age -1 is not stale)", warns)
	}
	gaugeIs("0")
}

func marshalToMap(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}
