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

// measurementPreWP2 replicates Measurement's exact wire shape BEFORE the
// WP-2 enrichment fields landed (device/w/voltage_v/hz/ts only). It exists
// so TestMeasurementWP2CrossDecode can prove old-subscriber tolerance
// against the real pre-change shape, not a hand-waved approximation.
type measurementPreWP2 struct {
	Envelope
	Device   string   `json:"device"`
	W        *float64 `json:"w,omitempty"`
	VoltageV *float64 `json:"voltage_v,omitempty"`
	Hz       *float64 `json:"hz,omitempty"`
	Ts       int64    `json:"ts"`
}

// measurementWP2Golden is the wire shape a WP-2 publisher emits with every
// enrichment field populated, still at "v":1 (additive-at-same-version,
// AD-006/architecture §2.1). Kept as a literal so the JSON keys themselves
// are pinned, not derived from the very struct tags under test.
const measurementWP2Golden = `{"v":1,"device":"meter0","w":-2500,"voltage_v":240.1,"hz":60.02,` +
	`"var_w":150.5,"va":2600,"pf":0.96,"op_state":1,"conn_state":1,"alarm_bits":4,` +
	`"wh_imp_total":12500000,"wh_exp_total":3400000,"ts":1752451200}`

// TestMeasurementWP2CrossDecode pins WP-2's additive-fields contract in both
// directions:
//
//  1. an OLD payload (pre-WP-2 publisher, no new keys) decodes into the NEW
//     struct with every enrichment field nil-absent — a new subscriber never
//     mistakes an old publisher for one reporting zeros;
//  2. a NEW payload (every enrichment field present) decodes into the OLD
//     struct shape without error, all pre-existing fields intact — an
//     old subscriber tolerates a new publisher, which is exactly why V
//     stays 1 instead of bumping.
func TestMeasurementWP2CrossDecode(t *testing.T) {
	t.Run("old payload into new struct: enrichment fields all nil", func(t *testing.T) {
		old := `{"v":1,"device":"inv0","w":1000.5,"voltage_v":240.5,"hz":60.0,"ts":1752451200}`
		var m Measurement
		if err := json.Unmarshal([]byte(old), &m); err != nil {
			t.Fatalf("Unmarshal old payload: %v", err)
		}
		if m.W == nil || *m.W != 1000.5 || m.VoltageV == nil || *m.VoltageV != 240.5 {
			t.Errorf("pre-existing fields mis-decoded: %+v", m)
		}
		if m.VarW != nil || m.VA != nil || m.PF != nil ||
			m.OpState != nil || m.ConnState != nil || m.AlarmBits != nil ||
			m.WhImpTotal != nil || m.WhExpTotal != nil {
			t.Errorf("enrichment fields must all be nil for an old payload, got %+v", m)
		}
		if err := m.Finite(); err != nil {
			t.Errorf("Finite() on decoded old payload = %v, want nil", err)
		}
	})

	t.Run("new payload into new struct: enrichment fields populated", func(t *testing.T) {
		var m Measurement
		if err := json.Unmarshal([]byte(measurementWP2Golden), &m); err != nil {
			t.Fatalf("Unmarshal new payload: %v", err)
		}
		if m.VarW == nil || *m.VarW != 150.5 || m.VA == nil || *m.VA != 2600 ||
			m.PF == nil || *m.PF != 0.96 {
			t.Errorf("power-quality fields mis-decoded: %+v", m)
		}
		if m.OpState == nil || *m.OpState != 1 || m.ConnState == nil || *m.ConnState != 1 ||
			m.AlarmBits == nil || *m.AlarmBits != 4 {
			t.Errorf("state fields mis-decoded: %+v", m)
		}
		if m.WhImpTotal == nil || *m.WhImpTotal != 12500000 || m.WhExpTotal == nil || *m.WhExpTotal != 3400000 {
			t.Errorf("energy fields mis-decoded: %+v", m)
		}
		if err := m.Finite(); err != nil {
			t.Errorf("Finite() on decoded new payload = %v, want nil", err)
		}
	})

	t.Run("new payload into pre-WP2 struct shape: old-subscriber tolerance", func(t *testing.T) {
		var m measurementPreWP2
		if err := json.Unmarshal([]byte(measurementWP2Golden), &m); err != nil {
			t.Fatalf("Unmarshal new payload into pre-WP2 shape: %v (an old subscriber must tolerate the new keys)", err)
		}
		if m.V != 1 || m.Device != "meter0" ||
			m.W == nil || *m.W != -2500 ||
			m.VoltageV == nil || *m.VoltageV != 240.1 ||
			m.Hz == nil || *m.Hz != 60.02 ||
			m.Ts != 1752451200 {
			t.Errorf("pre-existing fields mis-decoded by old shape: %+v", m)
		}
	})
}
