package bus

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// TestStdlibRejectsNaNIntoTypedFloat64 pins the crux fact this task's scope
// depends on (GAP-09 review §9, TASK-055's "Common mistakes to avoid"):
// encoding/json ALREADY refuses to decode a bare NaN/Infinity token, or a
// quoted "NaN"/"Infinity"/"-Infinity" string, into a typed float64 or
// *float64 field. If a future stdlib change (or a swap to a different JSON
// library) ever makes one of these subtests fail, that is exactly the
// regression this test exists to catch — it means the residual risk this
// task treats as "someone else's job" has become this package's job too.
func TestStdlibRejectsNaNIntoTypedFloat64(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"bare NaN token", `{"device":"d0","w":NaN,"ts":1}`},
		{"bare Infinity token", `{"device":"d0","w":Infinity,"ts":1}`},
		{"bare -Infinity token", `{"device":"d0","w":-Infinity,"ts":1}`},
		{"quoted NaN string", `{"device":"d0","w":"NaN","ts":1}`},
		{"quoted Infinity string", `{"device":"d0","w":"Infinity","ts":1}`},
		{"quoted -Infinity string", `{"device":"d0","w":"-Infinity","ts":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var m Measurement
			err := json.Unmarshal([]byte(tc.payload), &m)
			if err == nil {
				t.Fatalf("json.Unmarshal(%s) into Measurement.W (*float64) succeeded; "+
					"want an error — stdlib's rejection of non-finite tokens into typed "+
					"numeric fields is the premise this task's scope relies on", tc.payload)
			}
		})
	}
}

// TestStdlibAcceptsValidMeasurementPayloads is the accept-side complement to
// TestStdlibRejectsNaNIntoTypedFloat64: the tightening this task adds must
// never reject a genuinely valid message, including the nil-pointer
// absent-value convention ("{}" — no numeric fields reported at all).
func TestStdlibAcceptsValidMeasurementPayloads(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		wantW   *float64
	}{
		{"numeric w", `{"device":"d0","w":4500,"ts":1}`, float64ptr(4500)},
		{"absent w (nil pointer convention)", `{"device":"d0","ts":1}`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var m Measurement
			if err := json.Unmarshal([]byte(tc.payload), &m); err != nil {
				t.Fatalf("json.Unmarshal(%s): unexpected error: %v", tc.payload, err)
			}
			if (tc.wantW == nil) != (m.W == nil) {
				t.Fatalf("W = %v, want nil-ness %v", m.W, tc.wantW == nil)
			}
			if tc.wantW != nil && *m.W != *tc.wantW {
				t.Errorf("W = %v, want %v", *m.W, *tc.wantW)
			}
			if err := m.Finite(); err != nil {
				t.Errorf("Finite() = %v, want nil for a valid decoded message", err)
			}
		})
	}
}

// TestFiniteRejectsSlippedThroughNaN is the defense-in-depth half of GAP-09:
// it does not go through json.Unmarshal at all (stdlib already rejects the
// forms above) — it simulates what a hypothetically laxer decode path
// (UseNumber, interface{}/map[string]any, or a non-stdlib JSON library
// tolerant of NaN, none of which this repo's scope grep found in the bus
// decode path — see TASK-055's PR notes) would produce: a struct literal
// with a *float64 actually pointing at NaN/±Inf. Finite() must catch every
// one of those and name the offending field.
func TestFiniteRejectsSlippedThroughNaN(t *testing.T) {
	nan := math.NaN()
	posInf := math.Inf(1)
	negInf := math.Inf(-1)

	cases := []struct {
		name      string
		msg       interface{ Finite() error }
		wantField string
	}{
		{"Measurement/W=NaN", Measurement{Device: "d0", W: &nan, Ts: 1}, "w"},
		{"Measurement/VoltageV=+Inf", Measurement{Device: "d0", VoltageV: &posInf, Ts: 1}, "voltage_v"},
		{"Measurement/Hz=-Inf", Measurement{Device: "d0", Hz: &negInf, Ts: 1}, "hz"},
		{"Measurement/VarW=NaN", Measurement{Device: "d0", VarW: &nan, Ts: 1}, "var_w"},
		{"Measurement/VA=+Inf", Measurement{Device: "d0", VA: &posInf, Ts: 1}, "va"},
		{"Measurement/PF=NaN", Measurement{Device: "d0", PF: &nan, Ts: 1}, "pf"},
		{"Measurement/WhImpTotal=-Inf", Measurement{Device: "d0", WhImpTotal: &negInf, Ts: 1}, "wh_imp_total"},
		{"Measurement/WhExpTotal=NaN", Measurement{Device: "d0", WhExpTotal: &nan, Ts: 1}, "wh_exp_total"},
		{"BattMetrics/SOC=NaN", BattMetrics{Device: "b0", SOC: &nan, Ts: 1}, "soc_pct"},
		{"BattMetrics/MaxChargeW=+Inf", BattMetrics{Device: "b0", MaxChargeW: &posInf, Ts: 1}, "max_charge_w"},
		{"EVSEState/PowerW=NaN", EVSEState{StationID: "e0", PowerW: &nan, Ts: 1}, "power_w"},
		{"EVSEState/CurrentA=-Inf", EVSEState{StationID: "e0", CurrentA: &negInf, Ts: 1}, "current_a"},
		{"ComplianceAlert/LimitW=NaN", ComplianceAlert{MRID: "m0", LimitW: nan, Ts: 1}, "limit_w"},
		{"ComplianceAlert/ShortfallW=+Inf", ComplianceAlert{MRID: "m0", ShortfallW: posInf, Ts: 1}, "shortfall_w"},
		{"DERScheduleSlot/MaxLimW=NaN", DERScheduleSlot{Start: 1, End: 2, MaxLimW: &nan}, "max_lim_w"},
		{"DERScheduleSlot/ExpLimW=+Inf", DERScheduleSlot{Start: 1, End: 2, ExpLimW: &posInf}, "exp_lim_w"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.msg.Finite()
			if err == nil {
				t.Fatalf("Finite() = nil, want an error naming field %q", tc.wantField)
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Errorf("Finite() error %q does not name field %q", err.Error(), tc.wantField)
			}
		})
	}
}

// TestFiniteAcceptsNilAndValidFields proves Finite() never rejects the
// absent-value convention (nil pointers) or genuinely finite values — the
// "must not reject VALID messages" requirement from TASK-055's blast radius
// section.
func TestFiniteAcceptsNilAndValidFields(t *testing.T) {
	w := 4500.0
	if err := (Measurement{Device: "d0", Ts: 1}).Finite(); err != nil {
		t.Errorf("Finite() on all-nil Measurement = %v, want nil", err)
	}
	if err := (Measurement{Device: "d0", W: &w, Ts: 1}).Finite(); err != nil {
		t.Errorf("Finite() on valid Measurement = %v, want nil", err)
	}
	varW, va, pf, whImp, whExp := 120.0, 4600.0, 0.98, 1.25e7, 3.4e6
	opSt, connSt := uint16(1), uint16(1)
	alrm := uint32(0)
	full := Measurement{
		Device: "d0", W: &w, VarW: &varW, VA: &va, PF: &pf,
		OpState: &opSt, ConnState: &connSt, AlarmBits: &alrm,
		WhImpTotal: &whImp, WhExpTotal: &whExp, Ts: 1,
	}
	if err := full.Finite(); err != nil {
		t.Errorf("Finite() on fully-populated valid Measurement (WP-2 fields) = %v, want nil", err)
	}
	if err := (BattMetrics{Device: "b0", Ts: 1}).Finite(); err != nil {
		t.Errorf("Finite() on all-nil BattMetrics = %v, want nil", err)
	}
	if err := (ActiveControl{Source: "event", Ts: 1}).Finite(); err != nil {
		t.Errorf("Finite() on all-nil ActiveControl = %v, want nil", err)
	}
	if err := (ComplianceAlert{MRID: "m0", LimitW: 5000, MeasuredW: 5200, ShortfallW: 200, Ts: 1}).Finite(); err != nil {
		t.Errorf("Finite() on valid ComplianceAlert = %v, want nil", err)
	}
	if err := (EVSEState{StationID: "e0", Ts: 1}).Finite(); err != nil {
		t.Errorf("Finite() on all-nil EVSEState = %v, want nil", err)
	}
	slot := DERScheduleSlot{Start: 1, End: 2}
	if err := slot.Finite(); err != nil {
		t.Errorf("Finite() on all-nil DERScheduleSlot = %v, want nil", err)
	}
	sched := DERScheduleMsg{WindowStart: 1, WindowEnd: 2, Slots: []DERScheduleSlot{slot}}
	if err := sched.Finite(); err != nil {
		t.Errorf("Finite() on DERScheduleMsg with a valid slot = %v, want nil", err)
	}
}

// TestActiveControlNaNLimitNeverReachesOptimizer is the safety-payoff case
// TASK-055 exists for: a NaN in ActiveControl's export/import/generation/
// fixed-dispatch limit is the one bus value that maps directly onto a live
// control decision in cmd/hub's optimizer. json.Unmarshal already refuses a
// bare or quoted NaN into ExpLimW/ImpLimW/MaxLimW/FixedW (all *float64) —
// proven generically above — so the only way a NaN limit could ever reach
// this point is a lax decode path constructing the struct directly, which
// is what this test simulates. Finite() must reject the WHOLE message (not
// just the one field) so mqttutil.Subscribe drops it before handler runs,
// meaning the previously-adopted control's last-known-good limit holds
// (fail-closed) instead of a NaN cap being adopted.
func TestActiveControlNaNLimitNeverReachesOptimizer(t *testing.T) {
	nan := math.NaN()
	limits := []struct {
		name string
		ctrl ActiveControl
	}{
		{"ExpLimW", ActiveControl{Source: "event", MRID: "m1", ExpLimW: &nan, Ts: 1}},
		{"ImpLimW", ActiveControl{Source: "event", MRID: "m1", ImpLimW: &nan, Ts: 1}},
		{"MaxLimW", ActiveControl{Source: "event", MRID: "m1", MaxLimW: &nan, Ts: 1}},
		{"FixedW", ActiveControl{Source: "event", MRID: "m1", FixedW: &nan, Ts: 1}},
	}
	for _, tc := range limits {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.ctrl.Finite(); err == nil {
				t.Fatalf("Finite() = nil for an ActiveControl with a NaN %s; "+
					"a NaN control limit must be rejected, never adopted", tc.name)
			}
		})
	}
}
