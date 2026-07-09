package bus

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// jsonKeys unmarshals b into a map and reports whether "v" appears exactly
// once at the top level (any(map) collapses duplicate keys, so this also
// proves there's no second field also encoding to "v" — the Measurement
// voltage-collision landmine messages.go documents).
func jsonKeys(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

// assertSingleVKey re-marshals via a raw-key scan (not just map lookup, which
// would hide a second "v" emission since JSON object keys are unique once
// decoded into a map) to prove the wire text itself contains exactly one
// `"v":` key. This is the no-field-collides-with-"v" requirement.
func assertSingleVKey(t *testing.T, name string, b []byte) {
	t.Helper()
	count := strings.Count(string(b), `"v":`)
	if count != 1 {
		t.Errorf("%s: wire JSON has %d \"v\" keys, want exactly 1: %s", name, count, b)
	}
}

// TestIntentTypesStampV1AndRoundTrip covers every intent.go type: it marshals
// a fully-populated value stamped with its version constant, asserts exactly
// one "v" key appears (the collision guard every embedded-Envelope type
// needs, per messages_test.go's TestMeasurementVoltageDoesNotCollideWithEnvelopeV
// precedent), and round-trips it back through json.Unmarshal.
func TestIntentTypesStampV1AndRoundTrip(t *testing.T) {
	exportRate := 0.08
	cases := []struct {
		name string
		msg  any
	}{
		{"ModeIntent", ModeIntent{
			Envelope:   Envelope{V: ModeIntentV},
			IntentMeta: IntentMeta{ID: "i1", Origin: "app", Actor: "root", IssuedAt: 1},
			Mode:       "gateway",
		}},
		{"EVGoalIntent", EVGoalIntent{
			Envelope:      Envelope{V: EVGoalIntentV},
			IntentMeta:    IntentMeta{ID: "i2", Origin: "cloud", IssuedAt: 1},
			StationID:     "station-1",
			TargetSocKwh:  float64ptr(40),
			DepartureUnix: 1_700_000_000,
			InitialSocKwh: float64ptr(10),
			CapacityKwh:   float64ptr(60),
		}},
		{"BackupReserveIntent", BackupReserveIntent{
			Envelope:   Envelope{V: BackupReserveIntentV},
			IntentMeta: IntentMeta{ID: "i3", Origin: "app", IssuedAt: 1},
			ReservePct: float64ptr(30),
		}},
		{"SolarForecastIntent", SolarForecastIntent{
			Envelope:    Envelope{V: SolarForecastIntentV},
			IntentMeta:  IntentMeta{ID: "i4", Origin: "cloud", IssuedAt: 1},
			WindowStart: 1_700_000_000,
			StepKw:      []float64{1.1, 2.2, 0},
			SourceTs:    1_699_999_000,
		}},
		{"LoadProfileIntent", LoadProfileIntent{
			Envelope:   Envelope{V: LoadProfileIntentV},
			IntentMeta: IntentMeta{ID: "i5", Origin: "cloud", IssuedAt: 1},
			StepKw:     []float64{0.5, 0.6},
		}},
		{"TariffIntent", TariffIntent{
			Envelope:   Envelope{V: TariffIntentV},
			IntentMeta: IntentMeta{ID: "i6", Origin: "app", IssuedAt: 1},
			Tariff: TariffSpec{
				Currency: "USD",
				Periods: []TariffPeriod{
					{Label: "peak", Days: []int{1, 2, 3, 4, 5}, StartHH: 16, EndHH: 21, ImportPerKwh: 0.42, ExportPerKwh: &exportRate},
					{Label: "off-peak", Days: []int{0, 6}, StartHH: 0, EndHH: 24, ImportPerKwh: 0.12},
				},
			},
		}},
		{"ChargeNowIntent", ChargeNowIntent{
			Envelope:   Envelope{V: ChargeNowIntentV},
			IntentMeta: IntentMeta{ID: "i7", Origin: "app", IssuedAt: 1, TTLS: 300},
			StationID:  "station-1",
		}},
		{"IntentResult", IntentResult{
			Envelope: Envelope{V: IntentResultV},
			ID:       "i7", Kind: "chargenow", Outcome: "applied", Ts: 1,
		}},
		{"ModeStatus", ModeStatus{
			Envelope: Envelope{V: ModeStatusV},
			Mode:     "gateway", Since: 1, Actor: "root", IntentID: "i1", Ts: 1,
		}},
		{"CloudlinkStatus", CloudlinkStatus{
			Envelope:      Envelope{V: CloudlinkStatusV},
			Connected:     true,
			Endpoint:      "ssl://example:8883",
			SpoolBytes:    1024,
			SpoolOldestTs: 1,
			LastUplinkTs:  2,
			CertDaysLeft:  90,
			Ts:            3,
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.msg)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			assertSingleVKey(t, tc.name, b)
			m := jsonKeys(t, b)
			if v, ok := m["v"]; !ok || v != float64(1) {
				t.Fatalf("%s: v = %v (ok=%v), want 1", tc.name, v, ok)
			}
		})
	}
}

// TestModeIntentRoundTrip exercises the embedded-IntentMeta shape explicitly:
// marshal → unmarshal must reproduce every IntentMeta field alongside the
// kind-specific field, proving IntentMeta's embedding doesn't shadow or drop
// anything when combined with Envelope's own embedding in the same struct.
func TestModeIntentRoundTrip(t *testing.T) {
	want := ModeIntent{
		Envelope:   Envelope{V: ModeIntentV},
		IntentMeta: IntentMeta{ID: "abc-123", Origin: "cloud", Actor: "user@example.com", IssuedAt: 1_700_000_000, TTLS: 0},
		Mode:       "optimizer",
	}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ModeIntent
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != want {
		t.Errorf("round-trip mismatch:\n got  %+v\n want %+v\n wire %s", got, want, b)
	}
	// ttl_s is omitempty and TTLS is zero here — must not appear on the wire.
	if strings.Contains(string(b), "ttl_s") {
		t.Errorf("zero TTLS must be omitted (omitempty), got %s", b)
	}
}

// TestEVGoalIntentOptionalFieldsOmitted verifies the *float64 absent-value
// convention: InitialSocKwh/CapacityKwh nil must vanish from the wire (they
// are omitempty), while TargetSocKwh (no omitempty — always meaningful, even
// as an explicit 0) always appears, including when it points at 0.
func TestEVGoalIntentOptionalFieldsOmitted(t *testing.T) {
	zero := 0.0
	g := EVGoalIntent{
		Envelope:      Envelope{V: EVGoalIntentV},
		IntentMeta:    IntentMeta{ID: "g1", Origin: "app", IssuedAt: 1},
		TargetSocKwh:  &zero,
		DepartureUnix: 100,
		InitialSocKwh: nil,
		CapacityKwh:   nil,
	}
	b, err := json.Marshal(g)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"target_soc_kwh":0`) {
		t.Errorf(`explicit-zero target_soc_kwh must serialize as "target_soc_kwh":0, got %s`, b)
	}
	for _, key := range []string{"initial_soc_kwh", "capacity_kwh", "station_id"} {
		if strings.Contains(string(b), key) {
			t.Errorf("nil/empty field %q must be omitted, got %s", key, b)
		}
	}
}

// TestFiniteRejectsNonFiniteIntentFields is the Finite() rejection table for
// every intent.go type that carries numeric fields (finite.go's additions),
// mirroring nan_reject_test.go's TestFiniteRejectsSlippedThroughNaN shape:
// simulated slipped-through NaN/Inf values a lax decode path could produce,
// which Finite() must catch and name.
func TestFiniteRejectsNonFiniteIntentFields(t *testing.T) {
	nan := math.NaN()
	posInf := math.Inf(1)
	negInf := math.Inf(-1)
	exportNaN := math.NaN()

	cases := []struct {
		name      string
		msg       interface{ Finite() error }
		wantField string
	}{
		{"EVGoalIntent/TargetSocKwh=NaN", EVGoalIntent{TargetSocKwh: &nan}, "target_soc_kwh"},
		{"EVGoalIntent/InitialSocKwh=+Inf", EVGoalIntent{InitialSocKwh: &posInf}, "initial_soc_kwh"},
		{"EVGoalIntent/CapacityKwh=-Inf", EVGoalIntent{CapacityKwh: &negInf}, "capacity_kwh"},
		{"BackupReserveIntent/ReservePct=NaN", BackupReserveIntent{ReservePct: &nan}, "reserve_pct"},
		{"SolarForecastIntent/StepKw[1]=NaN", SolarForecastIntent{StepKw: []float64{1, math.NaN()}}, "step_kw"},
		{"LoadProfileIntent/StepKw[0]=+Inf", LoadProfileIntent{StepKw: []float64{math.Inf(1)}}, "step_kw"},
		{
			"TariffIntent/ImportPerKwh=NaN",
			TariffIntent{Tariff: TariffSpec{Periods: []TariffPeriod{{ImportPerKwh: math.NaN()}}}},
			"import_per_kwh",
		},
		{
			"TariffIntent/ExportPerKwh=NaN",
			TariffIntent{Tariff: TariffSpec{Periods: []TariffPeriod{{ImportPerKwh: 0.1, ExportPerKwh: &exportNaN}}}},
			"export_per_kwh",
		},
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

// TestFiniteAcceptsValidIntentFields is the accept-side complement: nil
// pointers and genuinely finite values must never be rejected.
func TestFiniteAcceptsValidIntentFields(t *testing.T) {
	if err := (EVGoalIntent{TargetSocKwh: float64ptr(40)}).Finite(); err != nil {
		t.Errorf("EVGoalIntent.Finite() = %v, want nil", err)
	}
	if err := (EVGoalIntent{}).Finite(); err != nil {
		t.Errorf("EVGoalIntent{} (all-nil).Finite() = %v, want nil", err)
	}
	if err := (BackupReserveIntent{ReservePct: float64ptr(20)}).Finite(); err != nil {
		t.Errorf("BackupReserveIntent.Finite() = %v, want nil", err)
	}
	if err := (SolarForecastIntent{StepKw: []float64{1, 2, 3}}).Finite(); err != nil {
		t.Errorf("SolarForecastIntent.Finite() = %v, want nil", err)
	}
	if err := (SolarForecastIntent{}).Finite(); err != nil {
		t.Errorf("SolarForecastIntent{} (empty StepKw).Finite() = %v, want nil", err)
	}
	if err := (LoadProfileIntent{StepKw: []float64{0.1}}).Finite(); err != nil {
		t.Errorf("LoadProfileIntent.Finite() = %v, want nil", err)
	}
	exportRate := 0.05
	valid := TariffIntent{Tariff: TariffSpec{Periods: []TariffPeriod{
		{ImportPerKwh: 0.3, ExportPerKwh: &exportRate},
		{ImportPerKwh: 0.1},
	}}}
	if err := valid.Finite(); err != nil {
		t.Errorf("TariffIntent.Finite() = %v, want nil", err)
	}
}

// TestIntentTopicBuilder covers IntentTopic against every request-kind
// constant (excluding "result", which is a reply topic, not a request kind —
// see IntentTopic's doc comment).
func TestIntentTopicBuilder(t *testing.T) {
	cases := []struct {
		kind string
		want string
	}{
		{"mode", TopicIntentMode},
		{"evgoal", TopicIntentEVGoal},
		{"reserve", TopicIntentReserve},
		{"tariff", TopicIntentTariff},
		{"solarforecast", TopicIntentSolarForecast},
		{"loadprofile", TopicIntentLoadProfile},
		{"chargenow", TopicIntentChargeNow},
	}
	for _, c := range cases {
		if got := IntentTopic(c.kind); got != c.want {
			t.Errorf("IntentTopic(%q) = %q, want %q", c.kind, got, c.want)
		}
	}
}

// TestSupportedVIntentTopics sweeps every new intent/mode/cloudlink topic
// constant against its version constant — the coverage sweep the task
// requires so a future topic added to the "lexa/intent/" family without a
// SupportedV case fails a test instead of silently falling to the default
// (which happens to also be 1 today, masking the gap).
func TestSupportedVIntentTopics(t *testing.T) {
	cases := []struct {
		name  string
		topic string
		want  int
	}{
		{"intent mode", TopicIntentMode, ModeIntentV},
		{"intent evgoal", TopicIntentEVGoal, EVGoalIntentV},
		{"intent reserve", TopicIntentReserve, BackupReserveIntentV},
		{"intent tariff", TopicIntentTariff, TariffIntentV},
		{"intent solarforecast", TopicIntentSolarForecast, SolarForecastIntentV},
		{"intent loadprofile", TopicIntentLoadProfile, LoadProfileIntentV},
		{"intent chargenow", TopicIntentChargeNow, ChargeNowIntentV},
		{"intent result", TopicIntentResult, IntentResultV},
		{"hub mode", TopicHubMode, ModeStatusV},
		{"cloudlink status", TopicCloudlinkStatus, CloudlinkStatusV},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SupportedV(tc.topic); got != tc.want {
				t.Errorf("SupportedV(%q) = %d, want %d", tc.topic, got, tc.want)
			}
		})
	}
}

// TestPubQoSIntentTopicsDefaultToQoS1 proves the ground rule the task
// mandates: no PubQoS change is needed because none of the new topics match
// the QoS-0 measurement-plane prefixes/suffixes, so every one of them falls
// through to PubQoS's `default:` arm (QoS 1).
func TestPubQoSIntentTopicsDefaultToQoS1(t *testing.T) {
	topics := []string{
		TopicIntentMode, TopicIntentEVGoal, TopicIntentReserve, TopicIntentTariff,
		TopicIntentSolarForecast, TopicIntentLoadProfile, TopicIntentChargeNow,
		TopicIntentResult, TopicHubMode, TopicCloudlinkStatus,
	}
	for _, topic := range topics {
		if got := PubQoS(topic); got != QoS1 {
			t.Errorf("PubQoS(%q) = %d, want %d (QoS1 default)", topic, got, QoS1)
		}
	}
}

// TestNoWildcardIntentSubscribeCollision documents and pins the §1.1 design
// rule: TopicIntentResult shares the "lexa/intent/" prefix with every request
// kind, which is exactly why nothing in this package defines a
// "lexa/intent/+" wildcard subscribe constant (unlike SubDesired/
// SubReconcileReport for their families) — a subscriber must list each kind
// topic explicitly. This test proves the prefix collision is real (the
// premise for that rule), not merely asserted in a comment.
func TestNoWildcardIntentSubscribeCollision(t *testing.T) {
	if !strings.HasPrefix(TopicIntentResult, "lexa/intent/") {
		t.Fatalf("TopicIntentResult = %q, expected it to share the lexa/intent/ prefix with request kinds "+
			"(this is the reason no lexa/intent/+ wildcard constant exists)", TopicIntentResult)
	}
}
