package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestStatusHandler_APICertFP pins the additive "api_cert_fp" /status field
// (DEVICE_ROADMAP.md §4.1/§4.6): present verbatim when TLS is on, and
// omitted entirely (not just empty-string) when it's off — existing
// consumers that don't know the field must see it simply absent, per
// omitempty, exactly like cert_status's existing precedent.
func TestStatusHandler_APICertFP_PresentWhenSet(t *testing.T) {
	store := newStateStore(nil, time.Minute)
	hb := newPlanHeartbeat(75 * time.Second)

	h := statusHandler(store, hb, "deadbeef00112233")
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
	got, _ := resp["api_cert_fp"].(string)
	if got != "deadbeef00112233" {
		t.Fatalf("api_cert_fp = %q, want %q", got, "deadbeef00112233")
	}
}

func TestStatusHandler_APICertFP_OmittedWhenTLSOff(t *testing.T) {
	store := newStateStore(nil, time.Minute)
	hb := newPlanHeartbeat(75 * time.Second)

	h := statusHandler(store, hb, "")
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if strings.Contains(rec.Body.String(), "api_cert_fp") {
		t.Fatalf("/status body contains api_cert_fp with an empty fingerprint (want omitempty to drop it): %s", rec.Body.String())
	}
}
