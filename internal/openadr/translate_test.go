package openadr

import (
	"reflect"
	"testing"
	"time"

	"lexa-hub/internal/bus"
)

// now14 is the fixed test clock: 2026-07-14T08:10:00Z.
var now14 = time.Date(2026, 7, 14, 8, 10, 0, 0, time.UTC)

func lookupNone(Event, string) *PayloadDescriptor { return nil }

// TestParseDuration table-tests the ISO 8601 subset incl. the infinity
// sentinel.
func TestParseDuration(t *testing.T) {
	cases := []struct {
		in        string
		want      time.Duration
		unbounded bool
		wantErr   bool
	}{
		{"PT15M", 15 * time.Minute, false, false},
		{"PT1H", time.Hour, false, false},
		{"PT1H30M", 90 * time.Minute, false, false},
		{"PT0.5S", 500 * time.Millisecond, false, false},
		{"P1D", 24 * time.Hour, false, false},
		{"P1W", 7 * 24 * time.Hour, false, false},
		{"P1DT2H", 26 * time.Hour, false, false},
		{"P9999Y", 0, true, false},
		{"", 0, false, true},
		{"15M", 0, false, true},
		{"PT", 0, false, true},
		{"P-1D", 0, false, true},
		{"PT5X", 0, false, true},
	}
	for _, tc := range cases {
		d, unb, err := ParseDuration(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseDuration(%q): want error, got %v/%v", tc.in, d, unb)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseDuration(%q): %v", tc.in, err)
			continue
		}
		if d != tc.want || unb != tc.unbounded {
			t.Errorf("ParseDuration(%q) = (%v,%v), want (%v,%v)", tc.in, d, unb, tc.want, tc.unbounded)
		}
	}
}

// TestTranslatePricesGolden is the CP-profile price golden: a two-interval
// PRICE event with a program-level payload descriptor (the tariff-program
// shape) translates to the EXACT bus.OpenADRPrices document — consecutive
// absolute interval starts, currency/units from the descriptor, stamped
// envelope version.
func TestTranslatePricesGolden(t *testing.T) {
	event := Event{
		ID:        "evt-price-1",
		ProgramID: "prog-tariff",
		IntervalPeriod: &IntervalPeriod{
			Start:    "2026-07-14T08:00:00Z",
			Duration: "PT15M",
		},
		Intervals: []Interval{
			{ID: 0, Payloads: []ValuesMap{{Type: "PRICE", Values: []any{0.17}}}},
			{ID: 1, Payloads: []ValuesMap{{Type: "PRICE", Values: []any{0.42}}}},
		},
	}
	programs := map[string]Program{
		"prog-tariff": {
			ID: "prog-tariff",
			PayloadDescriptors: []PayloadDescriptor{{
				PayloadType: "PRICE", Units: "KWH", Currency: "USD",
			}},
		},
	}
	got := TranslatePrices([]EventInstance{{Event: event}}, NewDescriptorLookup(programs), now14)

	want := bus.OpenADRPrices{
		Envelope: bus.Envelope{V: bus.OpenADRPricesV},
		Series: []bus.OpenADRPriceSeries{{
			ProgramID: "prog-tariff",
			EventID:   "evt-price-1",
			Kind:      "PRICE",
			Currency:  "USD",
			Units:     "KWH",
			Intervals: []bus.OpenADRPriceInterval{
				{StartTs: 1784016000, Start: "2026-07-14T08:00:00Z", DurationS: 900, Value: 0.17},
				{StartTs: 1784016900, Start: "2026-07-14T08:15:00Z", DurationS: 900, Value: 0.42},
			},
		}},
		Ts: now14.Unix(),
	}
	// Compute the expected StartTs from the same instants rather than
	// hardcoding epoch literals.
	want.Series[0].Intervals[0].StartTs = time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC).Unix()
	want.Series[0].Intervals[1].StartTs = time.Date(2026, 7, 14, 8, 15, 0, 0, time.UTC).Unix()

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("prices golden mismatch:\n got %+v\nwant %+v", got, want)
	}
	if err := got.Finite(); err != nil {
		t.Fatalf("golden output not finite: %v", err)
	}
}

// TestTranslatePricesIncludesFutureExcludesPast: prices are planner
// forward-input — a fully-ended event drops out; a future event stays.
func TestTranslatePricesIncludesFutureExcludesPast(t *testing.T) {
	past := Event{
		ID: "evt-past", ProgramID: "p",
		IntervalPeriod: &IntervalPeriod{Start: "2026-07-14T06:00:00Z", Duration: "PT15M"},
		Intervals:      []Interval{{ID: 0, Payloads: []ValuesMap{{Type: "PRICE", Values: []any{0.1}}}}},
	}
	future := Event{
		ID: "evt-future", ProgramID: "p",
		IntervalPeriod: &IntervalPeriod{Start: "2026-07-14T18:00:00Z", Duration: "PT15M"},
		Intervals:      []Interval{{ID: 0, Payloads: []ValuesMap{{Type: "PRICE", Values: []any{0.9}}}}},
	}
	got := TranslatePrices([]EventInstance{{Event: past}, {Event: future}}, lookupNone, now14)
	if len(got.Series) != 1 || got.Series[0].EventID != "evt-future" {
		t.Fatalf("series = %+v, want only evt-future", got.Series)
	}
}

// TestTranslatePricesGHGAndAlertKinds: GHG and ALERT_* payloads translate as
// their own series with the kind tag preserved; series order is (Kind,
// EventID) deterministic.
func TestTranslatePricesGHGAndAlertKinds(t *testing.T) {
	event := Event{
		ID: "evt-multi", ProgramID: "p",
		IntervalPeriod: &IntervalPeriod{Start: "2026-07-14T08:00:00Z", Duration: "PT1H"},
		Intervals: []Interval{{ID: 0, Payloads: []ValuesMap{
			{Type: "GHG", Values: []any{450.0}},
			{Type: "ALERT_GRID_EMERGENCY", Values: []any{1.0}},
			{Type: "DISPATCH_SETPOINT", Values: []any{2000.0}}, // NOT a price kind — never published (D9: CSIP wins dispatch)
		}}},
	}
	got := TranslatePrices([]EventInstance{{Event: event}}, lookupNone, now14)
	if len(got.Series) != 2 {
		t.Fatalf("series count = %d, want 2 (GHG + ALERT), got %+v", len(got.Series), got.Series)
	}
	if got.Series[0].Kind != "ALERT_GRID_EMERGENCY" || got.Series[1].Kind != "GHG" {
		t.Fatalf("series kinds = %q,%q — want deterministic (Kind sorted) ALERT_GRID_EMERGENCY,GHG",
			got.Series[0].Kind, got.Series[1].Kind)
	}
}

// TestTranslateLimitsGolden: one active event with both capacity axes (KW
// units via event-level descriptors) produces the exact bus.OpenADRLimits
// doc — watts conversion, event attribution, valid_until = event end.
func TestTranslateLimitsGolden(t *testing.T) {
	event := Event{
		ID:        "evt-lim-1",
		ProgramID: "prog-dr",
		PayloadDescriptors: []PayloadDescriptor{
			{PayloadType: "IMPORT_CAPACITY_LIMIT", Units: "KW"},
			{PayloadType: "EXPORT_CAPACITY_LIMIT", Units: "KW"},
		},
		IntervalPeriod: &IntervalPeriod{Start: "2026-07-14T08:00:00Z", Duration: "PT1H"},
		Intervals: []Interval{{ID: 0, Payloads: []ValuesMap{
			{Type: "IMPORT_CAPACITY_LIMIT", Values: []any{5.0}},
			{Type: "EXPORT_CAPACITY_LIMIT", Values: []any{3.0}},
		}}},
	}
	got := TranslateLimits([]EventInstance{{Event: event}}, NewDescriptorLookup(nil), now14)

	imp, exp := 5000.0, 3000.0
	want := bus.OpenADRLimits{
		Envelope:   bus.Envelope{V: bus.OpenADRLimitsV},
		ImpLimW:    &imp,
		ExpLimW:    &exp,
		EventID:    "evt-lim-1",
		ValidUntil: time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC).Unix(),
		Ts:         now14.Unix(),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("limits golden mismatch:\n got %+v (imp=%v exp=%v)\nwant %+v (imp=%v exp=%v)",
			got, deref(got.ImpLimW), deref(got.ExpLimW), want, imp, exp)
	}
}

func deref(p *float64) float64 {
	if p == nil {
		return -1
	}
	return *p
}

// TestTranslateLimitsMostRestrictiveMerge: two concurrently-active events on
// the same axis merge to the MINIMUM (most restrictive — D9), attribution
// names the binding event, and valid_until is the earliest binding end.
func TestTranslateLimitsMostRestrictiveMerge(t *testing.T) {
	loose := Event{
		ID: "evt-loose", ProgramID: "p",
		PayloadDescriptors: []PayloadDescriptor{{PayloadType: "IMPORT_CAPACITY_LIMIT", Units: "KW"}},
		IntervalPeriod:     &IntervalPeriod{Start: "2026-07-14T08:00:00Z", Duration: "PT4H"},
		Intervals:          []Interval{{ID: 0, Payloads: []ValuesMap{{Type: "IMPORT_CAPACITY_LIMIT", Values: []any{10.0}}}}},
	}
	tight := Event{
		ID: "evt-tight", ProgramID: "p",
		PayloadDescriptors: []PayloadDescriptor{{PayloadType: "IMPORT_CAPACITY_LIMIT", Units: "KW"}},
		IntervalPeriod:     &IntervalPeriod{Start: "2026-07-14T08:00:00Z", Duration: "PT1H"},
		Intervals:          []Interval{{ID: 0, Payloads: []ValuesMap{{Type: "IMPORT_CAPACITY_LIMIT", Values: []any{4.0}}}}},
	}
	got := TranslateLimits([]EventInstance{{Event: loose}, {Event: tight}}, NewDescriptorLookup(nil), now14)
	if got.ImpLimW == nil || *got.ImpLimW != 4000 {
		t.Fatalf("imp_lim_w = %v, want 4000 (most restrictive)", deref(got.ImpLimW))
	}
	if got.EventID != "evt-tight" {
		t.Fatalf("event_id = %q, want evt-tight (the binding event)", got.EventID)
	}
	if want := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC).Unix(); got.ValidUntil != want {
		t.Fatalf("valid_until = %d, want %d (binding event's end)", got.ValidUntil, want)
	}
	if got.ExpLimW != nil {
		t.Fatalf("exp_lim_w = %v, want nil (no export limit active)", *got.ExpLimW)
	}
}

// TestTranslateLimitsInactiveEventExcluded: a future event's limit is NOT a
// live obligation — both axes nil (the explicit-release doc shape).
func TestTranslateLimitsInactiveEventExcluded(t *testing.T) {
	future := Event{
		ID: "evt-future", ProgramID: "p",
		PayloadDescriptors: []PayloadDescriptor{{PayloadType: "IMPORT_CAPACITY_LIMIT", Units: "KW"}},
		IntervalPeriod:     &IntervalPeriod{Start: "2026-07-14T18:00:00Z", Duration: "PT1H"},
		Intervals:          []Interval{{ID: 0, Payloads: []ValuesMap{{Type: "IMPORT_CAPACITY_LIMIT", Values: []any{4.0}}}}},
	}
	got := TranslateLimits([]EventInstance{{Event: future}}, NewDescriptorLookup(nil), now14)
	if got.ImpLimW != nil || got.ExpLimW != nil || got.EventID != "" || got.ValidUntil != 0 {
		t.Fatalf("future event leaked into live limits: %+v", got)
	}
}

// TestTranslateLimitsUnconvertibleUnitsDropped: a limit whose descriptor
// units cannot be converted to watts is dropped, never guessed (G27).
func TestTranslateLimitsUnconvertibleUnitsDropped(t *testing.T) {
	event := Event{
		ID: "evt-bad-units", ProgramID: "p",
		PayloadDescriptors: []PayloadDescriptor{{PayloadType: "IMPORT_CAPACITY_LIMIT", Units: "PERCENT"}},
		IntervalPeriod:     &IntervalPeriod{Start: "2026-07-14T08:00:00Z", Duration: "PT1H"},
		Intervals:          []Interval{{ID: 0, Payloads: []ValuesMap{{Type: "IMPORT_CAPACITY_LIMIT", Values: []any{50.0}}}}},
	}
	got := TranslateLimits([]EventInstance{{Event: event}}, NewDescriptorLookup(nil), now14)
	if got.ImpLimW != nil {
		t.Fatalf("PERCENT-unit limit adopted as %v W — must be dropped", *got.ImpLimW)
	}
}

// TestRandomizeStartShiftsSchedule: the instance's once-assigned offset
// shifts every interval start uniformly.
func TestRandomizeStartShiftsSchedule(t *testing.T) {
	event := Event{
		ID: "evt-rand", ProgramID: "p",
		IntervalPeriod: &IntervalPeriod{Start: "2026-07-14T08:00:00Z", Duration: "PT15M", RandomizeStart: "PT5M"},
		Intervals:      []Interval{{ID: 0, Payloads: []ValuesMap{{Type: "PRICE", Values: []any{0.3}}}}},
	}
	got := TranslatePrices([]EventInstance{{Event: event, RandOffset: 90 * time.Second}}, lookupNone, now14)
	if len(got.Series) != 1 || len(got.Series[0].Intervals) != 1 {
		t.Fatalf("series = %+v", got.Series)
	}
	want := time.Date(2026, 7, 14, 8, 1, 30, 0, time.UTC).Unix()
	if got.Series[0].Intervals[0].StartTs != want {
		t.Fatalf("randomized start = %d, want %d (+90s)", got.Series[0].Intervals[0].StartTs, want)
	}
}

// TestCountActive covers the events_active gauge source.
func TestCountActive(t *testing.T) {
	active := Event{
		ID: "a", ProgramID: "p",
		IntervalPeriod: &IntervalPeriod{Start: "2026-07-14T08:00:00Z", Duration: "PT1H"},
		Intervals:      []Interval{{ID: 0}},
	}
	inactive := Event{
		ID: "b", ProgramID: "p",
		IntervalPeriod: &IntervalPeriod{Start: "2026-07-14T18:00:00Z", Duration: "PT1H"},
		Intervals:      []Interval{{ID: 0}},
	}
	if n := CountActive([]EventInstance{{Event: active}, {Event: inactive}}, now14); n != 1 {
		t.Fatalf("CountActive = %d, want 1", n)
	}
}

// TestNowSentinelStart: the "0001-01-01" start sentinel anchors the event at
// the translation clock — its first interval is active immediately.
func TestNowSentinelStart(t *testing.T) {
	event := Event{
		ID: "evt-now", ProgramID: "p",
		PayloadDescriptors: []PayloadDescriptor{{PayloadType: "IMPORT_CAPACITY_LIMIT", Units: "KW"}},
		IntervalPeriod:     &IntervalPeriod{Start: "0001-01-01T00:00:00Z", Duration: "PT1H"},
		Intervals:          []Interval{{ID: 0, Payloads: []ValuesMap{{Type: "IMPORT_CAPACITY_LIMIT", Values: []any{2.0}}}}},
	}
	got := TranslateLimits([]EventInstance{{Event: event}}, NewDescriptorLookup(nil), now14)
	if got.ImpLimW == nil || *got.ImpLimW != 2000 {
		t.Fatalf("now-sentinel event not active: %+v", got)
	}
}
