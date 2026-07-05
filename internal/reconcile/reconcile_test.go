package reconcile

import (
	"testing"
	"time"

	"lexa-hub/internal/bus"
)

// base is the fixed epoch for the fake clock; every event carries an injected
// time.Time derived from it — the package never reads a real clock.
var base = time.Unix(1_700_000_000, 0)

func at(d time.Duration) time.Time   { return base.Add(d) }
func issuedAt(d time.Duration) int64 { return base.Add(d).Unix() }

func fptr(v float64) *float64 { return &v }
func bptr(v bool) *bool       { return &v }

// battDoc builds a battery desired doc with a SetpointW opinion.
func battDoc(seq uint64, issued int64, setpoint float64) bus.DesiredState {
	return bus.DesiredState{
		Envelope:    bus.Envelope{V: bus.DesiredStateV},
		DeviceClass: bus.DesiredClassBattery,
		DeviceID:    "batt-0",
		SetpointW:   fptr(setpoint),
		Source:      "economic",
		MRID:        "mrid-1",
		IssuedAt:    issued,
		Seq:         seq,
	}
}

func obs(connected, plausible bool, when time.Time, read map[Field]float64) Observed {
	return Observed{Read: read, Connected: connected, At: when, Plausible: plausible}
}

// good is a connected+plausible readback with the given field values.
func good(when time.Time, read map[Field]float64) Observed {
	return obs(true, true, when, read)
}

// testCfg is a fast, fully-specified config for deterministic tests.
func testCfg() Config {
	return Config{
		ReadbackTolerance: map[Field]float64{SetpointW: 10},
		ConvergeTimeout:   30 * time.Second,
		RetryBackoff:      []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second},
		StaleAfter:        300 * time.Second,
		ReassertEvery:     60 * time.Second,
	}
}

func mustWrite(t *testing.T, a Action, reason string) {
	t.Helper()
	if a.Kind != ActionWrite {
		t.Fatalf("want Write(%q), got %s", reason, a.Kind)
	}
	if reason != "" && a.Reason != reason {
		t.Fatalf("want Write reason %q, got %q", reason, a.Reason)
	}
}

func mustNone(t *testing.T, a Action) {
	t.Helper()
	if a.Kind != ActionNone {
		t.Fatalf("want None, got Write %v (%s)", a.Fields, a.Reason)
	}
}

func wantField(t *testing.T, a Action, f Field, v float64) {
	t.Helper()
	got, ok := a.Fields[f]
	if !ok {
		t.Fatalf("Write missing field %s", f)
	}
	if got != v {
		t.Fatalf("Write field %s = %v, want %v", f, got, v)
	}
}

func countKind(rs []Report, k ReportKind) int {
	n := 0
	for _, r := range rs {
		if r.Kind == k {
			n++
		}
	}
	return n
}

func firstKind(rs []Report, k ReportKind) (Report, bool) {
	for _, r := range rs {
		if r.Kind == k {
			return r, true
		}
	}
	return Report{}, false
}

// -------------------------------------------------------------------------
// Background bullet: write-on-diff (Observe diverging → Write; within → None)
// -------------------------------------------------------------------------

func TestWriteOnDiff(t *testing.T) {
	r := New(bus.DesiredClassBattery, "batt-0", testCfg())
	a, _ := r.SetDesired(battDoc(1, issuedAt(0), 1000), at(0))
	mustWrite(t, a, "new-desired")
	wantField(t, a, SetpointW, 1000)

	// Diverged readback → corrective write.
	a, reps := r.Observe(good(at(1*time.Second), map[Field]float64{SetpointW: 0}), at(1*time.Second))
	mustWrite(t, a, "write-on-diff")
	wantField(t, a, SetpointW, 1000)
	if len(reps) != 0 {
		t.Fatalf("no reports expected yet, got %v", reps)
	}

	// Converged readback → None, and marks converged.
	a, _ = r.Observe(good(at(2*time.Second), map[Field]float64{SetpointW: 1000}), at(2*time.Second))
	mustNone(t, a)
	if !r.converged {
		t.Fatal("expected converged after matching readback")
	}
}

func TestWithinToleranceBoundary(t *testing.T) {
	// SetpointW tolerance is 10; exactly-at-tolerance is converged.
	for _, tc := range []struct {
		name string
		read float64
		conv bool
	}{
		{"below", 995, true},
		{"exact-high", 1010, true}, // |1010-1000| == 10, inclusive
		{"exact-low", 990, true},
		{"over", 1011, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := New(bus.DesiredClassBattery, "batt-0", testCfg())
			r.SetDesired(battDoc(1, issuedAt(0), 1000), at(0))
			a, _ := r.Observe(good(at(1*time.Second), map[Field]float64{SetpointW: tc.read}), at(1*time.Second))
			if tc.conv {
				mustNone(t, a)
				if !r.converged {
					t.Fatalf("read %v within tolerance should converge", tc.read)
				}
			} else {
				mustWrite(t, a, "write-on-diff")
			}
		})
	}
}

// -------------------------------------------------------------------------
// Background bullet: verify-by-readback — convergence never from write-ACK
// -------------------------------------------------------------------------

func TestConvergenceFromReadbackNotAck(t *testing.T) {
	r := New(bus.DesiredClassBattery, "batt-0", testCfg())
	r.SetDesired(battDoc(1, issuedAt(0), 1000), at(0)) // this Write is a command, not evidence

	// No matter how many writes go out, the device is not "converged" until a
	// plausible readback proves it. Drive diverged reads with time advancing.
	for i := 1; i <= 3; i++ {
		when := at(time.Duration(i) * 10 * time.Second)
		r.Observe(good(when, map[Field]float64{SetpointW: 0}), when)
		if r.converged {
			t.Fatalf("must not be converged from writes alone (iter %d)", i)
		}
	}
	// Only a matching measurement flips it.
	when := at(40 * time.Second)
	a, _ := r.Observe(good(when, map[Field]float64{SetpointW: 1000}), when)
	mustNone(t, a)
	if !r.converged {
		t.Fatal("matching readback should converge")
	}
}

// -------------------------------------------------------------------------
// Background bullet: implausible readback held (no write storm, plausibleW)
// -------------------------------------------------------------------------

func TestImplausibleReadbackHeld(t *testing.T) {
	r := New(bus.DesiredClassBattery, "batt-0", testCfg())
	r.SetDesired(battDoc(1, issuedAt(0), 1000), at(0))

	// Establish observed divergence.
	a, _ := r.Observe(good(at(1*time.Second), map[Field]float64{SetpointW: 0}), at(1*time.Second))
	mustWrite(t, a, "write-on-diff")
	if r.divergentSince.IsZero() {
		t.Fatal("expected divergence recorded")
	}

	// An implausible "converged-looking" sample must NOT close the divergence.
	a, reps := r.Observe(obs(true, false, at(2*time.Second), map[Field]float64{SetpointW: 1000}), at(2*time.Second))
	mustNone(t, a)
	if len(reps) != 0 {
		t.Fatalf("implausible sample should emit no reports, got %v", reps)
	}
	if r.divergentSince.IsZero() || r.converged {
		t.Fatal("implausible sample must hold previous (diverged) assessment")
	}

	// A retry still fires later — proof the assessment is still 'diverged'.
	a, _ = r.Tick(at(4 * time.Second)) // 2s after the write at t=1 → backoff[0] ready
	mustWrite(t, a, "retry")

	// A plausible converged sample finally closes it.
	a, _ = r.Observe(good(at(5*time.Second), map[Field]float64{SetpointW: 1000}), at(5*time.Second))
	mustNone(t, a)
	if !r.converged {
		t.Fatal("plausible match should converge")
	}
}

func TestImplausibleDoesNotStormWhenConverged(t *testing.T) {
	r := New(bus.DesiredClassBattery, "batt-0", testCfg())
	r.SetDesired(battDoc(1, issuedAt(0), 1000), at(0))
	r.Observe(good(at(1*time.Second), map[Field]float64{SetpointW: 1000}), at(1*time.Second))
	// Implausible junk while converged → held, no write.
	a, reps := r.Observe(obs(true, false, at(2*time.Second), map[Field]float64{SetpointW: 0}), at(2*time.Second))
	mustNone(t, a)
	if len(reps) != 0 {
		t.Fatalf("no reports for implausible sample, got %v", reps)
	}
}

// -------------------------------------------------------------------------
// Background bullet: reassert-on-reconnect (L4)
// -------------------------------------------------------------------------

func TestReassertOnReconnect(t *testing.T) {
	r := New(bus.DesiredClassBattery, "batt-0", testCfg())
	r.SetDesired(battDoc(1, issuedAt(0), 1000), at(0))
	// Converge first, so there is no pending divergence — reconnect must STILL write.
	r.Observe(good(at(1*time.Second), map[Field]float64{SetpointW: 1000}), at(1*time.Second))

	a, _ := r.Reconnected(at(2 * time.Second))
	mustWrite(t, a, "reconnect-reassert")
	wantField(t, a, SetpointW, 1000)
	if r.converged {
		t.Fatal("reconnect must drop the stale assessment until re-proven")
	}
}

func TestReconnectNoDesiredIsNoop(t *testing.T) {
	r := New(bus.DesiredClassBattery, "batt-0", testCfg())
	a, reps := r.Reconnected(at(0))
	mustNone(t, a)
	if reps != nil {
		t.Fatalf("no reports without a standing desired, got %v", reps)
	}
}

// -------------------------------------------------------------------------
// Background bullet: escalating retry backoff (schedule + exhaustion)
// -------------------------------------------------------------------------

func TestEscalatingRetryBackoff(t *testing.T) {
	r := New(bus.DesiredClassBattery, "batt-0", testCfg())
	r.SetDesired(battDoc(1, issuedAt(0), 1000), at(0))

	// First diverged observe writes immediately at t=0.
	a, _ := r.Observe(good(at(0), map[Field]float64{SetpointW: 0}), at(0))
	mustWrite(t, a, "write-on-diff")
	last := time.Duration(0)

	// Expected gaps between successive writes: 2,5,15,30,30 (last repeats).
	for i, gap := range []time.Duration{2, 5, 15, 30, 30} {
		g := gap * time.Second
		// Just before the gap: no write.
		early := last + g - time.Second
		a, _ = r.Tick(at(early))
		if a.Kind == ActionWrite {
			t.Fatalf("step %d: unexpected early retry at +%v", i, early)
		}
		// At the gap boundary: retry write.
		a, _ = r.Tick(at(last + g))
		mustWrite(t, a, "retry")
		last += g
	}
}

// -------------------------------------------------------------------------
// Background bullet: non-convergence report edges (once-per-episode, L5)
// -------------------------------------------------------------------------

func TestNonConvergenceEpisodeEdges(t *testing.T) {
	r := New(bus.DesiredClassBattery, "batt-0", testCfg()) // ConvergeTimeout 30s
	r.SetDesired(battDoc(1, issuedAt(0), 1000), at(0))
	r.Observe(good(at(0), map[Field]float64{SetpointW: 0}), at(0)) // divergence at t=0

	var all []Report
	// Drive ticks across the timeout; collect all reports.
	for s := 1; s <= 45; s++ {
		_, reps := r.Tick(at(time.Duration(s) * time.Second))
		all = append(all, reps...)
	}
	if got := countKind(all, ReportNonConvergedBegin); got != 1 {
		t.Fatalf("want exactly 1 NonConvergedBegin, got %d", got)
	}
	begin, _ := firstKind(all, ReportNonConvergedBegin)
	if begin.At != at(30*time.Second) {
		t.Fatalf("begin should fire at +30s, got %v", begin.At.Sub(base))
	}
	if begin.Episode != 1 {
		t.Fatalf("first episode should be #1, got %d", begin.Episode)
	}
	if begin.MRID != "mrid-1" || begin.Seq != 1 || begin.DeviceID != "batt-0" {
		t.Fatalf("begin missing attribution: %+v", begin)
	}

	// Re-converge → exactly one End, same episode number.
	_, reps := r.Observe(good(at(50*time.Second), map[Field]float64{SetpointW: 1000}), at(50*time.Second))
	if got := countKind(reps, ReportNonConvergedEnd); got != 1 {
		t.Fatalf("want exactly 1 NonConvergedEnd, got %d (%v)", got, reps)
	}
	end, _ := firstKind(reps, ReportNonConvergedEnd)
	if end.Episode != 1 {
		t.Fatalf("End should share episode #1, got %d", end.Episode)
	}

	// Further ticks emit nothing (episode closed, converged).
	_, reps = r.Tick(at(60 * time.Second))
	if len(reps) != 0 {
		t.Fatalf("no reports after close, got %v", reps)
	}
}

func TestNoEpisodeIfConvergesBeforeTimeout(t *testing.T) {
	r := New(bus.DesiredClassBattery, "batt-0", testCfg())
	r.SetDesired(battDoc(1, issuedAt(0), 1000), at(0))
	r.Observe(good(at(0), map[Field]float64{SetpointW: 0}), at(0))
	// Converge at +10s, before the 30s timeout.
	_, reps := r.Observe(good(at(10*time.Second), map[Field]float64{SetpointW: 1000}), at(10*time.Second))
	if len(reps) != 0 {
		t.Fatalf("no begin/end when never non-converged, got %v", reps)
	}
	// Ticks past the old timeout emit nothing.
	_, reps = r.Tick(at(40 * time.Second))
	if countKind(reps, ReportNonConvergedBegin) != 0 {
		t.Fatal("must not begin an episode after re-convergence")
	}
}

// -------------------------------------------------------------------------
// Background bullet: staleness — hold + report after StaleAfter, never clear
// -------------------------------------------------------------------------

func TestStalenessHoldsAndReports(t *testing.T) {
	r := New(bus.DesiredClassBattery, "batt-0", testCfg()) // StaleAfter 300s
	r.SetDesired(battDoc(1, issuedAt(0), 1000), at(0))

	_, reps := r.Tick(at(299 * time.Second))
	if countKind(reps, ReportStaleDesired) != 0 {
		t.Fatal("stale report fired too early")
	}
	_, reps = r.Tick(at(300 * time.Second))
	if countKind(reps, ReportStaleDesired) != 1 {
		t.Fatalf("want 1 StaleDesired at +300s, got %v", reps)
	}
	stale, _ := firstKind(reps, ReportStaleDesired)
	if stale.Seq != 1 || stale.MRID != "mrid-1" {
		t.Fatalf("stale report missing attribution: %+v", stale)
	}
	// Reported once only.
	_, reps = r.Tick(at(400 * time.Second))
	if countKind(reps, ReportStaleDesired) != 0 {
		t.Fatal("StaleDesired must fire once per stale period")
	}
	// Desired is HELD, not cleared: reconnect still asserts the held setpoint.
	a, _ := r.Reconnected(at(401 * time.Second))
	mustWrite(t, a, "reconnect-reassert")
	wantField(t, a, SetpointW, 1000)
}

func TestStaleThenFreshRecovery(t *testing.T) {
	r := New(bus.DesiredClassBattery, "batt-0", testCfg())
	r.SetDesired(battDoc(1, issuedAt(0), 1000), at(0))
	_, reps := r.Tick(at(300 * time.Second))
	if countKind(reps, ReportStaleDesired) != 1 {
		t.Fatal("expected first stale report")
	}
	// A fresh doc re-arms the staleness timer.
	r.SetDesired(battDoc(2, issuedAt(300*time.Second), 1000), at(300*time.Second))
	_, reps = r.Tick(at(599 * time.Second))
	if countKind(reps, ReportStaleDesired) != 0 {
		t.Fatal("stale must re-arm from the fresh doc")
	}
	_, reps = r.Tick(at(600 * time.Second))
	if countKind(reps, ReportStaleDesired) != 1 {
		t.Fatalf("want stale again 300s after the fresh doc, got %v", reps)
	}
}
