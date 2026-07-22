package sunspec

import (
	"fmt"
	"math"
)

// TASK-053: generative int16 x scale-factor boundary sweep.
//
// These are exported (not _test.go) so both this module's own test
// (scale_sweep_test.go) and each consumer repo's own CI-executed test can
// run the identical property assertions against their own vendored copy of
// this package — see docs/refactor/tasks/TASK-053.md in csip-tls-test for
// the full rationale (lexa-proto is unhosted and has no CI of its own, so
// "runs in both repos' CI" is only true if the consumers' own `go test`
// actually calls into this contract, not merely vendors the source).
//
// Each Sweep* function returns a slice of human-readable violation strings;
// an empty/nil slice means every property held. Callers should fail the
// test on any non-empty result. Violation counts are capped per-function so
// a systemic bug doesn't produce an unreadable wall of output.

const maxSweepViolations = 25

// ScaleFactors is the representative sf sweep set named by TASK-053: covers
// whole watts down to milli-precision and up to four decades of headroom
// (watts/scaled-kW range for SunSpec power/energy points).
var ScaleFactors = []int16{-3, -2, -1, 0, 1, 2, 3, 4}

func appendViolation(v []string, format string, args ...interface{}) []string {
	if len(v) >= maxSweepViolations {
		return v
	}
	v = append(v, fmt.Sprintf(format, args...))
	if len(v) == maxSweepViolations {
		v = append(v, "... (further violations suppressed)")
	}
	return v
}

// SweepRoundTripSigned checks, for every sf in sfs and every raw value in
// the full uint16 domain, that RawFromScaleSigned(ApplyScaleSigned(raw, sf), sf)
// == raw. This is the codec's documented inverse property (step 1): decode
// then re-encode must reproduce the original register word exactly, because
// decoded = float64(int16(raw)) * 10^sf always has an exactly-representable
// integer coefficient in int16 range, so re-encoding never needs to clamp.
func SweepRoundTripSigned(sfs []int16) []string {
	var violations []string
	for _, sf := range sfs {
		if sf == notImplemented {
			continue // sentinel is covered by SweepSentinel, not the round-trip contract.
		}
		for r := 0; r <= 0xFFFF; r++ {
			raw := uint16(r)
			if raw == sentI16 {
				// 0x8000 is the reserved NOT_IMPLEMENTED bit pattern for a signed
				// point. ApplyScaleSigned still decodes it as the ordinary int16
				// −32768 (FIX-B, read side), but the encoder deliberately never
				// re-emits the reserved pattern for a data value: −32768 clamps to
				// the max-valid 0x8001 (SUN-004). So the exact round-trip is not
				// required at this one reserved pattern — see SweepSentinel.
				continue
			}
			decoded := ApplyScaleSigned(raw, sf)
			re := RawFromScaleSigned(decoded, sf)
			if re != raw {
				violations = appendViolation(violations,
					"signed round-trip: raw=0x%04x sf=%d decoded=%g re=0x%04x (want 0x%04x)",
					raw, sf, decoded, re, raw)
			}
		}
	}
	return violations
}

// SweepRoundTripUint is SweepRoundTripSigned's unsigned counterpart
// (ApplyScaleUint / RawFromScaleUint) — same contract, unsigned coefficient.
func SweepRoundTripUint(sfs []int16) []string {
	var violations []string
	for _, sf := range sfs {
		if sf == notImplemented {
			continue
		}
		for r := 0; r <= 0xFFFF; r++ {
			raw := uint16(r)
			if raw == sentU16 {
				// 0xFFFF is the reserved NOT_IMPLEMENTED pattern for an unsigned
				// point; 65535 clamps to the max-valid 0xFFFE on encode (SUN-004),
				// so the exact round-trip is not required at this reserved pattern.
				continue
			}
			decoded := ApplyScaleUint(raw, sf)
			re := RawFromScaleUint(decoded, sf)
			if re != raw {
				violations = appendViolation(violations,
					"uint round-trip: raw=0x%04x sf=%d decoded=%g re=0x%04x (want 0x%04x)",
					raw, sf, decoded, re, raw)
			}
		}
	}
	return violations
}

// signedWrapGuardBoundaries are the boundary crossings named explicitly by
// TASK-053 step 2, expressed as base (unscaled, sf=0) watt values. They are
// multiplied by 10^sf for the other scale factors under test so the same
// *logical* crossing (the int16 edge) is probed at every scale.
var signedWrapGuardBoundaries = []int64{
	32767, 32768, 32769, // MaxInt16 and just past it
	-32768, -32769, // MinInt16 and just past it
	65535, 65536, // uint16 wrap points, in case of a raw-cast regression
	-65536, -65535,
	1_000_000, -1_000_000, // deep over-range, must still clamp not wrap
}

// SweepWrapGuardSigned is the GS-1/MTR-1 core property (step 2): a value
// that overflows int16 at a given scale must clamp, never wrap to the
// opposite sign. It checks two things at each sf:
//  1. explicit boundary crossings clamp to the documented int16 edge;
//  2. a dense monotonic sweep across the boundary never produces a
//     decoded output that goes backwards or flips sign as the input grows
//     in magnitude (a sign flip or non-monotonic jump is exactly what a
//     silent int16 wrap looks like).
func SweepWrapGuardSigned(sfs []int16) []string {
	var violations []string
	for _, sf := range sfs {
		if sf == notImplemented {
			continue
		}
		scale := math.Pow10(int(sf))

		// 1. Explicit boundary crossings.
		for _, base := range signedWrapGuardBoundaries {
			val := float64(base) * scale
			raw := RawFromScaleSigned(val, sf)
			decoded := ApplyScaleSigned(raw, sf)

			// Clamp to the MAX-VALID int16 edges, never the reserved sentinel:
			// high +32767 (0x7FFF), low −32767 (0x8001). −32768/0x8000 is the
			// reserved NOT_IMPLEMENTED pattern, so it is out of the valid data
			// range and a value at/below it clamps to −32767 (audit SUN-004).
			wantBase := base
			if wantBase > 32767 {
				wantBase = 32767
			}
			if wantBase < -32767 {
				wantBase = -32767
			}
			want := float64(wantBase) * scale
			if decoded != want {
				violations = appendViolation(violations,
					"wrap-guard boundary: sf=%d base=%d val=%g -> raw=0x%04x decoded=%g, want clamp to %g",
					sf, base, val, raw, decoded, want)
			}
		}

		// 2. Dense monotonic sweep through the boundary region (int16 edge
		// is at +-32767/-32768 in base units; scan +-40000 to comfortably
		// bracket it on both sides).
		var prev float64
		havePrev := false
		for base := -40000; base <= 40000; base++ {
			val := float64(base) * scale
			raw := RawFromScaleSigned(val, sf)
			decoded := ApplyScaleSigned(raw, sf)

			if val > 0 && decoded < 0 {
				violations = appendViolation(violations,
					"wrap-guard sign flip: sf=%d val=%g (positive) decoded=%g (negative) raw=0x%04x",
					sf, val, decoded, raw)
			}
			if val < 0 && decoded > 0 {
				violations = appendViolation(violations,
					"wrap-guard sign flip: sf=%d val=%g (negative) decoded=%g (positive) raw=0x%04x",
					sf, val, decoded, raw)
			}
			if havePrev && decoded < prev {
				violations = appendViolation(violations,
					"wrap-guard non-monotonic: sf=%d val=%g decoded=%g < previous decoded=%g",
					sf, val, decoded, prev)
			}
			prev = decoded
			havePrev = true
		}
	}
	return violations
}

// SweepWrapGuardUint is the unsigned counterpart: negative values clamp to
// 0, values above uint16 max clamp to 65535, and the decoded sequence is
// monotonic-nondecreasing through both boundaries (no wrap to a small
// positive value from a large or negative input).
func SweepWrapGuardUint(sfs []int16) []string {
	var violations []string
	boundaries := []int64{-2, -1, 0, 1, 65534, 65535, 65536, 65537, 1_000_000, -1_000_000}
	for _, sf := range sfs {
		if sf == notImplemented {
			continue
		}
		scale := math.Pow10(int(sf))

		for _, base := range boundaries {
			val := float64(base) * scale
			raw := RawFromScaleUint(val, sf)
			decoded := ApplyScaleUint(raw, sf)

			// Clamp to the MAX-VALID uint16 edge 65534 (0xFFFE), never the
			// reserved 0xFFFF sentinel; negative floors at 0 (audit SUN-004).
			wantBase := base
			if wantBase > 65534 {
				wantBase = 65534
			}
			if wantBase < 0 {
				wantBase = 0
			}
			want := float64(wantBase) * scale
			if decoded != want {
				violations = appendViolation(violations,
					"uint wrap-guard boundary: sf=%d base=%d val=%g -> raw=0x%04x decoded=%g, want clamp to %g",
					sf, base, val, raw, decoded, want)
			}
		}

		var prev float64
		havePrev := false
		for base := -5000; base <= 70000; base += 1 {
			val := float64(base) * scale
			raw := RawFromScaleUint(val, sf)
			decoded := ApplyScaleUint(raw, sf)
			if val < 0 && decoded != 0 {
				violations = appendViolation(violations,
					"uint wrap-guard: sf=%d negative val=%g decoded=%g (want 0)", sf, val, decoded)
			}
			if havePrev && decoded < prev {
				violations = appendViolation(violations,
					"uint wrap-guard non-monotonic: sf=%d val=%g decoded=%g < previous decoded=%g",
					sf, val, decoded, prev)
			}
			prev = decoded
			havePrev = true
		}
	}
	return violations
}

// SweepSentinel pins the FIX-B correctness contract (task review, step 4):
// the notImplemented sentinel (int16(-32768)) guards the SCALE-FACTOR
// argument, not raw register values.
//   - ApplyScale*(anyRaw, sentinel) == NaN, for every raw value.
//   - RawFromScale*(v, sentinel) == 0, for every v (including NaN/+-Inf).
//   - raw 0x8000 at a NORMAL sf is an ordinary int16 (-32768), decodes to
//     -32768 (not NaN), and round-trips exactly like any other value.
//
// This function must NEVER be "fixed" to make ApplyScaleSigned(0x8000, sf)
// return NaN for a non-sentinel sf — that would be exactly the bilateral
// register-semantics regression MTR-4 warns about.
func SweepSentinel() []string {
	var violations []string

	for r := 0; r <= 0xFFFF; r++ {
		raw := uint16(r)
		if got := ApplyScaleSigned(raw, notImplemented); !math.IsNaN(got) {
			violations = appendViolation(violations,
				"ApplyScaleSigned(0x%04x, sentinel) = %v, want NaN", raw, got)
		}
		if got := ApplyScaleUint(raw, notImplemented); !math.IsNaN(got) {
			violations = appendViolation(violations,
				"ApplyScaleUint(0x%04x, sentinel) = %v, want NaN", raw, got)
		}
	}

	sampleValues := []float64{0, 1, -1, 32767, -32768, 65535, 1e9, -1e9,
		math.NaN(), math.Inf(1), math.Inf(-1)}
	for _, v := range sampleValues {
		if got := RawFromScaleSigned(v, notImplemented); got != 0 {
			violations = appendViolation(violations,
				"RawFromScaleSigned(%v, sentinel) = 0x%04x, want 0", v, got)
		}
		if got := RawFromScaleUint(v, notImplemented); got != 0 {
			violations = appendViolation(violations,
				"RawFromScaleUint(%v, sentinel) = 0x%04x, want 0", v, got)
		}
	}

	// FIX-B pin (READ side, KEEP): raw 0x8000 at a normal (non-sentinel) sf is
	// an ordinary int16 value, −32768 — it decodes to −32768, NOT NaN and NOT
	// the "not implemented" marker (the MTR-4 bilateral-register-semantics
	// guard: the sentinel guards the SCALE FACTOR, not raw register values).
	//
	// SUN-004 (WRITE side): re-encoding that same −32768 must NOT emit the
	// reserved 0x8000 pattern — a finite value at the reserved negative extreme
	// clamps to the max-valid −32767 (0x8001). So the read side keeps
	// 0x8000 → −32768 while the write side maps −32768 → 0x8001; the round-trip
	// intentionally does not hold at this one reserved bit pattern.
	for _, sf := range ScaleFactors {
		dec := -32768.0 * math.Pow10(int(sf))
		if got := ApplyScaleSigned(0x8000, sf); got != dec {
			violations = appendViolation(violations,
				"FIX-B: ApplyScaleSigned(0x8000, %d) = %v, want %v (raw 0x8000 at a normal sf is an ordinary int16 −32768, not the sentinel)",
				sf, got, dec)
		}
		if re := RawFromScaleSigned(dec, sf); re != 0x8001 {
			violations = appendViolation(violations,
				"SUN-004: RawFromScaleSigned(%v, %d) = 0x%04x, want 0x8001 (finite −32768 clamps to max-valid −32767, never the reserved 0x8000 sentinel)",
				dec, sf, re)
		}
	}

	return violations
}

// SweepNaNInfEncode pins the RawFromScale* encoders' handling of the two
// distinct out-of-range classes the codec must NOT conflate (audit SUN-004):
//
//   - NON-FINITE (NaN, ±Inf) — a genuinely unknown / unrepresentable value —
//     encodes the reserved NOT_IMPLEMENTED sentinel (signed 0x8000, unsigned
//     0xFFFF). This is correct: it marks the point "not implemented" on the
//     wire, which is exactly what a value with no representable magnitude is.
//
//   - FINITE but out-of-range — a real, in-service measurement/command that
//     merely exceeds the register's range — clamps to the MAX-VALID edge
//     (signed +32767/−32767, unsigned 65534/0), NEVER the reserved sentinel.
//     Encoding such a value AS 0x8000/0xFFFF would silently corrupt it into
//     "not implemented" — the SUN-004 bug this test now guards against.
func SweepNaNInfEncode() []string {
	var violations []string
	for _, sf := range ScaleFactors {
		// Non-finite ⇒ reserved NOT_IMPLEMENTED sentinel.
		for _, nf := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
			if got := RawFromScaleSigned(nf, sf); got != sentI16 {
				violations = appendViolation(violations,
					"RawFromScaleSigned(%v, %d) = 0x%04x, want 0x8000 (non-finite → NOT_IMPLEMENTED sentinel)",
					nf, sf, got)
			}
			if got := RawFromScaleUint(nf, sf); got != sentU16 {
				violations = appendViolation(violations,
					"RawFromScaleUint(%v, %d) = 0x%04x, want 0xffff (non-finite → NOT_IMPLEMENTED sentinel)",
					nf, sf, got)
			}
		}

		// Finite but far out of range ⇒ MAX-VALID edge, never the sentinel.
		scale := math.Pow10(int(sf))
		hi := 1e6 * scale
		lo := -1e6 * scale
		if got := RawFromScaleSigned(hi, sf); got != 0x7FFF {
			violations = appendViolation(violations,
				"RawFromScaleSigned(%g, %d) = 0x%04x, want 0x7fff (finite over-range high → max-valid +32767, not sentinel)",
				hi, sf, got)
		}
		if got := RawFromScaleSigned(lo, sf); got != 0x8001 {
			violations = appendViolation(violations,
				"RawFromScaleSigned(%g, %d) = 0x%04x, want 0x8001 (finite over-range low → max-valid −32767, not sentinel 0x8000)",
				lo, sf, got)
		}
		if got := RawFromScaleUint(hi, sf); got != 0xFFFE {
			violations = appendViolation(violations,
				"RawFromScaleUint(%g, %d) = 0x%04x, want 0xfffe (finite over-range high → max-valid 65534, not sentinel 0xffff)",
				hi, sf, got)
		}
		if got := RawFromScaleUint(lo, sf); got != 0 {
			violations = appendViolation(violations,
				"RawFromScaleUint(%g, %d) = 0x%04x, want 0 (finite negative → unsigned floor 0)",
				lo, sf, got)
		}
	}
	return violations
}
