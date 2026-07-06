package constraint

import (
	"math"
	"testing"

	"lexa-hub/internal/orchestrator"
)

func TestTierResolutionOrder(t *testing.T) {
	// Safety resolves before compliance before economics (ascending numeric).
	if !(TierSafety < TierCompliance && TierCompliance < TierEconomics) {
		t.Fatalf("tier order wrong: safety=%d compliance=%d economics=%d",
			TierSafety, TierCompliance, TierEconomics)
	}
	for tier, want := range map[Tier]string{
		TierSafety:     "safety",
		TierCompliance: "compliance",
		TierEconomics:  "economics",
		Tier(99):       "unknown",
	} {
		if got := tier.String(); got != want {
			t.Errorf("Tier(%d).String()=%q want %q", tier, got, want)
		}
	}
}

func TestAxisString(t *testing.T) {
	for axis, want := range map[Axis]string{
		AxisSolarCeilingW:    "solar-ceiling-w",
		AxisBatterySetpointW: "battery-setpoint-w",
		AxisEVSECurrentA:     "evse-current-a",
		AxisConnect:          "connect",
		Axis(42):             "unknown-axis",
	} {
		if got := axis.String(); got != want {
			t.Errorf("Axis(%d).String()=%q want %q", axis, got, want)
		}
	}
}

func TestDemandBuilders(t *testing.T) {
	c := CeilingDemand("inv1", AxisSolarCeilingW, 4000, TierCompliance, "export")
	if !math.IsNaN(c.Min) || c.Max != 4000 || c.Tier != TierCompliance || c.Source != "export" {
		t.Errorf("CeilingDemand wrong: %+v", c)
	}
	p := PointDemand("bat1", AxisBatterySetpointW, -1500, TierEconomics, "self-use")
	if p.Min != -1500 || p.Max != -1500 {
		t.Errorf("PointDemand not pinned: %+v", p)
	}
	d := ConnectDemand("bat1", false, TierSafety, "disconnect")
	if d.Connect == nil || *d.Connect != false || d.Axis != AxisConnect {
		t.Errorf("ConnectDemand wrong: %+v", d)
	}
}

func TestDetectionWindowTicks(t *testing.T) {
	cases := []struct {
		name                 string
		control, meter, tick float64
		want                 int
	}{
		// Bench defaults reproduce the legacy exportBreachTicks=3 (~9 s @ 3 s).
		{"bench-default", 3, 5, 3, 3},
		// A slower plant grows the window (the point of the adaptive derivation).
		{"slow-plant", 10, 20, 3, 10},
		// STOCK cadence: 8 s of window is under one tick → floored at 2.
		{"stock-floor", 3, 5, 15, 2},
		// Zero latencies → floored at 2 (single-glitch tolerance).
		{"zero", 0, 0, 3, 2},
		// tickSeconds<=0 defaults to the tuned 3 s cadence.
		{"tick-default", 3, 5, 0, 3},
		// Negative inputs are clamped to 0, not subtracted.
		{"negative-clamped", -100, 5, 3, 2},
	}
	for _, tc := range cases {
		if got := DetectionWindowTicks(tc.control, tc.meter, tc.tick); got != tc.want {
			t.Errorf("%s: DetectionWindowTicks(%v,%v,%v)=%d want %d",
				tc.name, tc.control, tc.meter, tc.tick, got, tc.want)
		}
	}
}

func TestExportDetectionWindowTicks(t *testing.T) {
	p := Plant{
		Inverters: map[string]orchestrator.InverterPlant{
			"inv1": {ControlLatencyS: 3},
		},
		Meter: orchestrator.MeterPlant{MeterLagS: 5},
	}
	if got := p.ExportDetectionWindowTicks("inv1", 3); got != 3 {
		t.Errorf("inv1 window=%d want 3", got)
	}
	// Absent inverter → zero control latency, meter lag only, floor applies.
	if got := p.ExportDetectionWindowTicks("missing", 3); got != 2 {
		t.Errorf("missing window=%d want 2 (ceil(5/3)=2)", got)
	}
}
