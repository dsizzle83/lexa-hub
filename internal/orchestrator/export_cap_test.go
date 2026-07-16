package orchestrator

import (
	"math"
	"testing"
)

// planExportDischargeCapW sign-convention tests (audit EXPCAP-1/5). The cap is
// conservativeLimit − baseExport, where baseExport is "export attributable to
// everything but the battery" = signed net export − SIGNED battery power. The
// bug was summing only positive (discharge) battery power, so a CHARGING pack
// was credited as immovable site load and the cap was inflated by the full
// charge magnitude — briefly letting a plan discharge export past the cap at
// every charge→discharge flip.

func capFor(o *DefaultOptimizer, exportLimitW, netW float64, bats []BatteryState) float64 {
	return o.planExportDischargeCapW(
		gridConstraints{exportLimitW: exportLimitW, importLimitW: math.NaN(), maxLimitW: math.NaN()},
		SystemState{Grid: GridState{NetW: netW}, Batteries: bats},
	)
}

// The finding: a charging pack must NOT inflate the cap. Site net-importing
// 4000 W (load 1000 + charge 3000), export cap 1000 W (conservative 800):
// true headroom is 800 + 1000 load = 1800 W, NOT 4800 W.
func TestPlanExportDischargeCap_ChargingPackNotCreditedAsLoad(t *testing.T) {
	o := NewDefaultOptimizer() // ExportMarginFrac 0.20
	bats := []BatteryState{{Name: "b", PowerW: -3000, SOC: 50, Connected: true, MaxChargeW: 5000, MaxDischargeW: 5000}}
	got := capFor(o, 1000, 4000, bats) // NetW=+4000 importing
	if math.Abs(got-1800) > 1 {
		t.Fatalf("cap = %.0fW, want 1800W (conservative 800 + 1000W load); the old bug returned 4800 by crediting the 3000W charge as load", got)
	}
}

// The load-credit case must still work: net-importing 250 W with an idle pack
// under a 0 W export cap may still discharge up to the load (offsetting import,
// exporting nothing). conservative 0 − baseExport(−250) = 250.
func TestPlanExportDischargeCap_NetImportingCreditsLoad(t *testing.T) {
	o := NewDefaultOptimizer()
	bats := []BatteryState{{Name: "b", PowerW: 0, SOC: 50, Connected: true, MaxDischargeW: 5000}}
	got := capFor(o, 0, 250, bats) // 0 W export cap, importing 250 W, pack idle
	if math.Abs(got-250) > 1 {
		t.Fatalf("cap = %.0fW, want 250W (may discharge up to local load under a 0W export cap)", got)
	}
}

// A discharging pack mid-ramp: the subtraction of the flowing discharge keeps
// the cap stable. Site exporting 4750 W, pack discharging 3000 W of it, cap
// 1000 W (conservative 800): base = 4750 − 3000 = 1750 non-battery export; cap
// = max(0, 800 − 1750) = 0 (no headroom — already over even without battery).
func TestPlanExportDischargeCap_DischargingPackSubtracted(t *testing.T) {
	o := NewDefaultOptimizer()
	bats := []BatteryState{{Name: "b", PowerW: 3000, SOC: 60, Connected: true, MaxDischargeW: 5000}}
	got := capFor(o, 1000, -4750, bats) // exporting 4750 W
	if got != 0 {
		t.Fatalf("cap = %.0fW, want 0W (non-battery export 1750W already over the 800W conservative cap)", got)
	}
}

// No grid meter (NetW NaN): fall back to a base of 0 ⇒ cap = conservative limit,
// a pure-discharge bound that can never itself exceed the cap.
func TestPlanExportDischargeCap_NaNMeterFallsBackToConservative(t *testing.T) {
	o := NewDefaultOptimizer()
	bats := []BatteryState{{Name: "b", PowerW: 0, SOC: 50, Connected: true, MaxDischargeW: 5000}}
	got := capFor(o, 1000, math.NaN(), bats)
	if math.Abs(got-800) > 1 {
		t.Fatalf("cap = %.0fW, want 800W (conservative limit) with no meter", got)
	}
}

// No export limit ⇒ NaN ⇒ no cap (byte-identical to pre-fix plans).
func TestPlanExportDischargeCap_NoLimitReturnsNaN(t *testing.T) {
	o := NewDefaultOptimizer()
	if got := capFor(o, math.NaN(), 4000, nil); !math.IsNaN(got) {
		t.Fatalf("cap = %.0f, want NaN (no export limit ⇒ no cap)", got)
	}
}
