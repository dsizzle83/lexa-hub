package spool

import (
	"encoding/binary"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lexa-hub/internal/metrics"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

type testClock struct{ t time.Time }

func (c *testClock) now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func openTest(t *testing.T, dir string, maxBytes int64) *Spool {
	t.Helper()
	s, err := Open(dir, maxBytes, nil)
	if err != nil {
		t.Fatalf("Open(%s): %v", dir, err)
	}
	return s
}

// rec builds a record whose payload is a big-endian uint64 tag so tests can
// identify it after a round trip through disk.
func rec(stream string, prio int, ts int64, tag uint64) Record {
	p := make([]byte, 8)
	binary.BigEndian.PutUint64(p, tag)
	return Record{Stream: stream, Priority: prio, Ts: ts, Payload: p}
}

func tagOf(r Record) uint64 { return binary.BigEndian.Uint64(r.Payload) }

// sumSegBytes walks the whole spool tree and sums only seg-*.log file sizes —
// the "walk the dir, sum" budget oracle. Cursor/probe metadata is excluded by
// design (it is fixed O(1) overhead outside the data budget).
func sumSegBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasPrefix(d.Name(), "seg-") && strings.HasSuffix(d.Name(), ".log") {
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return total
}

// drainAll fully consumes the spool (highest priority first), returning the
// tags in delivery order.
func drainAll(t *testing.T, s *Spool) []uint64 {
	t.Helper()
	var out []uint64
	for {
		recs, err := s.Peek(1024, 0)
		if err != nil {
			t.Fatalf("Peek: %v", err)
		}
		if len(recs) == 0 {
			return out
		}
		for _, r := range recs {
			out = append(out, tagOf(r))
		}
		if err := s.Commit(len(recs)); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}
}

// ---------------------------------------------------------------------------
// Basic round-trip, FIFO, priority
// ---------------------------------------------------------------------------

func TestAppendPeekCommitRoundTrip(t *testing.T) {
	s := openTest(t, t.TempDir(), 1<<20)
	defer s.Close()

	for i := uint64(1); i <= 5; i++ {
		if err := s.Append(rec("telemetry", 2, int64(100+i), i)); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	recs, err := s.Peek(10, 0)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(recs) != 5 {
		t.Fatalf("Peek returned %d records, want 5", len(recs))
	}
	for i, r := range recs {
		if got, want := tagOf(r), uint64(i+1); got != want {
			t.Errorf("record %d tag = %d, want %d (FIFO order broken)", i, got, want)
		}
		if r.Stream != "telemetry" || r.Priority != 2 || r.Ts != int64(101+i) {
			t.Errorf("record %d fields wrong: %+v", i, r)
		}
	}
	// Peek must not consume: a second Peek returns the same set.
	again, _ := s.Peek(10, 0)
	if len(again) != 5 || tagOf(again[0]) != 1 {
		t.Fatalf("Peek consumed records (second peek got %d, first tag %d)", len(again), tagOf(again[0]))
	}
	if err := s.Commit(2); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	after, _ := s.Peek(10, 0)
	if len(after) != 3 || tagOf(after[0]) != 3 {
		t.Fatalf("after Commit(2): got %d records, first tag %d, want 3 records starting at 3", len(after), tagOf(after[0]))
	}
}

func TestPriorityDrainsHighestFirst(t *testing.T) {
	s := openTest(t, t.TempDir(), 1<<20)
	defer s.Close()

	// Interleave appends across all three classes.
	s.Append(rec("telemetry", 2, 1, 20))
	s.Append(rec("events", 0, 2, 1))
	s.Append(rec("health", 1, 3, 10))
	s.Append(rec("telemetry", 2, 4, 21))
	s.Append(rec("events", 0, 5, 2))
	s.Append(rec("health", 1, 6, 11))

	got := drainAll(t, s)
	want := []uint64{1, 2, 10, 11, 20, 21} // all P0, then all P1, then all P2, FIFO within each
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("drain order = %v, want %v (priority/FIFO violated)", got, want)
	}
}

func TestPeekReturnsSingleClassOnly(t *testing.T) {
	s := openTest(t, t.TempDir(), 1<<20)
	defer s.Close()
	s.Append(rec("events", 0, 1, 1))
	s.Append(rec("telemetry", 2, 2, 99))
	recs, _ := s.Peek(100, 0)
	if len(recs) != 1 || recs[0].Priority != 0 {
		t.Fatalf("Peek crossed classes: got %d records (want 1 P0 record)", len(recs))
	}
}

func TestPeekMaxCountAndMaxBytes(t *testing.T) {
	s := openTest(t, t.TempDir(), 1<<20)
	defer s.Close()
	for i := uint64(1); i <= 10; i++ {
		s.Append(rec("t", 2, int64(i), i))
	}
	// Count limit.
	if recs, _ := s.Peek(3, 0); len(recs) != 3 {
		t.Fatalf("Peek(3,0) returned %d, want 3", len(recs))
	}
	// Byte limit: one record on disk is recordOnDiskSize(1, 8).
	one := recordOnDiskSize(1, 8)
	if recs, _ := s.Peek(0, int(one*2+one/2)); len(recs) != 2 {
		t.Fatalf("Peek byte-limited returned %d, want 2", len(recs))
	}
	// A maxBytes smaller than a single record still yields exactly one (progress guarantee).
	if recs, _ := s.Peek(0, 1); len(recs) != 1 {
		t.Fatalf("Peek(0,1) returned %d, want 1 (progress guarantee)", len(recs))
	}
}

func TestPeekAcrossSegmentBoundaries(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, 1<<20)
	s.segCap = 120 // ~4 records/segment
	defer s.Close()

	for i := uint64(1); i <= 20; i++ {
		if err := s.Append(rec("t", 1, int64(i), i)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	// More than one segment must exist for this to be meaningful.
	if n := len(s.classes[1].segs); n < 2 {
		t.Fatalf("expected multiple segments, got %d", n)
	}
	recs, _ := s.Peek(1000, 0)
	if len(recs) != 20 {
		t.Fatalf("Peek across segments returned %d, want 20", len(recs))
	}
	for i, r := range recs {
		if tagOf(r) != uint64(i+1) {
			t.Fatalf("cross-segment order broken at %d: tag %d", i, tagOf(r))
		}
	}
}

func TestEmptySpoolPeekCommit(t *testing.T) {
	s := openTest(t, t.TempDir(), 1<<20)
	defer s.Close()
	if recs, err := s.Peek(10, 0); err != nil || recs != nil {
		t.Fatalf("Peek on empty = (%v,%v), want (nil,nil)", recs, err)
	}
	if err := s.Commit(5); err != nil {
		t.Fatalf("Commit on empty: %v", err)
	}
	if s.Bytes() != 0 || s.OldestTs() != 0 {
		t.Fatalf("empty spool Bytes=%d OldestTs=%d, want 0,0", s.Bytes(), s.OldestTs())
	}
	if !s.Healthy() {
		t.Fatalf("fresh spool should be Healthy")
	}
}

func TestBytesAndOldestTs(t *testing.T) {
	s := openTest(t, t.TempDir(), 1<<20)
	defer s.Close()
	s.Append(rec("t", 2, 500, 1))
	s.Append(rec("e", 0, 300, 2)) // older ts, higher priority
	s.Append(rec("h", 1, 700, 3))
	if got := s.OldestTs(); got != 300 {
		t.Fatalf("OldestTs = %d, want 300", got)
	}
	want := recordOnDiskSize(1, 8) * 3
	if got := s.Bytes(); got != want {
		t.Fatalf("Bytes = %d, want %d", got, want)
	}
	// Drain the P0 record (ts 300); oldest across the rest is now 500.
	recs, _ := s.Peek(1, 0)
	s.Commit(len(recs))
	if got := s.OldestTs(); got != 500 {
		t.Fatalf("OldestTs after draining P0 = %d, want 500", got)
	}
}

// ---------------------------------------------------------------------------
// Budget & eviction
// ---------------------------------------------------------------------------

func TestBudgetNeverExceeded(t *testing.T) {
	dir := t.TempDir()
	const maxBytes = 4000
	s := openTest(t, dir, maxBytes)
	s.segCap = 300
	defer s.Close()

	for i := uint64(1); i <= 2000; i++ {
		if err := s.Append(rec("t", 2, int64(i), i)); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
		if got := sumSegBytes(t, dir); got > maxBytes {
			t.Fatalf("on-disk seg bytes %d exceeded budget %d after append %d", got, maxBytes, i)
		}
		if s.Bytes() > maxBytes {
			t.Fatalf("Bytes() %d exceeded budget %d", s.Bytes(), maxBytes)
		}
	}
}

func TestP0AppendEvictsP2Oldest(t *testing.T) {
	dir := t.TempDir()
	const maxBytes = 2000
	reg := metrics.New()
	m := &Metrics{
		Drops:     reg.Counter("drops"),
		DropBytes: reg.Counter("drop_bytes"),
	}
	s, err := Open(dir, maxBytes, m)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s.segCap = 200
	defer s.Close()

	// Fill P2 to near the budget.
	var p2tag uint64
	for s.Bytes()+recordOnDiskSize(1, 8) <= maxBytes {
		p2tag++
		if err := s.Append(rec("t", 2, int64(p2tag), p2tag)); err != nil {
			t.Fatalf("Append p2: %v", err)
		}
	}
	oldestP2 := lowestTag(t, s, 2)

	// A P0 append must now evict P2's OLDEST segment, not touch P0.
	if err := s.Append(rec("e", 0, 9999, 1_000_000)); err != nil {
		t.Fatalf("Append p0: %v", err)
	}
	if sumSegBytes(t, dir) > maxBytes {
		t.Fatalf("budget exceeded after P0 append: %d > %d", sumSegBytes(t, dir), maxBytes)
	}
	// The P0 record is present and drains first.
	recs, _ := s.Peek(10, 0)
	if len(recs) == 0 || recs[0].Priority != 0 || tagOf(recs[0]) != 1_000_000 {
		t.Fatalf("P0 record missing after eviction: %+v", recs)
	}
	// P2's new oldest must be strictly greater than the pre-eviction oldest
	// (oldest-first eviction).
	if newOldest := lowestTag(t, s, 2); newOldest <= oldestP2 {
		t.Fatalf("P2 oldest tag = %d, want > %d (oldest-first eviction)", newOldest, oldestP2)
	}
	if dropped := counterVal(t, reg, "drops"); dropped == 0 {
		t.Fatalf("expected eviction Drops counter > 0")
	}
	if db := counterVal(t, reg, "drop_bytes"); db == 0 {
		t.Fatalf("expected eviction DropBytes counter > 0")
	}
}

func TestSelfEvictionWhenOnlyAppendingClassRemains(t *testing.T) {
	dir := t.TempDir()
	const maxBytes = 2000
	s := openTest(t, dir, maxBytes)
	s.segCap = 200
	defer s.Close()

	// Only P0 is ever used; once it fills the budget it must evict ITS OWN oldest.
	for i := uint64(1); i <= 500; i++ {
		if err := s.Append(rec("e", 0, int64(i), i)); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
		if got := sumSegBytes(t, dir); got > maxBytes {
			t.Fatalf("budget exceeded during self-eviction: %d > %d", got, maxBytes)
		}
	}
	// The newest tag must still be present; the oldest must have been evicted.
	recs, _ := s.Peek(10000, 0)
	if len(recs) == 0 {
		t.Fatal("expected surviving P0 records")
	}
	if last := tagOf(recs[len(recs)-1]); last != 500 {
		t.Fatalf("newest surviving tag = %d, want 500", last)
	}
	if first := tagOf(recs[0]); first <= 1 {
		t.Fatalf("oldest surviving tag = %d, want > 1 (self-eviction dropped the oldest)", first)
	}
	// Surviving tags must be contiguous and ascending (FIFO preserved under eviction).
	for i := 1; i < len(recs); i++ {
		if tagOf(recs[i]) != tagOf(recs[i-1])+1 {
			t.Fatalf("surviving tags not contiguous/ascending at %d: %d then %d", i, tagOf(recs[i-1]), tagOf(recs[i]))
		}
	}
}

func TestNeverEvictHigherPriorityForLowerAppend(t *testing.T) {
	dir := t.TempDir()
	const maxBytes = 2000
	s := openTest(t, dir, maxBytes)
	s.segCap = 200
	defer s.Close()

	// Seed a few P0 events that must NEVER be evicted by later P1 appends.
	for i := uint64(1); i <= 3; i++ {
		if err := s.Append(rec("e", 0, int64(i), i)); err != nil {
			t.Fatalf("Append p0: %v", err)
		}
	}
	// Flood P1 well past the budget; eviction must self-evict P1, never P0.
	for i := uint64(1); i <= 500; i++ {
		if err := s.Append(rec("h", 1, int64(i), 1000+i)); err != nil {
			t.Fatalf("Append p1: %v", err)
		}
	}
	// All three P0 events must still be there and drain first.
	recs, _ := s.Peek(100, 0)
	if len(recs) != 3 {
		t.Fatalf("P0 events count = %d, want 3 (P0 must survive P1 flood)", len(recs))
	}
	for i, r := range recs {
		if r.Priority != 0 || tagOf(r) != uint64(i+1) {
			t.Fatalf("P0 event %d corrupted: %+v", i, r)
		}
	}
}

func TestLowPriorityAppendDroppedWhenHigherPriorityFull(t *testing.T) {
	dir := t.TempDir()
	const maxBytes = 1200
	reg := metrics.New()
	m := &Metrics{Drops: reg.Counter("drops")}
	s, err := Open(dir, maxBytes, m)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s.segCap = 200
	defer s.Close()

	// Fill P0 (highest priority) to the budget.
	var p0 uint64
	for s.Bytes()+recordOnDiskSize(1, 8) <= maxBytes {
		p0++
		if err := s.Append(rec("e", 0, int64(p0), p0)); err != nil {
			t.Fatalf("Append p0: %v", err)
		}
	}
	p0Count := p0

	// A P2 append cannot evict P0 (higher priority) and must be dropped, not
	// stored at P0's expense, and must not push the spool over budget.
	if err := s.Append(rec("t", 2, 9999, 500_000)); err != nil {
		t.Fatalf("Append p2 should be a silent drop (nil), got: %v", err)
	}
	if got := sumSegBytes(t, dir); got > maxBytes {
		t.Fatalf("budget exceeded by dropped low-priority append: %d > %d", got, maxBytes)
	}
	if counterVal(t, reg, "drops") == 0 {
		t.Fatal("expected the refused low-priority append to increment Drops")
	}
	// P2 is empty; all P0 events survive intact.
	if pb := s.classes[2].pendingBytes(); pb != 0 {
		t.Fatalf("P2 should be empty (append dropped), pendingBytes=%d", pb)
	}
	recs, _ := s.Peek(10000, 0)
	if uint64(len(recs)) != p0Count || recs[0].Priority != 0 {
		t.Fatalf("P0 events not fully preserved: got %d records, want %d", len(recs), p0Count)
	}
}

func TestRecordLargerThanBudgetRejected(t *testing.T) {
	s := openTest(t, t.TempDir(), 100)
	defer s.Close()
	big := Record{Stream: "t", Priority: 2, Ts: 1, Payload: make([]byte, 200)}
	if err := s.Append(big); err == nil {
		t.Fatal("expected an error appending a record larger than the whole budget")
	}
	if s.Bytes() != 0 {
		t.Fatalf("rejected oversize record still consumed budget: Bytes=%d", s.Bytes())
	}
}

func TestStreamTooLongRejected(t *testing.T) {
	s := openTest(t, t.TempDir(), 1<<20)
	defer s.Close()
	r := Record{Stream: strings.Repeat("x", maxStreamLen+1), Priority: 0, Ts: 1}
	if err := s.Append(r); err == nil {
		t.Fatal("expected an error for a stream longer than u16")
	}
}

// ---------------------------------------------------------------------------
// Priority clamping
// ---------------------------------------------------------------------------

func TestPriorityClamped(t *testing.T) {
	s := openTest(t, t.TempDir(), 1<<20)
	defer s.Close()
	s.Append(rec("hi", -5, 1, 1)) // clamps to 0
	s.Append(rec("lo", 99, 2, 2)) // clamps to NumPriorities-1
	// Highest priority (clamped 0) drains first.
	recs, _ := s.Peek(1, 0)
	if len(recs) != 1 || recs[0].Priority != 0 || tagOf(recs[0]) != 1 {
		t.Fatalf("priority clamp low failed: %+v", recs)
	}
	// Verify the clamped-low record lives in p%(N-1).
	if _, err := os.Stat(filepath.Join(s.classes[NumPriorities-1].dir)); err != nil {
		t.Fatalf("expected lowest-priority dir populated: %v", err)
	}
}

// ---------------------------------------------------------------------------
// small helpers used above
// ---------------------------------------------------------------------------

func lowestTag(t *testing.T, s *Spool, class int) uint64 {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var got uint64
	found := false
	_ = s.walkClass(class, func(r Record, _ int64, _ int, _ int64) bool {
		got = tagOf(r)
		found = true
		return false // just the first (oldest) pending record
	})
	if !found {
		t.Fatalf("class %d has no pending records", class)
	}
	return got
}

func counterVal(t *testing.T, reg *metrics.Registry, name string) uint64 {
	t.Helper()
	out := reg.Format()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, name+" ") {
			var v uint64
			fmt.Sscanf(line, name+" %d", &v)
			return v
		}
	}
	return 0
}
