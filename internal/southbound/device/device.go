// Package device defines the abstract interface that all southbound DER
// hardware must implement, regardless of physical protocol (Modbus, DNP3, etc.).
//
// The interface is intentionally narrow — three methods plus Close — so that
// higher layers (registry, bridge) remain protocol-agnostic. Concrete
// implementations live in sibling packages (inverter, battery).
package device

import "lexa-hub/internal/northbound/model"

// Device is the southbound abstraction for a single piece of DER hardware.
// Implementations must be safe for concurrent use.
type Device interface {
	// ApplyControl writes the given DERControlBase parameters to the device.
	// Fields that are nil are left unchanged on the device. Implementations
	// should apply only the fields they support and silently ignore the rest.
	ApplyControl(ctrl model.DERControlBase) error

	// ReadMeasurements returns the latest AC/DC measurements from the device.
	// Fields that the device does not support should be set to math.NaN().
	ReadMeasurements() (Measurements, error)

	// Status returns the device's current connect/energize state.
	Status() (DeviceStatus, error)

	// Close releases any resources held by the device (e.g. Modbus connection).
	Close() error
}

// Measurements holds a snapshot of electrical measurements from a DER device.
//
// Sign convention — power fields use the generator/load sign from the device's
// own perspective (IEC 62053 / SunSpec convention):
//
//	W > 0  device is exporting power (solar generating, battery discharging)
//	W < 0  device is importing power (battery charging, load consuming)
//
// The grid meter's W follows the same convention from the meter's perspective:
//
//	W > 0  power flowing from grid into site (import)
//	W < 0  power flowing from site into grid (export)
//
// Fields set to math.NaN() are not available from this device.
type Measurements struct {
	// AC-side
	W   float64 // net AC real power (watts)
	VA  float64 // apparent power (volt-amps)
	Var float64 // reactive power (vars, positive = capacitive)
	V   float64 // phase-A-to-neutral voltage (volts)
	Hz  float64 // AC frequency (Hz)
	PF  float64 // power factor (−1 to +1)

	// DC-side (inverters only; NaN if not applicable)
	DCV float64 // DC bus voltage (volts)
	DCW float64 // DC power (watts)

	// Thermal
	TmpCab float64 // cabinet temperature (°C); NaN if not reported
}

// DeviceStatus describes the current operational state of a DER.
type DeviceStatus struct {
	// Connected is true when the device is electrically connected to the grid.
	Connected bool
	// Energized is true when the device is producing or consuming power.
	Energized bool
}
