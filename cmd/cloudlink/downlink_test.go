package main

// downlink_test.go: units 2.4/§2.6 — the cloud→intent validation chain.
// Fakes follow the house pattern: fakeBusClient is a minimal mqtt.Client
// double (cmd/api/mqttfake_test.go's shape), fakeCmdCloud wraps batch_test's
// fakeCloud with a recording SubscribeCmd.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/journal"
	"lexa-hub/internal/metrics"
)

// -----------------------------------------------------------------------------
// fakes.
// -----------------------------------------------------------------------------

type busPub struct {
	topic    string
	qos      byte
	retained bool
	payload  []byte
}

// fakeBusClient is a minimal LOCAL-bus mqtt.Client double: records publishes,
// optionally failing them, and can run a hook inside Publish (the
// journal-before-forward ordering probe). Methods the downlink never touches
// panic loudly (cmd/api/mqttfake_test.go's convention).
type fakeBusClient struct {
	mu        sync.Mutex
	publishes []busPub

	failAll bool
	// onPublish, if non-nil, runs synchronously inside Publish BEFORE the
	// publish is recorded — used to observe world state (e.g. journal
	// contents) at the exact moment the forward fires.
	onPublish func(topic string)
}

func (f *fakeBusClient) IsConnected() bool      { return true }
func (f *fakeBusClient) IsConnectionOpen() bool { return true }
func (f *fakeBusClient) Connect() mqtt.Token    { panic("not implemented") }
func (f *fakeBusClient) Disconnect(uint)        {}

func (f *fakeBusClient) Publish(topic string, qos byte, retained bool, payload interface{}) mqtt.Token {
	if f.onPublish != nil {
		f.onPublish(topic)
	}
	var b []byte
	switch p := payload.(type) {
	case []byte:
		b = p
	case string:
		b = []byte(p)
	}
	f.mu.Lock()
	f.publishes = append(f.publishes, busPub{topic: topic, qos: qos, retained: retained, payload: append([]byte(nil), b...)})
	fail := f.failAll
	f.mu.Unlock()
	if fail {
		return busFailedToken{}
	}
	return busDoneToken{}
}

func (f *fakeBusClient) Subscribe(string, byte, mqtt.MessageHandler) mqtt.Token {
	panic("not implemented")
}
func (f *fakeBusClient) SubscribeMultiple(map[string]byte, mqtt.MessageHandler) mqtt.Token {
	panic("not implemented")
}
func (f *fakeBusClient) Unsubscribe(...string) mqtt.Token        { panic("not implemented") }
func (f *fakeBusClient) AddRoute(string, mqtt.MessageHandler)    { panic("not implemented") }
func (f *fakeBusClient) OptionsReader() mqtt.ClientOptionsReader { panic("not implemented") }

func (f *fakeBusClient) pubs() []busPub {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]busPub(nil), f.publishes...)
}

type busDoneToken struct{}

func (busDoneToken) Wait() bool                     { return true }
func (busDoneToken) WaitTimeout(time.Duration) bool { return true }
func (busDoneToken) Done() <-chan struct{}          { c := make(chan struct{}); close(c); return c }
func (busDoneToken) Error() error                   { return nil }

type busFailedToken struct{}

func (busFailedToken) Wait() bool                     { return true }
func (busFailedToken) WaitTimeout(time.Duration) bool { return true }
func (busFailedToken) Done() <-chan struct{}          { c := make(chan struct{}); close(c); return c }
func (busFailedToken) Error() error                   { return errors.New("fake bus publish failure") }

// fakeCmdCloud satisfies cloudCmdSubscriber for runDownlink tests: embeds
// batch_test.go's fakeCloud (cloudPublisher) and records SubscribeCmd calls.
type fakeCmdCloud struct {
	*fakeCloud
	mu         sync.Mutex
	subscribes []string
	subErr     error
}

func (f *fakeCmdCloud) SubscribeCmd(topic string, _ mqtt.MessageHandler) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subErr != nil {
		return f.subErr
	}
	f.subscribes = append(f.subscribes, topic)
	return nil
}

func (f *fakeCmdCloud) subCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subscribes)
}

func (f *fakeCmdCloud) setConnected(v bool) {
	f.fakeCloud.mu.Lock()
	f.fakeCloud.connected = v
	f.fakeCloud.mu.Unlock()
}

// -----------------------------------------------------------------------------
// fixture + helpers.
// -----------------------------------------------------------------------------

type downlinkFixture struct {
	dl  *downlink
	fc  *fakeBusClient
	reg *metrics.Registry
	now time.Time // the frozen clock every component sees
}

func newTestDownlink(t *testing.T, jw *journal.Writer) *downlinkFixture {
	t.Helper()
	reg := metrics.New()
	m := newCloudlinkMetrics(reg)
	fc := &fakeBusClient{}
	dl := newDownlink(fc, jw, m)
	fx := &downlinkFixture{dl: dl, fc: fc, reg: reg, now: time.Unix(1752000000, 0)}
	nowFn := func() time.Time { return fx.now }
	dl.now = nowFn
	dl.limiter = newPerKindLimiter(nowFn)
	return fx
}

// metricValue parses reg.Format()'s exposition text for name's value.
func metricValue(t *testing.T, reg *metrics.Registry, name string) float64 {
	t.Helper()
	for _, line := range strings.Split(reg.Format(), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == name {
			v, err := strconv.ParseFloat(fields[1], 64)
			if err != nil {
				t.Fatalf("parse %s value %q: %v", name, fields[1], err)
			}
			return v
		}
	}
	t.Fatalf("metric %s not found in registry output", name)
	return 0
}

// cmdPayload builds a {v,kind,body} cloud command wrapper.
func cmdPayload(t *testing.T, v int, kind string, body any) []byte {
	t.Helper()
	bodyRaw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	raw, err := json.Marshal(map[string]any{"v": v, "kind": kind, "body": json.RawMessage(bodyRaw)})
	if err != nil {
		t.Fatalf("marshal cmd: %v", err)
	}
	return raw
}

// validBodyFor returns a minimal VALID body (origin "cloud", passes every
// validation) for each of the seven kinds, issued at issuedAt.
func validBodyFor(kind string, issuedAt int64) map[string]any {
	meta := map[string]any{"id": "id-" + kind, "origin": "cloud", "issued_at": issuedAt}
	switch kind {
	case "mode":
		meta["mode"] = "optimizer"
	case "evgoal":
		meta["target_soc_kwh"] = 40.0
		meta["departure_unix"] = issuedAt + 3600
	case "reserve":
		meta["reserve_pct"] = 30.0
	case "tariff":
		meta["tariff"] = map[string]any{"currency": "USD", "periods": []any{}}
	case "solarforecast":
		meta["window_start"] = issuedAt - issuedAt%300
		meta["step_kw"] = []float64{1, 2, 3}
	case "loadprofile":
		meta["step_kw"] = []float64{0.5, 0.7}
	case "chargenow":
		meta["ttl_s"] = 3600
	}
	return meta
}

// clJournalEvents scans a journal dir into type → events.
func clJournalEvents(t *testing.T, dir string) map[string][]journal.Event {
	t.Helper()
	out := make(map[string][]journal.Event)
	if _, err := journal.Scan(dir, journal.DefaultName, func(e journal.Event) error {
		out[e.Type] = append(out[e.Type], e)
		return nil
	}); err != nil {
		t.Fatalf("journal.Scan: %v", err)
	}
	return out
}

// -----------------------------------------------------------------------------
// dispatch table parity with cmd/api.
// -----------------------------------------------------------------------------

// TestIntentKinds_DispatchTable_ParityWithAPI pins the dispatch table against
// the IDENTICAL literal expectations cmd/api/intent.go's localIntentKinds +
// cloudOnlyIntentKinds encode (both files are `package main` in different
// binaries, so the shared-literal-table technique from cmd/hub/state_test.go's
// wattEncoderAgreement applies: a divergence here means one of the two choke
// points changed its retained/topic policy without the other).
func TestIntentKinds_DispatchTable_ParityWithAPI(t *testing.T) {
	want := map[string]struct {
		topic    string
		retained bool
		edge     bool
	}{
		// The five kinds cmd/api also serves — retained/topic copied verbatim
		// from its localIntentKinds table.
		"mode":      {"lexa/intent/mode", true, false},
		"evgoal":    {"lexa/intent/evgoal", true, false},
		"reserve":   {"lexa/intent/reserve", true, false},
		"tariff":    {"lexa/intent/tariff", true, false},
		"chargenow": {"lexa/intent/chargenow", false, true}, // the one edge kind, NOT retained
		// The two cloud-only kinds cmd/api REJECTS (cloudOnlyIntentKinds) and
		// only this service may publish — state kinds, retained.
		"solarforecast": {"lexa/intent/solarforecast", true, false},
		"loadprofile":   {"lexa/intent/loadprofile", true, false},
	}

	if len(intentKinds) != len(want) {
		t.Fatalf("intentKinds has %d kinds, want %d", len(intentKinds), len(want))
	}
	for kind, w := range want {
		spec, ok := intentKinds[kind]
		if !ok {
			t.Errorf("kind %q missing from intentKinds", kind)
			continue
		}
		if spec.topic != w.topic {
			t.Errorf("%s topic = %q, want %q", kind, spec.topic, w.topic)
		}
		if spec.topic != bus.IntentTopic(kind) {
			t.Errorf("%s topic = %q, want bus.IntentTopic(%q) = %q", kind, spec.topic, kind, bus.IntentTopic(kind))
		}
		if spec.retained != w.retained {
			t.Errorf("%s retained = %v, want %v", kind, spec.retained, w.retained)
		}
		if spec.edge != w.edge {
			t.Errorf("%s edge = %v, want %v", kind, spec.edge, w.edge)
		}
	}
	// diag is dispatched (process handles it) but must NOT be in the intent
	// table — it is not an intent and never reaches the bus.
	if _, ok := intentKinds[diagKind]; ok {
		t.Error("diag must not appear in intentKinds (it is not an intent)")
	}
}

// -----------------------------------------------------------------------------
// rejection table + in-order precedence.
// -----------------------------------------------------------------------------

func TestDownlink_RejectionTable(t *testing.T) {
	cases := []struct {
		name       string
		payload    func(fx *downlinkFixture) []byte
		wantKind   string
		wantReason string
	}{
		{"malformed json", func(fx *downlinkFixture) []byte {
			return []byte(`{"v":1,"kind":`)
		}, "", "malformed"},
		{"version absent (v0)", func(fx *downlinkFixture) []byte {
			raw, _ := json.Marshal(map[string]any{"kind": "mode", "body": validBodyFor("mode", fx.now.Unix())})
			return raw
		}, "", "version"},
		{"version zero", func(fx *downlinkFixture) []byte {
			return cmdPayload(t, 0, "mode", validBodyFor("mode", fx.now.Unix()))
		}, "", "version"},
		{"version too new", func(fx *downlinkFixture) []byte {
			return cmdPayload(t, cloudCmdV+1, "mode", validBodyFor("mode", fx.now.Unix()))
		}, "", "version"},
		{"unknown kind", func(fx *downlinkFixture) []byte {
			return cmdPayload(t, 1, "reboot", map[string]any{})
		}, "", "unknown-kind"},
		{"undecodable body", func(fx *downlinkFixture) []byte {
			return cmdPayload(t, 1, "evgoal", "not-an-object")
		}, "evgoal", "decode"},
		{"nan in body is a decode reject (stdlib refuses bare NaN)", func(fx *downlinkFixture) []byte {
			return []byte(`{"v":1,"kind":"evgoal","body":{"id":"x","origin":"cloud","target_soc_kwh":NaN}}`)
		}, "", "malformed"}, // bare NaN breaks the WRAPPER unmarshal too: json.RawMessage validates
		{"origin app is forgery", func(fx *downlinkFixture) []byte {
			body := validBodyFor("evgoal", fx.now.Unix())
			body["origin"] = "app"
			return cmdPayload(t, 1, "evgoal", body)
		}, "evgoal", "origin-forgery"},
		{"origin absent is forgery", func(fx *downlinkFixture) []byte {
			body := validBodyFor("evgoal", fx.now.Unix())
			delete(body, "origin")
			return cmdPayload(t, 1, "evgoal", body)
		}, "evgoal", "origin-forgery"},
		{"expired chargenow", func(fx *downlinkFixture) []byte {
			body := validBodyFor("chargenow", fx.now.Unix()-7200)
			body["ttl_s"] = 60 // issued 2h ago, 60s TTL → long dead
			return cmdPayload(t, 1, "chargenow", body)
		}, "chargenow", "expired"},
		{"valid mode forwards", func(fx *downlinkFixture) []byte {
			return cmdPayload(t, 1, "mode", validBodyFor("mode", fx.now.Unix()))
		}, "mode", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newTestDownlink(t, nil)
			kind, reason := fx.dl.process(tc.payload(fx))
			if kind != tc.wantKind || reason != tc.wantReason {
				t.Errorf("process() = (%q, %q), want (%q, %q)", kind, reason, tc.wantKind, tc.wantReason)
			}
			wantRejected, wantForwarded := 1.0, 0.0
			if tc.wantReason == "" {
				wantRejected, wantForwarded = 0, 1
			}
			if got := metricValue(t, fx.reg, "lexa_cloudlink_intents_rejected_total"); got != wantRejected {
				t.Errorf("intents_rejected_total = %v, want %v", got, wantRejected)
			}
			if got := metricValue(t, fx.reg, "lexa_cloudlink_intents_forwarded_total"); got != wantForwarded {
				t.Errorf("intents_forwarded_total = %v, want %v", got, wantForwarded)
			}
			if tc.wantReason != "" && len(fx.fc.pubs()) != 0 {
				t.Errorf("rejected command still published %d message(s)", len(fx.fc.pubs()))
			}
		})
	}
}

// TestDownlink_PrecedenceOrder pins the chain ORDER: each case crafts a
// payload violating TWO rules and asserts the earlier one's reason wins.
func TestDownlink_PrecedenceOrder(t *testing.T) {
	t.Run("version before unknown-kind", func(t *testing.T) {
		fx := newTestDownlink(t, nil)
		_, reason := fx.dl.process(cmdPayload(t, 99, "reboot", map[string]any{}))
		if reason != "version" {
			t.Errorf("reason = %q, want version (bad version must reject before unknown kind)", reason)
		}
	})
	t.Run("unknown-kind before decode", func(t *testing.T) {
		fx := newTestDownlink(t, nil)
		_, reason := fx.dl.process(cmdPayload(t, 1, "reboot", "garbage-body"))
		if reason != "unknown-kind" {
			t.Errorf("reason = %q, want unknown-kind", reason)
		}
	})
	t.Run("decode before origin-forgery", func(t *testing.T) {
		fx := newTestDownlink(t, nil)
		// Body is undecodable AND would be a forged origin if it decoded.
		_, reason := fx.dl.process(cmdPayload(t, 1, "evgoal", []any{"origin", "app"}))
		if reason != "decode" {
			t.Errorf("reason = %q, want decode", reason)
		}
	})
	t.Run("origin-forgery before expired", func(t *testing.T) {
		fx := newTestDownlink(t, nil)
		body := validBodyFor("chargenow", fx.now.Unix()-7200)
		body["ttl_s"] = 60
		body["origin"] = "app" // forged AND expired
		_, reason := fx.dl.process(cmdPayload(t, 1, "chargenow", body))
		if reason != "origin-forgery" {
			t.Errorf("reason = %q, want origin-forgery (checked before TTL)", reason)
		}
	})
	t.Run("expired before rate-limited and consumes no token", func(t *testing.T) {
		fx := newTestDownlink(t, nil)
		body := validBodyFor("chargenow", fx.now.Unix()-7200)
		body["ttl_s"] = 60
		payload := cmdPayload(t, 1, "chargenow", body)
		// Far more repeats than the burst: if expired consumed tokens, later
		// calls would flip to rate-limited.
		for i := 0; i < rateLimitBurst+3; i++ {
			if _, reason := fx.dl.process(payload); reason != "expired" {
				t.Fatalf("attempt %d reason = %q, want expired every time", i, reason)
			}
		}
	})
}

// TestDownlink_NonFiniteBranch exercises the Finite() defense-in-depth arm.
// Real JSON can't deliver a non-finite float through json.Unmarshal (stdlib
// rejects bare/quoted NaN — internal/bus/nan_reject_test.go pins that), so
// the branch is reached by temporarily registering a synthetic kind whose
// decode returns a type with a failing Finite(). The branch matters anyway:
// it guards a future decoder change, per GAP-09's residual.
type nonFiniteIntent struct{ meta bus.IntentMeta }

func (nonFiniteIntent) Finite() error { return errors.New("synthetic non-finite") }

func TestDownlink_NonFiniteBranch(t *testing.T) {
	const kind = "test-nonfinite"
	intentKinds[kind] = intentSpec{
		topic: bus.IntentTopic(kind), retained: false,
		decode: func(json.RawMessage) (any, bus.IntentMeta, error) {
			return nonFiniteIntent{}, bus.IntentMeta{Origin: "cloud"}, nil
		},
	}
	defer delete(intentKinds, kind)

	fx := newTestDownlink(t, nil)
	gotKind, reason := fx.dl.process(cmdPayload(t, 1, kind, map[string]any{}))
	if gotKind != kind || reason != "non-finite" {
		t.Errorf("process() = (%q, %q), want (%q, non-finite)", gotKind, reason, kind)
	}
	if len(fx.fc.pubs()) != 0 {
		t.Error("non-finite intent was published")
	}
}

// -----------------------------------------------------------------------------
// chargenow TTL boundary.
// -----------------------------------------------------------------------------

func TestDownlink_ChargeNowTTL(t *testing.T) {
	t.Run("live chargenow forwards, not retained", func(t *testing.T) {
		fx := newTestDownlink(t, nil)
		body := validBodyFor("chargenow", fx.now.Unix())
		_, reason := fx.dl.process(cmdPayload(t, 1, "chargenow", body))
		if reason != "" {
			t.Fatalf("live chargenow rejected: %q", reason)
		}
		pubs := fx.fc.pubs()
		if len(pubs) != 1 {
			t.Fatalf("published %d messages, want 1", len(pubs))
		}
		if pubs[0].topic != "lexa/intent/chargenow" {
			t.Errorf("topic = %q", pubs[0].topic)
		}
		if pubs[0].retained {
			t.Error("chargenow published retained — the edge kind must never be")
		}
	})
	t.Run("deadline boundary: issued_at+ttl == now is NOT expired", func(t *testing.T) {
		fx := newTestDownlink(t, nil)
		body := validBodyFor("chargenow", fx.now.Unix()-3600)
		body["ttl_s"] = 3600 // deadline lands exactly on the frozen now
		_, reason := fx.dl.process(cmdPayload(t, 1, "chargenow", body))
		if reason != "" {
			t.Errorf("boundary chargenow rejected %q, want forwarded (expired() is strict <)", reason)
		}
	})
	t.Run("one second past the deadline IS expired", func(t *testing.T) {
		fx := newTestDownlink(t, nil)
		body := validBodyFor("chargenow", fx.now.Unix()-3601)
		body["ttl_s"] = 3600
		_, reason := fx.dl.process(cmdPayload(t, 1, "chargenow", body))
		if reason != "expired" {
			t.Errorf("reason = %q, want expired", reason)
		}
	})
}

// -----------------------------------------------------------------------------
// rate limit — token bucket under a fake clock.
// -----------------------------------------------------------------------------

func TestDownlink_RateLimit_TokenBucket(t *testing.T) {
	fx := newTestDownlink(t, nil)
	payload := func() []byte { return cmdPayload(t, 1, "reserve", validBodyFor("reserve", fx.now.Unix())) }

	// Burst of 3 passes (commissioning bursts are legitimate)...
	for i := 0; i < rateLimitBurst; i++ {
		if _, reason := fx.dl.process(payload()); reason != "" {
			t.Fatalf("burst message %d rejected: %q", i, reason)
		}
	}
	// ...the 4th is rate-limited.
	if _, reason := fx.dl.process(payload()); reason != "rate-limited" {
		t.Fatalf("4th message reason = %q, want rate-limited", reason)
	}

	// The limit is PER KIND: a different kind still passes.
	if _, reason := fx.dl.process(cmdPayload(t, 1, "mode", validBodyFor("mode", fx.now.Unix()))); reason != "" {
		t.Fatalf("other kind rejected while reserve is limited: %q", reason)
	}

	// 10s refills exactly one token.
	fx.now = fx.now.Add(rateLimitRefillEvery)
	if _, reason := fx.dl.process(payload()); reason != "" {
		t.Fatalf("post-refill message rejected: %q", reason)
	}
	if _, reason := fx.dl.process(payload()); reason != "rate-limited" {
		t.Fatalf("second post-refill message reason = %q, want rate-limited (only 1 token refilled)", reason)
	}

	// A long idle refills to the burst cap, never beyond.
	fx.now = fx.now.Add(time.Hour)
	for i := 0; i < rateLimitBurst; i++ {
		if _, reason := fx.dl.process(payload()); reason != "" {
			t.Fatalf("post-idle message %d rejected: %q", i, reason)
		}
	}
	if _, reason := fx.dl.process(payload()); reason != "rate-limited" {
		t.Fatalf("burst cap exceeded after idle: reason = %q, want rate-limited", reason)
	}
}

// -----------------------------------------------------------------------------
// journal-before-forward ordering.
// -----------------------------------------------------------------------------

func TestDownlink_JournalBeforeForward(t *testing.T) {
	dir := t.TempDir()
	// FlushEvery:1 → every Append is durably on disk before Append returns,
	// so the probe inside Publish sees exactly what had been journaled at
	// publish time.
	jw, err := journal.Open(journal.Config{Dir: dir, FlushEvery: 1})
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	defer jw.Close()

	fx := newTestDownlink(t, jw)

	journaledAtPublish := false
	fx.fc.onPublish = func(string) {
		events := clJournalEvents(t, dir)
		for _, e := range events[journal.TypeIntentReceived] {
			var p journal.IntentReceived
			if json.Unmarshal(e.Data, &p) == nil && p.ID == "id-evgoal" {
				journaledAtPublish = true
			}
		}
	}

	_, reason := fx.dl.process(cmdPayload(t, 1, "evgoal", validBodyFor("evgoal", fx.now.Unix())))
	if reason != "" {
		t.Fatalf("valid evgoal rejected: %q", reason)
	}
	if !journaledAtPublish {
		t.Error("intent_received was NOT in the journal at the moment the forward published (audit must precede effect)")
	}

	// Exactly one intent_received, with the right fields.
	events := clJournalEvents(t, dir)
	recs := events[journal.TypeIntentReceived]
	if len(recs) != 1 {
		t.Fatalf("intent_received events = %d, want 1", len(recs))
	}
	var p journal.IntentReceived
	if err := json.Unmarshal(recs[0].Data, &p); err != nil {
		t.Fatalf("unmarshal IntentReceived: %v", err)
	}
	if p.Kind != "evgoal" || p.ID != "id-evgoal" || p.Origin != "cloud" {
		t.Errorf("journaled payload = %+v", p)
	}
}

func TestDownlink_RejectedCommandNeverJournalsNorPublishes(t *testing.T) {
	dir := t.TempDir()
	jw, err := journal.Open(journal.Config{Dir: dir, FlushEvery: 1})
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	defer jw.Close()

	fx := newTestDownlink(t, jw)
	body := validBodyFor("evgoal", fx.now.Unix())
	body["origin"] = "app" // forged
	if _, reason := fx.dl.process(cmdPayload(t, 1, "evgoal", body)); reason != "origin-forgery" {
		t.Fatalf("reason = %q, want origin-forgery", reason)
	}

	if events := clJournalEvents(t, dir); len(events) != 0 {
		t.Errorf("rejected command journaled %d event type(s), want none", len(events))
	}
	if len(fx.fc.pubs()) != 0 {
		t.Errorf("rejected command published %d message(s), want none", len(fx.fc.pubs()))
	}
}

func TestDownlink_PublishFailure_CountsPubFailNotRejection(t *testing.T) {
	dir := t.TempDir()
	jw, err := journal.Open(journal.Config{Dir: dir, FlushEvery: 1})
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	defer jw.Close()

	fx := newTestDownlink(t, jw)
	fx.fc.failAll = true

	kind, reason := fx.dl.process(cmdPayload(t, 1, "mode", validBodyFor("mode", fx.now.Unix())))
	if kind != "mode" || reason != "publish-failed" {
		t.Fatalf("process() = (%q, %q), want (mode, publish-failed)", kind, reason)
	}
	if got := metricValue(t, fx.reg, "lexa_cloudlink_intent_pub_fail_total"); got != 1 {
		t.Errorf("intent_pub_fail_total = %v, want 1", got)
	}
	if got := metricValue(t, fx.reg, "lexa_cloudlink_intents_rejected_total"); got != 0 {
		t.Errorf("intents_rejected_total = %v, want 0 (a bus fault is not a rejection)", got)
	}
	if got := metricValue(t, fx.reg, "lexa_cloudlink_intents_forwarded_total"); got != 0 {
		t.Errorf("intents_forwarded_total = %v, want 0", got)
	}
	// Journaled BEFORE the failed publish — the audit line exists even though
	// the forward never landed (accepted-for-forwarding semantics).
	events := clJournalEvents(t, dir)
	if len(events[journal.TypeIntentReceived]) != 1 {
		t.Errorf("intent_received events = %d, want 1 (journal precedes the publish attempt)", len(events[journal.TypeIntentReceived]))
	}
}

// -----------------------------------------------------------------------------
// forwarded wire shape.
// -----------------------------------------------------------------------------

func TestDownlink_ForwardedIntent_WireShape(t *testing.T) {
	fx := newTestDownlink(t, nil)
	body := validBodyFor("evgoal", fx.now.Unix())
	body["actor"] = "user@example.com"
	if _, reason := fx.dl.process(cmdPayload(t, 1, "evgoal", body)); reason != "" {
		t.Fatalf("rejected: %q", reason)
	}

	pubs := fx.fc.pubs()
	if len(pubs) != 1 {
		t.Fatalf("published %d, want 1", len(pubs))
	}
	if pubs[0].topic != bus.IntentTopic("evgoal") || !pubs[0].retained || pubs[0].qos != 1 {
		t.Errorf("publish = topic %q retained %v qos %d, want lexa/intent/evgoal retained qos1",
			pubs[0].topic, pubs[0].retained, pubs[0].qos)
	}
	var out bus.EVGoalIntent
	if err := json.Unmarshal(pubs[0].payload, &out); err != nil {
		t.Fatalf("decode forwarded intent: %v", err)
	}
	if out.V != bus.EVGoalIntentV {
		t.Errorf("forwarded v = %d, want %d (cloudlink stamps the kind's version, cmd/api parity)", out.V, bus.EVGoalIntentV)
	}
	if out.ID != "id-evgoal" || out.Origin != "cloud" || out.Actor != "user@example.com" {
		t.Errorf("meta not preserved verbatim: %+v", out.IntentMeta)
	}
	if out.TargetSocKwh == nil || *out.TargetSocKwh != 40.0 {
		t.Errorf("body fields not preserved: %+v", out)
	}
}

// -----------------------------------------------------------------------------
// grep-proof: never publishes desired docs or any non-intent topic.
// -----------------------------------------------------------------------------

func TestDownlink_GrepProof_OnlyIntentTopicsEverPublished(t *testing.T) {
	fx := newTestDownlink(t, nil)
	for kind := range intentKinds {
		if _, reason := fx.dl.process(cmdPayload(t, 1, kind, validBodyFor(kind, fx.now.Unix()))); reason != "" {
			t.Fatalf("kind %s rejected: %q", kind, reason)
		}
	}
	pubs := fx.fc.pubs()
	if len(pubs) != len(intentKinds) {
		t.Fatalf("published %d messages for %d kinds", len(pubs), len(intentKinds))
	}
	for _, p := range pubs {
		if !strings.HasPrefix(p.topic, "lexa/intent/") {
			t.Errorf("published to non-intent topic %q", p.topic)
		}
		for _, forbidden := range []string{"lexa/desired/", "lexa/control/", "lexa/evse/", "lexa/csip/"} {
			if strings.HasPrefix(p.topic, forbidden) {
				t.Errorf("published to FORBIDDEN topic family %q", p.topic)
			}
		}
	}
}

// TestDownlink_GrepProof_SourceNeverNamesDesiredTopics greps this unit's
// source files for desired-doc/command topic references — the literal
// "grep-proof" the brief asks for, pinned as a test so it survives review.
func TestDownlink_GrepProof_SourceNeverNamesDesiredTopics(t *testing.T) {
	for _, file := range []string{"downlink.go", "diag.go", "certmon.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, forbidden := range []string{"lexa/desired", "DesiredTopic", "lexa/control/", "EVSECommandTopic", "CtrlBatteryTopic", "CtrlSolarTopic"} {
			if strings.Contains(string(src), forbidden) {
				t.Errorf("%s references %q — cloudlink must never touch the command path", file, forbidden)
			}
		}
	}
}

// -----------------------------------------------------------------------------
// runDownlink subscribe/resubscribe.
// -----------------------------------------------------------------------------

func TestRunDownlink_SubscribesAndResubscribesOnReconnect(t *testing.T) {
	origPoll := downlinkReconnectPoll
	downlinkReconnectPoll = 5 * time.Millisecond
	defer func() { downlinkReconnectPoll = origPoll }()

	cloud := &fakeCmdCloud{fakeCloud: &fakeCloud{serial: "SER", connected: true}}
	fx := newTestDownlink(t, nil)

	ctx, cancel := stoppableCtx(t)
	done := make(chan struct{})
	go func() { runDownlink(ctx, cloud, fx.dl); close(done) }()

	waitFor(t, "initial subscribe", func() bool { return cloud.subCount() == 1 })

	// Drop the link; no duplicate subscribe while down.
	cloud.setConnected(false)
	time.Sleep(25 * time.Millisecond)
	if cloud.subCount() != 1 {
		t.Fatalf("subscribed %d times while disconnected, want still 1", cloud.subCount())
	}

	// Link returns → re-subscribe (CleanSession=true means the broker forgot).
	cloud.setConnected(true)
	waitFor(t, "resubscribe after reconnect", func() bool { return cloud.subCount() == 2 })

	cloud.mu.Lock()
	for _, topic := range cloud.subscribes {
		if topic != "lexa/v1/SER/cmd" {
			t.Errorf("subscribed to %q, want lexa/v1/SER/cmd", topic)
		}
	}
	cloud.mu.Unlock()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runDownlink did not exit on ctx cancel")
	}
}

func TestRunDownlink_RetriesFailedSubscribe(t *testing.T) {
	origPoll := downlinkReconnectPoll
	downlinkReconnectPoll = 5 * time.Millisecond
	defer func() { downlinkReconnectPoll = origPoll }()

	cloud := &fakeCmdCloud{fakeCloud: &fakeCloud{serial: "S", connected: true}}
	cloud.subErr = errors.New("suback timeout")
	fx := newTestDownlink(t, nil)

	ctx, cancel := stoppableCtx(t)
	done := make(chan struct{})
	go func() { runDownlink(ctx, cloud, fx.dl); close(done) }()

	// Initial subscribe fails; once the error clears, the poll loop retries
	// (wasConnected stayed false on failure, so the next up-edge fires).
	time.Sleep(15 * time.Millisecond)
	cloud.mu.Lock()
	cloud.subErr = nil
	cloud.mu.Unlock()
	waitFor(t, "retry after failed subscribe", func() bool { return cloud.subCount() >= 1 })

	cancel()
	<-done
}

// waitFor polls cond up to 2s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// stoppableCtx returns a cancellable context the test cleans up.
func stoppableCtx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx, cancel
}
