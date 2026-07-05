// Package meter implements device.Device for SunSpec-compliant AC grid meters
// (Models 201/202/203). These meters sit at the main service entrance and
// measure the net power exchanged with the utility grid.
//
// Sign convention (all models, per SunSpec spec):
//
//	W positive → site is net importing from grid
//	W negative → site is net exporting to grid
//
// This is the opposite of the inverter/battery sign convention (positive =
// export) and maps directly to orchestrator.GridState.NetW.
//
// ApplyControl is a no-op: meters are read-only devices. The registry will
// call ApplyControl on all registered devices; this implementation silently
// ignores it so the meter does not disrupt control fan-outs.
package meter

import (
	"fmt"
	"math"
	"time"

	model "lexa-proto/csipmodel"
	"lexa-hub/internal/southbound/device"
	"lexa-proto/modbus"
	"lexa-proto/sunspec"
)

// Meter implements device.Device for a SunSpec AC grid meter over Modbus.
type Meter struct {
	transport modbus.Transport
	reader    *sunspec.Reader
	model     uint16 // detected meter model ID (201, 202, or 203)
}

// New opens a Modbus connection to url, scans for a SunSpec AC meter model,
// and returns a Meter. Prefers Model 203 (three-phase) over 202 (split-phase)
// over 201 (single-phase) when multiple models are present.
//
// url selects the physical layer ("tcp://host:502", "rtu:///dev/ttyS0", …).
// timeout applies to each Modbus transaction.
// unitID is the Modbus slave address.
func New(url string, timeout time.Duration, unitID uint8) (*Meter, error) {
	t, err := modbus.NewTransport(url, timeout)
	if err != nil {
		return nil, err
	}
	if err := t.Open(); err != nil {
		return nil, fmt.Errorf("meter: open %s: %w", url, err)
	}
	if err := t.SetUnitID(unitID); err != nil {
		t.Close()
		return nil, fmt.Errorf("meter: set unit id %d: %w", unitID, err)
	}
	m, err := newFromTransport(t)
	if err != nil {
		t.Close()
		return nil, err
	}
	return m, nil
}

// newFromTransport creates a Meter from an already-open Transport.
// Used by tests that inject a pre-configured transport.
func newFromTransport(t modbus.Transport) (*Meter, error) {
	r, err := sunspec.NewReader(t)
	if err != nil {
		return nil, fmt.Errorf("meter: scan SunSpec blocks: %w", err)
	}

	// Prefer the most capable model available.
	meterModel := uint16(0)
	for _, candidate := range []uint16{
		sunspec.ModelMeterThreePh,
		sunspec.ModelMeterSplitPh,
		sunspec.ModelMeterSinglePh,
	} {
		if r.HasModel(candidate) {
			meterModel = candidate
			break
		}
	}
	if meterModel == 0 {
		return nil, fmt.Errorf("meter: device has no AC meter model (201/202/203)")
	}

	return &Meter{transport: t, reader: r, model: meterModel}, nil
}

// ModelID returns the SunSpec model ID detected on the device (201, 202, or 203).
func (m *Meter) ModelID() uint16 { return m.model }

// Close releases the Modbus transport.
func (m *Meter) Close() error { return m.transport.Close() }

// ReadMeasurements reads the net power, voltage, and frequency from the meter.
//
// Returned Measurements:
//   - W: net real power (positive = importing, negative = exporting)
//   - V: Phase A L-N voltage (volts)
//   - Hz: AC frequency (Hz)
//   - VA, Var, PF: power quality (where available)
//   - DCV, DCW: always 0 (meters have no DC side)
//   - TmpCab: always NaN (meters don't report temperature)
func (m *Meter) ReadMeasurements() (device.Measurements, error) {
	regs, err := m.reader.ReadModel(m.model)
	if err != nil {
		return device.Measurements{}, fmt.Errorf("meter: read model %d: %w", m.model, err)
	}
	return parseMeterModel(regs, m.model), nil
}

// Status returns Connected=true and Energized=true whenever the meter is
// reachable over Modbus. Meters are passive measurement devices — they don't
// have an operational "on/off" state the way batteries or inverters do.
func (m *Meter) Status() (device.DeviceStatus, error) {
	// A successful ReadMeasurements confirms the meter is alive; we do a
	// lightweight model read here to verify reachability.
	if _, err := m.reader.ReadModel(m.model); err != nil {
		return device.DeviceStatus{}, fmt.Errorf("meter: status read: %w", err)
	}
	return device.DeviceStatus{Connected: true, Energized: true}, nil
}

// ApplyControl is a no-op. AC meters are read-only devices and do not accept
// DERControlBase commands. This method exists solely to satisfy device.Device.
func (m *Meter) ApplyControl(_ model.DERControlBase) error { return nil }

// parseMeterModel extracts Measurements from a slice of raw meter model registers.
// The modelID selects which offset table to use (201/202/203).
func parseMeterModel(regs []uint16, modelID uint16) device.Measurements {
	get := func(offset int) uint16 {
		if offset < len(regs) {
			return regs[offset]
		}
		return 0
	}
	sf := func(offset int) int16 { return int16(get(offset)) }

	meas := device.Measurements{TmpCab: math.NaN()}

	// Select per-model register offsets.
	type offsets struct{ w, wSF, v, vSF, hz, hzSF, va, vaSF, varr, varrSF, pf, pfSF int }
	var o offsets
	switch modelID {
	case sunspec.ModelMeterSinglePh: // 201
		o = offsets{
			w: sunspec.M201_W, wSF: sunspec.M201_W_SF,
			v: sunspec.M201_PhVphA, vSF: sunspec.M201_V_SF,
			hz: sunspec.M201_Hz, hzSF: sunspec.M201_Hz_SF,
			va: sunspec.M201_VA, vaSF: sunspec.M201_VA_SF,
			varr: sunspec.M201_VAR, varrSF: sunspec.M201_VAR_SF,
			pf: sunspec.M201_PF, pfSF: sunspec.M201_PF_SF,
		}
	case sunspec.ModelMeterSplitPh: // 202
		o = offsets{
			w: sunspec.M202_W, wSF: sunspec.M202_W_SF,
			v: sunspec.M202_PhVphA, vSF: sunspec.M202_V_SF,
			hz: sunspec.M202_Hz, hzSF: sunspec.M202_Hz_SF,
			// 202 does not have per-total VA/VAR/PF at these offsets in this impl
			va: -1, varr: -1, pf: -1,
		}
	case sunspec.ModelMeterThreePh: // 203
		o = offsets{
			w: sunspec.M203_W, wSF: sunspec.M203_W_SF,
			v: sunspec.M203_PhVphA, vSF: sunspec.M203_V_SF,
			hz: sunspec.M203_Hz, hzSF: sunspec.M203_Hz_SF,
			va: sunspec.M203_VA, vaSF: sunspec.M203_VA_SF,
			varr: sunspec.M203_VAR, varrSF: sunspec.M203_VAR_SF,
			pf: sunspec.M203_PF, pfSF: sunspec.M203_PF_SF,
		}
	default:
		return meas
	}

	if len(regs) > o.wSF {
		meas.W = sunspec.ApplyScaleSigned(get(o.w), sf(o.wSF))
	}
	if len(regs) > o.vSF {
		meas.V = sunspec.ApplyScaleUint(get(o.v), sf(o.vSF))
	}
	if len(regs) > o.hzSF {
		meas.Hz = sunspec.ApplyScaleUint(get(o.hz), sf(o.hzSF))
	}
	if o.va >= 0 && len(regs) > o.vaSF {
		meas.VA = sunspec.ApplyScaleSigned(get(o.va), sf(o.vaSF))
	}
	if o.varr >= 0 && len(regs) > o.varrSF {
		meas.Var = sunspec.ApplyScaleSigned(get(o.varr), sf(o.varrSF))
	}
	if o.pf >= 0 && len(regs) > o.pfSF {
		// SunSpec PF stored ×100; divide back to get [-1, +1].
		meas.PF = sunspec.ApplyScaleSigned(get(o.pf), sf(o.pfSF)) / 100.0
	}
	return meas
}
