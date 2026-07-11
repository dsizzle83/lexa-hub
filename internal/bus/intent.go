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

// HubSettings — lexa/hub/settings (retained, GAP-8 read-back). The hub's
// EFFECTIVE reserve floor and ACTIVE tariff, published so the app's reserve
// slider and tariff viewer render hub truth instead of a locally-persisted
// last-submitted value (which is all they had while the hub exposed neither).
// Published by cmd/hub (settings.go) on every reserve/tariff change, once at
// startup as a seed, and whenever the effective reserve pct moves after a
// re-plan; folded into lexa-api's /status as "reserve" + "tariff".
type HubSettings struct {
	Envelope
	Reserve ReserveSettings `json:"reserve"`
	Tariff  TariffSettings  `json:"tariff"`
	Ts      int64           `json:"ts"`
}

// ReserveSettings is HubSettings' backup-reserve read-back block.
type ReserveSettings struct {
	// EffectivePct is the reserve floor (percent of battery capacity) the most
	// recent plan actually resolved to — the config floor RAISED by any reserve
	// intent (Engine.EffectiveReservePct). nil before the hub's first plan (the
	// engine's -1 sentinel), which is exactly why applyReserve could not report
	// "clamped" before this existed.
	EffectivePct *float64 `json:"effective_pct"`
	// FloorPct is the configured safety floor (percent) a reserve intent may
	// only raise above, never below — the value EffectivePct clamps against.
	FloorPct *float64 `json:"floor_pct"`
	// Source is where the standing reserve came from: "default" (no intent yet —
	// the config floor governs), or a reserve intent's origin ("app" | "cloud" |
	// "lexactl").
	Source string `json:"source"`
}

// TariffSettings is HubSettings' active-tariff read-back block.
type TariffSettings struct {
	// Source is "manual" once a tariff intent has set Spec, else "csip" (no
	// manual override — utility CSIP pricing or the built-in default TOU
	// governs). NOTE: "csip" here means only "not manually overridden"; the hub
	// does not yet distinguish an ACTIVE CSIP price feed from the default TOU,
	// nor detect a CSIP feed overriding a manual tariff — that live-CSIP-override
	// detection is a documented follow-up (needs a pricing-source signal the
	// engine does not expose today).
	Source string `json:"source"`
	// UpdatedAt is when the tariff last changed on the hub (Unix seconds); 0
	// before any tariff intent.
	UpdatedAt int64 `json:"updated_at"`
	// Spec is the last tariff intent's TariffSpec, echoed verbatim so the app's
	// tariff viewer renders exactly what it submitted. nil before any tariff
	// intent (the app then shows no manual tariff).
	Spec *TariffSpec `json:"spec,omitempty"`
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
