package reconcile

// WP-10: SetDesiredFields — the AD-013 gate for document types beyond
// bus.DesiredState (the adv shell's per-axis opinion sets). These tests pin
// that the generic entry point applies the IDENTICAL gate semantics
// (staleness, seq/issuedAt regression, publisher-restart SeqReset, NaN
// defense) and the same new-target/refresh Action behavior.

import (
	"math"
	"testing"
	"time"
)

func advFields(v float64) map[Field]float64 {
	return map[Field]float64{AdvContent: v}
}

func TestSetDesiredFields_AcceptAndWrite(t *testing.T) {
	r := New("adv", "inv-0", Config{})
	t0 := time.Now()

	a, reps := r.SetDesiredFields(DocMeta{MRID: "m1", Seq: 1, IssuedAt: t0.Unix()}, advFields(42), t0)
	if a.Kind != ActionWrite {
		t.Fatalf("new target must Write, got %v", a.Kind)
	}
	if len(reps) != 0 {
		t.Fatalf("clean accept must not report, got %v", reps)
	}
	if a.Fields[AdvContent] != 42 {
		t.Errorf("write fields = %v", a.Fields)
	}

	// Same-target refresh (heartbeat): no write.
	a, _ = r.SetDesiredFields(DocMeta{MRID: "m1", Seq: 2, IssuedAt: t0.Unix() + 10}, advFields(42), t0.Add(10*time.Second))
	if a.Kind != ActionNone {
		t.Fatalf("same-target refresh must be None, got %v", a.Kind)
	}

	// Content change: write again.
	a, _ = r.SetDesiredFields(DocMeta{MRID: "m1", Seq: 3, IssuedAt: t0.Unix() + 20}, advFields(43), t0.Add(20*time.Second))
	if a.Kind != ActionWrite {
		t.Fatalf("changed target must Write, got %v", a.Kind)
	}
}

func TestSetDesiredFields_GateRejections(t *testing.T) {
	t0 := time.Now()
	cases := []struct {
		name   string
		meta   DocMeta
		fields map[Field]float64
		at     time.Time
		want   RejectReason
	}{
		{"stale", DocMeta{Seq: 5, IssuedAt: t0.Add(-10 * time.Minute).Unix()}, advFields(1), t0, RejectStale},
		{"nan", DocMeta{Seq: 5, IssuedAt: t0.Unix()}, advFields(math.NaN()), t0, RejectNaN},
		{"inf", DocMeta{Seq: 5, IssuedAt: t0.Unix()}, advFields(math.Inf(1)), t0, RejectNaN},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := New("adv", "inv-0", Config{})
			a, reps := r.SetDesiredFields(tc.meta, tc.fields, tc.at)
			if a.Kind != ActionNone {
				t.Fatalf("rejected doc must be None, got %v", a.Kind)
			}
			if len(reps) != 1 || reps[0].Kind != ReportRejectedDoc || reps[0].Reject != tc.want {
				t.Fatalf("want RejectedDoc/%v, got %+v", tc.want, reps)
			}
		})
	}
}

func TestSetDesiredFields_ReplayAndSeqReset(t *testing.T) {
	r := New("adv", "inv-0", Config{})
	t0 := time.Now()

	if a, _ := r.SetDesiredFields(DocMeta{MRID: "m1", Seq: 5, IssuedAt: t0.Unix()}, advFields(1), t0); a.Kind != ActionWrite {
		t.Fatal("initial accept must Write")
	}

	// Replay (seq and issuedAt both not newer): rejected, state unchanged.
	a, reps := r.SetDesiredFields(DocMeta{MRID: "m1", Seq: 5, IssuedAt: t0.Unix()}, advFields(9), t0.Add(time.Second))
	if a.Kind != ActionNone || len(reps) != 1 || reps[0].Reject != RejectSeqRegression {
		t.Fatalf("replay must reject with SeqRegression, got %v %v", a.Kind, reps)
	}

	// Publisher restart: lower seq, strictly newer issuedAt — accepted with a
	// SeqReset report; the new content writes.
	a, reps = r.SetDesiredFields(DocMeta{MRID: "m1", Seq: 0, IssuedAt: t0.Unix() + 30}, advFields(9), t0.Add(30*time.Second))
	if a.Kind != ActionWrite {
		t.Fatalf("seq-reset accept must Write, got %v", a.Kind)
	}
	found := false
	for _, rep := range reps {
		if rep.Kind == ReportSeqReset {
			found = true
		}
	}
	if !found {
		t.Fatalf("publisher restart must report SeqReset, got %v", reps)
	}
}

// TestSetDesiredFields_ObserveConvergence closes the loop: an AdvContent
// readback equal to desired converges (boolean-exact tolerance), a different
// fingerprint diverges and triggers the corrective-write/backoff machinery.
func TestSetDesiredFields_ObserveConvergence(t *testing.T) {
	// The WP-10 shell's pacing: first retry tier ≥ the readback interval.
	r := New("adv", "inv-0", Config{ConvergeTimeout: time.Second, RetryBackoff: []time.Duration{15 * time.Second}})
	t0 := time.Now()
	r.SetDesiredFields(DocMeta{MRID: "m1", Seq: 1, IssuedAt: t0.Unix()}, advFields(42), t0)

	a, _ := r.Observe(Observed{Read: advFields(42), Connected: true, Plausible: true, At: t0.Add(time.Second)}, t0.Add(time.Second))
	if a.Kind != ActionNone {
		t.Fatalf("matching fingerprint must converge, got %v", a.Kind)
	}

	// Mismatch (desired+1 — advMismatch semantics): a NEW divergence episode's
	// first corrective write is immediate (core contract: writeAttempts resets
	// on episode open); subsequent retries pace on the backoff.
	a, _ = r.Observe(Observed{Read: advFields(43), Connected: true, Plausible: true, At: t0.Add(2 * time.Second)}, t0.Add(2*time.Second))
	if a.Kind != ActionWrite {
		t.Fatalf("first observed divergence must corrective-write immediately, got %v", a.Kind)
	}
	if a, _ := r.Tick(t0.Add(10 * time.Second)); a.Kind != ActionNone {
		t.Fatalf("Tick inside the 15s backoff must not retry, got %v", a.Kind)
	}
	a, reps := r.Tick(t0.Add(18 * time.Second))
	if a.Kind != ActionWrite {
		t.Fatalf("Tick past backoff must retry-write, got %v (reports %v)", a.Kind, reps)
	}
	// Episode accessor: divergence persisted past ConvergeTimeout ⇒ episode
	// opened, counter visible via Episode().
	if r.Episode() != 1 {
		t.Errorf("Episode() = %d, want 1", r.Episode())
	}
}
