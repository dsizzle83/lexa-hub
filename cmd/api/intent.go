package main

// intent.go implements DEVICE_ROADMAP.md §4.3's POST /intent route: validate
// → publish lexa/intent/{kind} → await the hub's matching lexa/intent/result
// (resultwaiter.go) → 200 with the hub's outcome verbatim, or 202 pending on
// timeout.
//
// This is a WRITE route (requireBearerStrict — main.go wires that wrapper on;
// this file only builds the inner handler) and the security-relevant choke
// point DEVICE_ROADMAP.md §1.4 describes: the whitelist below is deliberately
// narrower than the full lexa/intent/* family — solarforecast/loadprofile
// have no local source (cloud-computed data) and are rejected outright, and
// the ACL mechanically enforces the same asymmetry (lexa-api's grants never
// include topic write on those two topics — see this task's report for the
// exact list unit 1.3 should cross-check).
import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/mqttutil"
)

// intentMaxBodyBytes bounds POST /intent's request body (DEVICE_ROADMAP.md
// §4.3: "body ... ≤64KiB via MaxBytesReader").
const intentMaxBodyBytes = 64 << 10

// intentResultWait is how long POST /intent waits for the hub's matching
// IntentResult before returning 202 {"outcome":"pending"} (DEVICE_ROADMAP.md
// §4.3: "await ... (3 s)"). A var, not a const, solely so tests can shrink it
// — production code has no reason to change it.
var intentResultWait = 3 * time.Second

// intentSpec is one whitelisted /intent kind's publish policy: which topic,
// whether the publish is retained, and how to decode+stamp the request body
// into the concrete bus wire type.
type intentSpec struct {
	topic    string
	retained bool
	// decode unmarshals body into the kind's bus.*Intent type, then
	// overwrites its embedded Envelope (stamping the kind's version
	// constant) and IntentMeta (stamping meta — server-authoritative
	// ID/Origin/Actor/IssuedAt; see intentHandler) before returning it ready
	// to publish. Only the edge kind (chargenow) inspects/validates the
	// decoded TTLS; every state kind's meta carries TTLS==0, so any
	// client-supplied ttl_s in the body is silently discarded — the
	// "accept-and-ignore for state kinds" rule DEVICE_ROADMAP.md's task
	// brief calls for.
	decode func(body json.RawMessage, meta bus.IntentMeta) (any, error)
}

// localIntentKinds whitelists the /intent kinds this LOCAL HTTP path may
// publish (DEVICE_ROADMAP.md §1.4's asymmetry: cloud-only kinds are handled
// separately below, never here).
var localIntentKinds = map[string]intentSpec{
	"mode": {
		topic:    bus.TopicIntentMode,
		retained: true,
		decode: func(body json.RawMessage, meta bus.IntentMeta) (any, error) {
			var m bus.ModeIntent
			if err := json.Unmarshal(body, &m); err != nil {
				return nil, fmt.Errorf("mode: %w", err)
			}
			m.Envelope = bus.Envelope{V: bus.ModeIntentV}
			m.IntentMeta = meta
			return m, nil
		},
	},
	"evgoal": {
		topic:    bus.TopicIntentEVGoal,
		retained: true,
		decode: func(body json.RawMessage, meta bus.IntentMeta) (any, error) {
			var g bus.EVGoalIntent
			if err := json.Unmarshal(body, &g); err != nil {
				return nil, fmt.Errorf("evgoal: %w", err)
			}
			g.Envelope = bus.Envelope{V: bus.EVGoalIntentV}
			g.IntentMeta = meta
			return g, nil
		},
	},
	"reserve": {
		topic:    bus.TopicIntentReserve,
		retained: true,
		decode: func(body json.RawMessage, meta bus.IntentMeta) (any, error) {
			var rv bus.BackupReserveIntent
			if err := json.Unmarshal(body, &rv); err != nil {
				return nil, fmt.Errorf("reserve: %w", err)
			}
			rv.Envelope = bus.Envelope{V: bus.BackupReserveIntentV}
			rv.IntentMeta = meta
			return rv, nil
		},
	},
	"tariff": {
		topic:    bus.TopicIntentTariff,
		retained: true,
		decode: func(body json.RawMessage, meta bus.IntentMeta) (any, error) {
			var tf bus.TariffIntent
			if err := json.Unmarshal(body, &tf); err != nil {
				return nil, fmt.Errorf("tariff: %w", err)
			}
			tf.Envelope = bus.Envelope{V: bus.TariffIntentV}
			tf.IntentMeta = meta
			return tf, nil
		},
	},
	"chargenow": {
		topic:    bus.TopicIntentChargeNow,
		retained: false, // edge intent (DEVICE_ROADMAP.md §1.1): not retained, mandatory TTL
		decode: func(body json.RawMessage, meta bus.IntentMeta) (any, error) {
			var c bus.ChargeNowIntent
			if err := json.Unmarshal(body, &c); err != nil {
				return nil, fmt.Errorf("chargenow: %w", err)
			}
			if c.TTLS <= 0 {
				return nil, errors.New("chargenow: ttl_s is required and must be > 0")
			}
			meta.TTLS = c.TTLS // the one kind whose client-supplied TTL is honored
			c.Envelope = bus.Envelope{V: bus.ChargeNowIntentV}
			c.IntentMeta = meta
			return c, nil
		},
	},
}

// cloudOnlyIntentKinds names kinds that exist on the bus but have no local
// source — only lexa-cloudlink ever publishes them (DEVICE_ROADMAP.md §1.4).
// Rejected with a distinct message from a genuinely unknown kind so a caller
// sees WHY, not just "bad request".
var cloudOnlyIntentKinds = map[string]bool{
	"solarforecast": true,
	"loadprofile":   true,
}

// newRandomID returns a 16-byte crypto/rand value, hex-encoded (32 hex
// chars) — used for both intent IDs and scan request IDs (scan.go). No uuid
// dependency: the house rule for this task is crypto/rand hex, and nothing
// here needs RFC 4122 structure, just a unique, unguessable token.
func newRandomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing means the kernel RNG is unavailable — treat as
		// unrecoverable-but-not-fatal for a single request: fall back to a
		// timestamp-based id rather than panic a write path. This is a
		// dedupe/correlation key, not a security token, so the degraded
		// uniqueness bound (nanosecond clock resolution) is an acceptable
		// fallback for an exceedingly rare failure mode.
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// writeJSON writes v as a JSON body with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("lexa-api: response encode failed", "err", err)
	}
}

// intentHandler serves POST /intent (DEVICE_ROADMAP.md §4.3). Callers wrap
// this in requireBearerStrict (main.go) — this function assumes it has
// already been authorized and focuses purely on the whitelist/decode/
// publish/await flow.
func intentHandler(mc mqtt.Client, waiter *resultWaiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			Kind string          `json:"kind"`
			Body json.RawMessage `json:"body"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, intentMaxBodyBytes))
		if err := dec.Decode(&req); err != nil {
			var mbErr *http.MaxBytesError
			if errors.As(err, &mbErr) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}

		if cloudOnlyIntentKinds[req.Kind] {
			http.Error(w, fmt.Sprintf("kind %q is cloud-only (no local source) and is not accepted via /intent", req.Kind), http.StatusBadRequest)
			return
		}
		spec, ok := localIntentKinds[req.Kind]
		if !ok {
			http.Error(w, fmt.Sprintf("unknown intent kind %q", req.Kind), http.StatusBadRequest)
			return
		}

		meta := bus.IntentMeta{
			ID:       newRandomID(),
			Origin:   "app",
			Actor:    "local-api",
			IssuedAt: time.Now().Unix(),
		}
		msg, err := spec.decode(req.Body, meta)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Register BEFORE publishing: a hub reply racing the publish call's
		// own return must never arrive before anyone is listening for it.
		ch := waiter.expect(meta.ID)

		var pubErr error
		if spec.retained {
			pubErr = mqttutil.PublishJSONRetained(mc, spec.topic, msg)
		} else {
			pubErr = mqttutil.PublishJSONQoS(mc, spec.topic, bus.PubQoS(spec.topic), msg)
		}
		if pubErr != nil {
			waiter.cancel(meta.ID)
			slog.Error("lexa-api: intent publish failed", "route", "/intent", "kind", req.Kind, "id", meta.ID, "err", pubErr)
			http.Error(w, "bus publish failed", http.StatusBadGateway)
			return
		}

		select {
		case res := <-ch:
			slog.Info("lexa-api: intent result", "route", "/intent", "kind", req.Kind, "id", meta.ID, "outcome", res.Outcome)
			writeJSON(w, http.StatusOK, res)
		case <-time.After(intentResultWait):
			// Cleanup only — see resultWaiter.cancel's doc for why this is
			// safe even if a delivery races this exact instant.
			waiter.cancel(meta.ID)
			slog.Info("lexa-api: intent pending", "route", "/intent", "kind", req.Kind, "id", meta.ID, "outcome", "pending")
			writeJSON(w, http.StatusAccepted, map[string]string{"id": meta.ID, "outcome": "pending"})
		}
	}
}
