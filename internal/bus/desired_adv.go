package bus

// DesiredAdvanced bus contract (WP-9, standards-buildout C1/C3/C4 —
// architecture D6, NORMATIVE): the hub's advanced-DER author (cmd/hub/adv.go,
// gated behind hub.json `advanced_der:"on"`) publishes ONE retained document
// per inverter/battery on DesiredAdvTopic(device) — `lexa/desired/adv/{device}`,
// QoS 1 (PubQoS's non-measurement default) — carrying the device's resolved
// advanced operating point: the arbitrated reactive-power mode, the volt-watt /
// freq-watt overlays, frequency droop, trip/ride-through curve sets, energize,
// the DefaultDERControl ramp gradients, and the computed reversion window.
// The consumer is WP-10's cmd/modbus adv reconciler shell; until it lands the
// docs sit retained and harmless (cmd/modbus's SubDesired dispatch switches on
// the topic class and ignores "adv"; lexa-ocpp likewise).
//
// D6 rules carried in this shape:
//
//   - Mutual exclusivity of reactive modes is STRUCTURAL: ReactiveMode is ONE
//     field, so a reconciler can never observe torn multi-reactive state. The
//     hub (single author) owns the D7 arbitration that picks it.
//   - The five mode axes (reactive_mode, volt_watt, freq_watt, freq_droop,
//     trips) are always present on the wire — an un-commanded axis is an
//     EXPLICIT null (a release command: "no <axis> in force"), never an absent
//     key. This is AD-013's explicit-values-never-absence rule applied to
//     provisioning state: absence must never be readable as "keep whatever
//     curve happens to be adopted".
//   - Adoption state does NOT ride this doc (a desired doc is publisher-owned
//     intent; readback state on it would create a second writer) — it rides
//     the retained per-device ReconcileReport extension (WP-10, §2.2).
//   - Content hashes (AdvCurve.Hash / AdvTripSet.Hash — CurveSetContentHash
//     over the axis's canonical entries) let both sides skip no-op
//     re-adoptions after reconnect/restart.
type DesiredAdvanced struct {
	Envelope
	DeviceClass string `json:"device_class"` // DesiredClassBattery | DesiredClassSolar
	DeviceID    string `json:"device_id"`

	// ReactiveMode is the single arbitrated reactive-power mode (D7), or an
	// explicit null (no reactive mode commanded — release). NEVER omitempty:
	// see the type doc's explicit-null rule.
	ReactiveMode *AdvReactiveMode `json:"reactive_mode"`

	// VoltWatt / FreqWatt are concurrent active-power overlays (1547 §4.7):
	// they pass through arbitration untouched and coexist with ReactiveMode.
	VoltWatt *AdvCurve `json:"volt_watt"`
	FreqWatt *AdvCurve `json:"freq_watt"`

	// FreqDroop carries frequency-droop parameters in SunSpec model 711 Ctl
	// vocabulary (see AdvFreqDroop for the 2030.5→711 mapping), or explicit
	// null.
	FreqDroop *AdvFreqDroop `json:"freq_droop"`

	// Trips carries the trip/ride-through curve sets. Highest 1547 priority:
	// always passes through arbitration (D7 rule 3). Explicit null when the
	// active control links no ride-through curves.
	Trips *AdvTrips `json:"trips"`

	// Energize mirrors ActiveControl.Energize (opModEnergize — distinct from
	// connect). nil = no opinion (omitted), matching DesiredState's *T
	// convention for scalar opinions.
	Energize *bool `json:"energize,omitempty"`

	// SetGradW/SetSoftGradW are the DefaultDERControl-only ramp-rate defaults
	// (% of setMaxW per second, as decoded by the WP-8 publisher). nil on
	// event-sourced controls per the CSIP ramp-rate rule.
	SetGradW     *float64 `json:"set_grad_w,omitempty"`
	SetSoftGradW *float64 `json:"set_soft_grad_w,omitempty"`

	// RvrtTmsS is the computed device reversion window (C3): the authoring
	// hub's ValidUntil−serverNow clamped to [60 s, 24 h]; nil when the active
	// control carries no ValidUntil. Recomputed at every actual publish
	// (heartbeats refresh it); deliberately EXCLUDED from the author's
	// content-change comparison, like IssuedAt/Seq, so a ticking countdown
	// never forces a republish by itself.
	RvrtTmsS *int64 `json:"rvrt_tms_s,omitempty"`

	// Source attributes the intent: "csip-event" | "csip-default" | "none"
	// (no active control — the all-null release document).
	Source string `json:"source"`
	// MRID is the active CSIP control this document derives from (CannotComply
	// attribution); empty when Source is "none".
	MRID string `json:"mrid,omitempty"`
	// IssuedAt/Seq are the AD-013 staleness/replay pair, same semantics as
	// DesiredState (Seq per device, resets on publisher restart, disambiguated
	// by IssuedAt).
	IssuedAt int64  `json:"issued_at"`
	Seq      uint64 `json:"seq"`
}

// AdvReactiveMode reactive-power kinds (D6/D7 vocabulary). Note on WattVar:
// D6/D7 name the watt-shaped reactive axis "watt_var" (its SunSpec execution
// target is model 712, derbase.WriteWattVar), but IEEE 2030.5's carriage for
// it is opModWattPF (Table 19 curveType 2 — y values are PF×100, not var).
// The doc carries the resolved curve VERBATIM with its CurveType, so the
// executor always knows what the y axis means; a future genuine watt-var
// carriage rides the same axis with its own CurveType.
const (
	AdvReactiveFixedPF  = "fixed_pf"
	AdvReactiveFixedVar = "fixed_var"
	AdvReactiveVoltVar  = "volt_var"
	AdvReactiveWattVar  = "watt_var"
)

// AdvReactiveMode is the single reactive-power axis of a DesiredAdvanced doc.
// Exactly one of the three payload fields is populated, selected by Kind:
// FixedPF for "fixed_pf", FixedVarPct for "fixed_var", Curve for
// "volt_var"/"watt_var".
type AdvReactiveMode struct {
	Kind string `json:"kind"` // AdvReactive* vocabulary above

	// FixedPF (kind "fixed_pf") reuses ActiveControl's FixedPF shape
	// ({pf, over_excited}). When a control carries BOTH opModFixedPFInjectW
	// and opModFixedPFAbsorbW the author keeps the inject half (the
	// generation-side setting) and alarms the absorb half as an ignored mode
	// — the doc has one fixed-PF slot by design (D6).
	FixedPF *FixedPF `json:"fixed_pf,omitempty"`

	// FixedVarPct (kind "fixed_var") is the signed % of setMaxVar, straight
	// from ActiveControl.FixedVarPct.
	FixedVarPct *float64 `json:"fixed_var_pct,omitempty"`

	// Curve (kind "volt_var" / "watt_var") is the resolved curve content.
	Curve *AdvCurve `json:"curve,omitempty"`
}

// AdvCurve is one resolved curve on a DesiredAdvanced axis — the D6
// {curve_type, x_mult, y_mult, points≤10, hash} shape, carried verbatim from
// the matching CurveSetEntry (raw int32 breakpoints, multipliers never
// pre-applied — same reasoning as CurveSetEntry). YRefType is carried in
// addition to the D6-listed fields because it participates in the canonical
// content hash (CurveSetContentHash) — without it a reconciler could not
// recompute Hash from this doc + a device readback.
type AdvCurve struct {
	CurveType uint16       `json:"curve_type"`           // csipmodel CurveType* (Table 19)
	XMult     int8         `json:"x_mult,omitempty"`     // x-axis power-of-ten multiplier
	YMult     int8         `json:"y_mult,omitempty"`     // y-axis power-of-ten multiplier
	YRefType  uint8        `json:"y_ref_type,omitempty"` // Table 19 DERUnitRefType (0 = absent)
	Points    []CurvePoint `json:"points"`               // ordered (x, y) breakpoints, 1..10

	// Hash is CurveSetContentHash over the single canonical CurveSetEntry
	// this curve was resolved from (mode|curveType|mults|yRef|points) — the
	// per-axis no-op re-adoption key (D6), echoed back on the WP-10
	// ReconcileReport as curve_hash.
	Hash string `json:"hash"`
}

// AdvFreqDroop carries frequency-droop parameters in SunSpec model 711 Ctl
// vocabulary (DbOf/DbUf/KOf/KUf/OlrtS — the registers WP-10's
// derbase.WriteFreqDroop writes), converted by the authoring hub from the
// 2030.5 opModFreqDroop parameters (csipmodel.FreqDroop, carried on
// DERScheduleSlot.FreqDroop — see cmd/hub/adv.go's droopFromSchedule for the
// conversion and its 60 Hz nominal-frequency assumption).
type AdvFreqDroop struct {
	DbOfHz float64 `json:"dbof"`   // over-frequency dead band (Hz)
	DbUfHz float64 `json:"dbuf"`   // under-frequency dead band (Hz)
	KOf    float64 `json:"kof"`    // over-frequency per-unit droop slope
	KUf    float64 `json:"kuf"`    // under-frequency per-unit droop slope
	OlrtS  float64 `json:"olrt_s"` // open-loop response time (s)
}

// Adv trip-curve kinds (AdvTripCurve.Kind).
const (
	AdvTripMustTrip           = "must_trip"
	AdvTripMayTrip            = "may_trip"
	AdvTripMomentaryCessation = "momentary_cessation"
)

// AdvTrips groups the trip/ride-through curve sets by axis (D6:
// trips{lv,hv,lf,hf}). An axis with no commanded curves is omitted (the
// whole Trips field is an explicit null when NO axis has curves).
type AdvTrips struct {
	LV *AdvTripSet `json:"lv,omitempty"` // low-voltage ride-through (LVRT*)
	HV *AdvTripSet `json:"hv,omitempty"` // high-voltage ride-through (HVRT*)
	LF *AdvTripSet `json:"lf,omitempty"` // low-frequency ride-through (LFRT*)
	HF *AdvTripSet `json:"hf,omitempty"` // high-frequency ride-through (HFRT*)
}

// AdvTripSet is one trip axis's curves (≤3: must-trip, may-trip, momentary
// cessation — the LF/HF axes have no momentary-cessation mode in 2030.5) plus
// the axis-level content hash (CurveSetContentHash over the axis's canonical
// entries), the WP-10 re-adoption key for the whole set.
type AdvTripSet struct {
	Curves []AdvTripCurve `json:"curves"`
	Hash   string         `json:"hash"`
}

// AdvTripCurve is one ride-through curve bound to its trip kind.
type AdvTripCurve struct {
	Kind  string   `json:"kind"` // AdvTrip* vocabulary above
	Curve AdvCurve `json:"curve"`
}

// DesiredAdvTopic returns the retained advanced desired-state topic for a
// device (D6): lexa/desired/adv/{device}. "adv" occupies the {class} segment
// of the AD-013 desired-topic shape, so DeviceFromDesiredTopic extracts the
// device and the modbus/ocpp scalar reconcilers' class dispatch ignores these
// docs untouched (WP-10 is the first consumer).
func DesiredAdvTopic(device string) string {
	return "lexa/desired/adv/" + device
}

// SubDesiredAdv matches every retained advanced desired-state document
// (WP-10's cmd/modbus adv shell subscribes this; ACL rows in
// systemd/mosquitto-lexa.acl grant hub write / modbus read on it).
const SubDesiredAdv = "lexa/desired/adv/+"

// Finite is DesiredAdvanced's counterpart to Measurement.Finite (GAP-09):
// every *float64 (and the always-present droop parameters and FixedPF.PF) is
// checked. Curve breakpoints are raw int32 pairs — nothing to check there.
func (d DesiredAdvanced) Finite() error {
	if err := finite("set_grad_w", d.SetGradW); err != nil {
		return err
	}
	if err := finite("set_soft_grad_w", d.SetSoftGradW); err != nil {
		return err
	}
	if rm := d.ReactiveMode; rm != nil {
		if err := finite("reactive_mode.fixed_var_pct", rm.FixedVarPct); err != nil {
			return err
		}
		if rm.FixedPF != nil {
			if err := finiteVal("reactive_mode.fixed_pf.pf", rm.FixedPF.PF); err != nil {
				return err
			}
		}
	}
	if fd := d.FreqDroop; fd != nil {
		for _, f := range []struct {
			name string
			v    float64
		}{
			{"freq_droop.dbof", fd.DbOfHz},
			{"freq_droop.dbuf", fd.DbUfHz},
			{"freq_droop.kof", fd.KOf},
			{"freq_droop.kuf", fd.KUf},
			{"freq_droop.olrt_s", fd.OlrtS},
		} {
			if err := finiteVal(f.name, f.v); err != nil {
				return err
			}
		}
	}
	return nil
}
