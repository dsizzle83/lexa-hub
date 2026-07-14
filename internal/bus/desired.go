package bus

// DesiredState is the retained, versioned, per-device desired-state document
// (AD-013) — the single wire contract for AD-002's Device Reconciler. The
// optimizer publishes one retained document per device on
// lexa/desired/{class}/{device} (DesiredTopic); a reconciler co-located with
// the hardware driver reads it and owns write-on-diff / verify-by-readback /
// reassert-on-reconnect / non-convergence reporting. It replaces the four
// legacy convergence mechanisms tracked in
// docs/refactor/PRESERVATION_LEDGER.md (rows L1–L7).
//
// This type is introduced but not yet published or subscribed anywhere — the
// first publisher is TASK-027 and the first subscriber (the reconciler) is
// TASK-026, which also adds the topic → DesiredStateV case to SupportedV.
//
// Field-absence semantics (the silent-zero XML lesson applied to the bus):
// a nil *T field is "no opinion — leave that surface as the last standing
// intent set it", which is DISTINCT from an explicit zero, a command:
//   - SetpointW == &0 idles the battery (and is what enforces the SOC reserve,
//     ledger L1) — never confuse it with SetpointW == nil ("no setpoint").
//   - MaxCurrentA == &0 suspends the EVSE.
//   - Solar restore is an explicit large CeilingW (see RestoreCeilingW), NOT a
//     nil CeilingW: absence must never mean "restore to full output".
//
// Per class only that class's fields carry opinion; the rest stay nil (a
// battery document's CeilingW / MaxCurrentA are always nil, etc.).
type DesiredState struct {
	Envelope
	DeviceClass string `json:"device_class"` // "battery" | "solar" | "evse"
	DeviceID    string `json:"device_id"`    // device name, or EVSE stationID

	// CeilingW is the solar generation ceiling (W). Restore-to-full is an
	// explicit large value (RestoreCeilingW), which the device clamps to WMax;
	// nil means "no opinion", NOT "restore".
	CeilingW *float64 `json:"ceiling_w,omitempty"`
	// SetpointW is the battery setpoint (W): +discharge, −charge. An explicit
	// 0 idles the pack (enforces the SOC reserve); nil means "no setpoint".
	SetpointW *float64 `json:"setpoint_w,omitempty"`
	// Connect cease-energizes (false) or energizes (true) the battery; nil =
	// "no connect opinion".
	Connect *bool `json:"connect,omitempty"`
	// MaxCurrentA is the EVSE charging-current ceiling (A). An explicit 0
	// suspends the session; nil means "no current opinion".
	MaxCurrentA *float64 `json:"max_current_a,omitempty"`
	// ConnectorID is the EVSE connector this document targets (0 = the station
	// as a whole, per OCPP; the ocpp bridge maps 0 → 1). Meaningful only for
	// device_class == "evse"; carried inline so EVSE keeps one retained doc per
	// station (topic device == stationID).
	ConnectorID int `json:"connector_id,omitempty"`

	// Source attributes the intent: "csip-event" | "csip-default" |
	// "economic" | "safety".
	Source string `json:"source"`
	// MRID is the active CSIP control this intent derives from, for later
	// CannotComply attribution (TASK-031); empty for economic/safety sources.
	MRID string `json:"mrid,omitempty"`
	// IssuedAt is the publisher's wall clock (Unix seconds) when the document
	// was produced — half of the (seq, issued_at) staleness key (AD-013).
	IssuedAt int64 `json:"issued_at"`
	// Seq is a per-device monotonic counter owned by the publisher. It resets
	// to 0 on a publisher restart, which the consumer distinguishes from a
	// replay via IssuedAt (AD-013 rule 2, the SeqReset case). It is per device,
	// never per class or global.
	Seq uint64 `json:"seq"`
}

// RestoreCeilingW is the "no curtailment" solar ceiling encoded in CeilingW to
// mean "restore to full output" — far above any nameplate so the device clamps
// it to WMax. It mirrors cmd/modbus's restoreCeilingW: restore is an explicit
// value on the wire, never an absent field (AD-013 field-absence semantics).
const RestoreCeilingW = 1e9

// RestoreCurrentA is the EVSE sibling of RestoreCeilingW (WP-13, B3): an
// explicit "no CSMS-imposed charging limit" MaxCurrentA — far above any
// hardware rating, so a release is a VALUE on the wire, never an absent
// field (AD-013 field-absence semantics). The lexa-ocpp reconciler shell maps
// a desired MaxCurrentA at/above the station's rated maximum (this sentinel
// included, by construction) to an OCPP ClearChargingProfile — removing the
// standing TxDefaultProfile — instead of re-setting a large numeric limit.
// Convergence for a release is trivial under the one-sided metered-current
// rule (an EV under its limit is always compliant), so Clear-Accepted is the
// write success and the next plausible sample converges it.
const RestoreCurrentA = 1e6

// Desired-state device classes, the {class} segment of DesiredTopic.
const (
	DesiredClassBattery = "battery"
	DesiredClassSolar   = "solar"
	DesiredClassEVSE    = "evse"
)
