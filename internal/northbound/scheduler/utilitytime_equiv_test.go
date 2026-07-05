package scheduler

// Differential equivalence tests for the TASK-035 migration onto
// internal/utilitytime (AD-004). The scheduler's clock-regression and
// default-fallback guards are radioactive (05 §12): the migration swapped the
// serverNow SOURCE (utilitytime.Clock / ServerNowAt in place of
// scheduler.ServerNow) and the expiry/window PRIMITIVES (utilitytime.Expired /
// InWindow in place of the inline arithmetic) but must not move a single
// verdict. These tests pin that by computing serverNow both ways and asserting
// Evaluate resolves byte-identical *ActiveControl values across the exact
// fixtures the clock-jitter saga hardened against.

import (
	"reflect"
	"testing"
	"time"

	"lexa-hub/internal/utilitytime"
)

// TestServerNowEquivalence_LegacyVsUtilitytime proves the serverNow SOURCE
// swap is arithmetic-identical: for every (localNow, offset) pair — including
// the negative, stepped-back offsets that drive the regression guard — the
// legacy formula (local + offset, i.e. scheduler.ServerNow with a fixed local
// clock) equals both utilitytime.ServerNowAt (stateless) and a
// utilitytime.Clock fed the same offset (stateful).
func TestServerNowEquivalence_LegacyVsUtilitytime(t *testing.T) {
	locals := []int64{epoch, epoch + 3, epoch - 3, 0, 1700000000}
	offsets := []int64{0, 1, -1, 30, -30, 60, -60, 3600, -3600, 1 << 30, -(1 << 30)}

	for _, L := range locals {
		for _, O := range offsets {
			legacy := L + O // scheduler.ServerNow arithmetic, local clock pinned to L

			if got := utilitytime.ServerNowAt(time.Unix(L, 0), O); got != legacy {
				t.Errorf("ServerNowAt(L=%d, O=%d) = %d, legacy = %d", L, O, got, legacy)
			}

			l := L
			clk := utilitytime.New(utilitytime.Config{Now: func() time.Time { return time.Unix(l, 0) }})
			clk.SetOffset(O)
			if got := clk.ServerNow(); got != legacy {
				t.Errorf("Clock.ServerNow(L=%d, O=%d) = %d, legacy = %d", L, O, got, legacy)
			}
		}
	}
}

// TestEvaluate_EquivalentUnderBothServerNowSources drives two independent
// Scheduler instances through the identical program sequence — one fed
// serverNow via the legacy arithmetic, the other via a utilitytime.Clock — and
// asserts the resolved *ActiveControl is deeply equal at every step. The walk
// crosses the event boundary the way the V6 clock-jitter fixture does: fresh
// mid-window, a step BACK before the adopted event's start (the regression the
// default-fallback guard holds through), recovery, and finally past ValidUntil
// (a genuine end that must fall back to the default). Because the two schedulers
// stay in lockstep only if both the serverNow source and the migrated
// expiry/window primitives agree, any divergence fails here.
func TestEvaluate_EquivalentUnderBothServerNowSources(t *testing.T) {
	// Event: 0 W export cap active [epoch, epoch+600) in a program that also
	// carries a 5 kW default (progs) — the default-fallback-half fixture.
	evt := eventWithExpLim("E1", epoch, 600, 0, 0)
	programs := progs(evt)

	// Each step names a local clock and an offset; serverNow = local + offset.
	steps := []struct {
		name          string
		local, offset int64
	}{
		{"fresh mid-window", epoch, 30},                            // sn = epoch+30
		{"stepped back before start", epoch, -60},                  // sn = epoch-60 (regression)
		{"recovered", epoch, 35},                                   // sn = epoch+35
		{"still mid-window via large offset", epoch + 5000, -4600}, // sn = epoch+400
		{"past ValidUntil -> default", epoch, 700},                 // sn = epoch+700 (> epoch+600)
	}

	legacyS := New()
	utilS := New()

	for _, s := range steps {
		snLegacy := s.local + s.offset

		l := s.local
		clk := utilitytime.New(utilitytime.Config{Now: func() time.Time { return time.Unix(l, 0) }})
		clk.SetOffset(s.offset)
		snUtil := clk.ServerNow()

		if snLegacy != snUtil {
			t.Fatalf("%s: serverNow diverged: legacy=%d util=%d", s.name, snLegacy, snUtil)
		}

		gotLegacy := legacyS.Evaluate(programs, snLegacy)
		gotUtil := utilS.Evaluate(programs, snUtil)

		if !reflect.DeepEqual(gotLegacy, gotUtil) {
			t.Errorf("%s (sn=%d): Evaluate diverged:\n legacy = %+v\n util   = %+v",
				s.name, snLegacy, gotLegacy, gotUtil)
		}
	}
}
