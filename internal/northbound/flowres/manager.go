// Package flowres implements the client side of the IEEE 2030.5 Flow
// Reservation function set (§10.9). It receives FlowReservationRequest
// messages from the hub via MQTT and POSTs them to the utility server,
// tracking the path to use for each end device.
//
// Extracted from cmd/northbound/main.go (TASK-068, D12/R5) as a pure move —
// no behavior change from the original flowReservationManager.
package flowres

import (
	"encoding/json"
	"encoding/xml"
	"log"
	"sync"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/northbound/egress"
	model "lexa-proto/csipmodel"
)

// Poster is the subset of tlsclient.WolfSSLFetcher the Manager needs.
// Narrowing it to an interface (defined here, at the point of consumption —
// 05 §2) keeps HandleRequest unit testable without a live TLS session.
// *tlsclient.WolfSSLFetcher satisfies it.
type Poster interface {
	Post(path string, body []byte, contentType string) ([]byte, string, error)
}

// Manager handles the client side of the Flow Reservation function set
// (IEEE 2030.5 §10.9). It receives FlowReservationRequest messages from the
// hub via MQTT and POSTs them to the utility server, tracking the path to
// use for each end device.
type Manager struct {
	mu          sync.RWMutex
	fetcher     Poster
	lfdi        string
	requestPath string // EndDevice FlowReservationRequestListLink.Href; guarded by mu

	// gate is the optional WP-7/D4 egress gate: when suspended (the
	// registration-PIN freeze), HandleRequest transmits nothing. nil (the
	// default) never suspends — egress.Gate's nil convention.
	gate *egress.Gate
}

// New constructs a Manager that POSTs via f, identifying as lfdi.
func New(f Poster, lfdi string) *Manager {
	return &Manager{fetcher: f, lfdi: lfdi}
}

// SetEgressGate wires the WP-7/D4 egress gate. Call at wiring time, before
// the MQTT subscription that drives HandleRequest starts.
func (m *Manager) SetEgressGate(g *egress.Gate) {
	m.mu.Lock()
	m.gate = g
	m.mu.Unlock()
}

// SetRequestPath updates the server path to POST new requests to. Called
// after each discovery walk with the path from the EndDevice resource.
func (m *Manager) SetRequestPath(path string) {
	m.mu.Lock()
	m.requestPath = path
	m.mu.Unlock()
}

// HandleRequest is the MQTT message handler for bus.TopicCSIPFRRequest. It
// decodes the hub's FlowReservationRequestMsg and POSTs a corresponding
// FlowReservationRequest to the utility server.
func (m *Manager) HandleRequest(payload []byte) {
	var msg bus.FlowReservationRequestMsg
	if err := json.Unmarshal(payload, &msg); err != nil {
		log.Printf("lexa-northbound: flowreservation decode: %v", err)
		return
	}
	m.mu.RLock()
	requestPath := m.requestPath
	gate := m.gate
	m.mu.RUnlock()
	if gate.Suspended() {
		// WP-7 (D4): server egress suspended (registration-PIN freeze).
		// Rare — hub-driven requests only — so this is an edge, not spam.
		log.Printf("lexa-northbound: flowreservation POST suppressed — server egress suspended (%s)", gate.Reason())
		return
	}
	if requestPath == "" {
		log.Printf("lexa-northbound: flowreservation: no request path yet — server may not support FR")
		return
	}

	req := model.FlowReservationRequest{
		MRID:              msg.MRID,
		Description:       msg.Description,
		CreationTime:      msg.Ts,
		DurationRequested: msg.DurationRequested,
		EnergyRequested: &model.UnitValue{
			Multiplier: 0,
			Value:      int64(derefF64(msg.EnergyRequestedWh)),
		},
		IntervalRequested: model.DateTimeInterval{
			Start:    msg.IntervalStart,
			Duration: msg.IntervalDuration,
		},
		PowerRequested: &model.UnitValue{
			Multiplier: 0,
			Value:      int64(derefF64(msg.PowerRequestedW)),
		},
		RequestStatus: model.RequestStatus{
			DateTime:      msg.Ts,
			RequestStatus: model.FlowReqStatusRequested,
		},
	}

	body, err := xml.Marshal(&req)
	if err != nil {
		log.Printf("lexa-northbound: flowreservation marshal: %v", err)
		return
	}
	_, location, err := m.fetcher.Post(requestPath, body, "application/sep+xml")
	if err != nil {
		log.Printf("lexa-northbound: flowreservation POST %s: %v", requestPath, err)
		return
	}
	log.Printf("lexa-northbound: flowreservation POSTed mrid=%s location=%s", msg.MRID, location)
}

func derefF64(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}
