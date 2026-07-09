package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lexa-hub/internal/bus"
)

// TestModeHandler_503BeforeFirstModeStatus pins the "unknown" state: no
// ModeStatus has arrived yet.
func TestModeHandler_503BeforeFirstModeStatus(t *testing.T) {
	store := newStateStore(nil, time.Minute)
	h := modeHandler(store)
	req := httptest.NewRequest(http.MethodGet, "/mode", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["error"] != "unknown" {
		t.Errorf(`body = %v, want {"error":"unknown"}`, got)
	}
}

// TestModeHandler_200AfterModeStatusArrives pins the success path once the
// retained lexa/hub/mode has been seen.
func TestModeHandler_200AfterModeStatusArrives(t *testing.T) {
	store := newStateStore(nil, time.Minute)
	store.onModeStatus(bus.TopicHubMode, bus.ModeStatus{
		Mode: "gateway", Since: 1000, Actor: "installer@example.com", IntentID: "abc123",
	})

	h := modeHandler(store)
	req := httptest.NewRequest(http.MethodGet, "/mode", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got modeResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Mode != "gateway" || got.Since != 1000 || got.Actor != "installer@example.com" || got.IntentID != "abc123" {
		t.Errorf("modeResp = %+v, want the latest ModeStatus projected verbatim", got)
	}
}

// TestModeHandler_CORSPreflight pins the shared OPTIONS convention.
func TestModeHandler_CORSPreflight(t *testing.T) {
	store := newStateStore(nil, time.Minute)
	h := modeHandler(store)
	req := httptest.NewRequest(http.MethodOptions, "/mode", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS: status = %d, want 204", rec.Code)
	}
}
