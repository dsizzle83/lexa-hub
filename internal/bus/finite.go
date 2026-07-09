package bus

import (
	"fmt"
	"math"
)

// finite reports whether p is a safe bus value: nil (the absent-value
// convention — TASK-017/nan_test.go — always passes) or a finite float64. A
// non-nil NaN/±Inf is the case this file exists to catch: it should never
// happen from a lexa publisher (json.Marshal already fails on a *float64
// pointing to NaN, per nan_test.go's TestBusMessagesNaNPointerIsInvalid) and
// json.Unmarshal already rejects bare/quoted "NaN"/"Infinity" into a typed
// float64/*float64 field (GAP-09 review §9's crux). This is defense in
// depth for the residual: a lax future decoder (UseNumber, interface{},
// map[string]any, a third-party JSON lib) that lets a non-finite value
// through as something a later ParseFloat turns into a live NaN/Inf before
// it reaches a Finite() call. name is folded into the returned error so a
// caller — and the alarm log in mqttutil.Subscribe — can name the offending
// field.
func finite(name string, p *float64) error {
	if p == nil {
		return nil
	}
	return finiteVal(name, *p)
}

// finiteVal is finite's counterpart for message fields that are plain
// float64 (not pointers) — e.g. ComplianceAlert's always-present limit/
// measured/shortfall watts, which have no "absent value" convention to begin
// with, so there is no nil case to skip.
func finiteVal(name string, v float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("bus: field %q is non-finite (%v)", name, v)
	}
	return nil
}

// Finite reports whether every numeric field is nil (absent) or finite.
// Called by mqttutil.Subscribe (via an interface{ Finite() error } type
// assertion) immediately after a successful json.Unmarshal, so a value that
// slipped past the decoder as something other than a rejected-outright
// bare/quoted NaN/Inf token is still caught before it reaches handler —
// belt-and-suspenders on top of stdlib's existing rejection, not a
// replacement for it.
func (m Measurement) Finite() error {
	if err := finite("w", m.W); err != nil {
		return err
	}
	if err := finite("voltage_v", m.VoltageV); err != nil {
		return err
	}
	if err := finite("hz", m.Hz); err != nil {
		return err
	}
	return nil
}

// Finite is BattMetrics' counterpart to Measurement.Finite.
func (b BattMetrics) Finite() error {
	if err := finite("soc_pct", b.SOC); err != nil {
		return err
	}
	if err := finite("soh_pct", b.SOH); err != nil {
		return err
	}
	if err := finite("capacity_wh", b.CapacityWh); err != nil {
		return err
	}
	if err := finite("max_charge_w", b.MaxChargeW); err != nil {
		return err
	}
	if err := finite("max_discharge_w", b.MaxDischargeW); err != nil {
		return err
	}
	return nil
}

// Finite is ActiveControl's counterpart to Measurement.Finite. This is the
// safety-critical case (GAP-09's payoff): ExpLimW/ImpLimW/MaxLimW/FixedW are
// exactly the values cmd/hub's optimizer treats as authoritative export/
// import/generation/dispatch caps — a NaN here must never be adopted as a
// live limit, only ever cause the whole message to be dropped (fail-closed,
// last-known-good control holds).
func (a ActiveControl) Finite() error {
	if err := finite("exp_lim_w", a.ExpLimW); err != nil {
		return err
	}
	if err := finite("imp_lim_w", a.ImpLimW); err != nil {
		return err
	}
	if err := finite("max_lim_w", a.MaxLimW); err != nil {
		return err
	}
	if err := finite("fixed_w", a.FixedW); err != nil {
		return err
	}
	return nil
}

// Finite is ComplianceAlert's counterpart to Measurement.Finite. Unlike the
// other types here, ComplianceAlert's numeric fields are plain float64 (no
// absent-value convention — a compliance alert always carries real limit/
// measured/shortfall wattage), so finiteVal is used directly rather than
// finite's nil-skip wrapper.
func (c ComplianceAlert) Finite() error {
	if err := finiteVal("limit_w", c.LimitW); err != nil {
		return err
	}
	if err := finiteVal("measured_w", c.MeasuredW); err != nil {
		return err
	}
	if err := finiteVal("shortfall_w", c.ShortfallW); err != nil {
		return err
	}
	return nil
}

// Finite is EVSEState's counterpart to Measurement.Finite.
func (e EVSEState) Finite() error {
	if err := finite("current_a", e.CurrentA); err != nil {
		return err
	}
	if err := finite("max_current_a", e.MaxCurrentA); err != nil {
		return err
	}
	if err := finite("voltage_v", e.VoltageV); err != nil {
		return err
	}
	if err := finite("power_w", e.PowerW); err != nil {
		return err
	}
	if err := finite("soc_pct", e.SOC); err != nil {
		return err
	}
	if err := finite("energy_wh", e.EnergyWh); err != nil {
		return err
	}
	return nil
}

// Finite is DERScheduleSlot's counterpart to Measurement.Finite. Only the
// scalar operating-mode fields are checked; the curve-linked fields
// (VoltVar, FreqWatt, ...) carry int32 breakpoints (CurvePoint), not
// *float64, so they are outside this task's scope (GAP-09 is about *float64
// bus fields).
func (s DERScheduleSlot) Finite() error {
	if err := finite("max_lim_w", s.MaxLimW); err != nil {
		return err
	}
	if err := finite("fixed_w", s.FixedW); err != nil {
		return err
	}
	if err := finite("exp_lim_w", s.ExpLimW); err != nil {
		return err
	}
	if err := finite("imp_lim_w", s.ImpLimW); err != nil {
		return err
	}
	if err := finite("gen_lim_w", s.GenLimW); err != nil {
		return err
	}
	if err := finite("load_lim_w", s.LoadLimW); err != nil {
		return err
	}
	if err := finite("target_w", s.TargetW); err != nil {
		return err
	}
	if err := finite("fixed_var_pct", s.FixedVarPct); err != nil {
		return err
	}
	if err := finite("fixed_pf_absorb", s.FixedPFAbsorb); err != nil {
		return err
	}
	if err := finite("fixed_pf_inject", s.FixedPFInject); err != nil {
		return err
	}
	return nil
}

// Finite is DERScheduleMsg's counterpart to Measurement.Finite. DERScheduleMsg
// itself has no bare *float64 fields — its numeric payload lives in Slots —
// so this walks the slots and delegates to DERScheduleSlot.Finite. This
// matters because mqttutil.Subscribe's Finite() type assertion runs against
// the top-level decoded type (T = DERScheduleMsg for
// bus.TopicNorthboundSchedule), not the nested slice element; without this
// method a non-finite value in a slot would never be checked.
func (d DERScheduleMsg) Finite() error {
	for i, s := range d.Slots {
		if err := s.Finite(); err != nil {
			return fmt.Errorf("slots[%d]: %w", i, err)
		}
	}
	return nil
}

// Finite is EVGoalIntent's counterpart to Measurement.Finite (TASK-082).
func (g EVGoalIntent) Finite() error {
	if err := finite("target_soc_kwh", g.TargetSocKwh); err != nil {
		return err
	}
	if err := finite("initial_soc_kwh", g.InitialSocKwh); err != nil {
		return err
	}
	if err := finite("capacity_kwh", g.CapacityKwh); err != nil {
		return err
	}
	return nil
}

// Finite is BackupReserveIntent's counterpart to Measurement.Finite (TASK-082).
func (r BackupReserveIntent) Finite() error {
	return finite("reserve_pct", r.ReservePct)
}

// Finite is SolarForecastIntent's counterpart to Measurement.Finite
// (TASK-082). StepKw is a plain []float64 (no absent-value convention for a
// slice element — an entry that shouldn't carry an opinion is simply not in
// the slice), so each entry is checked with finiteVal rather than finite's
// nil-skip wrapper, same reasoning as DERScheduleMsg's slot walk above.
func (f SolarForecastIntent) Finite() error {
	for i, v := range f.StepKw {
		if err := finiteVal("step_kw", v); err != nil {
			return fmt.Errorf("step_kw[%d]: %w", i, err)
		}
	}
	return nil
}

// Finite is LoadProfileIntent's counterpart to Measurement.Finite (TASK-082),
// following SolarForecastIntent.Finite's per-entry StepKw walk.
func (l LoadProfileIntent) Finite() error {
	for i, v := range l.StepKw {
		if err := finiteVal("step_kw", v); err != nil {
			return fmt.Errorf("step_kw[%d]: %w", i, err)
		}
	}
	return nil
}

// Finite is TariffIntent's counterpart to Measurement.Finite (TASK-082): it
// walks Tariff.Periods and checks each period's rates — ImportPerKwh (always
// present, via finiteVal) and ExportPerKwh (optional, via finite's nil-skip
// wrapper).
func (t TariffIntent) Finite() error {
	for i, p := range t.Tariff.Periods {
		if err := finiteVal("import_per_kwh", p.ImportPerKwh); err != nil {
			return fmt.Errorf("tariff.periods[%d]: %w", i, err)
		}
		if err := finite("export_per_kwh", p.ExportPerKwh); err != nil {
			return fmt.Errorf("tariff.periods[%d]: %w", i, err)
		}
	}
	return nil
}

// Finite is ScanResult's counterpart to Measurement.Finite (TASK-082): it
// walks Devices and checks each hit's NameplateW.
func (s ScanResult) Finite() error {
	for i, d := range s.Devices {
		if err := finite("nameplate_w", d.NameplateW); err != nil {
			return fmt.Errorf("devices[%d]: %w", i, err)
		}
	}
	return nil
}
