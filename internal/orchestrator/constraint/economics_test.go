package constraint

import (
	"math"
	"testing"
	"time"

	"lexa-hub/internal/orchestrator"
	model "lexa-proto/csipmodel"
)

// These pin EconomicsConstraint (TASK-063) against the legacy economic rules it
// ports. The direct tests assert the proposal a single rule emits; the golden
// shadow-parity test (bottom) drives the WHOLE stack against the real
// DefaultOptimizer through a multi-tick sequence and asserts zero divergence on
// the ticks where the two are expected to agree (no active CSIP cap — where the
// cascade's compliance interleaving is a no-op, so a below-compliance economics
// layer is faithful). On-cap divergence is CHARACTERIZED separately, not asserted
// as parity (task: economics is where the cascade has the most bench-calibrated
// behaviour; that difference is TASK-064's finding).

// ── helpers ──────────────────────────────────────────────────────────────────

const (
	benchSOCReserve   = 20.0
	benchSOCFull      = 95.0
	benchExcessSolar  = 100.0
	benchExportMargin = 0.20
	benchEVCooldown   = 20
)

// newEconomicsPair returns a fresh economics constraint (bench config, default TOU
// schedule) and its base session at the tuned tick.
func newEconomicsPair() (*EconomicsConstraint, *Session) {
	c := NewEconomicsConstraint(orchestrator.DefaultTOUCostModel(),
		benchSOCReserve, benchSOCFull, benchExcessSolar, benchExportMargin, benchEVCooldown)
	return c, NewSession("economics", 0)
}

// battDemand returns the economics battery setpoint proposed for a device, or NaN.
func battDemand(demands []Demand, name string) float64 {
	for _, d := range demands {
		if d.Device == name && d.Axis == AxisBatterySetpointW {
			return d.Min // PointDemand: Min==Max
		}
	}
	return math.NaN()
}

// evDemand returns the economics EV current proposed for a connector, or NaN.
func evDemand(demands []Demand, station string, connector int) float64 {
	key := evseKey(station, connector)
	for _, d := range demands {
		if d.Device == key && d.Axis == AxisEVSECurrentA {
			return d.Min
		}
	}
	return math.NaN()
}

// peak/off-peak are keyed to LOCAL time because TOUCostModel.IsPeakHour reads the
// local hour of the instant (costmodel.go), and both the legacy optimizer and the
// economics constraint derive serverNow via time.Unix(...) → local. Using
// time.Local keeps these deterministic w.r.t. the DefaultTOUCostModel 16:00–21:00
// peak window regardless of the machine timezone.
func peakTime() time.Time    { return time.Date(2026, 7, 6, 17, 0, 0, 0, time.Local) } // 17:00 local → peak
func offPeakTime() time.Time { return time.Date(2026, 7, 6, 3, 0, 0, 0, time.Local) }  // 03:00 local → off-peak

// ── Rule 4: self-consumption ────────────────────────────────────────────────────

func TestEconomics_SelfConsumptionChargesSurplus(t *testing.T) {
	c, s := newEconomicsPair()
	st := orchestrator.SystemState{
		Timestamp: offPeakTime(),
		Solar:     []orchestrator.SolarState{{Name: "pv", PowerW: 4000, MaxW: 5000, Connected: true, Energized: true}},
		Batteries: []orchestrator.BatteryState{{Name: "bat", PowerW: 0, SOC: 50, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true}},
		Grid:      orchestrator.GridState{NetW: -3000, ImportLimitW: math.NaN(), ExportLimitW: math.NaN(), MaxLimitW: math.NaN()},
	}
	demands, _ := c.Evaluate(benchInput(st), s)
	// surplus = -netW - evseW = 3000; absorb = min(headroom 5000, 3000) = 3000 → -3000.
	if got := battDemand(demands, "bat"); math.Abs(got-(-3000)) > 1 {
		t.Fatalf("self-consumption battery setpoint = %.0f, want -3000", got)
	}
}

func TestEconomics_SelfConsumptionSkipsFullBattery(t *testing.T) {
	c, s := newEconomicsPair()
	st := orchestrator.SystemState{
		Timestamp: offPeakTime(),
		Solar:     []orchestrator.SolarState{{Name: "pv", PowerW: 4000, MaxW: 5000, Connected: true, Energized: true}},
		Batteries: []orchestrator.BatteryState{{Name: "bat", PowerW: 0, SOC: 96, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true}},
		Grid:      orchestrator.GridState{NetW: -3000, ImportLimitW: math.NaN(), ExportLimitW: math.NaN(), MaxLimitW: math.NaN()},
	}
	demands, _ := c.Evaluate(benchInput(st), s)
	if got := battDemand(demands, "bat"); !math.IsNaN(got) {
		t.Fatalf("full battery got charge proposal %.0f, want none", got)
	}
}

// ── Rule 5: TOU peak discharge ──────────────────────────────────────────────────

func TestEconomics_TOUPeakDischarges(t *testing.T) {
	c, s := newEconomicsPair()
	st := orchestrator.SystemState{
		Timestamp: peakTime(),
		Batteries: []orchestrator.BatteryState{{Name: "bat", PowerW: 0, SOC: 80, MaxChargeW: 5000, MaxDischargeW: 4000, Connected: true, Energized: true}},
		Grid:      orchestrator.GridState{NetW: 0, ImportLimitW: math.NaN(), ExportLimitW: math.NaN(), MaxLimitW: math.NaN()},
	}
	demands, _ := c.Evaluate(benchInput(st), s)
	// Peak + no export cap → discharge at MaxDischargeW.
	got := battDemand(demands, "bat")
	if math.IsNaN(got) {
		t.Fatal("TOU peak emitted no discharge proposal")
	}
	if math.Abs(got-4000) > 1 {
		t.Fatalf("TOU peak discharge = %.0f, want 4000 (MaxDischargeW)", got)
	}
}

func TestEconomics_TOUOffPeakNoDischarge(t *testing.T) {
	c, s := newEconomicsPair()
	st := orchestrator.SystemState{
		Timestamp: offPeakTime(),
		Batteries: []orchestrator.BatteryState{{Name: "bat", PowerW: 0, SOC: 80, MaxChargeW: 5000, MaxDischargeW: 4000, Connected: true, Energized: true}},
		Grid:      orchestrator.GridState{NetW: 0, ImportLimitW: math.NaN(), ExportLimitW: math.NaN(), MaxLimitW: math.NaN()},
	}
	demands, _ := c.Evaluate(benchInput(st), s)
	if got := battDemand(demands, "bat"); !math.IsNaN(got) {
		t.Fatalf("off-peak got discharge %.0f, want none", got)
	}
}

// TOU discharge is capped by the export headroom INSIDE economics (the legacy
// dischargeCapW leg), so the proposal itself never overshoots an active export cap
// even before the arbiter clamps it.
func TestEconomics_TOUDischargeCappedByExportHeadroom(t *testing.T) {
	c, s := newEconomicsPair()
	st := orchestrator.SystemState{
		Timestamp: peakTime(),
		Batteries: []orchestrator.BatteryState{{Name: "bat", PowerW: 0, SOC: 80, MaxChargeW: 5000, MaxDischargeW: 4000, Connected: true, Energized: true}},
		// Export cap 2000, currently exporting 0 → headroom = 2000*0.8 - 0 = 1600.
		Grid: orchestrator.GridState{NetW: 0, ExportLimitW: 2000, ImportLimitW: math.NaN(), MaxLimitW: math.NaN()},
	}
	demands, _ := c.Evaluate(benchInput(st), s)
	got := battDemand(demands, "bat")
	if math.IsNaN(got) {
		t.Fatal("TOU peak under export cap emitted no discharge proposal")
	}
	if math.Abs(got-1600) > 1 {
		t.Fatalf("TOU discharge under export cap = %.0f, want 1600 (headroom)", got)
	}
}

// ── Rule 2.5: plan following ────────────────────────────────────────────────────

func TestEconomics_PlanFollowingSetsBatteryAndEVAndSuppressesReactive(t *testing.T) {
	c, s := newEconomicsPair()
	st := orchestrator.SystemState{
		Timestamp:       peakTime(), // would TOU-discharge if the plan did not suppress it
		Solar:           []orchestrator.SolarState{{Name: "pv", PowerW: 4000, MaxW: 5000, Connected: true, Energized: true}},
		Batteries:       []orchestrator.BatteryState{{Name: "bat", PowerW: 0, SOC: 60, MaxChargeW: 4000, MaxDischargeW: 4000, Connected: true, Energized: true}},
		EVSEs:           []orchestrator.EVSEState{{StationID: "cs1", ConnectorID: 1, Connected: true, SessionActive: true, MaxCurrentA: 32, VoltageV: 230}},
		Grid:            orchestrator.GridState{NetW: -3000, ImportLimitW: math.NaN(), ExportLimitW: math.NaN(), MaxLimitW: math.NaN()},
		DailyPlanTarget: &orchestrator.PlanTarget{BattSetpointW: -1000, EVMaxCurrentA: 16},
	}
	demands, _ := c.Evaluate(benchInput(st), s)
	// Single battery: full plan setpoint -1000 (within [-4000,4000], SOC 60 ok).
	if got := battDemand(demands, "bat"); math.Abs(got-(-1000)) > 1 {
		t.Fatalf("plan battery = %.0f, want -1000 (not the TOU/self-use value)", got)
	}
	if got := evDemand(demands, "cs1", 1); math.Abs(got-16) > 0.01 {
		t.Fatalf("plan EV current = %.1f, want 16", got)
	}
	// Exactly ONE battery proposal — reactive rules must be suppressed under a plan.
	n := 0
	for _, d := range demands {
		if d.Axis == AxisBatterySetpointW {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("battery proposals = %d, want 1 (plan suppresses self-use/TOU)", n)
	}
}

// ── Rule 2: fixed dispatch ──────────────────────────────────────────────────────

func TestEconomics_FixedDispatchDischargesAndSuppressesPlan(t *testing.T) {
	c, s := newEconomicsPair()
	fixed := &model.ActivePower{Value: 3000, Multiplier: 0}
	st := orchestrator.SystemState{
		Timestamp:       peakTime(),
		Solar:           []orchestrator.SolarState{{Name: "pv", PowerW: 1000, MaxW: 5000, Connected: true, Energized: true}},
		Batteries:       []orchestrator.BatteryState{{Name: "bat", PowerW: 0, SOC: 80, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true}},
		Grid:            orchestrator.GridState{NetW: 1000, ImportLimitW: math.NaN(), ExportLimitW: math.NaN(), MaxLimitW: math.NaN()},
		DailyPlanTarget: &orchestrator.PlanTarget{BattSetpointW: -2000, EVMaxCurrentA: 10}, // must be ignored under OpModFixedW
		CSIPControl:     &orchestrator.CSIPControlState{Source: "event", MRID: "fx", Base: model.DERControlBase{OpModFixedW: fixed}},
	}
	// homeLoad = solar+batt+grid = 1000+0+1000 = 2000; available = solar-home = max(0,1000-2000)=0.
	// shortfall = 3000; battery dispatches min(availDischarge 5000, 3000) → setpoint 0+3000 = 3000.
	demands, _ := c.Evaluate(benchInput(st), s)
	if got := battDemand(demands, "bat"); math.Abs(got-3000) > 1 {
		t.Fatalf("fixed-dispatch battery = %.0f, want 3000 (not the plan -2000)", got)
	}
}

// ── Rule 6: EV charging ─────────────────────────────────────────────────────────

func TestEconomics_EVFullRateNoConstraint(t *testing.T) {
	c, s := newEconomicsPair()
	st := orchestrator.SystemState{
		Timestamp: offPeakTime(),
		EVSEs:     []orchestrator.EVSEState{{StationID: "cs1", ConnectorID: 1, Connected: true, SessionActive: true, MaxCurrentA: 32, VoltageV: 230}},
		Grid:      orchestrator.GridState{NetW: math.NaN(), ImportLimitW: math.NaN(), ExportLimitW: math.NaN(), MaxLimitW: math.NaN()},
	}
	// No solar, no grid meter, no constraint → full rate 32 A.
	demands, _ := c.Evaluate(benchInput(st), s)
	if got := evDemand(demands, "cs1", 1); math.Abs(got-32) > 0.01 {
		t.Fatalf("EV current = %.1f, want 32 (full rate)", got)
	}
}

// battery-empty-import-cap (HARD preserve): EV stays suspended during the import
// cooldown after an import cap arrives while the site is NOT compliant.
func TestEconomics_EVSuppressedDuringImportCooldown(t *testing.T) {
	c, s := newEconomicsPair()
	// Import cap arrives while over the limit (netW 3000 > 1000) → cooldown starts at 0.
	st := orchestrator.SystemState{
		Timestamp: offPeakTime(),
		EVSEs:     []orchestrator.EVSEState{{StationID: "cs1", ConnectorID: 1, Connected: true, SessionActive: true, MaxCurrentA: 32, VoltageV: 230}},
		Grid:      orchestrator.GridState{NetW: 3000, ImportLimitW: 1000, ExportLimitW: math.NaN(), MaxLimitW: math.NaN()},
	}
	demands, _ := c.Evaluate(benchInput(st), s)
	if got := evDemand(demands, "cs1", 1); math.Abs(got) > 0.01 {
		t.Fatalf("EV during import cooldown = %.1f, want 0 (suspended)", got)
	}
}

// ── Rule 1: cease-to-energize ────────────────────────────────────────────────────

func TestEconomics_DisconnectEmitsNoProposals(t *testing.T) {
	c, s := newEconomicsPair()
	no := false
	st := orchestrator.SystemState{
		Timestamp:   peakTime(),
		Solar:       []orchestrator.SolarState{{Name: "pv", PowerW: 4000, MaxW: 5000, Connected: true, Energized: true}},
		Batteries:   []orchestrator.BatteryState{{Name: "bat", PowerW: 0, SOC: 80, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true}},
		Grid:        orchestrator.GridState{NetW: -3000, ImportLimitW: math.NaN(), ExportLimitW: math.NaN(), MaxLimitW: math.NaN()},
		CSIPControl: &orchestrator.CSIPControlState{Source: "event", MRID: "off", Base: model.DERControlBase{OpModConnect: &no}},
	}
	demands, _ := c.Evaluate(benchInput(st), s)
	if len(demands) != 0 {
		t.Fatalf("economics proposed %d demands under cease-to-energize, want 0", len(demands))
	}
}

// ── Golden shadow parity (off-cap) ──────────────────────────────────────────────

// benchLegacy builds a DefaultOptimizer with the same config cmd/hub gives it.
func benchLegacy() *orchestrator.DefaultOptimizer {
	o := orchestrator.NewDefaultOptimizer()
	o.CostModel = orchestrator.DefaultTOUCostModel()
	return o
}

// benchFullStack builds the full-controller candidate: safety + compliance +
// economics, in the order main.go wires them, at the tuned tick.
func benchFullStack(st orchestrator.SystemState) *Stack {
	in := benchInput(st)
	return NewStack(in.Plant, 0,
		NewBatterySafetyConstraint(benchSOCReserve),
		NewExportConstraint(), NewGenLimitConstraint(), NewImportLimitConstraint(),
		NewEconomicsConstraint(orchestrator.DefaultTOUCostModel(),
			benchSOCReserve, benchSOCFull, benchExcessSolar, benchExportMargin, benchEVCooldown))
}

// TestEconomics_ShadowParityOffCap drives the full stack and the legacy optimizer
// through the SAME multi-tick sequence with no active CSIP cap and asserts the
// shadow wrapper records zero divergences. This is the golden-sequence equivalence
// (task acceptance criterion) for the regime where a below-compliance economics
// layer is faithful to the cascade — self-consumption, TOU peak, and EV allocation.
func TestEconomics_ShadowParityOffCap(t *testing.T) {
	seq := []orchestrator.SystemState{
		// t0: off-peak solar surplus → self-consumption charge.
		{
			Timestamp: offPeakTime(),
			Solar:     []orchestrator.SolarState{{Name: "pv", PowerW: 4000, MaxW: 5000, Connected: true, Energized: true}},
			Batteries: []orchestrator.BatteryState{{Name: "bat", PowerW: 0, SOC: 50, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true}},
			Grid:      orchestrator.GridState{NetW: -3000, ImportLimitW: math.NaN(), ExportLimitW: math.NaN(), MaxLimitW: math.NaN()},
		},
		// t1: same, battery now absorbing (maintain leg).
		{
			Timestamp: offPeakTime().Add(3 * time.Second),
			Solar:     []orchestrator.SolarState{{Name: "pv", PowerW: 4000, MaxW: 5000, Connected: true, Energized: true}},
			Batteries: []orchestrator.BatteryState{{Name: "bat", PowerW: -3000, SOC: 52, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true}},
			Grid:      orchestrator.GridState{NetW: 0, ImportLimitW: math.NaN(), ExportLimitW: math.NaN(), MaxLimitW: math.NaN()},
		},
		// t2: peak, no cap → TOU discharge at MaxDischargeW.
		{
			Timestamp: peakTime(),
			Batteries: []orchestrator.BatteryState{{Name: "bat", PowerW: 0, SOC: 80, MaxChargeW: 5000, MaxDischargeW: 4000, Connected: true, Energized: true}},
			Grid:      orchestrator.GridState{NetW: 500, ImportLimitW: math.NaN(), ExportLimitW: math.NaN(), MaxLimitW: math.NaN()},
		},
		// t3: EV charging, no grid meter, no constraint → full rate.
		{
			Timestamp: offPeakTime(),
			EVSEs:     []orchestrator.EVSEState{{StationID: "cs1", ConnectorID: 1, Connected: true, SessionActive: true, MaxCurrentA: 32, VoltageV: 230, PowerW: 0}},
			Grid:      orchestrator.GridState{NetW: math.NaN(), ImportLimitW: math.NaN(), ExportLimitW: math.NaN(), MaxLimitW: math.NaN()},
		},
	}

	legacy := benchLegacy()
	stack := benchFullStack(seq[0])
	var diverged []Divergence
	w := Wrap(legacy, stack, Options{
		Now:       func() time.Time { return time.Unix(0, 0) },
		OnDiverge: func(d Divergence) { diverged = append(diverged, d) },
	})
	for i, st := range seq {
		w.Optimize(st)
		if n := w.Divergences(); n != 0 {
			t.Fatalf("tick %d: %d cumulative divergence(s); first axes=%+v", i, n, diverged[0].Axes)
		}
	}
}
