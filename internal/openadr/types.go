// Package openadr is a pure-Go OpenADR 3.1 VEN client (WP-15,
// standards-buildout E1): OAuth2 client-credentials auth, the REST resources
// a pull-only Continuous-Pricing VEN needs (GET /programs, GET /events,
// POST /reports, GET/POST /vens, GET /auth/server), poll-based event
// lifecycle reconciliation, and translation of CP-profile payloads into the
// internal/bus OpenADR documents.
//
// Dependency posture: stdlib net/http + crypto/tls ONLY — no wolfSSL, no
// CGo, no oauth2 library (client-credentials is one form POST — see
// token.go), matching the digest's sizing ("OpenADR has NO CSIP-style cipher
// pin, so no wolfSSL/CGo involvement") and the repo's lean-dependency rule
// (internal/metrics' package doc).
//
// This package does no MQTT: cmd/openadr owns the bus session and feeds
// measurement snapshots in / publishes translated documents out, keeping
// this package unit-testable against a plain httptest VTN.
package openadr

import "encoding/json"

// Program is the slice of the 3.1 program object this VEN reads: identity
// plus the program-level payload descriptors (a tariff program typically
// hangs its PRICE descriptor here rather than on every event) and the
// default interval period. Unknown fields are ignored by json.Unmarshal —
// the 3.1-sanctioned extension mechanism.
type Program struct {
	ID                 string              `json:"id"`
	ProgramName        string              `json:"programName,omitempty"`
	PayloadDescriptors []PayloadDescriptor `json:"payloadDescriptors,omitempty"`
	IntervalPeriod     *IntervalPeriod     `json:"intervalPeriod,omitempty"`
}

// Event is the slice of the 3.1 event object this VEN consumes.
type Event struct {
	ID                 string              `json:"id"`
	ProgramID          string              `json:"programID"`
	EventName          string              `json:"eventName,omitempty"`
	Priority           *int                `json:"priority,omitempty"`
	PayloadDescriptors []PayloadDescriptor `json:"payloadDescriptors,omitempty"`
	ReportDescriptors  []ReportDescriptor  `json:"reportDescriptors,omitempty"`
	IntervalPeriod     *IntervalPeriod     `json:"intervalPeriod,omitempty"`
	Intervals          []Interval          `json:"intervals"`
	// ModificationDateTime participates in the update-detection hash (3.1
	// has no modificationNumber; lifecycle is by poll reconciliation), but
	// the hash covers the whole decoded object, so an event that changes
	// content without bumping this stamp is still detected.
	ModificationDateTime string `json:"modificationDateTime,omitempty"`
}

// IntervalPeriod is the 3.1 {start, duration, randomizeStart} triple.
// start is RFC3339 (the "0001-01-01..." sentinel means "now"); duration and
// randomizeStart are ISO 8601 durations ("P9999Y" ≈ infinity) — see
// duration.go for the parsing rules.
type IntervalPeriod struct {
	Start          string `json:"start"`
	Duration       string `json:"duration,omitempty"`
	RandomizeStart string `json:"randomizeStart,omitempty"`
}

// Interval is one event (or report) interval: an optional period override
// plus its payload valuesMaps.
type Interval struct {
	ID             int             `json:"id"`
	IntervalPeriod *IntervalPeriod `json:"intervalPeriod,omitempty"`
	Payloads       []ValuesMap     `json:"payloads"`
}

// ValuesMap is the 3.1 {type, values[]} pair. Values decode as generic JSON
// (numbers arrive as float64) because the spec allows numbers, strings,
// booleans, and points; FirstNumber extracts the numeric case this VEN
// translates.
type ValuesMap struct {
	Type   string `json:"type"`
	Values []any  `json:"values"`
}

// FirstNumber returns the first numeric entry in v.Values. JSON numbers
// decode into `any` as float64; json.Unmarshal already rejects bare NaN/Inf
// tokens (they are not valid JSON), so a true here implies a finite value.
func (v ValuesMap) FirstNumber() (float64, bool) {
	for _, x := range v.Values {
		if f, ok := x.(float64); ok {
			return f, true
		}
		// A json.Number would appear if a caller used a Decoder with
		// UseNumber; tolerate it defensively.
		if n, ok := x.(json.Number); ok {
			if f, err := n.Float64(); err == nil {
				return f, true
			}
		}
	}
	return 0, false
}

// PayloadDescriptor is the shared event/report payload descriptor shape
// ({payloadType, units, currency} for events; + readingType/accuracy for
// reports).
type PayloadDescriptor struct {
	ObjectType  string   `json:"objectType,omitempty"`
	PayloadType string   `json:"payloadType"`
	Units       string   `json:"units,omitempty"`
	Currency    string   `json:"currency,omitempty"`
	ReadingType string   `json:"readingType,omitempty"`
	Accuracy    *float64 `json:"accuracy,omitempty"`
}

// ReportDescriptor is how a 3.1 event asks the VEN for reports. frequency /
// startInterval / numIntervals / repeat count EVENT INTERVALS (integers),
// not durations — the report cadence is frequency × the event's interval
// duration (report.go resolves it).
type ReportDescriptor struct {
	PayloadType   string      `json:"payloadType"`
	ReadingType   string      `json:"readingType,omitempty"`
	Units         string      `json:"units,omitempty"`
	Targets       []ValuesMap `json:"targets,omitempty"`
	Aggregate     bool        `json:"aggregate,omitempty"`
	StartInterval int         `json:"startInterval,omitempty"`
	NumIntervals  int         `json:"numIntervals,omitempty"`
	Historical    bool        `json:"historical,omitempty"`
	Frequency     int         `json:"frequency,omitempty"`
	Repeat        int         `json:"repeat,omitempty"`
}

// Report is the 3.1 report object this VEN POSTs to /reports.
type Report struct {
	ObjectType         string              `json:"objectType,omitempty"` // "REPORT"
	ProgramID          string              `json:"programID"`
	EventID            string              `json:"eventID"`
	ClientName         string              `json:"clientName"`
	PayloadDescriptors []PayloadDescriptor `json:"payloadDescriptors,omitempty"`
	Resources          []ReportResource    `json:"resources"`
}

// ReportResource is one per-resource interval set inside a Report.
type ReportResource struct {
	ResourceName   string          `json:"resourceName"`
	IntervalPeriod *IntervalPeriod `json:"intervalPeriod,omitempty"`
	Intervals      []Interval      `json:"intervals"`
}

// Ven is the 3.1 ven registration object (venName unique per VTN).
type Ven struct {
	ID      string `json:"id,omitempty"`
	VenName string `json:"venName"`
}

// Event payloadTypes this VEN translates (3.1 Table 1, CP-profile slice).
const (
	PayloadPrice       = "PRICE"
	PayloadExportPrice = "EXPORT_PRICE"
	PayloadGHG         = "GHG"
	alertPrefix        = "ALERT_"

	PayloadImportCapacityLimit = "IMPORT_CAPACITY_LIMIT"
	PayloadExportCapacityLimit = "EXPORT_CAPACITY_LIMIT"
)

// IsPriceKind reports whether payloadType is one of the CP-profile
// price/level kinds carried on lexa/openadr/prices: PRICE, EXPORT_PRICE,
// GHG, or any ALERT_* tag.
func IsPriceKind(payloadType string) bool {
	switch payloadType {
	case PayloadPrice, PayloadExportPrice, PayloadGHG:
		return true
	}
	return len(payloadType) > len(alertPrefix) && payloadType[:len(alertPrefix)] == alertPrefix
}
