package csipmodel

import "encoding/xml"

// ───────────────────────────────────────────────────────────────────────
// Flow Reservation function set (IEEE 2030.5 §10.9)
// ───────────────────────────────────────────────────────────────────────
//
// Flow Reservation allows DER clients (PEVs, storage) to request an energy
// transfer window in advance. The client POSTs a FlowReservationRequest to
// the server's EndDevice FlowReservationRequestList; the server responds with
// a FlowReservationResponse that may grant a modified window.
//
// The hub publishes desired reservations to lexa/csip/flowreservation/request.
// The csip service posts them to the server and polls responses, publishing
// current granted windows to lexa/csip/flowreservation/status.

// RequestStatus codes (FlowReservationRequest.RequestStatus.requestStatus).
const (
	FlowReqStatusRequested uint8 = 0 // initial state when POSTed
	FlowReqStatusCancelled uint8 = 1 // client cancelled the request
)

// FlowResponseStatus codes (FlowReservationResponse.EventStatus.currentStatus).
const (
	FlowRespStatusScheduled  uint8 = 0 // server granted, not yet started
	FlowRespStatusActive     uint8 = 1 // interval is currently underway
	FlowRespStatusCancelled  uint8 = 2 // server or client cancelled
	FlowRespStatusSuperseded uint8 = 3 // replaced by a newer response
)

// RequestStatus is embedded in FlowReservationRequest to track the request
// lifecycle. Clients may only change requestStatus (per §10.9.3.1).
type RequestStatus struct {
	DateTime      int64 `xml:"dateTime"`
	RequestStatus uint8 `xml:"requestStatus"`
}

// FlowReservationRequest is POSTed by the client to the server's
// EndDevice FlowReservationRequestList. The server creates a corresponding
// FlowReservationResponse.
type FlowReservationRequest struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns FlowReservationRequest"`
	Resource

	MRID         string `xml:"mRID,omitempty"`
	Description  string `xml:"description,omitempty"`
	CreationTime int64  `xml:"creationTime,omitempty"`

	// DurationRequested is the charging/discharging duration needed (seconds).
	DurationRequested uint32 `xml:"durationRequested,omitempty"`

	// EnergyRequested is the total energy transfer requested (Wh).
	EnergyRequested *UnitValue `xml:"energyRequested,omitempty"`

	// IntervalRequested is the time window within which the transfer should occur.
	IntervalRequested DateTimeInterval `xml:"intervalRequested"`

	// PowerRequested is the desired charge/discharge rate (W).
	PowerRequested *UnitValue `xml:"powerRequested,omitempty"`

	RequestStatus RequestStatus `xml:"RequestStatus"`
}

// FlowReservationRequestList is a collection of FlowReservationRequest resources.
// The server exposes this list under each EndDevice for clients to POST to.
type FlowReservationRequestList struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns FlowReservationRequestList"`
	Resource

	All                    uint32                   `xml:"all,attr"`
	Results                uint32                   `xml:"results,attr"`
	FlowReservationRequest []FlowReservationRequest `xml:"FlowReservationRequest"`
}

// FlowReservationResponse is created by the server in response to each
// FlowReservationRequest. It carries the actual granted interval and power,
// which may differ from the request. The client acts on this interval.
type FlowReservationResponse struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns FlowReservationResponse"`
	Resource

	Subscribable uint8        `xml:"subscribable,attr,omitempty"`
	MRID         string       `xml:"mRID,omitempty"`
	Description  string       `xml:"description,omitempty"`
	CreationTime int64        `xml:"creationTime,omitempty"`
	EventStatus  *EventStatus `xml:"EventStatus,omitempty"`

	// Interval is the server-granted time window for the energy transfer.
	Interval DateTimeInterval `xml:"interval"`

	// EnergyAvailable is the total energy the server will deliver/accept (Wh).
	EnergyAvailable *UnitValue `xml:"energyAvailable,omitempty"`

	// PowerAvailable is the rate at which energy will be transferred (W).
	PowerAvailable *UnitValue `xml:"powerAvailable,omitempty"`

	// Subject is the mRID of the FlowReservationRequest this responds to.
	Subject string `xml:"subject,omitempty"`
}

// FlowReservationResponseList is a collection of FlowReservationResponse resources.
type FlowReservationResponseList struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns FlowReservationResponseList"`
	Resource

	All                     uint32                    `xml:"all,attr"`
	Results                 uint32                    `xml:"results,attr"`
	Subscribable            uint8                     `xml:"subscribable,attr,omitempty"`
	FlowReservationResponse []FlowReservationResponse `xml:"FlowReservationResponse"`
}
