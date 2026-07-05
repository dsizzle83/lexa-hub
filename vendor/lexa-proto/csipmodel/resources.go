// Package csipmodel defines Go structs for the IEEE 2030.5 / CSIP XML data
// model — the wire-format types both lexa-hub (client-side unmarshal) and
// csip-tls-test's gridsim (server-side marshal) work from (TASK-023). This is
// the data model only: walkers, schedulers, identity, and DNS-SD stay
// repo-local forks that merely import this package.
//
// Every struct uses XML tags that match the 2030.5 schema exactly,
// including the mandatory namespace urn:ieee:std:2030.5:ns.
// The inheritance hierarchy in the XSD (Resource → IdentifiedObject →
// SubscribableResource, etc.) is flattened into Go structs with embedded
// fields, because Go's encoding/xml handles embedded struct tags correctly.
//
// Only the resource types required by a CSIP DER client (and the gridsim
// that serves them) are defined here. Prepayment and messaging function
// sets are out of scope.
//
// CRITICAL — silent-failure hazard: a 2030.5 root element unmarshalled
// without its namespace (urn:ieee:std:2030.5:ns) decodes to a zero-value
// struct with NO error from encoding/xml. Every root element below carries
// an explicit `xml:"urn:ieee:std:2030.5:ns <Name>"` XMLName tag for exactly
// this reason — never add a root element type without one, and never edit
// an existing tag without re-running the round-trip suite in
// resources_test.go plus both consumers' conformance suites.
package csipmodel

import "encoding/xml"

// XMLNamespace is the IEEE 2030.5 XML namespace required on all root elements.
const XMLNamespace = "urn:ieee:std:2030.5:ns"

// ───────────────────────────────────────────────────────────────────────
// Base types — these model the XSD inheritance chain
// ───────────────────────────────────────────────────────────────────────

// Link is the base type for all link elements (EndDeviceListLink, TimeLink, etc.).
// In the XSD every *Link type has an href attribute.
type Link struct {
	Href string `xml:"href,attr"`
}

// ListLink extends Link with an "all" attribute indicating the total
// number of items in the referenced list.
type ListLink struct {
	Link
	All uint32 `xml:"all,attr,omitempty"`
}

// Resource is the base of most 2030.5 types — it carries an href.
type Resource struct {
	Href string `xml:"href,attr,omitempty"`
}

// ───────────────────────────────────────────────────────────────────────
// DeviceCapability — the root of the resource tree (GET /dcap)
// ───────────────────────────────────────────────────────────────────────

// DeviceCapability is returned by the server at the well-known /dcap URI.
// It is the entry point for all resource discovery. The client reads the
// link elements to find where EndDeviceList, Time, etc. live.
type DeviceCapability struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns DeviceCapability"`
	Resource

	// PollRate is the default polling interval for this function set, in seconds.
	// If omitted the spec default is 900 (15 min).
	PollRate uint32 `xml:"pollRate,attr,omitempty"`

	// Links inherited from FunctionSetAssignmentsBase
	DERProgramListLink    *ListLink `xml:"DERProgramListLink,omitempty"`
	TimeLink              *Link     `xml:"TimeLink,omitempty"`
	ResponseSetListLink   *ListLink `xml:"ResponseSetListLink,omitempty"`
	TariffProfileListLink *ListLink `xml:"TariffProfileListLink,omitempty"`

	// DeviceCapability-specific links
	EndDeviceListLink        *ListLink `xml:"EndDeviceListLink,omitempty"`
	MirrorUsagePointListLink *ListLink `xml:"MirrorUsagePointListLink,omitempty"`
	SelfDeviceLink           *Link     `xml:"SelfDeviceLink,omitempty"`
}

// ───────────────────────────────────────────────────────────────────────
// Time
// ───────────────────────────────────────────────────────────────────────

// Time is the server's current time resource. CSIP clients must
// synchronize to the server's time for event scheduling.
type Time struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns Time"`
	Resource

	// CurrentTime is seconds since Unix epoch (2030.5 uses Unix time).
	CurrentTime int64 `xml:"currentTime"`

	// DstEndTime is the end of DST in Unix time.
	DstEndTime int64 `xml:"dstEndTime"`

	// DstOffset is the DST offset in seconds.
	DstOffset int32 `xml:"dstOffset"`

	// TzOffset is the timezone offset from UTC in seconds.
	TzOffset int32 `xml:"tzOffset"`

	// Quality describes the clock source quality.
	Quality uint8 `xml:"quality,omitempty"`

	PollRate uint32 `xml:"pollRate,attr,omitempty"`
}

// ───────────────────────────────────────────────────────────────────────
// EndDevice and EndDeviceList
// ───────────────────────────────────────────────────────────────────────

// EndDevice represents a single DER device registered with the server.
// The client finds itself in the EndDeviceList by matching its LFDI.
type EndDevice struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns EndDevice"`
	Resource

	// Subscribable indicates subscription support. 0=none, 1=non-conditional, 3=conditional.
	Subscribable uint8 `xml:"subscribable,attr,omitempty"`

	// LFDI is the Long-Form Device Identifier (hex-encoded, 40 chars).
	LFDI string `xml:"lFDI,omitempty"`

	// SFDI is the Short-Form Device Identifier (decimal, up to 10 digits).
	SFDI uint64 `xml:"sFDI,omitempty"`

	// ChangedTime is the last-modified timestamp.
	ChangedTime int64 `xml:"changedTime,omitempty"`

	// Enabled indicates whether the device is enabled by the server.
	Enabled *bool `xml:"enabled,omitempty"`

	// Links to subordinate resources
	DERListLink                     *ListLink `xml:"DERListLink,omitempty"`
	FunctionSetAssignmentsListLink  *ListLink `xml:"FunctionSetAssignmentsListLink,omitempty"`
	RegistrationLink                *Link     `xml:"RegistrationLink,omitempty"`
	LogEventListLink                *ListLink `xml:"LogEventListLink,omitempty"`
	FlowReservationRequestListLink  *ListLink `xml:"FlowReservationRequestListLink,omitempty"`
	FlowReservationResponseListLink *ListLink `xml:"FlowReservationResponseListLink,omitempty"`
}

// EndDeviceList is a collection of EndDevice resources.
type EndDeviceList struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns EndDeviceList"`
	Resource

	All       uint32      `xml:"all,attr"`
	Results   uint32      `xml:"results,attr"`
	PollRate  uint32      `xml:"pollRate,attr,omitempty"`
	EndDevice []EndDevice `xml:"EndDevice"`
}

// ───────────────────────────────────────────────────────────────────────
// Registration
// ───────────────────────────────────────────────────────────────────────

// Registration holds the registration info for an EndDevice, including the PIN.
type Registration struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns Registration"`
	Resource

	DateTimeRegistered int64  `xml:"dateTimeRegistered"`
	PIN                uint32 `xml:"pIN"`
}

// ───────────────────────────────────────────────────────────────────────
// FunctionSetAssignments (FSA)
// ───────────────────────────────────────────────────────────────────────

// FunctionSetAssignments groups a set of programs assigned to a device.
// Each EndDevice has a FunctionSetAssignmentsListLink pointing to its FSAs.
type FunctionSetAssignments struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns FunctionSetAssignments"`
	Resource

	// Subscribable indicates subscription support.
	Subscribable uint8 `xml:"subscribable,attr,omitempty"`

	// Links to assigned function set resource lists.
	DERProgramListLink      *ListLink `xml:"DERProgramListLink,omitempty"`
	TimeLink                *Link     `xml:"TimeLink,omitempty"`
	TariffProfileListLink   *ListLink `xml:"TariffProfileListLink,omitempty"`
	CustomerAccountListLink *ListLink `xml:"CustomerAccountListLink,omitempty"`
	MRID                    string    `xml:"mRID,omitempty"`
	Description             string    `xml:"description,omitempty"`
	Version                 uint16    `xml:"version,omitempty"`
}

// FunctionSetAssignmentsList is a collection of FSA resources.
type FunctionSetAssignmentsList struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns FunctionSetAssignmentsList"`
	Resource

	All                    uint32                   `xml:"all,attr"`
	Results                uint32                   `xml:"results,attr"`
	PollRate               uint32                   `xml:"pollRate,attr,omitempty"`
	FunctionSetAssignments []FunctionSetAssignments `xml:"FunctionSetAssignments"`
}

// ───────────────────────────────────────────────────────────────────────
// DERProgram
// ───────────────────────────────────────────────────────────────────────

// DERProgram represents a utility's DER management program.
// It contains links to control lists, default controls, and curves.
type DERProgram struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns DERProgram"`
	Resource

	// Subscribable indicates subscription support.
	Subscribable uint8 `xml:"subscribable,attr,omitempty"`

	MRID        string `xml:"mRID,omitempty"`
	Description string `xml:"description,omitempty"`
	Version     uint16 `xml:"version,omitempty"`

	// Primacy determines which program's controls take priority.
	// Lower value = higher priority. CSIP requires this.
	Primacy uint8 `xml:"primacy"`

	// Links to subordinate resources
	DERControlListLink       *ListLink `xml:"DERControlListLink,omitempty"`
	DERCurveListLink         *ListLink `xml:"DERCurveListLink,omitempty"`
	DefaultDERControlLink    *Link     `xml:"DefaultDERControlLink,omitempty"`
	ActiveDERControlListLink *ListLink `xml:"ActiveDERControlListLink,omitempty"`
}

// DERProgramList is a collection of DERProgram resources.
type DERProgramList struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns DERProgramList"`
	Resource

	All        uint32       `xml:"all,attr"`
	Results    uint32       `xml:"results,attr"`
	PollRate   uint32       `xml:"pollRate,attr,omitempty"`
	DERProgram []DERProgram `xml:"DERProgram"`
}

// ───────────────────────────────────────────────────────────────────────
// DERControl and DefaultDERControl
// ───────────────────────────────────────────────────────────────────────

// DateTimeInterval represents a time interval with start and duration.
type DateTimeInterval struct {
	Duration uint32 `xml:"duration"`
	Start    int64  `xml:"start"`
}

// SignedPerCent represents a signed percentage × 100 (so 50% = 5000).
type SignedPerCent struct {
	Value int16 `xml:",chardata"`
}

// ActivePower represents watts with a power-of-ten multiplier.
type ActivePower struct {
	Multiplier int8  `xml:"multiplier"`
	Value      int16 `xml:"value"`
}

// FixedVar represents reactive power setting.
type FixedVar struct {
	RefType uint8         `xml:"refType"`
	Value   SignedPerCent `xml:"value"`
}

// DERControlBase contains the actual control parameters — what the DER
// should do. This is the payload of both DERControl events and the
// DefaultDERControl fallback.
type DERControlBase struct {
	// Operating modes — each is optional; the server sends only what it
	// wants to control.
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
	RampTms             *uint16        `xml:"rampTms,omitempty"`
}

// EventStatus describes the current state of an event.
type EventStatus struct {
	CurrentStatus         uint8 `xml:"currentStatus"`
	DateTime              int64 `xml:"dateTime"`
	PotentiallySuperseded bool  `xml:"potentiallySuperseded"`
}

// DERControl is a time-bound control event within a DERProgram.
// This is the main thing your client acts on.
type DERControl struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns DERControl"`
	Resource

	MRID           string           `xml:"mRID,omitempty"`
	Description    string           `xml:"description,omitempty"`
	Version        uint16           `xml:"version,omitempty"`
	CreationTime   int64            `xml:"creationTime,omitempty"`
	EventStatus    *EventStatus     `xml:"EventStatus,omitempty"`
	Interval       DateTimeInterval `xml:"interval"`
	DERControlBase DERControlBase   `xml:"DERControlBase"`

	// Randomize fields for staggering device responses
	RandomizeStart    *int32 `xml:"randomizeStart,omitempty"`
	RandomizeDuration *int32 `xml:"randomizeDuration,omitempty"`
}

// DERControlList is a collection of DERControl events.
type DERControlList struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns DERControlList"`
	Resource

	All        uint32       `xml:"all,attr"`
	Results    uint32       `xml:"results,attr"`
	PollRate   uint32       `xml:"pollRate,attr,omitempty"`
	DERControl []DERControl `xml:"DERControl"`
}

// DefaultDERControl is the fallback control that applies when no active
// DERControl event is in effect. Critical safety mechanism — prevents
// uncontrolled operation if comms are lost.
type DefaultDERControl struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns DefaultDERControl"`
	Resource

	MRID           string         `xml:"mRID,omitempty"`
	Description    string         `xml:"description,omitempty"`
	Version        uint16         `xml:"version,omitempty"`
	DERControlBase DERControlBase `xml:"DERControlBase"`
}

// ───────────────────────────────────────────────────────────────────────
// DER resource (device-level DER info)
// ───────────────────────────────────────────────────────────────────────

// DER represents a logical DER associated with an EndDevice.
type DER struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns DER"`
	Resource

	DERCapabilityLink   *Link `xml:"DERCapabilityLink,omitempty"`
	DERSettingsLink     *Link `xml:"DERSettingsLink,omitempty"`
	DERStatusLink       *Link `xml:"DERStatusLink,omitempty"`
	DERAvailabilityLink *Link `xml:"DERAvailabilityLink,omitempty"`
}

// DERList is a collection of DER resources.
type DERList struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns DERList"`
	Resource

	All     uint32 `xml:"all,attr"`
	Results uint32 `xml:"results,attr"`
	DER     []DER  `xml:"DER"`
}

// ───────────────────────────────────────────────────────────────────────
// MirrorUsagePoint (telemetry)
// ───────────────────────────────────────────────────────────────────────

// MirrorUsagePoint is used by the client to POST telemetry readings
// back to the utility server.
type MirrorUsagePoint struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns MirrorUsagePoint"`
	Resource

	MRID                string `xml:"mRID,omitempty"`
	Description         string `xml:"description,omitempty"`
	RoleFlags           uint16 `xml:"roleFlags,omitempty"`
	ServiceCategoryKind uint8  `xml:"serviceCategoryKind,omitempty"`
	Status              uint8  `xml:"status,omitempty"`
	DeviceLFDI          string `xml:"deviceLFDI,omitempty"`
	PostRate            uint32 `xml:"postRate,omitempty"`
}

// MirrorUsagePointList is a collection of MirrorUsagePoint resources.
type MirrorUsagePointList struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns MirrorUsagePointList"`
	Resource

	All              uint32             `xml:"all,attr"`
	Results          uint32             `xml:"results,attr"`
	PollRate         uint32             `xml:"pollRate,attr,omitempty"`
	MirrorUsagePoint []MirrorUsagePoint `xml:"MirrorUsagePoint"`
}

// ───────────────────────────────────────────────────────────────────────
// MirrorMeterReading (telemetry POST payload)
// ───────────────────────────────────────────────────────────────────────

// ReadingType describes the measurement commodity, units, and accumulation
// behaviour of a set of readings. IEEE 2030.5 table 22.
type ReadingType struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns ReadingType"`
	Resource

	AccumulationBehaviour     uint8  `xml:"accumulationBehaviour,omitempty"`
	CommodityType             uint8  `xml:"commodity,omitempty"`
	DataQualifier             uint8  `xml:"dataQualifier,omitempty"`
	FlowDirection             uint8  `xml:"flowDirection,omitempty"`
	IntervalLength            uint32 `xml:"intervalLength,omitempty"`
	Kind                      uint8  `xml:"kind,omitempty"`
	NumberOfConsumptionBlocks uint8  `xml:"numberOfConsumptionBlocks,omitempty"`
	NumberOfTouTiers          uint8  `xml:"numberOfTouTiers,omitempty"`
	Phase                     uint16 `xml:"phase,omitempty"`
	PowerOfTenMultiplier      int8   `xml:"powerOfTenMultiplier,omitempty"`
	TieredConsumptionBlocks   *bool  `xml:"tieredConsumptionBlocks,omitempty"`
	Uom                       uint8  `xml:"uom,omitempty"`
}

// Reading is a single measured value within a MirrorReadingSet.
type Reading struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns Reading"`

	// LocalID disambiguates multiple readings in one set.
	LocalID      uint16            `xml:"localID,omitempty"`
	TimePeriod   *DateTimeInterval `xml:"timePeriod,omitempty"`
	Value        int64             `xml:"value,omitempty"`
	QualityFlags uint16            `xml:"qualityFlags,omitempty"`
}

// MirrorReadingSet is a timestamped batch of readings for one reporting interval.
type MirrorReadingSet struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns MirrorReadingSet"`
	Resource

	StartTime int64     `xml:"timePeriod>start"`
	Duration  uint32    `xml:"timePeriod>duration"`
	Reading   []Reading `xml:"Reading"`
}

// MirrorMeterReading is the payload the client POSTs to /mup/{n}
// to report periodic telemetry. Each POST is one reading set.
type MirrorMeterReading struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns MirrorMeterReading"`
	Resource

	MRID             string             `xml:"mRID,omitempty"`
	Description      string             `xml:"description,omitempty"`
	ReadingType      *ReadingType       `xml:"ReadingType,omitempty"`
	MirrorReadingSet []MirrorReadingSet `xml:"MirrorReadingSet,omitempty"`
}

// ───────────────────────────────────────────────────────────────────────
// Response resources (GEN.044 — client must acknowledge events)
// ───────────────────────────────────────────────────────────────────────

// Response status codes (IEEE 2030.5 table 27).
const (
	ResponseEventReceived   uint8 = 1 // event text received and understood
	ResponseEventStarted    uint8 = 2 // event interval began
	ResponseEventCompleted  uint8 = 3 // event interval ended
	ResponseOptIn           uint8 = 4 // client opted in (for opt-in programs)
	ResponseOptOut          uint8 = 5 // client opted out
	ResponseEventCancelled  uint8 = 6 // event cancelled by the server (CORE-022)
	ResponseEventSuperseded uint8 = 7 // event superseded by an overlapping event (CORE-023)

	// ResponseCannotComply is a LEXA profile extension (NOT an IEEE 2030.5
	// Table 27 status). It alerts the server that the DER physically cannot meet
	// an active control limit — e.g. an import cap that would require battery
	// discharge below its SOC reserve. Chosen in the 0xF0–0xFF manufacturer
	// range so it never collides with a standard status (1–7); the gridsim
	// server treats any status ≥ 0xF0 as a resource-limited non-compliance
	// alert rather than a lifecycle acknowledgement.
	ResponseCannotComply uint8 = 0xF0 // 240 — LEXA: DER unable to honour the control
)

// IEEE 2030.5 UomType codes (Table for ReadingType.uom) used by MUP telemetry.
const (
	UomWatts uint8 = 38 // real power, W
	UomVolts uint8 = 29 // voltage, V
	UomHertz uint8 = 33 // frequency, Hz
)

// DataQualifier codes (ReadingType.dataQualifier).
const DataQualifierAverage uint8 = 2

// KindType codes (ReadingType.kind).
const (
	KindPower   uint8 = 37 // power (W)
	KindVoltage uint8 = 12 // voltage
	KindFreq    uint8 = 38 // frequency
)

// Response is posted by the client to the server's ResponseSetListLink
// to acknowledge receipt and state transitions of DERControl events.
// Per GEN.044, a conformant client must POST a Response for each event
// at each transition: received (1), started (2), completed (3).
type Response struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns Response"`
	Resource

	// CreatedDateTime is when this response was generated (server time).
	CreatedDateTime int64 `xml:"createdDateTime,omitempty"`
	// EndDeviceLFDI identifies the responding device.
	EndDeviceLFDI string `xml:"endDeviceLFDI,omitempty"`
	// Status is one of the ResponseEvent* constants above.
	Status uint8 `xml:"status"`
	// Subject is the mRID of the DERControl being acknowledged.
	Subject string `xml:"subject,omitempty"`
}

// ResponseSet groups Response resources for a single DERProgram.
// The server advertises the ResponseSet endpoint via ResponseSetListLink
// in DeviceCapability.
type ResponseSet struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns ResponseSet"`
	Resource

	MRID         string    `xml:"mRID,omitempty"`
	ResponseList *ListLink `xml:"ResponseListLink,omitempty"`
}

// ResponseSetList is a collection of ResponseSet resources.
type ResponseSetList struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns ResponseSetList"`
	Resource

	All         uint32        `xml:"all,attr"`
	Results     uint32        `xml:"results,attr"`
	ResponseSet []ResponseSet `xml:"ResponseSet"`
}

// ResponseList is a collection of Response resources within a ResponseSet.
type ResponseList struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns ResponseList"`
	Resource

	All      uint32     `xml:"all,attr"`
	Results  uint32     `xml:"results,attr"`
	Response []Response `xml:"Response"`
}

// ───────────────────────────────────────────────────────────────────────
// DERStatus, DERCapability, DERSettings (monitoring/reporting)
// ───────────────────────────────────────────────────────────────────────

// DERCapability describes the nameplate capabilities of a DER.
type DERCapability struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns DERCapability"`
	Resource

	// Type of DER: 0=unknown, 1=virtual, 2=reciprocating engine, 80=PV, 81=wind, 83=storage
	Type             uint8        `xml:"type"`
	RtgMaxW          ActivePower  `xml:"rtgMaxW"`
	RtgMaxVA         *ActivePower `xml:"rtgMaxVA,omitempty"`
	RtgMaxVar        *ActivePower `xml:"rtgMaxVar,omitempty"`
	RtgMaxChargeW    *ActivePower `xml:"rtgMaxChargeRateW,omitempty"`
	RtgMaxDischargeW *ActivePower `xml:"rtgMaxDischargeRateW,omitempty"`
}

// DERSettings contains the current operational settings of a DER.
type DERSettings struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns DERSettings"`
	Resource

	SetMaxW     *ActivePower `xml:"setMaxW,omitempty"`
	SetMaxVA    *ActivePower `xml:"setMaxVA,omitempty"`
	UpdatedTime int64        `xml:"updatedTime,omitempty"`
}

// DERStatus contains the current operational status of a DER.
type DERStatus struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns DERStatus"`
	Resource

	GenConnectStatus      *uint8 `xml:"genConnectStatus>value,omitempty"`
	OperationalModeStatus *uint8 `xml:"operationalModeStatus>value,omitempty"`
	ReadingTime           int64  `xml:"readingTime,omitempty"`
}
