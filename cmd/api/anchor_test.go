package main

import (
	"testing"
	"time"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/utilitytime"
)

// TestStateStore_Snapshot_FallsBackBeforeAnyControl verifies snapshot() only
// overrides clockOffsetS with the anchored derivation once utclk has
// anchored at least once (TASK-037) — before the first control/schedule
// message ever arrives, the legacy raw (zero-value) clockOffsetS is left
// untouched, matching pre-TASK-037 behavior.
func TestStateStore_Snapshot_FallsBackBeforeAnyControl(t *testing.T) {
	s := newStateStore(nil, 60*time.Second)
	snap := s.snapshot()
	if snap.clockOffsetS != 0 {
		t.Fatalf("clockOffsetS before any control ever arrived = %d, want 0", snap.clockOffsetS)
	}
}

// TestStateStore_OnCSIPControl_AnchorsUtilityTime proves the derived offset
// snapshot() computes after a control arrives reconstructs the anchored
// server-now (base.Unix()+ClockOffset) exactly, given the negligible real
// elapsed time between onCSIPControl and snapshot() in a unit test.
func TestStateStore_OnCSIPControl_AnchorsUtilityTime(t *testing.T) {
	s := newStateStore(nil, 60*time.Second)
	base := time.Now()
	const offset = int64(42)
	s.onCSIPControl("lexa/csip/control", bus.ActiveControl{
		Source: "event", MRID: "M1", ClockOffset: offset, Ts: base.Unix(),
	})

	snap := s.snapshot()
	gotServerNow := snap.clockOffsetS + snap.now.Unix()
	truth := base.Unix() + offset
	if d := gotServerNow - truth; d < -1 || d > 1 {
		t.Errorf("derived serverNow = %d, want within 1s of truth %d (diff %ds)", gotServerNow, truth, d)
	}
}

// TestStateStore_Snapshot_DerivedOffsetImmuneToLocalStep mirrors
// cmd/hub's TestReadSystemState_LocalStepDoesNotDropControl: lexa-api's
// /status grace evaluation (buildStatus's utilitytime.ReportGrace check,
// fed snap.clockOffsetS) must stay correct across a LOCAL wall-clock step
// between control arrivals, because s.utclk anchors utility time to the
// monotonic instant of the last onCSIPControl arrival rather than
// re-deriving from a live wall-clock read on every snapshot.
//
// As in utilitytime's own tests, a genuine wall-vs-monotonic desync can't be
// constructed through the public time.Time API, so this proves the anchored
// derivation tracks TRUE elapsed time (within 1s of ground truth) and
// contrasts it against what the pre-TASK-037 raw formula would have given
// under a simulated ±1h step.
func TestStateStore_Snapshot_DerivedOffsetImmuneToLocalStep(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stepDur time.Duration
	}{
		{"forward +1h local step", time.Hour},
		{"backward -1h local step", -time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStateStore(nil, 60*time.Second)
			base := time.Now() // must be time.Now()-derived to carry a monotonic reading
			fakeNow := base
			s.utclk = utilitytime.New(utilitytime.Config{Now: func() time.Time { return fakeNow }})

			const offset = int64(10)
			s.onCSIPControl("lexa/csip/control", bus.ActiveControl{
				Source: "event", MRID: "M1", ClockOffset: offset, Ts: base.Unix(),
			})

			trueElapsed := 5 * time.Second
			fakeNow = base.Add(trueElapsed)

			snap := s.snapshot()
			gotServerNow := snap.clockOffsetS + snap.now.Unix()
			truth := base.Unix() + offset + int64(trueElapsed.Seconds())
			if d := gotServerNow - truth; d < -1 || d > 1 {
				t.Errorf("derived serverNow = %d, want within 1s of truth %d (diff %ds)", gotServerNow, truth, d)
			}

			steppedNow := base.Add(trueElapsed + tc.stepDur)
			legacyServerNow := utilitytime.ServerNowAt(steppedNow, offset)
			if diff := legacyServerNow - gotServerNow; diff != int64(tc.stepDur.Seconds()) {
				t.Errorf("legacy-vs-anchored divergence = %ds, want %ds (the simulated step size)", diff, int64(tc.stepDur.Seconds()))
			}
		})
	}
}
