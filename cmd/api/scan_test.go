package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lexa-hub/internal/bus"
)

// TestScanPostHandler_PublishesScanRequest pins POST /scan's publish
// shape: QoS 1 (bus.PubQoS's default arm), NOT retained, a stamped
// crypto/rand ID, and every optional body field carried through.
func TestScanPostHandler_PublishesScanRequest(t *testing.T) {
	fc := &fakeAPIMQTTClient{}
	h := scanPostHandler(fc)

	body := `{"tcp_cidr":"192.168.1.0/24","tcp_port":502,"bauds":[9600,19200],"unit_ids":[1,2,3]}`
	req := httptest.NewRequest(http.MethodPost, "/scan", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if len(fc.publishes) != 1 {
		t.Fatalf("expected exactly 1 publish, got %d", len(fc.publishes))
	}
	pub := fc.publishes[0]
	if pub.topic != bus.TopicScanRequest {
		t.Errorf("topic = %q, want %q", pub.topic, bus.TopicScanRequest)
	}
	if pub.retained {
		t.Error("scan request must NOT be retained")
	}
	if pub.qos != 1 {
		t.Errorf("qos = %d, want 1", pub.qos)
	}

	var got bus.ScanRequest
	if err := json.Unmarshal(pub.payload, &got); err != nil {
		t.Fatalf("decode published payload: %v", err)
	}
	if got.ID == "" {
		t.Error("ScanRequest.ID is empty, want a stamped random ID")
	}
	if got.TCPCidr != "192.168.1.0/24" || got.TCPPort != 502 {
		t.Errorf("TCPCidr/TCPPort not carried through: %+v", got)
	}
	if len(got.Bauds) != 2 || len(got.UnitIDs) != 3 {
		t.Errorf("Bauds/UnitIDs not carried through: %+v", got)
	}

	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["id"] != got.ID {
		t.Errorf("response id = %q, want the published ScanRequest.ID %q", resp["id"], got.ID)
	}
}

// TestScanPostHandler_EmptyBodyUsesDefaults pins that an entirely absent
// body (the common case: "just scan the local subnet") is accepted, not a
// 400 — every ScanRequest field simply stays at its zero value.
func TestScanPostHandler_EmptyBodyUsesDefaults(t *testing.T) {
	fc := &fakeAPIMQTTClient{}
	h := scanPostHandler(fc)

	req := httptest.NewRequest(http.MethodPost, "/scan", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 for an empty body: %s", rec.Code, rec.Body.String())
	}
	if len(fc.publishes) != 1 {
		t.Fatalf("expected exactly 1 publish, got %d", len(fc.publishes))
	}
}

// TestScanPostHandler_OversizeBodyRejected pins the 64KiB MaxBytesReader bound.
func TestScanPostHandler_OversizeBodyRejected(t *testing.T) {
	fc := &fakeAPIMQTTClient{}
	h := scanPostHandler(fc)

	huge := strings.Repeat("a", scanMaxBodyBytes+1024)
	req := httptest.NewRequest(http.MethodPost, "/scan", strings.NewReader(`{"tcp_cidr":"`+huge+`"}`))
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 for an oversize body", rec.Code)
	}
	if len(fc.publishes) != 0 {
		t.Fatalf("expected no publish for an oversize body, got %d", len(fc.publishes))
	}
}

// TestScanPostHandler_PublishFailureReturns502 pins the publish-error path.
func TestScanPostHandler_PublishFailureReturns502(t *testing.T) {
	fc := &fakeAPIMQTTClient{failNextPublish: true}
	h := scanPostHandler(fc)

	req := httptest.NewRequest(http.MethodPost, "/scan", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 on publish failure: %s", rec.Code, rec.Body.String())
	}
}

// TestScanGetHandler_ProjectsStatusRingAndResult pins GET /scan's shape.
func TestScanGetHandler_ProjectsStatusRingAndResult(t *testing.T) {
	store := newStateStore(nil, time.Minute)
	store.onScanStatus(bus.TopicScanStatus, bus.ScanStatus{ID: "s1", Phase: "tcp", Probed: 5, Found: 1, Ts: 100})
	store.onScanStatus(bus.TopicScanStatus, bus.ScanStatus{ID: "s1", Phase: "done", Probed: 10, Found: 2, Ts: 101})
	store.onScanResult(bus.TopicScanResult, bus.ScanResult{
		ID: "s1",
		Devices: []bus.ScanHit{
			{URL: "tcp://192.168.1.40:502", UnitID: 1, Class: "inverter"},
		},
		Ts: 101,
	})

	h := scanGetHandler(store)
	req := httptest.NewRequest(http.MethodGet, "/scan", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got scanGetResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.Status) != 2 {
		t.Fatalf("status ring len = %d, want 2", len(got.Status))
	}
	if got.Status[0].Phase != "tcp" || got.Status[1].Phase != "done" {
		t.Errorf("status ring not oldest-first: %+v", got.Status)
	}
	if got.Result == nil || got.Result.ID != "s1" || len(got.Result.Devices) != 1 {
		t.Fatalf("result not projected correctly: %+v", got.Result)
	}
	if got.Result.Devices[0].Class != "inverter" {
		t.Errorf("scan hit class = %q, want inverter", got.Result.Devices[0].Class)
	}
}

// TestScanHandler_MethodDispatchAndAuth pins the shared-path auth split:
// GET uses requireBearer (empty token = open), POST uses requireBearerStrict
// (empty token = always 401).
func TestScanHandler_MethodDispatchAndAuth(t *testing.T) {
	store := newStateStore(nil, time.Minute)
	fc := &fakeAPIMQTTClient{}
	h := scanHandler(fc, store, "") // no token configured

	t.Run("GET open when no token configured", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/scan", nil)
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /scan with no token configured: status = %d, want 200", rec.Code)
		}
	})

	t.Run("POST always 401 when no token configured", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/scan", nil)
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("POST /scan with no token configured: status = %d, want 401 (strict write auth)", rec.Code)
		}
		if len(fc.publishes) != 0 {
			t.Fatalf("expected no publish when auth denies the request, got %d", len(fc.publishes))
		}
	})

	t.Run("OPTIONS is a CORS preflight, not a method error", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/scan", nil)
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("OPTIONS /scan: status = %d, want 204", rec.Code)
		}
	})

	t.Run("PUT is method not allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPut, "/scan", nil)
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("PUT /scan: status = %d, want 405", rec.Code)
		}
	})
}

// TestScanHandler_POSTWithConfiguredTokenRequiresIt pins that POST /scan
// behaves like every other requireBearerStrict route once a token IS
// configured: correct token passes, wrong/missing token 401s.
func TestScanHandler_POSTWithConfiguredTokenRequiresIt(t *testing.T) {
	store := newStateStore(nil, time.Minute)
	fc := &fakeAPIMQTTClient{}
	h := scanHandler(fc, store, "s3cret")

	req := httptest.NewRequest(http.MethodPost, "/scan", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing header: status = %d, want 401", rec.Code)
	}

	req2 := httptest.NewRequest(http.MethodPost, "/scan", nil)
	req2.Header.Set("Authorization", "Bearer s3cret")
	rec2 := httptest.NewRecorder()
	h(rec2, req2)
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("correct token: status = %d, want 202: %s", rec2.Code, rec2.Body.String())
	}
}
