package bus

import "fmt"

// OpenADR bus contract (WP-15, standards-buildout E1): lexa-openadr — the
// OpenADR 3.1 VEN service (cmd/openadr + internal/openadr, pure Go, no
// wolfSSL) — polls its configured VTN, translates Continuous-Pricing-profile
// event payloads, and publishes three RETAINED QoS 1 documents (state, not
// edges: latest wins, a restarting subscriber re-seeds from the broker,
// AD-011):
//
//	TopicOpenADRPrices  bus.OpenADRPrices  openadr → hub
//	TopicOpenADRLimits  bus.OpenADRLimits  openadr → hub
//	TopicOpenADRStatus  bus.OpenADRStatus  openadr → api
//
// NOTHING consumes the prices/limits docs yet: the cmd/hub adoption slice
// (prices → SetPrices/SetDeliveryTariff seam, limits → GridState arbitration)
// lands separately, after WP-11. The D9 semantics the docs carry are
// therefore normative for that future consumer and documented on each field
// below — the adoption slice implements what these doc comments promise, so
// they must not drift.
//
// D9 (architecture.md, NORMATIVE) as it applies to these docs:
//   - Capacity limits combine MOST-RESTRICTIVE with CSIP (min per axis) at
//     GridState assembly; OpenADRLimits already carries the min ACROSS
//     OpenADR events per axis, so the hub's merge is one more min() against
//     the CSIP cap, with per-source attribution kept (EventID here, MRID on
//     the CSIP side) for breach reporting.
//   - Prices sit BELOW the CSIP tariff in precedence (CSIP §10.5 walk >
//     OpenADR CP prices > app/cloud tariff intent): the hub only adopts
//     OpenADRPrices for seams the CSIP walk left empty.
//   - Dispatch/setpoint payloads are deliberately NOT carried on these
//     topics — CSIP wins dispatch outright (D9); a dispatch-shaped OpenADR
//     payload never becomes a bus document in this design.

// Topic constants for the three lexa-openadr families. Defined here rather
// than in topics.go to keep the concurrent-agent merge surface of the LANE
// files minimal (work-packages.md bus-lane discipline); topics.go still owns
// the SupportedV arms.
const (
	// TopicOpenADRPrices carries the translated CP-profile price series
	// (PRICE / EXPORT_PRICE / GHG / ALERT_*) for every tracked event that
	// has not yet ended — active AND future intervals, since a price series
	// is planner forward-input, not a live cap. Retained, QoS 1 (PubQoS's
	// non-measurement default).
	TopicOpenADRPrices = "lexa/openadr/prices"
	// TopicOpenADRLimits carries the merged IMPORT/EXPORT_CAPACITY_LIMIT
	// caps in watts — most-restrictive across the CURRENTLY ACTIVE events,
	// per axis. Unlike prices, limits are live obligations: a future event's
	// limit does not appear here until its interval begins (the VEN's poll
	// loop recomputes and republishes as events activate/expire). A doc with
	// both axes nil means "no OpenADR cap in force" — an explicit release,
	// not an absence (AD-013 discipline). Retained, QoS 1.
	TopicOpenADRLimits = "lexa/openadr/limits"
	// TopicOpenADRStatus is lexa-openadr's health doc (VTN reachability,
	// token state, last poll, program/event counts), published every poll
	// cycle including failed ones, for lexa-api's /status projection.
	// Retained, QoS 1.
	TopicOpenADRStatus = "lexa/openadr/status"
)

// OpenADRPriceInterval is one absolute-time price slot inside an
// OpenADRPriceSeries. OpenADR 3.1 interval starts are absolute RFC3339
// timestamps (already randomizeStart-adjusted by the VEN before publishing),
// which is deliberately cleaner than TOU hour-of-day rules — no tariff-zone
// ambiguity (WS-8/GAP-05) applies to these.
type OpenADRPriceInterval struct {
	// StartTs is the interval's absolute start, Unix seconds — the
	// authoritative machine-readable form.
	StartTs int64 `json:"start_ts"`
	// Start is the same instant rendered RFC3339 (UTC), stamped by the same
	// translator from the same time.Time as StartTs — a human/dashboard
	// mirror, never parsed back by a consumer.
	Start string `json:"start,omitempty"`
	// DurationS is the interval length in seconds. <= 0 means unbounded
	// (the 3.1 "P9999Y" infinity sentinel).
	DurationS int64 `json:"duration_s"`
	// Value is the price/level in the series' Units+Currency (e.g. 0.17
	// USD per KWH for Kind "PRICE").
	Value float64 `json:"value"`
}

// OpenADRPriceSeries is one (event, payload-kind) price/level series.
type OpenADRPriceSeries struct {
	ProgramID string `json:"program_id"`
	EventID   string `json:"event_id"`
	// Kind is the OpenADR 3.1 event payloadType this series was translated
	// from: "PRICE", "EXPORT_PRICE", "GHG", or an "ALERT_*" tag
	// (ALERT_GRID_EMERGENCY, ...). The CP-profile vocabulary — nothing else
	// is ever published here.
	Kind string `json:"kind"`
	// Currency is the ISO 4217 currency from the event's (or program's)
	// payload descriptor; empty for non-monetary kinds (GHG, ALERT_*).
	Currency string `json:"currency,omitempty"`
	// Units is the payload descriptor's units enum value (e.g. "KWH");
	// empty when the descriptor carried none.
	Units     string                 `json:"units,omitempty"`
	Intervals []OpenADRPriceInterval `json:"intervals"`
}

// OpenADRPrices is the retained TopicOpenADRPrices document: every price-kind
// series from every tracked (not-yet-ended) event, deterministically ordered
// (Kind, then EventID) so publishers can content-compare for change
// detection. Series vanish when their event ends or is deleted by the VTN;
// an empty Series slice is the "no price signal" state.
type OpenADRPrices struct {
	Envelope
	Series []OpenADRPriceSeries `json:"series"`
	// Ts is the publish wall-clock time (Unix seconds), refreshed on every
	// publish.
	Ts int64 `json:"ts"`
}

// Finite is OpenADRPrices' counterpart to Measurement.Finite (GAP-09): every
// interval value must be finite before a subscriber's handler sees the doc.
func (p OpenADRPrices) Finite() error {
	for i, s := range p.Series {
		for j, iv := range s.Intervals {
			if err := finiteVal("value", iv.Value); err != nil {
				return fmt.Errorf("series[%d].intervals[%d]: %w", i, j, err)
			}
		}
	}
	return nil
}

// OpenADRLimits is the retained TopicOpenADRLimits document: the
// most-restrictive IMPORT_CAPACITY_LIMIT / EXPORT_CAPACITY_LIMIT across all
// currently-active OpenADR events, per axis, converted to watts by the VEN
// (payload-descriptor units honored at translation; a payload whose units
// cannot be converted to W is dropped with an alarm, never guessed — G27).
//
// Adoption semantics (D9, for the future cmd/hub slice): merge each non-nil
// axis min() against the CSIP cap at GridState assembly; keep EventID for
// attribution so a breach while OpenADR-bound produces an OpenADR
// opt-out/report, never a 2030.5 CannotComply against a CSIP MRID.
type OpenADRLimits struct {
	Envelope
	// ImpLimW/ExpLimW are the site import/export caps in watts. nil = no
	// active OpenADR limit on that axis (the absent-value convention —
	// never 0, which would mean "cap AT zero").
	ImpLimW *float64 `json:"imp_lim_w,omitempty"`
	ExpLimW *float64 `json:"exp_lim_w,omitempty"`
	// EventID attributes the binding limit(s): the event ID whose limit
	// binds. When the two axes bind from different events, both IDs joined
	// by "," (import-axis event first) — split on "," for per-axis
	// attribution.
	EventID string `json:"event_id,omitempty"`
	// ValidUntil is the earliest end (Unix seconds) among the binding
	// events — the moment this doc is guaranteed to be recomputed and
	// republished by the VEN's poll loop. 0 = unbounded binding event.
	ValidUntil int64 `json:"valid_until,omitempty"`
	// Ts is the publish wall-clock time (Unix seconds).
	Ts int64 `json:"ts"`
}

// Finite is OpenADRLimits' counterpart to ActiveControl.Finite — the
// safety-relevant case: a NaN cap must never reach the (future) GridState
// arbitration, only ever cause the doc to be dropped.
func (l OpenADRLimits) Finite() error {
	if err := finite("imp_lim_w", l.ImpLimW); err != nil {
		return err
	}
	return finite("exp_lim_w", l.ExpLimW)
}

// OpenADRStatus is the retained TopicOpenADRStatus health document,
// published on EVERY poll cycle — success and failure alike — so a wedged or
// unreachable VTN is visible on the bus, not just in the journal.
type OpenADRStatus struct {
	Envelope
	// VTNOK reports whether the most recent poll cycle completed without a
	// transport/HTTP/decode error.
	VTNOK bool `json:"vtn_ok"`
	// TokenOK reports OAuth2 health: true when the VEN holds an unexpired
	// bearer token, or when no auth is configured (public-tariff VTN,
	// client_id "" — 3.1 allows unauthenticated GETs).
	TokenOK bool `json:"token_ok"`
	// LastPollTs is the Unix time of the last SUCCESSFUL poll (0 = none yet
	// this process lifetime).
	LastPollTs int64 `json:"last_poll_ts"`
	// Programs is the number of programs the VEN is tracking after the last
	// successful poll (post program_ids filtering).
	Programs int `json:"programs"`
	// ActiveEvents is the number of tracked events whose interval period
	// covers now.
	ActiveEvents int `json:"active_events"`
	// LastErr is the most recent poll error string; cleared ("") by a
	// subsequent successful poll.
	LastErr string `json:"last_err,omitempty"`
	// Ts is the publish wall-clock time (Unix seconds).
	Ts int64 `json:"ts"`
}
