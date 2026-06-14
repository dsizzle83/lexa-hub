// Package derbase provides shared SunSpec DER device logic for the inverter and
// battery packages: IEEE 1547-2018 models (701-714), legacy models
// (103/121/123/802), measurement parsing, and — its main job — translation of
// IEEE 2030.5 / CSIP DERControlBase operating modes into SunSpec Modbus writes.
//
// CSIP → SunSpec mapping (model 704 unless noted):
//
//	opModEnergize        → 703 ES (enter service / cease-to-energize)
//	opModConnect         → 123 Conn (legacy immediate connect/disconnect)
//	opModFixedPFInjectW  → 704 PFWInj{PF,Ext}  (constant PF while injecting W)
//	opModFixedPFAbsorbW  → 704 PFWAbs{PF,Ext}  (constant PF while absorbing W)
//	opModFixedVar        → 704 VarSet{Mod,Pri,Pct}
//	opModFixedW          → 704 WSet (Set Active Power, watts) — setpoint
//	opModMaxLimW/ExpLimW/GenLimW → 704 WMaxLimPct (% of WMax) — ceiling
//	opModImpLimW/LoadLimW → 704 WSet negative (charge), or legacy 123
//
// Curve modes (opModVoltVar / opModVoltWatt / ride-through / freq-droop) are
// applied through the typed curve writers, which follow the §3.1.2 adopt
// workflow: write the staging curve, request adoption (AdptCrvReq>1), poll
// AdptCrvRslt, then enable the function (Ena=1) per §3.3.
package derbase

import (
	"fmt"
	"math"
	"time"

	"lexa-hub/internal/northbound/model"
	"lexa-hub/internal/southbound/device"
	"lexa-hub/internal/southbound/sunspec"
)

// Base holds shared SunSpec DER state and methods. Embed in concrete types.
type Base struct {
	Reader    *sunspec.Reader
	Wmax      float64 // nameplate WMax in watts; NaN if unavailable
	MeasModel uint16  // measurement model: 701, 103, 102, or 101

	// DefaultRvrtTms, when non-zero, is written as the reversion timeout (seconds)
	// on 704 control writes so the DER auto-reverts if communication is lost. The
	// orchestrator may set this from the active control's remaining duration.
	DefaultRvrtTms uint32

	// AdoptPollTimeout bounds how long curve writers wait for AdptCrvRslt to
	// report COMPLETED before proceeding. Zero uses adoptPollDefault.
	AdoptPollTimeout time.Duration

	Has701, Has702, Has703, Has704, Has705, Has706 bool
	Has707, Has708, Has709, Has710, Has711, Has712 bool
	Has713, Has714                                 bool
}

const adoptPollDefault = 3 * time.Second
const adoptPollInterval = 100 * time.Millisecond

// Init populates model-presence flags, selects the measurement model, and reads
// WMax. tag is used in error messages (e.g. "inverter", "battery").
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
		Has713: r.HasModel(sunspec.ModelDERStorageCap),
		Has714: r.HasModel(sunspec.ModelDERMeasureDC),
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

// ReadMeasurementsM701 parses model 701 into device.Measurements.
func ReadMeasurementsM701(regs []uint16) device.Measurements {
	m := sunspec.Parse701(regs)
	v := m.LNV
	if math.IsNaN(v) {
		v = m.VL1
	}
	return device.Measurements{W: m.W, V: v, Hz: m.Hz, VA: m.VA, Var: m.Var, PF: m.PF, TmpCab: m.TmpCab, SOC: math.NaN()}
}

// ReadMeasurementsACModel parses legacy Model 10x (101/102/103).
func ReadMeasurementsACModel(regs []uint16) device.Measurements {
	get := func(off int) uint16 {
		if off < len(regs) {
			return regs[off]
		}
		return 0
	}
	sf := func(off int) int16 { return int16(get(off)) }
	m := device.Measurements{TmpCab: math.NaN(), SOC: math.NaN()}
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

func watts(ap *model.ActivePower) float64 {
	return float64(ap.Value) * math.Pow10(int(ap.Multiplier))
}

// ── ApplyControl: CSIP DERControlBase → SunSpec ──────────────────────────────

func (b *Base) ApplyControl(ctrl model.DERControlBase, tag string) error {
	if ctrl.OpModEnergize != nil && b.Has703 {
		if err := b.SetEnterServiceEnabled(*ctrl.OpModEnergize, tag); err != nil {
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
	if ctrl.OpModFixedPFInjectW != nil && b.Has704 {
		pf := math.Abs(float64(ctrl.OpModFixedPFInjectW.Value)) / 10000.0
		if err := b.SetFixedPF(true, pf, ctrl.OpModFixedPFInjectW.Value >= 0, tag); err != nil {
			return err
		}
	}
	if ctrl.OpModFixedPFAbsorbW != nil && b.Has704 {
		pf := math.Abs(float64(ctrl.OpModFixedPFAbsorbW.Value)) / 10000.0
		if err := b.SetFixedPF(false, pf, ctrl.OpModFixedPFAbsorbW.Value >= 0, tag); err != nil {
			return err
		}
	}
	if ctrl.OpModFixedVar != nil && b.Has704 {
		if err := b.SetConstantVar(float64(ctrl.OpModFixedVar.Value.Value)/100.0, tag); err != nil {
			return err
		}
	}
	if ctrl.OpModFixedW != nil && b.Has704 {
		if err := b.SetActivePowerWatts(watts(ctrl.OpModFixedW), tag); err != nil {
			return err
		}
	}

	// Ceilings → WMaxLimPct (% of WMax). First non-nil wins.
	if lim := firstNonNil(ctrl.OpModExpLimW, ctrl.OpModMaxLimW, ctrl.OpModGenLimW); lim != nil {
		if b.Has704 {
			if err := b.SetWMaxLimPctW(watts(lim), tag); err != nil {
				return err
			}
		} else if err := b.SetExportLimit(lim, tag); err != nil {
			return err
		}
	}

	// Import / load (charge) → negative Set Active Power, or legacy 123.
	if imp := firstNonNil(ctrl.OpModImpLimW, ctrl.OpModLoadLimW); imp != nil {
		if b.Has704 {
			if err := b.SetActivePowerWatts(-watts(imp), tag); err != nil {
				return err
			}
		} else if err := b.SetImportLimit(imp, tag); err != nil {
			return err
		}
	}
	return nil
}

func firstNonNil(aps ...*model.ActivePower) *model.ActivePower {
	for _, ap := range aps {
		if ap != nil {
			return ap
		}
	}
	return nil
}

// ── Model 703: Enter Service ─────────────────────────────────────────────────

func (b *Base) SetEnterService(s sunspec.EnterService, tag string) error {
	if !b.Has703 {
		return fmt.Errorf("%s: device has no M703 (DEREnterService)", tag)
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDEREnterService)
	if err != nil {
		return fmt.Errorf("%s: read M703: %w", tag, err)
	}
	if err := sunspec.Encode703(regs, s); err != nil {
		return err
	}
	return b.Reader.WriteModel(sunspec.ModelDEREnterService, 0, regs[:sunspec.L703.Len()])
}

// SetEnterServiceEnabled toggles only the ES permit-service / cease-to-energize bit.
func (b *Base) SetEnterServiceEnabled(energize bool, tag string) error {
	val := uint16(0)
	if energize {
		val = 1
	}
	return b.Reader.WriteModel(sunspec.ModelDEREnterService, uint16(sunspec.L703.Offset("ES")), []uint16{val})
}

func (b *Base) ReadEnterService(tag string) (sunspec.EnterService, error) {
	if !b.Has703 {
		return sunspec.EnterService{}, fmt.Errorf("%s: device has no M703", tag)
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDEREnterService)
	if err != nil {
		return sunspec.EnterService{}, fmt.Errorf("%s: read M703: %w", tag, err)
	}
	return sunspec.Parse703(regs), nil
}

// ── Model 704: AC Controls ───────────────────────────────────────────────────

// write704 read-modify-writes the whole 704 block after applying fn to its View.
// Writing the entire model keeps PF sync groups (PF+Ext) atomic per the spec.
func (b *Base) write704(tag string, fn func(v sunspec.View)) error {
	if !b.Has704 {
		return fmt.Errorf("%s: device has no M704 (DERCtlAC)", tag)
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDERCtlAC)
	if err != nil {
		return fmt.Errorf("%s: read M704: %w", tag, err)
	}
	fn(sunspec.L704.View(regs))
	return b.Reader.WriteModel(sunspec.ModelDERCtlAC, 0, regs[:sunspec.L704.Len()])
}

// SetFixedPF enables constant power factor. inject selects the PFWInj (injecting
// active power) vs PFWAbs (absorbing) sync group; overExcited sets excitation.
func (b *Base) SetFixedPF(inject bool, pf float64, overExcited bool, tag string) error {
	return b.write704(tag, func(v sunspec.View) {
		ext := uint16(sunspec.M704_Ext_OverExcited)
		if !overExcited {
			ext = sunspec.M704_Ext_UnderExcited
		}
		if inject {
			v.SetBool("PFWInjEna", true)
			v.SetFloat("PFWInj_PF", pf) // engineering value = power factor
			v.SetEnum("PFWInj_Ext", ext)
		} else {
			v.SetBool("PFWAbsEna", true)
			v.SetFloat("PFWAbs_PF", pf)
			v.SetEnum("PFWAbs_Ext", ext)
		}
	})
}

// SetConstantVar enables constant reactive power as a percentage (signed: + inject).
func (b *Base) SetConstantVar(pct float64, tag string) error {
	return b.write704(tag, func(v sunspec.View) {
		v.SetBool("VarSetEna", true)
		v.SetEnum("VarSetMod", sunspec.M704_VarSetMod_VarMaxPct)
		v.SetEnum("VarSetPri", sunspec.M704_VarSetPri_Reactive)
		v.SetFloat("VarSetPct", pct)
	})
}

// SetActivePowerWatts sets the absolute active-power setpoint in watts (WSet,
// signed: + discharge/export, − charge/import). Clamped to ±WMax when known.
func (b *Base) SetActivePowerWatts(w float64, tag string) error {
	if !math.IsNaN(b.Wmax) && b.Wmax > 0 {
		if w > b.Wmax {
			w = b.Wmax
		}
		if w < -b.Wmax {
			w = -b.Wmax
		}
	}
	return b.write704(tag, func(v sunspec.View) {
		v.SetBool("WSetEna", true)
		v.SetEnum("WSetMod", sunspec.M704_WSetMod_Watts)
		v.SetFloat("WSet", w)
		v.SetU32("WSetRvrtTms", b.DefaultRvrtTms)
	})
}

// SetWMaxLimPctW sets the active-power ceiling as a percentage of WMax.
func (b *Base) SetWMaxLimPctW(w float64, tag string) error {
	if math.IsNaN(b.Wmax) || b.Wmax <= 0 {
		return fmt.Errorf("%s: cannot set power limit: WMax unknown", tag)
	}
	if w < 0 {
		w = 0
	}
	if w > b.Wmax {
		w = b.Wmax
	}
	pct := w / b.Wmax * 100.0
	return b.write704(tag, func(v sunspec.View) {
		v.SetBool("WMaxLimPctEna", true)
		v.SetFloat("WMaxLimPct", pct)
		v.SetU32("WMaxLimPctRvrtTms", b.DefaultRvrtTms)
	})
}

func (b *Base) ReadDERCtlAC(tag string) (sunspec.ACControls, error) {
	if !b.Has704 {
		return sunspec.ACControls{}, fmt.Errorf("%s: device has no M704", tag)
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDERCtlAC)
	if err != nil {
		return sunspec.ACControls{}, fmt.Errorf("%s: read M704: %w", tag, err)
	}
	return sunspec.Parse704(regs), nil
}

// ── Model 702: Capacity ──────────────────────────────────────────────────────

func (b *Base) ReadDERCapacity(tag string) (sunspec.Capacity, error) {
	if !b.Has702 {
		return sunspec.Capacity{}, fmt.Errorf("%s: device has no M702 (DERCapacity)", tag)
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDERCapacity)
	if err != nil {
		return sunspec.Capacity{}, fmt.Errorf("%s: read M702: %w", tag, err)
	}
	return sunspec.Parse702(regs), nil
}

// SetCapacityWMax overrides the nameplate active-power rating with an operator
// setting (702 WMax). Demonstrates the writable rating-override path (§4.2).
func (b *Base) SetCapacityWMax(w float64, tag string) error {
	if !b.Has702 {
		return fmt.Errorf("%s: device has no M702", tag)
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDERCapacity)
	if err != nil {
		return fmt.Errorf("%s: read M702: %w", tag, err)
	}
	sunspec.L702.View(regs).SetFloat("WMax", w)
	return b.Reader.WriteModel(sunspec.ModelDERCapacity, 0, regs[:sunspec.L702.Len()])
}

// ── Curve adopt workflow (§3.1.2 / §3.3) ─────────────────────────────────────

// adoptCurve performs the full SunSpec curve-update handshake on a curve/control
// model: the staging curve has already been encoded into regs[start:end] at
// 0-based index 1. It writes that range, requests adoption with the 1-based
// staging index (=2, which the spec requires to be >1), polls the result point
// until COMPLETED/FAILED, and finally enables the function (Ena=1).
func (b *Base) adoptCurve(modelID uint16, regs []uint16, start, end int, adptReqField, adptRsltField, enaField string, hdr *sunspec.Layout, tag string) error {
	if err := b.Reader.WriteModel(modelID, uint16(start), regs[start:end]); err != nil {
		return fmt.Errorf("%s: write model %d curve: %w", tag, modelID, err)
	}
	const stagingIdx1Based = 2 // 0-based index 1 → 1-based 2 (>1 per §3.1.2)
	if err := b.Reader.WriteModel(modelID, uint16(hdr.Offset(adptReqField)), []uint16{stagingIdx1Based}); err != nil {
		return fmt.Errorf("%s: request adopt on model %d: %w", tag, modelID, err)
	}
	if err := b.pollAdoptResult(modelID, hdr.Offset(adptRsltField), tag); err != nil {
		return err
	}
	if err := b.Reader.WriteModel(modelID, uint16(hdr.Offset(enaField)), []uint16{1}); err != nil {
		return fmt.Errorf("%s: enable model %d: %w", tag, modelID, err)
	}
	return nil
}

func (b *Base) pollAdoptResult(modelID uint16, rsltOffset int, tag string) error {
	timeout := b.AdoptPollTimeout
	if timeout == 0 {
		timeout = adoptPollDefault
	}
	deadline := time.Now().Add(timeout)
	for {
		regs, err := b.Reader.ReadModel(modelID)
		if err != nil {
			return fmt.Errorf("%s: poll adopt result on model %d: %w", tag, modelID, err)
		}
		if rsltOffset >= 0 && rsltOffset < len(regs) {
			switch regs[rsltOffset] {
			case sunspec.AdptCompleted:
				return nil
			case sunspec.AdptFailed:
				return fmt.Errorf("%s: model %d adopt-curve FAILED", tag, modelID)
			}
		}
		if time.Now().After(deadline) {
			return nil // best-effort: device may not update the result point
		}
		time.Sleep(adoptPollInterval)
	}
}

// ── Volt-Var (705) ───────────────────────────────────────────────────────────

func (b *Base) ReadVoltVar(tag string) (sunspec.VoltVarCurve, error) {
	if !b.Has705 {
		return sunspec.VoltVarCurve{}, fmt.Errorf("%s: device has no M705 (DERVoltVar)", tag)
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDERVoltVar)
	if err != nil {
		return sunspec.VoltVarCurve{}, fmt.Errorf("%s: read M705: %w", tag, err)
	}
	return sunspec.Parse705Curve(regs, 0)
}

func (b *Base) WriteVoltVar(c sunspec.VoltVarCurve, tag string) error {
	if !b.Has705 {
		return fmt.Errorf("%s: device has no M705 (DERVoltVar)", tag)
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDERVoltVar)
	if err != nil {
		return fmt.Errorf("%s: read M705: %w", tag, err)
	}
	start, end, err := sunspec.Encode705Curve(regs, 1, c)
	if err != nil {
		return err
	}
	return b.adoptCurve(sunspec.ModelDERVoltVar, regs, start, end, "AdptCrvReq", "AdptCrvRslt", "Ena", sunspec.L705Hdr, tag)
}

// ── Volt-Watt (706) ─────────────────────────────────────────────────────────

func (b *Base) ReadVoltWatt(tag string) (sunspec.VoltWattCurve, error) {
	if !b.Has706 {
		return sunspec.VoltWattCurve{}, fmt.Errorf("%s: device has no M706 (DERVoltWatt)", tag)
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDERVoltWatt)
	if err != nil {
		return sunspec.VoltWattCurve{}, fmt.Errorf("%s: read M706: %w", tag, err)
	}
	return sunspec.Parse706Curve(regs, 0)
}

func (b *Base) WriteVoltWatt(c sunspec.VoltWattCurve, tag string) error {
	if !b.Has706 {
		return fmt.Errorf("%s: device has no M706 (DERVoltWatt)", tag)
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDERVoltWatt)
	if err != nil {
		return fmt.Errorf("%s: read M706: %w", tag, err)
	}
	start, end, err := sunspec.Encode706Curve(regs, 1, c)
	if err != nil {
		return err
	}
	return b.adoptCurve(sunspec.ModelDERVoltWatt, regs, start, end, "AdptCrvReq", "AdptCrvRslt", "Ena", sunspec.L706Hdr, tag)
}

// ── Voltage trip (707/708) ──────────────────────────────────────────────────

func (b *Base) ReadVoltageTripLV(tag string) (sunspec.VoltageTripSet, error) {
	return b.readVoltageTrip(sunspec.ModelDERTripLV, b.Has707, "M707", tag)
}
func (b *Base) WriteVoltageTripLV(c sunspec.VoltageTripSet, tag string) error {
	return b.writeVoltageTrip(sunspec.ModelDERTripLV, b.Has707, "M707", c, tag)
}
func (b *Base) ReadVoltageTripHV(tag string) (sunspec.VoltageTripSet, error) {
	return b.readVoltageTrip(sunspec.ModelDERTripHV, b.Has708, "M708", tag)
}
func (b *Base) WriteVoltageTripHV(c sunspec.VoltageTripSet, tag string) error {
	return b.writeVoltageTrip(sunspec.ModelDERTripHV, b.Has708, "M708", c, tag)
}

func (b *Base) readVoltageTrip(modelID uint16, has bool, name, tag string) (sunspec.VoltageTripSet, error) {
	if !has {
		return sunspec.VoltageTripSet{}, fmt.Errorf("%s: device has no %s", tag, name)
	}
	regs, err := b.Reader.ReadModel(modelID)
	if err != nil {
		return sunspec.VoltageTripSet{}, fmt.Errorf("%s: read %s: %w", tag, name, err)
	}
	return sunspec.Parse707Set(regs, 0)
}

func (b *Base) writeVoltageTrip(modelID uint16, has bool, name string, c sunspec.VoltageTripSet, tag string) error {
	if !has {
		return fmt.Errorf("%s: device has no %s", tag, name)
	}
	regs, err := b.Reader.ReadModel(modelID)
	if err != nil {
		return fmt.Errorf("%s: read %s: %w", tag, name, err)
	}
	start, end, err := sunspec.Encode707Set(regs, 1, c)
	if err != nil {
		return err
	}
	return b.adoptCurve(modelID, regs, start, end, "AdptCrvReq", "AdptCrvRslt", "Ena", sunspec.L707Hdr, tag)
}

// ── Frequency trip (709/710) ─────────────────────────────────────────────────

func (b *Base) ReadFreqTripLF(tag string) (sunspec.FreqTripSet, error) {
	return b.readFreqTrip(sunspec.ModelDERTripLF, b.Has709, "M709", tag)
}
func (b *Base) WriteFreqTripLF(c sunspec.FreqTripSet, tag string) error {
	return b.writeFreqTrip(sunspec.ModelDERTripLF, b.Has709, "M709", c, tag)
}
func (b *Base) ReadFreqTripHF(tag string) (sunspec.FreqTripSet, error) {
	return b.readFreqTrip(sunspec.ModelDERTripHF, b.Has710, "M710", tag)
}
func (b *Base) WriteFreqTripHF(c sunspec.FreqTripSet, tag string) error {
	return b.writeFreqTrip(sunspec.ModelDERTripHF, b.Has710, "M710", c, tag)
}

func (b *Base) readFreqTrip(modelID uint16, has bool, name, tag string) (sunspec.FreqTripSet, error) {
	if !has {
		return sunspec.FreqTripSet{}, fmt.Errorf("%s: device has no %s", tag, name)
	}
	regs, err := b.Reader.ReadModel(modelID)
	if err != nil {
		return sunspec.FreqTripSet{}, fmt.Errorf("%s: read %s: %w", tag, name, err)
	}
	return sunspec.Parse709Set(regs, 0)
}

func (b *Base) writeFreqTrip(modelID uint16, has bool, name string, c sunspec.FreqTripSet, tag string) error {
	if !has {
		return fmt.Errorf("%s: device has no %s", tag, name)
	}
	regs, err := b.Reader.ReadModel(modelID)
	if err != nil {
		return fmt.Errorf("%s: read %s: %w", tag, name, err)
	}
	start, end, err := sunspec.Encode709Set(regs, 1, c)
	if err != nil {
		return err
	}
	return b.adoptCurve(modelID, regs, start, end, "AdptCrvReq", "AdptCrvRslt", "Ena", sunspec.L709Hdr, tag)
}

// ── Frequency droop (711) ────────────────────────────────────────────────────

func (b *Base) ReadFreqDroop(tag string) (sunspec.FreqDroopCtl, error) {
	if !b.Has711 {
		return sunspec.FreqDroopCtl{}, fmt.Errorf("%s: device has no M711 (DERFreqDroop)", tag)
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDERFreqDroop)
	if err != nil {
		return sunspec.FreqDroopCtl{}, fmt.Errorf("%s: read M711: %w", tag, err)
	}
	return sunspec.Parse711Ctl(regs, 0)
}

func (b *Base) WriteFreqDroop(c sunspec.FreqDroopCtl, tag string) error {
	if !b.Has711 {
		return fmt.Errorf("%s: device has no M711 (DERFreqDroop)", tag)
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDERFreqDroop)
	if err != nil {
		return fmt.Errorf("%s: read M711: %w", tag, err)
	}
	start, end, err := sunspec.Encode711Ctl(regs, 1, c)
	if err != nil {
		return err
	}
	return b.adoptCurve(sunspec.ModelDERFreqDroop, regs, start, end, "AdptCtlReq", "AdptCtlRslt", "Ena", sunspec.L711Hdr, tag)
}

// ── Watt-Var (712) ───────────────────────────────────────────────────────────

func (b *Base) ReadWattVar(tag string) (sunspec.WattVarCurve, error) {
	if !b.Has712 {
		return sunspec.WattVarCurve{}, fmt.Errorf("%s: device has no M712 (DERWattVar)", tag)
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDERWattVar)
	if err != nil {
		return sunspec.WattVarCurve{}, fmt.Errorf("%s: read M712: %w", tag, err)
	}
	return sunspec.Parse712Curve(regs, 0)
}

func (b *Base) WriteWattVar(c sunspec.WattVarCurve, tag string) error {
	if !b.Has712 {
		return fmt.Errorf("%s: device has no M712 (DERWattVar)", tag)
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDERWattVar)
	if err != nil {
		return fmt.Errorf("%s: read M712: %w", tag, err)
	}
	start, end, err := sunspec.Encode712Curve(regs, 1, c)
	if err != nil {
		return err
	}
	return b.adoptCurve(sunspec.ModelDERWattVar, regs, start, end, "AdptCrvReq", "AdptCrvRslt", "Ena", sunspec.L712Hdr, tag)
}

// ── Model 713/714 reads ──────────────────────────────────────────────────────

func (b *Base) ReadStorageCapacity(tag string) (sunspec.StorageCapacity, error) {
	if !b.Has713 {
		return sunspec.StorageCapacity{}, fmt.Errorf("%s: device has no M713 (DERStorageCapacity)", tag)
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDERStorageCap)
	if err != nil {
		return sunspec.StorageCapacity{}, fmt.Errorf("%s: read M713: %w", tag, err)
	}
	return sunspec.Parse713(regs), nil
}

func (b *Base) ReadDCMeasurement(tag string) (sunspec.DCMeasurement, error) {
	if !b.Has714 {
		return sunspec.DCMeasurement{}, fmt.Errorf("%s: device has no M714 (DERMeasureDC)", tag)
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelDERMeasureDC)
	if err != nil {
		return sunspec.DCMeasurement{}, fmt.Errorf("%s: read M714: %w", tag, err)
	}
	return sunspec.Parse714(regs)
}

// ── Legacy M123 / M121 helpers ───────────────────────────────────────────────

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

func (b *Base) SetImportLimit(ap *model.ActivePower, tag string) error {
	return b.setLegacyWMaxLimPct(-watts(ap), tag)
}

func (b *Base) SetExportLimit(ap *model.ActivePower, tag string) error {
	return b.setLegacyWMaxLimPct(watts(ap), tag)
}

// setLegacyWMaxLimPct writes M123 WMaxLimPct. Negative w commands charge
// (battery sim convention); positive w limits export.
func (b *Base) setLegacyWMaxLimPct(w float64, tag string) error {
	if math.IsNaN(b.Wmax) || b.Wmax <= 0 {
		return fmt.Errorf("%s: cannot set power limit: WMax unknown", tag)
	}
	charge := w < 0
	mag := math.Abs(w)
	if mag > b.Wmax {
		mag = b.Wmax
	}
	regs, err := b.Reader.ReadModel(sunspec.ModelImmediateCtrl)
	if err != nil {
		return fmt.Errorf("%s: read Model 123: %w", tag, err)
	}
	if len(regs) <= sunspec.M123_WMaxLimPct_SF {
		return fmt.Errorf("%s: Model 123 too short", tag)
	}
	sf := int16(regs[sunspec.M123_WMaxLimPct_SF])
	pct := mag / b.Wmax * 100.0
	var raw uint16
	if charge {
		raw = sunspec.RawFromScaleSigned(-pct, sf)
	} else {
		raw = sunspec.RawFromScaleUint(pct, sf)
	}
	if err := b.Reader.WriteModel(sunspec.ModelImmediateCtrl, sunspec.M123_WMaxLimPct_RmpTms, []uint16{5}); err != nil {
		return fmt.Errorf("%s: set ramp time: %w", tag, err)
	}
	if err := b.Reader.WriteModel(sunspec.ModelImmediateCtrl, sunspec.M123_WMaxLimPct, []uint16{raw}); err != nil {
		return fmt.Errorf("%s: write WMaxLimPct: %w", tag, err)
	}
	return b.Reader.WriteModel(sunspec.ModelImmediateCtrl, sunspec.M123_WMaxLimPct_Ena, []uint16{1})
}

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
	wmax := sunspec.L702.View(regs).Float("WMaxRtg")
	if wmax <= 0 || math.IsNaN(wmax) {
		return 0, fmt.Errorf("sunspec: Model 702 WMaxRtg is %g (invalid)", wmax)
	}
	return wmax, nil
}
