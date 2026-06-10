package main

import (
	"testing"
	"time"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/orchestrator"
)

// TestPricesFromPricingUpdate_UncoveredStepsUseFallback is the C7 regression:
// plan steps not covered by any tariff interval must be priced at the
// fallback TOU rate, never zero — a zero price tells the planner those hours
// are free electricity and it schedules grid charging into them.
func TestPricesFromPricingUpdate_UncoveredStepsUseFallback(t *testing.T) {
	const planStepSec = int64(5 * 60)
	now := time.Now()
	ws := now.Unix() - now.Unix()%planStepSec

	// One tariff interval covering the first hour at 0.5 $/kWh (500 milli-currency).
	pricing := bus.PricingUpdate{TariffProfiles: []bus.TariffProfileMsg{{
		MRID: "tp-1",
		RateComponents: []bus.RateComponentMsg{{
			ActiveIntervals: []bus.TimeTariffMsg{{
				IntervalStart: ws,
				Duration:      3600,
				Blocks:        []bus.PriceBlockMsg{{Price: 500}},
			}},
		}},
	}}}

	imp, exp := pricesFromPricingUpdate(pricing, now)
	if imp == nil {
		t.Fatal("expected import prices, got nil")
	}
	if exp != nil {
		t.Errorf("expected nil export prices (no export tariff data), got len %d", len(exp))
	}

	// Covered step: tariff price wins.
	if imp[0] != 0.5 {
		t.Errorf("imp[0] = %v, want 0.5 (tariff)", imp[0])
	}

	// Uncovered step (step 100 ≈ 8 h 20 m out, well past the 1 h interval):
	// fallback TOU rate for that time of day, and never zero.
	fallback := orchestrator.DefaultTOUCostModel()
	stepT := ws + 100*planStepSec
	want := fallback.CurrentRate(time.Unix(stepT, 0).Local())
	if imp[100] != want {
		t.Errorf("imp[100] = %v, want fallback TOU rate %v", imp[100], want)
	}
	if imp[100] == 0 {
		t.Error("uncovered step priced at zero — planner would treat grid as free")
	}
}

func TestPricesFromPricingUpdate_NoProfiles(t *testing.T) {
	imp, exp := pricesFromPricingUpdate(bus.PricingUpdate{}, time.Now())
	if imp != nil || exp != nil {
		t.Errorf("expected nil, nil for empty update, got %v, %v", imp, exp)
	}
}
