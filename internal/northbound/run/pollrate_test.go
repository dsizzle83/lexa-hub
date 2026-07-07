package run

import (
	"testing"
	"time"

	model "lexa-proto/csipmodel"

	"lexa-hub/internal/northbound/discovery"
)

// treeWithPoll builds a minimal discovery.ResourceTree carrying the given
// pollRate values on DeviceCapability, Time, and (if ctrlPoll > 0) a single
// DERProgram's DERControlList — enough surface for advertisedPollSeconds /
// effectiveInterval without needing a real walk.
func treeWithPoll(dcapPoll, timePoll, ctrlPoll uint32) *discovery.ResourceTree {
	tree := &discovery.ResourceTree{
		DeviceCapability: &model.DeviceCapability{PollRate: dcapPoll},
		Time:             &model.Time{PollRate: timePoll},
	}
	if ctrlPoll > 0 {
		tree.Programs = []discovery.ProgramState{
			{Controls: &model.DERControlList{PollRate: ctrlPoll}},
		}
	}
	return tree
}

func TestAdvertisedPollSeconds(t *testing.T) {
	tests := []struct {
		name string
		tree *discovery.ResourceTree
		want uint32
	}{
		{"nil fields", &discovery.ResourceTree{}, 0},
		{"dcap only", treeWithPoll(300, 0, 0), 300},
		{"max across classes (control fastest, time slowest)", treeWithPoll(300, 900, 60), 900},
		{"single program control rate wins when largest", treeWithPoll(60, 60, 900), 900},
		{"all zero (nothing advertised)", treeWithPoll(0, 0, 0), 0},
		{"multiple programs, max wins", &discovery.ResourceTree{
			Programs: []discovery.ProgramState{
				{Controls: &model.DERControlList{PollRate: 60}},
				{Controls: &model.DERControlList{PollRate: 1200}},
				{Controls: nil}, // a program with no DERControlList must not panic
			},
		}, 1200},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := advertisedPollSeconds(tc.tree)
			if got != tc.want {
				t.Errorf("advertisedPollSeconds() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestEffectiveInterval_OverrideModeAlwaysReturnsBase(t *testing.T) {
	base := 5 * time.Second
	tree := treeWithPoll(300, 900, 60) // would push honor mode way past base
	cfg := PollRateConfig{Mode: PollRateOverride}

	got := effectiveInterval(tree, base, cfg)
	if got != base {
		t.Errorf("override mode = %v, want unchanged base %v", got, base)
	}

	// FAST-bench override must be immune to what the tree advertises even
	// when the tree came from a REAL walk against gridsim's actual rates —
	// re-check with gridsim's documented values directly (300/900/60).
	gridsimTree := treeWithPoll(300, 900, 60)
	if got := effectiveInterval(gridsimTree, base, cfg); got != base {
		t.Errorf("override mode against gridsim-shaped tree = %v, want %v (bench cadence must survive honoring math entirely)", got, base)
	}
}

func TestEffectiveInterval_ZeroValueModeBehavesLikeOverride(t *testing.T) {
	// The Go zero value for PollRateConfig (Mode == "") must reproduce
	// pre-TASK-071 behavior — defense in depth for any caller that
	// constructs Discovery/PollRateConfig directly (e.g. tests) without
	// going through cmd/northbound's config loader.
	base := 20 * time.Second
	tree := treeWithPoll(300, 900, 60)
	got := effectiveInterval(tree, base, PollRateConfig{})
	if got != base {
		t.Errorf("zero-value Mode = %v, want base %v (must default to override, not honor)", got, base)
	}
}

func TestEffectiveInterval_NilTreeAlwaysReturnsBase(t *testing.T) {
	base := 20 * time.Second
	got := effectiveInterval(nil, base, PollRateConfig{Mode: PollRateHonor})
	if got != base {
		t.Errorf("honor mode with nil tree = %v, want base %v (no successful walk yet / walk failed)", got, base)
	}
}

func TestEffectiveInterval_HonorModeUsesMaxAdvertisedClampedToBase(t *testing.T) {
	// gridsim's documented rates (TASK-071 background): dcap/lists 300s,
	// /tm 900s, DERControlList 60s. Honor mode must walk no more often than
	// the SLOWEST of these (900s) — walking faster would over-poll /tm.
	base := 5 * time.Second
	tree := treeWithPoll(300, 900, 60)
	want := 900 * time.Second
	got := effectiveInterval(tree, base, PollRateConfig{Mode: PollRateHonor})
	if got != want {
		t.Errorf("honor mode against gridsim rates = %v, want %v (max of 300/900/60)", got, want)
	}
}

func TestEffectiveInterval_HonorModeFloorsAtBase(t *testing.T) {
	// A server advertising a pollRate FASTER than the operator's own
	// configured base interval must not speed the walk up past that floor
	// — base always wins as the floor.
	base := 30 * time.Second
	tree := treeWithPoll(5, 0, 0) // advertised 5s < base 30s
	got := effectiveInterval(tree, base, PollRateConfig{Mode: PollRateHonor})
	if got != base {
		t.Errorf("honor mode with sub-floor pollRate = %v, want floor %v", got, base)
	}
}

func TestEffectiveInterval_HonorModeNothingAdvertisedFallsBackToBase(t *testing.T) {
	base := 20 * time.Second
	tree := treeWithPoll(0, 0, 0) // XML omitempty case: nothing advertised anywhere
	got := effectiveInterval(tree, base, PollRateConfig{Mode: PollRateHonor})
	if got != base {
		t.Errorf("honor mode with no advertised pollRate anywhere = %v, want base %v", got, base)
	}
}

func TestEffectiveInterval_HonorModeClampsHostilePollRate(t *testing.T) {
	// 05 §3 plausibility discipline / TASK-071 "Common mistakes to avoid":
	// a hostile or malformed pollRate (e.g. 4e9 seconds ~= 126 years) must
	// not freeze discovery forever. clockMaxStaleness (15m) is tighter than
	// pollRateMaxInterval (24h) and always wins (see the dedicated
	// clockMaxStaleness-vs-larger-MaxInterval test below), so THIS is the
	// clamp a hostile value actually hits in practice.
	base := 20 * time.Second
	tree := treeWithPoll(4_000_000_000, 0, 0)
	got := effectiveInterval(tree, base, PollRateConfig{Mode: PollRateHonor})
	if got != clockMaxStaleness {
		t.Errorf("honor mode with hostile pollRate = %v, want clamp to clockMaxStaleness %v", got, clockMaxStaleness)
	}
}

func TestEffectiveInterval_HonorModeClampsHostilePollRate_CustomMaxIntervalBelowClock(t *testing.T) {
	// With an explicit MaxInterval tighter than clockMaxStaleness, THAT one
	// governs instead — exercising the other branch of the min(maxI,
	// clockMaxStaleness) clamp.
	base := 20 * time.Second
	tree := treeWithPoll(4_000_000_000, 0, 0)
	cfg := PollRateConfig{Mode: PollRateHonor, MaxInterval: 2 * time.Minute}
	got := effectiveInterval(tree, base, cfg)
	if got != 2*time.Minute {
		t.Errorf("honor mode with hostile pollRate and MaxInterval=2m = %v, want 2m", got)
	}
}

func TestEffectiveInterval_ClockMaxStalenessWinsOverLargerMaxInterval(t *testing.T) {
	// "Things that must NOT change": clock resync max-staleness stays <=15
	// min in every mode, even if a caller supplies a larger MaxInterval, and
	// even though gridsim's own /tm pollRate (900s) happens to equal it —
	// the enforcement must be independent of that coincidence. Use a
	// pollRate well past 15 min to prove the clamp is really 15 min, not
	// just "whatever gridsim happens to advertise".
	base := 20 * time.Second
	tree := treeWithPoll(2*60*60, 0, 0) // 2h advertised
	cfg := PollRateConfig{Mode: PollRateHonor, MaxInterval: 6 * time.Hour}
	got := effectiveInterval(tree, base, cfg)
	if got != clockMaxStaleness {
		t.Errorf("honor mode with 2h pollRate and 6h MaxInterval = %v, want clockMaxStaleness %v (must win regardless of MaxInterval)", got, clockMaxStaleness)
	}
}

func TestEffectiveInterval_HonorModeWithinBounds(t *testing.T) {
	// A pollRate comfortably between base and the clamps passes through
	// unchanged.
	base := 5 * time.Second
	tree := treeWithPoll(120, 0, 0)
	want := 120 * time.Second
	got := effectiveInterval(tree, base, PollRateConfig{Mode: PollRateHonor})
	if got != want {
		t.Errorf("honor mode mid-range pollRate = %v, want %v (pass-through, no clamp should apply)", got, want)
	}
}

func TestEffectiveInterval_NonPositiveBaseDefensiveFallback(t *testing.T) {
	// Callers should never pass base<=0 (cmd/northbound's DiscoveryInterval
	// always derives from a positive discovery_interval_s default), but
	// effectiveInterval must not hang/spin the loop on a misconfiguration —
	// verify it substitutes a small positive value rather than returning
	// zero (which would busy-loop time.Timer).
	got := effectiveInterval(nil, 0, PollRateConfig{Mode: PollRateOverride})
	if got <= 0 {
		t.Errorf("effectiveInterval with base=0 = %v, want a positive fallback", got)
	}
}

// TestDiscovery_NextInterval_WiresLastTreeAndPollCfg exercises the
// Discovery.nextInterval helper directly (not through a real Loop/RunOnce),
// confirming it reads pollCfg/lastTree the way Loop depends on: honoring
// once a successful tree is cached, and reproducing override/zero-value
// behavior identically to the free function above.
func TestDiscovery_NextInterval_WiresLastTreeAndPollCfg(t *testing.T) {
	base := 5 * time.Second

	d := &Discovery{pollCfg: PollRateConfig{Mode: PollRateOverride}}
	d.lastTree = treeWithPoll(300, 900, 60)
	if got := d.nextInterval(base); got != base {
		t.Errorf("override Discovery.nextInterval = %v, want base %v", got, base)
	}

	d2 := &Discovery{pollCfg: PollRateConfig{Mode: PollRateHonor}}
	if got := d2.nextInterval(base); got != base {
		t.Errorf("honor Discovery.nextInterval with no successful walk yet = %v, want base %v", got, base)
	}
	d2.lastTree = treeWithPoll(300, 900, 60)
	if got, want := d2.nextInterval(base), 900*time.Second; got != want {
		t.Errorf("honor Discovery.nextInterval after a successful walk = %v, want %v", got, want)
	}
}
