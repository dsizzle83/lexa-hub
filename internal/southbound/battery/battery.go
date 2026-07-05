// Package battery implements device.Device for SunSpec-compliant battery
// storage systems covering both legacy SunSpec models (101/102/103, 121, 123,
// 802) and the IEEE 1547-2018 SunSpec Modbus profile (701-713).
package battery

import (
	"fmt"
	"time"

	model "lexa-proto/csipmodel"
	"lexa-proto/derbase"
	"lexa-hub/internal/southbound/device"
	"lexa-proto/modbus"
	"lexa-proto/sunspec"
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
	var m device.Measurements
	if b.MeasModel == sunspec.ModelDERMeasureAC {
		m = derbase.ReadMeasurementsM701(regs)
	} else {
		m = derbase.ReadMeasurementsACModel(regs)
	}
	// Attach state of charge so it rides the measurement bus to the API and
	// dashboard. Best-effort: a metrics read failure leaves SOC as NaN.
	if bm, err := b.ReadBatteryMetrics(); err == nil {
		m.SOC = bm.SOC
	}
	return m, nil
}

// Status reads the battery connection state.
// Uses M701 → M802 → M103 fallback chain.
func (b *Battery) Status() (device.DeviceStatus, error) {
	if b.MeasModel == sunspec.ModelDERMeasureAC {
		regs, err := b.Reader.ReadModel(sunspec.ModelDERMeasureAC)
		if err != nil {
			return device.DeviceStatus{}, fmt.Errorf("battery: read M701 status: %w", err)
		}
		m := sunspec.Parse701(regs)
		return device.DeviceStatus{
			Connected: m.ConnSt == 1,
			Energized: m.InvSt == 2 || m.InvSt == 3 || m.InvSt == 4,
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
func (b *Battery) ReadStorageCapacity() (sunspec.StorageCapacity, error) {
	return b.Base.ReadStorageCapacity(tag)
}

// ── Delegated IEEE 1547-2018 methods ─────────────────────────────────────────

func (b *Battery) SetEnterService(s sunspec.EnterService) error {
	return b.Base.SetEnterService(s, tag)
}
func (b *Battery) ReadEnterService() (sunspec.EnterService, error) {
	return b.Base.ReadEnterService(tag)
}
func (b *Battery) ReadDERCtlAC() (sunspec.ACControls, error) {
	return b.Base.ReadDERCtlAC(tag)
}
func (b *Battery) ReadDERCapacity() (sunspec.Capacity, error) {
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
func (b *Battery) ReadVoltageTripLV() (sunspec.VoltageTripSet, error) {
	return b.Base.ReadVoltageTripLV(tag)
}
func (b *Battery) WriteVoltageTripLV(c sunspec.VoltageTripSet) error {
	return b.Base.WriteVoltageTripLV(c, tag)
}
func (b *Battery) ReadVoltageTripHV() (sunspec.VoltageTripSet, error) {
	return b.Base.ReadVoltageTripHV(tag)
}
func (b *Battery) WriteVoltageTripHV(c sunspec.VoltageTripSet) error {
	return b.Base.WriteVoltageTripHV(c, tag)
}
func (b *Battery) ReadFreqTripLF() (sunspec.FreqTripSet, error) {
	return b.Base.ReadFreqTripLF(tag)
}
func (b *Battery) WriteFreqTripLF(c sunspec.FreqTripSet) error {
	return b.Base.WriteFreqTripLF(c, tag)
}
func (b *Battery) ReadFreqTripHF() (sunspec.FreqTripSet, error) {
	return b.Base.ReadFreqTripHF(tag)
}
func (b *Battery) WriteFreqTripHF(c sunspec.FreqTripSet) error {
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
