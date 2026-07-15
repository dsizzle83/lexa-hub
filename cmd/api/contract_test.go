package main

// contract_test.go is the hub⇄app HTTP contract DRIFT GATE (Workstream C).
//
// For every app-consumed route it builds the REAL cmd/api handler over a real
// stateStore (reusing this package's existing test scaffolding — the same
// store.onXxx feeders and fakeAPIMQTTClient the per-route tests use), captures
// the handler's actual JSON, and asserts it still conforms to the pinned
// golden fixture in internal/apicontract/http_v1/. A field the app reads being
// dropped, renamed, or retyped makes apicontract.Conform return a non-empty
// mismatch list and fails the build — the protection that did not exist
// before this workstream.
//
// It also pins the three version surfaces (X-Lexa-Contract-Version header,
// the additive contract_version JSON field in /status + /site, and the mDNS
// "contract=" TXT record) against the single apicontract.Version constant.
//
// The golden fixtures were captured FROM these same handlers; regenerate them
// (and bump apicontract.Version, per docs/API_CONTRACT.md) whenever a breaking
// contract change is intended.
import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"lexa-hub/internal/apicontract"
	"lexa-hub/internal/bus"
)

// assertConforms fails the test with every drift path if the live body no
// longer matches the named golden fixture.
func assertConforms(t *testing.T, fixture string, body []byte) {
	t.Helper()
	golden := apicontract.Golden(fixture)
	if golden == nil {
		t.Fatalf("missing embedded golden fixture %q", fixture)
	}
	ms := apicontract.Conform(golden, body)
	if len(ms) != 0 {
		t.Errorf("%s: live handler drifted from the golden contract (%d mismatch(es)):", fixture, len(ms))
		for _, m := range ms {
			t.Errorf("    %s", m)
		}
		t.Errorf("  live body was: %s", body)
	}
}

// statusStore builds a stateStore populated so GET /status emits every
// load-bearing app field (each is otherwise omitempty/conditional).
func statusStore(t *testing.T) *stateStore {
	t.Helper()
	store := newStateStore([]DeviceConfig{{Name: "batt-0", Role: "battery", MaxW: 10000}}, 2*time.Minute)

	w, v, hz, soc := 1000.0, 240.0, 60.0, 55.0
	store.onMeasurement(bus.MeasurementTopic("batt-0"), bus.Measurement{Device: "batt-0", W: &w, VoltageV: &v, Hz: &hz})
	store.onBattMetrics(bus.BattMetricsTopic("batt-0"), bus.BattMetrics{Device: "batt-0", SOC: &soc})

	cur, maxCur, evV, pw, evSOC := 16.0, 32.0, 240.0, 3840.0, 60.0
	store.onEVSEState(bus.EVSEStateTopic("evse-001"), bus.EVSEState{
		StationID: "evse-001", ConnectorID: 1, Connected: true, SessionActive: true,
		Status: "Charging", CurrentA: &cur, MaxCurrentA: &maxCur, VoltageV: &evV, PowerW: &pw, SOC: &evSOC,
	})
	// Backdate the active session so it reads stale — the simplest way to make
	// buildStatus emit a non-empty stale_sources array (INV-EVBLIND). Written
	// directly (sequential test goroutine, no concurrent snapshot ⇒ no race).
	store.evses[evseKey("evse-001", 1)].UpdatedAt = time.Now().Add(-45 * time.Second)

	expLim, maxLim, impLim, fixedW := 5000.0, 8000.0, 6000.0, 0.0
	connect := true
	store.onCSIPControl(bus.TopicCSIPControl, bus.ActiveControl{
		Source: "event", MRID: "abc123", Connect: &connect,
		ExpLimW: &expLim, MaxLimW: &maxLim, ImpLimW: &impLim, FixedW: &fixedW,
		ValidUntil: 4102444800, // year 2100 — far future so it never reports as expired
		Ts:         time.Now().Unix(),
	})

	store.onCertStatus(bus.TopicNorthboundCertStatus, bus.CertStatus{
		ClientDaysLeft: 90, CADaysLeft: 3650, DaysLeft: 90, Ts: time.Now().Unix(),
	})
	store.onCloudlinkStatus(bus.TopicCloudlinkStatus, bus.CloudlinkStatus{
		Connected: true, Endpoint: "ssl://cloud.example:8883", SpoolBytes: 0, CertDaysLeft: 45,
	})
	// WP-15: seed a healthy VEN status so the live /status emits the "openadr"
	// object the golden fixture pins (last_err stays empty ⇒ omitted, same as
	// cert_status's client_err/ca_err above).
	store.onOpenADRStatus(bus.TopicOpenADRStatus, bus.OpenADRStatus{
		VTNOK: true, TokenOK: true, LastPollTs: time.Now().Unix(), Programs: 2, ActiveEvents: 1,
	})
	store.onModeStatus(bus.TopicHubMode, bus.ModeStatus{Mode: "gateway", Since: 500})

	ep, fp, exp := 30.0, 20.0, 0.05
	store.onHubSettings(bus.TopicHubSettings, bus.HubSettings{
		Reserve: bus.ReserveSettings{EffectivePct: &ep, FloorPct: &fp, Source: "app"},
		Tariff: bus.TariffSettings{Source: "manual", UpdatedAt: 1752000000, Spec: &bus.TariffSpec{
			Currency: "USD",
			Periods: []bus.TariffPeriod{{
				Label: "peak", Days: []int{1, 2, 3, 4, 5}, StartHH: 16, EndHH: 21,
				ImportPerKwh: 0.38, ExportPerKwh: &exp,
			}},
		}},
	})

	store.onPlanLog(bus.TopicHubPlan, bus.PlanLog{
		Ts:        time.Now().Unix(),
		Decisions: []bus.PlanDecision{{Rule: "csip/export-limit", Reason: "export over cap", Impact: "curtail pv"}},
	})
	return store
}

// TestContract_Status is the /status drift gate.
func TestContract_Status(t *testing.T) {
	store := statusStore(t)
	planHB := newPlanHeartbeat(75 * time.Second)
	planHB.onPlanLog(time.Now().Unix(), time.Now()) // ⇒ plan_heartbeat state "ok"

	h := statusHandler(store, planHB, "0000000000000000000000000000000000000000000000000000000000000000")
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	assertConforms(t, "status.json", rec.Body.Bytes())

	// contract_version surfaces in the body and equals the constant.
	var got struct {
		ContractVersion int `json:"contract_version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode /status: %v", err)
	}
	if got.ContractVersion != apicontract.Version {
		t.Errorf("/status contract_version = %d, want %d", got.ContractVersion, apicontract.Version)
	}
}

// TestContract_Site is the /site drift gate + contract_version field.
func TestContract_Site(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "site.json")
	if err := os.WriteFile(cachePath, []byte(`{"address":"123 Main St"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	h := siteHandler("SN-TEST-1", cachePath)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/site", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	assertConforms(t, "site.json", rec.Body.Bytes())

	var got struct {
		ContractVersion int `json:"contract_version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode /site: %v", err)
	}
	if got.ContractVersion != apicontract.Version {
		t.Errorf("/site contract_version = %d, want %d", got.ContractVersion, apicontract.Version)
	}
}

// TestContract_Devices is the /devices drift gate.
func TestContract_Devices(t *testing.T) {
	store := newStateStore([]DeviceConfig{{Name: "batt-0", Role: "battery", MaxW: 10000}}, 2*time.Minute)
	w, v, hz, soc := 1000.0, 240.0, 60.0, 55.0
	store.onMeasurement(bus.MeasurementTopic("batt-0"), bus.Measurement{Device: "batt-0", W: &w, VoltageV: &v, Hz: &hz})
	store.onBattMetrics(bus.BattMetricsTopic("batt-0"), bus.BattMetrics{Device: "batt-0", SOC: &soc})

	cur, maxCur, evV, pw, evSOC := 16.0, 32.0, 240.0, 3840.0, 60.0
	store.onEVSEState(bus.EVSEStateTopic("evse-001"), bus.EVSEState{
		StationID: "evse-001", ConnectorID: 1, Connected: true, SessionActive: true,
		Status: "Charging", CurrentA: &cur, MaxCurrentA: &maxCur, VoltageV: &evV, PowerW: &pw, SOC: &evSOC,
	})

	np := 5000.0
	store.onScanResult(bus.TopicScanResult, bus.ScanResult{
		ID: "scan-1", Ts: time.Now().Unix(),
		Devices: []bus.ScanHit{{
			URL: "tcp://192.168.1.41:502", UnitID: 2, Manufacturer: "Acme", Model: "INV-5000",
			Serial: "INV-SN-1", FwVersion: "1.0.0", Class: "inverter", NameplateW: &np,
		}},
	})
	store.onOCPPPending(bus.TopicOCPPPending, bus.PendingStations{
		Stations: []bus.PendingStation{{
			StationID: "ev-unknown", Vendor: "AcmeEV", ModelName: "Wallbox-1",
			FirstSeenTs: time.Now().Unix(), RemoteAddr: "10.0.0.9:5000",
		}},
	})

	h := devicesHandler(store)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/devices", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	assertConforms(t, "devices.json", rec.Body.Bytes())
}

// TestContract_TelemetryRecent is the /telemetry/recent drift gate.
func TestContract_TelemetryRecent(t *testing.T) {
	store := newStateStore(nil, 2*time.Minute)
	w, v, hz := 1000.0, 240.0, 60.0
	store.onMeasurement(bus.MeasurementTopic("inv-0"), bus.Measurement{Device: "inv-0", W: &w, VoltageV: &v, Hz: &hz})
	soc, soh, capWh, mcw, mdw := 55.0, 98.0, 13500.0, 5000.0, 5000.0
	store.onBattMetrics(bus.BattMetricsTopic("batt-0"), bus.BattMetrics{
		Device: "batt-0", SOC: &soc, SOH: &soh, CapacityWh: &capWh, MaxChargeW: &mcw, MaxDischargeW: &mdw,
	})

	h := telemetryRecentHandler(store.telemetry)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/telemetry/recent", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	assertConforms(t, "telemetry_recent.json", rec.Body.Bytes())
}

// TestContract_Mode is the /mode drift gate.
func TestContract_Mode(t *testing.T) {
	store := newStateStore(nil, time.Minute)
	store.onModeStatus(bus.TopicHubMode, bus.ModeStatus{
		Mode: "gateway", Since: 1000, Actor: "installer@example.com", IntentID: "abc123",
	})
	h := modeHandler(store)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/mode", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	assertConforms(t, "mode.json", rec.Body.Bytes())
}

// TestContract_Plan is the /plan drift gate.
func TestContract_Plan(t *testing.T) {
	ws := int64(1_752_000_000)
	soc0 := 50.0
	store := newStateStore(nil, time.Minute)
	store.onHubSchedule(bus.TopicHubSchedule, bus.HubSchedule{
		Envelope: bus.Envelope{V: bus.HubScheduleV}, GeneratedAt: ws, WindowStart: ws, SlotMinutes: 5, HorizonH: 24,
		SolarForecastW: []float64{1500}, LoadForecastW: []float64{800}, BatterySetpointW: []float64{-1000}, BatterySocPct: []*float64{&soc0},
		EVPlanW: map[string][]float64{"evse-001": {-7200}},
		// Plan economics (PR-E) — seed so the live handler emits the new keys
		// (currency/total_cost/fixed_daily_charge + price_forecast/cost_plan).
		Currency:       "USD",
		ImportPriceKwh: []float64{0.38}, DeliveryPriceKwh: []float64{0.05}, ExportPriceKwh: []float64{0.10},
		GridW: []float64{1200}, MarginalCost: []float64{0.0076},
		TotalCost: 4.25, FixedDailyCharge: 0.35,
		Ts: ws + 10,
	})
	h := planHandler(store)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/plan", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	assertConforms(t, "plan.json", rec.Body.Bytes())
}

// TestContract_ScanGet is the GET /scan drift gate.
func TestContract_ScanGet(t *testing.T) {
	store := newStateStore(nil, time.Minute)
	store.onScanStatus(bus.TopicScanStatus, bus.ScanStatus{
		ID: "s1", Phase: "tcp", Probed: 5, Found: 1, Detail: "probing 192.168.1.0/24", Ts: time.Now().Unix(),
	})
	np := 5000.0
	store.onScanResult(bus.TopicScanResult, bus.ScanResult{
		ID: "s1", Ts: time.Now().Unix(),
		Devices: []bus.ScanHit{{
			URL: "tcp://192.168.1.40:502", UnitID: 1, Manufacturer: "Acme", Model: "INV-5000",
			Serial: "INV-SN-1", FwVersion: "1.0.0", Class: "inverter", NameplateW: &np,
		}},
	})
	h := scanGetHandler(store)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/scan", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	assertConforms(t, "scan_get.json", rec.Body.Bytes())
}

// TestContract_ScanPost is the POST /scan (202 {id}) drift gate.
func TestContract_ScanPost(t *testing.T) {
	fc := &fakeAPIMQTTClient{}
	h := scanPostHandler(fc)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/scan", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	assertConforms(t, "scan_post.json", rec.Body.Bytes())
}

// TestContract_IntentResult drives the full POST /intent round trip (publish →
// hub reply on lexa/intent/result → 200) and gates the IntentResult wire
// shape the app consumes.
func TestContract_IntentResult(t *testing.T) {
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
		result := bus.IntentResult{ID: published.ID, Kind: "mode", Outcome: "applied", Detail: "ok", Ts: time.Now().Unix()}
		payload, err := json.Marshal(result)
		if err != nil {
			t.Errorf("marshal result: %v", err)
			return
		}
		deliverAPI(t, fc, bus.TopicIntentResult, payload)
	}()

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/intent", strings.NewReader(`{"kind":"mode","body":{"mode":"gateway"}}`)))
	<-done

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	assertConforms(t, "intent_result.json", rec.Body.Bytes())
}

// TestContract_IntentRequests pins the REQUEST contract: each documented
// {kind, body{…}} fixture must still be ACCEPTED by the real intent whitelist/
// decode path (202 pending, exactly one publish) — a renamed body field or a
// tightened whitelist would break the app and is caught here.
func TestContract_IntentRequests(t *testing.T) {
	orig := intentResultWait
	intentResultWait = 20 * time.Millisecond // no reply configured ⇒ resolve to 202 fast
	defer func() { intentResultWait = orig }()

	for _, kind := range []string{"mode", "evgoal", "reserve", "tariff", "chargenow"} {
		t.Run(kind, func(t *testing.T) {
			body := apicontract.Golden("intent_" + kind + "_request.json")
			if body == nil {
				t.Fatalf("missing embedded request fixture for kind %q", kind)
			}
			fc := &fakeAPIMQTTClient{}
			waiter, err := newResultWaiter(fc)
			if err != nil {
				t.Fatalf("newResultWaiter: %v", err)
			}
			h := intentHandler(fc, waiter)

			rec := httptest.NewRecorder()
			h(rec, httptest.NewRequest(http.MethodPost, "/intent", bytes.NewReader(body)))

			if rec.Code != http.StatusAccepted {
				t.Fatalf("kind %q: status = %d, want 202 (documented request must be accepted): %s", kind, rec.Code, rec.Body.String())
			}
			if len(fc.publishes) != 1 {
				t.Fatalf("kind %q: expected exactly 1 publish, got %d", kind, len(fc.publishes))
			}
		})
	}
}

// TestContract_Pairing is the POST /devices/evse/{id}/pairing drift gate
// (WP-13): the documented request fixture must still be ACCEPTED by the real
// handler (202, exactly one bus publish — QoS 1, NOT retained, on
// lexa/ocpp/pairing), and the response body must conform to its golden
// fixture. An additive route: apicontract.Version stays 1 per the bumping
// rule (docs/API_CONTRACT.md — "a new route" is additive).
func TestContract_Pairing(t *testing.T) {
	body := apicontract.Golden("pairing_request.json")
	if body == nil {
		t.Fatal("missing embedded fixture pairing_request.json")
	}
	fc := &fakeAPIMQTTClient{}
	h := pairingHandler(fc)

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/devices/evse/ev-unknown/pairing", bytes.NewReader(body)))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (documented request must be accepted): %s", rec.Code, rec.Body.String())
	}
	if len(fc.publishes) != 1 {
		t.Fatalf("expected exactly 1 publish, got %d", len(fc.publishes))
	}
	pub := fc.publishes[0]
	if pub.topic != bus.TopicOCPPPairing {
		t.Errorf("published to %q, want %q", pub.topic, bus.TopicOCPPPairing)
	}
	if pub.retained {
		t.Error("pairing decision published RETAINED — it is an edge, never retained (D10/topics discipline)")
	}
	if pub.qos != bus.QoS1 {
		t.Errorf("pairing decision QoS = %d, want 1", pub.qos)
	}
	assertConforms(t, "pairing_response.json", rec.Body.Bytes())
}

// TestContract_VersionHeader pins that the withContractVersion middleware
// stamps X-Lexa-Contract-Version on a wrapped response.
func TestContract_VersionHeader(t *testing.T) {
	wrapped := withContractVersion(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if got := rec.Header().Get(contractVersionHeader); got != strconv.Itoa(apicontract.Version) {
		t.Errorf("%s = %q, want %q", contractVersionHeader, got, strconv.Itoa(apicontract.Version))
	}
}

// TestContract_MDNSTXT pins that the mDNS TXT record advertises the contract
// version.
func TestContract_MDNSTXT(t *testing.T) {
	a := &mdnsAdvertiser{serial: "SN-TEST", port: 9100, tlsOn: true}
	want := "contract=" + strconv.Itoa(apicontract.Version)
	found := false
	for _, rec := range a.txt() {
		if rec == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("mDNS TXT %v does not contain %q", a.txt(), want)
	}
}
