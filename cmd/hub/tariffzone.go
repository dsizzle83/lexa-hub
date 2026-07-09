package main

// tariffzone.go implements WS-8 (V1.0 punch list, TASK-079/GAP-05): a
// startup-only assertion that the process's configured time zone
// (time.Local, i.e. the SOM's /etc/localtime) matches the zone the "tariff_zone"
// config key names. TOUCostModel/planner (internal/orchestrator/costmodel.go,
// planner.go) evaluate TOU boundaries via t.Hour() in whatever Location the
// caller's time.Time carries — correct only when the process zone IS the
// tariff's zone (utility tariffs are defined in local clock time; see
// CLAUDE.md "SOM zone must match the tariff zone"). This file adds NO control
// behavior: it only logs loudly and exports a gauge on mismatch, so an
// operator or monitoring system catches a misconfigured SOM before it silently
// misprices/mistimes every evening.
import (
	"log/slog"
	"time"

	"lexa-hub/internal/metrics"
)

// tariffZoneSampleMonths are the month offsets (from "now") sampled when
// comparing time.Local's offset behavior against the configured tariff zone.
// Four quarterly samples across a year catch both DST states (and any zone
// whose DST rule differs from time.Local's, even if the two happen to share
// an offset today) without needing a real calendar of every transition.
var tariffZoneSampleMonths = []int{0, 3, 6, 9}

// tariffZoneMatches reports whether procLoc renders the same UTC offset as
// tariffLoc at every sampled instant derived from now — which is exactly
// what matters for TOUCostModel/planner's Hour()-keyed boundary evaluation:
// equal offsets at every sampled instant means Hour() (and therefore every
// TOU decision) renders identically in either zone. now/procLoc are
// parameters (rather than time.Now()/time.Local read inline) so this is
// deterministic and unit-testable with explicit locations, independent of
// whatever zone the test runner itself carries (TASK-079's testing
// convention — see costmodel_test.go).
func tariffZoneMatches(now time.Time, procLoc, tariffLoc *time.Location) bool {
	for _, months := range tariffZoneSampleMonths {
		t := now.AddDate(0, months, 0)
		_, procOff := t.In(procLoc).Zone()
		_, tariffOff := t.In(tariffLoc).Zone()
		if procOff != tariffOff {
			return false
		}
	}
	return true
}

// checkTariffZone runs the WS-8 startup assertion and sets the
// lexa_tariff_zone_mismatch gauge (0 = match/disabled, 1 = mismatch/invalid).
// It never returns an error and never changes control behavior — see the
// package doc above.
func checkTariffZone(cfg *Config, reg *metrics.Registry) {
	gauge := reg.Gauge("lexa_tariff_zone_mismatch")
	if cfg.TariffZone == "" {
		slog.Warn("lexa-hub: tariff_zone not configured — SOM-zone/tariff-zone match check disabled (TASK-079/WS-8); TOU pricing decisions will silently follow whatever zone this process's /etc/localtime carries")
		gauge.Set(0)
		return
	}
	loc, err := time.LoadLocation(cfg.TariffZone)
	if err != nil {
		slog.Error("lexa-hub: tariff_zone is invalid — cannot verify this SOM's zone matches the tariff zone (TASK-079/WS-8)",
			"tariff_zone", cfg.TariffZone, "err", err)
		gauge.Set(1)
		return
	}
	if tariffZoneMatches(time.Now(), time.Local, loc) {
		slog.Info("lexa-hub: SOM process zone matches configured tariff_zone", "tariff_zone", cfg.TariffZone)
		gauge.Set(0)
		return
	}
	slog.Error("lexa-hub: SOM PROCESS TIME ZONE DOES NOT MATCH THE CONFIGURED TARIFF ZONE — TOU pricing and autonomous peak-shift discharge will be MISPRICED and MIS-TIMED every day until this is fixed (TASK-079/WS-8/GAP-05; see CLAUDE.md 'SOM zone must match the tariff zone')",
		"tariff_zone", cfg.TariffZone, "process_zone_name", time.Local.String())
	gauge.Set(1)
}
