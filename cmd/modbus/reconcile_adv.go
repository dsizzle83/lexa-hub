// WP-10 (standards-buildout C2/C3): the advanced-DER reconciler shell for
// lexa-modbus — the execution half of the curve-control path (architecture
// §5(b) bottom half). One advShell per inverter/battery device consumes the
// retained lexa/desired/adv/{device} document (bus.DesiredAdvanced, authored
// by WP-9's cmd/hub adv author) and converges the device's PROVISIONED state
// — volt-var/volt-watt/watt-var curves, trip/ride-through sets, frequency
// droop, fixed PF/var, enter-service/energize — onto it, per axis.
//
// # Per-axis execution semantics (NORMATIVE, §5(b))
//
//   - Curve axes (volt_var, volt_watt, watt_var, trips, freq_droop): derbase's
//     typed writers run the SunSpec §3.1.2 adopt handshake INTERNALLY (stage
//     curve at index 1 → AdptCrvReq=2 → poll AdptCrvRslt to COMPLETED/FAILED,
//     bounded by derbase.Base.AdoptPollTimeout, default 3 s at 100 ms polls →
//     Ena=1). The handshake result is NEVER trusted on its own — pollAdoptResult
//     deliberately returns success on a device that never updates the result
//     point — so this shell then RE-READS the live curve (index 0) and
//     recomputes the pinned bus.CurveSetContentHash from the readback:
//     adopt_state=adopted ONLY on a readback hash match; an AdptCrvRslt FAILED
//     (or any write error) ⇒ failed, a readback mismatch ⇒ diverged, both
//     retried on a backoff whose first step is ≥ the poll/readback interval.
//     A live curve that ALREADY matches the desired hash is skipped entirely
//     (D6's no-op re-adoption after reconnect/restart).
//   - Fixed PF / fixed var: derbase.SetFixedPF / SetConstantVar (704 sync
//     groups — whole-block RMW, this shell is the SOLE writer of the PF/var
//     groups, see "Single-writer" below). Convergence is judged from MEASURED
//     PF/Var in the poll-loop observe() feed, one-sided/toleranced (bands
//     below) — a write's success is never convergence.
//   - Energize: derbase.SetEnterServiceEnabled (703 ES) with convergence from
//     measured cessation (W → ~0, or ConnSt) — one-sided like solar: a dark
//     inverter commanded TO energize is never called diverged (night is not
//     a fault, and 1547 enter-service has lawful randomized delays).
//   - Legacy 12x devices (der_gen "12x"): the sunspec package declares no
//     legacy curve models (126/131/132/…), so curve axes and freq-droop are
//     adopt_state=unsupported by decision, NOT emulated — we do not invent
//     register maps. The reduced axis set is fixed PF/var via model 123
//     (OutPFSet / VArPct — offsets declared in lexa-proto/sunspec/models.go)
//     with the Rule 21 enable-rewrite rule: the *_Ena point is rewritten
//     AFTER every value change, asserted in order by the write-log tests.
//     Energize is also unsupported on 12x: there is no 703 ES, and the only
//     analog (M123 Conn) is the SCALAR shells' surface — mapping energize
//     onto it would create a second writer of the same register.
//
// # Interlock seniority (ledger L8)
//
// While the Tier-0 battery interlock has a pack force-disconnected
// (isTripped), an energize-restoring write (ES=1) is SUPPRESSED and reported
// as InterlockHold — the same guard-versus-guard rule the battery shell
// applies to Connect. Cease-to-energize (ES=0) and provisioning writes pass.
//
// # Single-writer register ownership (grep-provable)
//
// This shell exclusively owns models 703 and 705–712 plus the 704 PF/var
// groups (SetFixedPF/SetConstantVar/SetEnterServiceEnabled and the curve
// writers are called from THIS FILE only). The scalar shells keep the
// 123/704 W-path (battCommandToControl/solarFieldsToControl construct only
// OpModExpLimW/ImpLimW/MaxLimW/Connect — derbase.ApplyControl's PF/var/
// energize branches are unreachable from them). The one shared surface —
// write704's whole-block RMW touching PF registers on a scalar WSet write —
// is safe because adv and scalar writes for one device serialize on the SAME
// per-device transport mutex (retryDevice.mu, via retryDevice.advanced), so
// the RMW can never interleave with an adv PF write.
//
// # Crash-only (AD-011)
//
// Everything re-seeds from retained topics: the desired doc replays on
// subscribe (initial-desired seed), the per-device retained ReconcileReport
// (axis/adopt_state/curve_hash — D6: adoption state rides the REPORT, never
// the desired doc) lets the hub re-seed provisioning state, and a stale
// retained NonConvergedBegin from a previous process instance is healed by
// the same healStaleRetainedReport the scalar shells use.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/metrics"
	"lexa-hub/internal/mqttutil"
	"lexa-hub/internal/reconcile"
	"lexa-hub/internal/southbound/device"
	"lexa-proto/derbase"
	"lexa-proto/sunspec"
)

// advClass is the {class} segment of the desired/report topic families this
// shell owns (lexa/desired/adv/{device}, lexa/reconcile/adv/{device}/report).
const advClass = "adv"

// Convergence bands and pacing (config-defaulted constants, mirroring the
// scalar shells' tolerance style — reconcileCeilingTolerance et al).
const (
	// advPFBand is the one-sided fixed-PF convergence band: a measured |PF|
	// at or above commanded−band is compliant (running CLOSER to unity than
	// commanded injects LESS reactive power than allowed — the limit
	// direction); only a sustained shortfall beyond the band is divergence.
	advPFBand = 0.02
	// advPFAssessMinW is the |W| floor below which a PF measurement is
	// meaningless (cosφ of noise) and the assessment HOLDS — never evidence.
	advPFAssessMinW = 100.0
	// advVarBandFrac is the fixed-var convergence band as a fraction of the
	// device's reactive rating: |measured − target| ≤ frac·rating converges.
	// Two-sided by design — fixed var is a SETPOINT, not a limit, and a
	// device ignoring it entirely (Var≈0) must read as divergence
	// (ev-accept-but-ignore's lesson).
	advVarBandFrac = 0.05
	// advCessationBandW is the measured-cessation floor for the energize
	// axis: |W| at or below it counts as ceased.
	advCessationBandW = 50.0
	// advMinRetryBackoff floors the corrective re-adopt backoff (must be
	// ≥ the poll/readback interval per §5(b); 15 s also matches the EVSE
	// shell's ≥-per-call-bound rationale).
	advMinRetryBackoff = 15 * time.Second
	// advReassertEvery is the slow re-verify cadence: converged axes re-run
	// their idempotent check-then-act executor (a pure readback when the
	// hash already matches; a 704 rewrite refreshing the RvrtTms reversion
	// window for fixed PF/var — deliberate, C3 keeps the window alive).
	advReassertEvery = 10 * time.Minute
	// Droop readback comparison tolerances (register scale factors quantize
	// the write, so exact equality is wrong): relative + absolute floor.
	advDroopRelTol = 0.02
	advDroopAbsTol = 0.005
)

// advAxisOrder is the stable per-doc processing order of the axis runners.
var advAxisOrder = []string{
	bus.AdvAxisVoltVar, bus.AdvAxisWattVar, bus.AdvAxisFixedPF, bus.AdvAxisFixedVar,
	bus.AdvAxisVoltWatt, bus.AdvAxisFreqWatt, bus.AdvAxisFreqDroop,
	bus.AdvAxisTripLV, bus.AdvAxisTripHV, bus.AdvAxisTripLF, bus.AdvAxisTripHF,
	bus.AdvAxisEnergize,
}

// ── Driver seam ──────────────────────────────────────────────────────────────

// advCaps is the device's advanced-execution capability snapshot, read from
// the live SunSpec model scan (execution truth — der_gen config is the
// authoring-side promise, this is what the hardware actually reports).
type advCaps struct {
	Has703, Has704, Has705, Has706 bool
	Has707, Has708, Has709, Has710 bool
	Has711, Has712, Has123         bool
	// VarRatingVar is the reactive rating (var) used as the fixed-var
	// convergence base (702 VarMaxInj setting, falling back through the
	// rating points); NaN when unknown — the assessment then HOLDS.
	VarRatingVar float64
}

// advDriver is how an ACTIVE adv shell reaches hardware. The one production
// implementation is advDeviceDriver (below), which routes every call through
// retryDevice.advanced — i.e. under the SAME per-device transport mutex the
// poll loop and the scalar write path already take. Nil in shadow mode: a
// recorder has no driver, and there is no reachable call from a shadow shell
// to any write (grep-proof, TASK-027 pattern).
type advDriver interface {
	Caps() (advCaps, error)

	ReadVoltVar() (sunspec.VoltVarCurve, error)
	WriteVoltVar(sunspec.VoltVarCurve) error
	ReadVoltWatt() (sunspec.VoltWattCurve, error)
	WriteVoltWatt(sunspec.VoltWattCurve) error
	ReadWattVar() (sunspec.WattVarCurve, error)
	WriteWattVar(sunspec.WattVarCurve) error
	ReadVoltageTrip(hv bool) (sunspec.VoltageTripSet, error)
	WriteVoltageTrip(hv bool, s sunspec.VoltageTripSet) error
	ReadFreqTrip(hf bool) (sunspec.FreqTripSet, error)
	WriteFreqTrip(hf bool, s sunspec.FreqTripSet) error
	ReadFreqDroop() (sunspec.FreqDroopCtl, error)
	WriteFreqDroop(sunspec.FreqDroopCtl) error

	// SetFixedPF / SetFixedVarPct write the 704 PFWInj / VarSet sync groups
	// (whole-block RMW inside derbase), stamping rvrtTms into
	// derbase.Base.DefaultRvrtTms for the duration of the write ONLY (C3) —
	// the previous value is restored so the scalar shells' 704 W-path writes
	// never inherit an adv reversion window.
	SetFixedPF(rvrtTms uint32, pf float64, overExcited bool) error
	SetFixedVarPct(rvrtTms uint32, pct float64) error
	// SetEnergize toggles 703 ES (enter service / cease to energize).
	SetEnergize(on bool) error

	// ReadFuncEnabled / DisableFunc read / clear the axis function's Ena
	// point (release enforcement: an explicit-null axis means "no <axis> in
	// force", AD-013 — absence must never be readable as "keep whatever is
	// adopted"). Supported axes: volt_var, volt_watt, watt_var (7xx curve
	// models), fixed_pf, fixed_var (704 group enables).
	ReadFuncEnabled(axis string) (bool, error)
	DisableFunc(axis string) error

	// Legacy 12x reduced axis set (model 123, enable-rewrite rule).
	SetFixedPF12x(pf float64, overExcited bool) error
	SetFixedVarPct12x(pct float64) error
	Read12xEnabled(axis string) (bool, error)
	Disable12x(axis string) error
}

// errAdvOffline / errAdvNoSurface classify advanced-op failures: offline is
// transient (the reconnect reassert re-executes), no-surface is structural
// (the device wrapper exposes no derbase.Base — meters).
var (
	errAdvOffline   = errors.New("device offline")
	errAdvNoSurface = errors.New("device has no advanced DER surface")
)

// derBaser is the wrapper-side accessor (inverter.Inverter / battery.Battery)
// exposing the shared derbase.Base.
type derBaser interface {
	DERBase() *derbase.Base
}

// advanced runs fn against the live device's derbase surface under the SAME
// per-device mutex ReadMeasurements/ApplyControl take — the per-device
// transport ownership every write in this process serializes on. It never
// drops the session itself (a FAILED adopt is a healthy device refusing a
// curve, not a dead transport); a genuinely dead session is dropped by the
// next poll's ReadMeasurements, and the reconnect reassert re-runs the op.
//
// NOTE: curve writes hold the mutex for the whole adopt handshake — up to
// Base.AdoptPollTimeout (default 3 s) of AdptCrvRslt polling — during which
// this device's poll blocks. Provisioning is rare (doc changes + the 10 min
// re-verify), so one delayed poll is the accepted cost of a serialized
// transport.
func (r *retryDevice) advanced(fn func(b *derbase.Base, tag string) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.live == nil {
		return errAdvOffline
	}
	db, ok := r.live.(derBaser)
	if !ok {
		return errAdvNoSurface
	}
	return fn(db.DERBase(), r.cfg.Name)
}

// advDeviceDriver is the production advDriver over one retryDevice.
type advDeviceDriver struct{ rd *retryDevice }

func (d *advDeviceDriver) Caps() (advCaps, error) {
	caps := advCaps{VarRatingVar: math.NaN()}
	err := d.rd.advanced(func(b *derbase.Base, tag string) error {
		caps.Has703, caps.Has704 = b.Has703, b.Has704
		caps.Has705, caps.Has706 = b.Has705, b.Has706
		caps.Has707, caps.Has708 = b.Has707, b.Has708
		caps.Has709, caps.Has710 = b.Has709, b.Has710
		caps.Has711, caps.Has712 = b.Has711, b.Has712
		caps.Has123 = b.Reader.HasModel(sunspec.ModelImmediateCtrl)
		if b.Has702 {
			if c, err := b.ReadDERCapacity(tag); err == nil {
				caps.VarRatingVar = firstPositiveVar(c.VarMaxInj, c.VarMaxInjRtg, c.VAMax, c.VAMaxRtg)
			}
		}
		return nil
	})
	return caps, err
}

// firstPositiveVar picks the first finite, positive rating.
func firstPositiveVar(vals ...float64) float64 {
	for _, v := range vals {
		if !math.IsNaN(v) && v > 0 {
			return v
		}
	}
	return math.NaN()
}

func (d *advDeviceDriver) ReadVoltVar() (c sunspec.VoltVarCurve, err error) {
	err = d.rd.advanced(func(b *derbase.Base, tag string) error { c, err = b.ReadVoltVar(tag); return err })
	return c, err
}
func (d *advDeviceDriver) WriteVoltVar(c sunspec.VoltVarCurve) error {
	return d.rd.advanced(func(b *derbase.Base, tag string) error { return b.WriteVoltVar(c, tag) })
}
func (d *advDeviceDriver) ReadVoltWatt() (c sunspec.VoltWattCurve, err error) {
	err = d.rd.advanced(func(b *derbase.Base, tag string) error { c, err = b.ReadVoltWatt(tag); return err })
	return c, err
}
func (d *advDeviceDriver) WriteVoltWatt(c sunspec.VoltWattCurve) error {
	return d.rd.advanced(func(b *derbase.Base, tag string) error { return b.WriteVoltWatt(c, tag) })
}
func (d *advDeviceDriver) ReadWattVar() (c sunspec.WattVarCurve, err error) {
	err = d.rd.advanced(func(b *derbase.Base, tag string) error { c, err = b.ReadWattVar(tag); return err })
	return c, err
}
func (d *advDeviceDriver) WriteWattVar(c sunspec.WattVarCurve) error {
	return d.rd.advanced(func(b *derbase.Base, tag string) error { return b.WriteWattVar(c, tag) })
}
func (d *advDeviceDriver) ReadVoltageTrip(hv bool) (s sunspec.VoltageTripSet, err error) {
	err = d.rd.advanced(func(b *derbase.Base, tag string) error {
		if hv {
			s, err = b.ReadVoltageTripHV(tag)
		} else {
			s, err = b.ReadVoltageTripLV(tag)
		}
		return err
	})
	return s, err
}
func (d *advDeviceDriver) WriteVoltageTrip(hv bool, s sunspec.VoltageTripSet) error {
	return d.rd.advanced(func(b *derbase.Base, tag string) error {
		if hv {
			return b.WriteVoltageTripHV(s, tag)
		}
		return b.WriteVoltageTripLV(s, tag)
	})
}
func (d *advDeviceDriver) ReadFreqTrip(hf bool) (s sunspec.FreqTripSet, err error) {
	err = d.rd.advanced(func(b *derbase.Base, tag string) error {
		if hf {
			s, err = b.ReadFreqTripHF(tag)
		} else {
			s, err = b.ReadFreqTripLF(tag)
		}
		return err
	})
	return s, err
}
func (d *advDeviceDriver) WriteFreqTrip(hf bool, s sunspec.FreqTripSet) error {
	return d.rd.advanced(func(b *derbase.Base, tag string) error {
		if hf {
			return b.WriteFreqTripHF(s, tag)
		}
		return b.WriteFreqTripLF(s, tag)
	})
}
func (d *advDeviceDriver) ReadFreqDroop() (c sunspec.FreqDroopCtl, err error) {
	err = d.rd.advanced(func(b *derbase.Base, tag string) error { c, err = b.ReadFreqDroop(tag); return err })
	return c, err
}
func (d *advDeviceDriver) WriteFreqDroop(c sunspec.FreqDroopCtl) error {
	return d.rd.advanced(func(b *derbase.Base, tag string) error { return b.WriteFreqDroop(c, tag) })
}

// withRvrtTms runs a 704-family write with DefaultRvrtTms temporarily set to
// the doc's reversion window (C3), restoring the previous value so the scalar
// shells' 704 W-path writes never inherit it.
func withRvrtTms(b *derbase.Base, rvrtTms uint32, fn func() error) error {
	prev := b.DefaultRvrtTms
	b.DefaultRvrtTms = rvrtTms
	defer func() { b.DefaultRvrtTms = prev }()
	return fn()
}

func (d *advDeviceDriver) SetFixedPF(rvrtTms uint32, pf float64, overExcited bool) error {
	return d.rd.advanced(func(b *derbase.Base, tag string) error {
		return withRvrtTms(b, rvrtTms, func() error { return b.SetFixedPF(true, pf, overExcited, tag) })
	})
}
func (d *advDeviceDriver) SetFixedVarPct(rvrtTms uint32, pct float64) error {
	return d.rd.advanced(func(b *derbase.Base, tag string) error {
		return withRvrtTms(b, rvrtTms, func() error { return b.SetConstantVar(pct, tag) })
	})
}
func (d *advDeviceDriver) SetEnergize(on bool) error {
	return d.rd.advanced(func(b *derbase.Base, tag string) error { return b.SetEnterServiceEnabled(on, tag) })
}

// advEnaPoint maps a releasable axis to its function-enable register.
func advEnaPoint(axis string) (modelID uint16, offset int, ok bool) {
	switch axis {
	case bus.AdvAxisVoltVar:
		return sunspec.ModelDERVoltVar, sunspec.L705Hdr.Offset("Ena"), true
	case bus.AdvAxisVoltWatt:
		return sunspec.ModelDERVoltWatt, sunspec.L706Hdr.Offset("Ena"), true
	case bus.AdvAxisWattVar:
		return sunspec.ModelDERWattVar, sunspec.L712Hdr.Offset("Ena"), true
	case bus.AdvAxisFixedPF:
		return sunspec.ModelDERCtlAC, sunspec.L704.Offset("PFWInjEna"), true
	case bus.AdvAxisFixedVar:
		return sunspec.ModelDERCtlAC, sunspec.L704.Offset("VarSetEna"), true
	default:
		return 0, 0, false
	}
}

func (d *advDeviceDriver) ReadFuncEnabled(axis string) (on bool, err error) {
	modelID, off, ok := advEnaPoint(axis)
	if !ok {
		return false, fmt.Errorf("axis %q has no releasable function enable", axis)
	}
	err = d.rd.advanced(func(b *derbase.Base, tag string) error {
		regs, rerr := b.Reader.ReadModel(modelID)
		if rerr != nil {
			return rerr
		}
		if off < 0 || off >= len(regs) {
			return fmt.Errorf("%s: model %d too short for Ena", tag, modelID)
		}
		on = regs[off] == 1
		return nil
	})
	return on, err
}

func (d *advDeviceDriver) DisableFunc(axis string) error {
	modelID, off, ok := advEnaPoint(axis)
	if !ok {
		return fmt.Errorf("axis %q has no releasable function enable", axis)
	}
	return d.rd.advanced(func(b *derbase.Base, tag string) error {
		return b.Reader.WriteModel(modelID, uint16(off), []uint16{0})
	})
}

func (d *advDeviceDriver) SetFixedPF12x(pf float64, overExcited bool) error {
	return d.rd.advanced(func(b *derbase.Base, tag string) error {
		return writeM123FixedPF(b.Reader, pf, overExcited)
	})
}
func (d *advDeviceDriver) SetFixedVarPct12x(pct float64) error {
	return d.rd.advanced(func(b *derbase.Base, tag string) error {
		return writeM123VarPct(b.Reader, pct)
	})
}

// adv12xEnaOffset maps a 12x-supported axis to its M123 enable point.
func adv12xEnaOffset(axis string) (int, bool) {
	switch axis {
	case bus.AdvAxisFixedPF:
		return sunspec.M123_OutPFSet_Ena, true
	case bus.AdvAxisFixedVar:
		return sunspec.M123_VArPct_Ena, true
	default:
		return 0, false
	}
}

func (d *advDeviceDriver) Read12xEnabled(axis string) (on bool, err error) {
	off, ok := adv12xEnaOffset(axis)
	if !ok {
		return false, fmt.Errorf("axis %q has no 12x enable", axis)
	}
	err = d.rd.advanced(func(b *derbase.Base, tag string) error {
		regs, rerr := b.Reader.ReadModel(sunspec.ModelImmediateCtrl)
		if rerr != nil {
			return rerr
		}
		if off >= len(regs) {
			return fmt.Errorf("%s: model 123 too short", tag)
		}
		on = regs[off] == 1
		return nil
	})
	return on, err
}

func (d *advDeviceDriver) Disable12x(axis string) error {
	off, ok := adv12xEnaOffset(axis)
	if !ok {
		return fmt.Errorf("axis %q has no 12x enable", axis)
	}
	return d.rd.advanced(func(b *derbase.Base, tag string) error {
		return b.Reader.WriteModel(sunspec.ModelImmediateCtrl, uint16(off), []uint16{0})
	})
}

// ── Legacy 12x writes (enable-rewrite rule) ──────────────────────────────────

// modelRW is the register-level slice of *sunspec.Reader the M123 writers
// need — an interface so the enable-rewrite ORDER is assertable against a
// recording fake without a Modbus server.
type modelRW interface {
	ReadModel(modelID uint16) ([]uint16, error)
	WriteModel(modelID uint16, offset uint16, values []uint16) error
}

// writeM123FixedPF writes the legacy fixed-power-factor setting: OutPFSet
// (signed engineering PF via OutPFSet_SF; this codebase's sign convention
// mirrors 704's excitation enum — positive = over-excited/injecting vars,
// negative = under-excited) THEN OutPFSet_Ena=1 — the Rule 21 guide's
// enable-rewrite rule: the enable point is rewritten AFTER any value change,
// value first, enable last, always.
func writeM123FixedPF(rw modelRW, pf float64, overExcited bool) error {
	regs, err := rw.ReadModel(sunspec.ModelImmediateCtrl)
	if err != nil {
		return fmt.Errorf("m123 fixed-pf: read: %w", err)
	}
	if len(regs) <= sunspec.M123_OutPFSet_SF {
		return fmt.Errorf("m123 fixed-pf: model 123 too short")
	}
	sf := int16(regs[sunspec.M123_OutPFSet_SF])
	signed := pf
	if !overExcited {
		signed = -pf
	}
	raw := sunspec.RawFromScaleSigned(signed, sf)
	if err := rw.WriteModel(sunspec.ModelImmediateCtrl, sunspec.M123_OutPFSet, []uint16{raw}); err != nil {
		return fmt.Errorf("m123 fixed-pf: write OutPFSet: %w", err)
	}
	return rw.WriteModel(sunspec.ModelImmediateCtrl, sunspec.M123_OutPFSet_Ena, []uint16{1})
}

// writeM123VarPct writes the legacy constant-var setting: VArPct_Mod (2 = %
// of VArMax, matching 704 VarSetMod VarMaxPct semantics) and VArPct, THEN
// VArPct_Ena=1 last (enable-rewrite rule, as above).
func writeM123VarPct(rw modelRW, pct float64) error {
	regs, err := rw.ReadModel(sunspec.ModelImmediateCtrl)
	if err != nil {
		return fmt.Errorf("m123 var-pct: read: %w", err)
	}
	if len(regs) <= sunspec.M123_VArPct_SF {
		return fmt.Errorf("m123 var-pct: model 123 too short")
	}
	sf := int16(regs[sunspec.M123_VArPct_SF])
	const varPctModVArMax = 2 // M123 VArPct_Mod: % of VArMax
	if err := rw.WriteModel(sunspec.ModelImmediateCtrl, sunspec.M123_VArPct_Mod, []uint16{varPctModVArMax}); err != nil {
		return fmt.Errorf("m123 var-pct: write VArPct_Mod: %w", err)
	}
	if err := rw.WriteModel(sunspec.ModelImmediateCtrl, sunspec.M123_VArPct, []uint16{sunspec.RawFromScaleSigned(pct, sf)}); err != nil {
		return fmt.Errorf("m123 var-pct: write VArPct: %w", err)
	}
	return rw.WriteModel(sunspec.ModelImmediateCtrl, sunspec.M123_VArPct_Ena, []uint16{1})
}

// ── Curve conversions + readback hash recomputation ──────────────────────────

// advEng applies a power-of-ten multiplier to a raw 2030.5 breakpoint.
func advEng(raw int32, mult int8) float64 { return float64(raw) * math.Pow10(int(mult)) }

// advRaw inverts advEng with rounding — the readback→canonical-points half of
// the pinned CurveSetContentHash recomputation. A device whose register scale
// factors cannot represent the commanded value exactly produces a different
// raw point here and therefore a hash mismatch (diverged) — deliberate: the
// device is NOT provisioned with the commanded curve.
func advRaw(eng float64, mult int8) int32 { return int32(math.Round(eng / math.Pow10(int(mult)))) }

// advEntry rebuilds the canonical CurveSetEntry a curve was resolved from:
// mode/curveType/mults/yRefType from the DOC (register readbacks carry no
// 2030.5 identity), points from the given source. Feeding the doc's own
// points reproduces AdvCurve.Hash exactly (pinned by WP-8's hash tests);
// feeding readback-derived points is the adoption verification.
func advEntry(mode string, c bus.AdvCurve, pts []bus.CurvePoint) bus.CurveSetEntry {
	return bus.CurveSetEntry{
		Mode: mode, CurveType: c.CurveType,
		XMult: c.XMult, YMult: c.YMult, YRefType: c.YRefType,
		Points: pts,
	}
}

// advCurveMode maps a curve axis to the CurveSet mode name its entry was
// hashed under by the WP-8/9 publishers. watt_var deliberately maps to
// "watt_pf": IEEE 2030.5's carriage for the watt-shaped reactive axis is
// opModWattPF (see bus.AdvReactiveWattVar's doc), so that is the mode string
// inside AdvCurve.Hash today.
func advCurveMode(axis string) string {
	switch axis {
	case bus.AdvAxisVoltVar:
		return bus.CurveModeVoltVar
	case bus.AdvAxisWattVar:
		return bus.CurveModeWattPF
	case bus.AdvAxisVoltWatt:
		return bus.CurveModeVoltWatt
	case bus.AdvAxisFreqWatt:
		return bus.CurveModeFreqWatt
	default:
		return axis
	}
}

// advTripModes maps (trip axis, AdvTrip kind) → CurveSet mode name — the
// inverse of cmd/hub/adv.go's advTripAxes table.
var advTripModes = map[string]map[string]string{
	bus.AdvAxisTripLV: {
		bus.AdvTripMustTrip:           bus.CurveModeLVRTMustTrip,
		bus.AdvTripMayTrip:            bus.CurveModeLVRTMayTrip,
		bus.AdvTripMomentaryCessation: bus.CurveModeLVRTMomentaryCessation,
	},
	bus.AdvAxisTripHV: {
		bus.AdvTripMustTrip:           bus.CurveModeHVRTMustTrip,
		bus.AdvTripMayTrip:            bus.CurveModeHVRTMayTrip,
		bus.AdvTripMomentaryCessation: bus.CurveModeHVRTMomentaryCessation,
	},
	bus.AdvAxisTripLF: {
		bus.AdvTripMustTrip: bus.CurveModeLFRTMustTrip,
		bus.AdvTripMayTrip:  bus.CurveModeLFRTMayTrip,
	},
	bus.AdvAxisTripHF: {
		bus.AdvTripMustTrip: bus.CurveModeHFRTMustTrip,
		bus.AdvTripMayTrip:  bus.CurveModeHFRTMayTrip,
	},
}

// Curve-shape conversions, doc → sunspec engineering values. 2030.5 axis
// semantics: volt-var/volt-watt x = voltage, y = var/watt; watt-var x = watt,
// y = var; ride-through x = duration (time), y = voltage/frequency.
func vvFromDoc(tmpl sunspec.VoltVarCurve, c bus.AdvCurve) sunspec.VoltVarCurve {
	out := tmpl
	out.ReadOnly = false
	out.Points = make([]sunspec.VVPoint, len(c.Points))
	for i, p := range c.Points {
		out.Points[i] = sunspec.VVPoint{V: advEng(p.X, c.XMult), Var: advEng(p.Y, c.YMult)}
	}
	return out
}

func vwFromDoc(tmpl sunspec.VoltWattCurve, c bus.AdvCurve) sunspec.VoltWattCurve {
	out := tmpl
	out.ReadOnly = false
	out.Points = make([]sunspec.VWPoint, len(c.Points))
	for i, p := range c.Points {
		out.Points[i] = sunspec.VWPoint{V: advEng(p.X, c.XMult), W: advEng(p.Y, c.YMult)}
	}
	return out
}

func wvFromDoc(tmpl sunspec.WattVarCurve, c bus.AdvCurve) sunspec.WattVarCurve {
	out := tmpl
	out.ReadOnly = false
	out.Points = make([]sunspec.WVPoint, len(c.Points))
	for i, p := range c.Points {
		out.Points[i] = sunspec.WVPoint{W: advEng(p.X, c.XMult), Var: advEng(p.Y, c.YMult)}
	}
	return out
}

func voltageTripFromDoc(set bus.AdvTripSet) sunspec.VoltageTripSet {
	var out sunspec.VoltageTripSet
	for _, tc := range set.Curves {
		pts := make([]sunspec.TripVPoint, len(tc.Curve.Points))
		for i, p := range tc.Curve.Points {
			pts[i] = sunspec.TripVPoint{V: advEng(p.Y, tc.Curve.YMult), Tms: advEng(p.X, tc.Curve.XMult)}
		}
		switch tc.Kind {
		case bus.AdvTripMustTrip:
			out.MustTrip = pts
		case bus.AdvTripMayTrip:
			out.MayTrip = pts
		case bus.AdvTripMomentaryCessation:
			out.MomCess = pts
		}
	}
	return out
}

func freqTripFromDoc(set bus.AdvTripSet) sunspec.FreqTripSet {
	var out sunspec.FreqTripSet
	for _, tc := range set.Curves {
		pts := make([]sunspec.TripHzPoint, len(tc.Curve.Points))
		for i, p := range tc.Curve.Points {
			pts[i] = sunspec.TripHzPoint{Hz: advEng(p.Y, tc.Curve.YMult), Tms: advEng(p.X, tc.Curve.XMult)}
		}
		switch tc.Kind {
		case bus.AdvTripMustTrip:
			out.MustTrip = pts
		case bus.AdvTripMayTrip:
			out.MayTrip = pts
		case bus.AdvTripMomentaryCessation:
			out.MomCess = pts
		}
	}
	return out
}

// Readback hash recomputation, per axis shape.
func readbackHashVV(axis string, c bus.AdvCurve, rb sunspec.VoltVarCurve) string {
	pts := make([]bus.CurvePoint, len(rb.Points))
	for i, p := range rb.Points {
		pts[i] = bus.CurvePoint{X: advRaw(p.V, c.XMult), Y: advRaw(p.Var, c.YMult)}
	}
	return bus.CurveSetContentHash([]bus.CurveSetEntry{advEntry(advCurveMode(axis), c, pts)})
}

func readbackHashVW(axis string, c bus.AdvCurve, rb sunspec.VoltWattCurve) string {
	pts := make([]bus.CurvePoint, len(rb.Points))
	for i, p := range rb.Points {
		pts[i] = bus.CurvePoint{X: advRaw(p.V, c.XMult), Y: advRaw(p.W, c.YMult)}
	}
	return bus.CurveSetContentHash([]bus.CurveSetEntry{advEntry(advCurveMode(axis), c, pts)})
}

func readbackHashWV(axis string, c bus.AdvCurve, rb sunspec.WattVarCurve) string {
	pts := make([]bus.CurvePoint, len(rb.Points))
	for i, p := range rb.Points {
		pts[i] = bus.CurvePoint{X: advRaw(p.W, c.XMult), Y: advRaw(p.Var, c.YMult)}
	}
	return bus.CurveSetContentHash([]bus.CurveSetEntry{advEntry(advCurveMode(axis), c, pts)})
}

// readbackHashVoltageTrip recomputes the axis-level hash from a 707/708
// readback. Returns "" (never matches a non-empty desired hash) when an
// UNCOMMANDED sub-curve is non-empty on the device — extra protective curves
// we did not command are a provisioning mismatch, not a match.
func readbackHashVoltageTrip(axis string, set bus.AdvTripSet, rb sunspec.VoltageTripSet) string {
	sub := func(kind string) []sunspec.TripVPoint {
		switch kind {
		case bus.AdvTripMustTrip:
			return rb.MustTrip
		case bus.AdvTripMayTrip:
			return rb.MayTrip
		default:
			return rb.MomCess
		}
	}
	commanded := map[string]bool{}
	var entries []bus.CurveSetEntry
	for _, tc := range set.Curves {
		commanded[tc.Kind] = true
		rbPts := sub(tc.Kind)
		pts := make([]bus.CurvePoint, len(rbPts))
		for i, p := range rbPts {
			pts[i] = bus.CurvePoint{X: advRaw(p.Tms, tc.Curve.XMult), Y: advRaw(p.V, tc.Curve.YMult)}
		}
		entries = append(entries, advEntry(advTripModes[axis][tc.Kind], tc.Curve, pts))
	}
	for _, kind := range []string{bus.AdvTripMustTrip, bus.AdvTripMayTrip, bus.AdvTripMomentaryCessation} {
		if !commanded[kind] && len(sub(kind)) > 0 {
			return ""
		}
	}
	return bus.CurveSetContentHash(entries)
}

func readbackHashFreqTrip(axis string, set bus.AdvTripSet, rb sunspec.FreqTripSet) string {
	sub := func(kind string) []sunspec.TripHzPoint {
		switch kind {
		case bus.AdvTripMustTrip:
			return rb.MustTrip
		case bus.AdvTripMayTrip:
			return rb.MayTrip
		default:
			return rb.MomCess
		}
	}
	commanded := map[string]bool{}
	var entries []bus.CurveSetEntry
	for _, tc := range set.Curves {
		commanded[tc.Kind] = true
		rbPts := sub(tc.Kind)
		pts := make([]bus.CurvePoint, len(rbPts))
		for i, p := range rbPts {
			pts[i] = bus.CurvePoint{X: advRaw(p.Tms, tc.Curve.XMult), Y: advRaw(p.Hz, tc.Curve.YMult)}
		}
		entries = append(entries, advEntry(advTripModes[axis][tc.Kind], tc.Curve, pts))
	}
	for _, kind := range []string{bus.AdvTripMustTrip, bus.AdvTripMayTrip, bus.AdvTripMomentaryCessation} {
		if !commanded[kind] && len(sub(kind)) > 0 {
			return ""
		}
	}
	return bus.CurveSetContentHash(entries)
}

// droopHash is the freq-droop axis's content fingerprint source — droop has
// no publisher-side hash (bus.AdvFreqDroop carries parameters, not points),
// so the shell digests the canonical parameter tuple itself.
func droopHash(d bus.AdvFreqDroop) string {
	h := sha256.New()
	fmt.Fprintf(h, "droop|%g|%g|%g|%g|%g", d.DbOfHz, d.DbUfHz, d.KOf, d.KUf, d.OlrtS)
	return hex.EncodeToString(h.Sum(nil))
}

// droopMatches compares a desired droop against a 711 readback under the
// scale-factor quantization tolerance.
func droopMatches(d bus.AdvFreqDroop, rb sunspec.FreqDroopCtl) bool {
	close := func(got, want float64) bool {
		return math.Abs(got-want) <= math.Max(advDroopRelTol*math.Abs(want), advDroopAbsTol)
	}
	return close(rb.DbOf, d.DbOfHz) && close(rb.DbUf, d.DbUfHz) &&
		close(rb.KOf, d.KOf) && close(rb.KUf, d.KUf) && close(rb.RspTms, d.OlrtS)
}

// advFingerprint folds a content hash into the reconcile.AdvContent field
// value: the first 8 hex digits as a float64 (exact — < 2^32 < 2^53). Never
// 0 (0 is the release sentinel) and never NaN. The fingerprint only carries
// "same or different" — the shell always compares FULL hashes before feeding
// it, so a 32-bit collision cannot fake a match (see advMismatch).
func advFingerprint(hash string) float64 {
	if len(hash) < 8 {
		return 1
	}
	v, err := strconv.ParseUint(hash[:8], 16, 64)
	if err != nil || v == 0 {
		return 1
	}
	return float64(v)
}

// advMismatch is a fed AdvContent value guaranteed ≠ the desired fingerprint
// (and ≠ 0), used to record a verified NON-match.
func advMismatch(desired float64) float64 { return desired + 1 }

// ── Per-axis desired decomposition ───────────────────────────────────────────

// advTarget is one axis's slice of a DesiredAdvanced doc.
type advTarget struct {
	// engaged: the axis is commanded (fields carry the content fingerprint /
	// setpoint). Not engaged + enforce: explicit-null RELEASE — the function
	// must be verified disabled ({AdvContent: 0}). Not engaged + !enforce:
	// no-opinion axis (trips/droop/freq_watt null, energize nil) — fields are
	// empty and the executor deliberately does nothing (protective functions
	// are never stripped on a null axis; energize nil is "no opinion" per the
	// doc contract).
	engaged bool
	enforce bool
	fields  map[reconcile.Field]float64
	hash    string  // curve/droop content hash ("" for measured axes)
	fp      float64 // advFingerprint(hash) for curve axes

	// Execution payloads (exactly one set, per axis kind).
	curve       *bus.AdvCurve
	trips       *bus.AdvTripSet
	droop       *bus.AdvFreqDroop
	fixedPF     *bus.FixedPF
	fixedVarPct float64
	energize    bool
}

// advAxisTargets decomposes one doc into the per-axis targets, in
// advAxisOrder keys. Pure — the D6 structural guarantees (ReactiveMode is ONE
// field) make the reactive fan-out trivial: the winner is engaged, the other
// three reactive axes are explicit releases.
func advAxisTargets(doc bus.DesiredAdvanced) map[string]advTarget {
	t := map[string]advTarget{}

	release := func() advTarget {
		return advTarget{enforce: true, fields: map[reconcile.Field]float64{reconcile.AdvContent: 0}}
	}
	noOpinion := func() advTarget {
		return advTarget{fields: map[reconcile.Field]float64{}}
	}
	curveTarget := func(c *bus.AdvCurve) advTarget {
		fp := advFingerprint(c.Hash)
		return advTarget{
			engaged: true, enforce: true, hash: c.Hash, fp: fp, curve: c,
			fields: map[reconcile.Field]float64{reconcile.AdvContent: fp},
		}
	}

	// Reactive axes (mutually exclusive by structure).
	t[bus.AdvAxisVoltVar] = release()
	t[bus.AdvAxisWattVar] = release()
	t[bus.AdvAxisFixedPF] = release()
	t[bus.AdvAxisFixedVar] = release()
	if rm := doc.ReactiveMode; rm != nil {
		switch rm.Kind {
		case bus.AdvReactiveVoltVar:
			if rm.Curve != nil {
				t[bus.AdvAxisVoltVar] = curveTarget(rm.Curve)
			}
		case bus.AdvReactiveWattVar:
			if rm.Curve != nil {
				t[bus.AdvAxisWattVar] = curveTarget(rm.Curve)
			}
		case bus.AdvReactiveFixedPF:
			if rm.FixedPF != nil {
				signed := rm.FixedPF.PF
				if !rm.FixedPF.OverExcited {
					signed = -signed
				}
				t[bus.AdvAxisFixedPF] = advTarget{
					engaged: true, enforce: true, fixedPF: rm.FixedPF,
					fields: map[reconcile.Field]float64{reconcile.FixedPF: signed},
				}
			}
		case bus.AdvReactiveFixedVar:
			if rm.FixedVarPct != nil {
				t[bus.AdvAxisFixedVar] = advTarget{
					engaged: true, enforce: true, fixedVarPct: *rm.FixedVarPct,
					fields: map[reconcile.Field]float64{reconcile.FixedVarPct: *rm.FixedVarPct},
				}
			}
		}
	}

	// Concurrent overlays.
	if doc.VoltWatt != nil {
		t[bus.AdvAxisVoltWatt] = curveTarget(doc.VoltWatt)
	} else {
		t[bus.AdvAxisVoltWatt] = release()
	}
	if doc.FreqWatt != nil {
		t[bus.AdvAxisFreqWatt] = curveTarget(doc.FreqWatt)
	} else {
		// No SunSpec model executes freq-watt (see file doc) — nothing to
		// disable on release either.
		t[bus.AdvAxisFreqWatt] = noOpinion()
	}

	// Droop and trips: protective/mandatory functions — a null axis is never
	// a disable write (no-opinion), per the file doc's no-strip rule.
	if doc.FreqDroop != nil {
		h := droopHash(*doc.FreqDroop)
		fp := advFingerprint(h)
		t[bus.AdvAxisFreqDroop] = advTarget{
			engaged: true, enforce: true, hash: h, fp: fp, droop: doc.FreqDroop,
			fields: map[reconcile.Field]float64{reconcile.AdvContent: fp},
		}
	} else {
		t[bus.AdvAxisFreqDroop] = noOpinion()
	}
	tripAxis := func(axis string, set *bus.AdvTripSet) {
		if set == nil {
			t[axis] = noOpinion()
			return
		}
		fp := advFingerprint(set.Hash)
		t[axis] = advTarget{
			engaged: true, enforce: true, hash: set.Hash, fp: fp, trips: set,
			fields: map[reconcile.Field]float64{reconcile.AdvContent: fp},
		}
	}
	if doc.Trips != nil {
		tripAxis(bus.AdvAxisTripLV, doc.Trips.LV)
		tripAxis(bus.AdvAxisTripHV, doc.Trips.HV)
		tripAxis(bus.AdvAxisTripLF, doc.Trips.LF)
		tripAxis(bus.AdvAxisTripHF, doc.Trips.HF)
	} else {
		tripAxis(bus.AdvAxisTripLV, nil)
		tripAxis(bus.AdvAxisTripHV, nil)
		tripAxis(bus.AdvAxisTripLF, nil)
		tripAxis(bus.AdvAxisTripHF, nil)
	}

	// Energize: *bool, nil = no opinion (never a release write).
	if doc.Energize != nil {
		v := 0.0
		if *doc.Energize {
			v = 1
		}
		t[bus.AdvAxisEnergize] = advTarget{
			engaged: true, enforce: true, energize: *doc.Energize,
			fields: map[reconcile.Field]float64{reconcile.Energize: v},
		}
	} else {
		t[bus.AdvAxisEnergize] = noOpinion()
	}
	return t
}

// ── The shell ────────────────────────────────────────────────────────────────

// advAxisRunner is one axis's convergence machinery: a single-Field
// reconcile.Reconciler (per-axis episodes, backoff, staleness, AD-013 gate)
// plus the shell-owned adoption state and execution payload.
type advAxisRunner struct {
	axis   string
	r      *reconcile.Reconciler
	target advTarget
	// adoptState is the §2.2 vocabulary state ("" = released / nothing in
	// force). Owned by the shell; transitions publish the retained report.
	adoptState string
}

// advShell is the per-device advanced reconciler shell. Like the scalar
// shells it locks mu on every entry point (feeds arrive on the MQTT
// subscription goroutine, publishMeasurements' loop, and the tick goroutine);
// internal/reconcile is single-writer by design and this type is the caller
// that serializes it.
type advShell struct {
	mu     sync.Mutex
	device string
	derGen string // "7xx" | "12x" | "" (unknown ⇒ everything unsupported)
	mode   shellMode

	driver    advDriver     // nil in shadow mode
	interlock interlockGate // Tier-0 seniority gate; nil in shadow mode

	runners map[string]*advAxisRunner

	// caps is the lazily-fetched capability snapshot; nil = not yet known
	// (device dark / never queried). Invalidated on reconnect — a replaced
	// device may report different models.
	caps *advCaps

	// Standing doc attribution for shell-authored (AdoptState) reports.
	docMRID     string
	docSeq      uint64
	docIssuedAt int64
	docRvrtTms  uint32

	reconnectPending atomic.Bool

	// Metrics (TASK-044 style, shared names across shells).
	adopts         *metrics.Counter // lexa_mb_adv_adopts_total
	failures       *metrics.Counter // lexa_mb_adv_failed_total (failed + diverged transitions)
	unsupportedCtr *metrics.Counter // lexa_mb_adv_unsupported_total
	wouldWrites    *metrics.Counter // lexa_mb_adv_would_writes_total (shadow verdicts too)
	writes         *metrics.Counter // lexa_mb_adv_writes_total (active driver mutations)
	writeFailures  *metrics.Counter // lexa_mb_adv_write_failures_total
	matches        *metrics.Counter // lexa_mb_adv_matches_total (measured-axis synthesis)
	divergences    *metrics.Counter // lexa_mb_adv_divergences_total
	interlockHolds *metrics.Counter // lexa_mb_interlock_holds_total (shared with battery shell)

	// pub forwards reports to MQTT (retained per device, class "adv"); nil in
	// shadow mode and tests.
	pub func(bus.ReconcileReport)
}

// newAdvShell builds a shell in the given mode. Shadow: driver and interlock
// MUST be nil (recorder); active: driver MUST be non-nil (interlock may be
// nil only in tests).
func newAdvShell(deviceName, derGen string, cfg reconcile.Config, mreg *metrics.Registry, mode shellMode, driver advDriver, gate interlockGate) *advShell {
	s := &advShell{
		device:         deviceName,
		derGen:         derGen,
		mode:           mode,
		driver:         driver,
		interlock:      gate,
		runners:        map[string]*advAxisRunner{},
		adopts:         mreg.Counter("lexa_mb_adv_adopts_total"),
		failures:       mreg.Counter("lexa_mb_adv_failed_total"),
		unsupportedCtr: mreg.Counter("lexa_mb_adv_unsupported_total"),
		wouldWrites:    mreg.Counter("lexa_mb_adv_would_writes_total"),
		writes:         mreg.Counter("lexa_mb_adv_writes_total"),
		writeFailures:  mreg.Counter("lexa_mb_adv_write_failures_total"),
		matches:        mreg.Counter("lexa_mb_adv_matches_total"),
		divergences:    mreg.Counter("lexa_mb_adv_divergences_total"),
		interlockHolds: mreg.Counter("lexa_mb_interlock_holds_total"),
	}
	for _, axis := range advAxisOrder {
		s.runners[axis] = &advAxisRunner{
			axis: axis,
			r:    reconcile.New(advClass, deviceName, cfg),
		}
	}
	return s
}

// advReconcileConfig is the per-axis reconciler tuning: the corrective
// re-adopt backoff's first step is ≥ the poll/readback interval (§5(b)) with
// the 15 s floor, and the slow reassert drives the periodic idempotent
// re-verify.
func advReconcileConfig(pollInterval time.Duration) reconcile.Config {
	first := pollInterval
	if first < advMinRetryBackoff {
		first = advMinRetryBackoff
	}
	return reconcile.Config{
		RetryBackoff:  []time.Duration{first, 2 * first, 4 * first, 8 * first},
		ReassertEvery: advReassertEvery,
	}
}

func (s *advShell) active() bool { return s.mode == modeActive }

func (s *advShell) tag() string {
	if s.active() {
		return "active"
	}
	return "shadow"
}

// markReconnected is retryDevice's onReconnect callback: atomic store only —
// same lock-order discipline as the scalar shells (never touches mu; the
// poll goroutine calls it under retryDevice.mu).
func (s *advShell) markReconnected() { s.reconnectPending.Store(true) }

// setDesired feeds one DesiredAdvanced document, fanned per axis through each
// runner's AD-013 gate (identical meta ⇒ identical accept/reject decisions).
func (s *advShell) setDesired(doc bus.DesiredAdvanced, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	meta := reconcile.DocMeta{MRID: doc.MRID, Seq: doc.Seq, IssuedAt: doc.IssuedAt}
	targets := advAxisTargets(doc)

	type pending struct {
		rn     *advAxisRunner
		action reconcile.Action
	}
	var writes []pending
	loggedReject, loggedSeqReset := false, false

	for _, axis := range advAxisOrder {
		rn := s.runners[axis]
		action, reports := rn.r.SetDesiredFields(meta, targets[axis].fields, now)
		for _, rep := range reports {
			switch rep.Kind {
			case reconcile.ReportRejectedDoc:
				if !loggedReject {
					loggedReject = true
					log.Printf("lexa-modbus: reconciler-adv[%s] %s: desired doc REJECTED (%s) seq=%d issued_at=%d mrid=%q",
						s.tag(), s.device, rep.Reject, doc.Seq, doc.IssuedAt, doc.MRID)
				}
			case reconcile.ReportSeqReset:
				if !loggedSeqReset {
					loggedSeqReset = true
					log.Printf("lexa-modbus: reconciler-adv[%s] %s: publisher restart (SeqReset) seq=%d issued_at=%d",
						s.tag(), s.device, doc.Seq, doc.IssuedAt)
				}
			default:
				s.handleReportLocked(rn, rep)
			}
		}
		if loggedReject {
			// A rejected doc changed nothing anywhere (identical gate per
			// runner); do not adopt payloads or execute.
			return
		}
		if action.Kind == reconcile.ActionWrite {
			writes = append(writes, pending{rn: rn, action: action})
		}
	}

	// Doc accepted: adopt attribution + payloads BEFORE executing.
	s.docMRID, s.docSeq, s.docIssuedAt = doc.MRID, doc.Seq, doc.IssuedAt
	s.docRvrtTms = 0
	if doc.RvrtTmsS != nil && *doc.RvrtTmsS > 0 {
		s.docRvrtTms = uint32(*doc.RvrtTmsS)
	}
	for _, axis := range advAxisOrder {
		rn := s.runners[axis]
		prev := rn.target
		rn.target = targets[axis]
		// Doc-arrival adopt-state transitions: a newly-commanded (or
		// re-commanded with new content) axis is pending until verified; a
		// release drops to "" once verified — but an unchanged axis keeps its
		// state (heartbeats must not flap adopted → pending).
		if rn.target.engaged && !fieldsEq(prev.fields, rn.target.fields) {
			s.transitionLocked(rn, bus.AdoptStatePending, now)
		}
	}
	for _, w := range writes {
		s.executeLocked(w.rn, w.action, now)
	}
}

// fieldsEq mirrors reconcile's fieldsEqual for the shell's transition edge.
func fieldsEq(a, b map[reconcile.Field]float64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || v != w {
			return false
		}
	}
	return true
}

// observe feeds one poll readback: measured convergence for the fixed_pf /
// fixed_var / energize axes, plus the active-mode reassert-on-reconnect.
func (s *advShell) observe(m device.Measurements, plausible bool, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.active() && s.reconnectPending.Swap(false) {
		s.caps = nil // device may have been replaced; re-read capability
		for _, axis := range advAxisOrder {
			rn := s.runners[axis]
			ra, reports := rn.r.Reconnected(now)
			for _, rep := range reports {
				s.handleReportLocked(rn, rep)
			}
			if ra.Kind == reconcile.ActionWrite {
				s.executeLocked(rn, ra, now)
			}
		}
	}

	for _, axis := range []string{bus.AdvAxisFixedPF, bus.AdvAxisFixedVar, bus.AdvAxisEnergize} {
		rn := s.runners[axis]
		if !rn.target.engaged {
			continue
		}
		read, verdict := s.synthesizeLocked(rn, m)
		if read == nil {
			continue // unassessable this poll: hold, never evidence
		}
		action, reports := rn.r.Observe(reconcile.Observed{
			Read: read, Connected: true, Plausible: plausible, At: now,
		}, now)
		if !plausible {
			continue // core held the sample (ledger L9); nothing to record
		}
		switch verdict {
		case advVerdictConverged:
			s.matches.Inc()
			s.transitionLocked(rn, bus.AdoptStateAdopted, now)
		case advVerdictDiverged:
			s.divergences.Inc()
			s.transitionLocked(rn, bus.AdoptStateDiverged, now)
		}
		for _, rep := range reports {
			s.handleReportLocked(rn, rep)
		}
		if action.Kind == reconcile.ActionWrite {
			s.executeLocked(rn, action, now)
		}
	}
}

// tick drives the per-axis wall-clock timers (retry backoff, episode begin,
// staleness, the slow re-verify reassert).
func (s *advShell) tick(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, axis := range advAxisOrder {
		rn := s.runners[axis]
		action, reports := rn.r.Tick(now)
		for _, rep := range reports {
			s.handleReportLocked(rn, rep)
		}
		if action.Kind == reconcile.ActionWrite {
			s.executeLocked(rn, action, now)
		}
	}
}

// ── Measured-axis synthesis ──────────────────────────────────────────────────

type advVerdict int

const (
	advVerdictHold advVerdict = iota
	advVerdictConverged
	advVerdictDiverged
)

// synthesizeLocked builds the one-sided/toleranced readback the measured
// axes feed the core: the DESIRED value on a converged assessment (so the
// core sees exact match), a genuinely-different measured value on
// divergence, nil when unassessable (hold — mirrors the solar shell's
// one-sided synthesis). mu held.
func (s *advShell) synthesizeLocked(rn *advAxisRunner, m device.Measurements) (map[reconcile.Field]float64, advVerdict) {
	switch rn.axis {
	case bus.AdvAxisFixedPF:
		target := rn.target.fields[reconcile.FixedPF] // signed
		if math.IsNaN(m.PF) || math.IsNaN(m.W) || math.Abs(m.W) < advPFAssessMinW {
			return nil, advVerdictHold
		}
		want := math.Abs(target)
		got := math.Abs(m.PF)
		// One-sided: |PF| at or above commanded−band is compliant (closer to
		// unity ⇒ less reactive than allowed).
		if got >= want-advPFBand {
			return map[reconcile.Field]float64{reconcile.FixedPF: target}, advVerdictConverged
		}
		return map[reconcile.Field]float64{reconcile.FixedPF: math.Copysign(got, target)}, advVerdictDiverged

	case bus.AdvAxisFixedVar:
		if math.IsNaN(m.Var) {
			return nil, advVerdictHold
		}
		rating := s.varRatingLocked()
		if math.IsNaN(rating) || rating <= 0 {
			return nil, advVerdictHold // no reactive rating: cannot assess (documented hold)
		}
		pct := rn.target.fields[reconcile.FixedVarPct]
		targetVar := pct / 100 * rating
		if math.Abs(m.Var-targetVar) <= advVarBandFrac*rating {
			return map[reconcile.Field]float64{reconcile.FixedVarPct: pct}, advVerdictConverged
		}
		return map[reconcile.Field]float64{reconcile.FixedVarPct: m.Var / rating * 100}, advVerdictDiverged

	case bus.AdvAxisEnergize:
		if !rn.target.energize {
			// Cease-to-energize: measured cessation (W→~0) or an explicit
			// disconnected ConnSt converges; sustained production diverges.
			if m.ConnSt != nil && *m.ConnSt == 0 {
				return map[reconcile.Field]float64{reconcile.Energize: 0}, advVerdictConverged
			}
			if !math.IsNaN(m.W) {
				if math.Abs(m.W) <= advCessationBandW {
					return map[reconcile.Field]float64{reconcile.Energize: 0}, advVerdictConverged
				}
				return map[reconcile.Field]float64{reconcile.Energize: 1}, advVerdictDiverged
			}
			return nil, advVerdictHold
		}
		// Energize: one-sided — positive evidence (running state, connected
		// state, or real power flow) converges; its ABSENCE is never
		// divergence (night, or a lawful 1547 enter-service delay).
		if (m.OpSt != nil && *m.OpSt == 1) || (m.ConnSt != nil && *m.ConnSt == 1) ||
			(!math.IsNaN(m.W) && math.Abs(m.W) > advCessationBandW) {
			return map[reconcile.Field]float64{reconcile.Energize: 1}, advVerdictConverged
		}
		return nil, advVerdictHold
	}
	return nil, advVerdictHold
}

// varRatingLocked returns the fixed-var convergence base, from the cached
// capability snapshot (NaN when unknown/unavailable). mu held.
func (s *advShell) varRatingLocked() float64 {
	caps, err := s.capsLocked()
	if err != nil {
		return math.NaN()
	}
	return caps.VarRatingVar
}

// capsLocked lazily fetches and caches the capability snapshot. mu held.
func (s *advShell) capsLocked() (advCaps, error) {
	if s.caps != nil {
		return *s.caps, nil
	}
	if s.driver == nil {
		return advCaps{}, errAdvNoSurface // shadow: capability from der_gen only
	}
	caps, err := s.driver.Caps()
	if err != nil {
		return advCaps{}, err
	}
	s.caps = &caps
	return caps, nil
}

// axisSupported is the execution-truth capability check: der_gen selects the
// generation, the live model scan gates each 7xx axis.
func axisSupported(axis, derGen string, caps advCaps) bool {
	switch derGen {
	case "7xx":
		switch axis {
		case bus.AdvAxisVoltVar:
			return caps.Has705
		case bus.AdvAxisVoltWatt:
			return caps.Has706
		case bus.AdvAxisWattVar:
			return caps.Has712
		case bus.AdvAxisFreqDroop:
			return caps.Has711
		case bus.AdvAxisTripLV:
			return caps.Has707
		case bus.AdvAxisTripHV:
			return caps.Has708
		case bus.AdvAxisTripLF:
			return caps.Has709
		case bus.AdvAxisTripHF:
			return caps.Has710
		case bus.AdvAxisFixedPF, bus.AdvAxisFixedVar:
			return caps.Has704
		case bus.AdvAxisEnergize:
			return caps.Has703
		}
		return false // freq_watt: no executing model in the 7xx profile
	case "12x":
		switch axis {
		case bus.AdvAxisFixedPF, bus.AdvAxisFixedVar:
			return caps.Has123
		}
		// Curve axes: the sunspec package declares no legacy curve models —
		// unsupported by decision (never invent register maps). Energize:
		// no 703, and M123 Conn belongs to the scalar shells (single-writer).
		return false
	default:
		return false // unknown generation: never write
	}
}

// ── Execution ────────────────────────────────────────────────────────────────

// executeLocked runs one reconciler Write action for an axis. In shadow mode
// it is a recorder: the would-write is counted and logged and NO driver
// exists to call. mu held.
func (s *advShell) executeLocked(rn *advAxisRunner, action reconcile.Action, now time.Time) {
	s.wouldWrites.Inc()
	if !s.active() {
		log.Printf("lexa-modbus: reconciler-adv[shadow] %s/%s: would=%s (reason=%s)",
			s.device, rn.axis, describeAdvTarget(rn.target), action.Reason)
		return
	}
	if len(rn.target.fields) == 0 {
		// No-opinion axis (released trips/droop/freq_watt, energize nil):
		// deliberately nothing to enforce — see advAxisTargets.
		return
	}

	caps, err := s.capsLocked()
	if err != nil {
		// Offline (or shadow-less surface): keep state, reconnect reasserts.
		log.Printf("lexa-modbus: reconciler-adv[active] %s/%s: device unavailable (%v) — will reassert on reconnect (reason=%s)",
			s.device, rn.axis, err, action.Reason)
		return
	}
	sup := axisSupported(rn.axis, s.derGen, caps)

	if !rn.target.engaged {
		// RELEASE enforcement: verify the function is disabled; disable it if
		// not. A device without the model has nothing in force — trivially
		// released.
		if !sup {
			s.feedExecLocked(rn, 0, now)
			s.transitionLocked(rn, "", now)
			return
		}
		s.executeReleaseLocked(rn, now)
		return
	}

	if !sup {
		s.transitionLocked(rn, bus.AdoptStateUnsupported, now)
		return
	}

	switch rn.axis {
	case bus.AdvAxisVoltVar, bus.AdvAxisWattVar, bus.AdvAxisVoltWatt, bus.AdvAxisFreqWatt:
		s.executeCurveLocked(rn, now)
	case bus.AdvAxisTripLV, bus.AdvAxisTripHV, bus.AdvAxisTripLF, bus.AdvAxisTripHF:
		s.executeTripLocked(rn, now)
	case bus.AdvAxisFreqDroop:
		s.executeDroopLocked(rn, now)
	case bus.AdvAxisFixedPF:
		var werr error
		if s.derGen == "12x" {
			werr = s.driver.SetFixedPF12x(rn.target.fixedPF.PF, rn.target.fixedPF.OverExcited)
		} else {
			werr = s.driver.SetFixedPF(s.docRvrtTms, rn.target.fixedPF.PF, rn.target.fixedPF.OverExcited)
		}
		s.recordMeasuredWriteLocked(rn, werr, now)
	case bus.AdvAxisFixedVar:
		var werr error
		if s.derGen == "12x" {
			werr = s.driver.SetFixedVarPct12x(rn.target.fixedVarPct)
		} else {
			werr = s.driver.SetFixedVarPct(s.docRvrtTms, rn.target.fixedVarPct)
		}
		s.recordMeasuredWriteLocked(rn, werr, now)
	case bus.AdvAxisEnergize:
		// Interlock seniority: an energize-restoring write is suppressed
		// while Tier-0 has the pack force-disconnected (ledger L8). The
		// InterlockHold report stays shell-log-only + counted, matching the
		// scalar shells' documented contract (bus.ReconcileReport's doc:
		// transient/diagnostic kinds are not published); the interlock clears
		// itself on the poll after the fault clears and the backoff/reassert
		// path re-runs this write.
		if rn.target.energize && s.interlock != nil && s.interlock.isTripped(s.device) {
			s.interlockHolds.Inc()
			log.Printf("lexa-modbus: reconciler-adv[active] %s/%s: interlock HOLD (%s) — Tier-0 senior, energize-restore suppressed",
				s.device, rn.axis, reconcile.ReportInterlockHold)
			return
		}
		s.recordMeasuredWriteLocked(rn, s.driver.SetEnergize(rn.target.energize), now)
	}
}

// recordMeasuredWriteLocked accounts a measured-convergence axis write
// (fixed PF/var, energize): the write's success is NEVER convergence — the
// poll-loop synthesis judges that — so state moves to pending on success and
// failed on error. mu held.
func (s *advShell) recordMeasuredWriteLocked(rn *advAxisRunner, err error, now time.Time) {
	if err != nil {
		s.writeFailures.Inc()
		log.Printf("lexa-modbus: reconciler-adv[active] %s/%s: write failed: %v", s.device, rn.axis, err)
		s.transitionLocked(rn, bus.AdoptStateFailed, now)
		return
	}
	s.writes.Inc()
	log.Printf("lexa-modbus: reconciler-adv[active] %s/%s: applied %s", s.device, rn.axis, describeAdvTarget(rn.target))
	switch rn.adoptState {
	case bus.AdoptStateAdopted, bus.AdoptStateDiverged:
		// Keep: adopted (a slow-reassert refresh of an already-verified
		// target, e.g. the RvrtTms window) or diverged (a corrective write
		// in flight — the divergence EVIDENCE stands until a measurement
		// clears it; the write's success is never convergence).
	default:
		s.transitionLocked(rn, bus.AdoptStatePending, now)
	}
}

// executeReleaseLocked verifies-then-disables an axis function (Ena=0) and
// feeds the verified result. mu held.
func (s *advShell) executeReleaseLocked(rn *advAxisRunner, now time.Time) {
	read := s.driver.ReadFuncEnabled
	disable := s.driver.DisableFunc
	if s.derGen == "12x" {
		read = s.driver.Read12xEnabled
		disable = s.driver.Disable12x
	}
	on, err := read(rn.axis)
	if err != nil {
		log.Printf("lexa-modbus: reconciler-adv[active] %s/%s: release verify read failed: %v", s.device, rn.axis, err)
		return // transient: retry via backoff/reassert
	}
	if on {
		if derr := disable(rn.axis); derr != nil {
			s.writeFailures.Inc()
			log.Printf("lexa-modbus: reconciler-adv[active] %s/%s: release disable failed: %v", s.device, rn.axis, derr)
			s.feedExecLocked(rn, 1, now) // observed still-enabled ⇒ diverged ⇒ backoff retry
			return
		}
		s.writes.Inc()
		on, err = read(rn.axis)
		if err != nil {
			log.Printf("lexa-modbus: reconciler-adv[active] %s/%s: release re-read failed: %v", s.device, rn.axis, err)
			return
		}
	}
	if on {
		s.feedExecLocked(rn, 1, now)
		log.Printf("lexa-modbus: reconciler-adv[active] %s/%s: release NOT verified — function still enabled", s.device, rn.axis)
		return
	}
	s.feedExecLocked(rn, 0, now)
	s.transitionLocked(rn, "", now)
}

// executeCurveLocked is the §5(b) curve-axis flow: read live → no-op skip on
// hash match → template-preserving staged write (adopt handshake inside
// derbase) → RE-READ live → recompute hash → adopted only on match. mu held.
func (s *advShell) executeCurveLocked(rn *advAxisRunner, now time.Time) {
	c := *rn.target.curve
	type curveOps struct {
		read  func() (string, error) // returns readback hash
		write func() error
	}
	var ops curveOps
	switch rn.axis {
	case bus.AdvAxisVoltVar:
		ops = curveOps{
			read: func() (string, error) {
				rb, err := s.driver.ReadVoltVar()
				if err != nil {
					return "", err
				}
				return readbackHashVV(rn.axis, c, rb), nil
			},
			write: func() error {
				tmpl, err := s.driver.ReadVoltVar()
				if err != nil {
					return err
				}
				return s.driver.WriteVoltVar(vvFromDoc(tmpl, c))
			},
		}
	case bus.AdvAxisVoltWatt:
		ops = curveOps{
			read: func() (string, error) {
				rb, err := s.driver.ReadVoltWatt()
				if err != nil {
					return "", err
				}
				return readbackHashVW(rn.axis, c, rb), nil
			},
			write: func() error {
				tmpl, err := s.driver.ReadVoltWatt()
				if err != nil {
					return err
				}
				return s.driver.WriteVoltWatt(vwFromDoc(tmpl, c))
			},
		}
	case bus.AdvAxisWattVar:
		ops = curveOps{
			read: func() (string, error) {
				rb, err := s.driver.ReadWattVar()
				if err != nil {
					return "", err
				}
				return readbackHashWV(rn.axis, c, rb), nil
			},
			write: func() error {
				tmpl, err := s.driver.ReadWattVar()
				if err != nil {
					return err
				}
				return s.driver.WriteWattVar(wvFromDoc(tmpl, c))
			},
		}
	default:
		// freq_watt reaches here only if axisSupported ever admits it —
		// today it never does (no executing model).
		s.transitionLocked(rn, bus.AdoptStateUnsupported, now)
		return
	}

	// No-op re-adoption skip (D6): live already carries the desired content.
	if h, err := ops.read(); err == nil && h == rn.target.hash {
		s.feedExecLocked(rn, rn.target.fp, now)
		s.transitionLocked(rn, bus.AdoptStateAdopted, now)
		return
	}

	if err := ops.write(); err != nil {
		// AdptCrvRslt FAILED and transport errors both land here: failed,
		// retried on the ≥-readback-interval backoff.
		s.writeFailures.Inc()
		log.Printf("lexa-modbus: reconciler-adv[active] %s/%s: curve adopt failed: %v", s.device, rn.axis, err)
		s.feedExecLocked(rn, advMismatch(rn.target.fp), now)
		s.transitionLocked(rn, bus.AdoptStateFailed, now)
		return
	}
	s.writes.Inc()

	h, err := ops.read()
	if err != nil {
		log.Printf("lexa-modbus: reconciler-adv[active] %s/%s: post-adopt readback failed: %v", s.device, rn.axis, err)
		s.feedExecLocked(rn, advMismatch(rn.target.fp), now)
		s.transitionLocked(rn, bus.AdoptStateFailed, now)
		return
	}
	if h == rn.target.hash {
		s.feedExecLocked(rn, rn.target.fp, now)
		s.transitionLocked(rn, bus.AdoptStateAdopted, now)
		log.Printf("lexa-modbus: reconciler-adv[active] %s/%s: ADOPTED (readback hash verified %s)", s.device, rn.axis, shortHash(rn.target.hash))
		return
	}
	s.feedExecLocked(rn, advMismatch(rn.target.fp), now)
	s.transitionLocked(rn, bus.AdoptStateDiverged, now)
	log.Printf("lexa-modbus: reconciler-adv[active] %s/%s: DIVERGED — readback hash %s != desired %s",
		s.device, rn.axis, shortHash(h), shortHash(rn.target.hash))
}

// executeTripLocked is executeCurveLocked for the trip-set axes. mu held.
func (s *advShell) executeTripLocked(rn *advAxisRunner, now time.Time) {
	set := *rn.target.trips
	voltage := rn.axis == bus.AdvAxisTripLV || rn.axis == bus.AdvAxisTripHV
	hv := rn.axis == bus.AdvAxisTripHV
	hf := rn.axis == bus.AdvAxisTripHF

	read := func() (string, error) {
		if voltage {
			rb, err := s.driver.ReadVoltageTrip(hv)
			if err != nil {
				return "", err
			}
			return readbackHashVoltageTrip(rn.axis, set, rb), nil
		}
		rb, err := s.driver.ReadFreqTrip(hf)
		if err != nil {
			return "", err
		}
		return readbackHashFreqTrip(rn.axis, set, rb), nil
	}
	write := func() error {
		if voltage {
			return s.driver.WriteVoltageTrip(hv, voltageTripFromDoc(set))
		}
		return s.driver.WriteFreqTrip(hf, freqTripFromDoc(set))
	}

	if h, err := read(); err == nil && h == rn.target.hash {
		s.feedExecLocked(rn, rn.target.fp, now)
		s.transitionLocked(rn, bus.AdoptStateAdopted, now)
		return
	}
	if err := write(); err != nil {
		s.writeFailures.Inc()
		log.Printf("lexa-modbus: reconciler-adv[active] %s/%s: trip-set adopt failed: %v", s.device, rn.axis, err)
		s.feedExecLocked(rn, advMismatch(rn.target.fp), now)
		s.transitionLocked(rn, bus.AdoptStateFailed, now)
		return
	}
	s.writes.Inc()
	h, err := read()
	if err != nil {
		log.Printf("lexa-modbus: reconciler-adv[active] %s/%s: post-adopt readback failed: %v", s.device, rn.axis, err)
		s.feedExecLocked(rn, advMismatch(rn.target.fp), now)
		s.transitionLocked(rn, bus.AdoptStateFailed, now)
		return
	}
	if h == rn.target.hash {
		s.feedExecLocked(rn, rn.target.fp, now)
		s.transitionLocked(rn, bus.AdoptStateAdopted, now)
		log.Printf("lexa-modbus: reconciler-adv[active] %s/%s: ADOPTED (readback hash verified %s)", s.device, rn.axis, shortHash(rn.target.hash))
		return
	}
	s.feedExecLocked(rn, advMismatch(rn.target.fp), now)
	s.transitionLocked(rn, bus.AdoptStateDiverged, now)
	log.Printf("lexa-modbus: reconciler-adv[active] %s/%s: DIVERGED — readback hash %s != desired %s",
		s.device, rn.axis, shortHash(h), shortHash(rn.target.hash))
}

// executeDroopLocked adopts the 711 droop parameters: skip when the live
// control already matches (under quantization tolerance), else write
// (carrying the device's own PMin through — the doc has no PMin opinion) and
// verify by re-read. mu held.
func (s *advShell) executeDroopLocked(rn *advAxisRunner, now time.Time) {
	d := *rn.target.droop
	live, err := s.driver.ReadFreqDroop()
	if err != nil {
		log.Printf("lexa-modbus: reconciler-adv[active] %s/%s: read live droop failed: %v", s.device, rn.axis, err)
		s.feedExecLocked(rn, advMismatch(rn.target.fp), now)
		s.transitionLocked(rn, bus.AdoptStateFailed, now)
		return
	}
	if droopMatches(d, live) {
		s.feedExecLocked(rn, rn.target.fp, now)
		s.transitionLocked(rn, bus.AdoptStateAdopted, now)
		return
	}
	ctl := sunspec.FreqDroopCtl{
		DbOf: d.DbOfHz, DbUf: d.DbUfHz, KOf: d.KOf, KUf: d.KUf,
		RspTms: d.OlrtS, PMin: live.PMin,
	}
	if err := s.driver.WriteFreqDroop(ctl); err != nil {
		s.writeFailures.Inc()
		log.Printf("lexa-modbus: reconciler-adv[active] %s/%s: droop adopt failed: %v", s.device, rn.axis, err)
		s.feedExecLocked(rn, advMismatch(rn.target.fp), now)
		s.transitionLocked(rn, bus.AdoptStateFailed, now)
		return
	}
	s.writes.Inc()
	rb, err := s.driver.ReadFreqDroop()
	if err != nil {
		log.Printf("lexa-modbus: reconciler-adv[active] %s/%s: post-adopt readback failed: %v", s.device, rn.axis, err)
		s.feedExecLocked(rn, advMismatch(rn.target.fp), now)
		s.transitionLocked(rn, bus.AdoptStateFailed, now)
		return
	}
	if droopMatches(d, rb) {
		s.feedExecLocked(rn, rn.target.fp, now)
		s.transitionLocked(rn, bus.AdoptStateAdopted, now)
		log.Printf("lexa-modbus: reconciler-adv[active] %s/%s: ADOPTED (readback verified)", s.device, rn.axis)
		return
	}
	s.feedExecLocked(rn, advMismatch(rn.target.fp), now)
	s.transitionLocked(rn, bus.AdoptStateDiverged, now)
	log.Printf("lexa-modbus: reconciler-adv[active] %s/%s: DIVERGED — droop readback disagrees with desired", s.device, rn.axis)
}

// feedExecLocked hands an execution-time verification result to the axis's
// core as an Observed sample (AdvContent fingerprint semantics — see the
// Field's doc). The returned action is deliberately DISCARDED: a corrective
// write straight out of the verification read would recurse into the
// executor; the Tick/backoff path owns retries (and the just-run write has
// already advanced the backoff cursor, so the core returns None here in
// practice). mu held.
func (s *advShell) feedExecLocked(rn *advAxisRunner, val float64, now time.Time) {
	_, reports := rn.r.Observe(reconcile.Observed{
		Read:      map[reconcile.Field]float64{reconcile.AdvContent: val},
		Connected: true, Plausible: true, At: now,
	}, now)
	for _, rep := range reports {
		s.handleReportLocked(rn, rep)
	}
}

// ── Reports ──────────────────────────────────────────────────────────────────

// transitionLocked moves an axis's adoption state, edge-logging, counting,
// and publishing the retained report exactly once per transition. mu held.
func (s *advShell) transitionLocked(rn *advAxisRunner, state string, now time.Time) {
	if rn.adoptState == state {
		return
	}
	prev := rn.adoptState
	rn.adoptState = state
	switch state {
	case bus.AdoptStateAdopted:
		s.adopts.Inc()
	case bus.AdoptStateFailed, bus.AdoptStateDiverged:
		s.failures.Inc()
	case bus.AdoptStateUnsupported:
		s.unsupportedCtr.Inc()
	}
	log.Printf("lexa-modbus: reconciler-adv[%s] %s/%s: adopt_state %q -> %q",
		s.tag(), s.device, rn.axis, prev, state)
	s.publishReportLocked(rn, reconcile.ReportAdoptState.String(), rn.r.Episode(), now)
}

// handleReportLocked logs one core report and forwards the convergence-state
// kinds to MQTT (the breach-episode evidence path, TASK-031). mu held.
func (s *advShell) handleReportLocked(rn *advAxisRunner, rep reconcile.Report) {
	log.Printf("lexa-modbus: reconciler-adv[%s] %s/%s: report=%s episode=%d mrid=%q reject=%s",
		s.tag(), s.device, rn.axis, rep.Kind, rep.Episode, rep.MRID, rep.Reject)
	if rep.Kind != reconcile.ReportNonConvergedBegin && rep.Kind != reconcile.ReportNonConvergedEnd {
		return
	}
	if s.pub == nil {
		return
	}
	s.pub(bus.ReconcileReport{
		Envelope:    bus.Envelope{V: bus.ReconcileReportV},
		Kind:        rep.Kind.String(),
		DeviceClass: advClass,
		DeviceID:    s.device,
		MRID:        rep.MRID,
		Seq:         rep.Seq,
		IssuedAt:    rep.IssuedAt,
		Episode:     rep.Episode,
		Ts:          rep.At.Unix(),
		Axis:        rn.axis,
		AdoptState:  rn.adoptState,
		CurveHash:   s.curveHashFor(rn),
	})
}

// publishReportLocked publishes a shell-authored report (AdoptState
// transitions, InterlockHold) retained on the adv report topic. mu held.
func (s *advShell) publishReportLocked(rn *advAxisRunner, kind string, episode uint64, now time.Time) {
	if s.pub == nil {
		return // shadow / tests: log-only
	}
	s.pub(bus.ReconcileReport{
		Envelope:    bus.Envelope{V: bus.ReconcileReportV},
		Kind:        kind,
		DeviceClass: advClass,
		DeviceID:    s.device,
		MRID:        s.docMRID,
		Seq:         s.docSeq,
		IssuedAt:    s.docIssuedAt,
		Episode:     episode,
		Ts:          now.Unix(),
		Axis:        rn.axis,
		AdoptState:  rn.adoptState,
		CurveHash:   s.curveHashFor(rn),
	})
}

// curveHashFor returns the readback-verified content hash — populated only
// when the axis is adopted (§2.2: "content hash of the adopted curve").
func (s *advShell) curveHashFor(rn *advAxisRunner) string {
	if rn.adoptState == bus.AdoptStateAdopted {
		return rn.target.hash
	}
	return ""
}

// newAdvReportPublisher returns the shell.pub sink: retained per-device on
// lexa/reconcile/adv/{device}/report (the hub's existing SubReconcileReport
// wildcard lexa/reconcile/+/+/report matches it — verified: four segments,
// class slot "adv"). Retained = state, latest wins (AD-011 re-seed).
func newAdvReportPublisher(mc mqtt.Client) func(bus.ReconcileReport) {
	return func(msg bus.ReconcileReport) {
		topic := bus.ReconcileReportTopic(advClass, msg.DeviceID)
		if err := mqttutil.PublishJSONRetained(mc, topic, msg); err != nil {
			log.Printf("lexa-modbus: publish adv reconcile report (%s): %v", msg.DeviceID, err)
		}
	}
}

// describeAdvTarget renders an axis target for the would=/applied logs.
func describeAdvTarget(t advTarget) string {
	switch {
	case !t.engaged && t.enforce:
		return "release(disable)"
	case !t.engaged:
		return "(no opinion)"
	case t.curve != nil:
		return "curve(hash=" + shortHash(t.hash) + ")"
	case t.trips != nil:
		return "trips(hash=" + shortHash(t.hash) + ")"
	case t.droop != nil:
		return fmt.Sprintf("droop(dbof=%g,kof=%g)", t.droop.DbOfHz, t.droop.KOf)
	case t.fixedPF != nil:
		return fmt.Sprintf("fixed_pf(pf=%.3f,over_excited=%t)", t.fixedPF.PF, t.fixedPF.OverExcited)
	default:
		if _, ok := t.fields[reconcile.FixedVarPct]; ok {
			return fmt.Sprintf("fixed_var(pct=%g)", t.fixedVarPct)
		}
		if _, ok := t.fields[reconcile.Energize]; ok {
			return fmt.Sprintf("energize(%t)", t.energize)
		}
		return "(empty)"
	}
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	if h == "" {
		return "(none)"
	}
	return h
}

// runAdvShellTicker drives every adv shell's Tick on a fixed cadence from one
// dedicated goroutine (same lifecycle as the scalar shell tickers).
func runAdvShellTicker(shells map[string]*advShell, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for now := range t.C {
		for _, s := range shells {
			s.tick(now)
		}
	}
}
