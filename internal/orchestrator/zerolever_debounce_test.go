package orchestrator_test

import (
	"testing"

	"lexa-hub/internal/orchestrator"
	model "lexa-proto/csipmodel"
)

// A no-lever export scenario: PV over a 0 W export cap, the battery FULL (cannot
// absorb) and no load or EV — so the ceiling controller curtails solar to ~0 in
// one tick (the feed-forward one-step correction) while the meter still shows
// export. This is exactly the state the export rule's zero-lever breach targets,
// and the one a brief meter-noise blip fakes for a tick or two.
func noLeverExportState() orchestrator.SystemState {
	s := state0()
	s.Solar = []orchestrator.SolarState{solar("pv", 4500, 5000)}
	s.Batteries = []orchestrator.BatteryState{battery("bat", 0, 96, 5000)} // SOC 96 > full threshold ⇒ cannot absorb
	s.Grid.NetW = -4500                                                    // exporting 4500 W against a 0 W cap
	s.CSIPControl = &orchestrator.CSIPControlState{
		Source: "event", MRID: "zl-mrid",
		Base: model.DERControlBase{OpModExpLimW: ap(0)},
	}
	return s
}

// TestOptimizer_ZeroLeverBreach_TransientNoBreach is the audit-D3 regression: a
// 1-2-tick no-lever excursion (the shape a meter-noise blip under a tight cap
// produces) must NOT post a CannotComply. Before the debounce the zero-lever
// path fired on the very first such tick.
func TestOptimizer_ZeroLeverBreach_TransientNoBreach(t *testing.T) {
	opt := newOpt()
	// Two no-lever ticks (counter reaches 2, below the exportBreachTicks=3 trip).
	for i := 0; i < 2; i++ {
		p := opt.Optimize(noLeverExportState())
		if p.Breach != nil {
			t.Fatalf("tick %d: a %d-tick no-lever transient must not breach (spurious CannotComply); breach=%+v", i, i+1, p.Breach)
		}
	}
	// Relief: the battery regains headroom and export falls under the cap, so the
	// leaky counter drains. No breach may appear.
	relief := state0()
	relief.Solar = []orchestrator.SolarState{solar("pv", 300, 5000)}
	relief.Batteries = []orchestrator.BatteryState{battery("bat", 0, 50, 5000)}
	relief.Grid.NetW = -50 // exporting 50 W, well under the 0-ish… actually under cap+band
	relief.CSIPControl = &orchestrator.CSIPControlState{
		Source: "event", MRID: "zl-mrid", Base: model.DERControlBase{OpModExpLimW: ap(500)},
	}
	for i := 0; i < 3; i++ {
		if p := opt.Optimize(relief); p.Breach != nil {
			t.Fatalf("relief tick %d: counter must drain to no breach, got %+v", i, p.Breach)
		}
	}
}

// TestOptimizer_ZeroLeverBreach_SustainedFires: a genuinely stuck no-lever
// episode still escalates to a CannotComply — but only after the debounce
// window, never on the first tick.
func TestOptimizer_ZeroLeverBreach_SustainedFires(t *testing.T) {
	opt := newOpt()
	firstBreach := -1
	var last orchestrator.Plan
	for i := 0; i < 6; i++ {
		last = opt.Optimize(noLeverExportState())
		if last.Breach != nil && firstBreach < 0 {
			firstBreach = i
		}
	}
	if firstBreach < 0 {
		t.Fatal("a sustained no-lever export episode must escalate to a CannotComply")
	}
	if firstBreach == 0 {
		t.Fatal("the breach fired on the FIRST tick — the zero-lever path is still single-sample (audit D3 not fixed)")
	}
	if last.Breach.LimitType != "export" {
		t.Errorf("breach LimitType = %q, want export", last.Breach.LimitType)
	}
	if last.Breach.MRID != "zl-mrid" {
		t.Errorf("breach MRID = %q, want zl-mrid (northbound needs it to address the Response)", last.Breach.MRID)
	}
}
