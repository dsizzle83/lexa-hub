package constraint

import (
	"math"
	"testing"
	"time"

	"lexa-hub/internal/orchestrator"
	model "lexa-proto/csipmodel"
)

// ── R4 STOCK-cadence flip investigation repro harness ────────────────────────
//
// TASK-060/061/FIX-F pinned the R4 flip's STOCK safety regression to three
// mayhem scenarios (malform-huge-activepower, wan-outage-hold,
// northbound-hang) that FAIL when all five constraints run active/composed
// at STOCK (15 s engine tick) but PASS at FAST. A control experiment then
// showed malform-huge-activepower and wan-outage-hold ALSO fail on the
// legacy cascade alone at STOCK (not flip-specific); only northbound-hang
// PASSES on legacy-alone and FAILS composed.
//
// This harness closed-loop-simulates the bench "armExportCap" shape (PV
// nameplate 5000 W, load 250 W, battery full/SOC=100 so ONLY solar
// curtailment can hold a 0 W export cap) at the real STOCK tick (15 s),
// self-consistently feeding solar's measured PowerW from whatever ceiling
// the PLAN UNDER TEST actually commanded the PRIOR tick, and drives the
// SAME fault sequence through legacy-alone and composed-active (single
// export constraint, and all five constraints matching the real bug
// configuration) to compare their behaviour directly.
//
// Result across every fault shape tried (frozen/unchanged control, control
// read absent (nil), an absurd ActivePower cap, and a per-tick-jittering cap
// value forcing a controller-session reset every tick) — legacy and
// composed produce IDENTICAL peak export and identical recovery. No
// composition-specific divergence was reproduced: export.go/session.go's
// port and shadow.go's compose()/ownsActive() are faithful. See the
// investigation writeup (task/stock-hold-fix) for the conclusion this
// supports: the northbound-hang-specific finding most likely traces to
// upstream state-freshness/fail-closed handling shared with the other two
// (non-flip-specific) scenarios, not to a defect in this package.

type stockWorld struct {
	solarPowerW float64 // measured output, tracks the last commanded ceiling
	nameplateW  float64
	loadW       float64
}

func newStockWorld() *stockWorld {
	return &stockWorld{solarPowerW: 5000, nameplateW: 5000, loadW: 250}
}

// state reports the SystemState for this tick given the world's current
// (already-settled) solar output and the CSIP control under test.
func (w *stockWorld) state(csip *orchestrator.CSIPControlState, ts time.Time) orchestrator.SystemState {
	out := math.Min(w.nameplateW, w.solarPowerW)
	netW := w.loadW - out // + import, - export
	return orchestrator.SystemState{
		Timestamp:   ts,
		Solar:       []orchestrator.SolarState{{Name: "pv", PowerW: math.Max(0, out), MaxW: w.nameplateW, Connected: true, Energized: true}},
		Batteries:   []orchestrator.BatteryState{{Name: "bat", PowerW: 0, SOC: 100, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true}},
		Grid:        orchestrator.GridState{NetW: netW, ImportLimitW: math.NaN(), ExportLimitW: math.NaN(), MaxLimitW: math.NaN()},
		CSIPControl: csip,
	}
}

// apply moves the world forward per the ceiling a plan commanded for "pv"
// this tick (no SolarCommand for pv, or a NaN ceiling, ⇒ uncurtailed ⇒ full
// nameplate — matching emitCommands'/legacy's "restore" convention).
func (w *stockWorld) apply(plan orchestrator.Plan) {
	ceil := math.NaN()
	for _, c := range plan.SolarCommands {
		if c.Name == "pv" {
			ceil = c.CurtailToW
		}
	}
	if math.IsNaN(ceil) {
		w.solarPowerW = w.nameplateW
	} else {
		w.solarPowerW = math.Min(w.nameplateW, math.Max(0, ceil))
	}
}

// exportW returns the world's current measured export magnitude (W, 0 if
// importing or balanced).
func (w *stockWorld) exportW() float64 {
	out := math.Min(w.nameplateW, w.solarPowerW)
	return math.Max(0, out-w.loadW)
}

func hugeExpLimControl() *orchestrator.CSIPControlState {
	return &orchestrator.CSIPControlState{
		Source: "event", MRID: "exp-mrid",
		Base: model.DERControlBase{OpModExpLimW: &model.ActivePower{Value: 32767, Multiplier: 9}},
	}
}

func stockPlant() Plant {
	return Plant{
		Inverters: map[string]orchestrator.InverterPlant{"pv": orchestrator.InverterPlant{}.WithDefaults()},
		Batteries: map[string]orchestrator.BatteryPlant{"bat": orchestrator.BatteryPlant{}.WithDefaults()},
		EVSEs:     map[string]orchestrator.EVSEPlant{},
		Meter:     orchestrator.MeterPlant{}.WithDefaults(),
	}
}

func stockLegacy() *orchestrator.DefaultOptimizer {
	o := orchestrator.NewDefaultOptimizer()
	o.CostModel = orchestrator.DefaultTOUCostModel()
	o.SetTickInterval(15 * time.Second)
	return o
}

// fullStockStack wires all five constraints main.go wires, in its order, at
// the given STOCK tick interval — matching the actual "R4 all five active"
// bug configuration (battery-safety, export, gen, import, economics).
func fullStockStack(tickInterval time.Duration) *Stack {
	cd := NewEVImportCooldown()
	return NewStack(stockPlant(), tickInterval,
		NewBatterySafetyConstraint(benchSOCReserve),
		NewExportConstraint(),
		NewGenLimitConstraint(),
		NewImportLimitConstraint(cd),
		NewEconomicsConstraint(orchestrator.DefaultTOUCostModel(), benchSOCReserve, 95.0, 100.0, 0.20, 20, cd),
	)
}

var fullActive = map[string]bool{"battery-safety": true, "export": true, "gen": true, "import": true, "economics": true}

// runStockScenario drives `optimize` for settleTicks (cap active+settling),
// then faultTicks under `faultCSIP`, then recoverTicks back on the good 0W
// cap, self-consistently at the STOCK 15 s tick. Returns the peak measured
// export observed during fault+recovery, and whether the site was back
// within the cap (export <= complianceMarginW) by the end of the run.
func runStockScenario(optimize func(orchestrator.SystemState) orchestrator.Plan,
	settleTicks, faultTicks, recoverTicks int, faultCSIP *orchestrator.CSIPControlState) (peakExportW float64, reseated bool) {
	w := newStockWorld()
	goodCSIP := expLimControl(0)
	ts := time.Unix(1000, 0)
	tick := 15 * time.Second

	step := func(csip *orchestrator.CSIPControlState) float64 {
		st := w.state(csip, ts)
		plan := optimize(st)
		w.apply(plan)
		ts = ts.Add(tick)
		return w.exportW()
	}

	for i := 0; i < settleTicks; i++ {
		step(goodCSIP)
	}
	peak := 0.0
	for i := 0; i < faultTicks; i++ {
		if e := step(faultCSIP); e > peak {
			peak = e
		}
	}
	for i := 0; i < recoverTicks; i++ {
		if e := step(goodCSIP); e > peak {
			peak = e
		}
	}
	return peak, w.exportW() <= complianceMarginW
}

// TestStockRepro_NoRefresh_FrozenControl_ComposedHoldsCap: the "the retained
// control simply never changes" case (northbound stops publishing but the
// last-good 0W cap stays on the bus, unexpired — the intended fail-closed
// behaviour for both wan-outage-hold and northbound-hang). No discontinuity
// ever reaches the constraint layer, so composed must never breach.
func TestStockRepro_NoRefresh_FrozenControl_ComposedHoldsCap(t *testing.T) {
	stack := NewStack(stockPlant(), 15*time.Second, NewExportConstraint())
	wrap := Wrap(stockLegacy(), stack, Options{ActiveConstraints: map[string]bool{"export": true}})

	peak, reseated := runStockScenario(wrap.Optimize, 8, 6, 4, expLimControl(0))
	t.Logf("frozen-control composed: peakExport=%.0fW reseated=%v", peak, reseated)
	if peak > complianceMarginW {
		t.Errorf("composed peak export = %.0fW under a FROZEN (unchanged) control — no discontinuity, should never breach", peak)
	}
	if !reseated {
		t.Errorf("composed did not end within the cap under a frozen control")
	}
}

// assertLegacyComposedMatch runs the same fault sequence through legacy-alone
// and composed-active (candidate built by newCandidate) and fails if their
// peak export or end-of-run reseating disagree — a genuine composition bug
// would show up here as a divergence between the two columns.
func assertLegacyComposedMatch(t *testing.T, name string, newCandidate func() (*Stack, map[string]bool),
	settleTicks, faultTicks, recoverTicks int, faultCSIP *orchestrator.CSIPControlState) {
	t.Helper()

	peakLegacy, reseatedLegacy := runStockScenario(stockLegacy().Optimize, settleTicks, faultTicks, recoverTicks, faultCSIP)

	stack, active := newCandidate()
	wrap := Wrap(stockLegacy(), stack, Options{ActiveConstraints: active})
	peakComposed, reseatedComposed := runStockScenario(wrap.Optimize, settleTicks, faultTicks, recoverTicks, faultCSIP)

	t.Logf("%s: legacy peak=%.0fW reseated=%v | composed peak=%.0fW reseated=%v",
		name, peakLegacy, reseatedLegacy, peakComposed, reseatedComposed)

	if math.Abs(peakLegacy-peakComposed) > complianceMarginW {
		t.Errorf("%s: peak export diverges — legacy=%.0fW composed=%.0fW (want within %.0fW)",
			name, peakLegacy, peakComposed, complianceMarginW)
	}
	if reseatedLegacy != reseatedComposed {
		t.Errorf("%s: reseat-by-end-of-run diverges — legacy=%v composed=%v", name, reseatedLegacy, reseatedComposed)
	}
}

func exportOnlyCandidate() (*Stack, map[string]bool) {
	return NewStack(stockPlant(), 15*time.Second, NewExportConstraint()), map[string]bool{"export": true}
}

func allFiveCandidate() (*Stack, map[string]bool) {
	return fullStockStack(15 * time.Second), fullActive
}

// TestStockRepro_NilControl: discovery reads the control as ABSENT
// (CSIPControl nil) for several STOCK ticks, then it returns. Both the
// export-only candidate and the real five-constraint configuration are
// checked against legacy-alone.
func TestStockRepro_NilControl(t *testing.T) {
	assertLegacyComposedMatch(t, "export-only", exportOnlyCandidate, 8, 4, 6, nil)
	assertLegacyComposedMatch(t, "all-five", allFiveCandidate, 8, 4, 6, nil)
}

// TestStockRepro_HugeActivePower: the malform-huge-activepower fault (an
// absurd 32767e9 W ActivePower limit) mid-cap, then reverting to the real
// 0W cap.
func TestStockRepro_HugeActivePower(t *testing.T) {
	assertLegacyComposedMatch(t, "export-only", exportOnlyCandidate, 8, 4, 6, hugeExpLimControl())
	assertLegacyComposedMatch(t, "all-five", allFiveCandidate, 8, 4, 6, hugeExpLimControl())
}

// TestStockRepro_JitteringCap_AllFiveActive: the control reads back with a
// SLIGHTLY different numeric ExpLimW every tick (a garbled/partial read
// while a stall is recovering) — this forces a fresh controller-session
// reset on the candidate EVERY tick (exportLimitW != sess.ctrl.activeLimitW)
// instead of ever settling into the slew-limited steady state. Checks
// whether perpetual-reset behaves differently between legacy and composed
// under the real (all five) constraint configuration.
func TestStockRepro_JitteringCap_AllFiveActive(t *testing.T) {
	tick := 15 * time.Second
	ts0 := time.Unix(1000, 0)
	jitterCSIP := func(i int) *orchestrator.CSIPControlState {
		return expLimControl(int16(i % 3)) // 0,1,2 W — ~zero, but a NEW value every tick
	}

	run := func(optimize func(orchestrator.SystemState) orchestrator.Plan) (peak float64, world *stockWorld) {
		w := newStockWorld()
		ts := ts0
		for i := 0; i < 20; i++ {
			st := w.state(expLimControl(0), ts)
			if i >= 8 && i < 14 {
				st.CSIPControl = jitterCSIP(i)
			}
			plan := optimize(st)
			w.apply(plan)
			ts = ts.Add(tick)
			if e := w.exportW(); e > peak {
				peak = e
			}
		}
		return peak, w
	}

	peakLeg, wLeg := run(stockLegacy().Optimize)

	stack := fullStockStack(tick)
	wrap := Wrap(stockLegacy(), stack, Options{ActiveConstraints: fullActive})
	peakComp, wComp := run(wrap.Optimize)

	t.Logf("jitter legacy: peak=%.0fW final solarPowerW=%.0f", peakLeg, wLeg.solarPowerW)
	t.Logf("jitter composed: peak=%.0fW final solarPowerW=%.0f", peakComp, wComp.solarPowerW)

	if math.Abs(peakLeg-peakComp) > complianceMarginW {
		t.Errorf("jittering-cap peak export diverges — legacy=%.0fW composed=%.0fW", peakLeg, peakComp)
	}
}
