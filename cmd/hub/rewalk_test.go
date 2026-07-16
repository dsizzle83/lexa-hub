package main

import (
	"testing"
	"time"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/utilitytime"
)

// TestOnCSIPControl_StaleCheckUsesCoherentServerTs is the CS-4 regression: the
// stale-at-adoption age is measured from the previous anchor's MONOTONIC clock
// against the message's coherent ServerTs — not a raw local-now−Ts wall diff
// that a forward wall step (or a rewalk timestamp refresh) between messages
// would inflate. A second message that is genuinely FRESH (coherent ServerTs =
// current server time) must not be flagged stale even if its raw Ts field looks
// old.
func TestOnCSIPControl_StaleCheckUsesCoherentServerTs(t *testing.T) {
	r := newMQTTSystemReader(nil, testFastInterval, nil)
	r.SetRetainedAdoptionMaxAge(60 * time.Second)
	var rewalkCalls int
	r.SetRewalkHandler(func(string) { rewalkCalls++ })
	base := time.Now() // time.Now()-derived so it carries a monotonic reading
	fakeNow := base
	r.utclk = utilitytime.New(utilitytime.Config{Now: func() time.Time { return fakeNow }})

	expLim := 5000.0
	// Message 1 anchors utility time at server-time = base.
	r.onCSIPControl("lexa/csip/control", bus.ActiveControl{
		Source: "event", MRID: "m1", ExpLimW: &expLim,
		ServerTs: base.Unix(), Ts: base.Unix(), ClockOffset: 0,
	})
	rewalkCalls = 0 // ignore anything from the first message

	// 5 s of TRUE elapsed time (monotonic advances).
	fakeNow = base.Add(5 * time.Second)

	// Message 2 is genuinely fresh (coherent ServerTs = current server time) but
	// its raw Ts field looks 1000 s old. The check must trust ServerTs.
	r.onCSIPControl("lexa/csip/control", bus.ActiveControl{
		Source: "event", MRID: "m2", ExpLimW: &expLim,
		ServerTs: base.Add(5 * time.Second).Unix(),
		Ts:       base.Unix() - 1000, ClockOffset: 0,
	})
	if r.lastCSIPStaleSuspect {
		t.Error("stale-suspect for a message with a FRESH coherent ServerTs — the age check must key off ServerTs, not the raw Ts (audit CS-4)")
	}
	if rewalkCalls != 0 {
		t.Errorf("rewalk fired %d times for a fresh coherent ServerTs, want 0", rewalkCalls)
	}
}

// TestOnCSIPControl_FreshControlNoAlarmNoRewalk is TASK-042's fresh-adoption
// control case: a retained control whose Ts is effectively "now" must not be
// flagged stale, must not fire the rewalk hook, and must still enforce
// normally.
func TestOnCSIPControl_FreshControlNoAlarmNoRewalk(t *testing.T) {
	r := newMQTTSystemReader(nil, testFastInterval, nil)
	r.SetRetainedAdoptionMaxAge(300 * time.Second)
	var rewalkCalls int
	r.SetRewalkHandler(func(reason string) { rewalkCalls++ })

	expLim := 5000.0
	r.onCSIPControl("lexa/csip/control", bus.ActiveControl{
		Source:  "event",
		MRID:    "fresh-evt",
		ExpLimW: &expLim,
		Ts:      time.Now().Unix(),
	})

	if r.lastCSIPStaleSuspect {
		t.Error("lastCSIPStaleSuspect = true for a fresh (age 0) control, want false")
	}
	if rewalkCalls != 0 {
		t.Errorf("rewalk handler called %d times for a fresh control, want 0", rewalkCalls)
	}
	state, err := r.ReadSystemState()
	if err != nil {
		t.Fatal(err)
	}
	if state.CSIPControl == nil {
		t.Fatal("fresh control not enforced")
	}
}

// TestOnCSIPControl_StaleAdoptionAlarmsAndRewalksOnceStillEnforced is
// TASK-042's core acceptance case: a retained control whose Ts is older than
// the configured bound must (a) still be enforced (enforce-but-verify, never
// fail-open), (b) flag lastCSIPStaleSuspect, and (c) fire the rewalk handler
// exactly once for this one message arrival.
func TestOnCSIPControl_StaleAdoptionAlarmsAndRewalksOnceStillEnforced(t *testing.T) {
	r := newMQTTSystemReader(nil, testFastInterval, nil)
	r.SetRetainedAdoptionMaxAge(60 * time.Second)
	var rewalkReasons []string
	r.SetRewalkHandler(func(reason string) { rewalkReasons = append(rewalkReasons, reason) })

	expLim := 5000.0
	r.onCSIPControl("lexa/csip/control", bus.ActiveControl{
		Source:  "event",
		MRID:    "stale-evt",
		ExpLimW: &expLim,
		Ts:      time.Now().Unix() - 300, // 300s old, bound is 60s
	})

	if !r.lastCSIPStaleSuspect {
		t.Error("lastCSIPStaleSuspect = false for a 300s-old control against a 60s bound, want true")
	}
	if len(rewalkReasons) != 1 || rewalkReasons[0] != "stale" {
		t.Fatalf("rewalk reasons = %v, want exactly [\"stale\"]", rewalkReasons)
	}

	// Still enforced: enforce-but-verify, never fail-open.
	state, err := r.ReadSystemState()
	if err != nil {
		t.Fatal(err)
	}
	if state.CSIPControl == nil {
		t.Fatal("stale-suspect control was NOT enforced; a stale-but-decodable cap must never be dropped")
	}
	if state.CSIPControl.MRID != "stale-evt" {
		t.Errorf("enforced control MRID = %q, want %q", state.CSIPControl.MRID, "stale-evt")
	}
}

// TestOnCSIPControl_StaleAdoptionRateLimited verifies the rewalk publish is
// rate-limited: two stale adoptions arriving within rewalkRateLimit of each
// other must only fire the handler once.
func TestOnCSIPControl_StaleAdoptionRateLimited(t *testing.T) {
	r := newMQTTSystemReader(nil, testFastInterval, nil)
	r.SetRetainedAdoptionMaxAge(60 * time.Second)
	var rewalkCalls int
	r.SetRewalkHandler(func(reason string) { rewalkCalls++ })

	expLim := 5000.0
	stale := bus.ActiveControl{
		Source:  "event",
		MRID:    "stale-evt",
		ExpLimW: &expLim,
		Ts:      time.Now().Unix() - 300,
	}
	r.onCSIPControl("lexa/csip/control", stale)
	r.onCSIPControl("lexa/csip/control", stale) // arrives well within rewalkRateLimit

	if rewalkCalls != 1 {
		t.Fatalf("rewalk handler called %d times for two stale adoptions inside the rate limit, want 1", rewalkCalls)
	}
}

// TestOnCSIPControl_SourceNoneExcludedFromStaleCheck pins the "must not
// change" carve-out: an old Ts on a Source=="none" control (no active
// intent) must never be flagged stale or trigger a rewalk — a "none"
// sentinel carries no compliance risk (mqtt-stale-retained's existing
// transient-drop-then-recover behavior must not get "improved" into a
// special case here).
func TestOnCSIPControl_SourceNoneExcludedFromStaleCheck(t *testing.T) {
	r := newMQTTSystemReader(nil, testFastInterval, nil)
	r.SetRetainedAdoptionMaxAge(60 * time.Second)
	var rewalkCalls int
	r.SetRewalkHandler(func(reason string) { rewalkCalls++ })

	r.onCSIPControl("lexa/csip/control", bus.ActiveControl{
		Source: "none",
		Ts:     time.Now().Unix() - 300,
	})

	if r.lastCSIPStaleSuspect {
		t.Error("lastCSIPStaleSuspect = true for a Source==\"none\" control, want false")
	}
	if rewalkCalls != 0 {
		t.Errorf("rewalk handler called %d times for a Source==\"none\" control, want 0", rewalkCalls)
	}
}

// TestOnCSIPControl_DisabledWhenBoundNotSet verifies the additive-setter
// default: a reader that never calls SetRetainedAdoptionMaxAge (every
// pre-TASK-042 test and call site) never flags staleness or fires a rewalk,
// regardless of how old Ts is — the check is opt-in, not a hidden default.
func TestOnCSIPControl_DisabledWhenBoundNotSet(t *testing.T) {
	r := newMQTTSystemReader(nil, testFastInterval, nil)
	var rewalkCalls int
	r.SetRewalkHandler(func(reason string) { rewalkCalls++ })

	expLim := 5000.0
	r.onCSIPControl("lexa/csip/control", bus.ActiveControl{
		Source:  "event",
		MRID:    "ancient-evt",
		ExpLimW: &expLim,
		Ts:      time.Now().Unix() - 1_000_000,
	})

	if r.lastCSIPStaleSuspect {
		t.Error("lastCSIPStaleSuspect = true with no bound configured, want false (check disabled)")
	}
	if rewalkCalls != 0 {
		t.Errorf("rewalk handler called %d times with no bound configured, want 0", rewalkCalls)
	}
}

// TestRequestRewalk_DecodeReasonSharesRateLimitWithStaleAdoption verifies the
// decode-error path (RequestRewalk("decode"), called from main.go's
// SubscribeDecodeErr onErr hook) shares its rate limiter with the
// stale-adoption path in onCSIPControl — a corrupted retained payload
// redelivered on every reconnect, interleaved with stale-control arrivals,
// must not double the effective rewalk rate.
func TestRequestRewalk_DecodeReasonSharesRateLimitWithStaleAdoption(t *testing.T) {
	r := newMQTTSystemReader(nil, testFastInterval, nil)
	r.SetRetainedAdoptionMaxAge(60 * time.Second)
	var rewalkReasons []string
	r.SetRewalkHandler(func(reason string) { rewalkReasons = append(rewalkReasons, reason) })

	expLim := 5000.0
	r.onCSIPControl("lexa/csip/control", bus.ActiveControl{
		Source:  "event",
		MRID:    "stale-evt",
		ExpLimW: &expLim,
		Ts:      time.Now().Unix() - 300,
	})
	r.RequestRewalk("decode") // arrives immediately after, well inside the rate limit

	if len(rewalkReasons) != 1 {
		t.Fatalf("rewalk handler called %d times across stale-adoption + immediate decode-error, want 1 (shared rate limit): %v",
			len(rewalkReasons), rewalkReasons)
	}
	if rewalkReasons[0] != "stale" {
		t.Errorf("the one rewalk call had reason %q, want %q (first-in wins the rate limit)", rewalkReasons[0], "stale")
	}
}

// TestRequestRewalk_NilHandlerIsNoop verifies RequestRewalk (and by
// extension onCSIPControl's internal requestRewalkLocked) never panics when
// no rewalk handler has been wired — the constructor's default state, and
// every pre-TASK-042 test.
func TestRequestRewalk_NilHandlerIsNoop(t *testing.T) {
	r := newMQTTSystemReader(nil, testFastInterval, nil)
	r.RequestRewalk("decode") // must not panic
}
