// This file extends the csipmodel package (see resources.go for the package
// doc comment and the XML-namespace invariant) with the full IEEE 2030.5 DER
// function set: DERControlBase (all operating modes, including curve-linked
// ride-through and inverter control modes), DERCurve, DERAvailability, and
// expanded DERCapability / DERStatus as specified in IEEE 2030.5-2018 §10.10.
package csipmodel

import "encoding/xml"

// ─── Operating-mode bitmask constants (DERCapability.ModesSupported) ──────────
//
// These match the DERControlType enumeration in the 2030.5 XSD (Table 21).
// A DER device sets the corresponding bit to advertise support for each mode.
const (
	ModeConnect                uint32 = 1 << 0  // opModConnect / opModEnergize
	ModeMaxLimW                uint32 = 1 << 1  // opModMaxLimW
	ModeFixedW                 uint32 = 1 << 2  // opModFixedW
	ModeFixedVar               uint32 = 1 << 3  // opModFixedVar
	ModeFixedPFAbsorb          uint32 = 1 << 4  // opModFixedPFAbsorbW
	ModeFixedPFInject          uint32 = 1 << 5  // opModFixedPFInjectW
	ModeVoltVar                uint32 = 1 << 6  // opModVoltVar (dynamic Volt-VAr)
	ModeFreqWatt               uint32 = 1 << 7  // opModFreqWatt (Freq-Watt)
	ModeWattPF                 uint32 = 1 << 8  // opModWattPF (Watt-PF)
	ModeVoltWatt               uint32 = 1 << 9  // opModVoltWatt (Volt-Watt)
	ModeHFRTMayTrip            uint32 = 1 << 10 // opModHFRTMayTrip
	ModeHFRTMustTrip           uint32 = 1 << 11 // opModHFRTMustTrip
	ModeHVRTMayTrip            uint32 = 1 << 12 // opModHVRTMayTrip
	ModeHVRTMomentaryCessation uint32 = 1 << 13 // opModHVRTMomentaryCessation
	ModeHVRTMustTrip           uint32 = 1 << 14 // opModHVRTMustTrip
	ModeLFRTMayTrip            uint32 = 1 << 15 // opModLFRTMayTrip
	ModeLFRTMustTrip           uint32 = 1 << 16 // opModLFRTMustTrip
	ModeLVRTMayTrip            uint32 = 1 << 17 // opModLVRTMayTrip
	ModeLVRTMomentaryCessation uint32 = 1 << 18 // opModLVRTMomentaryCessation
	ModeLVRTMustTrip           uint32 = 1 << 19 // opModLVRTMustTrip
	ModeFreqDroop              uint32 = 1 << 20 // opModFreqDroop
	ModeTargetW                uint32 = 1 << 21 // opModTargetW
	ModeTargetVar              uint32 = 1 << 22 // opModTargetVar
	ModeExpLimW                uint32 = 1 << 23 // opModExpLimW
	ModeImpLimW                uint32 = 1 << 24 // opModImpLimW
	ModeGenLimW                uint32 = 1 << 25 // opModGenLimW
	ModeLoadLimW               uint32 = 1 << 26 // opModLoadLimW
)

// ─── DERCurve curve-type codes ────────────────────────────────────────────────
//
// These match the DERCurveType enumeration (IEEE 2030.5-2018 Table 19).
const (
	CurveTypeVoltVar                uint16 = 0
	CurveTypeFreqWatt               uint16 = 1
	CurveTypeWattPF                 uint16 = 2
	CurveTypeVoltWatt               uint16 = 3
	CurveTypeHVRTMayTrip            uint16 = 4
	CurveTypeHVRTMomentaryCessation uint16 = 5
	CurveTypeHVRTMustTrip           uint16 = 6
	CurveTypeLVRTMayTrip            uint16 = 7
	CurveTypeLVRTMomentaryCessation uint16 = 8
	CurveTypeLVRTMustTrip           uint16 = 9
	CurveTypeHFRTMayTrip            uint16 = 10
	CurveTypeHFRTMustTrip           uint16 = 11
	CurveTypeLFRTMayTrip            uint16 = 12
	CurveTypeLFRTMustTrip           uint16 = 13
)

// ─── DER status code constants ────────────────────────────────────────────────

// Generator connection status codes (genConnectStatus).
const (
	GenConnectAvailable uint8 = 0 // available but not connected
	GenConnectConnected uint8 = 1 // connected and operating
	GenConnectTest      uint8 = 2 // in test mode
	GenConnectFault     uint8 = 3 // fault condition
)

// Operational mode status codes (operationalModeStatus).
const (
	OpStatusIdle      uint8 = 0
	OpStatusOperating uint8 = 1
	OpStatusStandby   uint8 = 2
	OpStatusShutdown  uint8 = 3
	OpStatusFault     uint8 = 4
	OpStatusSleeping  uint8 = 5
)

// Storage mode status codes (storageModeStatus).
const (
	StorageIdle        uint8 = 0
	StorageCharging    uint8 = 1
	StorageDischarging uint8 = 2
)

// Inverter status codes (inverterStatus).
const (
	InverterIdle      uint8 = 0
	InverterOperating uint8 = 1
	InverterOff       uint8 = 2
	InverterFault     uint8 = 3
)

// ─── Curve types ──────────────────────────────────────────────────────────────

// CurveLink is a reference to a DERCurve resource, embedded in DERControlBase.
// The server populates the href attribute; the client resolves the curve from
// its local DERCurveList cache.
type CurveLink struct {
	Href string `xml:"href,attr,omitempty"`
}

// DERCurveData is one (x, y) point in a piecewise-linear DERCurve.
// Units depend on the curve type (e.g., voltage in % of nominal for Volt-VAr).
type DERCurveData struct {
	XValue int32 `xml:"xvalue"` // x-axis value
	YValue int32 `xml:"yvalue"` // y-axis value
}

// DERCurve is a piecewise-linear inverter characteristic curve.
// It is referenced from DERControlBase via the opMod*Link fields.
type DERCurve struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns DERCurve"`
	Resource

	MRID         string `xml:"mRID,omitempty"`
	Description  string `xml:"description,omitempty"`
	Version      uint16 `xml:"version,omitempty"`
	CreationTime int64  `xml:"creationTime,omitempty"`

	// CurveType identifies what this curve represents (see CurveType* constants).
	CurveType uint16 `xml:"curveType"`

	// CurveData is the ordered list of (x,y) breakpoints.
	CurveData []DERCurveData `xml:"CurveData,omitempty"`

	// AutonomousVRefEnable: when true (for Volt-VAr), the device computes its own
	// voltage reference. Enabling this implicitly enables autonomous anti-islanding.
	AutonomousVRefEnable *bool `xml:"autonomousVRefEnable,omitempty"`
	// AutonomousVRefTimeConstant is the filtering time constant (seconds) for the
	// autonomous voltage reference (Volt-VAr curves only).
	AutonomousVRefTimeConstant *uint32 `xml:"autonomousVRefTimeConstant,omitempty"`

	// OpenLoopTms: time (in hundredths of a second) to reach 90 % of the
	// commanded output. Applies to VoltVar and VoltWatt modes.
	OpenLoopTms *uint16 `xml:"openLoopTms,omitempty"`

	// Ramp timing — all in hundredths of a second.
	RampDecTms *uint16 `xml:"rampDecTms,omitempty"` // output decrease ramp time
	RampIncTms *uint16 `xml:"rampIncTms,omitempty"` // output increase ramp time
	RampPT1Tms *uint16 `xml:"rampPT1Tms,omitempty"` // first-order lag time constant

	// Axis multipliers: apply 10^multiplier to all x or y values.
	XMultiplier int8 `xml:"xMultiplier,omitempty"`
	YMultiplier int8 `xml:"yMultiplier,omitempty"`

	// VRef: nominal AC voltage reference in V for VoltVar / VoltWatt curves.
	VRef *int16 `xml:"vRef,omitempty"`

	// XRefType / YRefType indicate the physical quantity on each axis (Table 19).
	XRefType uint8 `xml:"xRefType,omitempty"`
	YRefType uint8 `xml:"yRefType,omitempty"`
}

// DERCurveList is a collection of DERCurve resources belonging to one DERProgram.
type DERCurveList struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns DERCurveList"`
	Resource

	All      uint32     `xml:"all,attr"`
	Results  uint32     `xml:"results,attr"`
	PollRate uint32     `xml:"pollRate,attr,omitempty"`
	DERCurve []DERCurve `xml:"DERCurve"`
}

// ─── FreqDroop (anti-islanding / frequency droop settings) ───────────────────
//
// opModFreqDroop is the only DERControlBase mode that carries inline parameters
// rather than a curve link. It is used for active anti-islanding and frequency
// regulation.

// FreqDroop defines frequency droop (Frequency-Droop) parameters.
// All frequency values in mHz; all time values in hundredths of a second.
type FreqDroop struct {
	// dBuf: frequency dead-band width above/below nominal in mHz.
	DBuf uint16 `xml:"dBuf"`
	// dF: frequency deviation that triggers full droop response, in mHz.
	DF uint16 `xml:"dF"`
	// dP: change in output power per unit frequency deviation (W per Hz × 100).
	DP uint16 `xml:"dP"`
	// openLoopTms: time to reach 90 % of commanded output (hundredths of a second).
	OpenLoopTms uint16 `xml:"openLoopTms"`
	// tResponse: aggregate response time constant (hundredths of a second).
	TResponse uint16 `xml:"tResponse"`
}

// ─── ReactivePower / WattPower — for opModTargetVar / opModTargetW ───────────

// ReactivePower represents a reactive power set point.
// Value is in VAr; apply 10^Multiplier to get actual VAr.
type ReactivePower struct {
	Multiplier int8  `xml:"multiplier"`
	Value      int16 `xml:"value"`
}

// ─── Expanded DERControlBase ──────────────────────────────────────────────────
//
// The DERControlBase in resources.go holds the scalar modes. This file extends
// it with the curve-linked and droop modes that are defined elsewhere in the
// 2030.5 XSD.
//
// We cannot embed two structs with overlapping XML element names in Go's
// encoding/xml, so we extend DERControlBase directly with additional fields.
// The struct in resources.go is replaced by this comprehensive version.

// ExtendedDERControlBase is the full IEEE 2030.5 DERControlBase with both scalar
// operating modes and curve-linked / droop modes.
//
// The narrower DERControlBase in resources.go is kept for compatibility with the
// scheduler (which only ever touches scalar modes). The walker resolves curve
// links and stores them in the schedule layer, not here.
//
// XML element names match the 2030.5 schema exactly (case-sensitive).
type ExtendedDERControlBase struct {
	// ── Scalar modes ─────────────────────────────────────────────────────────
	OpModConnect        *bool          `xml:"opModConnect,omitempty"`
	OpModEnergize       *bool          `xml:"opModEnergize,omitempty"`
	OpModFixedPFAbsorbW *SignedPerCent `xml:"opModFixedPFAbsorbW,omitempty"`
	OpModFixedPFInjectW *SignedPerCent `xml:"opModFixedPFInjectW,omitempty"`
	OpModFixedVar       *FixedVar      `xml:"opModFixedVar,omitempty"`
	OpModFixedW         *ActivePower   `xml:"opModFixedW,omitempty"`
	OpModMaxLimW        *ActivePower   `xml:"opModMaxLimW,omitempty"`
	OpModExpLimW        *ActivePower   `xml:"opModExpLimW,omitempty"`
	OpModGenLimW        *ActivePower   `xml:"opModGenLimW,omitempty"`
	OpModImpLimW        *ActivePower   `xml:"opModImpLimW,omitempty"`
	OpModLoadLimW       *ActivePower   `xml:"opModLoadLimW,omitempty"`
	OpModTargetW        *ActivePower   `xml:"opModTargetW,omitempty"`
	OpModTargetVar      *ReactivePower `xml:"opModTargetVar,omitempty"`
	RampTms             *uint16        `xml:"rampTms,omitempty"`

	// ── Curve-linked modes — each holds an href to a DERCurve ────────────────
	// Dynamic Volt-VAr — anti-islanding baseline mode (§10.10.4.2).
	OpModVoltVar *CurveLink `xml:"opModVoltVar,omitempty"`
	// Frequency-Watt — droop-based frequency regulation (§10.10.4.3).
	OpModFreqWatt *CurveLink `xml:"opModFreqWatt,omitempty"`
	// Watt-PF — power-factor as a function of real power output (§10.10.4.5).
	OpModWattPF *CurveLink `xml:"opModWattPF,omitempty"`
	// Volt-Watt — ramp real power output as a function of voltage (§10.10.4.4).
	OpModVoltWatt *CurveLink `xml:"opModVoltWatt,omitempty"`

	// High-frequency ride-through curves.
	OpModHFRTMayTrip  *CurveLink `xml:"opModHFRTMayTrip,omitempty"`
	OpModHFRTMustTrip *CurveLink `xml:"opModHFRTMustTrip,omitempty"`
	// High-voltage ride-through curves.
	OpModHVRTMayTrip            *CurveLink `xml:"opModHVRTMayTrip,omitempty"`
	OpModHVRTMomentaryCessation *CurveLink `xml:"opModHVRTMomentaryCessation,omitempty"`
	OpModHVRTMustTrip           *CurveLink `xml:"opModHVRTMustTrip,omitempty"`
	// Low-frequency ride-through curves.
	OpModLFRTMayTrip  *CurveLink `xml:"opModLFRTMayTrip,omitempty"`
	OpModLFRTMustTrip *CurveLink `xml:"opModLFRTMustTrip,omitempty"`
	// Low-voltage ride-through curves.
	OpModLVRTMayTrip            *CurveLink `xml:"opModLVRTMayTrip,omitempty"`
	OpModLVRTMomentaryCessation *CurveLink `xml:"opModLVRTMomentaryCessation,omitempty"`
	OpModLVRTMustTrip           *CurveLink `xml:"opModLVRTMustTrip,omitempty"`

	// ── Frequency droop (inline, not a curve link) ────────────────────────────
	OpModFreqDroop *FreqDroop `xml:"opModFreqDroop,omitempty"`
}

// ExtendedDERControl wraps a DERControl with the full ExtendedDERControlBase.
// The walker populates this when DERCurveListLink is present in a program.
type ExtendedDERControl struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns DERControl"`
	Resource

	// Event-base RespondableResource attributes (audit CSIP-004) — the
	// extended (curve-linked) DERControl carries the same replyTo/
	// responseRequired the plain DERControl does; see DERControl in
	// resources.go for semantics. Additive/omitempty.
	ReplyTo          string            `xml:"replyTo,attr,omitempty"`
	ResponseRequired *ResponseRequired `xml:"responseRequired,attr,omitempty"`

	MRID              string                 `xml:"mRID,omitempty"`
	Description       string                 `xml:"description,omitempty"`
	Version           uint16                 `xml:"version,omitempty"`
	CreationTime      int64                  `xml:"creationTime,omitempty"`
	EventStatus       *EventStatus           `xml:"EventStatus,omitempty"`
	Interval          DateTimeInterval       `xml:"interval"`
	DERControlBase    ExtendedDERControlBase `xml:"DERControlBase"`
	RandomizeStart    *int32                 `xml:"randomizeStart,omitempty"`
	RandomizeDuration *int32                 `xml:"randomizeDuration,omitempty"`
}

// ExtendedDERControlList is a collection of ExtendedDERControl events.
type ExtendedDERControlList struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns DERControlList"`
	Resource

	All        uint32               `xml:"all,attr"`
	Results    uint32               `xml:"results,attr"`
	PollRate   uint32               `xml:"pollRate,attr,omitempty"`
	DERControl []ExtendedDERControl `xml:"DERControl"`
}

// ExtendedDefaultDERControl is a DefaultDERControl with the full control base.
type ExtendedDefaultDERControl struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns DefaultDERControl"`
	Resource

	MRID           string                 `xml:"mRID,omitempty"`
	Description    string                 `xml:"description,omitempty"`
	Version        uint16                 `xml:"version,omitempty"`
	DERControlBase ExtendedDERControlBase `xml:"DERControlBase"`
}

// ─── DERCapability (expanded) ─────────────────────────────────────────────────

// DERCapabilityFull is the expanded DERCapability with modesSupported bitmask
// and all nameplate ratings required by §10.10.2.
type DERCapabilityFull struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns DERCapability"`
	Resource

	// Type: 0=unknown, 1=virtual/mixed, 2=reciprocating engine, 80=PV, 81=wind,
	// 82=running CHP, 83=storage, 84=electric vehicle, 85=EVSE, 86=combined PV+storage.
	Type uint8 `xml:"type"`

	// ModesSupported is a bitmask of the DERControlBase operating modes this
	// DER supports. See Mode* constants defined above.
	ModesSupported uint32 `xml:"modesSupported"`

	// Nameplate ratings (all use ActivePower — value × 10^multiplier in W or VA or VAr).
	RtgMaxW              ActivePower  `xml:"rtgMaxW"`                        // nameplate peak active power
	RtgMaxVA             *ActivePower `xml:"rtgMaxVA,omitempty"`             // nameplate peak apparent power
	RtgMaxVar            *ActivePower `xml:"rtgMaxVar,omitempty"`            // nameplate peak reactive power (absorb)
	RtgMaxVarNeg         *ActivePower `xml:"rtgMaxVarNeg,omitempty"`         // nameplate peak reactive power (inject)
	RtgMinPFOverExcited  *int16       `xml:"rtgMinPFOverExcited,omitempty"`  // min power factor, over-excited
	RtgMinPFUnderExcited *int16       `xml:"rtgMinPFUnderExcited,omitempty"` // min power factor, under-excited
	RtgMaxChargeRateW    *ActivePower `xml:"rtgMaxChargeRateW,omitempty"`    // max charge rate (storage)
	RtgMaxDischargeRateW *ActivePower `xml:"rtgMaxDischargeRateW,omitempty"` // max discharge rate
	RtgVNom              *int32       `xml:"rtgVNom,omitempty"`              // nominal voltage (V)
	RtgVarNomPct         *int16       `xml:"rtgVarNomPct,omitempty"`         // var at nominal voltage (% of rtgMaxVA)
	RtgWOvPF             *int16       `xml:"rtgWOvPF,omitempty"`             // reactive power capability at rated W, over PF (VAr)
}

// ─── DERStatus (full) ─────────────────────────────────────────────────────────

// DERStatusValue is a typed measurement + timestamp.
type DERStatusValue struct {
	DateTime int64 `xml:"dateTime"`
	Value    uint8 `xml:"value"`
}

// DERStatusFull contains the complete real-time DER operational status.
type DERStatusFull struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns DERStatus"`
	Resource

	// ReadingTime is when this status snapshot was captured (Unix seconds).
	ReadingTime int64 `xml:"readingTime,omitempty"`

	// GenConnectStatus: generator connection state (see GenConnect* constants).
	GenConnectStatus *DERStatusValue `xml:"genConnectStatus,omitempty"`

	// InverterStatus: current inverter operating state (see Inverter* constants).
	InverterStatus *DERStatusValue `xml:"inverterStatus,omitempty"`

	// LocalControlModeStatus: 0=remote, 1=local.
	LocalControlModeStatus *DERStatusValue `xml:"localControlModeStatus,omitempty"`

	// ManufacturerStatus: manufacturer-defined status code.
	ManufacturerStatus *struct {
		DateTime    int64  `xml:"dateTime"`
		Description string `xml:"description,omitempty"`
		PEVInfo     string `xml:"pEVInfo,omitempty"`
	} `xml:"manufacturerStatus,omitempty"`

	// OperationalModeStatus: current operating mode (see OpStatus* constants).
	OperationalModeStatus *DERStatusValue `xml:"operationalModeStatus,omitempty"`

	// StateOfChargeStatus: battery state-of-charge in percent × 100 (0–10000).
	StateOfChargeStatus *struct {
		DateTime int64 `xml:"dateTime"`
		Value    int16 `xml:"value"` // 0–10000 (= 0–100.00 %)
	} `xml:"stateOfChargeStatus,omitempty"`

	// StorageModeStatus: charge/discharge/idle (see Storage* constants).
	StorageModeStatus *DERStatusValue `xml:"storageModeStatus,omitempty"`
}

// ─── DERAvailability ─────────────────────────────────────────────────────────

// DERAvailability represents the device's current and short-term available power.
type DERAvailability struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns DERAvailability"`
	Resource

	// ReadingTime is when this reading was captured (Unix seconds).
	ReadingTime int64 `xml:"readingTime,omitempty"`

	// AvailabilityDuration is how long the device can sustain its current
	// output (seconds). 0 means the information is not available.
	AvailabilityDuration *uint32 `xml:"availabilityDuration,omitempty"`

	// MaxChargeDuration is how long the device can sustain its maximum charge
	// rate (seconds). Storage only.
	MaxChargeDuration *uint32 `xml:"maxChargeDuration,omitempty"`

	// EstimatedVarAvail is the estimated reactive power available right now.
	EstimatedVarAvail *ReactivePower `xml:"estimatedVarAvail,omitempty"`

	// EstimatedWAvail is the estimated real power available right now.
	EstimatedWAvail *ActivePower `xml:"estimatedWAvail,omitempty"`

	// MaxForecastW is the forecast of maximum available real power over the
	// next period. Used for look-ahead dispatch.
	MaxForecastW *ActivePower `xml:"maxForecastW,omitempty"`
}

// ─── DERSettings (expanded) ──────────────────────────────────────────────────

// DERSettingsFull is the expanded DERSettings including all configurable limits.
type DERSettingsFull struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns DERSettings"`
	Resource

	UpdatedTime int64 `xml:"updatedTime,omitempty"`

	// Power limits — operator-configured ceilings.
	SetMaxW      *ActivePower `xml:"setMaxW,omitempty"`      // max real power output
	SetMaxVA     *ActivePower `xml:"setMaxVA,omitempty"`     // max apparent power
	SetMaxVar    *ActivePower `xml:"setMaxVar,omitempty"`    // max reactive power (absorb)
	SetMaxVarNeg *ActivePower `xml:"setMaxVarNeg,omitempty"` // max reactive power (inject)

	// Power factor limits (signed, hundredths: 95 = 0.95 leading).
	SetMinPFOverExcited  *int16 `xml:"setMinPFOverExcited,omitempty"`
	SetMinPFUnderExcited *int16 `xml:"setMinPFUnderExcited,omitempty"`

	// Storage-specific limits.
	SetMaxChargeRateW    *ActivePower `xml:"setMaxChargeRateW,omitempty"`
	SetMaxDischargeRateW *ActivePower `xml:"setMaxDischargeRateW,omitempty"`
	SetStorBattTarget    *int16       `xml:"setStorBattTarget,omitempty"` // target SOC % × 100

	// Voltage reference for Volt-VAr curves (V, integer).
	SetVRef *int32 `xml:"setVRef,omitempty"`
}
