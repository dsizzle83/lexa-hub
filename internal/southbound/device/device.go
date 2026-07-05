// Package device defines the abstract interface that all southbound DER
// hardware must implement, regardless of physical protocol (Modbus, DNP3, etc.).
//
// The interface is intentionally narrow — three methods plus Close — so that
// higher layers (registry, bridge) remain protocol-agnostic. Concrete
// implementations live in sibling packages (inverter, battery).
package device

import (
	model "lexa-proto/csipmodel"
	"lexa-proto/derbase"
)

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
// It is defined in lexa-proto/derbase (TASK-023) — derbase is what actually
// constructs it from raw SunSpec registers — and aliased here so every
// existing call site (device.Measurements{...} literals, method signatures)
// keeps compiling unchanged. See derbase.Measurements for the full field
// docs (sign convention, NaN-means-unavailable, etc).
type Measurements = derbase.Measurements

// DeviceStatus describes the current operational state of a DER.
type DeviceStatus struct {
	// Connected is true when the device is electrically connected to the grid.
	Connected bool
	// Energized is true when the device is producing or consuming power.
	Energized bool
}
