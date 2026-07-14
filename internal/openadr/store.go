package openadr

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/rand"
	"sort"
	"time"
)

// EventStore owns the poll-based event lifecycle (3.1 has no
// modificationNumber; a pull VEN reconciles by diffing each poll's event set
// against what it tracks — the digest's "UPDATE/DELETE ... + poll
// reconciliation"):
//
//   - Added:   an ID this store has never seen.
//   - Updated: a known ID whose decoded content hash changed.
//   - Deleted: a tracked ID absent from the current poll — the VTN removed
//     or cancelled it (implied cancel).
//
// randomizeStart discipline: an event whose intervalPeriod carries
// randomizeStart gets ONE random offset in [0, randomizeStart], assigned at
// first sight and kept across content updates — randomization is applied
// once per event (2.0a rule 12's spirit carried into 3.x), never re-rolled
// per poll, or the effective schedule would jitter every cycle.
//
// Not safe for concurrent use; owned by the single poll goroutine.
type EventStore struct {
	events map[string]*storedEvent
	// randFn is the randomizeStart offset source, seamed for deterministic
	// tests. Production: rand.N over [0, max].
	randFn func(max time.Duration) time.Duration
}

type storedEvent struct {
	ev         Event
	hash       string
	randOffset time.Duration
}

// NewEventStore constructs an empty store with the production random source.
func NewEventStore() *EventStore {
	return &EventStore{
		events: make(map[string]*storedEvent),
		randFn: func(max time.Duration) time.Duration {
			if max <= 0 {
				return 0
			}
			return time.Duration(rand.Int63n(int64(max) + 1))
		},
	}
}

// SetRandFn overrides the randomizeStart source (tests).
func (s *EventStore) SetRandFn(fn func(max time.Duration) time.Duration) { s.randFn = fn }

// Diff is one Reconcile call's lifecycle transitions, ID slices sorted for
// deterministic logging/tests.
type Diff struct {
	Added   []string
	Updated []string
	Deleted []string
}

// Empty reports whether the diff carries no transitions.
func (d Diff) Empty() bool {
	return len(d.Added) == 0 && len(d.Updated) == 0 && len(d.Deleted) == 0
}

// Reconcile replaces the store's view with polled and reports what changed.
// Events with an empty ID are ignored (a VTN object without identity cannot
// participate in lifecycle tracking).
func (s *EventStore) Reconcile(polled []Event) Diff {
	var d Diff
	seen := make(map[string]bool, len(polled))
	for _, ev := range polled {
		if ev.ID == "" {
			continue
		}
		seen[ev.ID] = true
		h := hashEvent(ev)
		if cur, ok := s.events[ev.ID]; ok {
			if cur.hash != h {
				// Content update: adopt new content, KEEP the randomize
				// offset (see type doc).
				cur.ev = ev
				cur.hash = h
				d.Updated = append(d.Updated, ev.ID)
			}
			continue
		}
		se := &storedEvent{ev: ev, hash: h}
		if ev.IntervalPeriod != nil && ev.IntervalPeriod.RandomizeStart != "" {
			if max, unbounded, err := ParseDuration(ev.IntervalPeriod.RandomizeStart); err == nil && !unbounded {
				se.randOffset = s.randFn(max)
			}
		}
		s.events[ev.ID] = se
		d.Added = append(d.Added, ev.ID)
	}
	for id := range s.events {
		if !seen[id] {
			d.Deleted = append(d.Deleted, id)
			delete(s.events, id)
		}
	}
	sort.Strings(d.Added)
	sort.Strings(d.Updated)
	sort.Strings(d.Deleted)
	return d
}

// Instances returns the tracked events with their effective randomizeStart
// offsets, sorted by event ID (deterministic translation output).
func (s *EventStore) Instances() []EventInstance {
	out := make([]EventInstance, 0, len(s.events))
	for _, se := range s.events {
		out = append(out, EventInstance{Event: se.ev, RandOffset: se.randOffset})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Event.ID < out[j].Event.ID })
	return out
}

// Len returns the number of tracked events.
func (s *EventStore) Len() int { return len(s.events) }

// hashEvent hashes an event's DECODED content (re-marshaled — struct field
// order is deterministic under encoding/json, unlike the VTN's own key
// order), so a byte-shuffled but semantically identical poll response does
// not read as an update. Fields this VEN does not decode do not participate;
// they also cannot change what it publishes.
func hashEvent(ev Event) string {
	b, err := json.Marshal(ev)
	if err != nil {
		// Marshal of a decoded Event cannot realistically fail; degrade to
		// "always changed" rather than panic.
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
