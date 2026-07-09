package main

// scan.go implements DEVICE_ROADMAP.md §4.3's scan routes: POST /scan
// publishes a commissioning ScanRequest for lexa-modbus to honor (or refuse,
// per §5.2's arming rule — this file has no opinion on that, it only
// publishes the request); GET /scan projects the ScanStatus progress ring
// plus the latest ScanResult.
import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/mqttutil"
)

// scanMaxBodyBytes bounds POST /scan's (optional) request body.
const scanMaxBodyBytes = 64 << 10

// scanHandler dispatches GET and POST /scan to their own handlers with their
// own auth policy — a read (requireBearer, staged-rollout empty-token-open)
// for GET, a write (requireBearerStrict, empty-token-closed) for POST. Both
// live at the same path, so this is the one place that decides which
// wrapper applies, based on method.
func scanHandler(mc mqtt.Client, store *stateStore, apiToken string) http.HandlerFunc {
	get := requireBearer(apiToken, scanGetHandler(store))
	post := requireBearerStrict(apiToken, scanPostHandler(mc))
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		switch r.Method {
		case http.MethodOptions:
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.WriteHeader(http.StatusNoContent)
		case http.MethodGet:
			get(w, r)
		case http.MethodPost:
			post(w, r)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

// scanStatusResp is one ScanStatus progress line's HTTP shape.
type scanStatusResp struct {
	ID     string `json:"id"`
	Phase  string `json:"phase"`
	Probed int    `json:"probed"`
	Found  int    `json:"found"`
	Detail string `json:"detail,omitempty"`
	Ts     string `json:"ts"` // RFC3339
}

// scanGetResp is GET /scan's JSON shape: the progress ring (oldest first)
// plus the latest completed result, if any.
type scanGetResp struct {
	Status []scanStatusResp `json:"status"`
	Result *scanResultResp  `json:"result,omitempty"`
}

func scanGetHandler(store *stateStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snap := store.snapshot()
		resp := scanGetResp{Status: []scanStatusResp{}}
		for _, st := range snap.scanStatusRing {
			resp.Status = append(resp.Status, scanStatusResp{
				ID: st.ID, Phase: st.Phase, Probed: st.Probed, Found: st.Found,
				Detail: st.Detail, Ts: time.Unix(st.Ts, 0).UTC().Format(time.RFC3339),
			})
		}
		if snap.scanResult != nil {
			resp.Result = scanResultRespFrom(snap.scanResult)
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// scanPostBody is the (entirely optional) request body for POST /scan —
// every field mirrors bus.ScanRequest's own optional fields; an empty/absent
// body publishes a ScanRequest with every field at its default (lexa-modbus's
// own "empty = local /24, default bauds/unit IDs" fallback rules apply).
type scanPostBody struct {
	TCPCidr string  `json:"tcp_cidr,omitempty"`
	TCPPort int     `json:"tcp_port,omitempty"`
	RTUDev  string  `json:"rtu_dev,omitempty"`
	Bauds   []int   `json:"bauds,omitempty"`
	UnitIDs []uint8 `json:"unit_ids,omitempty"`
}

func scanPostHandler(mc mqtt.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body scanPostBody
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, scanMaxBodyBytes))
		if err := dec.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			var mbErr *http.MaxBytesError
			if errors.As(err, &mbErr) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}

		req := bus.ScanRequest{
			Envelope: bus.Envelope{V: bus.ScanRequestV},
			ID:       newRandomID(),
			TCPCidr:  body.TCPCidr,
			TCPPort:  body.TCPPort,
			RTUDev:   body.RTUDev,
			Bauds:    body.Bauds,
			UnitIDs:  body.UnitIDs,
			Ts:       time.Now().Unix(),
		}
		if err := mqttutil.PublishJSONQoS(mc, bus.TopicScanRequest, bus.PubQoS(bus.TopicScanRequest), req); err != nil {
			slog.Error("lexa-api: scan publish failed", "route", "/scan", "id", req.ID, "err", err)
			http.Error(w, "bus publish failed", http.StatusBadGateway)
			return
		}
		slog.Info("lexa-api: scan requested", "route", "/scan", "id", req.ID, "outcome", "published")
		writeJSON(w, http.StatusAccepted, map[string]string{"id": req.ID})
	}
}
