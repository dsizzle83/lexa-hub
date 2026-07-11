package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lexa-hub/internal/buildinfo"
)

// TestStatusHandler_FWReflectsBuildinfoVersion pins the additive "fw"
// /status field (GAP-5): statusHandler stamps buildinfo.Version onto every
// response, the same process-static pattern api_cert_fp already uses, so a
// real -ldflags -X build stamp is visible from /status too, not just /site
// and the mDNS TXT record.
func TestStatusHandler_FWReflectsBuildinfoVersion(t *testing.T) {
	orig := buildinfo.Version
	defer func() { buildinfo.Version = orig }()
	buildinfo.Version = "1.2.3-test"

	store := newStateStore(nil, time.Minute)
	hb := newPlanHeartbeat(75 * time.Second)

	h := statusHandler(store, hb, "")
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode /status: %v", err)
	}
	got, _ := resp["fw"].(string)
	if got != "1.2.3-test" {
		t.Fatalf("fw = %q, want %q", got, "1.2.3-test")
	}
}
