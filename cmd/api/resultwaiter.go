package main

// resultwaiter.go implements DEVICE_ROADMAP.md §4.3's resultWaiter: matches
// hub-published bus.IntentResult messages (TopicIntentResult, not retained —
// one reply per received intent) back to the HTTP request that triggered
// them, keyed by IntentMeta.ID.
import (
	"sync"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/mqttutil"
)

// resultWaiter subscribes bus.TopicIntentResult exactly ONCE for the process
// lifetime (newResultWaiter, called once from main()) — never per request,
// mirroring §1.1's "no wildcard subscription" discipline applied here to "no
// per-request subscription" as well.
//
// Goroutine/leak safety: expect allocates a channel buffered to 1, so
// onResult's send NEVER blocks even if the HTTP handler already gave up
// waiting (the 202-timeout path) — no goroutine is spawned per intent, and
// no channel send can wedge the paho callback goroutine onResult runs on.
// The sync.Map entry is removed by whichever of onResult (delivery) or
// cancel (publish failure, or timeout cleanup) runs first; the other is a
// harmless no-op (LoadAndDelete/Delete on an already-absent key).
type resultWaiter struct {
	pending sync.Map // id string -> chan bus.IntentResult
}

// newResultWaiter subscribes bus.TopicIntentResult on mc and returns a
// resultWaiter ready to have expect called against it. The subscribe error
// path is fatal to the caller, matching every other subscribe in main.go.
func newResultWaiter(mc mqtt.Client) (*resultWaiter, error) {
	w := &resultWaiter{}
	if err := mqttutil.Subscribe(mc, bus.TopicIntentResult, w.onResult); err != nil {
		return nil, err
	}
	return w, nil
}

// expect registers id as awaited and returns the channel its IntentResult
// will be delivered on. Call BEFORE publishing the intent (see intentHandler)
// so a reply racing the publish call's own return can never be missed.
func (w *resultWaiter) expect(id string) <-chan bus.IntentResult {
	ch := make(chan bus.IntentResult, 1) // buffered: onResult's send must never block
	w.pending.Store(id, ch)
	return ch
}

// cancel removes id from the pending set without requiring a delivered
// result — used when a publish fails outright (nothing will ever arrive)
// and, as cleanup, on the 202-timeout path (a result MAY still arrive
// slightly late; onResult then simply drops it, matching this codebase's
// "late reply, harmless" convention elsewhere — mqttutil's publishTimeout
// doc comment is the canonical example). Safe to call on an id already
// removed by a concurrent onResult delivery.
func (w *resultWaiter) cancel(id string) {
	w.pending.Delete(id)
}

// onResult is the bus.TopicIntentResult subscribe handler. A result whose ID
// has no registered waiter (already delivered, cancelled, or an ID this
// process never issued) is dropped silently — expected traffic, not an
// error.
func (w *resultWaiter) onResult(_ string, res bus.IntentResult) {
	v, ok := w.pending.LoadAndDelete(res.ID)
	if !ok {
		return
	}
	ch := v.(chan bus.IntentResult)
	ch <- res
}

// pendingCount is a test hook (DEVICE_ROADMAP.md §4.3's leak-proof
// requirement): asserts the map returns to empty after both the delivery and
// timeout paths.
func (w *resultWaiter) pendingCount() int {
	n := 0
	w.pending.Range(func(_, _ any) bool { n++; return true })
	return n
}
