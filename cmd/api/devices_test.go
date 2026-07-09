package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lexa-hub/internal/bus"
)

// TestDevicesHandler_MergesLiveAndDiscoverySurfaces pins GET /devices's JSON
// shape (DEVICE_ROADMAP.md §4.3): the live device/EVSE projection merged
// with the last scan result and any OCPP stations awaiting approval.
func TestDevicesHandler_MergesLiveAndDiscoverySurfaces(t *testing.T) {
	store := newStateStore([]DeviceConfig{
		{Name: "inverter-0", Role: "inverter", MaxW: 5000},
	}, time.Minute)

	w := 1234.0
	store.onMeasurement(bus.MeasurementTopic("inverter-0"), bus.Measurement{Device: "inverter-0", W: &w})

	store.onScanResult(bus.TopicScanResult, bus.ScanResult{
		ID: "scan-1",
		Devices: []bus.ScanHit{
			{URL: "tcp://192.168.1.41:502", UnitID: 2, Manufacturer: "Acme", Class: "unknown-sunspec"},
		},
		Ts: 1000,
	})

	store.onOCPPPending(bus.TopicOCPPPending, bus.PendingStations{
		Stations: []bus.PendingStation{
			{StationID: "ev-unknown", Vendor: "AcmeEV", FirstSeenTs: 2000, RemoteAddr: "10.0.0.9:5000"},
		},
		Ts: 2000,
	})

	h := devicesHandler(store)
	req := httptest.NewRequest(http.MethodGet, "/devices", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got devicesResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	di, ok := got.Devices["inverter-0"]
	if !ok {
		t.Fatalf("devices map missing inverter-0: %+v", got.Devices)
	}
	if di.Role != "inverter" || di.W != 1234.0 || !di.Connected {
		t.Errorf("inverter-0 projection = %+v, want role=inverter W=1234 connected=true", di)
	}

	if got.ScanResult == nil || got.ScanResult.ID != "scan-1" || len(got.ScanResult.Devices) != 1 {
		t.Fatalf("scan_result not projected: %+v", got.ScanResult)
	}
	if got.ScanResult.Devices[0].Class != "unknown-sunspec" {
		t.Errorf("scan hit class = %q, want unknown-sunspec", got.ScanResult.Devices[0].Class)
	}

	if len(got.OCPPPending) != 1 || got.OCPPPending[0].StationID != "ev-unknown" {
		t.Fatalf("ocpp_pending not projected: %+v", got.OCPPPending)
	}
	if got.OCPPPending[0].Vendor != "AcmeEV" {
		t.Errorf("pending vendor = %q, want AcmeEV", got.OCPPPending[0].Vendor)
	}
}

// TestDevicesHandler_OmitsScanAndPendingWhenAbsent pins that /devices does
// not fabricate scan_result/ocpp_pending before anything has ever arrived.
func TestDevicesHandler_OmitsScanAndPendingWhenAbsent(t *testing.T) {
	store := newStateStore(nil, time.Minute)
	h := devicesHandler(store)
	req := httptest.NewRequest(http.MethodGet, "/devices", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, present := got["scan_result"]; present {
		t.Error("scan_result present with no scan ever received, want omitted")
	}
	if _, present := got["ocpp_pending"]; present {
		t.Error("ocpp_pending present with no pending message ever received, want omitted")
	}
}

// TestDevicesHandler_CORSPreflight pins the OPTIONS handling convention
// shared with every other read route in this package.
func TestDevicesHandler_CORSPreflight(t *testing.T) {
	store := newStateStore(nil, time.Minute)
	h := devicesHandler(store)
	req := httptest.NewRequest(http.MethodOptions, "/devices", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS: status = %d, want 204", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Methods") == "" {
		t.Error("expected Access-Control-Allow-Methods header on preflight response")
	}
}
