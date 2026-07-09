package main

// mode.go implements DEVICE_ROADMAP.md §4.3's GET /mode route: a thin
// projection of the hub's latest retained lexa/hub/mode (bus.ModeStatus).
import (
	"net/http"
)

// modeResp is GET /mode's JSON shape on success.
type modeResp struct {
	Mode     string `json:"mode"`
	Since    int64  `json:"since"`
	Actor    string `json:"actor,omitempty"`
	IntentID string `json:"intent_id,omitempty"`
}

// modeHandler serves GET /mode: 503 {"error":"unknown"} until the first
// ModeStatus has arrived (a retained topic means this is normally the case
// within one broker round trip of either side's startup — see
// stateStore.modeStatus's doc), 200 with the latest value otherwise.
func modeHandler(store *stateStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.WriteHeader(http.StatusNoContent)
			return
		}

		snap := store.snapshot()
		if snap.modeStatus == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unknown"})
			return
		}
		resp := modeResp{
			Mode:     snap.modeStatus.Mode,
			Since:    snap.modeStatus.Since,
			Actor:    snap.modeStatus.Actor,
			IntentID: snap.modeStatus.IntentID,
		}
		writeJSON(w, http.StatusOK, resp)
	}
}
