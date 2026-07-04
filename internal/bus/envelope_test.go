package bus

import (
	"encoding/json"
	"sync"
	"testing"
)

// resetVersionRejects clears package-level counter state between test cases
// so each test starts from a known baseline. rejectCounters is a package var
// (sync.Map by value), so reassigning it is legal from within this package.
func resetVersionRejects(t *testing.T) {
	t.Helper()
	rejectCounters = sync.Map{}
}

func TestCheckVersion(t *testing.T) {
	const supported = 2

	cases := []struct {
		name           string
		payload        string
		legacyAccepted bool
		wantErr        bool
		wantGot        int // only checked when wantErr
	}{
		{
			name:           "absent v, legacy accepted",
			payload:        `{"device":"bat0"}`,
			legacyAccepted: true,
			wantErr:        false,
		},
		{
			name:           "absent v, legacy not accepted",
			payload:        `{"device":"bat0"}`,
			legacyAccepted: false,
			wantErr:        true,
			wantGot:        0,
		},
		{
			name:           "v=1 (within supported)",
			payload:        `{"v":1,"device":"bat0"}`,
			legacyAccepted: true,
			wantErr:        false,
		},
		{
			name:           "v=supported",
			payload:        `{"v":2,"device":"bat0"}`,
			legacyAccepted: true,
			wantErr:        false,
		},
		{
			name:           "v=supported+1",
			payload:        `{"v":3,"device":"bat0"}`,
			legacyAccepted: true,
			wantErr:        true,
			wantGot:        3,
		},
		{
			name:           "v negative",
			payload:        `{"v":-1,"device":"bat0"}`,
			legacyAccepted: true,
			wantErr:        true,
			wantGot:        -1,
		},
		{
			name:           "v non-integer (string)",
			payload:        `{"v":"nope","device":"bat0"}`,
			legacyAccepted: true,
			wantErr:        false, // peek unmarshal fails -> defer to real unmarshal, per design
		},
		{
			name:           "malformed JSON",
			payload:        `{"device":`,
			legacyAccepted: true,
			wantErr:        false, // same: not CheckVersion's job to flag
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := LegacyV0Accepted
			LegacyV0Accepted = tc.legacyAccepted
			defer func() { LegacyV0Accepted = old }()

			err := CheckVersion("lexa/test/topic", []byte(tc.payload), supported)
			if tc.wantErr && err == nil {
				t.Fatalf("CheckVersion(%q) = nil, want error", tc.payload)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("CheckVersion(%q) = %v, want nil", tc.payload, err)
			}
			if tc.wantErr {
				verr, ok := err.(*VersionError)
				if !ok {
					t.Fatalf("CheckVersion(%q) error type = %T, want *VersionError", tc.payload, err)
				}
				if verr.Got != tc.wantGot {
					t.Errorf("VersionError.Got = %d, want %d", verr.Got, tc.wantGot)
				}
				if verr.Supported != supported {
					t.Errorf("VersionError.Supported = %d, want %d", verr.Supported, supported)
				}
				if verr.Topic != "lexa/test/topic" {
					t.Errorf("VersionError.Topic = %q, want %q", verr.Topic, "lexa/test/topic")
				}
			}
		})
	}
}

// TestCheckVersionDoesNotMaskMalformedJSON pins the documented design choice:
// CheckVersion is a version gate only, not a JSON validator. A malformed or
// wrongly-typed "v" field must not be reported by CheckVersion; it must
// surface later, from the real json.Unmarshal into the message type, exactly
// as it does today (pre-envelope).
func TestCheckVersionDoesNotMaskMalformedJSON(t *testing.T) {
	payloads := []string{
		`not json at all`,
		`{"v":"1"}`,
		`{"v":[1,2]}`,
		`{"v":{"nested":1}}`,
		``,
	}
	for _, p := range payloads {
		if err := CheckVersion("lexa/test/topic", []byte(p), 1); err != nil {
			t.Errorf("CheckVersion(%q) = %v, want nil (malformed detection belongs to the real unmarshal)", p, err)
		}
		// Confirm the real unmarshal path does in fact fail on these, so the
		// deferred error is not lost -- it just surfaces one call later.
		var v struct {
			V int `json:"v"`
		}
		if err := json.Unmarshal([]byte(p), &v); err == nil {
			t.Errorf("expected json.Unmarshal(%q) to fail (test payload choice invalid)", p)
		}
	}
}

func TestLegacyV0AcceptedDefaultIsTrue(t *testing.T) {
	if !LegacyV0Accepted {
		t.Fatal("LegacyV0Accepted default must be true (transition tolerance) unless deliberately flipped by TASK-018")
	}
}

func TestRejectAndAlarmCounts(t *testing.T) {
	resetVersionRejects(t)

	err := &VersionError{Topic: "lexa/test/counts", Got: 5, Supported: 1}
	for i := 0; i < 7; i++ {
		RejectAndAlarm(err)
	}

	got := VersionRejects()["lexa/test/counts"]
	if got != 7 {
		t.Fatalf("VersionRejects()[topic] = %d, want 7", got)
	}
}

func TestRejectAndAlarmPerTopicIsolation(t *testing.T) {
	resetVersionRejects(t)

	RejectAndAlarm(&VersionError{Topic: "lexa/test/a", Got: 5, Supported: 1})
	RejectAndAlarm(&VersionError{Topic: "lexa/test/a", Got: 5, Supported: 1})
	RejectAndAlarm(&VersionError{Topic: "lexa/test/b", Got: 5, Supported: 1})

	snap := VersionRejects()
	if snap["lexa/test/a"] != 2 {
		t.Errorf("topic a count = %d, want 2", snap["lexa/test/a"])
	}
	if snap["lexa/test/b"] != 1 {
		t.Errorf("topic b count = %d, want 1", snap["lexa/test/b"])
	}
}

func TestRejectAndAlarmNilIsNoOp(t *testing.T) {
	resetVersionRejects(t)
	RejectAndAlarm(nil) // must not panic or record anything
	if len(VersionRejects()) != 0 {
		t.Fatalf("VersionRejects() after nil RejectAndAlarm = %v, want empty", VersionRejects())
	}
}

// TestRejectAndAlarmRateLimitSignature makes the "first + every Nth" log
// rate-limit deterministic by shrinking logEveryN for the duration of the
// test, rather than firing 100+ real rejections. It asserts on the counter
// (the only externally observable, deterministic effect) since log output
// itself isn't captured here; the rate-limit arithmetic (n==1 || n%logEveryN==0)
// is exercised by driving n across a boundary.
func TestRejectAndAlarmRateLimitSignature(t *testing.T) {
	resetVersionRejects(t)

	oldN := logEveryN
	logEveryN = 3
	defer func() { logEveryN = oldN }()

	err := &VersionError{Topic: "lexa/test/ratelimit", Got: 9, Supported: 1}
	for i := 0; i < 6; i++ {
		RejectAndAlarm(err)
	}

	got := VersionRejects()["lexa/test/ratelimit"]
	if got != 6 {
		t.Fatalf("VersionRejects()[topic] = %d, want 6 (counter increments every call regardless of log rate-limit)", got)
	}
	// The would-be-logged occurrences are n=1 (first) and n=3, n=6 (every
	// 3rd); this test pins that the counter keeps incrementing past the log
	// boundary rather than saturating or resetting.
}

// envelopeCarrier is a minimal struct embedding Envelope, used only to prove
// the JSON round-trip contract: "v" appears when set, is omitted when zero,
// and coexists cleanly with the *float64/no-NaN bus convention (nan_test.go).
type envelopeCarrier struct {
	Envelope
	Device string   `json:"device"`
	W      *float64 `json:"w,omitempty"`
}

func TestEnvelopeJSONRoundTrip(t *testing.T) {
	t.Run("V set marshals v", func(t *testing.T) {
		msg := envelopeCarrier{Envelope: Envelope{V: 1}, Device: "bat0", W: float64ptr(100)}
		b, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		var round map[string]any
		if err := json.Unmarshal(b, &round); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		v, ok := round["v"]
		if !ok {
			t.Fatalf("marshaled JSON %s missing \"v\" key", b)
		}
		if v != float64(1) {
			t.Errorf("v = %v, want 1", v)
		}
	})

	t.Run("V zero omits v", func(t *testing.T) {
		msg := envelopeCarrier{Device: "bat0", W: float64ptr(100)}
		b, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		var round map[string]any
		if err := json.Unmarshal(b, &round); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if _, ok := round["v"]; ok {
			t.Errorf("marshaled JSON %s has \"v\" key, want omitted for zero-value Envelope", b)
		}
	})

	t.Run("nil W does not become NaN, with or without v set", func(t *testing.T) {
		for _, v := range []int{0, 1} {
			msg := envelopeCarrier{Envelope: Envelope{V: v}, Device: "bat0", W: nil}
			b, err := json.Marshal(msg)
			if err != nil {
				t.Fatalf("Marshal(v=%d): %v", v, err)
			}
			if containsNaN(string(b)) {
				t.Errorf("Marshal(v=%d) = %s, contains NaN literal", v, b)
			}
		}
	})
}

func TestEnvelopeUnmarshalRoundTrip(t *testing.T) {
	in := envelopeCarrier{Envelope: Envelope{V: 1}, Device: "bat0", W: float64ptr(42.5)}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out envelopeCarrier
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.V != 1 || out.Device != "bat0" || out.W == nil || *out.W != 42.5 {
		t.Errorf("round-trip mismatch: got %+v", out)
	}
}
