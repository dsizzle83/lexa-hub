package main

// downlink.go implements DEVICE_ROADMAP.md §2.6: the cloud→intent downlink —
// the single choke point where the cloud's write authority is narrowed to
// the intent vocabulary. It subscribes lexa/v1/{serial}/cmd on the CLOUD
// session (QoS 1) and, for each received command, runs an in-order
// validation chain before ever forwarding anything onto the LOCAL bus.
//
// # Validation chain (in order — every early return is a distinct, tested
// # rejection reason)
//
//	malformed envelope JSON       → "malformed"
//	envelope version out of range → "version"
//	unknown kind                  → "unknown-kind"
//	  (kind == "diag" diverts to processDiag — see diag.go; it is NOT an
//	  intent and never reaches any step below)
//	body doesn't decode into the kind's typed bus.*Intent struct → "decode"
//	Finite() fails (when the type implements it)                → "non-finite"
//	IntentMeta.Origin != "cloud" (origin-forgery)                → "origin-forgery"
//	edge kind (chargenow) past its IssuedAt+TTLS deadline         → "expired"
//	per-kind token bucket exhausted                              → "rate-limited"
//	  → journal.NewIntentReceivedEvent BEFORE the forwarding publish
//	  → mqttutil.PublishJSONTimeout to bus.IntentTopic(kind), 2s bound
//	    (failure → "publish-failed", counted separately — see below)
//
// Exactly one flat counter tracks rejections (m.intentsRejected, incremented
// for every non-empty reason above) plus a rate-limited WARN naming the
// reason (docs/DEVICE_ROADMAP.md §2.6/§2.9) — internal/metrics has no label
// dimension (see cmd/northbound/certmon.go's doc for the same constraint), so
// the reason lives in the log line, not a per-reason metric series. A
// forwarding PUBLISH failure (the command was valid and accepted, but the
// LOCAL broker round trip failed) is a distinct failure mode from a
// rejection — it gets its own counter, m.intentPubFail
// (lexa_cloudlink_intent_pub_fail_total, metrics.go), mirroring
// batch.go's uplinkFail/uplinkFrames split between "accepted" and
// "delivered".
//
// Cloudlink NEVER publishes a desired doc, an engine command, or any topic
// outside bus.IntentTopic(kind) for the seven intent kinds above — see
// downlink_test.go's grep-proof test.
import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/journal"
	"lexa-hub/internal/mqttutil"
)

// cloudCmdV is the WRAPPER envelope version for the {v,kind,body} cloud
// command shape — distinct from each per-kind bus intent type's OWN
// Envelope.V, which lives inside body and follows the existing per-family
// bus.*V convention (AD-006, envelope.go). This is a brand-new v1 wire
// format with no legacy producers ever having existed, so — unlike
// bus.CheckVersion's grandfathered "v absent = legacy v0, accepted" policy
// for the pre-existing topic families — v absent/0 is rejected here exactly
// like any other out-of-range version (docs/DEVICE_ROADMAP.md §2.6's sketch
// is explicit: `env.V < 1 || env.V > cloudCmdV` rejects).
const cloudCmdV = 1

// rateLimitBurst/rateLimitRefillEvery are the §2.6 per-kind token bucket:
// burst 3, refilling 1 token per 10s. Commissioning bursts (a
// freshly-provisioned device's cloud counterpart posting several initial
// goals back to back) are expected and must not be throttled into
// rejection; a runaway or misbehaving cloud command service must still not
// be able to flood the local bus / hub adopter indefinitely.
const (
	rateLimitBurst       = 3
	rateLimitRefillEvery = 10 * time.Second
)

// expired reports whether meta's edge deadline (IssuedAt+TTLS) has already
// passed as of now — the same check cmd/hub/intent.go's applyChargeNow makes
// independently at adoption time (defense in depth: the cloud-side check
// here stops a stale chargenow before it ever reaches the bus; the hub's own
// check is the authoritative backstop regardless).
func expired(meta bus.IntentMeta, now time.Time) bool {
	return meta.IssuedAt+int64(meta.TTLS) < now.Unix()
}

// intentSpec is one of the seven intent kinds' publish policy + decoder.
type intentSpec struct {
	topic    string
	retained bool
	edge     bool // true only for chargenow — the one TTL-checked kind
	decode   func(body json.RawMessage) (msg any, meta bus.IntentMeta, err error)
}

// intentKinds is the §2.6 dispatch table: kind string → {topic, retained,
// edge, decode}. Retained/topic values mirror cmd/api/intent.go's
// localIntentKinds EXACTLY for the five kinds both services handle
// (mode/evgoal/reserve/tariff retained=true; chargenow retained=false, the
// one edge kind) and extend it with the two CLOUD-ONLY kinds api rejects
// outright (solarforecast/loadprofile — cmd/api/intent.go's
// cloudOnlyIntentKinds), both retained=true per the documented "state-like
// intents are retained" rule (internal/bus/topics.go's doc comment on the
// TopicIntent* block). Each decode closure unmarshals body directly into the
// kind's concrete bus.*Intent type, re-stamps the embedded Envelope with the
// kind's own bus version constant (exactly as cmd/api/intent.go's decode
// closures do — the forwarded message is THIS service's publish, so it
// carries this service's supported schema version, never whatever "v" the
// cloud happened to put inside body), and returns the embedded IntentMeta
// verbatim — cloudlink, unlike cmd/api, never re-stamps id/origin/actor/
// issued_at: the cloud command service is the one that mints those, and
// origin-forgery below is exactly the check that a forged/wrong Origin never
// slips through unstamped.
var intentKinds = map[string]intentSpec{
	"mode": {
		topic: bus.IntentTopic("mode"), retained: true,
		decode: func(body json.RawMessage) (any, bus.IntentMeta, error) {
			var v bus.ModeIntent
			if err := json.Unmarshal(body, &v); err != nil {
				return nil, bus.IntentMeta{}, err
			}
			v.Envelope = bus.Envelope{V: bus.ModeIntentV}
			return v, v.IntentMeta, nil
		},
	},
	"evgoal": {
		topic: bus.IntentTopic("evgoal"), retained: true,
		decode: func(body json.RawMessage) (any, bus.IntentMeta, error) {
			var v bus.EVGoalIntent
			if err := json.Unmarshal(body, &v); err != nil {
				return nil, bus.IntentMeta{}, err
			}
			v.Envelope = bus.Envelope{V: bus.EVGoalIntentV}
			return v, v.IntentMeta, nil
		},
	},
	"reserve": {
		topic: bus.IntentTopic("reserve"), retained: true,
		decode: func(body json.RawMessage) (any, bus.IntentMeta, error) {
			var v bus.BackupReserveIntent
			if err := json.Unmarshal(body, &v); err != nil {
				return nil, bus.IntentMeta{}, err
			}
			v.Envelope = bus.Envelope{V: bus.BackupReserveIntentV}
			return v, v.IntentMeta, nil
		},
	},
	"tariff": {
		topic: bus.IntentTopic("tariff"), retained: true,
		decode: func(body json.RawMessage) (any, bus.IntentMeta, error) {
			var v bus.TariffIntent
			if err := json.Unmarshal(body, &v); err != nil {
				return nil, bus.IntentMeta{}, err
			}
			v.Envelope = bus.Envelope{V: bus.TariffIntentV}
			return v, v.IntentMeta, nil
		},
	},
	"solarforecast": {
		topic: bus.IntentTopic("solarforecast"), retained: true,
		decode: func(body json.RawMessage) (any, bus.IntentMeta, error) {
			var v bus.SolarForecastIntent
			if err := json.Unmarshal(body, &v); err != nil {
				return nil, bus.IntentMeta{}, err
			}
			v.Envelope = bus.Envelope{V: bus.SolarForecastIntentV}
			return v, v.IntentMeta, nil
		},
	},
	"loadprofile": {
		topic: bus.IntentTopic("loadprofile"), retained: true,
		decode: func(body json.RawMessage) (any, bus.IntentMeta, error) {
			var v bus.LoadProfileIntent
			if err := json.Unmarshal(body, &v); err != nil {
				return nil, bus.IntentMeta{}, err
			}
			v.Envelope = bus.Envelope{V: bus.LoadProfileIntentV}
			return v, v.IntentMeta, nil
		},
	},
	"chargenow": {
		topic: bus.IntentTopic("chargenow"), retained: false, edge: true,
		decode: func(body json.RawMessage) (any, bus.IntentMeta, error) {
			var v bus.ChargeNowIntent
			if err := json.Unmarshal(body, &v); err != nil {
				return nil, bus.IntentMeta{}, err
			}
			v.Envelope = bus.Envelope{V: bus.ChargeNowIntentV}
			return v, v.IntentMeta, nil
		},
	},
}

// -----------------------------------------------------------------------------
// per-kind token bucket rate limiter.
// -----------------------------------------------------------------------------

type tokenBucket struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

func (b *tokenBucket) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed / rateLimitRefillEvery.Seconds()
		if b.tokens > rateLimitBurst {
			b.tokens = rateLimitBurst
		}
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// perKindLimiter holds one tokenBucket per kind, created lazily (a diag
// command never allocates one — it isn't in this map at all).
type perKindLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
	now     func() time.Time
}

func newPerKindLimiter(now func() time.Time) *perKindLimiter {
	return &perKindLimiter{buckets: make(map[string]*tokenBucket), now: now}
}

func (l *perKindLimiter) allow(kind string) bool {
	l.mu.Lock()
	b, ok := l.buckets[kind]
	if !ok {
		b = &tokenBucket{tokens: rateLimitBurst, last: l.now()}
		l.buckets[kind] = b
	}
	l.mu.Unlock()
	return b.allow(l.now())
}

// -----------------------------------------------------------------------------
// cloudCmdSubscriber — the seam this file needs beyond cloudPublisher.
// -----------------------------------------------------------------------------

// cloudCmdSubscriber extends cloud.go's cloudPublisher (Connected/Serial/
// PublishFrame — the batcher's seam, which has no Subscribe) with the one
// extra capability the downlink needs: a live paho Subscribe against the
// CLOUD session. It is satisfied by NEW METHODS added to cloud.go's
// *cloudMQTT and status.go's stubCloudSession below — legal without editing
// either file, since Go method declarations may live in any file of the
// same package — so this unit's file ownership (downlink.go/certmon.go/
// diag.go + bounded main.go/config.go/status.go/metrics.go edits) stays
// intact with zero lines touched in cloud.go/status.go themselves.
type cloudCmdSubscriber interface {
	cloudPublisher
	// SubscribeCmd issues a real MQTT SUBSCRIBE at QoS 1 and waits up to 10s
	// for the SUBACK. Safe to call repeatedly: runDownlink calls it again on
	// every observed (re)connect — see that function's doc for why polling
	// Connected() substitutes for a second paho OnConnect hook.
	SubscribeCmd(topic string, handler mqtt.MessageHandler) error
}

func (c *cloudMQTT) SubscribeCmd(topic string, handler mqtt.MessageHandler) error {
	tok := c.client.Subscribe(topic, 1, handler)
	if !tok.WaitTimeout(10 * time.Second) {
		return fmt.Errorf("cloudlink: subscribe %s: no suback after 10s", topic)
	}
	return tok.Error()
}

// SubscribeCmd on the disabled stub is never called in practice (main.go
// only starts runDownlink when cfg.Enabled), but is implemented so
// stubCloudSession satisfies cloudCmdSubscriber for symmetry with its other
// methods (cloud.go's compile-time proof list).
func (stubCloudSession) SubscribeCmd(string, mqtt.MessageHandler) error {
	return fmt.Errorf("cloudlink: cloud session disabled")
}

var (
	_ cloudCmdSubscriber = (*cloudMQTT)(nil)
	_ cloudCmdSubscriber = stubCloudSession{}
)

// -----------------------------------------------------------------------------
// downlink — the validation chain + dispatch.
// -----------------------------------------------------------------------------

// downlink owns the cloud→intent validation chain. One instance per process,
// constructed in main.go when cfg.Enabled.
type downlink struct {
	mc mqtt.Client // LOCAL bus client — forwarded intents publish here
	jw *journal.Writer
	m  *cloudlinkMetrics

	limiter *perKindLimiter
	diag    *diagBuilder
	diagLim *diagLimiter

	now    func() time.Time
	warnRL *rlLogger // shared rate-limited WARN logger (uplink.go's type)
}

func newDownlink(mc mqtt.Client, jw *journal.Writer, m *cloudlinkMetrics) *downlink {
	now := time.Now
	return &downlink{
		mc:      mc,
		jw:      jw,
		m:       m,
		limiter: newPerKindLimiter(now),
		diag:    newDiagBuilder(defaultDiagPaths()),
		diagLim: &diagLimiter{},
		now:     now,
		warnRL:  &rlLogger{gap: 30 * time.Second},
	}
}

// handle is the mqtt.MessageHandler wired to the cloud subscription.
func (d *downlink) handle(_ mqtt.Client, msg mqtt.Message) {
	d.process(msg.Payload())
}

// process runs the full §2.6 validation chain against payload and returns
// (kind, reason): kind is "" only for a pre-dispatch rejection (malformed/
// version/unknown-kind, before the command's kind is even resolved),
// otherwise the resolved kind (including "diag"); reason is "" on success
// (forwarded intent, or a successfully executed diag command) or the
// rejection name. All side effects (metrics, logging, journaling,
// publishing) happen here, inline — process() is the single place that
// happens, which is also what makes it directly unit-testable (call it with
// a crafted payload and assert on the returned tuple + injected fakes'
// recorded calls).
func (d *downlink) process(payload []byte) (kind, reason string) {
	var env struct {
		V    int             `json:"v"`
		Kind string          `json:"kind"`
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return "", d.reject("malformed", err)
	}
	if env.V < 1 || env.V > cloudCmdV {
		return "", d.reject("version", fmt.Errorf("v=%d not in [1,%d]", env.V, cloudCmdV))
	}

	if env.Kind == diagKind {
		return diagKind, d.processDiag(env.Body)
	}

	spec, ok := intentKinds[env.Kind]
	if !ok {
		return "", d.reject("unknown-kind", fmt.Errorf("kind=%q", env.Kind))
	}

	msgVal, meta, err := spec.decode(env.Body)
	if err != nil {
		return env.Kind, d.reject("decode", err)
	}
	if fv, ok := msgVal.(interface{ Finite() error }); ok {
		if ferr := fv.Finite(); ferr != nil {
			return env.Kind, d.reject("non-finite", ferr)
		}
	}
	if meta.Origin != "cloud" {
		return env.Kind, d.reject("origin-forgery", fmt.Errorf("origin=%q", meta.Origin))
	}
	if spec.edge && expired(meta, d.now()) {
		return env.Kind, d.reject("expired", nil)
	}
	if !d.limiter.allow(env.Kind) {
		return env.Kind, d.reject("rate-limited", nil)
	}

	// Journal BEFORE the forwarding publish — the audit trail records
	// "accepted for forwarding" independent of whether the local bus round
	// trip that follows happens to succeed (downlink_test.go pins this
	// ordering directly).
	if d.jw != nil {
		if ev, jerr := journal.NewIntentReceivedEvent("cloudlink",
			journal.NewIntentReceived(env.Kind, meta.ID, meta.Origin, meta.Actor, meta.IssuedAt)); jerr == nil {
			_ = d.jw.Append(ev)
		}
	}

	if perr := mqttutil.PublishJSONTimeout(d.mc, spec.topic, spec.retained, msgVal, 2*time.Second); perr != nil {
		d.m.intentPubFail.Inc()
		d.warnRL.warn("cloudlink: intent publish failed", "kind", env.Kind, "topic", spec.topic, "err", perr)
		return env.Kind, "publish-failed"
	}
	d.m.intentsForwarded.Inc()
	return env.Kind, ""
}

// reject increments the shared rejection counter, emits a rate-limited WARN
// naming reason, and returns reason — so every call site can
// `return kind, d.reject(reason, err)` in one line.
func (d *downlink) reject(reason string, err error) string {
	d.m.intentsRejected.Inc()
	d.warnRL.warn("cloudlink: downlink command rejected", "reason", reason, "err", err)
	return reason
}

// processDiag validates and executes a "diag" command (§2.8). Diag is NOT an
// intent: it never touches m.intentsForwarded/m.intentsRejected (those are
// specifically the seven-intent-kind counters, §2.9) and never publishes to
// the local bus. Its own request/outcome is journaled directly, reusing the
// intent_received/intent_applied/intent_rejected journal shapes with
// kind="diag" — diag flows through the exact same cloud-authenticated choke
// point and deserves the same audit trail, even though nothing here is
// forwarded onto the bus or reaches the hub.
func (d *downlink) processDiag(body json.RawMessage) string {
	var req diagRequest
	if err := json.Unmarshal(body, &req); err != nil {
		d.warnRL.warn("cloudlink: diag command rejected", "reason", "decode", "err", err)
		return "decode"
	}
	if reason, detail := validateDiagRequest(req); reason != "" {
		d.warnRL.warn("cloudlink: diag command rejected", "reason", reason, "detail", detail)
		return reason
	}

	now := d.now()
	ok, why := d.diagLim.tryStart(now)
	if !ok {
		d.warnRL.warn("cloudlink: diag command rejected", "reason", "rate-limited", "detail", why)
		return "rate-limited"
	}
	defer d.diagLim.finish()

	d.journalDiag(journal.NewIntentReceived(diagKind, "", "cloud", "", now.Unix()))

	path, n, skipped, buildErr := d.diag.build(req.Include)
	if buildErr != nil {
		detail := "build failed: " + buildErr.Error()
		d.diagOutcome("rejected", detail)
		slog.Error("lexa-cloudlink: diag bundle build failed", "err", buildErr)
		return "build-failed"
	}
	defer os.Remove(path)

	if uploadErr := d.diag.upload(req.UploadURL, path); uploadErr != nil {
		detail := "upload failed: " + uploadErr.Error()
		d.diagOutcome("rejected", detail)
		slog.Error("lexa-cloudlink: diag bundle upload failed", "err", uploadErr)
		return "upload-failed"
	}

	detail := fmt.Sprintf("uploaded bundle (%d files included, %d skipped)", n, len(skipped))
	d.diagOutcome("applied", detail)
	slog.Info("lexa-cloudlink: diag bundle uploaded", "files", n, "skipped", len(skipped))
	return ""
}

func (d *downlink) journalDiag(p journal.IntentReceived) {
	if d.jw == nil {
		return
	}
	if ev, err := journal.NewIntentReceivedEvent("cloudlink", p); err == nil {
		_ = d.jw.Append(ev)
	}
}

func (d *downlink) diagOutcome(outcome, detail string) {
	if d.jw == nil {
		return
	}
	var ev journal.Event
	var err error
	if outcome == "applied" {
		ev, err = journal.NewIntentAppliedEvent("cloudlink", journal.NewIntentApplied(diagKind, "", outcome, detail, ""))
	} else {
		ev, err = journal.NewIntentRejectedEvent("cloudlink", journal.NewIntentRejected(diagKind, "", outcome, detail, ""))
	}
	if err == nil {
		_ = d.jw.Append(ev)
	}
}

// -----------------------------------------------------------------------------
// runDownlink — subscribe on connect, and again on every reconnect.
// -----------------------------------------------------------------------------

// downlinkReconnectPoll is how often runDownlink checks for a fresh cloud
// connection to (re)subscribe on. paho has exactly one OnConnect callback
// slot (vendor/.../options.go's ClientOptions.OnConnect) and cloud.go's
// newCloudSession already claims it (the connected-log + m.connected.Set(1)
// handler); appending a second call there would require editing cloud.go,
// which is outside this unit's file ownership, and paho gives no live way
// to register a second callback after mqtt.NewClient (ClientOptions is
// copied BY VALUE at construction — options.go's NewClient — so mutating the
// original struct afterward, even if a reference were kept, has no effect).
// Polling Connected() for a false→true edge is the substitute: it re-issues
// SubscribeCmd every time the CLOUD session comes up, including a reconnect
// after a WAN blip that (with CleanSession=true, cloud.go) would otherwise
// leave the broker having silently forgotten the subscription.
//
// A var, not a const, solely so tests can shrink it (the intentResultWait
// precedent, cmd/api/intent.go) — production code has no reason to change it.
var downlinkReconnectPoll = 5 * time.Second

// runDownlink drives the cloud→intent downlink for the lifetime of ctx.
func runDownlink(ctx context.Context, cloud cloudCmdSubscriber, dl *downlink) {
	topic := fmt.Sprintf("lexa/v1/%s/cmd", cloud.Serial())

	trySubscribe := func() bool {
		if err := cloud.SubscribeCmd(topic, dl.handle); err != nil {
			slog.Error("lexa-cloudlink: cloud downlink subscribe failed — will retry", "topic", topic, "err", err)
			return false
		}
		slog.Info("lexa-cloudlink: subscribed to cloud downlink", "topic", topic)
		return true
	}

	wasConnected := false
	if cloud.Connected() {
		wasConnected = trySubscribe()
	}

	ticker := time.NewTicker(downlinkReconnectPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			nowConnected := cloud.Connected()
			if nowConnected && !wasConnected {
				wasConnected = trySubscribe()
				continue
			}
			wasConnected = nowConnected
		}
	}
}
