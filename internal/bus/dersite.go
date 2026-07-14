package bus

// DERSiteReport bus contract (WP-4, standards-buildout A2 / CORE-009,
// CORE-014, BASIC-028): the hub's GFEMS aggregator (cmd/hub/dersite.go)
// publishes ONE site-level DER report — the D2 aggregate at the PCC — on
// TopicHubDERSite, RETAINED at QoS 1 (state, not an edge: latest wins, and a
// restarting lexa-northbound re-seeds from the broker). lexa-northbound's
// derreport manager (internal/northbound/derreport) converts it into
// csipmodel DERCapabilityFull / DERSettingsFull / DERStatusFull /
// DERAvailability and PUTs them to the hrefs the discovery walker observed
// on the self EndDevice's DERList entry.
//
// D2 (architecture.md, NORMATIVE) semantics carried here:
//   - Ratings (rtg_*) are physical sums at the PCC: Σ inverter nameplate
//     (+ Σ battery discharge rate — the batteries export), Σ battery
//     charge/discharge rates, Σ battery capacity Wh. EVSEs are EXCLUDED
//     until `ev_storage` exists.
//   - VA/Var ratings appear ONLY when device data exists (G27: omission over
//     fabrication) — hence *float64, nil-absent. No producer populates them
//     today; the fields exist so the wire shape doesn't change when a source
//     appears.
//   - Settings (set_*) are min(rtg, site policy) — ≤ ratings BY CONSTRUCTION
//     (CORE-014's operator check passes structurally; pinned by
//     cmd/hub/dersite_test.go).
//   - ModesSupported is a TRUTH MASK of csipmodel.Mode* bits the hub
//     actually enforces end-to-end — never advertise what a CannotComply
//     would immediately contradict. Derivation lives hub-side
//     (cmd/hub/dersite.go enforcedModes).
type DERSiteReport struct {
	Envelope

	// DERType is the 2030.5 DERCapability `type` code for the aggregate
	// (DERType* constants below); config override `der_type` in hub.json
	// wins over the derived value (D2: utility-handbook fiat).
	DERType uint8 `json:"der_type"`

	// ModesSupported is the csipmodel.Mode* truth mask (see type doc).
	ModesSupported uint32 `json:"modes_supported"`

	// Ratings — always-present physical sums (0 when the site has no source
	// for a rating, e.g. rtg_max_wh with no batteries).
	RtgMaxW              float64 `json:"rtg_max_w"`
	RtgMaxChargeRateW    float64 `json:"rtg_max_charge_rate_w"`
	RtgMaxDischargeRateW float64 `json:"rtg_max_discharge_rate_w"`
	RtgMaxWh             float64 `json:"rtg_max_wh"`
	// VA/Var ratings — nil unless real device data exists (G27; see type doc).
	RtgMaxVA  *float64 `json:"rtg_max_va,omitempty"`
	RtgMaxVar *float64 `json:"rtg_max_var,omitempty"`

	// Settings — operational caps, ≤ the matching rating by construction.
	SetMaxW              float64  `json:"set_max_w"`
	SetMaxChargeRateW    float64  `json:"set_max_charge_rate_w"`
	SetMaxDischargeRateW float64  `json:"set_max_discharge_rate_w"`
	SetMaxWh             float64  `json:"set_max_wh"`
	SetMaxVA             *float64 `json:"set_max_va,omitempty"`
	SetMaxVar            *float64 `json:"set_max_var,omitempty"`

	// Status is the live aggregate status block (Table 13 / G30 source).
	Status DERSiteStatus `json:"status"`

	// Avail is the availability block (statWAvail etc.) — nil when nothing
	// is derivable (no fresh generation or storage data; G27 again).
	Avail *DERSiteAvailability `json:"avail,omitempty"`

	// ContentHash is a stable hash of the CAPABILITY/SETTINGS-scoped content
	// only — der_type, modes_supported, and every rtg_*/set_* field — and
	// deliberately EXCLUDES the live Status/Avail blocks and Ts. It is the
	// G29 on-change trigger for the northbound DERCapability/DERSettings
	// PUTs: SoC/output jitter re-publishes this retained doc (bounded by the
	// hub's 60 s min republish interval) but must never re-PUT nameplate
	// data the server already has. Computed hub-side (dersiteCapHash) so
	// every subscriber agrees on one value.
	ContentHash string `json:"content_hash"`

	// Ts is the publish wall-clock time (hub-local Unix seconds). Refreshed
	// on every publish, including unchanged-content heartbeats.
	Ts int64 `json:"ts"`
}

// DERSiteStatus is DERSiteReport's live status block — the source material
// for the 2030.5 DERStatus PUT (Table 13, G30).
type DERSiteStatus struct {
	// SocPct is the capacity-weighted battery state of charge across the
	// site's packs, percent [0,100], rounded to 0.1. nil when no battery has
	// reported SOC (never fabricated).
	SocPct *float64 `json:"soc_pct,omitempty"`

	// GenConnectStatus is the aggregate generator connection state using the
	// csipmodel.GenConnect* codes (0 = available/not connected,
	// 1 = connected and operating).
	GenConnectStatus uint8 `json:"gen_connect_status"`

	// OperationalMode is the aggregate operational state using the
	// csipmodel.OpStatus* codes (0 = idle, 1 = operating).
	OperationalMode uint8 `json:"operational_mode"`

	// StorageMode is the aggregate storage state (csipmodel.Storage* codes:
	// 0 idle, 1 charging, 2 discharging), from the sign of the summed battery
	// net power. nil when the site has no fresh battery measurement.
	StorageMode *uint8 `json:"storage_mode,omitempty"`

	// AlarmBits is the 2030.5 DERStatus alarmStatus category bitmap (the
	// DERAlarm* constants below — bit = Table 14 alarm code / 2), OR'd across
	// every device's mapped 701 alarm bits plus the site-level breach episode
	// (EMERGENCY_REMOTE — the same condition WP-6's LogEvent pipeline posts).
	AlarmBits uint32 `json:"alarm_bits"`

	// ReadingTs is when this status block was computed (hub-local Unix
	// seconds); the northbound PUT converts it to server time.
	ReadingTs int64 `json:"reading_ts"`
}

// DERSiteAvailability is DERSiteReport's availability block — source
// material for the 2030.5 DERAvailability PUT. Every field is optional:
// derived only where the inputs exist (G27).
type DERSiteAvailability struct {
	// EstimatedWAvailW is the real power the site could deliver right now:
	// current inverter output plus available battery discharge.
	EstimatedWAvailW *float64 `json:"estimated_w_avail,omitempty"`

	// AvailabilityDurationS is how long (s) the site could sustain its max
	// discharge from the energy currently stored.
	AvailabilityDurationS *uint32 `json:"availability_duration_s,omitempty"`

	// MaxChargeDurationS is how long (s) the site could sustain its max
	// charge rate into the remaining battery headroom.
	MaxChargeDurationS *uint32 `json:"max_charge_duration_s,omitempty"`
}

// DERType* are the 2030.5 DERCapability `type` codes this repo emits.
// D2 (NORMATIVE) fixes 83 as the GFEMS aggregate's default for the product's
// common inverters+batteries case; the csipmodel vocabulary comment reads 83
// as "storage", so the two agree on the value for every mix this code
// derives. `der_type` in hub.json overrides for utility-handbook fiat.
const (
	DERTypeVirtualOrMixed uint8 = 1  // heterogeneous mix / EVSE-as-DER (ev_storage, future)
	DERTypePV             uint8 = 80 // PV-only site
	DERTypeStorage        uint8 = 83 // storage / PV+storage GFEMS aggregate (D2 default)
)

// DERAlarm* are the 2030.5 DERStatus.alarmStatus category bits. The bit
// position is the CSIP Table 14 alarm (even) code divided by 2 — see
// DERAlarmBitForCode — so the LogEvent vocabulary in logevent.go and this
// bitmap stay one mapping apart, never two.
const (
	DERAlarmOverCurrent      uint32 = 1 << 0
	DERAlarmOverVoltage      uint32 = 1 << 1
	DERAlarmUnderVoltage     uint32 = 1 << 2
	DERAlarmOverFrequency    uint32 = 1 << 3
	DERAlarmUnderFrequency   uint32 = 1 << 4
	DERAlarmVoltageImbalance uint32 = 1 << 5
	DERAlarmCurrentImbalance uint32 = 1 << 6
	DERAlarmEmergencyLocal   uint32 = 1 << 7
	DERAlarmEmergencyRemote  uint32 = 1 << 8
	DERAlarmLowPowerInput    uint32 = 1 << 9
	DERAlarmPhaseRotation    uint32 = 1 << 10
)

// DERAlarmBitForCode maps a CSIP Table 14 alarm/RTN code onto its
// DERStatus.alarmStatus category bit (code/2 — an RTN maps to the same
// category as its paired alarm). Codes outside Table 14 return 0.
func DERAlarmBitForCode(code uint8) uint32 {
	if !LogEventCodeValid(code) {
		return 0
	}
	return 1 << (code / 2)
}

// Finite is DERSiteReport's counterpart to Measurement.Finite (GAP-09):
// always-present ratings/settings via finiteVal, optional fields via
// finite's nil-skip wrapper, plus the nested status/availability blocks.
func (r DERSiteReport) Finite() error {
	if err := finiteVal("rtg_max_w", r.RtgMaxW); err != nil {
		return err
	}
	if err := finiteVal("rtg_max_charge_rate_w", r.RtgMaxChargeRateW); err != nil {
		return err
	}
	if err := finiteVal("rtg_max_discharge_rate_w", r.RtgMaxDischargeRateW); err != nil {
		return err
	}
	if err := finiteVal("rtg_max_wh", r.RtgMaxWh); err != nil {
		return err
	}
	if err := finite("rtg_max_va", r.RtgMaxVA); err != nil {
		return err
	}
	if err := finite("rtg_max_var", r.RtgMaxVar); err != nil {
		return err
	}
	if err := finiteVal("set_max_w", r.SetMaxW); err != nil {
		return err
	}
	if err := finiteVal("set_max_charge_rate_w", r.SetMaxChargeRateW); err != nil {
		return err
	}
	if err := finiteVal("set_max_discharge_rate_w", r.SetMaxDischargeRateW); err != nil {
		return err
	}
	if err := finiteVal("set_max_wh", r.SetMaxWh); err != nil {
		return err
	}
	if err := finite("set_max_va", r.SetMaxVA); err != nil {
		return err
	}
	if err := finite("set_max_var", r.SetMaxVar); err != nil {
		return err
	}
	if err := finite("status.soc_pct", r.Status.SocPct); err != nil {
		return err
	}
	if r.Avail != nil {
		if err := finite("avail.estimated_w_avail", r.Avail.EstimatedWAvailW); err != nil {
			return err
		}
	}
	return nil
}
