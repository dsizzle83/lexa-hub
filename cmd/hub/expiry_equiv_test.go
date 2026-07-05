package main

// Differential equivalence test for the TASK-036 migration of cmd/hub's CSIP
// expiry debounce onto internal/utilitytime (AD-004). cmd/hub is
// radioactive-adjacent (05 §12: control-plane expiry semantics) — this pins
// that swapping the bare csipExpiredTicks counter for
// utilitytime.DebouncedExpiry did not move a single drop/hold verdict, the
// same style as internal/northbound/scheduler/utilitytime_equiv_test.go's
// TASK-035 differential test.

import (
	"testing"

	"lexa-hub/internal/utilitytime"
)

// legacyExpiryStep reproduces the pre-TASK-036 inline logic removed from
// ReadSystemState (state.go, formerly: "if validUntil>0 && serverNow>=validUntil
// { ticks++; if ticks>=confirm { ticks=0; drop } } else { ticks=0 }") bit for
// bit, for differential comparison against utilitytime.DebouncedExpiry.Observe.
func legacyExpiryStep(ticks *int, confirm int, expired bool) (drop bool) {
	if !expired {
		*ticks = 0
		return false
	}
	*ticks++
	if *ticks >= confirm {
		*ticks = 0
		return true
	}
	return false
}

// TestExpiryEquivalence_LegacyVsUtilitytime drives the legacy counter and a
// utilitytime.DebouncedExpiry through the identical (hasControl, validUntil,
// serverNow) sequence at FAST's confirm=3 and asserts every drop/hold verdict
// matches. The sequence covers: normal hold, a 2-tick transient excursion that
// rides out (settles back before confirming), a genuine 3-tick sustained
// expiry that drops on the 3rd tick, and the post-drop state (no control —
// mirrors ReadSystemState's r.lastCSIP==nil short-circuit, which is why
// "expired" is forced false regardless of validUntil once dropped).
func TestExpiryEquivalence_LegacyVsUtilitytime(t *testing.T) {
	const confirm = 3 // FAST cadence: confirmTicksFor(3*time.Second) == 3
	const validUntil = int64(1_000_000)

	steps := []struct {
		name       string
		hasControl bool
		serverNow  int64
	}{
		{"well before ValidUntil", true, validUntil - 100},
		{"transient excursion tick 1", true, validUntil + 1},
		{"transient excursion tick 2", true, validUntil + 2},
		{"settles back — rides out, counter resets", true, validUntil - 1},
		{"sustained tick 1 of 3", true, validUntil + 1},
		{"sustained tick 2 of 3", true, validUntil + 2},
		{"sustained tick 3 of 3 — drops", true, validUntil + 3},
		{"post-drop: no control", false, validUntil + 4},
		{"post-drop: still no control", false, validUntil + 5},
	}

	legacyTicks := 0
	d := utilitytime.DebouncedExpiry{Confirm: confirm}

	for _, s := range steps {
		legacyExpired := s.hasControl && validUntil > 0 && s.serverNow >= validUntil
		utilExpired := s.hasControl && utilitytime.Expired(validUntil, s.serverNow)
		if legacyExpired != utilExpired {
			t.Fatalf("%s: expired-source mismatch: legacy=%v utilitytime=%v", s.name, legacyExpired, utilExpired)
		}

		wantDrop := legacyExpiryStep(&legacyTicks, confirm, legacyExpired)
		gotDrop := d.Observe(utilExpired)
		if gotDrop != wantDrop {
			t.Fatalf("%s: Observe(%v) = %v, legacy = %v", s.name, utilExpired, gotDrop, wantDrop)
		}
	}
}
