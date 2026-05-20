// Package inverter implements device.Device for SunSpec-compliant grid-tied
// inverters covering both legacy SunSpec models (101/102/103, 121, 123) and
// the IEEE 1547-2018 SunSpec Modbus profile (701-712).
package inverter

import (
	"fmt"
	"time"

	"lexa-hub/internal/csip/model"
	"lexa-hub/internal/southbound/derbase"
	"lexa-hub/internal/southbound/device"
	"lexa-hub/internal/southbound/modbus"
	"lexa-hub/internal/southbound/sunspec"
)

const tag = "inverter"

// Inverter implements device.Device for a SunSpec inverter over Modbus.
type Inverter struct {
	derbase.Base
	transport modbus.Transport
}

// New opens a Modbus connection, scans SunSpec models, and returns an Inverter.
func New(url string, timeout time.Duration, unitID uint8) (*Inverter, error) {
	t, err := modbus.NewTransport(url, timeout)
	if err != nil {
		return nil, err
	}
	if err := t.Open(); err != nil {
		return nil, fmt.Errorf("inverter: open %s: %w", url, err)
	}
	if err := t.SetUnitID(unitID); err != nil {
		t.Close()
		return nil, fmt.Errorf("inverter: set unit id %d: %w", unitID, err)
	}
	inv, err := newFromTransport(t)
	if err != nil {
		t.Close()
		return nil, err
	}
	return inv, nil
}

func newFromTransport(t modbus.Transport) (*Inverter, error) {
	r, err := sunspec.NewReader(t)
	if err != nil {
		return nil, fmt.Errorf("inverter: scan SunSpec blocks: %w", err)
	}
	base, err := derbase.Init(r, tag)
	if err != nil {
		return nil, err
	}
	return &Inverter{Base: base, transport: t}, nil
}

func (inv *Inverter) Close() error {
	return inv.transport.Close()
}

func (inv *Inverter) ReadMeasurements() (device.Measurements, error) {
	regs, err := inv.Reader.ReadModel(inv.MeasModel)
	if err != nil {
		return device.Measurements{}, fmt.Errorf("inverter: read model %d: %w", inv.MeasModel, err)
	}
	if inv.MeasModel == sunspec.ModelDERMeasureAC {
		return derbase.ReadMeasurementsM701(regs), nil
	}
	return derbase.ReadMeasurementsACModel(regs), nil
}

// Status reads the operating state from the inverter.
func (inv *Inverter) Status() (device.DeviceStatus, error) {
	regs, err := inv.Reader.ReadModel(inv.MeasModel)
	if err != nil {
		return device.DeviceStatus{}, fmt.Errorf("inverter: read status: %w", err)
	}
	if inv.MeasModel == sunspec.ModelDERMeasureAC {
		if len(regs) <= sunspec.M701_ConnSt {
			return device.DeviceStatus{}, fmt.Errorf("inverter: M701 too short for St/ConnSt")
		}
		st := sunspec.M701St(regs[sunspec.M701_St])
		return device.DeviceStatus{
			Connected: regs[sunspec.M701_ConnSt] == 1,
			Energized: st == sunspec.M701StOn || st == sunspec.M701StThrottled || st == sunspec.M701StStarting,
		}, nil
	}
	if len(regs) <= sunspec.M103_St {
		return device.DeviceStatus{}, fmt.Errorf("inverter: model %d too short for St", inv.MeasModel)
	}
	st := regs[sunspec.M103_St]
	return device.DeviceStatus{
		Connected: st == 4 || st == 5,
		Energized: st >= 3 && st <= 6,
	}, nil
}

func (inv *Inverter) ApplyControl(ctrl model.DERControlBase) error {
	return inv.Base.ApplyControl(ctrl, tag)
}

// ── Delegated IEEE 1547-2018 methods ─────────────────────────────────────────

func (inv *Inverter) SetEnterService(s sunspec.DEREnterServiceSettings) error {
	return inv.Base.SetEnterService(s, tag)
}
func (inv *Inverter) ReadEnterService() (sunspec.DEREnterServiceSettings, error) {
	return inv.Base.ReadEnterService(tag)
}
func (inv *Inverter) SetDERCtlAC(s sunspec.DERCtlACSettings) error {
	return inv.Base.SetDERCtlAC(s, tag)
}
func (inv *Inverter) ReadDERCtlAC() (sunspec.DERCtlACSettings, error) {
	return inv.Base.ReadDERCtlAC(tag)
}
func (inv *Inverter) ReadDERCapacity() (sunspec.DERCapacity, error) {
	return inv.Base.ReadDERCapacity(tag)
}
func (inv *Inverter) ReadVoltVar() (sunspec.VoltVarCurve, error) {
	return inv.Base.ReadVoltVar(tag)
}
func (inv *Inverter) WriteVoltVar(c sunspec.VoltVarCurve) error {
	return inv.Base.WriteVoltVar(c, tag)
}
func (inv *Inverter) ReadVoltWatt() (sunspec.VoltWattCurve, error) {
	return inv.Base.ReadVoltWatt(tag)
}
func (inv *Inverter) WriteVoltWatt(c sunspec.VoltWattCurve) error {
	return inv.Base.WriteVoltWatt(c, tag)
}
func (inv *Inverter) ReadVoltageTripLV() (sunspec.VoltageTripCurve, error) {
	return inv.Base.ReadVoltageTripLV(tag)
}
func (inv *Inverter) WriteVoltageTripLV(c sunspec.VoltageTripCurve) error {
	return inv.Base.WriteVoltageTripLV(c, tag)
}
func (inv *Inverter) ReadVoltageTripHV() (sunspec.VoltageTripCurve, error) {
	return inv.Base.ReadVoltageTripHV(tag)
}
func (inv *Inverter) WriteVoltageTripHV(c sunspec.VoltageTripCurve) error {
	return inv.Base.WriteVoltageTripHV(c, tag)
}
func (inv *Inverter) ReadFreqTripLF() (sunspec.FreqTripCurve, error) {
	return inv.Base.ReadFreqTripLF(tag)
}
func (inv *Inverter) WriteFreqTripLF(c sunspec.FreqTripCurve) error {
	return inv.Base.WriteFreqTripLF(c, tag)
}
func (inv *Inverter) ReadFreqTripHF() (sunspec.FreqTripCurve, error) {
	return inv.Base.ReadFreqTripHF(tag)
}
func (inv *Inverter) WriteFreqTripHF(c sunspec.FreqTripCurve) error {
	return inv.Base.WriteFreqTripHF(c, tag)
}
func (inv *Inverter) ReadFreqDroop() (sunspec.FreqDroopCtl, error) {
	return inv.Base.ReadFreqDroop(tag)
}
func (inv *Inverter) WriteFreqDroop(c sunspec.FreqDroopCtl) error {
	return inv.Base.WriteFreqDroop(c, tag)
}
func (inv *Inverter) ReadWattVar() (sunspec.WattVarCurve, error) {
	return inv.Base.ReadWattVar(tag)
}
func (inv *Inverter) WriteWattVar(c sunspec.WattVarCurve) error {
	return inv.Base.WriteWattVar(c, tag)
}
