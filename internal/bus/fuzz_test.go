package bus

// Go-native fuzz target for the bus JSON decode surface (TASK-048): every
// service decodes messages via mqttutil.Subscribe[T] -> json.Unmarshal
// (internal/mqttutil/mqttutil.go), gated first by CheckVersion (AD-006,
// TASK-017/018 — fully landed as of this task; the envelope is no longer
// "when present", every message type embeds it). FuzzBusDecode mirrors that
// exact two-step decode path — CheckVersion, then json.Unmarshal — for a
// representative slice of the type inventory in messages.go, table-driven
// over a type-tag byte prefix (one Fuzz corpus entry is (tag byte, JSON
// []byte); tag selects which message type this iteration decodes into).
//
// Assertions, per the task: no panic (the harness's job — a crash here is a
// fuzz failure); re-marshal -> re-decode is stable for anything that
// successfully decodes (round-trip idempotence); and the *float64 fields'
// absent-vs-zero distinction survives decode (nil stays nil when the wire
// key is simply missing — the NaN lesson (nan_test.go, TASK's own GAP-09
// sibling) applied to structural absence rather than value content).
//
// Run locally (nightly CI runs this at 15m; see the `fuzz` Makefile target
// and .github/workflows/ci.yml):
//
//	go test -fuzz=FuzzBusDecode -fuzztime=15m ./internal/bus/

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// busFuzzCase pairs a topic (drives CheckVersion/SupportedV exactly like a
// real subscriber) with a seed payload for one message type.
type busFuzzCase struct {
	topic string
	seed  []byte
}

// numBusTypes must match the number of cases switch/case handles in
// fuzzDecodeByTag below; tag % numBusTypes selects which type a given fuzz
// corpus entry decodes into.
const numBusTypes = 7

// busSeeds returns real-shape JSON payloads for each of the numBusTypes
// message types this fuzz target covers, one representative "real" payload
// per type (field values chosen to match messages.go's own doc-comment
// examples and the wire shapes gridsim/hub actually produce) plus the
// absent-vs-nil and envelope-version edge cases the task calls out.
func busSeeds() []busFuzzCase {
	return []busFuzzCase{
		// 0: Measurement — full payload, then one with pointer fields absent.
		{MeasurementTopic("bat0"), []byte(`{"device":"bat0","w":1500.5,"voltage_v":240.1,"hz":60.01,"ts":1751500000}`)},
		{MeasurementTopic("bat0"), []byte(`{"device":"bat0","ts":1751500000}`)},             // w/voltage_v/hz all absent -> must stay nil
		{MeasurementTopic("bat0"), []byte(`{"v":1,"device":"bat0","w":0,"ts":1751500000}`)}, // w present as literal 0, must NOT be nil

		// 1: BattMetrics
		{BattMetricsTopic("bat0"), []byte(`{"device":"bat0","soc_pct":55.2,"soh_pct":98.0,"capacity_wh":13500,"max_charge_w":5000,"max_discharge_w":5000,"ts":1751500000}`)},
		{BattMetricsTopic("bat0"), []byte(`{"device":"bat0","ts":1751500000}`)},

		// 2: ActiveControl (the "captured lexa/csip/control payload" the task names)
		{TopicCSIPControl, []byte(`{"v":1,"source":"event","mrid":"DERC-SP-002","connect":true,"exp_lim_w":2500,"clock_offset":3,"valid_until":1751500480,"ts":1751500000}`)},
		{TopicCSIPControl, []byte(`{"source":"none","clock_offset":0,"ts":1751500000}`)}, // every *float64 absent
		{TopicCSIPControl, []byte(`{"v":99,"source":"event","mrid":"x","ts":1}`)},        // unsupported version -> CheckVersion must reject before json.Unmarshal

		// 3: ComplianceAlert
		{TopicCSIPComplianceAlert, []byte(`{"mrid":"DERC-SP-002","limit_type":"export","limit_w":2500,"measured_w":3100,"shortfall_w":600,"reason":"battery at SOC reserve","active":true,"ts":1751500000}`)},

		// 4: EVSEState
		{EVSEStateTopic("ev0"), []byte(`{"station_id":"ev0","connector_id":1,"connected":true,"session_active":true,"current_a":16,"max_current_a":32,"voltage_v":230,"power_w":3680,"soc_pct":42.5,"energy_wh":1234.5,"status":"Charging","ts":1751500000}`)},
		{EVSEStateTopic("ev0"), []byte(`{"station_id":"ev0","connector_id":1,"connected":false,"session_active":false,"status":"Available","ts":1751500000}`)},

		// 5: DERScheduleMsg (nested slices/pointers)
		{TopicNorthboundSchedule, []byte(`{"window_start":1751500000,"window_end":1751586400,"build_time":1751500000,"clock_offset":3,` +
			`"slots":[{"start":1751500000,"end":1751500600,"source":"event","mrid":"DERC-SP-002","max_lim_w":2500}],"ts":1751500000}`)},

		// 6: FlowReservationRequestMsg
		{TopicCSIPFRRequest, []byte(`{"mrid":"abc","energy_requested_wh":10000,"power_requested_w":7200,"duration_requested":3600,"interval_start":1751500000,"interval_duration":3600,"ts":1751500000}`)},
		{TopicCSIPFRRequest, []byte(`{"mrid":"abc","duration_requested":0,"interval_start":0,"interval_duration":0,"ts":0}`)}, // both *float64 absent
	}
}

func FuzzBusDecode(f *testing.F) {
	for i, c := range busSeeds() {
		f.Add(byte(i), c.seed)
	}
	// A handful of structural edge cases independent of any specific type.
	f.Add(byte(0), []byte(`{}`))
	f.Add(byte(0), []byte(`null`))
	f.Add(byte(0), []byte(`[]`))
	f.Add(byte(0), []byte(``))
	f.Add(byte(0), []byte(`{"device":"bat0","w":"not-a-number","ts":1}`))
	f.Add(byte(0), []byte(`{"v":"not-an-int","device":"bat0","ts":1}`))

	topics := []string{
		MeasurementTopic("dev0"),
		BattMetricsTopic("dev0"),
		TopicCSIPControl,
		TopicCSIPComplianceAlert,
		EVSEStateTopic("dev0"),
		TopicNorthboundSchedule,
		TopicCSIPFRRequest,
	}

	f.Fuzz(func(t *testing.T, tag byte, data []byte) {
		idx := int(tag) % numBusTypes
		topic := topics[idx]
		switch idx {
		case 0:
			fuzzDecodeOne[Measurement](t, topic, data)
		case 1:
			fuzzDecodeOne[BattMetrics](t, topic, data)
		case 2:
			fuzzDecodeOne[ActiveControl](t, topic, data)
		case 3:
			fuzzDecodeOne[ComplianceAlert](t, topic, data)
		case 4:
			fuzzDecodeOne[EVSEState](t, topic, data)
		case 5:
			fuzzDecodeOne[DERScheduleMsg](t, topic, data)
		case 6:
			fuzzDecodeOne[FlowReservationRequestMsg](t, topic, data)
		}
	})
}

// fuzzDecodeOne mirrors mqttutil.Subscribe[T]'s exact decode path for one
// message type: CheckVersion first (a rejected version never reaches
// json.Unmarshal, exactly like production), then json.Unmarshal. Anything
// that fails either step is a normal log-and-drop outcome in production —
// nothing further to assert. Anything that succeeds must round-trip stably
// and must not have turned an absent wire key into a non-nil zero pointer.
func fuzzDecodeOne[T any](t *testing.T, topic string, data []byte) {
	t.Helper()

	if verr := CheckVersion(topic, data, SupportedV(topic)); verr != nil {
		return // production: bus.RejectAndAlarm, handler never invoked
	}

	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return // production: log-and-drop (mqttutil.Subscribe), handler never invoked
	}

	// Round-trip idempotence: re-marshal a successfully-decoded value and
	// re-decode it; the result must be identical to the first decode.
	remarshaled, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("re-marshal of successfully-decoded %T failed: %v (input=%s)", v, err, data)
	}
	var v2 T
	if err := json.Unmarshal(remarshaled, &v2); err != nil {
		t.Fatalf("re-decode of re-marshaled %T failed: %v (remarshaled=%s)", v, err, remarshaled)
	}
	if !reflect.DeepEqual(v, v2) {
		t.Fatalf("round-trip unstable for %T: first=%+v second=%+v (input=%s)", v, v, v2, data)
	}

	// Absent-vs-zero: a *float64 field whose wire key was absent from data
	// must decode to nil, never a pointer to 0.0 (or any other value).
	assertAbsentFloatsStayNil(t, data, v)
}

// assertAbsentFloatsStayNil walks v's direct exported fields; for every
// *float64 field with a json tag, it checks whether that key was present in
// the raw top-level JSON object data. If the key was absent, the decoded
// field must be a nil pointer — a non-nil pointer there means
// json.Unmarshal (or a hand-written UnmarshalJSON, not the case for any bus
// type today) manufactured a value the wire never sent, exactly the
// "silent zero-value" hazard this task applies to JSON. If data is not a
// top-level JSON object (array, scalar, null, or malformed — though a
// malformed object would already have failed the caller's json.Unmarshal
// into T and returned before reaching here), the presence check is
// skipped: there is no key set to compare against.
func assertAbsentFloatsStayNil(t *testing.T, data []byte, v any) {
	t.Helper()

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return
	}

	rv := reflect.ValueOf(v)
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		field := rt.Field(i)
		if field.Type.Kind() != reflect.Ptr || field.Type.Elem().Kind() != reflect.Float64 {
			continue
		}
		tag := field.Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if name == "" || name == "-" {
			continue
		}
		// encoding/json's Unmarshal matches an object key to a struct field
		// case-INsensitively when no exact match exists (documented
		// behavior, not a bug): {"soC_pCt":0} still populates a field
		// tagged `json:"soc_pct"`. A case-sensitive presence check here
		// would misreport that as "key absent, pointer should be nil" and
		// fail on a false positive (found by this very fuzz target: seed
		// input `{"soC_pCt":0}` against BattMetrics.SOC) — match the real
		// decoder's own case-insensitive rule, not a stricter one of our
		// own invention.
		present := false
		for k := range raw {
			if strings.EqualFold(k, name) {
				present = true
				break
			}
		}
		fv := rv.Field(i)
		if !present && !fv.IsNil() {
			t.Fatalf("field %s: JSON key %q (case-insensitive) absent from input but decoded pointer is non-nil (%v) — "+
				"absent-vs-zero distinction lost (input=%s)", field.Name, name, fv.Elem().Interface(), data)
		}
	}
}

// TestBusDecodeRejectsUnknownEnvelopeVersion pins the task's "if the 018
// envelope has landed, include unknown-v seeds and assert reject path"
// instruction with a named, always-run test (018 has landed — see
// envelope.go/mqttutil.go, both fully wired, not merely "when present").
func TestBusDecodeRejectsUnknownEnvelopeVersion(t *testing.T) {
	supported := SupportedV(TopicCSIPControl) // ActiveControlV == 1
	data := []byte(`{"v":99,"source":"event","mrid":"x","ts":1}`)

	verr := CheckVersion(TopicCSIPControl, data, supported)
	ve, ok := verr.(*VersionError)
	if !ok || ve == nil {
		t.Fatalf("expected a *VersionError for v=99 > supported=%d, got %#v", supported, verr)
	}
	if ve.Topic != TopicCSIPControl || ve.Got != 99 || ve.Supported != supported {
		t.Fatalf("VersionError fields wrong: %+v", ve)
	}

	// Production behavior (mqttutil.Subscribe): a rejected version means
	// json.Unmarshal into the real type never runs at all for this payload.
	// fuzzDecodeOne enforces that same short-circuit above.
}
