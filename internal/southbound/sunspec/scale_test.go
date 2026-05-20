package sunspec

import (
	"math"
	"testing"
)

func TestApplyScaleUint(t *testing.T) {
	tests := []struct {
		raw  uint16
		sf   int16
		want float64
	}{
		{1000, 0, 1000},
		{1000, -1, 100},
		{1000, -2, 10},
		{1000, 1, 10000},
		{5000, -2, 50},   // 50.00 Hz
		{2400, -1, 240},  // 240.0 V
		{0, 0, 0},
	}
	for _, tc := range tests {
		got := ApplyScaleUint(tc.raw, tc.sf)
		if math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("ApplyScaleUint(%d, %d) = %g, want %g", tc.raw, tc.sf, got, tc.want)
		}
	}
}

func TestApplyScaleUint_NotImplemented(t *testing.T) {
	got := ApplyScaleUint(1000, -32768)
	if !math.IsNaN(got) {
		t.Errorf("expected NaN for not-implemented SF, got %g", got)
	}
}

func TestApplyScaleSigned(t *testing.T) {
	neg500 := int16(-500)
	neg100 := int16(-100)
	tests := []struct {
		raw  uint16
		sf   int16
		want float64
	}{
		{uint16(int16(3000)), 0, 3000},   // 3000 W
		{uint16(neg500), 0, -500},        // -500 W (negative power)
		{uint16(int16(1000)), -1, 100},   // 100.0 W
		{uint16(neg100), -1, -10},        // -10.0 W
		{uint16(int16(9500)), -2, 95},    // 95.00%
	}
	for _, tc := range tests {
		got := ApplyScaleSigned(tc.raw, tc.sf)
		if math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("ApplyScaleSigned(0x%04x, %d) = %g, want %g", tc.raw, tc.sf, got, tc.want)
		}
	}
}

func TestApplyScaleSigned_NotImplemented(t *testing.T) {
	got := ApplyScaleSigned(0, -32768)
	if !math.IsNaN(got) {
		t.Errorf("expected NaN for not-implemented SF, got %g", got)
	}
}

func TestRawFromScaleUint_RoundTrip(t *testing.T) {
	tests := []struct {
		val float64
		sf  int16
	}{
		{100.0, -2},  // 100.00 → raw=10000
		{240.0, -1},  // 240.0 V → raw=2400
		{50.0, -2},   // 50.00 Hz → raw=5000
		{0.0, 0},
		{65535.0, 0}, // max uint16
	}
	for _, tc := range tests {
		raw := RawFromScaleUint(tc.val, tc.sf)
		got := ApplyScaleUint(raw, tc.sf)
		if math.Abs(got-tc.val) > math.Pow10(int(tc.sf)) {
			t.Errorf("round-trip %g (sf=%d): raw=%d → %g", tc.val, tc.sf, raw, got)
		}
	}
}

func TestRawFromScaleSigned_RoundTrip(t *testing.T) {
	tests := []struct {
		val float64
		sf  int16
	}{
		{3000.0, 0},
		{-500.0, 0},
		{100.5, -1},
		{-10.3, -1},
	}
	for _, tc := range tests {
		raw := RawFromScaleSigned(tc.val, tc.sf)
		got := ApplyScaleSigned(raw, tc.sf)
		tol := math.Pow10(int(tc.sf))
		if math.Abs(got-tc.val) > tol {
			t.Errorf("round-trip %g (sf=%d): raw=%d → %g (tol %g)", tc.val, tc.sf, raw, got, tol)
		}
	}
}

func TestRawFromScaleUint_Clamp(t *testing.T) {
	// Negative values should clamp to 0.
	if got := RawFromScaleUint(-100, 0); got != 0 {
		t.Errorf("negative value should clamp to 0, got %d", got)
	}
	// Values above uint16 max should clamp to 65535.
	if got := RawFromScaleUint(1e9, 0); got != 65535 {
		t.Errorf("overflow should clamp to 65535, got %d", got)
	}
}
