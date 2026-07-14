package meter

import (
	"math"
	"testing"

	"lexa-proto/sunspec"
)

// mk builds a zeroed register slice of the common-meter length and applies
// the given point writes (offset → raw word). Writing raw words directly —
// including the two-word acc32 splits — keeps each case's wire bytes
// explicit, the same discipline the sunspecsweep contract tests use for the
// scale codec.
func mk(n int, set map[int]uint16) []uint16 {
	r := make([]uint16, n)
	for o, v := range set {
		r[o] = v
	}
	return r
}

// approxEq compares with a relative tolerance wide enough for one Pow10
// rounding step, tight enough to catch any offset/assembly mistake.
func approxEq(a, b float64) bool {
	if a == b {
		return true
	}
	return math.Abs(a-b) <= 1e-9*math.Max(math.Abs(a), math.Abs(b))
}

// i16 reinterprets a signed point value as its raw register word (a runtime
// conversion — a constant uint16(int16(-1)) is a compile-time overflow).
func i16(v int16) uint16 { return uint16(v) }

// base201 is a plausible single-phase meter snapshot: 2.5 kW net export,
// 240.1 V, 60.02 Hz, 2600 VA, 150.5 VAr, PF 0.96, 12.5 MWh imported /
// 3.4 MWh exported lifetime.
func base201() map[int]uint16 {
	return map[int]uint16{
		sunspec.M201_W:      i16(-2500), // export (meter sign: − = export)
		sunspec.M201_W_SF:   0,
		sunspec.M201_PhVphA: 2401,
		sunspec.M201_V_SF:   i16(-1),
		sunspec.M201_Hz:     6002,
		sunspec.M201_Hz_SF:  i16(-2),
		sunspec.M201_VA:     2600,
		sunspec.M201_VA_SF:  0,
		sunspec.M201_VAR:    1505,
		sunspec.M201_VAR_SF: i16(-1),
		sunspec.M201_PF:     96,
		sunspec.M201_PF_SF:  0,
		// acc32, big-endian high word first: 3,400,000 = 0x0033_E140.
		sunspec.M201_TotWhExp:     0x0033,
		sunspec.M201_TotWhExp + 1: 0xE140,
		// 12,500,000 = 0x00BE_BC20.
		sunspec.M201_TotWhImp:     0x00BE,
		sunspec.M201_TotWhImp + 1: 0xBC20,
		sunspec.M201_TotWh_SF:     0,
	}
}

func TestParseMeterModelWhTotals(t *testing.T) {
	sfSentinel := uint16(0x8000) // sunssf "not implemented"

	cases := []struct {
		name    string
		modelID uint16
		mut     func(map[int]uint16) // applied over base201
		wantImp float64              // NaN = absent
		wantExp float64
	}{
		{
			name:    "201 full parse",
			modelID: sunspec.ModelMeterSinglePh,
			mut:     func(map[int]uint16) {},
			wantImp: 12500000,
			wantExp: 3400000,
		},
		{
			// 203 shares the M201 common-meter offsets (models.go note); a
			// non-zero scale factor exercises the shared TotWh_SF once.
			name:    "203 same offsets, sf=1",
			modelID: sunspec.ModelMeterThreePh,
			mut:     func(s map[int]uint16) { s[sunspec.M201_TotWh_SF] = 1 },
			wantImp: 125000000,
			wantExp: 34000000,
		},
		{
			name:    "scale factor sentinel withholds both",
			modelID: sunspec.ModelMeterSinglePh,
			mut:     func(s map[int]uint16) { s[sunspec.M201_TotWh_SF] = sfSentinel },
			wantImp: math.NaN(),
			wantExp: math.NaN(),
		},
		{
			name:    "acc32 zero sentinel is absent",
			modelID: sunspec.ModelMeterSinglePh,
			mut: func(s map[int]uint16) {
				s[sunspec.M201_TotWhImp] = 0
				s[sunspec.M201_TotWhImp+1] = 0
			},
			wantImp: math.NaN(),
			wantExp: 3400000,
		},
		{
			// hi word set, lo word zero: a 16-bit truncation would decode 0
			// (sentinel-absent); only correct big-endian 32-bit assembly
			// yields 65536 — the register-wrap class GS-1/MTR-1 is about.
			name:    "acc32 crosses the 16-bit boundary",
			modelID: sunspec.ModelMeterSinglePh,
			mut: func(s map[int]uint16) {
				s[sunspec.M201_TotWhImp] = 1
				s[sunspec.M201_TotWhImp+1] = 0
			},
			wantImp: 65536,
			wantExp: 3400000,
		},
		{
			name:    "acc32 max value survives",
			modelID: sunspec.ModelMeterSinglePh,
			mut: func(s map[int]uint16) {
				s[sunspec.M201_TotWhExp] = 0xFFFF
				s[sunspec.M201_TotWhExp+1] = 0xFFFF
			},
			wantImp: 12500000,
			wantExp: 4294967295,
		},
		{
			name:    "202 does not parse Wh",
			modelID: sunspec.ModelMeterSplitPh,
			mut:     func(map[int]uint16) {},
			wantImp: math.NaN(),
			wantExp: math.NaN(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set := base201()
			tc.mut(set)
			m := parseMeterModel(mk(sunspec.M201Len, set), tc.modelID)

			check := func(name string, got, want float64) {
				if math.IsNaN(want) {
					if !math.IsNaN(got) {
						t.Errorf("%s = %v, want NaN (absent)", name, got)
					}
					return
				}
				if !approxEq(got, want) {
					t.Errorf("%s = %v, want %v", name, got, want)
				}
			}
			check("WhImpTotal", m.WhImpTotal, tc.wantImp)
			check("WhExpTotal", m.WhExpTotal, tc.wantExp)
		})
	}
}

// TestParseMeterModelPowerQuality locks the shared W/V/Hz/VA/Var/PF parse
// conventions the Wh parse follows (offset table + scale-factor application),
// and the NaN-absent initialization the 202 path relies on so cmd/modbus
// never publishes a fabricated zero for a quantity the model lacks (G27).
func TestParseMeterModelPowerQuality(t *testing.T) {
	m := parseMeterModel(mk(sunspec.M201Len, base201()), sunspec.ModelMeterSinglePh)
	for _, q := range []struct {
		name      string
		got, want float64
	}{
		{"W", m.W, -2500},
		{"V", m.V, 240.1},
		{"Hz", m.Hz, 60.02},
		{"VA", m.VA, 2600},
		{"Var", m.Var, 150.5},
		{"PF", m.PF, 0.96},
	} {
		if !approxEq(q.got, q.want) {
			t.Errorf("%s = %v, want %v", q.name, q.got, q.want)
		}
	}

	// 202: VA/Var/PF (and SOC, always) must be NaN-absent, never zero.
	m202 := parseMeterModel(mk(sunspec.M201Len, base201()), sunspec.ModelMeterSplitPh)
	for _, q := range []struct {
		name string
		got  float64
	}{
		{"VA", m202.VA}, {"Var", m202.Var}, {"PF", m202.PF}, {"SOC", m202.SOC},
	} {
		if !math.IsNaN(q.got) {
			t.Errorf("202 %s = %v, want NaN (absent — G27)", q.name, q.got)
		}
	}
}

// TestParseMeterModelTruncatedRegisters proves a register read shorter than
// the Wh points degrades to absent (NaN), never to a partial/garbage decode.
func TestParseMeterModelTruncatedRegisters(t *testing.T) {
	regs := mk(sunspec.M201Len, base201())[:sunspec.M201_TotWhExp+2] // cut before TotWhImp/TotWh_SF
	m := parseMeterModel(regs, sunspec.ModelMeterSinglePh)
	if !math.IsNaN(m.WhImpTotal) || !math.IsNaN(m.WhExpTotal) {
		t.Errorf("truncated read: WhImpTotal=%v WhExpTotal=%v, want both NaN", m.WhImpTotal, m.WhExpTotal)
	}
}
