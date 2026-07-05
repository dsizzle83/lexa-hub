package main

import (
	"math"
	"testing"
	"time"

	"lexa-hub/internal/bus"
)

// decodeAP mirrors the orchestrator's apW: value × 10^multiplier.
func decodeAP(value int16, multiplier int8) float64 {
	return float64(value) * math.Pow10(int(multiplier))
}

// testFastInterval is the bench's FAST engine cadence (bench-up.sh --fast /
// hub-replay-tune.sh fast). Existing expiry tests below construct their
// reader at this cadence so confirmTicksFor(testFastInterval) == 3, matching
// the pre-TASK-036 expiryConfirmTicks=3 constant bit-for-bit (AD-004).
const testFastInterval = 3 * time.Second

func TestWattsToActivePower(t *testing.T) {
	cases := []struct {
		name  string
		watts float64
		exact bool // expect lossless round-trip
	}{
		{"zero", 0, true},
		{"small", 5000, true},
		{"int16 max", 32767, true},
		{"just over int16", 32768, false},
		{"residential export limit", 50000, false},
		{"negative dispatch (absorb)", -50000, false},
		{"int16 min", -32768, true},
		{"odd value", 123456, false},
		{"feeder scale 20 MW", 20e6, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ap := wattsToActivePower(tc.watts)
			got := decodeAP(ap.Value, ap.Multiplier)

			// Error is bounded by half of one scale step.
			tol := 0.5 * math.Pow10(int(ap.Multiplier))
			if tc.exact {
				tol = 0
			}
			if math.Abs(got-tc.watts) > tol {
				t.Errorf("wattsToActivePower(%v) = {Value:%d Mult:%d} → %v, want within %v",
					tc.watts, ap.Value, ap.Multiplier, got, tol)
			}
		})
	}
}

// TestBusToCSIPControlLargeLimits is the C1 regression: limits ≥ 32.768 kW
// must survive the bus → CSIPControlState conversion intact, not wrap or
// saturate through a bare int16 cast.
func TestBusToCSIPControlLargeLimits(t *testing.T) {
	exp, imp, max, fixed := 50000.0, 40000.0, 100000.0, -75000.0
	cs := busToCSIPControl(&bus.ActiveControl{
		Source:  "event",
		MRID:    "test",
		ExpLimW: &exp,
		ImpLimW: &imp,
		MaxLimW: &max,
		FixedW:  &fixed,
	})
	if cs == nil {
		t.Fatal("busToCSIPControl returned nil for an event control")
	}

	check := func(name string, want float64, v int16, m int8) {
		t.Helper()
		got := decodeAP(v, m)
		if math.Abs(got-want) > 0.5*math.Pow10(int(m)) {
			t.Errorf("%s: got %v (Value:%d Mult:%d), want ≈%v", name, got, v, m, want)
		}
	}
	check("OpModExpLimW", exp, cs.Base.OpModExpLimW.Value, cs.Base.OpModExpLimW.Multiplier)
	check("OpModImpLimW", imp, cs.Base.OpModImpLimW.Value, cs.Base.OpModImpLimW.Multiplier)
	check("OpModMaxLimW", max, cs.Base.OpModMaxLimW.Value, cs.Base.OpModMaxLimW.Multiplier)
	check("OpModFixedW", fixed, cs.Base.OpModFixedW.Value, cs.Base.OpModFixedW.Multiplier)
}

// TestReadSystemState_ExpiredCSIPControlDropped is the C5 regression: the
// retained CSIP control must eventually stop being enforced past its ValidUntil
// when lexa-northbound stops refreshing it — but only after the expiry is
// confirmed sustained (r.expiry.Confirm consecutive ticks), so a transient
// clock jump can't drop a still-valid control (see
// TestReadSystemState_ClockLurchKeepsControl).
func TestReadSystemState_ExpiredCSIPControlDropped(t *testing.T) {
	r := newMQTTSystemReader(nil, testFastInterval)
	expLim := 5000.0
	r.onCSIPControl("lexa/csip/control", bus.ActiveControl{
		Source:     "event",
		MRID:       "expired-evt",
		ExpLimW:    &expLim,
		ValidUntil: time.Now().Unix() - 10, // already past in server time
	})

	// During the confirm window the cap is STILL enforced — a sustained expiry,
	// not a single excursion, is what drops it.
	for i := 0; i < r.expiry.Confirm-1; i++ {
		state, err := r.ReadSystemState()
		if err != nil {
			t.Fatal(err)
		}
		if state.CSIPControl == nil {
			t.Fatalf("control dropped on tick %d; it must persist through the confirm window", i+1)
		}
	}

	// The tick that confirms sustained expiry drops it.
	state, _ := r.ReadSystemState()
	if state.CSIPControl != nil {
		t.Errorf("expected sustained-expired control to be dropped after %d ticks, got mrid=%s",
			r.expiry.Confirm, state.CSIPControl.MRID)
	}
	// Stays gone.
	state, _ = r.ReadSystemState()
	if state.CSIPControl != nil {
		t.Error("expected CSIP control to stay nil after drop")
	}
}

// TestReadSystemState_ClockLurchKeepsControl is the clock-lurch regression: a
// non-monotonic server clock that repeatedly jumps past ValidUntil and back must
// NOT drop a control that is still valid once the clock settles. Before the fix,
// the first forward excursion deleted the control and the back-jump could not
// restore it, so the hub silently stopped enforcing the cap.
func TestReadSystemState_ClockLurchKeepsControl(t *testing.T) {
	r := newMQTTSystemReader(nil, testFastInterval)
	expLim := 5000.0
	r.onCSIPControl("lexa/csip/control", bus.ActiveControl{
		Source:     "event",
		MRID:       "lurch-evt",
		ExpLimW:    &expLim,
		ValidUntil: time.Now().Unix() + 300, // 5 min out in server time
	})

	// Alternate a +2h offset (server-now past ValidUntil) with a normal offset,
	// many times. The control must be enforced on every tick.
	for i := 0; i < 12; i++ {
		if i%2 == 0 {
			r.clockOffset = 7200 // lurch forward, past ValidUntil
		} else {
			r.clockOffset = 0 // settled, well inside the window
		}
		state, _ := r.ReadSystemState()
		if state.CSIPControl == nil {
			t.Fatalf("control dropped during clock lurch (tick %d) — the cap must stay enforced", i)
		}
	}
}

func TestReadSystemState_ActiveCSIPControlKept(t *testing.T) {
	cases := []struct {
		name       string
		validUntil int64
	}{
		{"future expiry", time.Now().Unix() + 3600},
		{"no expiry (0)", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newMQTTSystemReader(nil, testFastInterval)
			expLim := 5000.0
			r.onCSIPControl("lexa/csip/control", bus.ActiveControl{
				Source:     "event",
				MRID:       "live-evt",
				ExpLimW:    &expLim,
				ValidUntil: tc.validUntil,
			})
			state, err := r.ReadSystemState()
			if err != nil {
				t.Fatal(err)
			}
			if state.CSIPControl == nil {
				t.Fatal("expected live CSIP control to be kept")
			}
			if state.CSIPControl.MRID != "live-evt" {
				t.Errorf("MRID = %s, want live-evt", state.CSIPControl.MRID)
			}
		})
	}
}

// TestConfirmTicksFor_ScalesToEngineCadence pins the AD-004/TASK-036 wall-clock
// scaling table (code review checklist): FAST's 3 s tick keeps the legacy 3-tick
// debounce (9 s) exactly; STOCK's 15 s tick floors to 2 ticks (30 s, a
// deliberate improvement over the legacy tick-counted 45 s — see
// expiryConfirmWindowS's doc and AD-004); a 1 s tick scales up to 9 ticks
// (still exactly 9 s). The <=0 defensive fallback lands on the STOCK value.
func TestConfirmTicksFor_ScalesToEngineCadence(t *testing.T) {
	cases := []struct {
		name     string
		interval time.Duration
		want     int
	}{
		{"FAST 3s -> 3 ticks (9s, bit-identical to legacy)", 3 * time.Second, 3},
		{"STOCK 15s -> 2 ticks (30s, deliberate 45s->30s change)", 15 * time.Second, 2},
		{"1s -> 9 ticks (still exactly 9s)", 1 * time.Second, 9},
		{"defensive <=0 falls back to STOCK's 15s -> 2 ticks", 0, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := confirmTicksFor(tc.interval); got != tc.want {
				t.Errorf("confirmTicksFor(%s) = %d, want %d", tc.interval, got, tc.want)
			}
		})
	}
}

// TestReadSystemState_ExpiryDebounce_STOCKCadence is the STOCK-side sibling of
// TestReadSystemState_ExpiredCSIPControlDropped: at the 15 s engine interval the
// debounce now confirms in 2 ticks (30 s) rather than the legacy tick-counted 3
// ticks (45 s) — the AD-004/TASK-036 deliberate wall-clock correction. It also
// exercises the same transient-excursion-rides-out shape as
// TestReadSystemState_ClockLurchKeepsControl, but at STOCK cadence: one expired
// tick, then a settle-back tick, must NOT drop the control (the excursion resets
// the counter), and only a genuinely sustained 2-in-a-row expiry drops it.
func TestReadSystemState_ExpiryDebounce_STOCKCadence(t *testing.T) {
	const stockInterval = 15 * time.Second
	r := newMQTTSystemReader(nil, stockInterval)
	if r.expiry.Confirm != 2 {
		t.Fatalf("precondition: STOCK confirm = %d, want 2", r.expiry.Confirm)
	}
	expLim := 5000.0
	validUntil := time.Now().Unix() - 10 // already past in server time

	r.onCSIPControl("lexa/csip/control", bus.ActiveControl{
		Source:     "event",
		MRID:       "stock-expired",
		ExpLimW:    &expLim,
		ValidUntil: validUntil,
	})

	// Tick 1 of 2: expired, but not yet confirmed — control held.
	state, err := r.ReadSystemState()
	if err != nil {
		t.Fatal(err)
	}
	if state.CSIPControl == nil {
		t.Fatal("tick 1 of 2: transient excursion must not drop the control")
	}

	// Settle back inside the window: resets the debounce entirely (mirrors
	// TestReadSystemState_ClockLurchKeepsControl's shape at STOCK cadence).
	r.lastCSIP.ValidUntil = time.Now().Unix() + 300
	state, err = r.ReadSystemState()
	if err != nil {
		t.Fatal(err)
	}
	if state.CSIPControl == nil {
		t.Fatal("settle-back tick must not drop the control (excursion rides out)")
	}

	// Push back past ValidUntil and confirm across exactly 2 consecutive ticks.
	r.lastCSIP.ValidUntil = time.Now().Unix() - 10
	state, err = r.ReadSystemState()
	if err != nil {
		t.Fatal(err)
	}
	if state.CSIPControl == nil {
		t.Fatal("sustained tick 1 of 2 (post-reset): must not drop yet")
	}
	state, err = r.ReadSystemState()
	if err != nil {
		t.Fatal(err)
	}
	if state.CSIPControl != nil {
		t.Errorf("sustained tick 2 of 2: expected drop at STOCK's 2-tick confirm, got mrid=%s", state.CSIPControl.MRID)
	}
}

// TestNoteStaleness_EdgeTriggers verifies the staleness tracker flips state only
// on transitions: never-received is not stale, an old snapshot goes stale, and a
// fresh one recovers. (The log lines are the operator-visible surfacing; here we
// assert the underlying state the logging is gated on.)
func TestNoteStaleness_EdgeTriggers(t *testing.T) {
	r := newMQTTSystemReader([]DeviceConfig{{Name: "meter-0", Role: "meter"}}, testFastInterval)
	now := time.Now()

	r.noteStaleness("meter-0", measSnapshot{at: time.Time{}}, now)
	if r.stale["meter-0"] {
		t.Fatal("a never-received source must not be marked stale (startup, not a transition)")
	}
	r.noteStaleness("meter-0", measSnapshot{at: now.Add(-time.Second)}, now)
	if r.stale["meter-0"] {
		t.Fatal("a fresh source must not be stale")
	}
	r.noteStaleness("meter-0", measSnapshot{at: now.Add(-measStaleAfter - time.Second)}, now)
	if !r.stale["meter-0"] {
		t.Fatal("a source older than measStaleAfter must be marked stale")
	}
	r.noteStaleness("meter-0", measSnapshot{at: now}, now)
	if r.stale["meter-0"] {
		t.Fatal("a recovered source must clear stale")
	}
}
