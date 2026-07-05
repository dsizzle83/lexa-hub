package reconcile

import "time"

// Field identifies one actuatable surface the reconciler tracks. It is
// deliberately class-generic: the shells (TASK-027 battery, TASK-029 solar,
// TASK-030 EVSE) map each Field to the register / charging-profile parameter of
// their device. The core never knows a battery from an inverter — it only
// knows "desired value for Field f" vs "read value for Field f".
//
// Connect is a boolean surface carried through the float64 read/write maps as
// 1 (energize / connected) or 0 (cease-energize / disconnected); its readback
// tolerance defaults below 0.5 so 0 and 1 never alias.
type Field int

const (
	// SetpointW is the battery real-power setpoint (W): +discharge, −charge,
	// explicit 0 = idle (enforces the SOC reserve, ledger L1).
	SetpointW Field = iota
	// CeilingW is the solar generation ceiling (W); restore = an explicit
	// large value (bus.RestoreCeilingW), never an absent field.
	CeilingW
	// Connect is the battery cease/energize surface, carried as 1 (on) / 0 (off).
	Connect
	// MaxCurrentA is the EVSE charging-current ceiling (A); explicit 0 = suspend.
	MaxCurrentA
)

// String renders a Field for logs and test failures.
func (f Field) String() string {
	switch f {
	case SetpointW:
		return "SetpointW"
	case CeilingW:
		return "CeilingW"
	case Connect:
		return "Connect"
	case MaxCurrentA:
		return "MaxCurrentA"
	default:
		return "Field(?)"
	}
}

// ActionKind is the discriminant of Action.
type ActionKind int

const (
	// ActionNone is "do nothing this event" — the zero value, so a zero Action
	// is inert.
	ActionNone ActionKind = iota
	// ActionWrite instructs the shell to write Action.Fields to the device.
	ActionWrite
)

// String renders an ActionKind.
func (k ActionKind) String() string {
	switch k {
	case ActionNone:
		return "None"
	case ActionWrite:
		return "Write"
	default:
		return "ActionKind(?)"
	}
}

// Action is the ONLY way the reconciler affects the world: a value the shell
// interprets. The core performs no I/O and starts no goroutines (see the
// package doc); it returns Actions and the shell drives the driver. This is
// what makes the state machine exhaustively table-testable.
//
// For ActionWrite, Fields is the complete set of field→value opinions the
// current desired document expresses (Connect as 1/0). It is a fresh map on
// every Action, never an alias of internal state, so a caller may retain or
// mutate it freely.
type Action struct {
	Kind   ActionKind
	Fields map[Field]float64
	// Reason is a short, stable cause tag ("write-on-diff", "retry",
	// "reconnect-reassert", "reassert", "new-desired") for logs/metrics.
	Reason string
}

// none is the inert action.
func none() Action { return Action{Kind: ActionNone} }

// ReportKind is the discriminant of Report. TASK-031 (the CannotComply
// collapse) consumes these as the device-level evidence source.
type ReportKind int

const (
	// ReportNonConvergedBegin fires exactly once when divergence has persisted
	// Config.ConvergeTimeout of wall-clock time (edge semantics, ledger L5).
	ReportNonConvergedBegin ReportKind = iota
	// ReportNonConvergedEnd fires exactly once when a device that had opened a
	// non-convergence episode is next observed converged (or its target changes).
	ReportNonConvergedEnd
	// ReportStaleDesired fires once when no fresh document has arrived for
	// Config.StaleAfter; the desired state is HELD (fail-closed), never cleared.
	ReportStaleDesired
	// ReportRejectedDoc fires when an incoming DesiredState is rejected
	// (seq/issuedAt regression, staleness, or NaN); state is left unchanged.
	ReportRejectedDoc
	// ReportRejectedObs fires when an Observed sample is rejected (NaN/Inf);
	// the sample is dropped and the previous assessment held.
	ReportRejectedObs
	// ReportSeqReset fires when a document is ACCEPTED despite a lower/reset seq
	// because its issuedAt is strictly newer — the publisher-restart case
	// (AD-013 rule 2). A restart must be observable, not silent.
	ReportSeqReset
	// ReportInterlockHold is emitted by an ACTIVE reconciler shell (never by the
	// core itself) when it suppresses a connect-restoring Write because the
	// Tier-0 battery interlock (ledger L8) currently has the device
	// force-disconnected. It records that convergence is being withheld by
	// design — Tier-0 is senior — so TASK-031 can attribute a held device to
	// "not converged because the interlock vetoed" rather than a reconciler
	// fault. Appended at the end of the enum so existing values keep their
	// iota (the append-only journal, TASK-039, is String-keyed but the numeric
	// stability costs nothing).
	ReportInterlockHold
)

// String renders a ReportKind.
func (k ReportKind) String() string {
	switch k {
	case ReportNonConvergedBegin:
		return "NonConvergedBegin"
	case ReportNonConvergedEnd:
		return "NonConvergedEnd"
	case ReportStaleDesired:
		return "StaleDesired"
	case ReportRejectedDoc:
		return "RejectedDoc"
	case ReportRejectedObs:
		return "RejectedObs"
	case ReportSeqReset:
		return "SeqReset"
	case ReportInterlockHold:
		return "InterlockHold"
	default:
		return "ReportKind(?)"
	}
}

// RejectReason explains a ReportRejectedDoc / ReportRejectedObs. It is
// RejectNone on every other report kind.
type RejectReason int

const (
	// RejectNone means "not a rejection report".
	RejectNone RejectReason = iota
	// RejectSeqRegression: seq <= lastAppliedSeq AND issuedAt <= lastAppliedIssuedAt
	// (AD-013 rule 1 — retained-redelivery / replay).
	RejectSeqRegression
	// RejectStale: issuedAt older than the staleness bound, regardless of seq
	// (AD-013 rule 3).
	RejectStale
	// RejectNaN: a NaN/Inf value in a document field or an observation.
	RejectNaN
)

// String renders a RejectReason.
func (r RejectReason) String() string {
	switch r {
	case RejectNone:
		return "None"
	case RejectSeqRegression:
		return "SeqRegression"
	case RejectStale:
		return "Stale"
	case RejectNaN:
		return "NaN"
	default:
		return "RejectReason(?)"
	}
}

// Report is a structured, I/O-free observation the shell forwards (retained,
// per device) to the breach-episode component (TASK-031). It carries exactly
// the attribution that component needs to form CannotComply episode IDs
// (typically MRID + "-" + IssuedAt) and dedupe per episode: DeviceID, MRID,
// Seq, and a monotonic Episode counter.
type Report struct {
	Kind        ReportKind
	DeviceClass string
	DeviceID    string
	// MRID is the active CSIP control the held/rejected intent derives from
	// (empty for economic/safety sources); TASK-031's attribution key.
	MRID string
	// Seq is the seq of the relevant document (the held desired for
	// convergence/staleness reports; the rejected/accepted doc otherwise).
	Seq uint64
	// IssuedAt is that document's publisher wall clock (Unix seconds); half of
	// the episode ID TASK-031 forms.
	IssuedAt int64
	// Episode is a per-reconciler monotonic counter incremented once per
	// NonConvergedBegin; Begin and its matching End share a value.
	Episode uint64
	// Reject is set only on ReportRejectedDoc / ReportRejectedObs.
	Reject RejectReason
	// At is the injected wall-clock time the report was produced.
	At time.Time
}
