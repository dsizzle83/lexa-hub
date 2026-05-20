package orchestrator

import (
	"math"
	"time"
)

// TOUPeriod defines a time-of-use pricing window.
type TOUPeriod struct {
	// StartHour and EndHour are in 24-hour local time [0, 23].
	// EndHour is exclusive: {Start:16, End:21} means 16:00–20:59.
	StartHour int
	EndHour   int
	// RatePerKwh is the electricity rate in cost-units per kWh.
	RatePerKwh float64
	// Label is a human-readable name for this period ("peak", "off-peak", etc.)
	Label string
}

// TOUCostModel implements time-of-use pricing.
type TOUCostModel struct {
	periods       []TOUPeriod
	defaultRate   float64
	peakThreshold float64 // rates above this are considered "peak"
}

// NewTOUCostModel creates a cost model from the given periods.
// defaultRate applies when no period matches.
// peakThreshold marks which rates are "peak" for demand-response purposes.
func NewTOUCostModel(periods []TOUPeriod, defaultRate, peakThreshold float64) *TOUCostModel {
	return &TOUCostModel{
		periods:       periods,
		defaultRate:   defaultRate,
		peakThreshold: peakThreshold,
	}
}

// DefaultTOUCostModel returns a typical US residential TOU schedule.
// Rates are illustrative ($/kWh).
func DefaultTOUCostModel() *TOUCostModel {
	return NewTOUCostModel(
		[]TOUPeriod{
			{StartHour: 16, EndHour: 21, RatePerKwh: 0.38, Label: "peak"},
			{StartHour: 7, EndHour: 16, RatePerKwh: 0.18, Label: "partial-peak"},
			{StartHour: 21, EndHour: 24, RatePerKwh: 0.10, Label: "off-peak"},
			{StartHour: 0, EndHour: 7, RatePerKwh: 0.10, Label: "off-peak"},
		},
		0.18,  // default rate
		0.30,  // rates >= 0.30 are "peak"
	)
}

// CurrentRate returns the electricity rate at t.
func (m *TOUCostModel) CurrentRate(t time.Time) float64 {
	hour := t.Hour()
	for _, p := range m.periods {
		if hourInPeriod(hour, p) {
			return p.RatePerKwh
		}
	}
	return m.defaultRate
}

// IsPeakHour returns true when the current TOU rate at t is at or above
// the peak threshold.
func (m *TOUCostModel) IsPeakHour(t time.Time) bool {
	return m.CurrentRate(t) >= m.peakThreshold
}

// CurrentPeriodLabel returns the label of the active TOU period at t.
func (m *TOUCostModel) CurrentPeriodLabel(t time.Time) string {
	hour := t.Hour()
	for _, p := range m.periods {
		if hourInPeriod(hour, p) {
			return p.Label
		}
	}
	return "default"
}

// ChargeCost computes the cost to charge energyKwh at time t.
func (m *TOUCostModel) ChargeCost(t time.Time, energyKwh float64) float64 {
	return m.CurrentRate(t) * energyKwh
}

// DischargeSavings computes the savings from discharging energyKwh at time t
// (displacing grid import at the current rate).
func (m *TOUCostModel) DischargeSavings(t time.Time, energyKwh float64) float64 {
	return m.CurrentRate(t) * energyKwh
}

// OptimalChargeWindow finds the cheapest N-hour window within the next 24 hours
// to charge a battery. Returns the start hour (local time).
func (m *TOUCostModel) OptimalChargeWindow(now time.Time, durationHours int) int {
	if durationHours <= 0 {
		return now.Hour()
	}
	bestCost := math.MaxFloat64
	bestHour := now.Hour()

	for startHour := 0; startHour < 24; startHour++ {
		var totalCost float64
		for h := 0; h < durationHours; h++ {
			t := time.Date(now.Year(), now.Month(), now.Day(),
				(startHour+h)%24, 0, 0, 0, now.Location())
			totalCost += m.CurrentRate(t)
		}
		if totalCost < bestCost {
			bestCost = totalCost
			bestHour = startHour
		}
	}
	return bestHour
}

func hourInPeriod(hour int, p TOUPeriod) bool {
	if p.StartHour <= p.EndHour {
		return hour >= p.StartHour && hour < p.EndHour
	}
	// Wraps midnight: e.g. 22–06
	return hour >= p.StartHour || hour < p.EndHour
}
