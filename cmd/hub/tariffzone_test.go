package main

import (
	"strings"
	"testing"
	"time"

	"lexa-hub/internal/metrics"
)

// WS-8 (V1.0 punch list, TASK-079/GAP-05): additive-only startup assertion
// that the process zone matches the configured tariff_zone. Per TASK-079's
// testing convention (costmodel_test.go), tariffZoneMatches takes explicit
// procLoc/tariffLoc locations rather than reading the global time.Local, so
// these tests are independent of whatever zone the test runner itself
// carries and pass under any TZ.

func mustLoadLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("%s not available in this environment: %v", name, err)
	}
	return loc
}

func TestTariffZoneMatches_SameZone(t *testing.T) {
	la := mustLoadLocation(t, "America/Los_Angeles")
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	if !tariffZoneMatches(now, la, la) {
		t.Fatal("tariffZoneMatches(now, la, la) = false, want true (same zone both sides)")
	}
}

func TestTariffZoneMatches_DifferentZone_Mismatch(t *testing.T) {
	// Pacific vs. Eastern: both observe US DST on the same schedule, offset
	// by exactly one hour year-round, so every quarterly sample mismatches.
	la := mustLoadLocation(t, "America/Los_Angeles")
	ny := mustLoadLocation(t, "America/New_York")
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	if tariffZoneMatches(now, la, ny) {
		t.Fatal("tariffZoneMatches(now, la, ny) = true, want false (LA and NY differ year-round)")
	}
}

func TestTariffZoneMatches_UTCAlwaysDiffersFromDSTZone(t *testing.T) {
	// UTC never matches a DST-observing zone's summer offset — this is the
	// exact "TZ=UTC SOM serving a local-clock tariff" deployment hazard
	// TASK-079's TestTOU_UTCvsLA_Divergence_DeploymentHazard documents.
	la := mustLoadLocation(t, "America/Los_Angeles")
	summer := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	if tariffZoneMatches(summer, time.UTC, la) {
		t.Fatal("tariffZoneMatches(summer, UTC, la) = true, want false")
	}
}

func TestTariffZoneMatches_DSTRuleDifference_SameInstantOffsetInsufficient(t *testing.T) {
	// Arizona (no DST, always UTC-7) shares ITS offset with Pacific Time's
	// SUMMER offset (PDT, UTC-7) but diverges every winter (PST, UTC-8) — a
	// single-instant check anchored in summer would wrongly call this a
	// match; the year-round quarterly sample must catch the winter
	// divergence even though the passed instant itself is mid-summer.
	phoenix := mustLoadLocation(t, "America/Phoenix")
	la := mustLoadLocation(t, "America/Los_Angeles")
	summer := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	_, phoenixOff := summer.In(phoenix).Zone()
	_, laOff := summer.In(la).Zone()
	if phoenixOff != laOff {
		t.Skip("test setup assumption (Phoenix/LA share summer offset) does not hold in this Go tzdata version")
	}
	if tariffZoneMatches(summer, phoenix, la) {
		t.Fatal("tariffZoneMatches(summer, phoenix, la) = true, want false (they diverge in winter — the year-round sample must catch it even though the passed instant is summer)")
	}
}

func TestCheckTariffZone_Empty_DisabledZeroGauge(t *testing.T) {
	reg := metrics.New()
	cfg := &Config{TariffZone: ""}
	checkTariffZone(cfg, reg)
	assertGauge(t, reg, "lexa_tariff_zone_mismatch", 0)
}

func TestCheckTariffZone_InvalidZone_MismatchGauge(t *testing.T) {
	reg := metrics.New()
	cfg := &Config{TariffZone: "Not/A_Real_Zone"}
	checkTariffZone(cfg, reg)
	assertGauge(t, reg, "lexa_tariff_zone_mismatch", 1)
}

func TestCheckTariffZone_MatchesProcessZone_ZeroGauge(t *testing.T) {
	// time.Local IS the process zone checkTariffZone reads; the only
	// zone name guaranteed to match it in any environment is "Local"
	// itself (time.LoadLocation special-cases that name to return
	// time.Local unchanged) or, when the runner happens to run UTC,
	// "UTC" — try both rather than assuming a specific runner TZ.
	reg := metrics.New()
	name := "UTC"
	if time.Local != time.UTC {
		name = "Local"
	}
	cfg := &Config{TariffZone: name}
	checkTariffZone(cfg, reg)
	assertGauge(t, reg, "lexa_tariff_zone_mismatch", 0)
}

// assertGauge greps the registry's rendered exposition text for the exact
// "name value" line — Format() is the only exported read path (Gauge.value
// is unexported to this package).
func assertGauge(t *testing.T, reg *metrics.Registry, name string, want float64) {
	t.Helper()
	out := reg.Format()
	var wantLine string
	if want == 0 {
		wantLine = name + " 0\n"
	} else {
		wantLine = name + " 1\n"
	}
	if !strings.Contains(out, wantLine) {
		t.Fatalf("Format() does not contain %q; got:\n%s", wantLine, out)
	}
}
