package orchestrator

import (
	"math"
	"testing"
	"time"
)

// benchParams is the review's documented worst case: 10 kWh battery +
// 75 kWh EV at the default 0.5 kWh SOC step.
func benchParams() PlannerParams {
	solar := make([]float64, planSteps)
	for i := range solar {
		h := float64(i) * 5.0 / 60.0
		if h > 6 && h < 18 {
			solar[i] = 5 * math.Sin((h-6)/12*math.Pi)
		}
	}
	return PlannerParams{
		Now:                time.Unix(1750000000, 0),
		WindowStart:        1750000000,
		BattCapacityKwh:    10,
		BattMaxChargeKw:    5,
		BattMaxDischargeKw: 5,
		InitialBattSocKwh:  5,
		EVCapacityKwh:      75,
		EVMaxChargeKw:      11.5,
		InitialEVSocKwh:    30,
		EVTargetSocKwh:     60,
		EVDepartureUnix:    1750000000 + 16*3600,
		EVVoltageV:         240,
		SolarForecastKw:    solar,
		LoadForecastKw:     1.2,
		FallbackTOU:        DefaultTOUCostModel(),
	}
}

// constrainedBenchParams adds DER constraints to exercise the limit and
// fixed-dispatch paths.
func constrainedBenchParams() PlannerParams {
	p := benchParams()
	cons := make([]StepConstraint, planSteps)
	for i := range cons {
		cons[i] = StepConstraint{ExpLimW: math.NaN(), ImpLimW: math.NaN(), MaxLimW: math.NaN(), FixedW: math.NaN(), GenLimW: math.NaN(), LoadLimW: math.NaN()}
	}
	for i := 24; i < 48; i++ { // 2h export limit
		cons[i].ExpLimW = 2000
	}
	for i := 60; i < 72; i++ { // 1h disconnect
		cons[i].Disconnect = true
	}
	for i := 100; i < 112; i++ { // 1h fixed dispatch
		cons[i].FixedW = 1500
	}
	for i := 150; i < 200; i++ { // import limit window
		cons[i].ImpLimW = 4000
	}
	p.DERConstraints = cons
	return p
}

func BenchmarkDailyPlanner_Plan(b *testing.B) {
	pl := NewDailyPlanner()
	p := benchParams()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = pl.Plan(p)
	}
}

func BenchmarkDailyPlanner_PlanConstrained(b *testing.B) {
	pl := NewDailyPlanner()
	p := constrainedBenchParams()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = pl.Plan(p)
	}
}
