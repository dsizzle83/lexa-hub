package orchestrator

import (
	"encoding/json"
	"math"
	"testing"
)

const eps = 1e-9

// TestPlantDefaults_ReproduceBenchConstants is the core equivalence proof
// (TASK-057 acceptance): a zero-valued plant, run through withDefaults, must
// yield EXACTLY today's optimizer.go constants — including the per-second ↔
// per-tick conversions round-tripping back to the legacy per-tick slews.
func TestPlantDefaults_ReproduceBenchConstants(t *testing.T) {
	tick := tunedTickInterval.Seconds() // 3 s — the cadence the constants encode

	inv := InverterPlant{}.WithDefaults()
	// MaxRampDownWPerS × tick must recover maxDropW = 1500 W/tick (optimizer.go).
	if got := inv.MaxRampDownWPerS * tick; math.Abs(got-1500.0) > eps {
		t.Errorf("MaxRampDownWPerS*%.0fs = %v, want 1500 (optimizer maxDropW)", tick, got)
	}
	// MaxRampUpWPerS × tick must recover maxRiseW = 500 W/tick.
	if got := inv.MaxRampUpWPerS * tick; math.Abs(got-500.0) > eps {
		t.Errorf("MaxRampUpWPerS*%.0fs = %v, want 500 (optimizer maxRiseW)", tick, got)
	}
	if math.Abs(inv.ControlLatencyS-3.0) > eps {
		t.Errorf("Inverter ControlLatencyS = %v, want 3 (one tuned tick)", inv.ControlLatencyS)
	}

	bat := BatteryPlant{}.WithDefaults()
	if math.Abs(bat.CapacityKWh-10.0) > eps {
		t.Errorf("CapacityKWh = %v, want 10 (bench pack)", bat.CapacityKWh)
	}
	if math.Abs(bat.SOCTaperStartPct-80.0) > eps {
		t.Errorf("SOCTaperStartPct = %v, want 80 (optimizer socTaperStart)", bat.SOCTaperStartPct)
	}
	if math.Abs(bat.ConvergeFrac-battConvergeFrac) > eps {
		t.Errorf("ConvergeFrac = %v, want %v (optimizer battConvergeFrac)", bat.ConvergeFrac, battConvergeFrac)
	}
	if math.Abs(bat.ControlLatencyS-3.0) > eps {
		t.Errorf("Battery ControlLatencyS = %v, want 3", bat.ControlLatencyS)
	}
	if bat.TaperCurve != nil {
		t.Errorf("TaperCurve = %v, want nil (empty = linear taper default)", bat.TaperCurve)
	}

	if got := (MeterPlant{}).WithDefaults().MeterLagS; math.Abs(got-5.0) > eps {
		t.Errorf("Meter MeterLagS = %v, want 5 (bench meter cadence)", got)
	}
	if got := (EVSEPlant{}).WithDefaults().MeterLagS; math.Abs(got-10.0) > eps {
		t.Errorf("EVSE MeterLagS = %v, want 10 (OCPP MeterValues cadence)", got)
	}
}

// TestPlantDefaults_PreserveExplicitValues verifies withDefaults only fills
// zero fields — a partially-populated plant keeps every value it set.
func TestPlantDefaults_PreserveExplicitValues(t *testing.T) {
	inv := InverterPlant{MaxRampDownWPerS: 42}.WithDefaults()
	if inv.MaxRampDownWPerS != 42 {
		t.Errorf("explicit MaxRampDownWPerS overwritten: got %v", inv.MaxRampDownWPerS)
	}
	if inv.MaxRampUpWPerS == 0 || inv.ControlLatencyS == 0 {
		t.Errorf("absent fields not defaulted: %+v", inv)
	}

	curve := []TaperPoint{{SOCPct: 80, Frac: 1}, {SOCPct: 100, Frac: 0}}
	bat := BatteryPlant{CapacityKWh: 20, TaperCurve: curve}.WithDefaults()
	if bat.CapacityKWh != 20 {
		t.Errorf("explicit CapacityKWh overwritten: got %v", bat.CapacityKWh)
	}
	if len(bat.TaperCurve) != 2 {
		t.Errorf("explicit TaperCurve dropped: got %v", bat.TaperCurve)
	}
	if bat.SOCTaperStartPct != 80 || bat.ConvergeFrac != 0.5 {
		t.Errorf("absent battery fields not defaulted: %+v", bat)
	}
}

// TestPlantJSONRoundTrip checks the wire tags: the snake_case keys the hub.json
// example ships decode into the typed fields and marshal back unchanged.
func TestPlantJSONRoundTrip(t *testing.T) {
	src := `{"max_ramp_down_w_per_s":500,"max_ramp_up_w_per_s":166.6666666667,"control_latency_s":3}`
	var inv InverterPlant
	if err := json.Unmarshal([]byte(src), &inv); err != nil {
		t.Fatalf("unmarshal inverter plant: %v", err)
	}
	if inv.MaxRampDownWPerS != 500 || inv.ControlLatencyS != 3 {
		t.Fatalf("inverter decode mismatch: %+v", inv)
	}

	bsrc := `{"capacity_kwh":10,"soc_taper_start_pct":80,"converge_frac":0.5,"control_latency_s":3,` +
		`"taper_curve":[{"soc_pct":80,"frac":1},{"soc_pct":95,"frac":0}]}`
	var bat BatteryPlant
	if err := json.Unmarshal([]byte(bsrc), &bat); err != nil {
		t.Fatalf("unmarshal battery plant: %v", err)
	}
	if len(bat.TaperCurve) != 2 || bat.TaperCurve[0].SOCPct != 80 || bat.TaperCurve[1].Frac != 0 {
		t.Fatalf("taper curve decode mismatch: %+v", bat.TaperCurve)
	}
}
