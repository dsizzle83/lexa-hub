// Package derbase provides shared SunSpec DER device logic for inverter and
// battery packages. It handles IEEE 1547-2018 models (M701-M712), legacy
// models (M103/M121/M123), measurement parsing, and control application.
//
// Embed Base in a concrete device type and call Init after SunSpec scan.
package derbase

import (
	"fmt"
	"math"

	"lexa-hub/internal/northbound/model"
	"lexa-hub/internal/southbound/device"
	"lexa-hub/internal/southbound/sunspec"
)

// Base holds shared SunSpec DER state and methods. Embed in concrete types.
type Base struct {
	Reader    *sunspec.Reader
	Wmax      float64 // nameplate WMax in watts; NaN if unavailable
	MeasModel uint16  // model ID for measurements: 701, 103, 102, or 101
	Has701    bool
	Has702    bool
	Has703    bool
	Has704    bool
	Has705    bool
	Has706    bool
	Has707    bool
	Has708    bool
	Has709    bool
	Has710    bool
	Has711    bool
	Has712    bool
}

// Init populates model-presence flags, selects the measurement model, and
// reads WMax. tag is used in error messages (e.g. "inverter", "battery").
func Init(r *sunspec.Reader, tag string) (Base, error) {
	b := Base{
		Reader: r,
		Wmax:   math.NaN(),
		Has701: r.HasModel(sunspec.ModelDERMeasureAC),
		Has702: r.HasModel(sunspec.ModelDERCapacity),
		Has703: r.HasModel(sunspec.ModelDEREnterService),
		Has704: r.HasModel(sunspec.ModelDERCtlAC),
		Has705: r.HasModel(sunspec.ModelDERVoltVar),
		Has706: r.HasModel(sunspec.ModelDERVoltWatt),
		Has707: r.HasModel(sunspec.ModelDERTripLV),
		Has708: r.HasModel(sunspec.ModelDERTripHV),
		Has709: r.HasModel(sunspec.ModelDERTripLF),
		Has710: r.HasModel(sunspec.ModelDERTripHF),
		Has711: r.HasModel(sunspec.ModelDERFreqDroop),
		Has712: r.HasModel(sunspec.ModelDERWattVar),
	}

	if b.Has701 {
		b.MeasModel = sunspec.ModelDERMeasureAC
	} else {
		for _, c := range []uint16{sunspec.ModelInverterThreePh, sunspec.ModelInverterSplitPh, sunspec.ModelInverterSinglePh} {
			if r.HasModel(c) {
				b.MeasModel = c
				break
			}
		}
		if b.MeasModel == 0 {
			return Base{}, fmt.Errorf("%s: device has no AC measurement model (701, 103, 102, or 101)", tag)
		}
	}

	if b.Has702 {
		if w, err := ReadWMaxFrom702(r); err == nil {
			b.Wmax = w
		}
	} else if r.HasModel(sunspec.ModelBasicSettings) {
		if w, err := ReadWMax(r); err == nil {
			b.Wmax = w
		}
	}

	return b, nil
}

// ── Measurements ─────────────────────────────────────────────────────────────

// ReadMeasurementsM701 parses M701 (DERMeasureAC) registers.
func ReadMeasurementsM701(regs []uint16) device.Measurements {
	get := func(offset int) uint16 {
		if offset < len(regs) {
			return regs[offset]
		}
		return 0
	}
	sf := func(offset int) int16 { return int16(get(offset)) }

	m := device.Measurements{TmpCab: math.NaN()}
	if len(regs) > sunspec.M701_W_SF {
		m.W = sunspec.ApplyScaleSigned(get(sunspec.M701_W), sf(sunspec.M701_W_SF))
	}
	if len(regs) > sunspec.M701_V_SF {
		v := get(sunspec.M701_VL1)
		if v == 0x8000 {
			v = get(sunspec.M701_LNV)
		}
		m.V = sunspec.ApplyScaleUint(v, sf(sunspec.M701_V_SF))
	}
	if len(regs) > sunspec.M701_Hz_SF {
		m.Hz = sunspec.ApplyScaleUint(get(sunspec.M701_Hz), sf(sunspec.M701_Hz_SF))
	}
	if len(regs) > sunspec.M701_VA_SF {
		m.VA = sunspec.ApplyScaleSigned(get(sunspec.M701_VA), sf(sunspec.M701_VA_SF))
	}
	if len(regs) > sunspec.M701_Var_SF {
		m.Var = sunspec.ApplyScaleSigned(get(sunspec.M701_Var), sf(sunspec.M701_Var_SF))
	}
	if len(regs) > sunspec.M701_PF_SF {
		m.PF = sunspec.ApplyScaleSigned(get(sunspec.M701_PF), sf(sunspec.M701_PF_SF)) / 100.0
	}
	return m
}

// ReadMeasurementsACModel parses Model 10x (101/102/103) registers.
func ReadMeasurementsACModel(regs []uint16) device.Measurements {
	get := func(offset int) uint16 {
		if offset < len(regs) {
			return regs[offset]
		}
		return 0
	}
	sf := func(offset int) int16 { return int16(get(offset)) }

	m := device.Measurements{TmpCab: math.NaN()}
	if len(regs) > sunspec.M103_W_SF {
		m.W = sunspec.ApplyScaleSigned(get(sunspec.M103_W), sf(sunspec.M103_W_SF))
	}
	if len(regs) > sunspec.M103_V_SF {
		m.V = sunspec.ApplyScaleUint(get(sunspec.M103_PhVphA), sf(sunspec.M103_V_SF))
	}
	if len(regs) > sunspec.M103_Hz_SF {
		m.Hz = sunspec.ApplyScaleUint(get(sunspec.M103_Hz), sf(sunspec.M103_Hz_SF))
	}
	if len(regs) > sunspec.M103_VA_SF {
		m.VA = sunspec.ApplyScaleSigned(get(sunspec.M103_VA), sf(sunspec.M103_VA_SF))
	}
	if len(regs) > sunspec.M103_VAr_SF {
		m.Var = sunspec.ApplyScaleSigned(get(sunspec.M103_VAr), sf(sunspec.M103_VAr_SF))
	}
	if len(regs) > sunspec.M103_PF_SF {
		m.PF = sunspec.ApplyScaleSigned(get(sunspec.M103_PF), sf(sunspec.M103_PF_SF)) / 100.0
	}
	if len(regs) > sunspec.M103_DCW_SF {
		m.DCV = sunspec.ApplyScaleUint(get(sunspec.M103_DCV), sf(sunspec.M103_DCV_SF))
		m.DCW = sunspec.ApplyScaleSigned(get(sunspec.M103_DCW), sf(sunspec.M103_DCW_SF))
	}
	if len(regs) > sunspec.M103_Tmp_SF {
		m.TmpCab = sunspec.ApplyScaleSigned(get(sunspec.M103_TmpCab), sf(sunspec.M103_Tmp_SF))
	}
	return m
}

// ── ApplyControl ─────────────────────────────────────────────────────────────

// ApplyControl writes CSIP DERControlBase to the device using the best
// available model path. tag is for error messages.
func (b *Base) ApplyControl(ctrl model.DERControlBase, tag string) error {
	if ctrl.OpModEnergize != nil && b.Has703 {
		if err := b.SetEnterServiceBool(*ctrl.OpModEnergize, tag); err != nil {
			return err
		}
	}

	if ctrl.OpModConnect != nil {
		if !b.Reader.HasModel(sunspec.ModelImmediateCtrl) {
			return fmt.Errorf("%s: no M123 for connect control", tag)
		}
		if err := b.SetConnect(*ctrl.OpModConnect, tag); err != nil {
			return err
		}
	}

	if ctrl.OpModFixedPFInjectW != nil || ctrl.OpModFixedPFAbsorbW != nil {
		if b.Has704 {
			if err := b.SetPowerFactor704(ctrl, tag); err != nil {
				return err
			}
		}
	}

	if ctrl.OpModFixedVar != nil && b.Has704 {
		if err := b.SetConstantVar704(ctrl.OpModFixedVar, tag); err != nil {
			return err
		}
	}

	lim := ctrl.OpModExpLimW
	if lim == nil {
		lim = ctrl.OpModMaxLimW
	}
	if lim != nil {
		if b.Has704 {
			if err := b.SetWMaxLimPct704(lim, tag); err != nil {
				return err
			}
		} else {
			if err := b.SetExportLimit(lim, tag); err != nil {
				return err
			}
		}
	}

	// OpModImpLimW: import (charge) setpoint.
	// Uses signed negative WMaxLimPct so the battery sim charges in the right direction.
	if ctrl.OpModImpLimW != nil {
		if b.Has704 {
			if err := b.SetWMaxLimPct704(ctrl.OpModImpLimW, tag); err != nil {
				return err
			}
		} else {
			if err := b.SetImportLimit(ctrl.OpModImpLimW, tag); err != nil {
				return err
			}
		}
	}

	return nil
}

// ── M703 enter service ───────────────────────────────────────────────────────

// SetEnterService writes full M703 settings.
func (b *Base) SetEnterService(s sunspec.DEREnterServiceSettings, tag string) error {
	if !b.Has703 {
		return fmt.Errorf("%s: device has no M703 (DEREnterService)", tag)
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDEREnterService)
	if err != nil {
		return fmt.Errorf("%s: read M703: %w", tag, err)
	}
	if err := sunspec.EncodeDEREnterService(regs, s); err != nil {
		return err
	}
	return b.Reader.WriteModel(sunspec.ModelDEREnterService, 0, regs)
}

// SetEnterServiceBool is the internal path called from ApplyControl.
func (b *Base) SetEnterServiceBool(energize bool, tag string) error {
	regs, err := b.Reader.ReadModel(sunspec.ModelDEREnterService)
	if err != nil {
		return fmt.Errorf("%s: read M703: %w", tag, err)
	}
	if energize {
		regs[sunspec.M703_ES] = 1
	} else {
		regs[sunspec.M703_ES] = 0
	}
	return b.Reader.WriteModel(sunspec.ModelDEREnterService, 0, regs[:1])
}

// ReadEnterService reads M703 settings.
func (b *Base) ReadEnterService(tag string) (sunspec.DEREnterServiceSettings, error) {
	if !b.Has703 {
		return sunspec.DEREnterServiceSettings{}, fmt.Errorf("%s: device has no M703", tag)
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDEREnterService)
	if err != nil {
		return sunspec.DEREnterServiceSettings{}, fmt.Errorf("%s: read M703: %w", tag, err)
	}
	return sunspec.ParseDEREnterService(regs)
}

// ── M704 DERCtlAC helpers ────────────────────────────────────────────────────

// SetPowerFactor704 maps CSIP OpModFixedPF* to M704 PFWInj fields.
func (b *Base) SetPowerFactor704(ctrl model.DERControlBase, tag string) error {
	regs, err := b.Reader.ReadModel(sunspec.ModelDERCtlAC)
	if err != nil {
		return fmt.Errorf("%s: read M704: %w", tag, err)
	}
	s, err := sunspec.ParseDERCtlAC(regs)
	if err != nil {
		return err
	}
	if ctrl.OpModFixedPFInjectW != nil {
		s.PFWInjEna = true
		s.PFWInj_PF = math.Abs(float64(ctrl.OpModFixedPFInjectW.Value)) / 10000.0
		s.PFWInj_Ext = true
	} else if ctrl.OpModFixedPFAbsorbW != nil {
		s.PFWInjEna = true
		s.PFWInj_PF = math.Abs(float64(ctrl.OpModFixedPFAbsorbW.Value)) / 10000.0
		s.PFWInj_Ext = false
	}
	if err := sunspec.EncodeDERCtlAC(regs, s); err != nil {
		return err
	}
	return b.Reader.WriteModel(sunspec.ModelDERCtlAC, 0, regs)
}

// SetConstantVar704 maps CSIP OpModFixedVar to M704 VarSetPct.
func (b *Base) SetConstantVar704(fv *model.FixedVar, tag string) error {
	if fv == nil {
		return nil
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDERCtlAC)
	if err != nil {
		return fmt.Errorf("%s: read M704 for var: %w", tag, err)
	}
	s, err := sunspec.ParseDERCtlAC(regs)
	if err != nil {
		return err
	}
	s.VarSetEna = true
	s.VarSetPri = 1
	s.VarSetPct = float64(fv.Value.Value) / 100.0
	if err := sunspec.EncodeDERCtlAC(regs, s); err != nil {
		return err
	}
	return b.Reader.WriteModel(sunspec.ModelDERCtlAC, 0, regs)
}

// SetWMaxLimPct704 maps an active power limit to M704.WMaxLimPct.
func (b *Base) SetWMaxLimPct704(ap *model.ActivePower, tag string) error {
	if math.IsNaN(b.Wmax) || b.Wmax <= 0 {
		return fmt.Errorf("%s: cannot set power limit: WMax unknown", tag)
	}
	requestedW := float64(ap.Value) * math.Pow10(int(ap.Multiplier))
	if requestedW < 0 {
		requestedW = 0
	}
	if requestedW > b.Wmax {
		requestedW = b.Wmax
	}
	pct := (requestedW / b.Wmax) * 100.0

	regs, err := b.Reader.ReadModel(sunspec.ModelDERCtlAC)
	if err != nil {
		return fmt.Errorf("%s: read M704 for WMaxLimPct: %w", tag, err)
	}
	s, err := sunspec.ParseDERCtlAC(regs)
	if err != nil {
		return err
	}
	s.WMaxLimPctEna = true
	s.WMaxLimPct = pct
	if err := sunspec.EncodeDERCtlAC(regs, s); err != nil {
		return err
	}
	return b.Reader.WriteModel(sunspec.ModelDERCtlAC, 0, regs)
}

// SetDERCtlAC writes a complete M704 settings struct.
func (b *Base) SetDERCtlAC(s sunspec.DERCtlACSettings, tag string) error {
	if !b.Has704 {
		return fmt.Errorf("%s: device has no M704 (DERCtlAC)", tag)
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDERCtlAC)
	if err != nil {
		return fmt.Errorf("%s: read M704: %w", tag, err)
	}
	if err := sunspec.EncodeDERCtlAC(regs, s); err != nil {
		return err
	}
	return b.Reader.WriteModel(sunspec.ModelDERCtlAC, 0, regs)
}

// ReadDERCtlAC reads M704 settings.
func (b *Base) ReadDERCtlAC(tag string) (sunspec.DERCtlACSettings, error) {
	if !b.Has704 {
		return sunspec.DERCtlACSettings{}, fmt.Errorf("%s: device has no M704", tag)
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDERCtlAC)
	if err != nil {
		return sunspec.DERCtlACSettings{}, fmt.Errorf("%s: read M704: %w", tag, err)
	}
	return sunspec.ParseDERCtlAC(regs)
}

// ReadDERCapacity reads M702 nameplate data.
func (b *Base) ReadDERCapacity(tag string) (sunspec.DERCapacity, error) {
	if !b.Has702 {
		return sunspec.DERCapacity{}, fmt.Errorf("%s: device has no M702 (DERCapacity)", tag)
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDERCapacity)
	if err != nil {
		return sunspec.DERCapacity{}, fmt.Errorf("%s: read M702: %w", tag, err)
	}
	return sunspec.ParseDERCapacity(regs)
}

// ── Volt-Var (M705) ──────────────────────────────────────────────────────────

func (b *Base) ReadVoltVar(tag string) (sunspec.VoltVarCurve, error) {
	if !b.Has705 {
		return sunspec.VoltVarCurve{}, fmt.Errorf("%s: device has no M705 (DERVoltVar)", tag)
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDERVoltVar)
	if err != nil {
		return sunspec.VoltVarCurve{}, fmt.Errorf("%s: read M705: %w", tag, err)
	}
	return sunspec.ParseVoltVarCurve(regs, 0)
}

func (b *Base) WriteVoltVar(c sunspec.VoltVarCurve, tag string) error {
	if !b.Has705 {
		return fmt.Errorf("%s: device has no M705 (DERVoltVar)", tag)
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDERVoltVar)
	if err != nil {
		return fmt.Errorf("%s: read M705: %w", tag, err)
	}
	start, end, err := sunspec.EncodeVoltVarCurve(regs, c)
	if err != nil {
		return err
	}
	if err := b.Reader.WriteModel(sunspec.ModelDERVoltVar, uint16(start), regs[start:end]); err != nil {
		return fmt.Errorf("%s: write M705 curve: %w", tag, err)
	}
	return b.Reader.WriteModel(sunspec.ModelDERVoltVar, sunspec.M705_AdptCrvReq, []uint16{1})
}

// ── Volt-Watt (M706) ────────────────────────────────────────────────────────

func (b *Base) ReadVoltWatt(tag string) (sunspec.VoltWattCurve, error) {
	if !b.Has706 {
		return sunspec.VoltWattCurve{}, fmt.Errorf("%s: device has no M706 (DERVoltWatt)", tag)
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDERVoltWatt)
	if err != nil {
		return sunspec.VoltWattCurve{}, fmt.Errorf("%s: read M706: %w", tag, err)
	}
	return sunspec.ParseVoltWattCurve(regs, 0)
}

func (b *Base) WriteVoltWatt(c sunspec.VoltWattCurve, tag string) error {
	if !b.Has706 {
		return fmt.Errorf("%s: device has no M706 (DERVoltWatt)", tag)
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDERVoltWatt)
	if err != nil {
		return fmt.Errorf("%s: read M706: %w", tag, err)
	}
	start, end, err := sunspec.EncodeVoltWattCurve(regs, c)
	if err != nil {
		return err
	}
	if err := b.Reader.WriteModel(sunspec.ModelDERVoltWatt, uint16(start), regs[start:end]); err != nil {
		return fmt.Errorf("%s: write M706 curve: %w", tag, err)
	}
	return b.Reader.WriteModel(sunspec.ModelDERVoltWatt, sunspec.M706_AdptCrvReq, []uint16{1})
}

// ── Voltage trip (M707/M708) ────────────────────────────────────────────────

func (b *Base) ReadVoltageTripLV(tag string) (sunspec.VoltageTripCurve, error) {
	if !b.Has707 {
		return sunspec.VoltageTripCurve{}, fmt.Errorf("%s: device has no M707 (DERTripLV)", tag)
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDERTripLV)
	if err != nil {
		return sunspec.VoltageTripCurve{}, fmt.Errorf("%s: read M707: %w", tag, err)
	}
	return sunspec.ParseVoltageTripCurve(regs, 0, false)
}

func (b *Base) WriteVoltageTripLV(c sunspec.VoltageTripCurve, tag string) error {
	if !b.Has707 {
		return fmt.Errorf("%s: device has no M707 (DERTripLV)", tag)
	}
	return b.writeTripCurve707(sunspec.ModelDERTripLV, sunspec.M707_AdptCrvReq, c, tag)
}

func (b *Base) ReadVoltageTripHV(tag string) (sunspec.VoltageTripCurve, error) {
	if !b.Has708 {
		return sunspec.VoltageTripCurve{}, fmt.Errorf("%s: device has no M708 (DERTripHV)", tag)
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDERTripHV)
	if err != nil {
		return sunspec.VoltageTripCurve{}, fmt.Errorf("%s: read M708: %w", tag, err)
	}
	return sunspec.ParseVoltageTripCurve(regs, 0, false)
}

func (b *Base) WriteVoltageTripHV(c sunspec.VoltageTripCurve, tag string) error {
	if !b.Has708 {
		return fmt.Errorf("%s: device has no M708 (DERTripHV)", tag)
	}
	return b.writeTripCurve707(sunspec.ModelDERTripHV, sunspec.M707_AdptCrvReq, c, tag)
}

func (b *Base) writeTripCurve707(modelID, adptReqOffset uint16, c sunspec.VoltageTripCurve, tag string) error {
	regs, err := b.Reader.ReadModel(modelID)
	if err != nil {
		return fmt.Errorf("%s: read model %d: %w", tag, modelID, err)
	}
	start, end, err := sunspec.EncodeVoltageTripCurve(regs, c, false)
	if err != nil {
		return err
	}
	if err := b.Reader.WriteModel(modelID, uint16(start), regs[start:end]); err != nil {
		return fmt.Errorf("%s: write model %d: %w", tag, modelID, err)
	}
	return b.Reader.WriteModel(modelID, adptReqOffset, []uint16{1})
}

// ── Frequency trip (M709/M710) ───────────────────────────────────────────────

func (b *Base) ReadFreqTripLF(tag string) (sunspec.FreqTripCurve, error) {
	if !b.Has709 {
		return sunspec.FreqTripCurve{}, fmt.Errorf("%s: device has no M709 (DERTripLF)", tag)
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDERTripLF)
	if err != nil {
		return sunspec.FreqTripCurve{}, fmt.Errorf("%s: read M709: %w", tag, err)
	}
	return sunspec.ParseFreqTripCurve(regs, 0, false)
}

func (b *Base) WriteFreqTripLF(c sunspec.FreqTripCurve, tag string) error {
	if !b.Has709 {
		return fmt.Errorf("%s: device has no M709 (DERTripLF)", tag)
	}
	return b.writeTripCurve709(sunspec.ModelDERTripLF, c, tag)
}

func (b *Base) ReadFreqTripHF(tag string) (sunspec.FreqTripCurve, error) {
	if !b.Has710 {
		return sunspec.FreqTripCurve{}, fmt.Errorf("%s: device has no M710 (DERTripHF)", tag)
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDERTripHF)
	if err != nil {
		return sunspec.FreqTripCurve{}, fmt.Errorf("%s: read M710: %w", tag, err)
	}
	return sunspec.ParseFreqTripCurve(regs, 0, false)
}

func (b *Base) WriteFreqTripHF(c sunspec.FreqTripCurve, tag string) error {
	if !b.Has710 {
		return fmt.Errorf("%s: device has no M710 (DERTripHF)", tag)
	}
	return b.writeTripCurve709(sunspec.ModelDERTripHF, c, tag)
}

func (b *Base) writeTripCurve709(modelID uint16, c sunspec.FreqTripCurve, tag string) error {
	regs, err := b.Reader.ReadModel(modelID)
	if err != nil {
		return fmt.Errorf("%s: read model %d: %w", tag, modelID, err)
	}
	start, end, err := sunspec.EncodeFreqTripCurve(regs, c, false)
	if err != nil {
		return err
	}
	if err := b.Reader.WriteModel(modelID, uint16(start), regs[start:end]); err != nil {
		return fmt.Errorf("%s: write model %d: %w", tag, modelID, err)
	}
	return b.Reader.WriteModel(modelID, sunspec.M709_AdptCrvReq, []uint16{1})
}

// ── Frequency droop (M711) ───────────────────────────────────────────────────

func (b *Base) ReadFreqDroop(tag string) (sunspec.FreqDroopCtl, error) {
	if !b.Has711 {
		return sunspec.FreqDroopCtl{}, fmt.Errorf("%s: device has no M711 (DERFreqDroop)", tag)
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDERFreqDroop)
	if err != nil {
		return sunspec.FreqDroopCtl{}, fmt.Errorf("%s: read M711: %w", tag, err)
	}
	return sunspec.ParseFreqDroop(regs, 0)
}

func (b *Base) WriteFreqDroop(c sunspec.FreqDroopCtl, tag string) error {
	if !b.Has711 {
		return fmt.Errorf("%s: device has no M711 (DERFreqDroop)", tag)
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDERFreqDroop)
	if err != nil {
		return fmt.Errorf("%s: read M711: %w", tag, err)
	}
	start, end, err := sunspec.EncodeFreqDroop(regs, c)
	if err != nil {
		return err
	}
	if err := b.Reader.WriteModel(sunspec.ModelDERFreqDroop, uint16(start), regs[start:end]); err != nil {
		return fmt.Errorf("%s: write M711: %w", tag, err)
	}
	return b.Reader.WriteModel(sunspec.ModelDERFreqDroop, sunspec.M711_AdptCtlReq, []uint16{1})
}

// ── Watt-Var (M712) ──────────────────────────────────────────────────────────

func (b *Base) ReadWattVar(tag string) (sunspec.WattVarCurve, error) {
	if !b.Has712 {
		return sunspec.WattVarCurve{}, fmt.Errorf("%s: device has no M712 (DERWattVar)", tag)
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDERWattVar)
	if err != nil {
		return sunspec.WattVarCurve{}, fmt.Errorf("%s: read M712: %w", tag, err)
	}
	return sunspec.ParseWattVarCurve(regs, 0)
}

func (b *Base) WriteWattVar(c sunspec.WattVarCurve, tag string) error {
	if !b.Has712 {
		return fmt.Errorf("%s: device has no M712 (DERWattVar)", tag)
	}
	if len(c.Pts) != 6 {
		return fmt.Errorf("%s: WattVar curve must have 6 points per IEEE 1547-2018 (got %d)", tag, len(c.Pts))
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDERWattVar)
	if err != nil {
		return fmt.Errorf("%s: read M712: %w", tag, err)
	}
	start, end, err := sunspec.EncodeWattVarCurve(regs, c)
	if err != nil {
		return err
	}
	if err := b.Reader.WriteModel(sunspec.ModelDERWattVar, uint16(start), regs[start:end]); err != nil {
		return fmt.Errorf("%s: write M712: %w", tag, err)
	}
	return b.Reader.WriteModel(sunspec.ModelDERWattVar, sunspec.M712_AdptCrvReq, []uint16{1})
}

// ── Legacy M123 helpers ──────────────────────────────────────────────────────

// SetConnect writes to Model 123 Conn: 1=connect, 0=disconnect.
func (b *Base) SetConnect(connect bool, tag string) error {
	val := uint16(0)
	if connect {
		val = 1
	}
	if err := b.Reader.WriteModel(sunspec.ModelImmediateCtrl, sunspec.M123_Conn, []uint16{val}); err != nil {
		return fmt.Errorf("%s: set connect=%v: %w", tag, connect, err)
	}
	return nil
}

// SetImportLimit writes a signed negative WMaxLimPct to M123, commanding the
// device to absorb power at the requested rate.  This uses a sign convention
// supported by the battery simulator: negative pct = charge direction.
func (b *Base) SetImportLimit(ap *model.ActivePower, tag string) error {
	if math.IsNaN(b.Wmax) || b.Wmax <= 0 {
		return fmt.Errorf("%s: cannot set import limit: WMax unknown", tag)
	}
	requestedW := float64(ap.Value) * math.Pow10(int(ap.Multiplier))
	if requestedW < 0 {
		requestedW = 0
	}
	if requestedW > b.Wmax {
		requestedW = b.Wmax
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelImmediateCtrl)
	if err != nil {
		return fmt.Errorf("%s: read Model 123 for SetImportLimit: %w", tag, err)
	}
	if len(regs) <= sunspec.M123_WMaxLimPct_SF {
		return fmt.Errorf("%s: Model 123 too short", tag)
	}
	sf := int16(regs[sunspec.M123_WMaxLimPct_SF])
	pct := -(requestedW / b.Wmax) * 100.0 // negative = charge direction
	raw := sunspec.RawFromScaleSigned(pct, sf)
	if err := b.Reader.WriteModel(sunspec.ModelImmediateCtrl, sunspec.M123_WMaxLimPct_RmpTms, []uint16{5}); err != nil {
		return fmt.Errorf("%s: set ramp time: %w", tag, err)
	}
	if err := b.Reader.WriteModel(sunspec.ModelImmediateCtrl, sunspec.M123_WMaxLimPct, []uint16{raw}); err != nil {
		return fmt.Errorf("%s: write WMaxLimPct (import): %w", tag, err)
	}
	if err := b.Reader.WriteModel(sunspec.ModelImmediateCtrl, sunspec.M123_WMaxLimPct_Ena, []uint16{1}); err != nil {
		return fmt.Errorf("%s: enable WMaxLimPct: %w", tag, err)
	}
	return nil
}

// SetExportLimit converts watts to WMaxLimPct and writes to M123.
func (b *Base) SetExportLimit(ap *model.ActivePower, tag string) error {
	if math.IsNaN(b.Wmax) || b.Wmax <= 0 {
		return fmt.Errorf("%s: cannot set export limit: WMax unknown (Model 121 absent or zero)", tag)
	}
	requestedW := float64(ap.Value) * math.Pow10(int(ap.Multiplier))
	if requestedW < 0 {
		requestedW = 0
	}
	if requestedW > b.Wmax {
		requestedW = b.Wmax
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelImmediateCtrl)
	if err != nil {
		return fmt.Errorf("%s: read Model 123 for WMaxLimPct_SF: %w", tag, err)
	}
	if len(regs) <= sunspec.M123_WMaxLimPct_SF {
		return fmt.Errorf("%s: Model 123 too short for WMaxLimPct_SF", tag)
	}
	sf := int16(regs[sunspec.M123_WMaxLimPct_SF])
	pct := (requestedW / b.Wmax) * 100.0
	raw := sunspec.RawFromScaleUint(pct, sf)
	if err := b.Reader.WriteModel(sunspec.ModelImmediateCtrl, sunspec.M123_WMaxLimPct_RmpTms, []uint16{5}); err != nil {
		return fmt.Errorf("%s: set ramp time: %w", tag, err)
	}
	if err := b.Reader.WriteModel(sunspec.ModelImmediateCtrl, sunspec.M123_WMaxLimPct, []uint16{raw}); err != nil {
		return fmt.Errorf("%s: write WMaxLimPct: %w", tag, err)
	}
	if err := b.Reader.WriteModel(sunspec.ModelImmediateCtrl, sunspec.M123_WMaxLimPct_Ena, []uint16{1}); err != nil {
		return fmt.Errorf("%s: enable WMaxLimPct: %w", tag, err)
	}
	return nil
}

// ── WMax helpers ─────────────────────────────────────────────────────────────

func ReadWMax(r *sunspec.Reader) (float64, error) {
	regs, err := r.ReadModel(sunspec.ModelBasicSettings)
	if err != nil {
		return 0, err
	}
	if len(regs) <= sunspec.M121_WMax_SF {
		return 0, fmt.Errorf("sunspec: Model 121 too short for WMax_SF")
	}
	sf := int16(regs[sunspec.M121_WMax_SF])
	wmax := sunspec.ApplyScaleUint(regs[sunspec.M121_WMax], sf)
	if wmax <= 0 || math.IsNaN(wmax) {
		return 0, fmt.Errorf("sunspec: Model 121 WMax is %g (invalid)", wmax)
	}
	return wmax, nil
}

func ReadWMaxFrom702(r *sunspec.Reader) (float64, error) {
	regs, err := r.ReadModel(sunspec.ModelDERCapacity)
	if err != nil {
		return 0, err
	}
	if len(regs) <= sunspec.M702_W_SF {
		return 0, fmt.Errorf("sunspec: Model 702 too short for W_SF")
	}
	sf := int16(regs[sunspec.M702_W_SF])
	wmax := sunspec.ApplyScaleUint(regs[sunspec.M702_WMaxRtg], sf)
	if wmax <= 0 || math.IsNaN(wmax) {
		return 0, fmt.Errorf("sunspec: Model 702 WMaxRtg is %g (invalid)", wmax)
	}
	return wmax, nil
}
