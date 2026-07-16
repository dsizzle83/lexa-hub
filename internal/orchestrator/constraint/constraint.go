package constraint

import (
	"math"

	"lexa-hub/internal/orchestrator"
)

// Tier is a constraint's priority band. Resolution runs SAFETY first, then
// COMPLIANCE, then ECONOMICS; a lower-priority tier may only narrow the
// admissible interval a higher tier has already set, never widen it. Numerically
// the tiers ascend in resolution order (TierSafety = 0 resolves first), so
// sorting demands by Tier ascending yields the priority order.
type Tier int

const (
	// TierSafety is the highest-priority band: physical/CSIP-disconnect
	// protection. Its demands win over everything. (Battery Tier-1 safety stays
	// on DefaultOptimizer until TASK-062; see interfaces.go SafetyEvaluator.)
	TierSafety Tier = iota
	// TierCompliance is the CSIP limit band: export/import/generation caps that
	// must be honoured (and reported as CannotComply when they cannot).
	TierCompliance
	// TierEconomics is the lowest band: cost-optimal dispatch, self-consumption,
	// TOU peak, EV allocation. Economics PROPOSES a working point inside the
	// interval the higher tiers leave admissible.
	TierEconomics
)

// String renders a Tier for decision/diagnostic traces.
func (t Tier) String() string {
	switch t {
	case TierSafety:
		return "safety"
	case TierCompliance:
		return "compliance"
	case TierEconomics:
		return "economics"
	default:
		return "unknown"
	}
}

// Axis identifies one actuator degree of freedom a Demand can bound.
type Axis int

const (
	// AxisSolarCeilingW bounds a solar inverter's output ceiling (W). A Demand's
	// Max is the ceiling; Min is normally unbounded (NaN). Maps to
	// SolarCommand.CurtailToW (NaN ceiling ⇒ no curtailment / restore).
	AxisSolarCeilingW Axis = iota
	// AxisBatterySetpointW bounds a battery net-power setpoint (W; + discharge,
	// − charge). Compliance/safety give an interval; economics pins a point
	// (Min==Max). Maps to BatteryCommand.SetpointW.
	AxisBatterySetpointW
	// AxisEVSECurrentA bounds an EVSE charging-current ceiling (A). Max is the
	// limit (0 ⇒ suspend). Maps to EVSECommand.MaxCurrentA.
	AxisEVSECurrentA
	// AxisConnect is the connect/disconnect axis; the interval fields are unused
	// and Demand.Connect carries the desired state. false (disconnect) is the
	// safe value and always wins. Maps to the Connect field of the command class
	// the device's value axis names — SolarCommand for a ceiling-bound device,
	// EVSECommand for a current-bound one, else BatteryCommand (unit 3.6's
	// emitCommands fan-out; before that it mapped to BatteryCommand only).
	AxisConnect
)

// axisOrder is the deterministic iteration order over axes (map iteration is
// never allowed to leak into output — shadow diffing needs reproducibility).
var axisOrder = []Axis{AxisSolarCeilingW, AxisBatterySetpointW, AxisEVSECurrentA, AxisConnect}

// axisKey builds the device+axis lookup key shared by three FIX-F consumers
// that must all agree on the exact same format: Resolve's per-tick authorship
// map (Desired.Authors, surfaced by Stack.AxisAuthors), the post-arbiter
// override attribution (stack.go attributePostArbiterAuthorship), and the
// Wrapper's divergence Author lookup (shadow.go), which builds the same key
// from an already-formatted AxisDivergence.Device/Axis pair.
func axisKey(device string, axis Axis) string {
	return device + "/" + axis.String()
}

// String renders an Axis for traces.
func (a Axis) String() string {
	switch a {
	case AxisSolarCeilingW:
		return "solar-ceiling-w"
	case AxisBatterySetpointW:
		return "battery-setpoint-w"
	case AxisEVSECurrentA:
		return "evse-current-a"
	case AxisConnect:
		return "connect"
	default:
		return "unknown-axis"
	}
}

// Demand is one constraint's admissible bound on one device axis for this tick.
//
// Demands are BOUNDS, not commands: [Min, Max] is the interval the constraint
// will accept, and NaN marks an unbounded side (a pure ceiling sets Max and
// leaves Min NaN). Arbitration intersects demands per (Device, Axis); a lower
// tier can only shrink the interval. Economics pins a working point with a
// degenerate interval (Min==Max).
type Demand struct {
	// Device is the actuator name: a BatteryState/SolarState Name, or an EVSE
	// StationID. It keys the demand to a device in SystemState.
	Device string
	// Axis is the actuator degree of freedom this demand bounds.
	Axis Axis
	// Min, Max are the admissible interval on Axis. NaN = unbounded on that side.
	Min, Max float64
	// Connect is the desired connect state, used only for AxisConnect. nil means
	// "no opinion". false (disconnect) is the safe value and wins over true.
	Connect *bool
	// Tier is the constraint's priority band.
	Tier Tier
	// Source is the emitting constraint's Name(), recorded on decisions and
	// conflicts for diagnostics.
	Source string
}

// CeilingDemand is a convenience builder for a pure upper bound (Max only) —
// the common shape for export/generation caps and EV current limits.
func CeilingDemand(device string, axis Axis, maxW float64, tier Tier, source string) Demand {
	return Demand{Device: device, Axis: axis, Min: math.NaN(), Max: maxW, Tier: tier, Source: source}
}

// PointDemand is a convenience builder for a pinned working point (Min==Max) —
// the shape an economics constraint uses to PROPOSE a setpoint.
func PointDemand(device string, axis Axis, value float64, tier Tier, source string) Demand {
	return Demand{Device: device, Axis: axis, Min: value, Max: value, Tier: tier, Source: source}
}

// ConnectDemand is a convenience builder for a connect/disconnect demand.
func ConnectDemand(device string, connect bool, tier Tier, source string) Demand {
	c := connect
	return Demand{Device: device, Axis: AxisConnect, Min: math.NaN(), Max: math.NaN(), Connect: &c, Tier: tier, Source: source}
}

// Plant bundles the per-device plant models (TASK-057) a constraint reads to
// size ramps, latencies, and detection windows. The wiring layer (cmd/hub,
// TASK-059+) fills these from hub.json with defaults already applied; a device
// absent from a map yields a zero-value plant, so constraints must treat
// zeroes defensively (a real controller reads the defaulted copy).
type Plant struct {
	Inverters map[string]orchestrator.InverterPlant
	Batteries map[string]orchestrator.BatteryPlant
	EVSEs     map[string]orchestrator.EVSEPlant
	Meter     orchestrator.MeterPlant
}

// Input is everything a Constraint sees for one tick. It is read-only; a
// constraint's ONLY mutable state is its Session.
type Input struct {
	// State is the current system snapshot (from the Engine's reader).
	State orchestrator.SystemState
	// Plant is the per-device physical-response model.
	Plant Plant
	// TickSeconds is the wall-clock length of one engine tick (injected — the
	// layer never reads a clock). Used to convert plant latencies (per-second)
	// into tick-denominated detection windows.
	TickSeconds float64

	// DischargeBlocked is the Stack's shared hysteretic reserve-floor gate for
	// THIS tick (audit B-1): the single owner of "may this pack discharge?",
	// advanced by the Stack before the constraint pass and consulted by every
	// discharge author in place of a bare SOC ≤ reserve check. Nil when a
	// constraint's Evaluate is driven directly in a unit test without a Stack —
	// reserveBlocker then falls back to the instantaneous check.
	DischargeBlocked func(orchestrator.BatteryState) bool
}

// Constraint is one narrowing (or proposing) rule in the ladder. Evaluate is
// PURE: it reads Input, updates its Session's typed inter-tick state, and
// returns the demands it wants enforced plus an optional ComplianceBreach when
// a CSIP limit it owns cannot be met this tick (worst-shortfall arbitrated by
// the Stack). No I/O, no time.Now(), no logging.
type Constraint interface {
	// Name is a stable identifier; it keys the constraint's Session and appears
	// as Demand.Source. Must be unique within a Stack.
	Name() string
	// Tier is the constraint's priority band.
	Tier() Tier
	// Evaluate returns this tick's demands and an optional breach.
	Evaluate(in Input, s *Session) ([]Demand, *orchestrator.ComplianceBreach)
}

// DetectionWindowTicks derives how many ticks a compliance constraint should
// wait for a commanded correction to appear at the meter before declaring a
// CannotComply breach. It sizes the window from real plant physics — the
// command→effect control latency plus the meter's reporting lag — instead of a
// fixed constant.
//
// This is the adaptive replacement AD-007 exists to enable. The legacy
// exportBreachTicks/battBreachTicks/genBreachTicks are all fixed at 3 (~9 s at
// the 3 s tuned tick); on battery-charge-disabled that ~9 s window races the
// ~11 s oracle boundary (M2 soak, 2026-07-03). A window derived from the actual
// InverterPlant.ControlLatencyS + MeterPlant.MeterLagS grows automatically for a
// slower plant, so the detector never fires before the meter could possibly
// have shown the correction.
//
// The window is ceil((controlLatencyS+meterLagS)/tickSeconds), floored at 2 to
// preserve the single-glitch tolerance the leaky counters rely on (matching
// scaleTicks' floor). With bench defaults (controlLatency 3 s, meterLag 5 s,
// tick 3 s) this yields 3 ticks — bit-identical to today's constant — but it
// tracks the plant instead of the bench.
func DetectionWindowTicks(controlLatencyS, meterLagS, tickSeconds float64) int {
	if tickSeconds <= 0 {
		tickSeconds = tunedTickInterval.Seconds()
	}
	windowS := math.Max(0, controlLatencyS) + math.Max(0, meterLagS)
	n := int(math.Ceil(windowS / tickSeconds))
	if n < 2 {
		n = 2
	}
	return n
}

// ExportDetectionWindowTicks sizes the export-breach detection window for one
// inverter from ITS control latency and the site meter's lag (AD-007) — the
// per-device derivation the compliance path uses in place of the fixed
// exportBreachTicks. An inverter absent from the map contributes a zero
// latency (the wiring layer is expected to pre-default the map), and the floor
// of 2 in DetectionWindowTicks keeps the window sane regardless.
func (p Plant) ExportDetectionWindowTicks(inverter string, tickSeconds float64) int {
	ip := p.Inverters[inverter]
	return DetectionWindowTicks(ip.ControlLatencyS, p.Meter.MeterLagS, tickSeconds)
}

// ImportDetectionWindowTicks sizes the import-breach detection window for one
// battery from ITS control latency and the site meter's lag (AD-007) — the
// import lever is battery discharge, so this is the per-device derivation the
// import compliance path uses in place of the fixed importBreachTicks. A battery
// absent from the map contributes a zero latency (the wiring layer pre-defaults
// the map), and the floor of 2 in DetectionWindowTicks keeps the window sane.
func (p Plant) ImportDetectionWindowTicks(battery string, tickSeconds float64) int {
	bp := p.Batteries[battery]
	return DetectionWindowTicks(bp.ControlLatencyS, p.Meter.MeterLagS, tickSeconds)
}
