// Package battery implements device.Device for SunSpec-compliant battery
// storage systems covering both legacy SunSpec models (101/102/103, 121, 123,
// 802) and the IEEE 1547-2018 SunSpec Modbus profile (701-713).
package battery

import (
	"fmt"
	"time"

	"lexa-hub/internal/northbound/model"
	"lexa-hub/internal/southbound/derbase"
	"lexa-hub/internal/southbound/device"
	"lexa-hub/internal/southbound/modbus"
	"lexa-hub/internal/southbound/sunspec"
)

const tag = "battery"

// Battery implements device.Device for a SunSpec battery storage system.
type Battery struct {
	derbase.Base
	transport modbus.Transport
	has713    bool // DERStorageCapacity present (IEEE 1547-2018)
}

// New opens a Modbus connection, scans SunSpec models, and returns a Battery.
func New(url string, timeout time.Duration, unitID uint8) (*Battery, error) {
	t, err := modbus.NewTransport(url, timeout)
	if err != nil {
		return nil, err
	}
	if err := t.Open(); err != nil {
		return nil, fmt.Errorf("battery: open %s: %w", url, err)
	}
	if err := t.SetUnitID(unitID); err != nil {
		t.Close()
		return nil, fmt.Errorf("battery: set unit id %d: %w", unitID, err)
	}
	b, err := newFromTransport(t)
	if err != nil {
		t.Close()
		return nil, err
	}
	return b, nil
}

func newFromTransport(t modbus.Transport) (*Battery, error) {
	r, err := sunspec.NewReader(t)
	if err != nil {
		return nil, fmt.Errorf("battery: scan SunSpec blocks: %w", err)
	}
	base, err := derbase.Init(r, tag)
	if err != nil {
		return nil, err
	}
	return &Battery{
		Base:      base,
		transport: t,
		has713:    r.HasModel(sunspec.ModelDERStorageCap),
	}, nil
}

func (b *Battery) Close() error {
	return b.transport.Close()
}

func (b *Battery) ReadMeasurements() (device.Measurements, error) {
	regs, err := b.Reader.ReadModel(b.MeasModel)
	if err != nil {
		return device.Measurements{}, fmt.Errorf("battery: read model %d: %w", b.MeasModel, err)
	}
	if b.MeasModel == sunspec.ModelDERMeasureAC {
		return derbase.ReadMeasurementsM701(regs), nil
	}
	return derbase.ReadMeasurementsACModel(regs), nil
}

// Status reads the battery connection state.
// Uses M701 → M802 → M103 fallback chain.
func (b *Battery) Status() (device.DeviceStatus, error) {
	if b.MeasModel == sunspec.ModelDERMeasureAC {
		regs, err := b.Reader.ReadModel(sunspec.ModelDERMeasureAC)
		if err != nil {
			return device.DeviceStatus{}, fmt.Errorf("battery: read M701 status: %w", err)
		}
		if len(regs) <= sunspec.M701_ConnSt {
			return device.DeviceStatus{}, fmt.Errorf("battery: M701 too short for St/ConnSt")
		}
		st := sunspec.M701St(regs[sunspec.M701_St])
		return device.DeviceStatus{
			Connected: regs[sunspec.M701_ConnSt] == 1,
			Energized: st == sunspec.M701StOn || st == sunspec.M701StThrottled || st == sunspec.M701StStarting,
		}, nil
	}

	if b.Reader.HasModel(sunspec.ModelLithiumBattery) {
		regs, err := b.Reader.ReadModel(sunspec.ModelLithiumBattery)
		if err != nil {
			return device.DeviceStatus{}, fmt.Errorf("battery: read Model 802: %w", err)
		}
		if len(regs) > sunspec.M802_State {
			state := regs[sunspec.M802_State]
			chaSt := uint16(0)
			if len(regs) > sunspec.M802_ChaSt {
				chaSt = regs[sunspec.M802_ChaSt]
			}
			return device.DeviceStatus{
				Connected: state == 2 || state == 3,
				Energized: chaSt >= 3 && chaSt <= 6,
			}, nil
		}
	}

	regs, err := b.Reader.ReadModel(b.MeasModel)
	if err != nil {
		return device.DeviceStatus{}, fmt.Errorf("battery: read status: %w", err)
	}
	if len(regs) <= sunspec.M103_St {
		return device.DeviceStatus{}, fmt.Errorf("battery: model too short for St")
	}
	st := regs[sunspec.M103_St]
	return device.DeviceStatus{
		Connected: st == 4 || st == 5,
		Energized: st >= 3 && st <= 6,
	}, nil
}

func (b *Battery) ApplyControl(ctrl model.DERControlBase) error {
	return b.Base.ApplyControl(ctrl, tag)
}

// ReadStorageCapacity reads battery storage state from M713.
func (b *Battery) ReadStorageCapacity() (sunspec.DERStorageCapacity, error) {
	if !b.has713 {
		return sunspec.DERStorageCapacity{}, fmt.Errorf("battery: device has no M713 (DERStorageCapacity)")
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDERStorageCap)
	if err != nil {
		return sunspec.DERStorageCapacity{}, fmt.Errorf("battery: read M713: %w", err)
	}
	return sunspec.ParseDERStorageCapacity(regs)
}

// ── Delegated IEEE 1547-2018 methods ─────────────────────────────────────────

func (b *Battery) SetEnterService(s sunspec.DEREnterServiceSettings) error {
	return b.Base.SetEnterService(s, tag)
}
func (b *Battery) ReadEnterService() (sunspec.DEREnterServiceSettings, error) {
	return b.Base.ReadEnterService(tag)
}
func (b *Battery) SetDERCtlAC(s sunspec.DERCtlACSettings) error {
	return b.Base.SetDERCtlAC(s, tag)
}
func (b *Battery) ReadDERCtlAC() (sunspec.DERCtlACSettings, error) {
	return b.Base.ReadDERCtlAC(tag)
}
func (b *Battery) ReadDERCapacity() (sunspec.DERCapacity, error) {
	return b.Base.ReadDERCapacity(tag)
}
func (b *Battery) ReadVoltVar() (sunspec.VoltVarCurve, error) {
	return b.Base.ReadVoltVar(tag)
}
func (b *Battery) WriteVoltVar(c sunspec.VoltVarCurve) error {
	return b.Base.WriteVoltVar(c, tag)
}
func (b *Battery) ReadVoltWatt() (sunspec.VoltWattCurve, error) {
	return b.Base.ReadVoltWatt(tag)
}
func (b *Battery) WriteVoltWatt(c sunspec.VoltWattCurve) error {
	return b.Base.WriteVoltWatt(c, tag)
}
func (b *Battery) ReadVoltageTripLV() (sunspec.VoltageTripCurve, error) {
	return b.Base.ReadVoltageTripLV(tag)
}
func (b *Battery) WriteVoltageTripLV(c sunspec.VoltageTripCurve) error {
	return b.Base.WriteVoltageTripLV(c, tag)
}
func (b *Battery) ReadVoltageTripHV() (sunspec.VoltageTripCurve, error) {
	return b.Base.ReadVoltageTripHV(tag)
}
func (b *Battery) WriteVoltageTripHV(c sunspec.VoltageTripCurve) error {
	return b.Base.WriteVoltageTripHV(c, tag)
}
func (b *Battery) ReadFreqTripLF() (sunspec.FreqTripCurve, error) {
	return b.Base.ReadFreqTripLF(tag)
}
func (b *Battery) WriteFreqTripLF(c sunspec.FreqTripCurve) error {
	return b.Base.WriteFreqTripLF(c, tag)
}
func (b *Battery) ReadFreqTripHF() (sunspec.FreqTripCurve, error) {
	return b.Base.ReadFreqTripHF(tag)
}
func (b *Battery) WriteFreqTripHF(c sunspec.FreqTripCurve) error {
	return b.Base.WriteFreqTripHF(c, tag)
}
func (b *Battery) ReadFreqDroop() (sunspec.FreqDroopCtl, error) {
	return b.Base.ReadFreqDroop(tag)
}
func (b *Battery) WriteFreqDroop(c sunspec.FreqDroopCtl) error {
	return b.Base.WriteFreqDroop(c, tag)
}
func (b *Battery) ReadWattVar() (sunspec.WattVarCurve, error) {
	return b.Base.ReadWattVar(tag)
}
func (b *Battery) WriteWattVar(c sunspec.WattVarCurve) error {
	return b.Base.WriteWattVar(c, tag)
}
