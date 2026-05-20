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
// The result is rounded to the nearest integer and clamped to int16 range.
func RawFromScaleSigned(val float64, sf int16) uint16 {
	if sf == notImplemented {
		return 0
	}
	scaled := val / math.Pow10(int(sf))
	rounded := math.Round(scaled)
	// Clamp to int16 range.
	if rounded > math.MaxInt16 {
		rounded = math.MaxInt16
	}
	if rounded < math.MinInt16 {
		rounded = math.MinInt16
	}
	return uint16(int16(rounded))
}

// RawFromScaleUint converts a non-negative float64 value to a uint16 register
// word given the target scale factor. Negative values are clamped to 0.
func RawFromScaleUint(val float64, sf int16) uint16 {
	if sf == notImplemented || val < 0 {
		return 0
	}
	scaled := val / math.Pow10(int(sf))
	rounded := math.Round(scaled)
	if rounded > math.MaxUint16 {
		rounded = math.MaxUint16
	}
	if rounded < 0 {
		return 0
	}
	return uint16(rounded)
}
