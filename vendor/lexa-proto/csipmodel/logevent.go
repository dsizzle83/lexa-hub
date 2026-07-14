// This file extends the csipmodel package (see resources.go for the package
// doc comment and the XML-namespace invariant) with the IEEE 2030.5 Log Event
// function set (§11.4): the LogEvent resource a client POSTs to its
// EndDevice's LogEventListLink to report locally generated events — DER
// alarms and their return-to-normal pairs in the CSIP profile (BASIC-027).
package csipmodel

import "encoding/xml"

// LogEvent function-set identifiers (functionSet element). Table 24 of IEEE
// 2030.5-2018 assigns 11 to the DER function set — the value CSIP DER alarm
// LogEvents carry.
const (
	LogFunctionSetDER uint8 = 11
)

// LogEvent is a single locally generated event record. The client POSTs one
// per event occurrence (an alarm and its return-to-normal are two separate
// LogEvents) to the server's LogEventList for this EndDevice.
type LogEvent struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns LogEvent"`
	Resource

	// CreatedDateTime is when the event was generated (Unix seconds).
	CreatedDateTime int64 `xml:"createdDateTime"`

	// ExtendedData carries optional manufacturer-defined detail for the event.
	ExtendedData *uint32 `xml:"extendedData,omitempty"`

	// FunctionSet identifies the function set the event belongs to
	// (LogFunctionSetDER for DER alarms).
	FunctionSet uint8 `xml:"functionSet"`

	// LogEventCode is the event code within the function set — for
	// functionSet 11 these are the CSIP Table 14 DER alarm/RTN codes.
	LogEventCode uint8 `xml:"logEventCode"`

	// LogEventID disambiguates events created in the same second; unique per
	// (functionSet, logEventPEN, createdDateTime).
	LogEventID uint16 `xml:"logEventID"`

	// LogEventPEN is the IANA Private Enterprise Number of the entity that
	// defined the logEventCode space.
	LogEventPEN uint32 `xml:"logEventPEN"`

	// ProfileID identifies the profile the event was generated under
	// (0=default IEEE 2030.5, 2=IEEE 2030.5 CSIP).
	ProfileID uint8 `xml:"profileID"`
}

// LogEventList is a collection of LogEvent resources for one EndDevice.
type LogEventList struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns LogEventList"`
	Resource

	All      uint32     `xml:"all,attr"`
	Results  uint32     `xml:"results,attr"`
	PollRate uint32     `xml:"pollRate,attr,omitempty"`
	LogEvent []LogEvent `xml:"LogEvent"`
}
