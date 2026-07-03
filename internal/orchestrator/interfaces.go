package orchestrator

// Optimizer computes a Plan from the current SystemState.
//
// Optimize is called from a single goroutine (the Engine's control loop), so
// implementations may keep unsynchronized per-tick state across calls —
// DefaultOptimizer does (export/import guard hysteresis).  Do not call
// Optimize from multiple goroutines concurrently.
type Optimizer interface {
	Optimize(state SystemState) Plan
}

// SystemReader assembles a complete SystemState snapshot.
// The Engine calls this on each tick.
type SystemReader interface {
	ReadSystemState() (SystemState, error)
}

// SafetyEvaluator is an OPTIONAL extension of Optimizer. When the Engine's
// optimizer implements it (and the reader implements SafetyReader), the Engine
// runs a fast protection loop between economic ticks that calls EvaluateSafety and
// actuates only its protective commands. EvaluateSafety must be stateless enough
// to run at a high cadence and must share the optimizer's control goroutine (no
// concurrent use with Optimize). See ADR-0001 (two-loop control hierarchy).
type SafetyEvaluator interface {
	EvaluateSafety(state SystemState) Plan
}

// SafetyReader is an OPTIONAL extension of SystemReader that returns a cheap,
// side-effect-free snapshot (battery/grid only) for the fast protection loop, so
// polling it at a high cadence does not perturb the full reader's per-tick state
// (e.g. CSIP-control expiry counting).
type SafetyReader interface {
	ReadSafetyState() (SystemState, error)
}

// BatteryActuator applies a BatteryCommand to a physical or simulated battery.
type BatteryActuator interface {
	ApplyBatteryCommand(cmd BatteryCommand) error
}

// SolarActuator applies a SolarCommand (curtailment) to a solar inverter.
type SolarActuator interface {
	ApplySolarCommand(cmd SolarCommand) error
}

// EVSEActuator sends a charging current limit to an EV charging station.
type EVSEActuator interface {
	ApplyEVSECommand(cmd EVSECommand) error
}

// BatteryMetricsReader is an optional extension of device.Device for battery
// devices that can report SOC/SOH/capacity via their underlying Modbus model.
// The adapters type-assert to this interface when building SystemState.
type BatteryMetricsReader interface {
	ReadBatteryMetrics() (BatteryMetrics, error)
}

// BatteryMetrics holds battery-specific readings that are not part of the
// generic device.Measurements.
type BatteryMetrics struct {
	// SOC is the state of charge in percent [0, 100]; NaN if unavailable.
	SOC float64
	// SOH is the state of health in percent [0, 100]; NaN if unavailable.
	SOH float64
	// CapacityWh is the total usable capacity in watt-hours; NaN if unavailable.
	CapacityWh float64
	// MaxChargeW is the maximum charge rate in watts.
	MaxChargeW float64
	// MaxDischargeW is the maximum discharge rate in watts.
	MaxDischargeW float64
}
