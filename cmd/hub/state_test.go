package main

import (
	"math"
	"testing"
	"time"

	"lexa-hub/internal/bus"
)

// decodeAP mirrors the orchestrator's apW: value × 10^multiplier.
func decodeAP(value int16, multiplier int8) float64 {
	return float64(value) * math.Pow10(int(multiplier))
}

func TestWattsToActivePower(t *testing.T) {
	cases := []struct {
		name  string
		watts float64
		exact bool // expect lossless round-trip
	}{
		{"zero", 0, true},
		{"small", 5000, true},
		{"int16 max", 32767, true},
		{"just over int16", 32768, false},
		{"residential export limit", 50000, false},
		{"negative dispatch (absorb)", -50000, false},
		{"int16 min", -32768, true},
		{"odd value", 123456, false},
		{"feeder scale 20 MW", 20e6, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ap := wattsToActivePower(tc.watts)
			got := decodeAP(ap.Value, ap.Multiplier)

			// Error is bounded by half of one scale step.
			tol := 0.5 * math.Pow10(int(ap.Multiplier))
			if tc.exact {
				tol = 0
			}
			if math.Abs(got-tc.watts) > tol {
				t.Errorf("wattsToActivePower(%v) = {Value:%d Mult:%d} → %v, want within %v",
					tc.watts, ap.Value, ap.Multiplier, got, tol)
			}
		})
	}
}

// TestBusToCSIPControlLargeLimits is the C1 regression: limits ≥ 32.768 kW
// must survive the bus → CSIPControlState conversion intact, not wrap or
// saturate through a bare int16 cast.
func TestBusToCSIPControlLargeLimits(t *testing.T) {
	exp, imp, max, fixed := 50000.0, 40000.0, 100000.0, -75000.0
	cs := busToCSIPControl(&bus.ActiveControl{
		Source:  "event",
		MRID:    "test",
		ExpLimW: &exp,
		ImpLimW: &imp,
		MaxLimW: &max,
		FixedW:  &fixed,
	})
	if cs == nil {
		t.Fatal("busToCSIPControl returned nil for an event control")
	}

	check := func(name string, want float64, v int16, m int8) {
		t.Helper()
		got := decodeAP(v, m)
		if math.Abs(got-want) > 0.5*math.Pow10(int(m)) {
			t.Errorf("%s: got %v (Value:%d Mult:%d), want ≈%v", name, got, v, m, want)
		}
	}
	check("OpModExpLimW", exp, cs.Base.OpModExpLimW.Value, cs.Base.OpModExpLimW.Multiplier)
	check("OpModImpLimW", imp, cs.Base.OpModImpLimW.Value, cs.Base.OpModImpLimW.Multiplier)
	check("OpModMaxLimW", max, cs.Base.OpModMaxLimW.Value, cs.Base.OpModMaxLimW.Multiplier)
	check("OpModFixedW", fixed, cs.Base.OpModFixedW.Value, cs.Base.OpModFixedW.Multiplier)
}

// TestReadSystemState_ExpiredCSIPControlDropped is the C5 regression: the
// retained CSIP control must not be enforced past its ValidUntil when
// lexa-northbound stops refreshing it.
func TestReadSystemState_ExpiredCSIPControlDropped(t *testing.T) {
	r := newMQTTSystemReader(nil)
	expLim := 5000.0
	r.onCSIPControl("lexa/csip/control", bus.ActiveControl{
		Source:     "event",
		MRID:       "expired-evt",
		ExpLimW:    &expLim,
		ValidUntil: time.Now().Unix() - 10, // already past
	})

	state, err := r.ReadSystemState()
	if err != nil {
		t.Fatal(err)
	}
	if state.CSIPControl != nil {
		t.Errorf("expected expired CSIP control to be dropped, got source=%s mrid=%s",
			state.CSIPControl.Source, state.CSIPControl.MRID)
	}

	// The stored value is cleared, so subsequent reads stay clean (and the
	// expiry is logged once, not every tick).
	state, _ = r.ReadSystemState()
	if state.CSIPControl != nil {
		t.Error("expected CSIP control to stay nil on subsequent reads")
	}
}

func TestReadSystemState_ActiveCSIPControlKept(t *testing.T) {
	cases := []struct {
		name       string
		validUntil int64
	}{
		{"future expiry", time.Now().Unix() + 3600},
		{"no expiry (0)", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newMQTTSystemReader(nil)
			expLim := 5000.0
			r.onCSIPControl("lexa/csip/control", bus.ActiveControl{
				Source:     "event",
				MRID:       "live-evt",
				ExpLimW:    &expLim,
				ValidUntil: tc.validUntil,
			})
			state, err := r.ReadSystemState()
			if err != nil {
				t.Fatal(err)
			}
			if state.CSIPControl == nil {
				t.Fatal("expected live CSIP control to be kept")
			}
			if state.CSIPControl.MRID != "live-evt" {
				t.Errorf("MRID = %s, want live-evt", state.CSIPControl.MRID)
			}
		})
	}
}
