package orchestrator_test

import (
	"testing"
	"time"

	"lexa-hub/internal/orchestrator"
)

func makeTime(hour int) time.Time {
	return time.Date(2024, 1, 15, hour, 0, 0, 0, time.UTC)
}

func TestTOU_PeakRate(t *testing.T) {
	m := orchestrator.DefaultTOUCostModel()
	rate := m.CurrentRate(makeTime(17)) // 17:00 → peak
	if rate < 0.30 {
		t.Errorf("17:00 rate = %.3f, want ≥ 0.30 (peak)", rate)
	}
}

func TestTOU_OffPeakRate(t *testing.T) {
	m := orchestrator.DefaultTOUCostModel()
	rate := m.CurrentRate(makeTime(2)) // 02:00 → off-peak
	if rate >= 0.30 {
		t.Errorf("02:00 rate = %.3f, want < 0.30 (off-peak)", rate)
	}
}

func TestTOU_IsPeakHour(t *testing.T) {
	m := orchestrator.DefaultTOUCostModel()
	if !m.IsPeakHour(makeTime(18)) {
		t.Error("18:00 should be peak hour")
	}
	if m.IsPeakHour(makeTime(3)) {
		t.Error("03:00 should not be peak hour")
	}
}

func TestTOU_CurrentPeriodLabel(t *testing.T) {
	m := orchestrator.DefaultTOUCostModel()
	cases := []struct {
		hour  int
		label string
	}{
		{17, "peak"},
		{2, "off-peak"},
		{10, "partial-peak"},
	}
	for _, tc := range cases {
		got := m.CurrentPeriodLabel(makeTime(tc.hour))
		if got != tc.label {
			t.Errorf("hour=%d: label=%q, want %q", tc.hour, got, tc.label)
		}
	}
}

func TestTOU_ChargeCost(t *testing.T) {
	m := orchestrator.DefaultTOUCostModel()
	// 1 kWh at off-peak (rate=0.10) = $0.10
	cost := m.ChargeCost(makeTime(2), 1.0)
	if cost != 0.10 {
		t.Errorf("ChargeCost = %.3f, want 0.100", cost)
	}
}

func TestTOU_DischargeSavings(t *testing.T) {
	m := orchestrator.DefaultTOUCostModel()
	// Discharging 1 kWh during peak (rate=0.38) saves $0.38
	savings := m.DischargeSavings(makeTime(17), 1.0)
	if savings != 0.38 {
		t.Errorf("DischargeSavings = %.3f, want 0.380", savings)
	}
}

func TestTOU_OptimalChargeWindow_IsOffPeak(t *testing.T) {
	m := orchestrator.DefaultTOUCostModel()
	now := makeTime(12)
	bestStart := m.OptimalChargeWindow(now, 2)
	// The cheapest 2-hour window should be in off-peak (0–7 or 21–24).
	rate := m.CurrentRate(time.Date(2024, 1, 15, bestStart, 0, 0, 0, time.UTC))
	if rate >= 0.30 {
		t.Errorf("optimal charge window at hour %d has rate %.3f (peak!)", bestStart, rate)
	}
}

func TestTOU_CustomPeriods(t *testing.T) {
	m := orchestrator.NewTOUCostModel(
		[]orchestrator.TOUPeriod{
			{StartHour: 8, EndHour: 20, RatePerKwh: 0.25, Label: "day"},
			{StartHour: 20, EndHour: 8, RatePerKwh: 0.08, Label: "night"},
		},
		0.15, // default
		0.20, // peak threshold
	)

	if !m.IsPeakHour(makeTime(10)) {
		t.Error("10:00 should be peak with custom threshold 0.20")
	}
	if m.IsPeakHour(makeTime(22)) {
		t.Error("22:00 should not be peak (night rate 0.08 < 0.20)")
	}
}
