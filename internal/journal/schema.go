package journal

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// SchemaV is the current Event envelope schema version. Bump only on a
// breaking change to the envelope itself (Event's own fields); adding a new
// field to a payload type, or a new event Type, is NOT a version bump (the
// schema is append-only — see AD-005 / TASK-039's "Blast radius").
const SchemaV = 1

// Event is one NDJSON line: the journal's envelope. Every field is stable
// wire shape once TASK-040 ships a first writer — see the package doc's
// "Blast radius" note.
type Event struct {
	V    int             `json:"v"`               // schema version, SchemaV
	Ts   int64           `json:"ts"`              // local Unix seconds (wall; observability)
	SrvT int64           `json:"srv_t,omitempty"` // server time when known (utilitytime)
	Seq  uint64          `json:"seq"`             // per-writer monotonic, assigned by Writer.Append
	Type string          `json:"type"`
	Svc  string          `json:"svc"` // "hub" | "northbound" | ...
	Data json.RawMessage `json:"data,omitempty"`
}

// Event Type values. This is the complete vocabulary TASK-040 wires; it is
// deliberately transition-only (05 §9) — there is no "tick"/steady-state
// event by design (see "Common mistakes to avoid" in TASK-039).
const (
	TypeControlAdopted     = "control_adopted"
	TypeControlReleased    = "control_released"
	TypeDispatch           = "dispatch"
	TypeBreachBegin        = "breach_begin"
	TypeBreachEnd          = "breach_end"
	TypeCannotComplyPosted = "cannot_comply_posted"
	TypeServiceStart       = "service_start"
	TypeSnapshotWritten    = "snapshot_written"
	TypeSnapshotRestored   = "snapshot_restored"
)

// ControlReleased.Reason values.
const (
	ReasonExpired  = "expired"
	ReasonCleared  = "cleared"
	ReasonReplaced = "replaced"
)

// Dispatch.Kind values (mirrors bus.DesiredClass* naming, AD-013).
const (
	KindBattery = "battery"
	KindSolar   = "solar"
	KindEVSE    = "evse"
)

// ControlAdopted is the control_adopted payload: a scheduler.ActiveControl
// (bus.ActiveControl on the wire) became the standing intent. Field shapes
// mirror bus.ActiveControl's limit pointers (nil = no opinion on that
// surface, per AD-013's absence semantics — never confuse with an explicit
// zero).
type ControlAdopted struct {
	Source      string   `json:"source"`
	MRID        string   `json:"mrid"`
	ExpLimW     *float64 `json:"exp_lim_w,omitempty"`
	ImpLimW     *float64 `json:"imp_lim_w,omitempty"`
	MaxLimW     *float64 `json:"max_lim_w,omitempty"`
	FixedW      *float64 `json:"fixed_w,omitempty"`
	ValidUntil  int64    `json:"valid_until,omitempty"`
	ClockOffset int64    `json:"clock_offset"`
}

// NewControlAdopted builds a ControlAdopted payload.
func NewControlAdopted(source, mrid string, expLimW, impLimW, maxLimW, fixedW *float64, validUntil, clockOffset int64) ControlAdopted {
	return ControlAdopted{
		Source:      source,
		MRID:        mrid,
		ExpLimW:     expLimW,
		ImpLimW:     impLimW,
		MaxLimW:     maxLimW,
		FixedW:      fixedW,
		ValidUntil:  validUntil,
		ClockOffset: clockOffset,
	}
}

// NewControlAdoptedEvent wraps a ControlAdopted payload as an Event ready
// for (*Writer).Append (V/Ts/Seq are filled in by Append; set Data now so
// the JSON shape is locked at construction time, not at write time).
func NewControlAdoptedEvent(svc string, p ControlAdopted) (Event, error) {
	return newEvent(TypeControlAdopted, svc, p)
}

// ControlReleased is the control_released payload: the previously-adopted
// control stopped being the standing intent.
type ControlReleased struct {
	MRID   string `json:"mrid"`
	Reason string `json:"reason"` // ReasonExpired | ReasonCleared | ReasonReplaced
}

// NewControlReleased builds a ControlReleased payload.
func NewControlReleased(mrid, reason string) ControlReleased {
	return ControlReleased{MRID: mrid, Reason: reason}
}

// NewControlReleasedEvent wraps a ControlReleased payload as an Event.
func NewControlReleasedEvent(svc string, p ControlReleased) (Event, error) {
	return newEvent(TypeControlReleased, svc, p)
}

// Dispatch is the dispatch payload: a device-level command actually sent
// (post-dedupe — no per-tick steady-state dispatch is journaled).
type Dispatch struct {
	Device      string   `json:"device"`
	Kind        string   `json:"kind"` // KindBattery | KindSolar | KindEVSE
	SetpointW   *float64 `json:"setpoint_w,omitempty"`
	CeilingW    *float64 `json:"ceiling_w,omitempty"`
	MaxCurrentA *float64 `json:"max_current_a,omitempty"`
	Connect     *bool    `json:"connect,omitempty"`
}

// NewDispatch builds a Dispatch payload.
func NewDispatch(device, kind string, setpointW, ceilingW, maxCurrentA *float64, connect *bool) Dispatch {
	return Dispatch{
		Device:      device,
		Kind:        kind,
		SetpointW:   setpointW,
		CeilingW:    ceilingW,
		MaxCurrentA: maxCurrentA,
		Connect:     connect,
	}
}

// NewDispatchEvent wraps a Dispatch payload as an Event.
func NewDispatchEvent(svc string, p Dispatch) (Event, error) {
	return newEvent(TypeDispatch, svc, p)
}

// Breach is the breach_begin / breach_end payload (bus.ComplianceAlert
// on/off edges). EpisodeID is the journalctl-correlation key TASK-040's
// diagnosis story uses — see EpisodeID.
type Breach struct {
	EpisodeID  string  `json:"episode_id"`
	MRID       string  `json:"mrid"`
	LimitType  string  `json:"limit_type"`
	LimitW     float64 `json:"limit_w"`
	MeasuredW  float64 `json:"measured_w"`
	ShortfallW float64 `json:"shortfall_w"`
	Reason     string  `json:"reason"`
}

// NewBreach builds a Breach payload.
func NewBreach(episodeID, mrid, limitType string, limitW, measuredW, shortfallW float64, reason string) Breach {
	return Breach{
		EpisodeID:  episodeID,
		MRID:       mrid,
		LimitType:  limitType,
		LimitW:     limitW,
		MeasuredW:  measuredW,
		ShortfallW: shortfallW,
		Reason:     reason,
	}
}

// NewBreachBeginEvent wraps a Breach payload as a breach_begin Event.
func NewBreachBeginEvent(svc string, p Breach) (Event, error) {
	return newEvent(TypeBreachBegin, svc, p)
}

// NewBreachEndEvent wraps a Breach payload as a breach_end Event.
func NewBreachEndEvent(svc string, p Breach) (Event, error) {
	return newEvent(TypeBreachEnd, svc, p)
}

// EpisodeID derives the episode correlation key used by breach_begin/
// breach_end/cannot_comply_posted: mrid + "/" + beginTs (Unix seconds of the
// breach's onset). TASK-040's diagnosis story greps this key across the
// journal and journalctl output for one episode's full timeline.
func EpisodeID(mrid string, beginTs int64) string {
	return mrid + "/" + strconv.FormatInt(beginTs, 10)
}

// CannotComplyPosted is the cannot_comply_posted payload: the northbound
// service POSTed a CannotComply Response for episodeID.
type CannotComplyPosted struct {
	EpisodeID  string `json:"episode_id"`
	MRID       string `json:"mrid"`
	HTTPStatus int    `json:"http_status"`
}

// NewCannotComplyPosted builds a CannotComplyPosted payload.
func NewCannotComplyPosted(episodeID, mrid string, httpStatus int) CannotComplyPosted {
	return CannotComplyPosted{EpisodeID: episodeID, MRID: mrid, HTTPStatus: httpStatus}
}

// NewCannotComplyPostedEvent wraps a CannotComplyPosted payload as an Event.
func NewCannotComplyPostedEvent(svc string, p CannotComplyPosted) (Event, error) {
	return newEvent(TypeCannotComplyPosted, svc, p)
}

// ServiceStart is the service_start payload: emitted once per process start
// (a natural transition — the process itself is the transition).
type ServiceStart struct {
	Version    string `json:"version"`
	ConfigHash string `json:"config_hash"`
}

// NewServiceStart builds a ServiceStart payload.
func NewServiceStart(version, configHash string) ServiceStart {
	return ServiceStart{Version: version, ConfigHash: configHash}
}

// NewServiceStartEvent wraps a ServiceStart payload as an Event.
func NewServiceStartEvent(svc string, p ServiceStart) (Event, error) {
	return newEvent(TypeServiceStart, svc, p)
}

// Snapshot is the snapshot_written / snapshot_restored payload (AD-005's
// guard/breach snapshot, TASK-041). BreachEpisode is the EpisodeID of the
// in-flight breach the snapshot captured/restored, or "" if none was active.
type Snapshot struct {
	Path          string `json:"path"`
	BreachEpisode string `json:"breach_episode,omitempty"`
}

// NewSnapshot builds a Snapshot payload.
func NewSnapshot(path, breachEpisode string) Snapshot {
	return Snapshot{Path: path, BreachEpisode: breachEpisode}
}

// NewSnapshotWrittenEvent wraps a Snapshot payload as a snapshot_written Event.
func NewSnapshotWrittenEvent(svc string, p Snapshot) (Event, error) {
	return newEvent(TypeSnapshotWritten, svc, p)
}

// NewSnapshotRestoredEvent wraps a Snapshot payload as a snapshot_restored Event.
func NewSnapshotRestoredEvent(svc string, p Snapshot) (Event, error) {
	return newEvent(TypeSnapshotRestored, svc, p)
}

// newEvent marshals payload into Data and stamps Type/Svc/V. Ts and Seq are
// left zero-valued: (*Writer).Append fills Ts (from Config.Now, if unset)
// and always assigns Seq itself (the writer, not the caller, owns the
// per-writer monotonic counter).
func newEvent(typ, svc string, payload any) (Event, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return Event{}, fmt.Errorf("journal: marshal %s payload: %w", typ, err)
	}
	return Event{V: SchemaV, Type: typ, Svc: svc, Data: b}, nil
}
