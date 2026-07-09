package spool

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// forceSync fsyncs all open segments without advancing any cursor — a stand-in
// for "the last <5s of appends happened to reach disk" so a crash simulation
// (drop the handle without Close) still exercises durable-but-uncommitted data.
func forceSync(t *testing.T, s *Spool) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.syncAllActive(); err != nil {
		t.Fatalf("syncAllActive: %v", err)
	}
}

// newestSegPath returns the highest-seq seg file path in a class dir.
func newestSegPath(t *testing.T, classDir string) string {
	t.Helper()
	entries, err := os.ReadDir(classDir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", classDir, err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "seg-") && strings.HasSuffix(e.Name(), ".log") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		t.Fatalf("no seg files in %s", classDir)
	}
	sort.Strings(names)
	return filepath.Join(classDir, names[len(names)-1])
}

// ---------------------------------------------------------------------------
// Reopen continuity
// ---------------------------------------------------------------------------

func TestReopenPreservesUncommitted(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, 1<<20)
	for i := uint64(1); i <= 6; i++ {
		if err := s.Append(rec("t", 2, int64(i), i)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2 := openTest(t, dir, 1<<20)
	defer s2.Close()
	got := drainAll(t, s2)
	if len(got) != 6 {
		t.Fatalf("reopen lost records: got %d, want 6", len(got))
	}
	for i, tag := range got {
		if tag != uint64(i+1) {
			t.Fatalf("reopen order broken at %d: tag %d", i, tag)
		}
	}
}

func TestReopenAfterPartialCommit(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, 1<<20)
	for i := uint64(1); i <= 6; i++ {
		s.Append(rec("t", 2, int64(i), i))
	}
	recs, _ := s.Peek(4, 0)
	if err := s.Commit(len(recs)); err != nil { // commit tags 1..4
		t.Fatalf("Commit: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2 := openTest(t, dir, 1<<20)
	defer s2.Close()
	got := drainAll(t, s2)
	want := []uint64{5, 6}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("after reopen post-commit: got %v, want %v (committed must not reappear)", got, want)
	}
}

func TestReopenContinuesAppendingSegment(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, 1<<20)
	s.Append(rec("t", 2, 1, 1))
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Record the single segment's name.
	first := newestSegPath(t, filepath.Join(dir, classDir(2)))

	s2 := openTest(t, dir, 1<<20)
	defer s2.Close()
	if err := s2.Append(rec("t", 2, 2, 2)); err != nil {
		t.Fatalf("Append after reopen: %v", err)
	}
	// The append should have continued the SAME segment (had room), not spawned a new one.
	if newestSegPath(t, filepath.Join(dir, classDir(2))) != first {
		t.Fatalf("reopen spawned a new segment instead of continuing the existing one")
	}
	got := drainAll(t, s2)
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("reopen+append order = %v, want [1 2]", got)
	}
}

// ---------------------------------------------------------------------------
// Crash between Peek and Commit ⇒ redelivery (at-least-once)
// ---------------------------------------------------------------------------

func TestCrashBetweenPeekAndCommitRedelivers(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, 1<<20)
	for i := uint64(1); i <= 5; i++ {
		s.Append(rec("e", 0, int64(i), i))
	}
	forceSync(t, s) // the appends are durable...
	recs, _ := s.Peek(5, 0)
	if len(recs) != 5 {
		t.Fatalf("Peek got %d, want 5", len(recs))
	}
	// ...but we CRASH before Commit: drop the handle without Close/Commit.

	s2 := openTest(t, dir, 1<<20)
	defer s2.Close()
	got := drainAll(t, s2)
	if len(got) != 5 {
		t.Fatalf("redelivery failed: got %d records, want 5 (uncommitted must reappear)", len(got))
	}
	for i, tag := range got {
		if tag != uint64(i+1) {
			t.Fatalf("redelivered order broken at %d: tag %d", i, tag)
		}
	}
}

// ---------------------------------------------------------------------------
// Torn final record tolerated on reopen (truncate-or-skip), no corruption
// before the tear.
// ---------------------------------------------------------------------------

func TestTornFinalRecordRecovery(t *testing.T) {
	// Truncate the newest segment at a spread of byte offsets inside the final
	// record; every offset must recover the records BEFORE the tear cleanly.
	for _, chop := range []int64{1, 3, 7, 12, 20} {
		t.Run("chop"+itoa(chop), func(t *testing.T) {
			dir := t.TempDir()
			s := openTest(t, dir, 1<<20)
			s.segCap = 1 << 20 // single segment so the tear is in it
			for i := uint64(1); i <= 8; i++ {
				if err := s.Append(rec("t", 2, int64(i), i)); err != nil {
					t.Fatalf("Append: %v", err)
				}
			}
			if err := s.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			seg := newestSegPath(t, filepath.Join(dir, classDir(2)))
			info, err := os.Stat(seg)
			if err != nil {
				t.Fatalf("Stat: %v", err)
			}
			// One record on disk is recordOnDiskSize(1,8); chop into the last one.
			one := recordOnDiskSize(1, 8)
			newLen := info.Size() - chop
			if newLen <= info.Size()-one {
				t.Fatalf("chop %d too large for record size %d", chop, one)
			}
			if err := os.Truncate(seg, newLen); err != nil {
				t.Fatalf("Truncate: %v", err)
			}

			s2 := openTest(t, dir, 1<<20)
			defer s2.Close()
			got := drainAll(t, s2)
			// The 8th record was torn; records 1..7 must survive intact and in order.
			if len(got) != 7 {
				t.Fatalf("chop %d: got %d records, want 7 (torn tail dropped, rest intact)", chop, len(got))
			}
			for i, tag := range got {
				if tag != uint64(i+1) {
					t.Fatalf("chop %d: corruption before the tear at %d: tag %d", chop, i, tag)
				}
			}
			// The file must have been truncated to a clean record boundary so
			// a subsequent append does not concatenate onto garbage.
			if err := s2.Append(rec("t", 2, 99, 100)); err != nil {
				t.Fatalf("append after torn recovery: %v", err)
			}
			got2 := drainAll(t, s2)
			if len(got2) != 1 || got2[0] != 100 {
				t.Fatalf("post-recovery append not cleanly readable: %v", got2)
			}
		})
	}
}

func TestTornRecordInMiddleSegmentBoundaryClean(t *testing.T) {
	// A torn tail in the newest segment must not affect records in earlier,
	// fully-fsynced segments.
	dir := t.TempDir()
	s := openTest(t, dir, 1<<20)
	s.segCap = 120 // several records per segment, multiple segments
	for i := uint64(1); i <= 20; i++ {
		s.Append(rec("t", 2, int64(i), i))
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	seg := newestSegPath(t, filepath.Join(dir, classDir(2)))
	info, _ := os.Stat(seg)
	if err := os.Truncate(seg, info.Size()-2); err != nil { // chop the last record
		t.Fatalf("Truncate: %v", err)
	}
	s2 := openTest(t, dir, 1<<20)
	defer s2.Close()
	got := drainAll(t, s2)
	// Exactly one record (the newest) is lost to the tear; the prefix is intact.
	if len(got) != 19 {
		t.Fatalf("got %d records, want 19", len(got))
	}
	for i, tag := range got {
		if tag != uint64(i+1) {
			t.Fatalf("order broken at %d: tag %d", i, tag)
		}
	}
}

// ---------------------------------------------------------------------------
// Cursor atomicity
// ---------------------------------------------------------------------------

func TestCursorPartialTmpIgnored(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, 1<<20)
	for i := uint64(1); i <= 6; i++ {
		s.Append(rec("t", 2, int64(i), i))
	}
	recs, _ := s.Peek(4, 0)
	if err := s.Commit(len(recs)); err != nil { // durable cursor past tags 1..4
		t.Fatalf("Commit: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Simulate a kill mid-cursor-write: a partial/garbage cursor.tmp left behind,
	// the real cursor untouched. The tmp must be ignored (rename never happened).
	cd := filepath.Join(dir, classDir(2))
	if err := os.WriteFile(filepath.Join(cd, cursorName+".tmp"), []byte("99 999999"), 0o644); err != nil {
		t.Fatalf("write garbage tmp: %v", err)
	}

	s2 := openTest(t, dir, 1<<20)
	defer s2.Close()
	got := drainAll(t, s2)
	want := []uint64{5, 6}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("old committed cursor not honored: got %v, want %v", got, want)
	}
}

func TestGarbageCursorFallsBackToOldest(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, 1<<20)
	for i := uint64(1); i <= 5; i++ {
		s.Append(rec("t", 2, int64(i), i))
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Corrupt the real cursor file with unparseable content.
	cd := filepath.Join(dir, classDir(2))
	if err := os.WriteFile(cursorPath(cd), []byte("not-a-cursor\x00\xff"), 0o644); err != nil {
		t.Fatalf("corrupt cursor: %v", err)
	}
	s2 := openTest(t, dir, 1<<20)
	defer s2.Close()
	// Fail-safe: redeliver everything from the oldest rather than skip.
	got := drainAll(t, s2)
	if len(got) != 5 || got[0] != 1 {
		t.Fatalf("garbage cursor should redeliver from oldest: got %v", got)
	}
}

// ---------------------------------------------------------------------------
// Fsync policy: no fsync per Append; batched on interval and rotation.
// ---------------------------------------------------------------------------

func TestNoFsyncPerAppend(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, 1<<20)
	s.segCap = 1 << 20 // large: no rotation during this test
	clock := &testClock{t: time.Unix(1_700_000_000, 0)}
	s.now = clock.now
	s.lastSync = clock.now() // realign the interval baseline onto the frozen clock
	var syncs int
	s.syncFn = func(f *os.File) error { syncs++; return f.Sync() }

	// Many appends inside one flush interval, no rotation: zero fsyncs.
	for i := uint64(1); i <= 50; i++ {
		if err := s.Append(rec("t", 2, int64(i), i)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if syncs != 0 {
		t.Fatalf("fsync happened during buffered appends: %d (want 0)", syncs)
	}

	// Cross the 5s interval: the next append must fsync (once).
	clock.advance(flushInterval + time.Second)
	if err := s.Append(rec("t", 2, 51, 51)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if syncs == 0 {
		t.Fatalf("expected an interval fsync after %v elapsed, got 0", flushInterval)
	}
	before := syncs
	// Immediately after, still within the new interval: no further fsyncs.
	for i := uint64(52); i <= 60; i++ {
		s.Append(rec("t", 2, int64(i), i))
	}
	if syncs != before {
		t.Fatalf("extra fsyncs inside the interval: %d -> %d", before, syncs)
	}
	s.Close()
}

func TestRotationFsyncs(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, 1<<20)
	s.segCap = 120 // small, so records force rotations
	// Freeze the clock so the interval timer never fires — isolate rotation fsyncs.
	clock := &testClock{t: time.Unix(1_700_000_000, 0)}
	s.now = clock.now
	var syncs int
	s.syncFn = func(f *os.File) error { syncs++; return f.Sync() }

	for i := uint64(1); i <= 40; i++ {
		if err := s.Append(rec("t", 2, int64(i), i)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if syncs == 0 {
		t.Fatalf("expected fsyncs on segment rotation, got 0")
	}
	// Rotations are bounded: far fewer than the append count.
	if syncs >= 40 {
		t.Fatalf("fsync count %d looks like per-append syncing, not per-rotation", syncs)
	}
	s.Close()
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
