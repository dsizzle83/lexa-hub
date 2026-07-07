package orchestrator

// Plant model — per-device physical-response parameters (TASK-057, AD-007).
//
// The optimizer today hard-codes this bench's physics: how fast THIS inverter
// may be walked, THIS pack's energy and SOC taper, THIS bench's meter/OCPP
// reporting lags (review W1/D6, §8.1). Those constants "won't transfer to real
// vendors" — a Sunny Boy + Powerwall + arbitrary CT meter has different ramps
// and lags. This file is the vocabulary Phase 5 uses to replace those globals
// with named, unit-suffixed, provenance-documented per-device parameters.
//
// NOTHING reads these types yet. They are additive config vocabulary; the
// wiring (the constraint controller consuming a PlantModel instead of the
// globals, and the constant burn-down) is TASK-064, gated on the
// export-constraint migration and its identical-behavior proof. Until then the
// constants in optimizer.go remain the single source of truth and Optimize()
// output is bit-identical.
//
// Conventions (05 §5, §6):
//   - Every field carries its unit in the name (W, WPerS, S, Pct, KWh).
//   - Slews are denominated per WALL-CLOCK SECOND, never per tick. Ticks are an
//     engine detail (FAST 3 s vs STOCK 15 s, see tunedTickInterval); the schema
//     is cadence-independent and scaled to ticks only at the consuming edge.
//   - Each field's doc names the optimizer.go constant it will replace and its
//     provenance ("bench-calibrated").
//   - A zero/absent field means "use the bench default"; WithDefaults() fills
//     it. The defaults reproduce today's constants EXACTLY (per-second defaults
//     multiplied back by tunedTickInterval recover the legacy per-tick values).
//
// Note on socStepEstimate (optimizer.go:787, 1.0 %/tick): physically it is
// DERIVABLE — the SOC a pack climbs per tick at full charge, i.e. MaxChargeW
// (from live BatteryMetrics) × tickSeconds ÷ (CapacityKWh × 36 000), which for the
// bench pack (5 kW, 10 kWh, 3 s) yields ≈0.42 %/tick, NOT 1.0. The legacy 1.0 is a
// DELIBERATE conservative overestimate ("Calibrated for the 20× demo"): it errs
// HIGH so the SOC taper hands off EARLY, never late. TASK-064's mandate is
// identical bench behaviour, so it does NOT switch to the derived value — that is a
// behaviour change. Instead BatteryPlant carries an explicit
// SOCStepPctPerTickOverride defaulting to the legacy 1.0 (preserve-first, marked
// legacy debt per 05 §6); the derived-formula migration is backlogged (10_BACKLOG,
// "derived socStep + discovery probe"). The override is a per-pack knob so a real
// pack can be calibrated without editing product code.

// Bench-calibration provenance constants. These name the legacy values so the
// per-second/per-percent defaults below (and the equivalence test) have a
// single, documented source. The per-TICK slews come from applyExportLimitRule
// (optimizer.go maxDropW/maxRiseW); the schema stores per-second, so the
// defaults divide by tunedTickInterval.
const (
	// benchCeilingDropWPerTick is optimizer.go's maxDropW (1500 W/tick): how
	// fast the solar ceiling may be TIGHTENED per tick. Defend the cap quickly.
	benchCeilingDropWPerTick = 1500.0
	// benchCeilingRiseWPerTick is optimizer.go's maxRiseW (500 W/tick): how
	// fast the ceiling may be RELAXED per tick. Give generation back slowly.
	benchCeilingRiseWPerTick = 500.0

	// benchBatteryCapacityKWh is the bench pack size named in the
	// socStepEstimate comment (optimizer.go:782, "10 kWh / 5 kW pack").
	benchBatteryCapacityKWh = 10.0

	// benchSOCTaperStartPct mirrors socTaperStart (optimizer.go:778): the SOC
	// at which the battery's charge power begins its linear taper.
	benchSOCTaperStartPct = 80.0

	// benchMeterLagS / benchEVSEMeterLagS are the reporting cadences the
	// filterAlpha=0.4 low-pass encodes (optimizer.go:692-696, "5 s vs 10 s"):
	// the physical meter refreshes ~every 5 s, OCPP MeterValues ~every 10 s.
	benchMeterLagS     = 5.0
	benchEVSEMeterLagS = 10.0

	// benchFilterAlpha is optimizer.go's filterAlpha (:696, 0.4): the EMA low-pass
	// coefficient on the measured site-export reading that rejects the meter/OCPP
	// reporting jitter. Tuned for this bench's 5 s meter / 10 s OCPP cadence; the
	// FilterAlphaFor mapping derives ≈0.375 from those lags, close but NOT identical,
	// so the bench keeps this explicit tuned value (preserve-first, TASK-064).
	benchFilterAlpha = 0.4

	// benchSOCStepPctPerTick is optimizer.go's socStepEstimate (:787, 1.0 %/tick):
	// the SOC the export controller ASSUMES the pack climbs per tick when it
	// pre-positions the taper one tick ahead. A deliberate conservative overestimate
	// (see the socStepEstimate note above). Bench-calibrated legacy debt.
	benchSOCStepPctPerTick = 1.0
)

// benchControlLatencyS is the bench command→measured-effect lag. It equals one
// tuned tick (the cadence the *BreachTicks debounces were calibrated at); a
// command issued this tick is expected to show up in the meter by the next.
// A function (not a const) because tunedTickInterval is a Duration.
func benchControlLatencyS() float64 { return tunedTickInterval.Seconds() }

// InverterPlant is the smart-inverter / PV-string plant model: how fast the
// hub may walk this inverter's export ceiling, and its command latency.
type InverterPlant struct {
	// MaxRampDownWPerS bounds how fast the solar export ceiling may be
	// TIGHTENED (W per wall-clock second). Replaces optimizer.go's per-tick
	// maxDropW (1500 W/tick @ tunedTickInterval). Bench-calibrated.
	// Default: 1500 / 3 s = 500 W/s.
	MaxRampDownWPerS float64 `json:"max_ramp_down_w_per_s"`
	// MaxRampUpWPerS bounds how fast the ceiling may be RELAXED (W per second).
	// Replaces optimizer.go's per-tick maxRiseW (500 W/tick). Bench-calibrated.
	// Default: 500 / 3 s ≈ 166.7 W/s.
	MaxRampUpWPerS float64 `json:"max_ramp_up_w_per_s"`
	// ControlLatencyS is the command→measured-effect lag (seconds): how long
	// after a setpoint change the meter reflects it. Bench-calibrated to one
	// tuned tick (3 s). TASK-064 use case: a constraint controller sizes the
	// export-breach detection window from this + MeterPlant.MeterLagS instead
	// of the fixed exportBreachTicks=3 (~9 s) — the fixed window races the
	// ~11 s oracle boundary on battery-charge-disabled (live finding
	// 2026-07-03), so an adaptive window derived from real plant latency is
	// exactly what this field exists to feed.
	ControlLatencyS float64 `json:"control_latency_s"`
}

// withDefaults returns a copy with any zero/absent field filled with the bench
// calibration, so the result reproduces today's optimizer.go constants exactly.
func (p InverterPlant) WithDefaults() InverterPlant {
	if p.MaxRampDownWPerS == 0 {
		p.MaxRampDownWPerS = benchCeilingDropWPerTick / tunedTickInterval.Seconds()
	}
	if p.MaxRampUpWPerS == 0 {
		p.MaxRampUpWPerS = benchCeilingRiseWPerTick / tunedTickInterval.Seconds()
	}
	if p.ControlLatencyS == 0 {
		p.ControlLatencyS = benchControlLatencyS()
	}
	return p
}

// TaperPoint is one point of a piecewise-linear SOC→charge-fraction taper.
// Frac is the fraction of MaxChargeW allowed at SOCPct (1.0 = full power,
// 0.0 = charging stopped).
type TaperPoint struct {
	SOCPct float64 `json:"soc_pct"`
	Frac   float64 `json:"frac"`
}

// BatteryPlant is the storage plant model: pack energy, SOC taper shape, and
// the closed-loop absorption convergence expected from a healthy charge ramp.
type BatteryPlant struct {
	// CapacityKWh is the pack's usable energy. Bench-calibrated to the 10 kWh
	// bench pack (optimizer.go:782). TASK-064 derives socStepEstimate from
	// this (MaxChargeW × tick ÷ CapacityKWh) instead of the hard-coded
	// 1.0 %/tick, so the SOC-taper handoff tracks the real pack.
	CapacityKWh float64 `json:"capacity_kwh"`
	// SOCTaperStartPct is the SOC at which charge power begins tapering toward
	// zero at SOCFullThreshold. Replaces optimizer.go's socTaperStart (80.0).
	// Bench-calibrated. Default 80.
	SOCTaperStartPct float64 `json:"soc_taper_start_pct"`
	// TaperCurve is an optional piecewise-linear override of the taper shape
	// (must be sorted ascending by SOCPct if set). Empty/nil = today's
	// behavior: a straight linear taper from SOCTaperStartPct (Frac 1.0) to
	// the optimizer's SOCFullThreshold (Frac 0.0). Bench uses the linear
	// default; the curve exists for vendor packs with a non-linear CV knee.
	TaperCurve []TaperPoint `json:"taper_curve,omitempty"`
	// ConvergeFrac is the measured/commanded absorption floor: measured
	// absorption must reach at least this fraction of the commanded charge
	// within the detection window, else the phantom absorption is dropped and
	// the inverter is curtailed. Replaces optimizer.go's battConvergeFrac
	// (0.5). Bench-calibrated. Default 0.5.
	ConvergeFrac float64 `json:"converge_frac"`
	// SOCStepPctPerTickOverride is the SOC (%) the export controller ASSUMES the
	// pack climbs in one engine tick when it pre-positions the SOC taper one tick
	// ahead (optimizer.go:787, socStepEstimate). It is a DELIBERATE conservative
	// overestimate — the derived value is MaxChargeW×tickS/(CapacityKWh×36000)
	// (≈0.42 %/tick for the bench pack) but the legacy 1.0 errs HIGH so the taper
	// hands off early, never late. TASK-064 keeps the 1.0 override to preserve bench
	// behaviour EXACTLY; the derived formula is backlogged (10_BACKLOG). This is an
	// explicit per-pack legacy-debt knob (05 §6): burn down after real-pack
	// calibration replaces it with the derived value. Default 1.0.
	SOCStepPctPerTickOverride float64 `json:"soc_step_pct_per_tick_override"`
	// ControlLatencyS is the command→measured-effect lag (seconds) for a
	// battery charge setpoint. Bench-calibrated to one tuned tick (3 s). Feeds
	// the same adaptive detection window as InverterPlant.ControlLatencyS
	// (TASK-064) — battBreachTicks=3 is the battery-side twin of the
	// export-breach window that races the oracle boundary.
	ControlLatencyS float64 `json:"control_latency_s"`
}

func (p BatteryPlant) WithDefaults() BatteryPlant {
	if p.CapacityKWh == 0 {
		p.CapacityKWh = benchBatteryCapacityKWh
	}
	if p.SOCTaperStartPct == 0 {
		p.SOCTaperStartPct = benchSOCTaperStartPct
	}
	if p.ConvergeFrac == 0 {
		p.ConvergeFrac = battConvergeFrac
	}
	if p.ControlLatencyS == 0 {
		p.ControlLatencyS = benchControlLatencyS()
	}
	if p.SOCStepPctPerTickOverride == 0 {
		p.SOCStepPctPerTickOverride = benchSOCStepPctPerTick
	}
	// TaperCurve stays nil by default (empty = linear taper); never synthesized.
	return p
}

// MeterPlant is the revenue/CT meter plant model: how stale its export reading
// can be. filterAlpha (optimizer.go:696, the low-pass on measured export) will
// DERIVE from MeterLagS in TASK-064 rather than being hard-coded — a slower
// meter needs a heavier filter.
type MeterPlant struct {
	// MeterLagS is the meter's export-reading refresh cadence (seconds).
	// Bench-calibrated to the ~5 s bench meter (optimizer.go:693). Default 5.
	MeterLagS float64 `json:"meter_lag_s"`
	// FilterAlpha is the EMA low-pass coefficient the export controller applies to
	// the measured site-export reading (optimizer.go:696, filterAlpha=0.4) to reject
	// the meter/OCPP reporting jitter. It is DERIVABLE from MeterLagS and the tick
	// (see FilterAlphaFor: a slower meter needs a heavier filter — smaller alpha),
	// but TASK-064 keeps an explicit tuned override to preserve bench behaviour
	// EXACTLY (the derived value ≈0.375 is close but not identical to the tuned 0.4).
	// The override WINS when set; the derivation is the documented fallback for a
	// vendor meter that ships only a datasheet refresh cadence. Bench-calibrated.
	// Default 0.4.
	FilterAlpha float64 `json:"filter_alpha"`
}

func (p MeterPlant) WithDefaults() MeterPlant {
	if p.MeterLagS == 0 {
		p.MeterLagS = benchMeterLagS
	}
	if p.FilterAlpha == 0 {
		p.FilterAlpha = benchFilterAlpha
	}
	return p
}

// FilterAlphaFor derives an EMA low-pass coefficient from a meter's reporting lag
// and the engine tick, for a vendor meter that ships only a datasheet refresh
// cadence and no tuned alpha. It is the exponential-smoothing form
// alpha = tickS / (meterLagS + tickS): a slower meter (larger lag) yields a smaller
// alpha (heavier filter). At the bench (meterLag 5 s, tick 3 s) it yields 0.375 —
// close to, but NOT identical to, the tuned 0.4 the bench keeps as an explicit
// override (preserve-first, TASK-064; provenance benchFilterAlpha). Result is
// clamped to (0,1]; a zero/negative lag or tick yields 1.0 (no filtering).
func FilterAlphaFor(meterLagS, tickS float64) float64 {
	if meterLagS <= 0 || tickS <= 0 {
		return 1.0
	}
	a := tickS / (meterLagS + tickS)
	if a > 1 {
		a = 1
	}
	return a
}

// EVSEPlant is the EV-charger plant model. Its meter lag is the OCPP
// MeterValues cadence, which is coarser than the site meter's — the same
// desync filterAlpha exists to absorb (optimizer.go:692-695).
type EVSEPlant struct {
	// MeterLagS is the OCPP MeterValues reporting cadence (seconds).
	// Bench-calibrated to ~10 s (optimizer.go:693). Default 10.
	MeterLagS float64 `json:"meter_lag_s"`
}

func (p EVSEPlant) WithDefaults() EVSEPlant {
	if p.MeterLagS == 0 {
		p.MeterLagS = benchEVSEMeterLagS
	}
	return p
}
