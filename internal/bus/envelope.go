package bus

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
)

// Envelope is embedded by value in a bus message struct that participates in
// the versioned-schema convention (AD-006, TASK-017). Embedding it adds a
// "v" field to that struct's JSON shape without changing any existing field.
//
// json:"v,omitempty" is deliberate: it means a struct whose Envelope.V is
// left at its zero value serializes with no "v" key at all, which is how a
// legacy (pre-versioning) publisher's wire shape looks. A publisher that has
// been updated to stamp a real schema version always sets V >= 1, so "v"
// absent (v0, legacy) and "v" present (v1+, versioned) stay distinguishable
// on the wire. Nothing in this repo sets V to 0 explicitly — see the
// per-schema constants below, all born at 1.
//
// This type is introduced but not yet wired into any publisher or
// subscriber (that is TASK-018); embedding it here is inert until then.
type Envelope struct {
	V int `json:"v,omitempty"`
}

// Per-schema envelope versions (AD-006 design decision: one constant per
// message family, not a single global version — a global would force
// lockstep version bumps across schemas that change independently). Every
// family is born at 1. TASK-018 wires each constant into that family's
// publisher (stamped into the embedded Envelope.V) and subscriber (passed
// as CheckVersion's `supported` argument); bump a family's constant only
// when that family's shape changes in a way old subscribers can't tolerate.
const (
	MeasurementV            = 1 // lexa/measurements/{device}               (Measurement)
	BattMetricsV            = 1 // lexa/battery/{device}/metrics            (BattMetrics)
	ActiveControlV          = 1 // lexa/csip/control                        (ActiveControl)
	ComplianceAlertV        = 1 // lexa/csip/compliance/alert               (ComplianceAlert)
	BattCommandV            = 1 // lexa/control/battery/{device}            (BattCommand)
	SolarCommandV           = 1 // lexa/control/solar/{device}              (SolarCommand)
	EVSEStateV              = 1 // lexa/evse/{station}/state                (EVSEState)
	EVSECommandV            = 1 // lexa/evse/{station}/command              (EVSECommand)
	PricingUpdateV          = 1 // lexa/csip/pricing                        (PricingUpdate)
	BillingUpdateV          = 1 // lexa/csip/billing                        (BillingUpdate)
	FlowReservationRequestV = 1 // lexa/csip/flowreservation/request        (FlowReservationRequestMsg)
	FlowReservationStatusV  = 1 // lexa/csip/flowreservation/status         (FlowReservationStatusMsg)
	DERScheduleV            = 1 // lexa/northbound/schedule                 (DERScheduleMsg)
	PlanLogV                = 1 // lexa/hub/plan                            (PlanLog)
	DesiredStateV           = 1 // lexa/desired/{class}/{device}            (DesiredState, AD-013)
	ReconcileReportV        = 1 // lexa/reconcile/{class}/{device}/report   (ReconcileReport, TASK-031)
	RewalkRequestV          = 1 // lexa/csip/rewalk                        (RewalkRequest, TASK-042)
	CertStatusV             = 1 // lexa/northbound/certstatus               (CertStatus, TASK-072)

	// Intent/scan/mode/status schemas (TASK-082, docs/DEVICE_ROADMAP.md §1.3).
	// All born at 1, same convention as every family above.
	ModeIntentV          = 1 // lexa/intent/mode
	EVGoalIntentV        = 1 // lexa/intent/evgoal
	BackupReserveIntentV = 1 // lexa/intent/reserve
	TariffIntentV        = 1 // lexa/intent/tariff
	SolarForecastIntentV = 1 // lexa/intent/solarforecast
	LoadProfileIntentV   = 1 // lexa/intent/loadprofile
	ChargeNowIntentV     = 1 // lexa/intent/chargenow
	IntentResultV        = 1 // lexa/intent/result
	ModeStatusV          = 1 // lexa/hub/mode
	CloudlinkStatusV     = 1 // lexa/cloudlink/status
	ScanRequestV         = 1 // lexa/scan/request
	ScanStatusV          = 1 // lexa/scan/status
	ScanResultV          = 1 // lexa/scan/result
	PendingStationsV     = 1 // lexa/ocpp/pending
)

// LegacyV0Accepted is the transition switch for AD-006's compatibility
// policy. While true (the default), a message with no "v" field — which is
// indistinguishable on the wire from an explicit "v":0, since omitempty
// never serializes zero — is treated as a legacy v0 publisher and accepted.
// Once every publisher in a topic's family is confirmed to stamp v>=1
// (tracked by TASK-018), this is flipped to false so stragglers are rejected
// instead of silently tolerated forever. It is a package-level var, not a
// per-topic setting, because the transition is expected to be a single
// repo-wide cutover; a later task can promote it to per-topic config if that
// assumption stops holding.
var LegacyV0Accepted = true

// VersionError is returned by CheckVersion when a message's envelope version
// falls outside the range a subscriber supports. It is exported (not just an
// error string) so callers can inspect Topic/Got/Supported — e.g. to decide,
// per AD-006, whether a rejected retained control-plane message should hold
// last-known-good (now) or trigger a re-request (TASK-042, later).
type VersionError struct {
	Topic     string
	Got       int
	Supported int
}

func (e *VersionError) Error() string {
	return fmt.Sprintf("bus: %s: version %d exceeds supported %d", e.Topic, e.Got, e.Supported)
}

// CheckVersion peeks at payload's envelope version and reports whether a
// subscriber supporting versions 1..supported should accept the message.
// It does not unmarshal into the real message type and never mutates
// anything; it is meant to run before the real json.Unmarshal in a decode
// path (TASK-018 wires this into mqttutil.Subscribe).
//
// Decode policy (AD-006):
//   - "v" absent (equivalently, an explicit "v":0 — the two are indistinguishable
//     given omitempty) is legacy v0: accepted while LegacyV0Accepted is true,
//     rejected once it is flipped false.
//   - 1 <= v <= supported: accepted.
//   - v > supported, or v < 0: rejected, returns *VersionError.
//   - Unknown fields alongside a supported v are not this function's concern:
//     Go's json.Unmarshal already ignores unrecognized keys by default, which
//     is what keeps additive (same-major) schema evolution cheap.
//
// Malformed-JSON responsibility (recorded here per the task's design
// requirement): CheckVersion's internal peek unmarshals only into a
// struct{ V int }. If that peek itself fails — payload is not a JSON object,
// or "v" is present but is not a JSON number (e.g. a string) — CheckVersion
// returns nil rather than an error. It deliberately does not attempt to
// detect or report malformed JSON: that is the real json.Unmarshal's job,
// a few lines later in the same decode path, and it is single-responsibility
// for CheckVersion to leave it there rather than duplicate (and risk
// disagreeing with) that error. A malformed payload that passes CheckVersion
// will fail the subsequent real unmarshal and be logged there, exactly as it
// is today with no version envelope at all.
func CheckVersion(topic string, payload []byte, supported int) error {
	var peek struct {
		V int `json:"v"`
	}
	if err := json.Unmarshal(payload, &peek); err != nil {
		// Malformed JSON or a non-integer "v" — not our job to flag; see the
		// doc comment above.
		return nil
	}
	if peek.V == 0 {
		if LegacyV0Accepted {
			return nil
		}
		return &VersionError{Topic: topic, Got: 0, Supported: supported}
	}
	if peek.V < 0 || peek.V > supported {
		return &VersionError{Topic: topic, Got: peek.V, Supported: supported}
	}
	return nil
}

// rejectCounters holds one *uint64 per topic that has ever had a version
// rejected, incremented atomically by RejectAndAlarm. A sync.Map is used
// instead of a mutex+map because the write pattern (rare new keys, frequent
// increments to existing keys) is exactly what it's optimized for, and this
// is called from arbitrary subscriber goroutines.
var rejectCounters sync.Map // topic string -> *uint64

// logEveryN is the log rate-limit divisor for RejectAndAlarm: the first
// rejection recorded for a topic, and every logEveryNth one after, is
// logged; the rest only increment the counter. It is a var rather than a
// const solely so tests can shrink it and exercise the rate-limit path
// without firing hundreds of messages; production code has no reason to
// change it.
var logEveryN uint64 = 100

// RejectAndAlarm records one version rejection for err.Topic: it increments
// that topic's counter (exposed via VersionRejects for TASK-044's metrics
// endpoint to scrape once it exists) and emits a rate-limited structured log
// line. Logging is deliberately not one-line-per-message: a publisher stuck
// on the wrong schema version would otherwise spam the journal past its
// budget (TASK-009), so only the first rejection for a topic and every
// logEveryNth one after are logged.
func RejectAndAlarm(err *VersionError) {
	if err == nil {
		return
	}
	v, _ := rejectCounters.LoadOrStore(err.Topic, new(uint64))
	n := atomic.AddUint64(v.(*uint64), 1)
	if n == 1 || n%logEveryN == 0 {
		// TASK-045: migrated to slog (rate-limited decode-reject alarm).
		// "REJECT" kept intact in the message text.
		slog.Warn("[bus] REJECT unknown schema version",
			"topic", err.Topic, "v", err.Got, "supported", err.Supported, "count", n)
	}
}

// VersionRejects returns a snapshot of the per-topic reject counters
// maintained by RejectAndAlarm. Nothing scrapes this yet — TASK-044 is the
// consumer once a metrics endpoint exists.
func VersionRejects() map[string]uint64 {
	out := make(map[string]uint64)
	rejectCounters.Range(func(k, v any) bool {
		out[k.(string)] = atomic.LoadUint64(v.(*uint64))
		return true
	})
	return out
}

// decodeFailCounters holds one *uint64 per topic that has ever had a decode
// failure recorded by RecordDecodeFailure. Same sync.Map rationale as
// rejectCounters just above: rare new keys, frequent increments to existing
// ones, called from arbitrary subscriber goroutines. Kept as a distinct map
// (a sibling of rejectCounters, not a shared one) because a version reject
// and a decode/finite failure are different failure modes worth telling
// apart in a topic's history, even though both are meant to roll up into the
// same lexa_bus_decode_failures_total total.
var decodeFailCounters sync.Map // topic string -> *uint64

// RecordDecodeFailure records one decode failure for topic: either a raw
// json.Unmarshal error, or a message that unmarshalled successfully but
// failed its own Finite() check (GAP-09 — a non-finite numeric value that
// slipped past a lax decode path). It is mqttutil.Subscribe's sibling to
// RejectAndAlarm — same rate-limited counter+log shape — for the failure
// mode CheckVersion/RejectAndAlarm does not cover: today, a plain
// json.Unmarshal failure on a non-control topic is only ever log.Printf'd
// (mqttutil.go), invisible to anything scraping metrics. This turns that
// silent drop into a counted, alarmed one, matching the treatment a version
// rejection already gets.
func RecordDecodeFailure(topic string, err error) {
	if err == nil {
		return
	}
	v, _ := decodeFailCounters.LoadOrStore(topic, new(uint64))
	n := atomic.AddUint64(v.(*uint64), 1)
	if n == 1 || n%logEveryN == 0 {
		slog.Warn("[bus] REJECT decode failure",
			"topic", topic, "err", err, "count", n)
	}
}

// DecodeFailures returns a snapshot of the per-topic decode-failure counters
// maintained by RecordDecodeFailure. Sibling of VersionRejects; a caller
// wiring lexa_bus_decode_failures_total (TASK-044) should sum both this and
// VersionRejects — today only VersionRejects feeds that metric (see each
// service's main.go Collect callback), which is exactly the GAP-09 gap this
// task exists to close. Wiring the sum into those six main.go files is a
// follow-up outside this task's internal/bus + mqttutil lane.
func DecodeFailures() map[string]uint64 {
	out := make(map[string]uint64)
	decodeFailCounters.Range(func(k, v any) bool {
		out[k.(string)] = atomic.LoadUint64(v.(*uint64))
		return true
	})
	return out
}
