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

			wantBase := base
			if wantBase > math.MaxInt16 {
				wantBase = math.MaxInt16
			}
			if wantBase < math.MinInt16 {
				wantBase = math.MinInt16
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

			wantBase := base
			if wantBase > math.MaxUint16 {
				wantBase = math.MaxUint16
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

	// FIX-B pin: raw 0x8000 at a normal (non-sentinel) sf is an ordinary
	// int16 value, -32768 -- NOT the "not implemented" marker.
	for _, sf := range ScaleFactors {
		want := -32768.0 * math.Pow10(int(sf))
		got := ApplyScaleSigned(0x8000, sf)
		if got != want {
			violations = appendViolation(violations,
				"FIX-B: ApplyScaleSigned(0x8000, %d) = %v, want %v (raw 0x8000 at a normal sf is an ordinary int16, not the sentinel)",
				sf, got, want)
		}
		if re := RawFromScaleSigned(want, sf); re != 0x8000 {
			violations = appendViolation(violations,
				"FIX-B: RawFromScaleSigned(%v, %d) = 0x%04x, want 0x8000 (must round-trip)",
				want, sf, re)
		}
	}

	return violations
}

// SweepNaNInfEncode pins the (previously undocumented, now regression-swept)
// behavior of the RawFromScale* encoders when handed a non-finite float:
// encoding NaN/+-Inf must not produce a spurious sentinel raw value and must
// not wrap -- it must clamp/degrade predictably (step 4).
//
//   - NaN encodes to raw 0 (Go's float->int conversion of NaN; harmless
//     because raw 0 decodes back to an ordinary in-range value, 0, not the
//     sentinel raw 0x8000).
//   - +Inf clamps to the maximum representable raw (MaxInt16 / MaxUint16).
//   - -Inf clamps to the minimum representable raw (MinInt16 signed / 0
//     unsigned).
func SweepNaNInfEncode() []string {
	var violations []string
	for _, sf := range ScaleFactors {
		if got := RawFromScaleSigned(math.NaN(), sf); got != 0 {
			violations = appendViolation(violations,
				"RawFromScaleSigned(NaN, %d) = 0x%04x, want 0", sf, got)
		}
		if got := RawFromScaleUint(math.NaN(), sf); got != 0 {
			violations = appendViolation(violations,
				"RawFromScaleUint(NaN, %d) = 0x%04x, want 0", sf, got)
		}

		wantMaxSignedRaw := uint16(int16(math.MaxInt16))
		if got := RawFromScaleSigned(math.Inf(1), sf); got != wantMaxSignedRaw {
			violations = appendViolation(violations,
				"RawFromScaleSigned(+Inf, %d) = 0x%04x, want 0x%04x (MaxInt16 clamp)",
				sf, got, wantMaxSignedRaw)
		}
		if got := RawFromScaleUint(math.Inf(1), sf); got != math.MaxUint16 {
			violations = appendViolation(violations,
				"RawFromScaleUint(+Inf, %d) = 0x%04x, want 0xffff (MaxUint16 clamp)", sf, got)
		}

		minInt16 := int16(math.MinInt16) // break constant folding: uint16(int16(-32768)) is a valid runtime reinterpretation (-> 0x8000) but not a valid constant conversion.
		wantMinSignedRaw := uint16(minInt16)
		if got := RawFromScaleSigned(math.Inf(-1), sf); got != wantMinSignedRaw {
			violations = appendViolation(violations,
				"RawFromScaleSigned(-Inf, %d) = 0x%04x, want 0x%04x (MinInt16 clamp)",
				sf, got, wantMinSignedRaw)
		}
		if got := RawFromScaleUint(math.Inf(-1), sf); got != 0 {
			violations = appendViolation(violations,
				"RawFromScaleUint(-Inf, %d) = 0x%04x, want 0 (negative clamp)", sf, got)
		}
	}
	return violations
}
