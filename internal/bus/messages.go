package bus

// Measurement is published by the modbus service for each device poll.
// Pointer fields are omitted when the device does not report that quantity.
//
// The voltage field is named VoltageV (wire key "voltage_v"), not V/"v": this
// type is one of the ~15 top-level published types that embeds Envelope
// (TASK-018), and Envelope's own field is also named V with wire key "v" (the
// schema version). Go's JSON encoder resolves same-key conflicts between an
// embedded field and the struct's own field by depth — the shallower (own)
// field silently wins and the embedded one is dropped from the wire
// entirely — so keeping this field as V/"v" would have made every
// Measurement publish appear to stamp a version while actually never
// emitting "v" at all (verified: internal/bus/envelope_test.go's collision
// case). VoltageV/"voltage_v" also matches the naming EVSEState already
// uses for the same physical quantity, so this aligns Measurement with the
// existing convention rather than inventing a new one. Every caller that
// read/wrote the old V field (cmd/modbus, cmd/hub, cmd/api, cmd/telemetry)
// is updated in the same change; there is no other reader of the old wire
// key "v" as voltage to migrate.
type Measurement struct {
	Envelope
	Device   string   `json:"device"`
	W        *float64 `json:"w,omitempty"`         // net power (W): + discharge/gen, - charge/load
	VoltageV *float64 `json:"voltage_v,omitempty"` // voltage (V)
	Hz       *float64 `json:"hz,omitempty"`        // frequency (Hz)
	Ts       int64    `json:"ts"`                  // Unix seconds
}

// BattMetrics is published by the modbus service for battery-role devices after
// each successful SunSpec battery metrics read.
type BattMetrics struct {
	Envelope
	Device        string   `json:"device"`
	SOC           *float64 `json:"soc_pct,omitempty"`
	SOH           *float64 `json:"soh_pct,omitempty"`
	CapacityWh    *float64 `json:"capacity_wh,omitempty"`
	MaxChargeW    *float64 `json:"max_charge_w,omitempty"`
	MaxDischargeW *float64 `json:"max_discharge_w,omitempty"`
	Ts            int64    `json:"ts"`
}

// ActiveControl is published by the csip service after every discovery walk.
// Watt values already have the IEEE 2030.5 ActivePower multiplier applied.
// Source is "event", "default", or "none" (no programs / no active control).
type ActiveControl struct {
	Envelope
	Source      string   `json:"source"`
	MRID        string   `json:"mrid,omitempty"`
	Connect     *bool    `json:"connect,omitempty"`
	ExpLimW     *float64 `json:"exp_lim_w,omitempty"`   // export limit (W)
	ImpLimW     *float64 `json:"imp_lim_w,omitempty"`   // import limit (W)
	MaxLimW     *float64 `json:"max_lim_w,omitempty"`   // generation cap (W)
	FixedW      *float64 `json:"fixed_w,omitempty"`     // fixed dispatch (W)
	ClockOffset int64    `json:"clock_offset"`          // server_time − local_time (s)
	ValidUntil  int64    `json:"valid_until,omitempty"` // Unix seconds; 0 = no expiry
	Ts          int64    `json:"ts"`
}

// RewalkRequest is published by lexa-hub on TopicCSIPRewalk (TASK-042, not
// retained, QoS 1) to ask lexa-northbound to refresh the retained
// lexa/csip/control message immediately, outside its normal discovery
// cadence. Reason is "stale" (a retained control was adopted with an age —
// measured against its own Ts — exceeding the hub's configured
// retained_adoption_max_age_s) or "decode" (the retained payload failed to
// unmarshal at all). See TopicCSIPRewalk's doc for the full mechanism.
type RewalkRequest struct {
	Envelope
	Reason string `json:"reason"` // "stale" | "decode"
	Ts     int64  `json:"ts"`
}

// ComplianceAlert is published by the hub (orchestrator) on
// TopicCSIPComplianceAlert when it cannot meet an active CSIP control limit.
// Active distinguishes the onset (true) from the clear (false) of a breach so
// the northbound service posts exactly one CannotComply Response per episode.
type ComplianceAlert struct {
	Envelope
	MRID       string  `json:"mrid"`        // active DERControl that cannot be met
	LimitType  string  `json:"limit_type"`  // "import" | "export" | "generation"
	LimitW     float64 `json:"limit_w"`     // commanded limit (W)
	MeasuredW  float64 `json:"measured_w"`  // actual net/generation at the meter (W)
	ShortfallW float64 `json:"shortfall_w"` // how far over the limit (W)
	Reason     string  `json:"reason"`      // human-readable cause
	Active     bool    `json:"active"`      // true = breach onset, false = cleared
	Ts         int64   `json:"ts"`          // Unix seconds
	// EpisodeID names the breach episode this edge belongs to (TASK-031). The
	// breach-episode component (cmd/hub/breach.go) forms it once at onset
	// (mrid@issuedAt) and reuses it for the whole episode, across both evidence
	// sources (optimizer meter breaches + reconciler non-convergence). The
	// northbound responseTracker dedupes CannotComply POSTs by this ID when
	// present (falling back to MRID for pre-TASK-031 publishers and as a
	// hub-restart safety net), so both sources reporting the same real episode
	// yield exactly one CannotComply. Additive/omitempty: an alert without it is
	// a legacy publisher and dedupes by MRID as before.
	EpisodeID string `json:"episode_id,omitempty"`
}

// ReconcileReport is the device-level non-convergence evidence a reconciler
// shell (cmd/modbus battery/solar, cmd/ocpp EVSE) forwards to the hub's
// breach-episode component (TASK-031). It is the bus projection of an
// internal/reconcile.Report: "the hardware won't do what the active CSIP
// control asked", complementary to the optimizer's meter-level breach.
//
// Published RETAINED per device on ReconcileReportTopic(class, device) so the
// hub re-seeds current convergence state after its own restart (state, not an
// edge — the latest NonConvergedBegin/End wins). Only the two convergence-state
// kinds are published on this topic; transient/diagnostic report kinds
// (StaleDesired, Rejected*, SeqReset, InterlockHold) stay shell-log-only for
// now (TASK-031 scope).
type ReconcileReport struct {
	Envelope
	Kind        string `json:"kind"`                // reconcile.ReportKind.String() (NonConvergedBegin|NonConvergedEnd)
	DeviceClass string `json:"device_class"`        // battery | solar | evse
	DeviceID    string `json:"device_id"`           // device / EVSE station ID
	MRID        string `json:"mrid,omitempty"`      // active CSIP control the held intent derives from
	Seq         uint64 `json:"seq,omitempty"`       // seq of the held desired document
	IssuedAt    int64  `json:"issued_at,omitempty"` // held document's publisher wall clock (Unix s)
	Episode     uint64 `json:"episode"`             // per-reconciler monotonic episode counter
	Ts          int64  `json:"ts"`                  // report wall-clock time (Unix s)
}

// BattCommand is published by the hub (orchestrator) to the modbus service.
// Nil SetpointW means "leave unchanged".
type BattCommand struct {
	Envelope
	Device    string   `json:"device"`
	SetpointW *float64 `json:"setpoint_w,omitempty"` // + discharge, − charge (W)
	Connect   *bool    `json:"connect,omitempty"`
	Ts        int64    `json:"ts"`
}

// SolarCommand is published by the hub to the modbus service.
// Nil CurtailToW means "restore to full nameplate output".
type SolarCommand struct {
	Envelope
	Device     string   `json:"device"`
	CurtailToW *float64 `json:"curtail_to_w,omitempty"` // nil = uncurtailed
	Ts         int64    `json:"ts"`
}

// EVSEState is published by the ocpp service whenever connector state changes.
type EVSEState struct {
	Envelope
	StationID     string   `json:"station_id"`
	ConnectorID   int      `json:"connector_id"`
	Connected     bool     `json:"connected"`
	SessionActive bool     `json:"session_active"`
	CurrentA      *float64 `json:"current_a,omitempty"`
	MaxCurrentA   *float64 `json:"max_current_a,omitempty"`
	VoltageV      *float64 `json:"voltage_v,omitempty"`
	PowerW        *float64 `json:"power_w,omitempty"`
	SOC           *float64 `json:"soc_pct,omitempty"`
	EnergyWh      *float64 `json:"energy_wh,omitempty"`
	Status        string   `json:"status"`
	Ts            int64    `json:"ts"`
}

// EVSECommand is published by the hub to the ocpp service.
// MaxCurrentA == 0 means suspend the charging session.
type EVSECommand struct {
	Envelope
	StationID   string  `json:"station_id"`
	ConnectorID int     `json:"connector_id"`
	MaxCurrentA float64 `json:"max_current_a"`
	Ts          int64   `json:"ts"`
}

// ─── Pricing function set (IEEE 2030.5 §10.5) ───────────────────────────────

// PricingUpdate is published by the csip service after each discovery walk
// that finds a TariffProfile. It carries the full schedule of upcoming pricing
// intervals so the hub can make look-ahead battery dispatch decisions.
type PricingUpdate struct {
	Envelope
	TariffProfiles []TariffProfileMsg `json:"tariff_profiles"`
	Ts             int64              `json:"ts"`
}

// TariffProfileMsg is the per-profile slice of PricingUpdate.
type TariffProfileMsg struct {
	MRID                      string             `json:"mrid"`
	Description               string             `json:"description,omitempty"`
	Currency                  uint16             `json:"currency,omitempty"`      // ISO 4217
	PricePowerOfTenMultiplier int8               `json:"price_power_of_ten_mult"` // apply to Price values
	Primacy                   uint8              `json:"primacy"`
	RateCode                  string             `json:"rate_code,omitempty"`
	RateComponents            []RateComponentMsg `json:"rate_components,omitempty"`
}

// RateComponentMsg carries the upcoming price schedule for one rate direction.
type RateComponentMsg struct {
	MRID               string          `json:"mrid"`
	Description        string          `json:"description,omitempty"`
	NumberOfTouTiers   uint8           `json:"num_tou_tiers,omitempty"`
	ActiveIntervals    []TimeTariffMsg `json:"active_intervals,omitempty"`
	ScheduledIntervals []TimeTariffMsg `json:"scheduled_intervals,omitempty"`
}

// TimeTariffMsg is one pricing interval with its consumption tier prices.
// Price values are in units determined by TariffProfileMsg.PricePowerOfTenMultiplier.
type TimeTariffMsg struct {
	MRID          string          `json:"mrid"`
	Description   string          `json:"description,omitempty"`
	TouTier       uint8           `json:"tou_tier"`
	IntervalStart int64           `json:"interval_start"` // Unix seconds
	Duration      uint32          `json:"duration"`       // seconds
	Blocks        []PriceBlockMsg `json:"blocks,omitempty"`
}

// PriceBlockMsg is one consumption block within a TimeTariffMsg.
type PriceBlockMsg struct {
	ConsumptionBlock uint8 `json:"consumption_block"`
	Price            int32 `json:"price"`       // apply PricePowerOfTenMultiplier for real value
	StartValue       int64 `json:"start_value"` // cumulative commodity units at which this block starts
}

// ─── Billing function set (IEEE 2030.5 §10.7) ───────────────────────────────

// BillingUpdate is published by the csip service when billing data is available.
// It carries the current billing period summary for each customer agreement.
type BillingUpdate struct {
	Envelope
	CustomerAccounts []CustomerAccountMsg `json:"customer_accounts"`
	Ts               int64                `json:"ts"`
}

// CustomerAccountMsg is the per-account slice of BillingUpdate.
type CustomerAccountMsg struct {
	MRID         string                 `json:"mrid"`
	CustomerName string                 `json:"customer_name,omitempty"`
	Currency     uint16                 `json:"currency,omitempty"`
	Agreements   []CustomerAgreementMsg `json:"agreements,omitempty"`
}

// CustomerAgreementMsg summarises one service agreement's billing status.
type CustomerAgreementMsg struct {
	MRID            string             `json:"mrid"`
	Description     string             `json:"description,omitempty"`
	ServiceLocation string             `json:"service_location,omitempty"`
	BillingPeriods  []BillingPeriodMsg `json:"billing_periods,omitempty"`
}

// BillingPeriodMsg is a single billing period summary.
type BillingPeriodMsg struct {
	IntervalStart  int64  `json:"interval_start"`
	Duration       uint32 `json:"duration"`
	BillLastPeriod *int64 `json:"bill_last_period,omitempty"` // in currency micro-units
	BillToDate     *int64 `json:"bill_to_date,omitempty"`     // in currency micro-units
}

// ─── Flow Reservation function set (IEEE 2030.5 §10.9) ──────────────────────

// FlowReservationRequestMsg is published by the hub on
// lexa/csip/flowreservation/request when it wants to schedule a charging or
// discharging window. The csip service will POST the request to the utility
// server's EndDevice FlowReservationRequestList.
type FlowReservationRequestMsg struct {
	Envelope
	// MRID is the client-assigned identifier for this request (hex string).
	MRID        string `json:"mrid"`
	Description string `json:"description,omitempty"`

	// EnergyRequestedWh is the total energy transfer needed (Wh).
	EnergyRequestedWh *float64 `json:"energy_requested_wh,omitempty"`
	// PowerRequestedW is the desired charge/discharge rate (W).
	PowerRequestedW *float64 `json:"power_requested_w,omitempty"`
	// DurationRequested is the minimum charging duration needed (seconds).
	DurationRequested uint32 `json:"duration_requested"`

	// IntervalStart and IntervalDuration define the requested time window.
	IntervalStart    int64  `json:"interval_start"`
	IntervalDuration uint32 `json:"interval_duration"`

	Ts int64 `json:"ts"`
}

// FlowReservationStatusMsg is published by the csip service on
// lexa/csip/flowreservation/status after each discovery walk that finds
// FlowReservationResponses. The hub uses this to schedule EVSE charging windows.
type FlowReservationStatusMsg struct {
	Envelope
	Reservations []ReservationMsg `json:"reservations"`
	Ts           int64            `json:"ts"`
}

// ReservationMsg is one granted (or cancelled/superseded) flow reservation.
type ReservationMsg struct {
	MRID          string   `json:"mrid"`
	Subject       string   `json:"subject"`        // mRID of the FlowReservationRequest
	CurrentStatus uint8    `json:"current_status"` // 0=scheduled, 1=active, 2=cancelled, 3=superseded
	IntervalStart int64    `json:"interval_start"`
	Duration      uint32   `json:"duration"`
	EnergyAvailWh *float64 `json:"energy_avail_wh,omitempty"`
	PowerAvailW   *float64 `json:"power_avail_w,omitempty"`
}

// ─── DER 24-hour schedule (northbound → hub) ─────────────────────────────────

// DERScheduleMsg is published by lexa-northbound on lexa/northbound/schedule
// after each discovery walk. It carries the resolved 24-hour DER control plan,
// which lexa-hub uses for look-ahead battery dispatch and EVSE scheduling.
type DERScheduleMsg struct {
	Envelope
	WindowStart int64              `json:"window_start"` // Unix seconds
	WindowEnd   int64              `json:"window_end"`
	BuildTime   int64              `json:"build_time"`
	ClockOffset int64              `json:"clock_offset"` // server − local (s)
	Slots       []DERScheduleSlot  `json:"slots"`
	DERStatus   []DERStatusSummary `json:"der_status,omitempty"`
	Ts          int64              `json:"ts"`
}

// DERScheduleSlot is one time-contiguous segment of the 24-hour plan.
type DERScheduleSlot struct {
	Start       int64  `json:"start"` // Unix seconds
	End         int64  `json:"end"`
	Source      string `json:"source"` // "event", "default", or "none"
	MRID        string `json:"mrid,omitempty"`
	Description string `json:"description,omitempty"`
	ProgramMRID string `json:"program_mrid,omitempty"`
	Primacy     uint8  `json:"primacy,omitempty"`

	// Scalar operating modes — nil means not controlled in this slot.
	Connect       *bool    `json:"connect,omitempty"`
	Energize      *bool    `json:"energize,omitempty"`
	MaxLimW       *float64 `json:"max_lim_w,omitempty"`       // W
	FixedW        *float64 `json:"fixed_w,omitempty"`         // W (+ inject, − absorb)
	ExpLimW       *float64 `json:"exp_lim_w,omitempty"`       // W
	ImpLimW       *float64 `json:"imp_lim_w,omitempty"`       // W
	GenLimW       *float64 `json:"gen_lim_w,omitempty"`       // W
	LoadLimW      *float64 `json:"load_lim_w,omitempty"`      // W
	TargetW       *float64 `json:"target_w,omitempty"`        // W
	FixedVarPct   *float64 `json:"fixed_var_pct,omitempty"`   // % of rated VAr
	FixedPFAbsorb *float64 `json:"fixed_pf_absorb,omitempty"` // power factor × 100
	FixedPFInject *float64 `json:"fixed_pf_inject,omitempty"` // power factor × 100
	RampTms       *uint16  `json:"ramp_tms,omitempty"`        // hundredths of a second

	// Curve-linked modes — curves are summarized inline (not raw XML breakpoints).
	VoltVar  *DERCurveSummary `json:"volt_var,omitempty"`
	FreqWatt *DERCurveSummary `json:"freq_watt,omitempty"`
	WattPF   *DERCurveSummary `json:"watt_pf,omitempty"`
	VoltWatt *DERCurveSummary `json:"volt_watt,omitempty"`

	// Ride-through curves — present when the server commands specific ride-through behavior.
	HFRTMayTrip            *DERCurveSummary `json:"hfrt_may_trip,omitempty"`
	HFRTMustTrip           *DERCurveSummary `json:"hfrt_must_trip,omitempty"`
	HVRTMayTrip            *DERCurveSummary `json:"hvrt_may_trip,omitempty"`
	HVRTMomentaryCessation *DERCurveSummary `json:"hvrt_momentary_cessation,omitempty"`
	HVRTMustTrip           *DERCurveSummary `json:"hvrt_must_trip,omitempty"`
	LFRTMayTrip            *DERCurveSummary `json:"lfrt_may_trip,omitempty"`
	LFRTMustTrip           *DERCurveSummary `json:"lfrt_must_trip,omitempty"`
	LVRTMayTrip            *DERCurveSummary `json:"lvrt_may_trip,omitempty"`
	LVRTMomentaryCessation *DERCurveSummary `json:"lvrt_momentary_cessation,omitempty"`
	LVRTMustTrip           *DERCurveSummary `json:"lvrt_must_trip,omitempty"`

	// FreqDroop parameters — present when opModFreqDroop is commanded.
	FreqDroop *FreqDroopMsg `json:"freq_droop,omitempty"`
}

// DERCurveSummary carries the key fields of a resolved DERCurve.
type DERCurveSummary struct {
	MRID        string       `json:"mrid,omitempty"`
	Description string       `json:"description,omitempty"`
	CurveType   uint16       `json:"curve_type"`
	XMultiplier int8         `json:"x_mult,omitempty"`
	YMultiplier int8         `json:"y_mult,omitempty"`
	Points      []CurvePoint `json:"points,omitempty"`
}

// CurvePoint is one (x, y) breakpoint in a DERCurveSummary.
type CurvePoint struct {
	X int32 `json:"x"`
	Y int32 `json:"y"`
}

// FreqDroopMsg carries the inline frequency droop parameters from opModFreqDroop.
type FreqDroopMsg struct {
	DBuf        uint16 `json:"d_buf_mhz"`     // dead-band width (mHz)
	DF          uint16 `json:"d_f_mhz"`       // full-response deviation (mHz)
	DP          uint16 `json:"d_p"`           // W/Hz × 100
	OpenLoopTms uint16 `json:"open_loop_tms"` // hundredths of a second
	TResponse   uint16 `json:"t_response"`    // hundredths of a second
}

// DERStatusSummary carries the last-known operational status of one DER device.
type DERStatusSummary struct {
	DERHref          string   `json:"der_href,omitempty"`
	GenConnectStatus *uint8   `json:"gen_connect_status,omitempty"`
	InverterStatus   *uint8   `json:"inverter_status,omitempty"`
	OperationalMode  *uint8   `json:"operational_mode,omitempty"`
	StorageMode      *uint8   `json:"storage_mode,omitempty"`
	StateOfChargePct *float64 `json:"soc_pct,omitempty"`
	EstimatedWAvail  *float64 `json:"estimated_w_avail,omitempty"`
	ModesSupported   uint32   `json:"modes_supported,omitempty"`
}

// PlanLog is the optimizer's plan trace for one engine pass (TopicHubPlan).
// Decisions may be empty — the message is still published so its timestamp
// serves as an engine heartbeat.
type PlanLog struct {
	Envelope
	Ts        int64          `json:"ts"` // Unix seconds of the plan's evaluation
	Decisions []PlanDecision `json:"decisions,omitempty"`
	// ShadowDivergences is the running count of constraint-shadow divergent
	// ticks (TASK-059), included so the dashboard/QA can watch the shadow diff
	// rate without a metrics scrape. Additive, omitempty ⇒ absent (and the wire
	// version unchanged, PlanLogV) whenever the shadow harness is off or has
	// seen zero divergences; a legacy decoder ignores the unknown key.
	ShadowDivergences uint64 `json:"shadow_divergences,omitempty"`
}

// PlanDecision mirrors orchestrator.Decision for the bus.
type PlanDecision struct {
	Rule   string `json:"rule"`
	Reason string `json:"reason"`
	Impact string `json:"impact"`
}
