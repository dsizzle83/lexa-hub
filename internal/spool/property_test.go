package spool

import (
	"math/rand"
	"os"
	"testing"
)

// The property tests exercise random append/peek/commit/reopen interleavings
// against a reference model and assert the spool's core guarantees. Seeds are
// fixed so a failure reproduces exactly (no new deps, hand-rolled PRNG driver).

func reopenProp(t *testing.T, dir string, maxBytes, segCap int64) *Spool {
	t.Helper()
	s := openTest(t, dir, maxBytes)
	s.segCap = segCap
	// Skip real fsync on the property path: these tests reopen thousands of
	// times and assert LOGICAL correctness (order, budget, redelivery), which
	// depends only on data reaching the file via bufio.Flush — never on flash
	// durability. No property test simulates a power cut (the torn-record and
	// cursor-atomicity tests, which do, use the default real Sync). This keeps
	// -race -count=2 fast without weakening what is checked.
	s.syncFn = func(*os.File) error { return nil }
	return s
}

// TestProperty_NoEviction runs with a budget large enough that eviction never
// fires, so an EXACT model holds. It asserts:
//
//	(b) FIFO order within a priority class,
//	(c) higher priority always drains first,
//	(d) committed records never reappear,
//	(e) uncommitted records always reappear after reopen,
//	(a) the on-disk budget is never exceeded (trivially, plus checked).
func TestProperty_NoEviction(t *testing.T) {
	for _, seed := range []int64{1, 7, 42, 1009, 8675309} {
		t.Run("seed"+itoa(seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			dir := t.TempDir()
			const maxBytes = 8 << 20 // far above anything these ops write
			const segCap = 200       // small, to churn segments/rotation/GC
			s := reopenProp(t, dir, maxBytes, segCap)

			var pending [NumPriorities][]uint64 // model of what remains, per class, in order
			committed := map[uint64]bool{}      // tags durably consumed
			var nextTag uint64
			appended := map[uint64]bool{}

			topClass := func() int {
				for c := 0; c < NumPriorities; c++ {
					if len(pending[c]) > 0 {
						return c
					}
				}
				return -1
			}

			const ops = 400
			for i := 0; i < ops; i++ {
				switch rng.Intn(10) {
				case 0, 1, 2, 3, 4: // append (weighted)
					class := rng.Intn(NumPriorities)
					nextTag++
					if err := s.Append(rec("s", class, int64(nextTag), nextTag)); err != nil {
						t.Fatalf("Append: %v", err)
					}
					pending[class] = append(pending[class], nextTag)
					appended[nextTag] = true

				case 5, 6, 7: // peek then maybe commit
					top := topClass()
					max := rng.Intn(8) + 1
					recs, err := s.Peek(max, 0)
					if err != nil {
						t.Fatalf("Peek: %v", err)
					}
					if top < 0 {
						if len(recs) != 0 {
							t.Fatalf("Peek on empty spool returned %d records", len(recs))
						}
						continue
					}
					// (c) the peeked class must be the model's top class.
					for _, r := range recs {
						if r.Priority != top {
							t.Fatalf("Peek returned class %d, model top is %d", r.Priority, top)
						}
					}
					// (b),(d) exact front-of-class match, and none already committed.
					want := pending[top]
					if max < len(want) {
						want = want[:max]
					}
					if len(recs) != len(want) {
						t.Fatalf("Peek len = %d, want %d (class %d, max %d)", len(recs), len(want), top, max)
					}
					for j, r := range recs {
						if tagOf(r) != want[j] {
							t.Fatalf("Peek order/content mismatch at %d: got %d want %d", j, tagOf(r), want[j])
						}
						if committed[tagOf(r)] {
							t.Fatalf("committed tag %d reappeared in Peek", tagOf(r))
						}
					}
					// Commit a random prefix of what we peeked.
					n := rng.Intn(len(recs) + 1)
					if err := s.Commit(n); err != nil {
						t.Fatalf("Commit: %v", err)
					}
					for _, tag := range pending[top][:n] {
						committed[tag] = true
					}
					pending[top] = pending[top][n:]

				case 8: // reopen (clean or crash-style)
					if rng.Intn(2) == 0 {
						if err := s.Close(); err != nil {
							t.Fatalf("Close: %v", err)
						}
					} else {
						forceSync(t, s) // durable-but-uncommitted, then abandon the handle
					}
					s = reopenProp(t, dir, maxBytes, segCap)

				case 9: // budget spot-check (a)
					if got := sumSegBytes(t, dir); got > maxBytes {
						t.Fatalf("budget exceeded: %d > %d", got, maxBytes)
					}
				}

				if got := sumSegBytes(t, dir); got > maxBytes {
					t.Fatalf("op %d: budget exceeded: %d > %d", i, got, maxBytes)
				}
			}

			// (e) Drain everything left; committed ∪ drained must equal every
			// appended tag, with no overlap and no duplicates.
			final := drainAll(t, s)
			s.Close()

			seen := map[uint64]bool{}
			for _, tag := range final {
				if committed[tag] {
					t.Fatalf("committed tag %d reappeared in final drain", tag)
				}
				if seen[tag] {
					t.Fatalf("tag %d delivered twice in final drain", tag)
				}
				seen[tag] = true
				if !appended[tag] {
					t.Fatalf("final drain produced never-appended tag %d", tag)
				}
			}
			// Union count must equal all appended.
			if len(committed)+len(final) != len(appended) {
				t.Fatalf("accounting: committed %d + drained %d != appended %d",
					len(committed), len(final), len(appended))
			}
			// And no tag both committed and drained.
			for tag := range committed {
				if seen[tag] {
					t.Fatalf("tag %d both committed and drained", tag)
				}
			}
		})
	}
}

// TestProperty_WithEviction runs under heavy budget pressure so eviction fires
// constantly. The exact model no longer holds (records get dropped), so it
// asserts the invariants that survive drops:
//
//	(a) the on-disk budget is NEVER exceeded,
//	(b) FIFO: tags within any single Peek strictly ascend,
//	(c) a Peek returns exactly one priority class,
//	(d) a committed tag never reappears.
func TestProperty_WithEviction(t *testing.T) {
	for _, seed := range []int64{2, 3, 99, 12345, 271828} {
		t.Run("seed"+itoa(seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			dir := t.TempDir()
			const maxBytes = 2500
			const segCap = 200
			s := reopenProp(t, dir, maxBytes, segCap)

			committed := map[uint64]bool{}
			var nextTag uint64

			const ops = 1500
			for i := 0; i < ops; i++ {
				switch rng.Intn(10) {
				case 0, 1, 2, 3, 4, 5, 6: // append (heavy, to drive eviction)
					class := rng.Intn(NumPriorities)
					nextTag++
					// Vary payload size a little to exercise mixed record sizes.
					size := 8 + rng.Intn(24)
					p := make([]byte, size)
					// stash the tag in the first 8 bytes for identification
					for b := 0; b < 8; b++ {
						p[b] = byte(nextTag >> (8 * (7 - b)))
					}
					if err := s.Append(Record{Stream: "s", Priority: class, Ts: int64(nextTag), Payload: p}); err != nil {
						t.Fatalf("Append: %v", err)
					}

				case 7, 8: // peek + commit
					max := rng.Intn(6) + 1
					recs, err := s.Peek(max, 0)
					if err != nil {
						t.Fatalf("Peek: %v", err)
					}
					if len(recs) == 0 {
						continue
					}
					cls := recs[0].Priority
					var last uint64
					for j, r := range recs {
						if r.Priority != cls {
							t.Fatalf("Peek mixed classes: %d and %d", cls, r.Priority)
						}
						tag := uint64(0)
						for b := 0; b < 8; b++ {
							tag = tag<<8 | uint64(r.Payload[b])
						}
						if j > 0 && tag <= last {
							t.Fatalf("FIFO within class violated: %d after %d", tag, last)
						}
						last = tag
						if committed[tag] {
							t.Fatalf("committed tag %d reappeared under eviction", tag)
						}
					}
					n := rng.Intn(len(recs) + 1)
					if err := s.Commit(n); err != nil {
						t.Fatalf("Commit: %v", err)
					}
					for j := 0; j < n; j++ {
						tag := uint64(0)
						for b := 0; b < 8; b++ {
							tag = tag<<8 | uint64(recs[j].Payload[b])
						}
						committed[tag] = true
					}

				case 9: // reopen
					if err := s.Close(); err != nil {
						t.Fatalf("Close: %v", err)
					}
					s = reopenProp(t, dir, maxBytes, segCap)
				}

				if got := sumSegBytes(t, dir); got > maxBytes {
					t.Fatalf("op %d: budget exceeded: %d > %d", i, got, maxBytes)
				}
				if !s.Healthy() {
					t.Fatalf("op %d: spool reported unhealthy (accounting drift?)", i)
				}
			}
			s.Close()
		})
	}
}
