package discovery

import (
	"context"

	model "lexa-proto/csipmodel"
)

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
//
// Not touched by TASK-070 (ctx propagation walker-deep): this Poster is
// unrelated to the Fetcher.Get path the walk loop drives, and no production
// code in this repo implements/consumes it today (internal/northbound/flowres
// and internal/northbound/responses each define their own local Poster
// instead — see those packages).
type Poster interface {
	Post(path string, body []byte, contentType string) ([]byte, string, error)
}

// fetchFlowReservationResponseList GETs the list of existing
// FlowReservationResponses from the server's EndDevice resource.
func (w *Walker) fetchFlowReservationResponseList(ctx context.Context, path string) (*model.FlowReservationResponseList, error) {
	var r model.FlowReservationResponseList
	return &r, w.fetchAndParse(ctx, path, &r)
}

// fetchFlowReservationRequestList GETs the list of existing
// FlowReservationRequests (used to check what's already been submitted).
func (w *Walker) fetchFlowReservationRequestList(ctx context.Context, path string) (*model.FlowReservationRequestList, error) {
	var r model.FlowReservationRequestList
	return &r, w.fetchAndParse(ctx, path, &r)
}

// discoverFlowReservation fetches the FlowReservationResponseList from the
// EndDevice. Returns nil if the device has no FlowReservationResponseListLink
// or on fetch error (non-fatal).
func (w *Walker) discoverFlowReservation(ctx context.Context, edev *model.EndDevice) *model.FlowReservationResponseList {
	if edev.FlowReservationResponseListLink == nil {
		return nil
	}
	frpl, err := w.fetchFlowReservationResponseList(ctx, edev.FlowReservationResponseListLink.Href)
	if err != nil {
		w.logf("flowreservation: fetch FlowReservationResponseList %s: %v",
			edev.FlowReservationResponseListLink.Href, err)
		return nil
	}
	return frpl
}
