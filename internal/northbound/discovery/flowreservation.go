package discovery

import model "lexa-proto/csipmodel"

// ───────────────────────────────────────────────────────────────────────
// Flow Reservation function set discovery
// ───────────────────────────────────────────────────────────────────────

// Poster is the interface for HTTP POST operations. Implemented by
// tlsclient.WolfSSLFetcher. Accepted by FlowReservationPoster so the
// posting logic stays decoupled from TLS details.
//
// Note: the Walker only does GET-based discovery; POSTing new
// FlowReservationRequests is done directly in cmd/csip/main.go using
// the same WolfSSLFetcher that the walker uses for GETs.
type Poster interface {
	Post(path string, body []byte, contentType string) ([]byte, string, error)
}

// fetchFlowReservationResponseList GETs the list of existing
// FlowReservationResponses from the server's EndDevice resource.
func (w *Walker) fetchFlowReservationResponseList(path string) (*model.FlowReservationResponseList, error) {
	var r model.FlowReservationResponseList
	return &r, w.fetchAndParse(path, &r)
}

// fetchFlowReservationRequestList GETs the list of existing
// FlowReservationRequests (used to check what's already been submitted).
func (w *Walker) fetchFlowReservationRequestList(path string) (*model.FlowReservationRequestList, error) {
	var r model.FlowReservationRequestList
	return &r, w.fetchAndParse(path, &r)
}

// discoverFlowReservation fetches the FlowReservationResponseList from the
// EndDevice. Returns nil if the device has no FlowReservationResponseListLink
// or on fetch error (non-fatal).
func (w *Walker) discoverFlowReservation(edev *model.EndDevice) *model.FlowReservationResponseList {
	if edev.FlowReservationResponseListLink == nil {
		return nil
	}
	frpl, err := w.fetchFlowReservationResponseList(edev.FlowReservationResponseListLink.Href)
	if err != nil {
		w.logf("flowreservation: fetch FlowReservationResponseList %s: %v",
			edev.FlowReservationResponseListLink.Href, err)
		return nil
	}
	return frpl
}
