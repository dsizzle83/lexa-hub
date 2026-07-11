package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lexa-hub/internal/bus"
)

func fptr(v float64) *float64 { return &v }

// TestBuildStatus_ReserveTariffAdditive pins the GAP-8 /status additions:
// omitted until the first HubSettings arrives, then reserve{effective_pct,
// floor_pct,source} + tariff{source,updated_at,spec} projected verbatim, and
// never disturbing the existing /status shape.
func TestBuildStatus_ReserveTariffAdditive(t *testing.T) {
	store := newStateStore(nil, time.Minute)

	t.Run("omitted before any HubSettings", func(t *testing.T) {
		resp := buildStatus(store.snapshot(), heartbeatStatus{State: heartbeatNever})
		if resp.Reserve != nil {
			t.Errorf("Reserve = %+v, want nil before any HubSettings", resp.Reserve)
		}
		if resp.Tariff != nil {
			t.Errorf("Tariff = %+v, want nil before any HubSettings", resp.Tariff)
		}
	})

	store.onHubSettings(bus.TopicHubSettings, bus.HubSettings{
		Reserve: bus.ReserveSettings{EffectivePct: fptr(35), FloorPct: fptr(20), Source: "app"},
		Tariff: bus.TariffSettings{
			Source:    "manual",
			UpdatedAt: 1_752_000_000,
			Spec: &bus.TariffSpec{Currency: "USD", Periods: []bus.TariffPeriod{
				{Label: "peak", Days: []int{0, 1, 2, 3, 4, 5, 6}, StartHH: 16, EndHH: 21, ImportPerKwh: 0.38},
			}},
		},
	})

	t.Run("populated once HubSettings arrives", func(t *testing.T) {
		resp := buildStatus(store.snapshot(), heartbeatStatus{State: heartbeatNever})
		if resp.Reserve == nil {
			t.Fatal("Reserve is nil, want populated")
		}
		if resp.Reserve.EffectivePct == nil || *resp.Reserve.EffectivePct != 35 ||
			resp.Reserve.FloorPct == nil || *resp.Reserve.FloorPct != 20 ||
			resp.Reserve.Source != "app" {
			t.Errorf("Reserve = %+v, want {effective 35, floor 20, app}", resp.Reserve)
		}
		if resp.Tariff == nil || resp.Tariff.Source != "manual" || resp.Tariff.UpdatedAt != 1_752_000_000 {
			t.Fatalf("Tariff = %+v, want manual/1752000000", resp.Tariff)
		}
		if resp.Tariff.Spec == nil || resp.Tariff.Spec.Currency != "USD" ||
			len(resp.Tariff.Spec.Periods) != 1 || resp.Tariff.Spec.Periods[0].Label != "peak" {
			t.Errorf("Tariff.Spec = %+v, want the 1-period USD peak spec", resp.Tariff.Spec)
		}
	})
}

// TestStatusHandler_ReserveTariffWireShape drives /status end to end and pins
// the exact JSON keys the app's /status map + TariffSpec.fromJson consume.
func TestStatusHandler_ReserveTariffWireShape(t *testing.T) {
	store := newStateStore(nil, time.Minute)
	store.onHubSettings(bus.TopicHubSettings, bus.HubSettings{
		Reserve: bus.ReserveSettings{EffectivePct: fptr(30), FloorPct: fptr(20), Source: "lexactl"},
		Tariff: bus.TariffSettings{Source: "csip", UpdatedAt: 42, Spec: &bus.TariffSpec{
			Currency: "USD",
			Periods: []bus.TariffPeriod{
				{Label: "off-peak", Days: []int{0, 1, 2, 3, 4, 5, 6}, StartHH: 0, EndHH: 24, ImportPerKwh: 0.15, ExportPerKwh: fptr(0)},
			},
		}},
	})

	h := statusHandler(store, newPlanHeartbeat(75*time.Second), "")
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp struct {
		Reserve struct {
			EffectivePct *float64 `json:"effective_pct"`
			FloorPct     *float64 `json:"floor_pct"`
			Source       string   `json:"source"`
		} `json:"reserve"`
		Tariff struct {
			Source    string `json:"source"`
			UpdatedAt int64  `json:"updated_at"`
			Spec      struct {
				Currency string `json:"currency"`
				Periods  []struct {
					Label        string  `json:"label"`
					Days         []int   `json:"days"`
					StartHH      int     `json:"start_hh"`
					EndHH        int     `json:"end_hh"`
					ImportPerKwh float64 `json:"import_per_kwh"`
					ExportPerKwh float64 `json:"export_per_kwh"`
				} `json:"periods"`
			} `json:"spec"`
		} `json:"tariff"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode /status with app shape: %v", err)
	}
	if resp.Reserve.EffectivePct == nil || *resp.Reserve.EffectivePct != 30 || resp.Reserve.Source != "lexactl" {
		t.Errorf("reserve = %+v, want {30, lexactl}", resp.Reserve)
	}
	if resp.Tariff.Source != "csip" || resp.Tariff.Spec.Currency != "USD" || len(resp.Tariff.Spec.Periods) != 1 {
		t.Fatalf("tariff = %+v, want csip/USD/1-period", resp.Tariff)
	}
	p := resp.Tariff.Spec.Periods[0]
	if p.Label != "off-peak" || p.StartHH != 0 || p.EndHH != 24 || p.ImportPerKwh != 0.15 {
		t.Errorf("period = %+v, want off-peak 0..24 @0.15", p)
	}
}
