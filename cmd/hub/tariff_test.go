package main

import (
	"math"
	"strings"
	"testing"
	"time"

	"lexa-hub/internal/bus"
)

func fPtr(f float64) *float64 { return &f }

// allDays is the every-day-of-week set the common valid specs use.
var allDays = []int{0, 1, 2, 3, 4, 5, 6}

// validTOUSpec is a fully-covering, day-invariant TOU tariff mirroring
// DefaultTOUCostModel's schedule (peak 16–21 @0.38, partial 7–16 @0.18,
// off-peak split across midnight @0.10). Used by the happy-path, DST, and
// zone-hazard tests.
func validTOUSpec() bus.TariffSpec {
	return bus.TariffSpec{
		Currency: "USD",
		Periods: []bus.TariffPeriod{
			{Label: "off-peak", Days: allDays, StartHH: 0, EndHH: 7, ImportPerKwh: 0.10},
			{Label: "partial-peak", Days: allDays, StartHH: 7, EndHH: 16, ImportPerKwh: 0.18},
			{Label: "peak", Days: allDays, StartHH: 16, EndHH: 21, ImportPerKwh: 0.38},
			{Label: "off-peak", Days: allDays, StartHH: 21, EndHH: 24, ImportPerKwh: 0.10},
		},
	}
}

func TestCompileTariff_HappyPath(t *testing.T) {
	m, err := compileTariff(validTOUSpec())
	if err != nil {
		t.Fatalf("compileTariff: unexpected error: %v", err)
	}
	// hour-of-day resolution (evaluated in UTC — value is zone-agnostic here).
	at := func(hh int) time.Time { return time.Date(2026, 6, 15, hh, 0, 0, 0, time.UTC) }

	checks := []struct {
		hh    int
		rate  float64
		peak  bool
		label string
	}{
		{3, 0.10, false, "off-peak"},
		{7, 0.18, false, "partial-peak"},
		{15, 0.18, false, "partial-peak"},
		{16, 0.38, true, "peak"},
		{17, 0.38, true, "peak"},
		{20, 0.38, true, "peak"},
		{21, 0.10, false, "off-peak"},
		{23, 0.10, false, "off-peak"},
	}
	for _, c := range checks {
		if got := m.CurrentRate(at(c.hh)); got != c.rate {
			t.Errorf("CurrentRate(%02d:00) = %v, want %v", c.hh, got, c.rate)
		}
		if got := m.IsPeakHour(at(c.hh)); got != c.peak {
			t.Errorf("IsPeakHour(%02d:00) = %v, want %v", c.hh, got, c.peak)
		}
		if got := m.CurrentPeriodLabel(at(c.hh)); got != c.label {
			t.Errorf("CurrentPeriodLabel(%02d:00) = %q, want %q", c.hh, got, c.label)
		}
	}
}

func TestCompileTariff_FlatTariff_NeverPeak(t *testing.T) {
	spec := bus.TariffSpec{
		Currency: "USD",
		Periods:  []bus.TariffPeriod{{Label: "flat", Days: allDays, StartHH: 0, EndHH: 24, ImportPerKwh: 0.20}},
	}
	m, err := compileTariff(spec)
	if err != nil {
		t.Fatalf("compileTariff(flat): unexpected error: %v", err)
	}
	for hh := 0; hh < 24; hh++ {
		ti := time.Date(2026, 6, 15, hh, 0, 0, 0, time.UTC)
		if m.CurrentRate(ti) != 0.20 {
			t.Errorf("flat CurrentRate(%02d) = %v, want 0.20", hh, m.CurrentRate(ti))
		}
		if m.IsPeakHour(ti) {
			t.Errorf("flat tariff has no peak window, but IsPeakHour(%02d) = true", hh)
		}
	}
}

func TestCompileTariff_ExportRate_NilOrZeroAccepted(t *testing.T) {
	spec := validTOUSpec()
	spec.Periods[2].ExportPerKwh = fPtr(0.0) // explicit zero — nothing lost
	if _, err := compileTariff(spec); err != nil {
		t.Fatalf("explicit zero export should be accepted, got: %v", err)
	}
	spec.Periods[2].ExportPerKwh = nil // absent — nothing lost
	if _, err := compileTariff(spec); err != nil {
		t.Fatalf("nil export should be accepted, got: %v", err)
	}
}

func TestCompileTariff_Rejections(t *testing.T) {
	// helper spec builders that return an invalid spec + the substring the
	// error must mention.
	badCurrency := validTOUSpec()
	badCurrency.Currency = "EUR"

	emptyCurrency := validTOUSpec()
	emptyCurrency.Currency = ""

	noPeriods := bus.TariffSpec{Currency: "USD"}

	emptyDays := validTOUSpec()
	emptyDays.Periods[0].Days = nil

	dayOOR := validTOUSpec()
	dayOOR.Periods[0].Days = []int{0, 7}

	startOOR := validTOUSpec()
	startOOR.Periods[0].StartHH = -1

	endOOR := validTOUSpec()
	endOOR.Periods[0].EndHH = 25

	wrap := validTOUSpec()
	wrap.Periods[0].StartHH = 22
	wrap.Periods[0].EndHH = 6 // start >= end

	emptyWindow := validTOUSpec()
	emptyWindow.Periods[0].StartHH = 5
	emptyWindow.Periods[0].EndHH = 5

	negRate := validTOUSpec()
	negRate.Periods[0].ImportPerKwh = -0.01

	nanRate := validTOUSpec()
	nanRate.Periods[0].ImportPerKwh = math.NaN()

	infRate := validTOUSpec()
	infRate.Periods[0].ImportPerKwh = math.Inf(1)

	nonZeroExport := validTOUSpec()
	nonZeroExport.Periods[2].ExportPerKwh = fPtr(0.05)

	negExport := validTOUSpec()
	negExport.Periods[2].ExportPerKwh = fPtr(-0.01)

	// gap: leave hour 16 uncovered (0–16 then 17–24).
	gap := bus.TariffSpec{
		Currency: "USD",
		Periods: []bus.TariffPeriod{
			{Label: "a", Days: allDays, StartHH: 0, EndHH: 16, ImportPerKwh: 0.10},
			{Label: "b", Days: allDays, StartHH: 17, EndHH: 24, ImportPerKwh: 0.10},
		},
	}

	// overlap: two periods both cover hour 10.
	overlap := bus.TariffSpec{
		Currency: "USD",
		Periods: []bus.TariffPeriod{
			{Label: "a", Days: allDays, StartHH: 0, EndHH: 12, ImportPerKwh: 0.10},
			{Label: "b", Days: allDays, StartHH: 10, EndHH: 24, ImportPerKwh: 0.10},
		},
	}

	// per-day-of-week RATE divergence: weekday peak differs from weekend.
	perDayRate := bus.TariffSpec{
		Currency: "USD",
		Periods: []bus.TariffPeriod{
			{Label: "day", Days: allDays, StartHH: 0, EndHH: 16, ImportPerKwh: 0.10},
			{Label: "eve", Days: allDays, StartHH: 21, EndHH: 24, ImportPerKwh: 0.10},
			{Label: "wkday-peak", Days: []int{1, 2, 3, 4, 5}, StartHH: 16, EndHH: 21, ImportPerKwh: 0.38},
			{Label: "wkend-off", Days: []int{0, 6}, StartHH: 16, EndHH: 21, ImportPerKwh: 0.10},
		},
	}

	// per-day-of-week LABEL divergence: same rate, different label by day.
	perDayLabel := bus.TariffSpec{
		Currency: "USD",
		Periods: []bus.TariffPeriod{
			{Label: "day", Days: allDays, StartHH: 0, EndHH: 16, ImportPerKwh: 0.10},
			{Label: "eve", Days: allDays, StartHH: 21, EndHH: 24, ImportPerKwh: 0.10},
			{Label: "peak", Days: []int{1, 2, 3, 4, 5}, StartHH: 16, EndHH: 21, ImportPerKwh: 0.38},
			{Label: "weekend-peak", Days: []int{0, 6}, StartHH: 16, EndHH: 21, ImportPerKwh: 0.38},
		},
	}

	cases := []struct {
		name string
		spec bus.TariffSpec
		want string
	}{
		{"bad currency", badCurrency, "currency"},
		{"empty currency", emptyCurrency, "currency"},
		{"no periods", noPeriods, "at least one period"},
		{"empty days", emptyDays, "Days is empty"},
		{"day out of range", dayOOR, "day 7 out of range"},
		{"start hh out of range", startOOR, "start_hh"},
		{"end hh out of range", endOOR, "end_hh"},
		{"wrap window", wrap, "must be <"},
		{"empty window", emptyWindow, "must be <"},
		{"negative rate", negRate, "import_per_kwh"},
		{"nan rate", nanRate, "import_per_kwh"},
		{"inf rate", infRate, "import_per_kwh"},
		{"non-zero export", nonZeroExport, "export_per_kwh"},
		{"negative export", negExport, "export_per_kwh"},
		{"gap", gap, "gap"},
		{"overlap", overlap, "overlap"},
		{"per-day rate", perDayRate, "per-day-of-week rates"},
		{"per-day label", perDayLabel, "per-day-of-week labels"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, err := compileTariff(c.spec)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil (model=%v)", c.want, m)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), c.want)
			}
			if m != nil {
				t.Errorf("expected nil model on error, got %v", m)
			}
		})
	}
}

// TestCompileTariff_DST_PricesLocalClock is the compiled-model twin of
// costmodel_test.go's DST tables: a user tariff compiled via compileTariff
// prices the spring-forward and fall-back days correctly in America/Los_Angeles
// because the model keys off local-clock hour-of-day (WS-8/TASK-079: the SOM
// process zone must equal the tariff zone, which checkTariffZone asserts).
func TestCompileTariff_DST_PricesLocalClock(t *testing.T) {
	loc := mustLoadLocation(t, "America/Los_Angeles")
	m, err := compileTariff(validTOUSpec())
	if err != nil {
		t.Fatalf("compileTariff: %v", err)
	}

	cases := []struct {
		name     string
		y        int
		mo       time.Month
		d        int
		hh       int
		wantPeak bool
		wantRate float64
	}{
		{"spring-forward evening peak", 2026, time.March, 8, 17, true, 0.38},
		{"spring-forward morning partial", 2026, time.March, 8, 9, false, 0.18},
		{"spring-forward post-gap off-peak (03:00)", 2026, time.March, 8, 3, false, 0.10},
		{"fall-back evening peak", 2026, time.November, 1, 17, true, 0.38},
		{"fall-back fold-hour off-peak (01:00)", 2026, time.November, 1, 1, false, 0.10},
		{"normal-day peak control", 2026, time.June, 15, 17, true, 0.38},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ti := time.Date(c.y, c.mo, c.d, c.hh, 0, 0, 0, loc)
			if got := m.IsPeakHour(ti); got != c.wantPeak {
				t.Errorf("IsPeakHour(%s) = %v, want %v", ti.Format(time.RFC3339), got, c.wantPeak)
			}
			if got := m.CurrentRate(ti); got != c.wantRate {
				t.Errorf("CurrentRate(%s) = %v, want %v", ti.Format(time.RFC3339), got, c.wantRate)
			}
		})
	}
}

// TestCompileTariff_ZoneMismatchHazard mirrors
// TestTOU_UTCvsLA_Divergence_DeploymentHazard for the compiled model: the SAME
// absolute instant classifies differently in LA vs UTC, so the SOM zone must
// match the tariff zone (documented, not "fixed" — the local-clock semantics
// are correct; see CLAUDE.md "SOM zone must match the tariff zone").
func TestCompileTariff_ZoneMismatchHazard(t *testing.T) {
	loc := mustLoadLocation(t, "America/Los_Angeles")
	m, err := compileTariff(validTOUSpec())
	if err != nil {
		t.Fatalf("compileTariff: %v", err)
	}
	inLA := time.Date(2026, 6, 15, 17, 0, 0, 0, loc) // 17:00 PDT = peak
	inUTC := inLA.In(time.UTC)                       // same instant, Hour()==0 in UTC = off-peak

	if !m.IsPeakHour(inLA) {
		t.Fatal("17:00 LA should be peak under the compiled tariff")
	}
	if m.IsPeakHour(inUTC) {
		t.Fatal("the UTC rendering of the same instant must misclassify as not-peak (documents the zone hazard)")
	}
	if inLA.Unix() != inUTC.Unix() {
		t.Fatal("test setup: inLA and inUTC must be the same absolute instant")
	}
}
