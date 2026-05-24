package bus

import (
	"encoding/json"
	"math"
	"testing"
)

// TestBusMessagesNaNRoundTrip verifies that all bus message types with
// *float64 fields marshal to valid JSON when those fields are nil (the
// correct representation of "not reported" / NaN at the producer level).
//
// The invariant: producers must pass nil, not &NaN, for absent values.
// Types that previously used bare float64 fields allowed NaN to silently
// reach json.Marshal, causing a silent publish failure. The *float64 type
// enforces the producer to make "nil vs value" explicit.
func TestBusMessagesNaNRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		msg  any
	}{
		{
			name: "BattMetrics/nil max power fields",
			msg: BattMetrics{
				Device:        "bat0",
				MaxChargeW:    nil, // not reported by this device
				MaxDischargeW: nil,
				Ts:            1,
			},
		},
		{
			name: "BattMetrics/non-nil max power fields",
			msg: BattMetrics{
				Device:        "bat0",
				MaxChargeW:    float64ptr(5000),
				MaxDischargeW: float64ptr(5000),
				Ts:            1,
			},
		},
		{
			name: "EVSEState/nil numeric fields (no session)",
			msg: EVSEState{
				StationID:   "ev0",
				CurrentA:    nil,
				MaxCurrentA: nil,
				VoltageV:    nil,
				PowerW:      nil,
				EnergyWh:    nil,
				Ts:          1,
			},
		},
		{
			name: "EVSEState/non-nil numeric fields",
			msg: EVSEState{
				StationID:   "ev0",
				CurrentA:    float64ptr(16.0),
				MaxCurrentA: float64ptr(32.0),
				VoltageV:    float64ptr(230.0),
				PowerW:      float64ptr(3680.0),
				EnergyWh:    float64ptr(1234.5),
				Ts:          1,
			},
		},
		{
			name: "FlowReservationRequestMsg/nil energy+power",
			msg: FlowReservationRequestMsg{
				MRID:              "abc",
				EnergyRequestedWh: nil,
				PowerRequestedW:   nil,
				Ts:                1,
			},
		},
		{
			name: "FlowReservationRequestMsg/non-nil energy+power",
			msg: FlowReservationRequestMsg{
				MRID:              "abc",
				EnergyRequestedWh: float64ptr(10000),
				PowerRequestedW:   float64ptr(7200),
				Ts:                1,
			},
		},
		{
			name: "ReservationMsg/nil energy+power",
			msg: ReservationMsg{
				MRID:          "abc",
				Subject:       "sub",
				EnergyAvailWh: nil,
				PowerAvailW:   nil,
			},
		},
		{
			name: "ReservationMsg/non-nil energy+power",
			msg: ReservationMsg{
				MRID:          "abc",
				Subject:       "sub",
				EnergyAvailWh: float64ptr(8000),
				PowerAvailW:   float64ptr(5000),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.msg)
			if err != nil {
				t.Fatalf("json.Marshal failed: %v", err)
			}
			if containsNaN(string(b)) {
				t.Errorf("marshaled JSON contains NaN literal: %s", b)
			}
		})
	}
}

// TestBusMessagesNaNPointerIsInvalid documents that a *float64 pointing to
// NaN is not a safe bus value — callers must use nil, not &NaN. This test
// exists to make the invariant explicit: if a producer passes &NaN, Marshal
// fails; producers must guard with "if NaN { field = nil }".
func TestBusMessagesNaNPointerIsInvalid(t *testing.T) {
	nan := math.NaN()
	msg := BattMetrics{
		Device:        "bat0",
		MaxChargeW:    &nan,
		MaxDischargeW: &nan,
		Ts:            1,
	}
	if _, err := json.Marshal(msg); err == nil {
		t.Error("expected json.Marshal to fail for *float64 pointing to NaN; " +
			"if this passes, encoding/json changed behavior and producers no longer need the nil guard")
	}
}

func float64ptr(v float64) *float64 { return &v }

// containsNaN reports whether s contains the JSON-invalid NaN literal.
func containsNaN(s string) bool {
	for i := 0; i+2 < len(s); i++ {
		if s[i] == 'N' && s[i+1] == 'a' && s[i+2] == 'N' {
			return true
		}
	}
	return false
}
