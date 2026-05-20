package orchestrator

// Optimizer computes a Plan from the current SystemState.
// Implementations must be safe to call from multiple goroutines.
type Optimizer interface {
	Optimize(state SystemState) Plan
}

// SystemReader assembles a complete SystemState snapshot.
// The Engine calls this on each tick.
type SystemReader interface {
	ReadSystemState() (SystemState, error)
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
