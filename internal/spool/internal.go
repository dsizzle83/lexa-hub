package spool

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

// This file holds Spool's unexported machinery. Every function here is called
// with s.mu already held (Open being the one exception — it runs before the
// Spool is shared). Splitting it out keeps spool.go focused on the public
// contract and the doc header.

// topClass returns the highest-priority class with pending records, or -1.
func (s *Spool) topClass() int {
	for c := 0; c < NumPriorities; c++ {
		if s.classes[c].pendingBytes() > 0 {
			return c
		}
	}
	return -1
}

// pendingBytes is the on-disk byte size of a class's uncommitted records.
func (cl *class) pendingBytes() int64 {
	var sum int64
	for _, seg := range cl.segs {
		switch {
		case seg.seq < cl.cursor.seq: // consumed, awaiting gc
		case seg.seq == cl.cursor.seq:
			sum += seg.bytes - cl.cursor.off
		default:
			sum += seg.bytes
		}
	}
	if sum < 0 {
		sum = 0
	}
	return sum
}

// victimClass picks the eviction target for an append to appendClass: the
// lowest-priority (highest-index) non-empty class that is NOT higher priority
// than the appending class. Returns -1 when nothing is evictable.
func (s *Spool) victimClass(appendClass int) int {
	for idx := NumPriorities - 1; idx >= appendClass; idx-- {
		if len(s.classes[idx].segs) > 0 {
			return idx
		}
	}
	return -1
}

// evictOldest drops the oldest whole segment of class v (its records are lost;
// counted). Returns false when the class has no segment to evict.
func (s *Spool) evictOldest(v int) bool {
	cl := &s.classes[v]
	if len(cl.segs) == 0 {
		return false
	}
	seg := cl.segs[0]
	if seg.w != nil {
		seg.w.discard() // drop buffered records with the segment
		seg.w = nil
	}
	_ = os.Remove(seg.path)
	s.totalBytes -= seg.bytes
	s.m.Drops.Add(uint64(seg.records))
	s.m.DropBytes.Add(uint64(seg.bytes))

	// If the read cursor sat inside the evicted segment, its remaining records
	// are gone; move it to the next survivor (or reset when the class emptied —
	// a later append + walkClass's oldest fallback re-establish it).
	if cl.cursor.seq == seg.seq {
		if len(cl.segs) > 1 {
			cl.cursor = cursor{seq: cl.segs[1].seq, off: 0}
		} else {
			cl.cursor = cursor{}
		}
	}
	cl.segs = cl.segs[1:]
	s.m.Bytes.Set(float64(s.totalBytes))
	return true
}

// ensureActive returns the class's writable newest segment, reopening the last
// segment for append if it still has room, else creating a fresh one.
func (s *Spool) ensureActive(c int) (*segment, error) {
	cl := &s.classes[c]
	if len(cl.segs) > 0 {
		last := cl.segs[len(cl.segs)-1]
		if last.w != nil {
			return last, nil
		}
		if last.bytes < s.segCap {
			w, err := openSegForAppend(last.path)
			if err != nil {
				return nil, err
			}
			last.w = w
			return last, nil
		}
	}
	return s.newSegment(c)
}

func (s *Spool) newSegment(c int) (*segment, error) {
	cl := &s.classes[c]
	seq := cl.nextSeq
	cl.nextSeq++
	path := filepath.Join(cl.dir, segName(seq))
	w, err := openSegForAppend(path)
	if err != nil {
		return nil, err
	}
	seg := &segment{seq: seq, path: path, w: w}
	cl.segs = append(cl.segs, seg)
	return seg, nil
}

// rotate fsyncs and closes a full segment (the fsync-on-close half of the
// flash policy). The segment stays in the list, now read-only.
func (s *Spool) rotate(seg *segment) error {
	if seg.w != nil {
		if err := seg.w.close(s.syncFn); err != nil {
			return err
		}
		seg.w = nil
	}
	return nil
}

// gcConsumed deletes segments entirely before the class cursor and reclaims
// their bytes. These are committed, not evicted — no drop counters move.
func (s *Spool) gcConsumed(c int) {
	cl := &s.classes[c]
	kept := cl.segs[:0]
	for _, seg := range cl.segs {
		if seg.seq < cl.cursor.seq {
			if seg.w != nil {
				seg.w.discard()
				seg.w = nil
			}
			_ = os.Remove(seg.path)
			s.totalBytes -= seg.bytes
			continue
		}
		kept = append(kept, seg)
	}
	cl.segs = kept
}

// walkClass reads a class's records oldest-first from its cursor, invoking fn
// per record with (record, on-disk size, segment index, offset-after-record).
// fn returns false to stop. Active segments are flushed (not fsynced) so
// just-appended records are visible. A cursor whose segment no longer exists
// falls back to the oldest survivor (redeliver-safe).
func (s *Spool) walkClass(c int, fn func(Record, int64, int, int64) bool) error {
	cl := &s.classes[c]
	if len(cl.segs) == 0 {
		return nil
	}
	startIdx, startOff := 0, int64(0)
	for i, seg := range cl.segs {
		if seg.seq == cl.cursor.seq {
			startIdx, startOff = i, cl.cursor.off
			break
		}
	}
	for i := startIdx; i < len(cl.segs); i++ {
		seg := cl.segs[i]
		if seg.w != nil {
			if err := seg.w.flush(); err != nil {
				return err
			}
		}
		f, err := os.Open(seg.path)
		if err != nil {
			return err
		}
		off := int64(0)
		if i == startIdx {
			off = startOff
			if off > 0 {
				if _, err := f.Seek(off, io.SeekStart); err != nil {
					f.Close()
					return err
				}
			}
		}
		br := bufio.NewReaderSize(f, 64<<10)
		for {
			rec, sz, derr := decodeRecord(br)
			if derr != nil {
				f.Close()
				if errors.Is(derr, io.EOF) || errors.Is(derr, errTorn) {
					break // end of this segment; advance to the next
				}
				return derr
			}
			off += sz
			if !fn(rec, sz, i, off) {
				f.Close()
				return nil
			}
		}
	}
	return nil
}

// maybeSync fsyncs active segments when buffered data has aged past
// flushInterval — the lazy, caller-goroutine half of the flash policy.
func (s *Spool) maybeSync() error {
	if !s.dirty || s.now().Sub(s.lastSync) < flushInterval {
		return nil
	}
	if err := s.syncAllActive(); err != nil {
		return err
	}
	s.dirty = false
	s.lastSync = s.now()
	return nil
}

func (s *Spool) syncAllActive() error {
	var firstErr error
	for c := 0; c < NumPriorities; c++ {
		if err := s.syncClassActive(c); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (s *Spool) syncClassActive(c int) error {
	for _, seg := range s.classes[c].segs {
		if seg.w != nil {
			if err := seg.w.sync(s.syncFn); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Spool) probe() error {
	p := filepath.Join(s.dir, ".probe")
	if err := os.WriteFile(p, []byte("ok"), 0o644); err != nil {
		return err
	}
	return os.Remove(p)
}

// accountingOK checks the running byte total is non-negative, within budget,
// and equal to the sum of the tracked segment sizes.
func (s *Spool) accountingOK() bool {
	if s.totalBytes < 0 || s.totalBytes > s.maxBytes {
		return false
	}
	var sum int64
	for c := 0; c < NumPriorities; c++ {
		for _, seg := range s.classes[c].segs {
			sum += seg.bytes
		}
	}
	return sum == s.totalBytes
}

// fail counts an I/O error, edge-marks the spool unhealthy (log once on onset),
// and returns err unchanged so call sites can `return s.fail(err)`.
func (s *Spool) fail(err error) error {
	s.m.Errors.Inc()
	if s.healthy {
		s.healthy = false
		slog.Error("spool: I/O failing", "dir", s.dir, "err", err)
	}
	return err
}

// recovered clears the unhealthy edge after a successful operation, logging the
// recovery once.
func (s *Spool) recovered() {
	if !s.healthy {
		s.healthy = true
		slog.Info("spool: I/O recovered", "dir", s.dir)
	}
}
