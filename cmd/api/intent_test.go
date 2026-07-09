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

// TestIntentHandler_WhitelistTable covers the kind whitelist (DEVICE_ROADMAP.md
// §1.4/§4.3): an unknown kind and each cloud-only kind must 400 before ever
// touching the bus, while every locally-accepted kind's happy path reaches
// the publish call (verified per-kind further down).
func TestIntentHandler_WhitelistTable(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"unknown kind", `{"kind":"bogus","body":{}}`, http.StatusBadRequest},
		{"cloud-only solarforecast", `{"kind":"solarforecast","body":{}}`, http.StatusBadRequest},
		{"cloud-only loadprofile", `{"kind":"loadprofile","body":{}}`, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fc := &fakeAPIMQTTClient{}
			waiter, err := newResultWaiter(fc)
			if err != nil {
				t.Fatalf("newResultWaiter: %v", err)
			}
			h := intentHandler(fc, waiter)

			req := httptest.NewRequest(http.MethodPost, "/intent", strings.NewReader(c.body))
			rec := httptest.NewRecorder()
			h(rec, req)

			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, c.want, rec.Body.String())
			}
			if len(fc.publishes) != 0 {
				t.Fatalf("expected no publish for a rejected kind, got %d", len(fc.publishes))
			}
		})
	}
}

// TestIntentHandler_PerKindPublish pins each locally-accepted kind's publish
// topic, retained flag, and stamped IntentMeta (DEVICE_ROADMAP.md §4.3: "Per
// kind: decode into the bus type, stamp IntentMeta ... publish: retained for
// mode/evgoal/reserve/tariff, non-retained QoS1 for chargenow").
func TestIntentHandler_PerKindPublish(t *testing.T) {
	cases := []struct {
		kind         string
		body         string
		wantTopic    string
		wantRetained bool
		wantQoS      byte
	}{
		{"mode", `{"mode":"gateway"}`, bus.TopicIntentMode, true, 1},
		{"evgoal", `{"target_soc_kwh":40,"departure_unix":1999999999}`, bus.TopicIntentEVGoal, true, 1},
		{"reserve", `{"reserve_pct":30}`, bus.TopicIntentReserve, true, 1},
		{"tariff", `{"tariff":{"currency":"USD","periods":[{"label":"peak","import_per_kwh":0.3}]}}`, bus.TopicIntentTariff, true, 1},
		{"chargenow", `{"station_id":"ev-1","ttl_s":900}`, bus.TopicIntentChargeNow, false, 1},
	}

	orig := intentResultWait
	intentResultWait = 30 * time.Millisecond // shrink so the timeout branch (no reply configured) resolves fast
	defer func() { intentResultWait = orig }()

	for _, c := range cases {
		t.Run(c.kind, func(t *testing.T) {
			fc := &fakeAPIMQTTClient{}
			waiter, err := newResultWaiter(fc)
			if err != nil {
				t.Fatalf("newResultWaiter: %v", err)
			}
			h := intentHandler(fc, waiter)

			reqBody := `{"kind":"` + c.kind + `","body":` + c.body + `}`
			req := httptest.NewRequest(http.MethodPost, "/intent", strings.NewReader(reqBody))
			rec := httptest.NewRecorder()
			h(rec, req)

			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202 (no reply configured): %s", rec.Code, rec.Body.String())
			}
			if len(fc.publishes) != 1 {
				t.Fatalf("expected exactly 1 publish, got %d", len(fc.publishes))
			}
			pub := fc.publishes[0]
			if pub.topic != c.wantTopic {
				t.Errorf("topic = %q, want %q", pub.topic, c.wantTopic)
			}
			if pub.retained != c.wantRetained {
				t.Errorf("retained = %v, want %v", pub.retained, c.wantRetained)
			}
			if pub.qos != c.wantQoS {
				t.Errorf("qos = %d, want %d", pub.qos, c.wantQoS)
			}

			// Every published intent must carry a stamped IntentMeta:
			// server-generated ID, Origin "app", Actor "local-api", a real
			// IssuedAt, and Envelope.V > 0.
			var meta struct {
				V        int    `json:"v"`
				ID       string `json:"id"`
				Origin   string `json:"origin"`
				Actor    string `json:"actor"`
				IssuedAt int64  `json:"issued_at"`
			}
			if err := json.Unmarshal(pub.payload, &meta); err != nil {
				t.Fatalf("decode published payload: %v", err)
			}
			if meta.V == 0 {
				t.Error("published envelope v is 0, want a stamped schema version")
			}
			if meta.ID == "" {
				t.Error("published IntentMeta.ID is empty, want a stamped random ID")
			}
			if meta.Origin != "app" {
				t.Errorf("Origin = %q, want %q", meta.Origin, "app")
			}
			if meta.Actor != "local-api" {
				t.Errorf("Actor = %q, want %q", meta.Actor, "local-api")
			}
			if meta.IssuedAt == 0 {
				t.Error("IssuedAt is 0, want the request's stamped time")
			}

			if waiter.pendingCount() != 0 {
				t.Errorf("pendingCount = %d after timeout, want 0 (leak)", waiter.pendingCount())
			}
		})
	}
}

// TestIntentHandler_ChargeNowRequiresPositiveTTL pins DEVICE_ROADMAP.md
// §4.3's mandatory TTL rule for the one edge kind: ttl_s missing or <= 0
// must 400 and never reach the bus; state kinds carrying a stray ttl_s must
// simply have it ignored (accept-and-ignore), not rejected.
func TestIntentHandler_ChargeNowRequiresPositiveTTL(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"missing ttl_s", `{"kind":"chargenow","body":{"station_id":"ev-1"}}`, http.StatusBadRequest},
		{"zero ttl_s", `{"kind":"chargenow","body":{"station_id":"ev-1","ttl_s":0}}`, http.StatusBadRequest},
		{"negative ttl_s", `{"kind":"chargenow","body":{"station_id":"ev-1","ttl_s":-5}}`, http.StatusBadRequest},
		{"positive ttl_s accepted", `{"kind":"chargenow","body":{"station_id":"ev-1","ttl_s":900}}`, http.StatusAccepted},
	}
	orig := intentResultWait
	intentResultWait = 30 * time.Millisecond
	defer func() { intentResultWait = orig }()

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fc := &fakeAPIMQTTClient{}
			waiter, err := newResultWaiter(fc)
			if err != nil {
				t.Fatalf("newResultWaiter: %v", err)
			}
			h := intentHandler(fc, waiter)
			req := httptest.NewRequest(http.MethodPost, "/intent", strings.NewReader(c.body))
			rec := httptest.NewRecorder()
			h(rec, req)
			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

// TestIntentHandler_StateKindIgnoresClientTTL pins the "accept-and-ignore"
// rule for state kinds: a stray ttl_s in the body must not be rejected and
// must not survive into the published message's IntentMeta.TTLS.
func TestIntentHandler_StateKindIgnoresClientTTL(t *testing.T) {
	fc := &fakeAPIMQTTClient{}
	waiter, err := newResultWaiter(fc)
	if err != nil {
		t.Fatalf("newResultWaiter: %v", err)
	}
	h := intentHandler(fc, waiter)

	orig := intentResultWait
	intentResultWait = 30 * time.Millisecond
	defer func() { intentResultWait = orig }()

	body := `{"kind":"mode","body":{"mode":"gateway","ttl_s":600}}`
	req := httptest.NewRequest(http.MethodPost, "/intent", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if len(fc.publishes) != 1 {
		t.Fatalf("expected exactly 1 publish, got %d", len(fc.publishes))
	}
	var got struct {
		TTLS int `json:"ttl_s"`
	}
	if err := json.Unmarshal(fc.publishes[0].payload, &got); err != nil {
		t.Fatalf("decode published payload: %v", err)
	}
	if got.TTLS != 0 {
		t.Errorf("published ttl_s = %d, want 0 (server-stamped meta must discard a client-supplied ttl_s on a state kind)", got.TTLS)
	}
}

// TestIntentHandler_OversizeBodyRejected pins the 64KiB MaxBytesReader bound.
func TestIntentHandler_OversizeBodyRejected(t *testing.T) {
	fc := &fakeAPIMQTTClient{}
	waiter, err := newResultWaiter(fc)
	if err != nil {
		t.Fatalf("newResultWaiter: %v", err)
	}
	h := intentHandler(fc, waiter)

	huge := strings.Repeat("a", intentMaxBodyBytes+1024)
	body := `{"kind":"mode","body":{"mode":"` + huge + `"}}`
	req := httptest.NewRequest(http.MethodPost, "/intent", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 for an oversize body", rec.Code)
	}
	if len(fc.publishes) != 0 {
		t.Fatalf("expected no publish for an oversize body, got %d", len(fc.publishes))
	}
}

// TestIntentHandler_PublishFailureReturns502 pins the publish-error path.
func TestIntentHandler_PublishFailureReturns502(t *testing.T) {
	fc := &fakeAPIMQTTClient{failNextPublish: true}
	waiter, err := newResultWaiter(fc)
	if err != nil {
		t.Fatalf("newResultWaiter: %v", err)
	}
	h := intentHandler(fc, waiter)

	req := httptest.NewRequest(http.MethodPost, "/intent", strings.NewReader(`{"kind":"mode","body":{"mode":"gateway"}}`))
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 on publish failure: %s", rec.Code, rec.Body.String())
	}
	if waiter.pendingCount() != 0 {
		t.Errorf("pendingCount = %d after a publish failure, want 0 (waiter.cancel must clean up)", waiter.pendingCount())
	}
}

// TestIntentHandler_DeliveredResultReturns200Verbatim exercises the full
// round trip: the handler publishes, a concurrent goroutine (standing in for
// the hub) replies on lexa/intent/result with the matching ID, and the
// handler must return 200 with the hub's IntentResult verbatim rather than
// inventing its own outcome.
func TestIntentHandler_DeliveredResultReturns200Verbatim(t *testing.T) {
	fc := &fakeAPIMQTTClient{publishedCh: make(chan fakeAPIPublish, 1)}
	waiter, err := newResultWaiter(fc)
	if err != nil {
		t.Fatalf("newResultWaiter: %v", err)
	}
	h := intentHandler(fc, waiter)

	done := make(chan struct{})
	go func() {
		defer close(done)
		pub := <-fc.publishedCh
		var published bus.ModeIntent
		if err := json.Unmarshal(pub.payload, &published); err != nil {
			t.Errorf("decode published intent: %v", err)
			return
		}
		result := bus.IntentResult{
			ID: published.ID, Kind: "mode", Outcome: "applied", Detail: "ok", Ts: time.Now().Unix(),
		}
		payload, err := json.Marshal(result)
		if err != nil {
			t.Errorf("marshal result: %v", err)
			return
		}
		deliverAPI(t, fc, bus.TopicIntentResult, payload)
	}()

	req := httptest.NewRequest(http.MethodPost, "/intent", strings.NewReader(`{"kind":"mode","body":{"mode":"gateway"}}`))
	rec := httptest.NewRecorder()
	h(rec, req)
	<-done

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got bus.IntentResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Outcome != "applied" || got.Detail != "ok" || got.Kind != "mode" {
		t.Errorf("response = %+v, want the hub's IntentResult verbatim (outcome=applied, detail=ok, kind=mode)", got)
	}
	if waiter.pendingCount() != 0 {
		t.Errorf("pendingCount = %d after delivery, want 0 (leak)", waiter.pendingCount())
	}
}

// TestIntentHandler_TimeoutReturns202AndNoLeak pins the 202-pending path and
// the goroutine/map-leak-proof requirement: after the wait elapses with no
// reply, the pending map must be empty.
func TestIntentHandler_TimeoutReturns202AndNoLeak(t *testing.T) {
	fc := &fakeAPIMQTTClient{}
	waiter, err := newResultWaiter(fc)
	if err != nil {
		t.Fatalf("newResultWaiter: %v", err)
	}
	h := intentHandler(fc, waiter)

	orig := intentResultWait
	intentResultWait = 20 * time.Millisecond
	defer func() { intentResultWait = orig }()

	req := httptest.NewRequest(http.MethodPost, "/intent", strings.NewReader(`{"kind":"mode","body":{"mode":"gateway"}}`))
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got["outcome"] != "pending" || got["id"] == "" {
		t.Errorf("response = %+v, want {id:<non-empty>, outcome:pending}", got)
	}
	if waiter.pendingCount() != 0 {
		t.Errorf("pendingCount = %d after timeout, want 0 (leak)", waiter.pendingCount())
	}
}

// TestRequireBearerStrict_WiredOnIntent is a smoke test that main.go's
// intended wiring (requireBearerStrict wrapping intentHandler) behaves as
// documented: an empty/unconfigured token must 401 even though read routes
// stay open (auth_test.go already pins requireBearerStrict itself in
// isolation; this pins the composition this package actually uses).
func TestRequireBearerStrict_WiredOnIntent(t *testing.T) {
	fc := &fakeAPIMQTTClient{}
	waiter, err := newResultWaiter(fc)
	if err != nil {
		t.Fatalf("newResultWaiter: %v", err)
	}
	wrapped := requireBearerStrict("", intentHandler(fc, waiter))

	req := httptest.NewRequest(http.MethodPost, "/intent", strings.NewReader(`{"kind":"mode","body":{"mode":"gateway"}}`))
	rec := httptest.NewRecorder()
	wrapped(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (no token configured must fail closed for writes)", rec.Code)
	}
	if len(fc.publishes) != 0 {
		t.Fatalf("expected no publish when auth denies the request, got %d", len(fc.publishes))
	}
}
