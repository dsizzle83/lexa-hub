package reconcile

import (
	"math"
	"testing"
	"time"

	"lexa-hub/internal/bus"
)

// -------------------------------------------------------------------------
// Background bullet: AD-013 rejection (regression) + SeqReset (publisher restart)
// -------------------------------------------------------------------------

func TestRejectSeqIssuedRegression(t *testing.T) {
	r := New(bus.DesiredClassBattery, "batt-0", testCfg())
	r.SetDesired(battDoc(5, issuedAt(10*time.Second), 1000), at(10*time.Second))

	// seq<=5 AND issued<=t+10 → replay, reject, state unchanged.
	a, reps := r.SetDesired(battDoc(5, issuedAt(10*time.Second), 2000), at(11*time.Second))
	mustNone(t, a)
	rej, ok := firstKind(reps, ReportRejectedDoc)
	if !ok || rej.Reject != RejectSeqRegression {
		t.Fatalf("want RejectedDoc/SeqRegression, got %v", reps)
	}
	if r.desired.SetpointW == nil || *r.desired.SetpointW != 1000 {
		t.Fatal("rejected doc must not change the standing desired")
	}
	// Lower seq, older issued → also rejected.
	_, reps = r.SetDesired(battDoc(3, issuedAt(5*time.Second), 3000), at(12*time.Second))
	if rej, ok := firstKind(reps, ReportRejectedDoc); !ok || rej.Reject != RejectSeqRegression {
		t.Fatalf("want SeqRegression, got %v", reps)
	}
	// Higher seq → accepted.
	a, _ = r.SetDesired(battDoc(6, issuedAt(10*time.Second), 4000), at(13*time.Second))
	mustWrite(t, a, "new-desired")
	if *r.desired.SetpointW != 4000 {
		t.Fatal("higher seq should be adopted")
	}
}

func TestSeqResetPublisherRestart(t *testing.T) {
	r := New(bus.DesiredClassBattery, "batt-0", testCfg())
	r.SetDesired(battDoc(5, issuedAt(10*time.Second), 1000), at(10*time.Second))

	// Publisher restarts: seq resets to 0 but wall clock advanced → accept + SeqReset.
	a, reps := r.SetDesired(battDoc(0, issuedAt(20*time.Second), 2000), at(20*time.Second))
	mustWrite(t, a, "new-desired") // different target → re-write
	sr, ok := firstKind(reps, ReportSeqReset)
	if !ok {
		t.Fatalf("want SeqReset report, got %v", reps)
	}
	if sr.Seq != 0 || sr.IssuedAt != issuedAt(20*time.Second) {
		t.Fatalf("SeqReset should carry the new (reset) baseline: %+v", sr)
	}
	if r.lastAppliedSeq != 0 || r.lastAppliedIssuedAt != issuedAt(20*time.Second) {
		t.Fatal("baseline must advance to the accepted reset doc")
	}

	// A second restart increments the SeqReset count (counter monotonicity of the signal).
	_, reps = r.SetDesired(battDoc(0, issuedAt(30*time.Second), 3000), at(30*time.Second))
	if countKind(reps, ReportSeqReset) != 1 {
		t.Fatalf("second restart should emit another SeqReset, got %v", reps)
	}

	// Normal forward progress after a reset is NOT a SeqReset.
	_, reps = r.SetDesired(battDoc(1, issuedAt(30*time.Second), 3000), at(31*time.Second))
	if countKind(reps, ReportSeqReset) != 0 {
		t.Fatalf("forward seq must not signal SeqReset, got %v", reps)
	}
}

func TestSeqResetSameTargetNoWrite(t *testing.T) {
	r := New(bus.DesiredClassBattery, "batt-0", testCfg())
	r.SetDesired(battDoc(5, issuedAt(10*time.Second), 1000), at(10*time.Second))
	// Restart re-publishing the SAME intent: SeqReset report, but no re-write.
	a, reps := r.SetDesired(battDoc(0, issuedAt(20*time.Second), 1000), at(20*time.Second))
	mustNone(t, a)
	if countKind(reps, ReportSeqReset) != 1 {
		t.Fatalf("want SeqReset, got %v", reps)
	}
}

// TestHeartbeatRefreshSameTargetNoWrite is WS-2 fix 1's reconciler-side half:
// a hub re-stamping IssuedAt/Seq on an UNCHANGED standing intent (the
// desiredHeartbeatInterval re-publish in cmd/hub/desired.go — normal forward
// seq progress, not a publisher-restart SeqReset) must advance the staleness
// baseline (so the doc never ages past StaleAfter) WITHOUT re-issuing a write
// — i.e. the content-dedupe to hardware holds; no register churn from a
// heartbeat. This is the "higherSeq" branch of SetDesired, distinct from
// TestSeqResetSameTargetNoWrite's "newerIssued only" (seq reset) branch.
func TestHeartbeatRefreshSameTargetNoWrite(t *testing.T) {
	r := New(bus.DesiredClassBattery, "batt-0", testCfg())
	a, _ := r.SetDesired(battDoc(0, issuedAt(0), 1000), at(0))
	mustWrite(t, a, "new-desired") // first doc: unconditional write

	// A heartbeat re-stamp: same content, seq advances normally, issuedAt newer.
	a, reps := r.SetDesired(battDoc(1, issuedAt(150*time.Second), 1000), at(150*time.Second))
	mustNone(t, a) // no register write — content unchanged
	if countKind(reps, ReportSeqReset) != 0 {
		t.Fatalf("normal forward seq progress must not report SeqReset, got %v", reps)
	}
	if r.lastAppliedSeq != 1 || r.lastAppliedIssuedAt != issuedAt(150*time.Second) {
		t.Fatal("heartbeat refresh must still advance the seq/issuedAt staleness baseline")
	}

	// Staleness never fires because the heartbeat kept re-arming the timer
	// (composes with WS-2 fix 1's ≤StaleAfter/2 cadence — see
	// cmd/hub/desired.go's desiredHeartbeatInterval doc).
	_, reps = r.Tick(at(300 * time.Second))
	if countKind(reps, ReportStaleDesired) != 0 {
		t.Fatalf("a live heartbeat re-stamp must prevent StaleDesired, got %v", reps)
	}

	// Another heartbeat at +300s (150s after the last one): still no write,
	// still re-arms.
	a, _ = r.SetDesired(battDoc(2, issuedAt(300*time.Second), 1000), at(300*time.Second))
	mustNone(t, a)
	_, reps = r.Tick(at(450 * time.Second))
	if countKind(reps, ReportStaleDesired) != 0 {
		t.Fatalf("second heartbeat must again prevent StaleDesired at the next 300s mark, got %v", reps)
	}
}

// -------------------------------------------------------------------------
// Background bullet: reject stale doc (issuedAt older than the bound)
// -------------------------------------------------------------------------

func TestRejectStaleDoc(t *testing.T) {
	r := New(bus.DesiredClassBattery, "batt-0", testCfg())
	// First doc, but its issuedAt is 400s old at receipt (> 300s bound) → reject.
	a, reps := r.SetDesired(battDoc(9, issuedAt(0), 1000), at(400*time.Second))
	mustNone(t, a)
	rej, ok := firstKind(reps, ReportRejectedDoc)
	if !ok || rej.Reject != RejectStale {
		t.Fatalf("want RejectedDoc/Stale, got %v", reps)
	}
	if r.desired != nil {
		t.Fatal("a stale doc must not become the standing desired")
	}
}

// -------------------------------------------------------------------------
// Background bullet: NaN defense (doc + observation)
// -------------------------------------------------------------------------

func TestNaNDocRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    float64
	}{{"nan", math.NaN()}, {"posinf", math.Inf(1)}, {"neginf", math.Inf(-1)}} {
		t.Run(tc.name, func(t *testing.T) {
			r := New(bus.DesiredClassBattery, "batt-0", testCfg())
			a, reps := r.SetDesired(battDoc(1, issuedAt(0), tc.v), at(0))
			mustNone(t, a)
			rej, ok := firstKind(reps, ReportRejectedDoc)
			if !ok || rej.Reject != RejectNaN {
				t.Fatalf("want RejectedDoc/NaN, got %v", reps)
			}
			if r.desired != nil {
				t.Fatal("NaN doc must never be stored")
			}
		})
	}
}

func TestNaNObservationRejected(t *testing.T) {
	r := New(bus.DesiredClassBattery, "batt-0", testCfg())
	r.SetDesired(battDoc(1, issuedAt(0), 1000), at(0))
	r.Observe(good(at(1*time.Second), map[Field]float64{SetpointW: 1000}), at(1*time.Second))
	if !r.converged {
		t.Fatal("precondition: converged")
	}
	// NaN readback → rejected, does not disturb the prior assessment.
	a, reps := r.Observe(good(at(2*time.Second), map[Field]float64{SetpointW: math.NaN()}), at(2*time.Second))
	mustNone(t, a)
	rej, ok := firstKind(reps, ReportRejectedObs)
	if !ok || rej.Reject != RejectNaN {
		t.Fatalf("want RejectedObs/NaN, got %v", reps)
	}
	if !r.converged {
		t.Fatal("NaN observation must not clobber the held assessment")
	}
}

// -------------------------------------------------------------------------
// Extra table cases: reconnect mid-episode, doc-update mid-episode,
// episode-counter monotonicity, disconnected-held, no-desired.
// -------------------------------------------------------------------------

func TestReconnectMidEpisode(t *testing.T) {
	r := New(bus.DesiredClassBattery, "batt-0", testCfg())
	r.SetDesired(battDoc(1, issuedAt(0), 1000), at(0))
	r.Observe(good(at(0), map[Field]float64{SetpointW: 0}), at(0)) // diverge
	_, reps := r.Tick(at(30 * time.Second))                        // open episode
	if countKind(reps, ReportNonConvergedBegin) != 1 {
		t.Fatal("precondition: episode open")
	}
	// Reconnect mid-episode: forces a write, must NOT close the episode.
	a, reps := r.Reconnected(at(31 * time.Second))
	mustWrite(t, a, "reconnect-reassert")
	if countKind(reps, ReportNonConvergedEnd) != 0 {
		t.Fatal("reconnect must not end the episode")
	}
	if !r.inEpisode {
		t.Fatal("episode should stay open across a reconnect")
	}
	// Convergence still ends it exactly once.
	_, reps = r.Observe(good(at(40*time.Second), map[Field]float64{SetpointW: 1000}), at(40*time.Second))
	if countKind(reps, ReportNonConvergedEnd) != 1 {
		t.Fatalf("want 1 End on convergence, got %v", reps)
	}
}

func TestDocUpdateMidEpisodeResetsWindow(t *testing.T) {
	r := New(bus.DesiredClassBattery, "batt-0", testCfg())
	r.SetDesired(battDoc(1, issuedAt(0), 1000), at(0))
	r.Observe(good(at(0), map[Field]float64{SetpointW: 0}), at(0))
	_, reps := r.Tick(at(30 * time.Second)) // episode #1 open
	if countKind(reps, ReportNonConvergedBegin) != 1 {
		t.Fatal("precondition: episode #1 open")
	}
	// New target mid-episode → closes episode #1 (End) and re-writes.
	a, reps := r.SetDesired(battDoc(2, issuedAt(31*time.Second), 2000), at(31*time.Second))
	mustWrite(t, a, "new-desired")
	wantField(t, a, SetpointW, 2000)
	if countKind(reps, ReportNonConvergedEnd) != 1 {
		t.Fatalf("target change must close the open episode, got %v", reps)
	}
	if r.inEpisode || !r.divergentSince.IsZero() {
		t.Fatal("convergence window must reset on a new target")
	}
	// A fresh divergence against the NEW target opens episode #2.
	r.Observe(good(at(31*time.Second), map[Field]float64{SetpointW: 0}), at(31*time.Second))
	_, reps = r.Tick(at(61 * time.Second)) // 30s after new divergence
	begin, ok := firstKind(reps, ReportNonConvergedBegin)
	if !ok || begin.Episode != 2 {
		t.Fatalf("want episode #2 begin, got %v", reps)
	}
	if begin.Seq != 2 {
		t.Fatalf("episode #2 should attribute to seq 2, got %d", begin.Seq)
	}
}

func TestEpisodeCounterMonotonic(t *testing.T) {
	r := New(bus.DesiredClassBattery, "batt-0", testCfg())
	r.SetDesired(battDoc(1, issuedAt(0), 1000), at(0))

	drive := func(startSec int) uint64 {
		s := time.Duration(startSec) * time.Second
		r.Observe(good(at(s), map[Field]float64{SetpointW: 0}), at(s)) // diverge
		_, reps := r.Tick(at(s + 30*time.Second))                      // begin
		begin, ok := firstKind(reps, ReportNonConvergedBegin)
		if !ok {
			t.Fatalf("expected begin at start %ds", startSec)
		}
		// Re-converge to close the episode.
		r.Observe(good(at(s+40*time.Second), map[Field]float64{SetpointW: 1000}), at(s+40*time.Second))
		return begin.Episode
	}
	e1 := drive(0)
	e2 := drive(100)
	e3 := drive(200)
	if e1 != 1 || e2 != 2 || e3 != 3 {
		t.Fatalf("episode counter must be monotonic 1,2,3; got %d,%d,%d", e1, e2, e3)
	}
}

func TestDisconnectedObservationHeld(t *testing.T) {
	r := New(bus.DesiredClassBattery, "batt-0", testCfg())
	r.SetDesired(battDoc(1, issuedAt(0), 1000), at(0))
	r.Observe(good(at(1*time.Second), map[Field]float64{SetpointW: 1000}), at(1*time.Second))
	// A disconnected sample carries no evidence.
	a, reps := r.Observe(obs(false, true, at(2*time.Second), map[Field]float64{SetpointW: 0}), at(2*time.Second))
	mustNone(t, a)
	if len(reps) != 0 || !r.converged {
		t.Fatalf("disconnected sample must be held; reps=%v converged=%v", reps, r.converged)
	}
}

func TestFieldIncompleteObservationHeld(t *testing.T) {
	// desired opines on SetpointW AND Connect; a read missing Connect can't be judged.
	r := New(bus.DesiredClassBattery, "batt-0", testCfg())
	doc := battDoc(1, issuedAt(0), 1000)
	doc.Connect = bptr(true)
	r.SetDesired(doc, at(0))
	a, reps := r.Observe(good(at(1*time.Second), map[Field]float64{SetpointW: 1000}), at(1*time.Second))
	mustNone(t, a) // Connect absent → cannot assess → held
	if len(reps) != 0 || r.converged {
		t.Fatal("incomplete readback must hold, not converge")
	}
	// Full readback (Connect=1) converges.
	a, _ = r.Observe(good(at(2*time.Second), map[Field]float64{SetpointW: 1000, Connect: 1}), at(2*time.Second))
	mustNone(t, a)
	if !r.converged {
		t.Fatal("complete matching readback should converge")
	}
}

func TestNoDesiredNoAction(t *testing.T) {
	r := New(bus.DesiredClassBattery, "batt-0", testCfg())
	a, reps := r.Observe(good(at(0), map[Field]float64{SetpointW: 1000}), at(0))
	mustNone(t, a)
	if reps != nil {
		t.Fatalf("Observe before any desired → None/no reports, got %v", reps)
	}
	a, reps = r.Tick(at(1 * time.Second))
	mustNone(t, a)
	if reps != nil {
		t.Fatalf("Tick before any desired → None/no reports, got %v", reps)
	}
}

// -------------------------------------------------------------------------
// Connect field exactness + slow reassert watchdog + defaults.
// -------------------------------------------------------------------------

func TestConnectFieldExactTolerance(t *testing.T) {
	r := New(bus.DesiredClassBattery, "batt-0", testCfg())
	doc := bus.DesiredState{
		Envelope: bus.Envelope{V: bus.DesiredStateV}, DeviceClass: bus.DesiredClassBattery,
		DeviceID: "batt-0", Connect: bptr(true), Source: "safety", MRID: "m", IssuedAt: issuedAt(0), Seq: 1,
	}
	a, _ := r.SetDesired(doc, at(0))
	mustWrite(t, a, "new-desired")
	wantField(t, a, Connect, 1)
	// Connect=0 differs from desired 1 by 1 > 0.5 default → diverged.
	a, _ = r.Observe(good(at(1*time.Second), map[Field]float64{Connect: 0}), at(1*time.Second))
	mustWrite(t, a, "write-on-diff")
	// Connect=1 matches.
	a, _ = r.Observe(good(at(2*time.Second), map[Field]float64{Connect: 1}), at(2*time.Second))
	mustNone(t, a)
}

func TestSlowReassertWatchdog(t *testing.T) {
	r := New(bus.DesiredClassBattery, "batt-0", testCfg()) // ReassertEvery 60s
	r.SetDesired(battDoc(1, issuedAt(0), 1000), at(0))     // writes, lastReassert=t0
	r.Observe(good(at(1*time.Second), map[Field]float64{SetpointW: 1000}), at(1*time.Second))

	// Before 60s since the last write → no reassert.
	a, _ := r.Tick(at(59 * time.Second))
	mustNone(t, a)
	// At 60s → reassert.
	a, _ = r.Tick(at(60 * time.Second))
	mustWrite(t, a, "reassert")
	wantField(t, a, SetpointW, 1000)
	// Timer restarts from that write.
	a, _ = r.Tick(at(119 * time.Second))
	mustNone(t, a)
	a, _ = r.Tick(at(120 * time.Second))
	mustWrite(t, a, "reassert")
}

func TestReassertDisabledWhenZero(t *testing.T) {
	cfg := testCfg()
	cfg.ReassertEvery = 0
	r := New(bus.DesiredClassBattery, "batt-0", cfg)
	r.SetDesired(battDoc(1, issuedAt(0), 1000), at(0))
	r.Observe(good(at(1*time.Second), map[Field]float64{SetpointW: 1000}), at(1*time.Second))
	for s := 60; s <= 600; s += 60 {
		a, _ := r.Tick(at(time.Duration(s) * time.Second))
		if a.Kind == ActionWrite {
			t.Fatalf("reassert must be disabled at ReassertEvery=0 (fired at %ds)", s)
		}
	}
}

func TestConfigDefaults(t *testing.T) {
	r := New(bus.DesiredClassBattery, "batt-0", Config{})
	if r.cfg.ConvergeTimeout != DefaultConvergeTimeout {
		t.Fatalf("ConvergeTimeout default = %v", r.cfg.ConvergeTimeout)
	}
	if r.cfg.StaleAfter != DefaultStaleAfter {
		t.Fatalf("StaleAfter default = %v", r.cfg.StaleAfter)
	}
	if len(r.cfg.RetryBackoff) != len(DefaultRetryBackoff) {
		t.Fatalf("RetryBackoff default len = %d", len(r.cfg.RetryBackoff))
	}
	if r.cfg.ReassertEvery != 0 {
		t.Fatalf("ReassertEvery zero must stay disabled, got %v", r.cfg.ReassertEvery)
	}

	// Behavioral: default 60s ConvergeTimeout — no begin at 59s, begin at 60s.
	r.SetDesired(battDoc(1, issuedAt(0), 1000), at(0))
	r.Observe(good(at(0), map[Field]float64{SetpointW: 0}), at(0))
	_, reps := r.Tick(at(59 * time.Second))
	if countKind(reps, ReportNonConvergedBegin) != 0 {
		t.Fatal("default ConvergeTimeout should be 60s, begin fired too early")
	}
	_, reps = r.Tick(at(60 * time.Second))
	if countKind(reps, ReportNonConvergedBegin) != 1 {
		t.Fatal("default ConvergeTimeout begin should fire at 60s")
	}
}

func TestNegativeReassertDisabled(t *testing.T) {
	cfg := testCfg()
	cfg.ReassertEvery = -5 * time.Second
	r := New(bus.DesiredClassBattery, "batt-0", cfg)
	if r.cfg.ReassertEvery != 0 {
		t.Fatalf("negative ReassertEvery must normalize to 0 (disabled), got %v", r.cfg.ReassertEvery)
	}
}

func TestConnectFalseField(t *testing.T) {
	// Exercises fieldsOf's Connect==false branch.
	r := New(bus.DesiredClassBattery, "batt-0", testCfg())
	doc := bus.DesiredState{
		Envelope: bus.Envelope{V: bus.DesiredStateV}, DeviceClass: bus.DesiredClassBattery,
		DeviceID: "batt-0", Connect: bptr(false), Source: "safety", MRID: "m", IssuedAt: issuedAt(0), Seq: 1,
	}
	a, _ := r.SetDesired(doc, at(0))
	mustWrite(t, a, "new-desired")
	wantField(t, a, Connect, 0)
}

func TestStringers(t *testing.T) {
	// Field
	for f, want := range map[Field]string{SetpointW: "SetpointW", CeilingW: "CeilingW", Connect: "Connect", MaxCurrentA: "MaxCurrentA", Field(99): "Field(?)"} {
		if got := f.String(); got != want {
			t.Errorf("Field(%d).String()=%q want %q", f, got, want)
		}
	}
	for k, want := range map[ActionKind]string{ActionNone: "None", ActionWrite: "Write", ActionKind(9): "ActionKind(?)"} {
		if got := k.String(); got != want {
			t.Errorf("ActionKind.String()=%q want %q", got, want)
		}
	}
	for k, want := range map[ReportKind]string{
		ReportNonConvergedBegin: "NonConvergedBegin", ReportNonConvergedEnd: "NonConvergedEnd",
		ReportStaleDesired: "StaleDesired", ReportRejectedDoc: "RejectedDoc",
		ReportRejectedObs: "RejectedObs", ReportSeqReset: "SeqReset", ReportKind(99): "ReportKind(?)",
	} {
		if got := k.String(); got != want {
			t.Errorf("ReportKind.String()=%q want %q", got, want)
		}
	}
	for k, want := range map[RejectReason]string{
		RejectNone: "None", RejectSeqRegression: "SeqRegression", RejectStale: "Stale",
		RejectNaN: "NaN", RejectReason(9): "RejectReason(?)",
	} {
		if got := k.String(); got != want {
			t.Errorf("RejectReason.String()=%q want %q", got, want)
		}
	}
}

// -------------------------------------------------------------------------
// fieldsOf / docHasNaN coverage for solar (CeilingW) and EVSE (MaxCurrentA).
// -------------------------------------------------------------------------

func TestSolarAndEVSEFields(t *testing.T) {
	// Solar CeilingW opinion.
	rs := New(bus.DesiredClassSolar, "inv-0", testCfg())
	solar := bus.DesiredState{
		Envelope: bus.Envelope{V: bus.DesiredStateV}, DeviceClass: bus.DesiredClassSolar,
		DeviceID: "inv-0", CeilingW: fptr(bus.RestoreCeilingW), Source: "csip-event", IssuedAt: issuedAt(0), Seq: 1,
	}
	a, _ := rs.SetDesired(solar, at(0))
	mustWrite(t, a, "new-desired")
	wantField(t, a, CeilingW, bus.RestoreCeilingW)

	// EVSE MaxCurrentA opinion + NaN defense on that field.
	re := New(bus.DesiredClassEVSE, "STAT-1", testCfg())
	evse := bus.DesiredState{
		Envelope: bus.Envelope{V: bus.DesiredStateV}, DeviceClass: bus.DesiredClassEVSE,
		DeviceID: "STAT-1", MaxCurrentA: fptr(16), ConnectorID: 1, Source: "economic", IssuedAt: issuedAt(0), Seq: 1,
	}
	a, _ = re.SetDesired(evse, at(0))
	mustWrite(t, a, "new-desired")
	wantField(t, a, MaxCurrentA, 16)

	bad := evse
	bad.MaxCurrentA = fptr(math.Inf(1))
	bad.Seq = 2
	_, reps := re.SetDesired(bad, at(1*time.Second))
	if rej, ok := firstKind(reps, ReportRejectedDoc); !ok || rej.Reject != RejectNaN {
		t.Fatalf("EVSE Inf current must be rejected, got %v", reps)
	}
}
