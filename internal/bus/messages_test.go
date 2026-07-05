package bus

import (
	"encoding/json"
	"testing"
)

// TestPublishedTypesStampV1 pins TASK-018's rollout: every top-level
// published type that embeds Envelope marshals "v":1 when stamped with its
// per-schema constant. This is deliberately mechanical — one case per type
// in the task's inventory table — so a future type that forgets to embed
// Envelope, or a publisher that forgets to stamp it, has a test that fails
// close to the mistake rather than only showing up as a missing "v" key on
// the wire during a bench spot check.
//
// Measurement is the one type in this list whose voltage field had to move
// from V/"v" to VoltageV/"voltage_v" to make room for the embedded
// Envelope's "v" (see messages.go's doc comment on Measurement for why); its
// case below asserts both "v" (schema version) and "voltage_v" (the renamed
// field) appear side by side without collision.
func TestPublishedTypesStampV1(t *testing.T) {
	cases := []struct {
		name string
		msg  any
	}{
		{"Measurement", Measurement{Envelope: Envelope{V: MeasurementV}, Device: "bat0", VoltageV: float64ptr(240.5), Ts: 1}},
		{"BattMetrics", BattMetrics{Envelope: Envelope{V: BattMetricsV}, Device: "bat0", Ts: 1}},
		{"ActiveControl", ActiveControl{Envelope: Envelope{V: ActiveControlV}, Source: "none", Ts: 1}},
		{"ComplianceAlert", ComplianceAlert{Envelope: Envelope{V: ComplianceAlertV}, MRID: "m", Ts: 1}},
		{"BattCommand", BattCommand{Envelope: Envelope{V: BattCommandV}, Device: "bat0", Ts: 1}},
		{"SolarCommand", SolarCommand{Envelope: Envelope{V: SolarCommandV}, Device: "solar0", Ts: 1}},
		{"EVSEState", EVSEState{Envelope: Envelope{V: EVSEStateV}, StationID: "ev0", Ts: 1}},
		{"EVSECommand", EVSECommand{Envelope: Envelope{V: EVSECommandV}, StationID: "ev0", Ts: 1}},
		{"PricingUpdate", PricingUpdate{Envelope: Envelope{V: PricingUpdateV}, Ts: 1}},
		{"BillingUpdate", BillingUpdate{Envelope: Envelope{V: BillingUpdateV}, Ts: 1}},
		{"FlowReservationRequestMsg", FlowReservationRequestMsg{Envelope: Envelope{V: FlowReservationRequestV}, MRID: "m", Ts: 1}},
		{"FlowReservationStatusMsg", FlowReservationStatusMsg{Envelope: Envelope{V: FlowReservationStatusV}, Ts: 1}},
		{"DERScheduleMsg", DERScheduleMsg{Envelope: Envelope{V: DERScheduleV}, Ts: 1}},
		{"PlanLog", PlanLog{Envelope: Envelope{V: PlanLogV}, Ts: 1}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.msg)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var round map[string]any
			if err := json.Unmarshal(b, &round); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			v, ok := round["v"]
			if !ok {
				t.Fatalf("%s marshaled JSON %s missing \"v\" key", tc.name, b)
			}
			if v != float64(1) {
				t.Errorf("%s: v = %v, want 1", tc.name, v)
			}
		})
	}
}

// TestMeasurementVoltageDoesNotCollideWithEnvelopeV is a regression test for
// the specific landmine documented on the Measurement type: before the
// voltage field was renamed to VoltageV, both it and the embedded
// Envelope.V serialized to JSON key "v", and Go's encoder silently let the
// shallower (Measurement's own) field win — so stamping Envelope.V never
// actually appeared on the wire at all, while looking like ordinary working
// code (msg.Envelope.V = 1 compiles and does not error). This test proves
// the wire wire carries the version in "v" AND the voltage reading in
// "voltage_v" simultaneously.
func TestMeasurementVoltageDoesNotCollideWithEnvelopeV(t *testing.T) {
	msg := Measurement{
		Envelope: Envelope{V: MeasurementV},
		Device:   "bat0",
		VoltageV: float64ptr(241.3),
		Ts:       1,
	}
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var round map[string]any
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if round["v"] != float64(1) {
		t.Errorf("v = %v, want 1 (schema version)", round["v"])
	}
	if round["voltage_v"] != 241.3 {
		t.Errorf("voltage_v = %v, want 241.3 (voltage reading)", round["voltage_v"])
	}
}

// TestPublishedTypesLegacyV0GoldenPayload pins the decode side of the
// transition (AD-006): a golden pre-envelope payload — the exact wire shape
// a not-yet-upgraded publisher emits, with no "v" key at all — still
// unmarshals cleanly into today's type, with V left at its zero value. This
// is the retained-message trap from the task background: at rolling-upgrade
// time the broker holds exactly these payloads for the retained
// control-plane topics, and a new subscriber must decode them the same as
// any v1 payload's non-envelope fields.
func TestPublishedTypesLegacyV0GoldenPayload(t *testing.T) {
	cases := []struct {
		name    string
		golden  string
		checkFn func(t *testing.T, b []byte)
	}{
		{
			// A rolling-restart window can have an old lexa-modbus binary
			// (pre-TASK-018, still writing voltage under "v") publishing
			// while a new lexa-hub binary (post-TASK-018, "v" now means
			// schema version, an int) subscribes. A real voltage reading is
			// virtually never a whole number, so this is the overwhelmingly
			// likely shape of that mixed-version message: json.Unmarshal
			// itself fails (a JSON number with a fractional part cannot
			// convert to Go's int), which is safe — it is indistinguishable
			// from any other malformed payload and is dropped by
			// mqttutil.Subscribe's pre-existing log-and-drop, the same path
			// that has always handled bad JSON on this topic. This is not a
			// case CheckVersion needs to reject: the real unmarshal rejects
			// it first, exactly as documented (envelope.go's CheckVersion
			// doc comment: "not our job to flag; the real json.Unmarshal's
			// job, a few lines later").
			name:   "Measurement mixed-version window (pre-rename \"v\"-as-voltage, fractional)",
			golden: `{"device":"bat0","w":100.5,"v":240.5,"hz":60.0,"ts":1}`,
			checkFn: func(t *testing.T, b []byte) {
				var m Measurement
				if err := json.Unmarshal(b, &m); err == nil {
					t.Fatalf("Unmarshal succeeded decoding %+v; want an error (fractional \"v\" cannot be the int schema version)", m)
				}
			},
		},
		{
			name:   "ActiveControl legacy (absent v)",
			golden: `{"source":"event","mrid":"abc","ts":5}`,
			checkFn: func(t *testing.T, b []byte) {
				var ac ActiveControl
				if err := json.Unmarshal(b, &ac); err != nil {
					t.Fatalf("Unmarshal: %v", err)
				}
				if ac.V != 0 {
					t.Errorf("Envelope.V = %d, want 0", ac.V)
				}
				if ac.Source != "event" || ac.MRID != "abc" || ac.Ts != 5 {
					t.Errorf("decode mismatch: %+v", ac)
				}
			},
		},
		{
			name:   "PlanLog legacy (absent v)",
			golden: `{"ts":9}`,
			checkFn: func(t *testing.T, b []byte) {
				var pl PlanLog
				if err := json.Unmarshal(b, &pl); err != nil {
					t.Fatalf("Unmarshal: %v", err)
				}
				if pl.V != 0 {
					t.Errorf("Envelope.V = %d, want 0", pl.V)
				}
				if pl.Ts != 9 {
					t.Errorf("decode mismatch: %+v", pl)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.checkFn(t, []byte(tc.golden))
		})
	}
}
