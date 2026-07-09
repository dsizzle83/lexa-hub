package bus

// Intent message types (TASK-082, docs/DEVICE_ROADMAP.md §1.2): the bus
// contract for cloud/app/CLI-originated goals flowing into lexa-hub, and the
// hub's own mode/status state flowing back out. Every type here follows the
// house rules already established for DesiredState/ActiveControl: Envelope
// embedded by value (the "v" schema-version key), *float64 for optional
// quantities (never NaN on the wire — see finite.go's Finite() additions for
// this file), and no field colliding with the embedded "v" key (see
// Measurement's doc comment in messages.go for why that collision is a live
// landmine, not a hypothetical one).
//
// IntentMeta is embedded (not a named field) in every request-kind intent —
// ModeIntent, EVGoalIntent, BackupReserveIntent, SolarForecastIntent,
// LoadProfileIntent, TariffIntent, ChargeNowIntent — so ID/Origin/Actor/
// IssuedAt/TTLS ride inline on each one's wire shape. IntentResult and
// ModeStatus are hub-authored REPLIES, not requests, so they carry their own
// ID/Ts fields directly rather than embedding IntentMeta.

// IntentMeta is common to every intent kind. Origin/Actor make the journal
// audit trail meaningful; ID makes retained redelivery idempotent; TTLS is
// mandatory for edge kinds (chargenow) and ignored for state kinds.
type IntentMeta struct {
	ID       string `json:"id"`              // caller-generated, unique
	Origin   string `json:"origin"`          // "cloud" | "app" | "cli"
	Actor    string `json:"actor,omitempty"` // user email / token id / "root"
	IssuedAt int64  `json:"issued_at"`       // Unix seconds at the source
	TTLS     int    `json:"ttl_s,omitempty"` // edge intents only
}

// ModeIntent — lexa/intent/mode (retained). Requests a switch between the
// hub's optimizer and gateway plan authors (§3.5).
type ModeIntent struct {
	Envelope
	IntentMeta
	Mode string `json:"mode"` // "optimizer" | "gateway"
}

// EVGoalIntent — lexa/intent/evgoal (retained). kWh terms: the app/cloud
// resolves per-weekday defaults and %→kWh before publishing; the hub stays
// unit-simple (PlannerParams already works in kWh).
type EVGoalIntent struct {
	Envelope
	IntentMeta
	StationID     string   `json:"station_id,omitempty"` // empty = the single/default station
	TargetSocKwh  *float64 `json:"target_soc_kwh"`
	DepartureUnix int64    `json:"departure_unix"`
	InitialSocKwh *float64 `json:"initial_soc_kwh,omitempty"` // user estimate at plug-in ("estimated" in UI)
	CapacityKwh   *float64 `json:"capacity_kwh,omitempty"`    // user-stated vehicle pack size
}

// BackupReserveIntent — lexa/intent/reserve (retained). The hub clamps to
// >= the configured safety floor; intents can only RAISE the reserve.
type BackupReserveIntent struct {
	Envelope
	IntentMeta
	ReservePct *float64 `json:"reserve_pct"`
}

// SolarForecastIntent — lexa/intent/solarforecast (retained). StepKw is on
// the planner's 5-min grid starting at WindowStart (<=288 entries; shorter
// is zero-filled by the planner, same rule as SolarForecastKw today).
type SolarForecastIntent struct {
	Envelope
	IntentMeta
	WindowStart int64     `json:"window_start"` // Unix seconds, 5-min aligned
	StepKw      []float64 `json:"step_kw"`
	SourceTs    int64     `json:"source_ts"` // when the weather model ran (staleness input)
}

// LoadProfileIntent — lexa/intent/loadprofile (retained). Same grid rules.
type LoadProfileIntent struct {
	Envelope
	IntentMeta
	StepKw []float64 `json:"step_kw"`
}

// TariffIntent — lexa/intent/tariff (retained). Compiled on the hub into a
// TOUCostModel; CSIP-published pricing (SetPrices arrays) still wins when
// present, by the planner's existing nil-slice fallback rule.
type TariffIntent struct {
	Envelope
	IntentMeta
	Tariff TariffSpec `json:"tariff"`
}

// TariffSpec is the compiled-from-intent tariff shape (a user/cloud-supplied
// counterpart to PricingUpdate's CSIP-derived schedule).
type TariffSpec struct {
	Currency string         `json:"currency"` // "USD"
	Periods  []TariffPeriod `json:"periods"`
}

// TariffPeriod is one recurring rate window within a TariffSpec.
type TariffPeriod struct {
	Label        string   `json:"label"`    // "peak", "off-peak", …
	Days         []int    `json:"days"`     // 0=Sun … 6=Sat
	StartHH      int      `json:"start_hh"` // local tariff-zone hour, inclusive
	EndHH        int      `json:"end_hh"`   // exclusive
	ImportPerKwh float64  `json:"import_per_kwh"`
	ExportPerKwh *float64 `json:"export_per_kwh,omitempty"`
}

// ChargeNowIntent — lexa/intent/chargenow (NOT retained; TTLS mandatory).
type ChargeNowIntent struct {
	Envelope
	IntentMeta
	StationID string `json:"station_id,omitempty"`
}

// IntentResult — lexa/intent/result (not retained). One per received intent.
type IntentResult struct {
	Envelope
	ID      string `json:"id"`      // echoes IntentMeta.ID
	Kind    string `json:"kind"`    // "mode" | "evgoal" | …
	Outcome string `json:"outcome"` // "applied" | "clamped" | "rejected" | "expired" | "duplicate"
	Detail  string `json:"detail,omitempty"`
	Ts      int64  `json:"ts"`
}

// ModeStatus — lexa/hub/mode (retained). Authoritative mode state; also the
// hub's own restart re-seed (subscribe-own-retained, like breach snapshots).
type ModeStatus struct {
	Envelope
	Mode     string `json:"mode"`
	Since    int64  `json:"since"`
	Actor    string `json:"actor,omitempty"`
	IntentID string `json:"intent_id,omitempty"`
	Ts       int64  `json:"ts"`
}

// CloudlinkStatus — lexa/cloudlink/status (retained). Folded into lexa-api's
// /status as "cloud_link" and uplinked as part of the health stream.
type CloudlinkStatus struct {
	Envelope
	Connected     bool   `json:"connected"`
	Endpoint      string `json:"endpoint,omitempty"`
	SpoolBytes    int64  `json:"spool_bytes"`
	SpoolOldestTs int64  `json:"spool_oldest_ts,omitempty"`
	LastUplinkTs  int64  `json:"last_uplink_ts,omitempty"`
	CertDaysLeft  int    `json:"cert_days_left,omitempty"`
	Ts            int64  `json:"ts"`
}
