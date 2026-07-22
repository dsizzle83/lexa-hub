package sunspec

import "math"

// notImplemented is the SunSpec sentinel value for int16 scale factors meaning
// "this point is not implemented on this device".
const notImplemented = int16(-32768) // 0x8000 reinterpreted as int16

// ApplyScaleUint converts an unsigned SunSpec register value to a float64 by
// applying the given scale factor: actual = raw × 10^sf.
// Returns math.NaN() when sf is the "not implemented" sentinel (0x8000).
func ApplyScaleUint(raw uint16, sf int16) float64 {
	if sf == notImplemented {
		return math.NaN()
	}
	return float64(raw) * math.Pow10(int(sf))
}

// ApplyScaleSigned converts a signed SunSpec register value (int16 stored in
// a uint16 word) to a float64 by applying the given scale factor.
// Returns math.NaN() when sf is the "not implemented" sentinel.
func ApplyScaleSigned(raw uint16, sf int16) float64 {
	if sf == notImplemented {
		return math.NaN()
	}
	return float64(int16(raw)) * math.Pow10(int(sf))
}

// RawFromScaleSigned converts a float64 value back to a uint16 register word
// given the target scale factor. Used when writing control values to a device.
// The result is rounded to the nearest integer, then:
//
//   - a NON-FINITE value (NaN, ±Inf) — genuinely unknown/unrepresentable —
//     encodes the reserved NOT_IMPLEMENTED sentinel (0x8000);
//   - a FINITE but out-of-range value clamps to the MAX-VALID int16 edge —
//     +32767 (0x7FFF) high, −32767 (0x8001) low — NEVER the reserved 0x8000
//     sentinel. Emitting 0x8000 for a real, in-service value would corrupt it
//     into "not implemented" on the wire (audit SUN-004).
func RawFromScaleSigned(val float64, sf int16) uint16 {
	if sf == notImplemented {
		return 0
	}
	if math.IsNaN(val) || math.IsInf(val, 0) {
		return sentI16 // 0x8000 — no representable value ⇒ NOT_IMPLEMENTED.
	}
	rounded := math.Round(val / math.Pow10(int(sf)))
	if rounded > maxValidI16 {
		rounded = maxValidI16 // +32767 (0x7FFF)
	}
	if rounded < minValidI16 {
		rounded = minValidI16 // −32767 (0x8001); −32768/0x8000 is reserved.
	}
	return uint16(int16(rounded))
}

// RawFromScaleUint converts a float64 value to a uint16 register word given the
// target scale factor. A non-finite value (NaN, ±Inf) encodes the reserved
// NOT_IMPLEMENTED sentinel (0xFFFF); a finite negative value floors at 0; a
// finite over-range value clamps to the MAX-VALID edge 65534 (0xFFFE), NEVER
// the reserved 0xFFFF sentinel (audit SUN-004).
func RawFromScaleUint(val float64, sf int16) uint16 {
	if sf == notImplemented {
		return 0
	}
	if math.IsNaN(val) || math.IsInf(val, 0) {
		return sentU16 // 0xFFFF — no representable value ⇒ NOT_IMPLEMENTED.
	}
	if val < 0 {
		return 0
	}
	rounded := math.Round(val / math.Pow10(int(sf)))
	if rounded > maxValidU16 {
		rounded = maxValidU16 // 65534 (0xFFFE); 65535/0xFFFF is reserved.
	}
	if rounded < 0 {
		return 0
	}
	return uint16(rounded)
}
