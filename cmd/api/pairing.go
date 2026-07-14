package main

// pairing.go implements WP-13's POST /devices/evse/{id}/pairing route (D10):
// validate {action: approve|deny} → publish one bus.PairingDecision edge on
// lexa/ocpp/pairing (QoS 1, NOT retained — a retained decision would replay
// as a false edge; durability is lexa-ocpp's persisted allowlist) → 202
// Accepted. The effect is asynchronous by design: lexa-ocpp applies and
// persists the decision, and the station's next BootNotification (nudged via
// TriggerMessage where still connected) is answered Accepted/Rejected. There
// is no result topic to await (unlike POST /intent), so 202 is the honest
// terminal status here.
//
// This is a WRITE route: main.go wraps it in requireBearerStrict, exactly
// like POST /intent — an empty/unconfigured token fails CLOSED.
import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/mqttutil"
)

// pairingMaxBodyBytes bounds the request body — the body is a single small
// action object, so 4 KiB is generous.
const pairingMaxBodyBytes = 4 << 10

// pairingPathPrefix/Suffix frame the {id} segment of the route. Registered
// as the "/devices/evse/" subtree on the mux (the same classic-ServeMux
// style as "/config/"); this handler parses the station ID itself.
const (
	pairingPathPrefix = "/devices/evse/"
	pairingPathSuffix = "/pairing"
)

// stationIDFromPairingPath extracts the {id} from
// /devices/evse/{id}/pairing, or "" when the path does not match the route
// shape (missing/empty id, extra segments, wrong suffix).
func stationIDFromPairingPath(path string) string {
	rest, ok := strings.CutPrefix(path, pairingPathPrefix)
	if !ok {
		return ""
	}
	// The suffix must be present in what REMAINS after the prefix — checking
	// it on the full path would let the two overlap ("/devices/evse/pairing"
	// must not parse as station "pairing").
	id, ok := strings.CutSuffix(rest, pairingPathSuffix)
	if !ok || id == "" || strings.Contains(id, "/") {
		return ""
	}
	return id
}

// pairingHandler serves POST /devices/evse/{id}/pairing. Callers wrap this
// in requireBearerStrict (main.go) — this function assumes it has already
// been authorized.
func pairingHandler(mc mqtt.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		stationID := stationIDFromPairingPath(r.URL.Path)
		if stationID == "" {
			http.NotFound(w, r)
			return
		}

		var req struct {
			Action string `json:"action"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, pairingMaxBodyBytes))
		if err := dec.Decode(&req); err != nil {
			var mbErr *http.MaxBytesError
			if errors.As(err, &mbErr) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.Action != bus.PairingActionApprove && req.Action != bus.PairingActionDeny {
			http.Error(w, fmt.Sprintf("unknown action %q (want %q or %q)", req.Action, bus.PairingActionApprove, bus.PairingActionDeny), http.StatusBadRequest)
			return
		}

		msg := bus.PairingDecision{
			Envelope:  bus.Envelope{V: bus.PairingDecisionV},
			StationID: stationID,
			Action:    req.Action,
			Actor:     "local-api",
			Ts:        time.Now().Unix(),
		}
		// EDGE publish: QoS 1 (bus.PubQoS), never retained — see
		// bus.TopicOCPPPairing's doc.
		if err := mqttutil.PublishJSONQoS(mc, bus.TopicOCPPPairing, bus.PubQoS(bus.TopicOCPPPairing), msg); err != nil {
			slog.Error("lexa-api: pairing publish failed", "route", "/devices/evse/{id}/pairing", "station", stationID, "action", req.Action, "err", err)
			http.Error(w, "bus publish failed", http.StatusBadGateway)
			return
		}
		slog.Info("lexa-api: pairing decision published", "route", "/devices/evse/{id}/pairing", "station", stationID, "action", req.Action)
		writeJSON(w, http.StatusAccepted, map[string]string{
			"station_id": stationID,
			"action":     req.Action,
			"outcome":    "accepted",
		})
	}
}
