// Package orchestrator is the central "brain" of the energy management system.
//
// It consumes control signals from IEEE 2030.5 (CSIP), monitors real-time
// device state from Modbus and OCPP, and produces coordinated control actions
// across batteries, solar inverters, and EV chargers.
//
// Data flow:
//
//	[CSIP control (MQTT)] ──ReadSystemState──► Engine.state
//	[Modbus registry] ──Subscribe()────► Engine.state
//	[OCPP tracker] ────EVSEStates()────► Engine.state
//	                                          │
//	                                   Optimizer.Optimize()
//	                                          │
//	                              [BatteryActuator] → Modbus
//	                              [SolarActuator]  → Modbus
//	                              [EVSEActuator]   → OCPP
//
// Concurrency model: a single goroutine owns all state mutation; external
// callers only push through channels or call thread-safe setters.
package orchestrator

import (
	"math"
	"time"

	"lexa-hub/internal/northbound/model"
)

// ── Device state snapshots ────────────────────────────────────────────────────

// DeviceRole classifies a device for the optimizer.
type DeviceRole uint8

const (
	RoleBattery DeviceRole = iota
	RoleSolar
)

// BatteryState is a read-only snapshot of a battery device.
type BatteryState struct {
	Name string

	// PowerW is the current net AC power.
	// Positive = discharging (export); negative = charging (import).
	PowerW float64

	// SOC is the state of charge in percent [0, 100].
	// math.NaN() when not available from the device.
	SOC float64

	// SOH is the state of health in percent [0, 100].
	// math.NaN() when not available.
	SOH float64

	// MaxChargeW is the maximum charge rate in watts (positive magnitude).
	MaxChargeW float64

	// MaxDischargeW is the maximum discharge rate in watts (positive magnitude).
	MaxDischargeW float64

	// CapacityWh is the usable energy capacity in watt-hours.
	// math.NaN() when not available.
	CapacityWh float64

	// ChargeEfficiency and DischargeEfficiency are roundtrip fractions [0,1].
	// Default 1.0 when unknown.
	ChargeEfficiency    float64
	DischargeEfficiency float64

	Connected bool
	Energized bool
}

// AvailableChargeW returns the additional watts of charge power that can be
// commanded right now — the full swing from the current setpoint down to
// −MaxChargeW.  When the battery is currently discharging (PowerW > 0) this
// is larger than MaxChargeW because the discharge headroom can also be
// redirected into charge.
func (b BatteryState) AvailableChargeW() float64 {
	if !b.Connected {
		return 0
	}
	return math.Max(0, b.MaxChargeW+b.PowerW)
}

// AvailableDischargeW returns the additional watts of discharge power that can
// be commanded right now — the full swing from the current setpoint up to
// +MaxDischargeW.  When the battery is currently charging (PowerW < 0) this
// is larger than MaxDischargeW.
func (b BatteryState) AvailableDischargeW() float64 {
	if !b.Connected || !b.Energized {
		return 0
	}
	return math.Max(0, b.MaxDischargeW-b.PowerW)
}

// SolarState is a read-only snapshot of a solar inverter.
type SolarState struct {
	Name string

	// PowerW is the current generation (always ≥ 0 for solar).
	PowerW float64

	// MaxW is the inverter's nameplate generation capacity.
	MaxW float64

	Connected bool
	Energized bool
}

// EVSEState is a read-only snapshot of an EV charging station connector.
type EVSEState struct {
	// StationID is the OCPP charging station identifier.
	StationID string

	// ConnectorID is 1-based; 0 means the EVSE as a whole.
	ConnectorID int

	// Connected indicates the WebSocket (OCPP) connection is up.
	Connected bool

	// SessionActive is true when a vehicle is plugged in and charging.
	SessionActive bool

	// CurrentA is the measured charging current in amperes (from MeterValues).
	// This is actual current, not the commanded limit.
	CurrentA float64

	// MaxCurrentA is the EVSE hardware limit in amperes.
	MaxCurrentA float64

	// VoltageV is the supply voltage.
	VoltageV float64

	// PowerW is the current power draw (positive = consuming from grid/battery/solar).
	PowerW float64

	// Status is the OCPP ConnectorStatus string ("Available", "Occupied", etc.).
	Status string

	// SOC is the EV battery state of charge in percent [0, 100].
	// math.NaN() when no MeterValues with SoC have been received.
	SOC float64

	// EnergyWh is the cumulative energy delivered this session (Wh).
	// Zero when no session is active or no MeterValues received.
	EnergyWh float64
}

// GridState holds measured and constrained grid values.
type GridState struct {
	// Measured values (from a grid meter if present; NaN if unavailable).
	FrequencyHz float64
	VoltageV    float64

	// NetW is the net grid power at the point of common coupling.
	// Positive = import from grid; negative = export to grid.
	// math.NaN() when no grid meter is present.
	NetW float64

	// Constraint limits received from CSIP (NaN = no limit imposed).
	// These are the distilled values after resolving active CSIP programs.
	ImportLimitW float64 // max watts we may draw from grid
	ExportLimitW float64 // max watts we may push to grid
	MaxLimitW    float64 // absolute generation cap (OpModMaxLimW)
}

// CSIPControlState summarises the currently-active CSIP control signal.
type CSIPControlState struct {
	Source string // "event" or "default"
	MRID   string
	Base   model.DERControlBase
	// ValidUntil is the Unix timestamp when this control expires (0 = no expiry).
	ValidUntil int64
}

// SystemState is the complete picture of the energy system at one moment.
// The Engine builds this on every optimization tick.
type SystemState struct {
	Timestamp time.Time

	Batteries []BatteryState
	Solar     []SolarState
	EVSEs     []EVSEState
	Grid      GridState

	// CSIPControl is nil when no programs have been received yet.
	CSIPControl *CSIPControlState

	// ClockOffset is (server_time − local_time) in seconds; from CSIP discovery.
	ClockOffset int64

	// DailyPlanTarget is the cost-optimal target for the current 5-min interval,
	// set by the Engine from the most recent DailyPlan. Nil when no plan is available
	// or the current time is outside the plan window.
	DailyPlanTarget *PlanTarget
}

// TotalSolarW sums generation across all solar devices.
func (s SystemState) TotalSolarW() float64 {
	total := 0.0
	for _, sol := range s.Solar {
		if sol.Connected {
			total += sol.PowerW
		}
	}
	return total
}

// TotalBatteryW sums net battery power (+ discharge, - charge).
func (s SystemState) TotalBatteryW() float64 {
	total := 0.0
	for _, b := range s.Batteries {
		if b.Connected {
			total += b.PowerW
		}
	}
	return total
}

// InferredLoadW estimates total site load (home + EV) from the energy balance
// at the point of common coupling:
//
//	load_W = solar_W + battery_W + grid_W
//
// All generation and grid import feed the site bus; whatever isn't sent to the
// grid must be consumed on site.  Returns math.NaN() when no grid meter is
// present (Grid.NetW is NaN).
func (s SystemState) InferredLoadW() float64 {
	if math.IsNaN(s.Grid.NetW) {
		return math.NaN()
	}
	return s.TotalSolarW() + s.TotalBatteryW() + s.Grid.NetW
}

// TotalEVSEW sums current EV charging load.
func (s SystemState) TotalEVSEW() float64 {
	total := 0.0
	for _, e := range s.EVSEs {
		if e.SessionActive {
			total += e.PowerW
		}
	}
	return total
}

// ── Plan types ────────────────────────────────────────────────────────────────

// BatteryCommand is a setpoint for one battery.
type BatteryCommand struct {
	Name string

	// SetpointW: positive = discharge, negative = charge, 0 = idle.
	// math.NaN() means "leave unchanged".
	SetpointW float64

	// Connect: nil means "leave unchanged".
	Connect *bool
}

// SolarCommand limits a solar inverter's output.
type SolarCommand struct {
	Name string

	// CurtailToW caps the inverter output. math.NaN() means "no curtailment".
	CurtailToW float64
}

// EVSECommand sets a current limit on an EV charger.
type EVSECommand struct {
	StationID   string
	ConnectorID int
	// MaxCurrentA: 0 = suspend session, >0 = set limit.
	MaxCurrentA float64
}

// Decision records one reasoning step in the optimizer.
type Decision struct {
	Rule   string // rule name ("csip-export-limit", "self-consumption", …)
	Reason string // why this rule fired
	Impact string // what it changes
}

// ComplianceBreach reports a CSIP control limit the optimizer could not honour
// this tick after exhausting every lever — e.g. an import cap that would need
// the battery to discharge below its SOC reserve, or an export/generation cap
// the inverters cannot curtail far enough. It is informational: the limit is
// still violated, but the hub reports it upstream as a 2030.5 CannotComply
// Response so the grid server knows the DER is resource-limited, not faulty.
type ComplianceBreach struct {
	MRID       string  // active DERControl that cannot be met
	LimitType  string  // "import" | "export" | "generation"
	LimitW     float64 // commanded limit (W)
	MeasuredW  float64 // actual net/generation at the meter (W)
	ShortfallW float64 // how far over the limit, after all levers (W)
	Reason     string  // human-readable cause, e.g. "battery at SOC reserve"
}

// Plan is the optimizer's output: a set of commands plus a decision trace.
type Plan struct {
	Timestamp time.Time

	BatteryCommands []BatteryCommand
	SolarCommands   []SolarCommand
	EVSECommands    []EVSECommand

	// Decisions records why each command was issued, for observability.
	Decisions []Decision

	// Breach, when non-nil, flags a CSIP limit that could not be met this tick.
	Breach *ComplianceBreach

	// Safety marks a fast-protection-loop plan (EvaluateSafety). Safety plans
	// never evaluate CSIP limits, so Breach==nil on them means "not assessed",
	// not "compliant" — consumers tracking breach begin/clear edges (the
	// CannotComply alerter) must skip them or an inter-tick safety action
	// would publish a spurious breach-clear mid-episode.
	Safety bool
}

// AddDecision appends a Decision to the plan's trace.
func (p *Plan) AddDecision(rule, reason, impact string) {
	p.Decisions = append(p.Decisions, Decision{Rule: rule, Reason: reason, Impact: impact})
}

// ── Default values ────────────────────────────────────────────────────────────

// NewBatteryState returns a BatteryState with sensible NaN defaults.
func NewBatteryState(name string) BatteryState {
	return BatteryState{
		Name:                name,
		SOC:                 math.NaN(),
		SOH:                 math.NaN(),
		CapacityWh:          math.NaN(),
		ChargeEfficiency:    1.0,
		DischargeEfficiency: 1.0,
	}
}

// NewGridState returns a GridState with all limits unset (NaN).
func NewGridState() GridState {
	return GridState{
		FrequencyHz:  math.NaN(),
		VoltageV:     math.NaN(),
		NetW:         math.NaN(),
		ImportLimitW: math.NaN(),
		ExportLimitW: math.NaN(),
		MaxLimitW:    math.NaN(),
	}
}
