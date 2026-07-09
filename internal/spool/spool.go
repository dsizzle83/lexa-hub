// Package spool is a disk-backed, byte-budgeted, priority-classed FIFO — the
// store-and-forward buffer between the local MQTT bus and the cloud uplink
// (lexa-cloudlink, DEVICE_ROADMAP.md §2.4/§2.5). It exists so a WAN outage of
// arbitrary length costs a bounded, capped amount of flash and never a lost
// safety/compliance event: telemetry is sacrificed first, events last.
//
// It is a leaf package (stdlib + internal/metrics only), like internal/metrics
// and internal/journal, and it inherits journal's flash-wear discipline.
//
// # At-least-once delivery contract (Peek / Commit)
//
// The consumer (the batcher) drives the spool with a strict Peek → publish →
// Commit loop:
//
//	recs, _ := sp.Peek(max, maxBytes) // read the oldest records of the top class; DOES NOT consume
//	if publishToCloud(recs) == ack {  // PUBACK from the cloud broker
//	    sp.Commit(len(recs))          // NOW consume them, durably
//	}
//
// Peek never advances the read cursor; Commit does, and fsyncs the advance
// before returning. A crash — or simply dropping the *Spool without Commit —
// between Peek and Commit therefore REDELIVERS those records on the next Open:
// this is at-least-once, never exactly-once. The cloud ingest deduplicates on
// its own (serial, stream, seq); the box never invents exactly-once. Committed
// records never reappear; peeked-but-uncommitted records that were durable
// always reappear.
//
// Durability window: appends are buffered and fsynced only on segment rotation
// and on a 5s interval (below), so a power cut loses at most the last <5s of
// un-fsynced appends. Those records were, by construction, never Peek'd and
// never uplinked — losing them is invisible end-to-end. The read cursor is
// never advanced past durable data (Commit fsyncs the class's segment before
// the cursor, and Open re-clamps a cursor that a lost page-cache write left
// pointing past the file), so a torn tail can never make a committed offset
// point into garbage.
//
// # Priority classes and FIFO order
//
// Records carry a Priority (0 = highest). Each class is its own on-disk FIFO
// under a subdirectory (p0/ p1/ p2/); NumPriorities classes total. Peek always
// returns the oldest records of the HIGHEST-priority non-empty class, so P0
// (events) fully drains before any P1, which drains before any P2 (telemetry).
// Peek spans segment boundaries within one class; it never crosses classes
// (the caller loops). Order within a class is strict append order.
//
// # Eviction policy (drop-oldest, lowest-priority-first)
//
// Total on-disk record bytes across all classes stay <= maxBytes. When an
// Append would exceed the budget, whole oldest SEGMENTS of the lowest-priority
// non-empty class are evicted first (a segment, not a record, is the eviction
// unit — cheap, fsync-free, and it bounds churn). A class is never evicted to
// make room for a strictly-lower-priority append: appending a P0 event evicts
// P2 then P1 before ever touching P0; appending P1 evicts P2 then its own P1
// oldest but never P0. Only when the appending class is the sole non-empty
// class does it evict its own oldest. Thus P0 events are dropped only when P0
// alone exceeds the entire budget — counted and surfaced via Metrics.Drops.
// Symmetrically, a lower-priority append that cannot fit without displacing a
// higher-priority class is itself dropped (counted), never stored at the cost
// of more-important data.
//
// The budget bounds seg-*.log data bytes. The tiny fixed-size cursor files
// (one per class, <=32 B) are recovery metadata outside the data budget.
//
// # On-disk layout
//
//	dir/
//	  p0/ seg-<seq>.log …   cursor        (highest priority)
//	  p1/ seg-<seq>.log …   cursor
//	  p2/ seg-<seq>.log …   cursor        (lowest priority)
//
// Segment files are <=256 KiB, records length-prefixed (see segment.go).
// seq is a per-class monotonic uint64, zero-padded so lexical == numeric order.
// The cursor file is rewritten atomically via tmp+rename.
//
// # Crash recovery (Open)
//
//   - Torn final record: a segment longer than its last valid record boundary
//     (power cut mid-append) is truncated back to that boundary and logged once.
//   - Cursor atomicity: a partial cursor.tmp is ignored; an unreadable/garbage
//     cursor falls back to redelivering from the oldest record (never skips).
//   - Consumed cleanup: segments fully before the cursor (a crash between the
//     cursor fsync and the segment unlink) are deleted; a cursor left pointing
//     past the file after a lost page-cache write is clamped to the file end.
//
// # Concurrency
//
// One mutex guards all state; every method takes it. There are NO background
// goroutines: the 5s fsync boundary is a lazy wall-clock check on the caller's
// Append goroutine (journal's pattern). Safe for concurrent use — the batcher
// Peeks/Commits on one goroutine while collectors Append on others.
//
// Nothing here ever panics on an I/O fault (AD-011): errors are returned,
// counted (Metrics.Errors), and edge-logged; the caller decides.
package spool

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"lexa-hub/internal/metrics"
)

// NumPriorities is the number of priority classes (subdirectories p0..p{N-1}).
// A Record.Priority outside [0, NumPriorities-1] is clamped into range.
const NumPriorities = 3

// flushInterval bounds how stale a buffered (un-fsynced) append may be: the
// caller-goroutine lazy check fsyncs active segments at most once per interval.
const flushInterval = 5 * time.Second

// Record is one spooled item. Priority 0 is highest; values outside
// [0, NumPriorities-1] are clamped. Payload is copied on Append and again on
// Peek — a returned Record shares no backing array with spool internals.
type Record struct {
	Stream   string // logical stream ("events" | "health" | "plan" | "telemetry")
	Priority int    // 0 = highest
	Ts       int64  // arrival Unix time stamped by the producer
	Payload  []byte
}

// Metrics is an optional, fully nil-safe counter/gauge set (mirroring
// journal.Metrics): every field is a *metrics.Counter/*metrics.Gauge, all of
// whose methods no-op on a nil receiver, so an unset Metrics — or an unset
// field within it — costs nothing and needs no call-site guard. Open replaces
// a nil *Metrics with an empty one so internal call sites are unconditional.
type Metrics struct {
	Bytes     *metrics.Gauge   // current on-disk data bytes (set after each mutation)
	Appends   *metrics.Counter // records successfully appended
	Commits   *metrics.Counter // records committed (consumed)
	Drops     *metrics.Counter // records dropped by eviction or over-budget rejection
	DropBytes *metrics.Counter // bytes dropped by eviction
	Errors    *metrics.Counter // I/O failures (write/rotate/sync/cursor)
}

// class is one priority level's on-disk FIFO state.
type class struct {
	dir     string
	segs    []*segment // oldest -> newest; the last may be open for append (w != nil)
	cursor  cursor     // first uncommitted record
	nextSeq uint64
}

// Spool is a disk-backed priority FIFO. Construct with Open; safe for
// concurrent use.
type Spool struct {
	mu       sync.Mutex
	dir      string
	maxBytes int64

	classes    [NumPriorities]class
	totalBytes int64 // sum of every segment.bytes across all classes

	// segCap is the per-segment rotation threshold, defaulted to the 256 KiB
	// maxSegBytes production constant in Open. It is a field, not the constant,
	// only so same-package tests can shrink it to exercise rotation/GC/eviction
	// across segment boundaries without writing megabytes; production never
	// changes it.
	segCap int64

	lastPeekClass int // class the most recent Peek read from; Commit consumes from it

	dirty    bool      // buffered appends exist that no fsync has covered
	lastSync time.Time // last interval fsync (or Open)

	healthy bool
	m       *Metrics

	// Seams: defaulted in Open, overridable by same-package tests.
	now    func() time.Time
	syncFn func(*os.File) error
}

// Open prepares dir as a spool with the given byte budget. It creates dir and
// the per-priority subdirectories, recovers any torn tails and stale cursors
// from a prior crash, and probes writability. maxBytes must be > 0. A nil
// Metrics is accepted (no-op). Open never starts a goroutine.
func Open(dir string, maxBytes int64, m *Metrics) (*Spool, error) {
	if dir == "" {
		return nil, errors.New("spool: dir is required")
	}
	if maxBytes <= 0 {
		return nil, fmt.Errorf("spool: maxBytes must be > 0, got %d", maxBytes)
	}
	if m == nil {
		m = &Metrics{}
	}
	s := &Spool{
		dir:           dir,
		maxBytes:      maxBytes,
		m:             m,
		segCap:        maxSegBytes,
		lastPeekClass: -1,
		healthy:       true,
		now:           time.Now,
		syncFn:        (*os.File).Sync,
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("spool: mkdir %s: %w", dir, err)
	}

	tornAny := false
	for c := 0; c < NumPriorities; c++ {
		cd := filepath.Join(dir, classDir(c))
		if err := os.MkdirAll(cd, 0o755); err != nil {
			return nil, fmt.Errorf("spool: mkdir %s: %w", cd, err)
		}
		s.classes[c].dir = cd
		if err := s.recoverClass(c, &tornAny); err != nil {
			return nil, err
		}
	}
	if tornAny {
		slog.Warn("spool: truncated torn final record(s) on open", "dir", dir)
	}

	s.lastSync = s.now()
	if err := s.probe(); err != nil {
		s.healthy = false
		slog.Error("spool: directory not writable at open", "dir", dir, "err", err)
	}
	s.m.Bytes.Set(float64(s.totalBytes))
	return s, nil
}

// recoverClass scans one class directory, applies torn-tail recovery, resolves
// the cursor (persisted, else oldest), deletes already-consumed segments, and
// folds the surviving bytes into the running total. Must hold no lock (Open is
// single-threaded before returning the Spool).
func (s *Spool) recoverClass(c int, tornAny *bool) error {
	cl := &s.classes[c]
	segs, nextSeq, torn, err := scanClass(cl.dir, s.syncFn)
	if err != nil {
		return fmt.Errorf("spool: scan %s: %w", cl.dir, err)
	}
	if torn {
		*tornAny = true
	}
	cl.nextSeq = nextSeq

	cur, ok := readCursor(cl.dir)
	if !ok {
		// No/garbage cursor: redeliver everything from the oldest record.
		if len(segs) > 0 {
			cur = cursor{seq: segs[0].seq, off: 0}
		}
	}

	// Drop segments fully before the cursor (consumed; a crash may have left
	// them after the cursor fsync but before the unlink).
	kept := segs[:0]
	for _, seg := range segs {
		if seg.seq < cur.seq {
			_ = os.Remove(seg.path)
			continue
		}
		kept = append(kept, seg)
		s.totalBytes += seg.bytes
	}
	cl.segs = kept

	// Reconcile the cursor against what actually survived.
	cl.cursor = normalizeCursor(cur, cl.segs)
	return nil
}

// normalizeCursor clamps a loaded cursor to the surviving segments: an offset
// past its segment's end is clamped; a seq below the oldest survivor rewinds to
// the oldest (redeliver); a seq past the newest parks at the newest's end
// (drained). Any inconsistency resolves toward redelivering, never skipping.
func normalizeCursor(cur cursor, segs []*segment) cursor {
	if len(segs) == 0 {
		return cursor{}
	}
	oldest, newest := segs[0], segs[len(segs)-1]
	if cur.seq < oldest.seq {
		return cursor{seq: oldest.seq, off: 0}
	}
	for _, seg := range segs {
		if seg.seq == cur.seq {
			if cur.off > seg.bytes {
				cur.off = seg.bytes
			}
			return cur
		}
	}
	if cur.seq > newest.seq {
		return cursor{seq: newest.seq, off: newest.bytes} // fully drained
	}
	// Gap (a middle seq missing): rewind to the oldest survivor.
	return cursor{seq: oldest.seq, off: 0}
}

// Append adds r to its priority class's FIFO. If the record would push on-disk
// bytes over the budget, whole oldest segments of the lowest-priority evictable
// class are dropped first (see the package doc's eviction policy). Append never
// fsyncs per record — durability is amortized onto rotation and the 5s interval,
// checked lazily here. A record larger than the whole budget is rejected.
func (s *Spool) Append(r Record) error {
	c := clampPriority(r.Priority)
	if len(r.Stream) > maxStreamLen {
		s.m.Drops.Inc()
		return fmt.Errorf("spool: stream too long: %d > %d", len(r.Stream), maxStreamLen)
	}
	recBytes := recordOnDiskSize(len(r.Stream), len(r.Payload))
	if recBytes > s.maxBytes {
		s.m.Drops.Inc()
		return fmt.Errorf("spool: record %d B exceeds budget %d B", recBytes, s.maxBytes)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for s.totalBytes+recBytes > s.maxBytes {
		v := s.victimClass(c)
		if v < 0 || !s.evictOldest(v) {
			break
		}
	}
	// If room still cannot be made without evicting a HIGHER-priority class
	// (victimClass refuses to), the incoming record is the least important
	// thing in the spool: drop it rather than exceed the budget or sacrifice
	// more-important data. This is the symmetric counterpart of "P0 is only
	// dropped when P0 alone exceeds the budget" — a P2 append never displaces
	// P1/P0. Counted like any other drop; not an error (fire-and-forget).
	if s.totalBytes+recBytes > s.maxBytes {
		s.m.Drops.Inc()
		s.m.DropBytes.Add(uint64(recBytes))
		return nil
	}

	seg, err := s.ensureActive(c)
	if err != nil {
		return s.fail(fmt.Errorf("spool: ensure active: %w", err))
	}
	if seg.bytes > 0 && seg.bytes+recBytes > s.segCap {
		if err := s.rotate(seg); err != nil {
			return s.fail(fmt.Errorf("spool: rotate: %w", err))
		}
		if seg, err = s.newSegment(c); err != nil {
			return s.fail(fmt.Errorf("spool: new segment: %w", err))
		}
	}

	buf := encodeRecord(make([]byte, 0, recBytes), r)
	if err := seg.w.write(buf); err != nil {
		return s.fail(fmt.Errorf("spool: write: %w", err))
	}
	seg.bytes += recBytes
	seg.records++
	s.totalBytes += recBytes
	s.dirty = true

	if err := s.maybeSync(); err != nil {
		return s.fail(fmt.Errorf("spool: sync: %w", err))
	}

	s.m.Appends.Inc()
	s.m.Bytes.Set(float64(s.totalBytes))
	s.recovered()
	return nil
}

// Peek returns up to max records / maxBytes of on-disk record size from the
// highest-priority non-empty class, oldest first, WITHOUT consuming them.
// max <= 0 means no count limit; maxBytes <= 0 means no byte limit. At least
// one record is returned when the class is non-empty, even if it alone exceeds
// maxBytes (so a large record can never wedge the drain).
func (s *Spool) Peek(max int, maxBytes int) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c := s.topClass()
	if c < 0 {
		return nil, nil
	}
	s.lastPeekClass = c

	var recs []Record
	var acc int64
	err := s.walkClass(c, func(rec Record, sz int64, _ int, _ int64) bool {
		if max > 0 && len(recs) >= max {
			return false
		}
		if maxBytes > 0 && len(recs) > 0 && acc+sz > int64(maxBytes) {
			return false
		}
		recs = append(recs, rec)
		acc += sz
		return true
	})
	if err != nil {
		return nil, s.fail(fmt.Errorf("spool: peek: %w", err))
	}
	return recs, nil
}

// Commit durably consumes the n oldest records of the class the most recent
// Peek read from (falling back to the current highest-priority class if no Peek
// preceded it). n is clamped to what is actually available. The cursor advance
// is fsynced before Commit returns; fully-consumed segments are then deleted.
func (s *Spool) Commit(n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n <= 0 {
		return nil
	}

	c := s.lastPeekClass
	if c < 0 || s.classes[c].pendingBytes() == 0 {
		c = s.topClass()
	}
	if c < 0 {
		return nil
	}
	cl := &s.classes[c]

	walked := 0
	endIdx := -1
	var endOff int64
	err := s.walkClass(c, func(_ Record, _ int64, i int, off int64) bool {
		walked++
		endIdx, endOff = i, off
		return walked < n
	})
	if err != nil {
		return s.fail(fmt.Errorf("spool: commit walk: %w", err))
	}
	if walked == 0 {
		return nil
	}

	newSeq, newOff := cl.segs[endIdx].seq, endOff
	// Normalize a boundary landing to the next segment's start so the drained
	// segment becomes deletable below.
	if newOff >= cl.segs[endIdx].bytes && endIdx+1 < len(cl.segs) {
		newSeq, newOff = cl.segs[endIdx+1].seq, 0
	}

	// Make the class's data durable up to the new cursor, then persist the
	// cursor, so the committed offset can never point past durable bytes.
	if err := s.syncClassActive(c); err != nil {
		return s.fail(fmt.Errorf("spool: commit sync: %w", err))
	}
	cl.cursor = cursor{seq: newSeq, off: newOff}
	if err := writeCursor(cl.dir, cl.cursor, s.syncFn); err != nil {
		return s.fail(fmt.Errorf("spool: write cursor: %w", err))
	}
	s.gcConsumed(c)

	s.m.Commits.Add(uint64(walked))
	s.m.Bytes.Set(float64(s.totalBytes))
	s.recovered()
	return nil
}

// Bytes reports current on-disk record bytes (excludes cursor metadata).
func (s *Spool) Bytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.totalBytes
}

// OldestTs returns the Ts of the oldest uncommitted record across all classes,
// or 0 when the spool is empty.
func (s *Spool) OldestTs() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var oldest int64
	found := false
	for c := 0; c < NumPriorities; c++ {
		if s.classes[c].pendingBytes() == 0 {
			continue
		}
		var ts int64
		got := false
		if err := s.walkClass(c, func(rec Record, _ int64, _ int, _ int64) bool {
			ts, got = rec.Ts, true
			return false
		}); err != nil {
			continue // best-effort for a status read
		}
		if got && (!found || ts < oldest) {
			oldest, found = ts, true
		}
	}
	return oldest
}

// Healthy reports whether the spool directory is writable (per the Open probe
// and subsequent operation outcomes) AND its byte accounting is self-consistent.
func (s *Spool) Healthy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.healthy && s.accountingOK()
}

// Close fsyncs and closes every open segment and persists each class cursor so
// a clean shutdown reopens exactly where it left off. Returns the first error
// encountered but always attempts every class.
func (s *Spool) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for c := 0; c < NumPriorities; c++ {
		cl := &s.classes[c]
		for _, seg := range cl.segs {
			if seg.w != nil {
				note(seg.w.close(s.syncFn))
				seg.w = nil
			}
		}
		if len(cl.segs) > 0 {
			note(writeCursor(cl.dir, cl.cursor, s.syncFn))
		}
	}
	return firstErr
}
