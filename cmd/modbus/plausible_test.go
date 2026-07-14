package main

import (
	"math"
	"testing"
)

func TestPlausibleW(t *testing.T) {
	cases := []struct {
		name string
		w    float64
		maxW float64
		want bool
	}{
		{"half nameplate", 2400, 4800, true},
		{"at nameplate", 4800, 4800, true},
		{"within tolerance", 5600, 4800, true}, // 4800*1.2 = 5760
		{"just over tolerance", 5800, 4800, false},
		{"bad-scale 10x", 48000, 4800, false},
		{"negative within rating (battery charge)", -4000, 4800, true},
		{"negative bad-scale", -48000, 4800, false},
		{"unknown nameplate accepts", 48000, 0, true},
		{"NaN rejected", math.NaN(), 4800, false},
		{"Inf rejected", math.Inf(1), 4800, false},
	}
	for _, c := range cases {
		if got := plausibleW(c.w, c.maxW); got != c.want {
			t.Errorf("%s: plausibleW(%v, %v) = %v, want %v", c.name, c.w, c.maxW, got, c.want)
		}
	}
}

// TestWhMonotonicGate drives one device through the whole fault episode
// shape: baseline, normal advance, a non-monotonic sample (withheld, WARN
// edge once), the stuck-low continuation (withheld, NO second edge — flash
// budget), recovery past the last good value, and a fresh fault (new edge).
func TestWhMonotonicGate(t *testing.T) {
	g := newWhMonotonicGate()

	steps := []struct {
		name     string
		imp, exp float64
		wantImp  bool
		wantExp  bool
		wantEdge bool
	}{
		{"first sample baselines and publishes", 1000, 500, true, true, false},
		{"increase publishes", 1100, 500, true, true, false},
		{"equal publishes (non-decreasing, not strictly increasing)", 1100, 500, true, true, false},
		{"decrease withheld with one WARN edge", 110, 500, false, true, true},
		{"continued decrease stays withheld, no re-log", 111, 500, false, true, false},
		{"recovery at the last accepted value publishes again", 1100, 510, true, true, false},
		{"fresh fault after recovery logs a new edge", 90, 510, false, true, true},
	}
	for _, s := range steps {
		impOK, expOK, edge := g.admit("m0", s.imp, s.exp)
		if impOK != s.wantImp || expOK != s.wantExp || edge != s.wantEdge {
			t.Errorf("%s: admit(m0, %v, %v) = (imp %v, exp %v, edge %v), want (%v, %v, %v)",
				s.name, s.imp, s.exp, impOK, expOK, edge, s.wantImp, s.wantExp, s.wantEdge)
		}
	}
}

// TestWhMonotonicGateBaselineHoldsAcrossRejects pins the fail-closed baseline
// rule: a rejected sample must NOT lower the baseline, so a stuck-low scale
// factor stays withheld for the whole episode instead of being re-accepted
// one tick after the first reject. Restart is the only re-baseline
// (crash-only, AD-011) — newWhMonotonicGate() IS the restart in this test.
func TestWhMonotonicGateBaselineHoldsAcrossRejects(t *testing.T) {
	g := newWhMonotonicGate()
	g.admit("m0", 1000000, 0)

	if impOK, _, _ := g.admit("m0", 100000, 0); impOK {
		t.Fatal("decreased sample accepted, want withheld")
	}
	// Above the rejected sample but below the last ACCEPTED value: still
	// withheld — the reject did not become the new baseline.
	if impOK, _, _ := g.admit("m0", 100100, 0); impOK {
		t.Error("sample above rejected value but below baseline accepted, want withheld")
	}
	if impOK, _, _ := g.admit("m0", 1000050, 0); !impOK {
		t.Error("sample past the real baseline withheld, want accepted (recovery)")
	}

	// Process restart: fresh gate, the lower (post-reset) total re-baselines.
	g2 := newWhMonotonicGate()
	if impOK, _, _ := g2.admit("m0", 100100, 0); !impOK {
		t.Error("fresh gate rejected first sample, want accepted (restart re-baselines)")
	}
}

func TestWhMonotonicGateFieldAndDeviceIsolation(t *testing.T) {
	g := newWhMonotonicGate()
	g.admit("m0", 1000, 500)
	g.admit("m1", 2000, 700)

	// imp decreases while exp advances: only imp withheld.
	impOK, expOK, edge := g.admit("m0", 900, 600)
	if impOK || !expOK || !edge {
		t.Errorf("admit(m0, 900, 600) = (%v, %v, %v), want (false, true, true)", impOK, expOK, edge)
	}
	// m1 is untouched by m0's episode.
	impOK, expOK, edge = g.admit("m1", 2100, 800)
	if !impOK || !expOK || edge {
		t.Errorf("admit(m1, ...) = (%v, %v, %v), want (true, true, false)", impOK, expOK, edge)
	}
}

// TestWhMonotonicGateAbsentAndNonFinite: NaN is the absent-value convention —
// never publishable, never suspect, and it must not disturb the baseline. A
// non-finite Inf (defense in depth; the parse layer shouldn't produce one) is
// withheld the same way.
func TestWhMonotonicGateAbsentAndNonFinite(t *testing.T) {
	g := newWhMonotonicGate()
	nan := math.NaN()

	impOK, expOK, edge := g.admit("m0", nan, nan)
	if impOK || expOK || edge {
		t.Errorf("all-NaN admit = (%v, %v, %v), want (false, false, false) — absent is not a fault", impOK, expOK, edge)
	}

	g.admit("m0", 1000, 500)
	// A NaN gap (device briefly stopped reporting) leaves the baseline alone...
	if impOK, _, edge := g.admit("m0", nan, 500); impOK || edge {
		t.Errorf("NaN imp: (impOK %v, edge %v), want (false, false)", impOK, edge)
	}
	// ...so a later real sample is still judged against 1000.
	if impOK, _, _ := g.admit("m0", 999, 500); impOK {
		t.Error("post-gap decrease accepted, want withheld against the pre-gap baseline")
	}

	if impOK, _, _ := g.admit("m0", math.Inf(1), 500); impOK {
		t.Error("+Inf accepted, want withheld (non-finite)")
	}
}
