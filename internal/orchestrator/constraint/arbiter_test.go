package constraint

import (
	"fmt"
	"math"
	"sort"
	"testing"
)

func nan() float64 { return math.NaN() }

// eqBound compares interval fields treating NaN as equal to NaN.
func eqBound(a, b float64) bool {
	if math.IsNaN(a) && math.IsNaN(b) {
		return true
	}
	return a == b
}

func TestResolve_IntersectionLowestCeilingWins(t *testing.T) {
	// Two compliance ceilings on the same inverter → the tighter one wins.
	demands := []Demand{
		CeilingDemand("inv1", AxisSolarCeilingW, 4000, TierCompliance, "export"),
		CeilingDemand("inv1", AxisSolarCeilingW, 2500, TierCompliance, "generation"),
	}
	got := Resolve(demands)
	iv := got["inv1"].Bounds[AxisSolarCeilingW]
	if iv.Max != 2500 || !math.IsNaN(iv.Min) {
		t.Fatalf("ceiling=%+v want {NaN,2500}", iv)
	}
	if len(got["inv1"].Conflicts) != 0 {
		t.Errorf("unexpected conflicts: %+v", got["inv1"].Conflicts)
	}
}

func TestResolve_NarrowingOnly(t *testing.T) {
	// Economics tries to raise a ceiling a higher tier already set — it must NOT
	// be able to widen it. This is the AD-007 invariant, enforced structurally.
	demands := []Demand{
		CeilingDemand("inv1", AxisSolarCeilingW, 5000, TierEconomics, "cost"),
		CeilingDemand("inv1", AxisSolarCeilingW, 2000, TierCompliance, "export"),
		CeilingDemand("inv1", AxisSolarCeilingW, 1500, TierSafety, "protect"),
	}
	iv := Resolve(demands)["inv1"].Bounds[AxisSolarCeilingW]
	if iv.Max != 1500 {
		t.Fatalf("ceiling widened to %v; safety 1500 must win", iv.Max)
	}
}

func TestResolve_PointPinnedInsideInterval(t *testing.T) {
	// Safety bounds the battery to [-1000,1000]; economics pins 500 W discharge.
	c := true
	demands := []Demand{
		{Device: "bat1", Axis: AxisBatterySetpointW, Min: -1000, Max: 1000, Tier: TierSafety, Source: "protect"},
		PointDemand("bat1", AxisBatterySetpointW, 500, TierEconomics, "tou"),
		{Device: "bat1", Axis: AxisConnect, Connect: &c, Tier: TierEconomics, Source: "tou"},
	}
	des := Resolve(demands)["bat1"]
	iv := des.Bounds[AxisBatterySetpointW]
	if iv.Min != 500 || iv.Max != 500 {
		t.Fatalf("battery interval=%+v want pinned 500", iv)
	}
	if des.Connect == nil || *des.Connect != true {
		t.Errorf("connect=%v want true", des.Connect)
	}
}

func TestResolve_SameTierIntervalConflict(t *testing.T) {
	// Same tier, non-intersecting bounds: a ceiling of 1000 and a floor of 2000.
	// Most-restrictive (lowest ceiling) wins and a conflict is recorded.
	demands := []Demand{
		{Device: "inv1", Axis: AxisSolarCeilingW, Min: nan(), Max: 1000, Tier: TierCompliance, Source: "a-ceiling"},
		{Device: "inv1", Axis: AxisSolarCeilingW, Min: 2000, Max: nan(), Tier: TierCompliance, Source: "b-floor"},
	}
	des := Resolve(demands)["inv1"]
	if des.Bounds[AxisSolarCeilingW].Max != 1000 {
		t.Fatalf("ceiling=%v want 1000 (most restrictive)", des.Bounds[AxisSolarCeilingW].Max)
	}
	if len(des.Conflicts) != 1 {
		t.Fatalf("conflicts=%d want 1", len(des.Conflicts))
	}
	if des.Conflicts[0].Tier != TierCompliance || des.Conflicts[0].Axis != AxisSolarCeilingW {
		t.Errorf("conflict metadata wrong: %+v", des.Conflicts[0])
	}
}

func TestResolve_ConnectFalseBeatsTrue(t *testing.T) {
	tt := true
	ff := false
	t.Run("cross-tier-false-narrows", func(t *testing.T) {
		// safety true, economics false: disconnect is a valid narrowing → false
		// wins, no conflict (different tiers).
		des := Resolve([]Demand{
			{Device: "bat1", Axis: AxisConnect, Connect: &tt, Tier: TierSafety, Source: "s"},
			{Device: "bat1", Axis: AxisConnect, Connect: &ff, Tier: TierEconomics, Source: "e"},
		})["bat1"]
		if des.Connect == nil || *des.Connect != false {
			t.Fatalf("connect=%v want false", des.Connect)
		}
		if len(des.Conflicts) != 0 {
			t.Errorf("unexpected conflict: %+v", des.Conflicts)
		}
	})
	t.Run("same-tier-conflict", func(t *testing.T) {
		des := Resolve([]Demand{
			{Device: "bat1", Axis: AxisConnect, Connect: &tt, Tier: TierCompliance, Source: "keep"},
			{Device: "bat1", Axis: AxisConnect, Connect: &ff, Tier: TierCompliance, Source: "drop"},
		})["bat1"]
		if des.Connect == nil || *des.Connect != false {
			t.Fatalf("connect=%v want false", des.Connect)
		}
		if len(des.Conflicts) != 1 || des.Conflicts[0].Axis != AxisConnect {
			t.Fatalf("want 1 connect conflict, got %+v", des.Conflicts)
		}
	})
}

// serialize renders a Resolve result to a stable string for determinism tests.
func serialize(m map[string]Desired) string {
	devs := make([]string, 0, len(m))
	for d := range m {
		devs = append(devs, d)
	}
	sort.Strings(devs)
	var s string
	for _, dev := range devs {
		d := m[dev]
		s += dev + ":"
		for _, axis := range axisOrder {
			if iv, ok := d.Bounds[axis]; ok {
				s += fmt.Sprintf("[%s %.3f/%.3f]", axis, iv.Min, iv.Max)
			}
		}
		if d.Connect != nil {
			s += fmt.Sprintf("conn=%v", *d.Connect)
		}
		for _, c := range d.Conflicts {
			s += fmt.Sprintf("{cf %s %s %v}", c.Axis, c.Tier, c.Sources)
		}
		s += ";"
	}
	return s
}

func TestResolve_Deterministic(t *testing.T) {
	build := func() []Demand {
		tt := true
		ff := false
		return []Demand{
			CeilingDemand("inv2", AxisSolarCeilingW, 3000, TierEconomics, "z-cost"),
			CeilingDemand("inv1", AxisSolarCeilingW, 2000, TierCompliance, "export"),
			CeilingDemand("inv1", AxisSolarCeilingW, 2500, TierCompliance, "gen"),
			{Device: "bat1", Axis: AxisConnect, Connect: &tt, Tier: TierCompliance, Source: "a"},
			{Device: "bat1", Axis: AxisConnect, Connect: &ff, Tier: TierCompliance, Source: "b"},
			PointDemand("bat1", AxisBatterySetpointW, -1200, TierEconomics, "self-use"),
			CeilingDemand("evse1", AxisEVSECurrentA, 16, TierCompliance, "import"),
		}
	}
	want := serialize(Resolve(build()))
	for i := 0; i < 1000; i++ {
		if got := serialize(Resolve(build())); got != want {
			t.Fatalf("run %d nondeterministic:\n got=%s\nwant=%s", i, got, want)
		}
	}
}

func TestResolve_Empty(t *testing.T) {
	if got := Resolve(nil); len(got) != 0 {
		t.Errorf("Resolve(nil)=%v want empty", got)
	}
}

// ── FIX-F: axis authorship ──────────────────────────────────────────────────

// A single demand on an axis is trivially its own author.
func TestResolve_AuthorSingleDemand(t *testing.T) {
	des := Resolve([]Demand{
		CeilingDemand("inv1", AxisSolarCeilingW, 3000, TierCompliance, "export"),
	})["inv1"]
	if a, ok := des.Author(AxisSolarCeilingW); !ok || a != "export" {
		t.Fatalf("author=%q ok=%v want export/true", a, ok)
	}
}

// The tighter of two same-tier ceilings is the author (mirrors
// TestResolve_IntersectionLowestCeilingWins's value assertion, authorship
// side).
func TestResolve_AuthorSameTierTighterWins(t *testing.T) {
	des := Resolve([]Demand{
		CeilingDemand("inv1", AxisSolarCeilingW, 4000, TierCompliance, "export"),
		CeilingDemand("inv1", AxisSolarCeilingW, 2500, TierCompliance, "generation"),
	})["inv1"]
	if a, _ := des.Author(AxisSolarCeilingW); a != "generation" {
		t.Fatalf("author=%q want generation (the tighter 2500 ceiling)", a)
	}
}

// A higher tier that a lower tier's demand does not actually narrow keeps its
// OWN author — the lower tier's redundant/wider demand never claims
// authorship it did not earn (mirrors TestResolve_NarrowingOnly).
func TestResolve_AuthorHigherTierKeepsCreditWhenNotNarrowed(t *testing.T) {
	des := Resolve([]Demand{
		CeilingDemand("inv1", AxisSolarCeilingW, 5000, TierEconomics, "cost"),
		CeilingDemand("inv1", AxisSolarCeilingW, 2000, TierCompliance, "export"),
		CeilingDemand("inv1", AxisSolarCeilingW, 1500, TierSafety, "protect"),
	})["inv1"]
	if a, _ := des.Author(AxisSolarCeilingW); a != "protect" {
		t.Fatalf("author=%q want protect (safety 1500 is the tightest, real, surviving bound)", a)
	}
}

// A lower tier that DOES narrow further than the higher tier's bound becomes
// the author — narrowing (not merely existing at a lower tier) is what
// transfers credit.
func TestResolve_AuthorLowerTierTakesCreditWhenItNarrows(t *testing.T) {
	des := Resolve([]Demand{
		CeilingDemand("inv1", AxisSolarCeilingW, 3000, TierCompliance, "export"),
		CeilingDemand("inv1", AxisSolarCeilingW, 1200, TierEconomics, "cost"),
	})["inv1"]
	if a, _ := des.Author(AxisSolarCeilingW); a != "cost" {
		t.Fatalf("author=%q want cost (1200 narrows inside compliance's 3000)", a)
	}
}

// When the higher tier wins outright on a cross-tier conflict (the lower
// tier's demand is infeasible inside the higher tier's interval and gets
// clamped away), the higher tier's author must survive unchanged.
func TestResolve_AuthorHigherTierWinsOutrightKeepsAuthor(t *testing.T) {
	des := Resolve([]Demand{
		PointDemand("bat1", AxisBatterySetpointW, 1700, TierCompliance, "import"),
		PointDemand("bat1", AxisBatterySetpointW, -3000, TierEconomics, "self-use"),
	})["bat1"]
	if a, _ := des.Author(AxisBatterySetpointW); a != "import" {
		t.Fatalf("author=%q want import (compliance wins outright over the contradictory economics point)", a)
	}
}

// A point demand strictly INSIDE the higher tier's interval is a real
// narrowing (Min==Max collapses the interval to a single value), so it takes
// authorship — the normal "economics proposes a point the caps allow" case.
func TestResolve_AuthorPointInsideIntervalTakesCredit(t *testing.T) {
	des := Resolve([]Demand{
		{Device: "bat1", Axis: AxisBatterySetpointW, Min: -1000, Max: 1000, Tier: TierSafety, Source: "protect"},
		PointDemand("bat1", AxisBatterySetpointW, 500, TierEconomics, "tou"),
	})["bat1"]
	if a, _ := des.Author(AxisBatterySetpointW); a != "tou" {
		t.Fatalf("author=%q want tou (the in-range point narrows [-1000,1000] to {500})", a)
	}
}

// Connect axis: false (disconnect) wins and its source is the author, even
// when a higher tier merely held true (which false narrows, not conflicts
// with).
func TestResolve_AuthorConnectFalseSource(t *testing.T) {
	tt, ff := true, false
	des := Resolve([]Demand{
		{Device: "bat1", Axis: AxisConnect, Connect: &tt, Tier: TierSafety, Source: "hold-connect"},
		{Device: "bat1", Axis: AxisConnect, Connect: &ff, Tier: TierEconomics, Source: "battery-safety"},
	})["bat1"]
	if a, ok := des.Author(AxisConnect); !ok || a != "battery-safety" {
		t.Fatalf("connect author=%q ok=%v want battery-safety/true", a, ok)
	}
}

// An axis no demand touched has no author.
func TestResolve_AuthorAbsentWhenNoDemand(t *testing.T) {
	des := Resolve([]Demand{
		CeilingDemand("inv1", AxisSolarCeilingW, 3000, TierCompliance, "export"),
	})["inv1"]
	if _, ok := des.Author(AxisBatterySetpointW); ok {
		t.Fatalf("battery axis has no demand; Author must report ok=false")
	}
}
