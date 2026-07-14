package main

// pairing_test.go: WP-13 POST /devices/evse/{id}/pairing route tests — the
// publish path (payload/topic/QoS/non-retained), validation rejections, and
// the write-route auth posture. The contract drift gate for the documented
// request/response fixtures lives in contract_test.go (TestContract_Pairing).
import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lexa-hub/internal/bus"
)

func doPairing(t *testing.T, fc *fakeAPIMQTTClient, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := pairingHandler(fc)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

// TestPairing_ApprovePublishesDecision pins the full happy path: 202, one
// QoS-1 non-retained publish on lexa/ocpp/pairing whose payload carries the
// station ID from the URL, the action, the stamped envelope version, and the
// local-api actor.
func TestPairing_ApprovePublishesDecision(t *testing.T) {
	fc := &fakeAPIMQTTClient{}
	rec := doPairing(t, fc, http.MethodPost, "/devices/evse/ev-unknown/pairing", `{"action":"approve"}`)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if len(fc.publishes) != 1 {
		t.Fatalf("expected 1 publish, got %d", len(fc.publishes))
	}
	pub := fc.publishes[0]
	if pub.topic != bus.TopicOCPPPairing || pub.retained || pub.qos != bus.QoS1 {
		t.Errorf("publish topic=%q retained=%v qos=%d, want %q/false/1", pub.topic, pub.retained, pub.qos, bus.TopicOCPPPairing)
	}
	var d bus.PairingDecision
	if err := json.Unmarshal(pub.payload, &d); err != nil {
		t.Fatalf("decode published decision: %v", err)
	}
	if d.StationID != "ev-unknown" || d.Action != bus.PairingActionApprove || d.V != bus.PairingDecisionV || d.Actor != "local-api" || d.Ts == 0 {
		t.Errorf("published decision = %+v, want station ev-unknown, action approve, v=%d, actor local-api, ts set", d, bus.PairingDecisionV)
	}
}

// TestPairing_DenyPublishesDecision pins the deny action end-to-end.
func TestPairing_DenyPublishesDecision(t *testing.T) {
	fc := &fakeAPIMQTTClient{}
	rec := doPairing(t, fc, http.MethodPost, "/devices/evse/cs-x/pairing", `{"action":"deny"}`)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	var d bus.PairingDecision
	if err := json.Unmarshal(fc.publishes[0].payload, &d); err != nil {
		t.Fatalf("decode published decision: %v", err)
	}
	if d.StationID != "cs-x" || d.Action != bus.PairingActionDeny {
		t.Errorf("published decision = %+v, want cs-x/deny", d)
	}
}

// TestPairing_Rejections pins every validation rejection: unknown action,
// missing action, malformed JSON, malformed paths, and non-POST methods —
// all without a single bus publish.
func TestPairing_Rejections(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   string
		want   int
	}{
		{"unknown action", http.MethodPost, "/devices/evse/cs-1/pairing", `{"action":"maybe"}`, http.StatusBadRequest},
		{"missing action", http.MethodPost, "/devices/evse/cs-1/pairing", `{}`, http.StatusBadRequest},
		{"malformed json", http.MethodPost, "/devices/evse/cs-1/pairing", `{`, http.StatusBadRequest},
		{"empty station id", http.MethodPost, "/devices/evse//pairing", `{"action":"approve"}`, http.StatusNotFound},
		{"missing suffix", http.MethodPost, "/devices/evse/cs-1", `{"action":"approve"}`, http.StatusNotFound},
		{"extra segment", http.MethodPost, "/devices/evse/cs-1/x/pairing", `{"action":"approve"}`, http.StatusNotFound},
		{"GET", http.MethodGet, "/devices/evse/cs-1/pairing", ``, http.StatusMethodNotAllowed},
		{"PUT", http.MethodPut, "/devices/evse/cs-1/pairing", `{"action":"approve"}`, http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeAPIMQTTClient{}
			rec := doPairing(t, fc, tc.method, tc.path, tc.body)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if len(fc.publishes) != 0 {
				t.Errorf("a rejected request must publish nothing, got %d publishes", len(fc.publishes))
			}
		})
	}
}

// TestPairing_PublishFailureIs502: a broker publish failure surfaces as 502,
// mirroring /intent's bus-publish error contract.
func TestPairing_PublishFailureIs502(t *testing.T) {
	fc := &fakeAPIMQTTClient{failNextPublish: true}
	rec := doPairing(t, fc, http.MethodPost, "/devices/evse/cs-1/pairing", `{"action":"approve"}`)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502: %s", rec.Code, rec.Body.String())
	}
}

// TestPairing_WriteRouteFailsClosed pins the auth posture main.go wires: the
// route is wrapped in requireBearerStrict, so an EMPTY configured token is a
// 401 (write routes never inherit requireBearer's staged-rollout escape
// hatch), and a wrong token is a 401.
func TestPairing_WriteRouteFailsClosed(t *testing.T) {
	fc := &fakeAPIMQTTClient{}
	h := requireBearerStrict("", pairingHandler(fc))
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/devices/evse/cs-1/pairing", strings.NewReader(`{"action":"approve"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("empty token: status = %d, want 401 (fail closed)", rec.Code)
	}

	h = requireBearerStrict("secret", pairingHandler(fc))
	req := httptest.NewRequest(http.MethodPost, "/devices/evse/cs-1/pairing", strings.NewReader(`{"action":"approve"}`))
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d, want 401", rec.Code)
	}
	if len(fc.publishes) != 0 {
		t.Errorf("unauthorized requests must publish nothing, got %d", len(fc.publishes))
	}
}

// TestPairing_StationIDFromPath pins the path parser's shape rules.
func TestPairing_StationIDFromPath(t *testing.T) {
	cases := map[string]string{
		"/devices/evse/cs-001/pairing":  "cs-001",
		"/devices/evse/ev%20x/pairing":  "ev%20x", // opaque segment, passed through
		"/devices/evse//pairing":        "",
		"/devices/evse/pairing":         "", // no id segment at all
		"/devices/evse/a/b/pairing":     "",
		"/devices/evse/cs-001":          "",
		"/devices/cs-001/pairing":       "",
		"/devices/evse/cs-001/pairingX": "",
	}
	for path, want := range cases {
		if got := stationIDFromPairingPath(path); got != want {
			t.Errorf("stationIDFromPairingPath(%q) = %q, want %q", path, got, want)
		}
	}
}
