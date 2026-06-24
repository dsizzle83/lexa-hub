package main

import (
	"math"
	"testing"

	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/types"
)

func TestImplausibleCurrent(t *testing.T) {
	cases := []struct {
		name     string
		measured float64
		max      float64
		want     bool
	}{
		{"normal at half rating", 16, 32, false},
		{"at rating", 32, 32, false},
		{"slightly over within tolerance", 38, 32, false}, // 32*1.25 = 40
		{"just over tolerance", 41, 32, true},
		{"wrong-units mA-as-A 1000x", 6000, 32, true},
		{"unknown rating accepts anything", 6000, 0, false},
		{"NaN always implausible", math.NaN(), 32, true},
		{"Inf always implausible", math.Inf(1), 32, true},
	}
	for _, c := range cases {
		if got := implausibleCurrent(c.measured, c.max); got != c.want {
			t.Errorf("%s: implausibleCurrent(%v, %v) = %v, want %v", c.name, c.measured, c.max, got, c.want)
		}
	}
}

func currentSample(a float64) []types.MeterValue {
	return []types.MeterValue{{
		SampledValue: []types.SampledValue{{
			Value:     a,
			Measurand: types.MeasurandCurrentImport,
		}},
	}}
}

// A wrong-units current reading must be dropped, leaving the last good value in
// place rather than letting a 1000× value reach the optimizer.
func TestApplySamples_RejectsImplausibleCurrent(t *testing.T) {
	s := &stationState{id: "CS1", maxCurrentA: 32, voltageV: 230, soc: math.NaN()}

	applySamplesLocked(s, currentSample(16))
	if s.currentA != 16 {
		t.Fatalf("expected currentA=16 after a valid sample, got %v", s.currentA)
	}

	// 6000 A under an "A" label (mA mislabeled) — must be rejected.
	applySamplesLocked(s, currentSample(6000))
	if s.currentA != 16 {
		t.Errorf("implausible current was ingested: currentA=%v, want last-good 16", s.currentA)
	}

	// A subsequent valid reading is accepted again.
	applySamplesLocked(s, currentSample(10))
	if s.currentA != 10 {
		t.Errorf("expected currentA=10 after recovery, got %v", s.currentA)
	}
}
