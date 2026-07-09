package spool

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// On-disk record framing (little machinery, big consequences — this is the
// byte format a torn write and a reopen must agree on):
//
//	[u32 len][u8 prio][u64 ts][u16 streamlen][stream][payload]
//
// len is big-endian and covers EVERYTHING after itself: prio(1) + ts(8) +
// streamlen(2) + stream + payload. The u32 length prefix is NOT part of len,
// so a record occupies 4+len bytes on disk. Big-endian throughout so a hex
// dump of a segment reads left-to-right; there is no CPU-endianness ambiguity
// across a reflash to a different SOM.
const (
	lenPrefix      = 4         // u32 length prefix (not counted inside len)
	recHeaderBytes = 1 + 8 + 2 // prio + ts + streamlen — the fixed part inside len
	maxStreamLen   = 1<<16 - 1 // stream length is a u16
	maxRecordBytes = 16 << 20  // decode sanity ceiling: a len larger than this is
	// treated as a torn/garbage prefix, never allocated.
	maxSegBytes = 256 << 10 // 256 KiB per segment file (spec)
)

// errTorn signals a truncated or corrupt final record — the on-disk signature
// of a power-cut mid-append. It is never fatal: readers stop at the tear and
// recovery truncates it away. Distinct from io.EOF (a clean segment end) and
// from a real I/O error (which propagates).
var errTorn = errors.New("spool: torn record")

// recordOnDiskSize is the exact byte count encodeRecord will emit for a record
// with the given stream/payload lengths. Kept in lockstep with encodeRecord —
// budget accounting depends on it being right without re-encoding.
func recordOnDiskSize(streamLen, payloadLen int) int64 {
	return int64(lenPrefix + recHeaderBytes + streamLen + payloadLen)
}

// encodeRecord appends the framed bytes for r to dst and returns the grown
// slice. The stored priority byte is the CLAMPED class (0..NumPriorities-1),
// matching the subdirectory the record physically lives in, so a decode round
// trip yields the same class the spool filed it under.
func encodeRecord(dst []byte, r Record) []byte {
	slen := len(r.Stream)
	plen := len(r.Payload)
	l := recHeaderBytes + slen + plen

	var hdr [lenPrefix]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(l))
	dst = append(dst, hdr[:]...)

	dst = append(dst, byte(clampPriority(r.Priority)))

	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(r.Ts))
	dst = append(dst, ts[:]...)

	var sl [2]byte
	binary.BigEndian.PutUint16(sl[:], uint16(slen))
	dst = append(dst, sl[:]...)

	dst = append(dst, r.Stream...)
	dst = append(dst, r.Payload...)
	return dst
}

// decodeRecord reads one framed record from br. It returns:
//   - (rec, size, nil)      a good record and its total on-disk byte size
//   - (_,   0,    io.EOF)   a clean segment end (zero bytes remained)
//   - (_,   0,    errTorn)  a truncated/corrupt final record (stop, tolerate)
//   - (_,   0,    err)      a real read error (propagate)
//
// A garbage length prefix (l < the fixed header, or l past maxRecordBytes) is
// reported as errTorn rather than trusted into an allocation — segments are
// append-only and never overwritten, so the only ill-formed bytes that can
// exist are a torn tail, and treating an implausible length as a tear is the
// safe reading of it.
func decodeRecord(br *bufio.Reader) (Record, int64, error) {
	var lb [lenPrefix]byte
	n, err := io.ReadFull(br, lb[:])
	if err != nil {
		if n == 0 && errors.Is(err, io.EOF) {
			return Record{}, 0, io.EOF
		}
		return Record{}, 0, errTorn // partial length prefix
	}
	l := binary.BigEndian.Uint32(lb[:])
	if l < recHeaderBytes || int64(l) > maxRecordBytes {
		return Record{}, 0, errTorn
	}
	body := make([]byte, l)
	if _, err := io.ReadFull(br, body); err != nil {
		return Record{}, 0, errTorn // body shorter than len promised
	}
	prio := int(body[0])
	ts := int64(binary.BigEndian.Uint64(body[1:9]))
	slen := int(binary.BigEndian.Uint16(body[9:11]))
	if recHeaderBytes+slen > int(l) {
		return Record{}, 0, errTorn // streamlen overruns the record
	}
	stream := string(body[recHeaderBytes : recHeaderBytes+slen])
	payload := append([]byte(nil), body[recHeaderBytes+slen:]...)
	return Record{Stream: stream, Priority: prio, Ts: ts, Payload: payload}, lenPrefix + int64(l), nil
}

// skipRecord reads and discards one record's bytes, returning its on-disk
// size. It is decodeRecord without the allocation — used by the Open-time
// recovery scan, which needs record counts and the last valid offset but not
// the payloads. Same io.EOF / errTorn contract as decodeRecord.
func skipRecord(br *bufio.Reader) (int64, error) {
	var lb [lenPrefix]byte
	n, err := io.ReadFull(br, lb[:])
	if err != nil {
		if n == 0 && errors.Is(err, io.EOF) {
			return 0, io.EOF
		}
		return 0, errTorn
	}
	l := binary.BigEndian.Uint32(lb[:])
	if l < recHeaderBytes || int64(l) > maxRecordBytes {
		return 0, errTorn
	}
	if _, err := br.Discard(int(l)); err != nil {
		return 0, errTorn
	}
	return lenPrefix + int64(l), nil
}

// ---------------------------------------------------------------------------
// Segment writer — one open, append-only file with a bufio buffer in front of
// it. Buffered writes go to the OS on flush() (readable) and to durable
// storage on sync() (fsync). This is the flash-budget seam: appends never
// fsync per record; only rotation and the 5s interval do.
// ---------------------------------------------------------------------------

type segWriter struct {
	f  *os.File
	bw *bufio.Writer
}

func openSegForAppend(path string) (*segWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &segWriter{f: f, bw: bufio.NewWriter(f)}, nil
}

func (w *segWriter) write(b []byte) error { _, err := w.bw.Write(b); return err }

// flush pushes buffered bytes to the OS page cache (making them visible to a
// fresh read handle) WITHOUT fsync. Reads for Peek/Commit call this so a
// just-appended record is peekable before the next interval fsync.
func (w *segWriter) flush() error { return w.bw.Flush() }

// sync flushes and fsyncs. syncFn is a seam so tests can count fsyncs; it is
// (*os.File).Sync in production.
func (w *segWriter) sync(syncFn func(*os.File) error) error {
	if err := w.bw.Flush(); err != nil {
		return err
	}
	return syncFn(w.f)
}

func (w *segWriter) close(syncFn func(*os.File) error) error {
	serr := w.sync(syncFn)
	cerr := w.f.Close()
	if serr != nil {
		return serr
	}
	return cerr
}

// discard drops the buffer and closes the fd without fsync — used when
// eviction reclaims a segment we were appending to (its buffered records are
// intentionally lost).
func (w *segWriter) discard() { _ = w.f.Close() }

// ---------------------------------------------------------------------------
// Segment metadata (in-memory) and the read cursor.
// ---------------------------------------------------------------------------

// segment is the in-memory record of one on-disk seg-*.log file. bytes and
// records are the logical totals written (bytes may exceed the file's fsynced
// size while buffered, never the reverse — so summing bytes bounds on-disk
// use conservatively). w is non-nil only for the newest segment while it is
// open for appending.
type segment struct {
	seq     uint64
	path    string
	bytes   int64
	records int
	w       *segWriter
}

// cursor is the durable read position within one priority class: the first
// UNcommitted record lives at (seq, off). Committed records are everything
// before it. seq identifies the segment file; off is a byte offset into it.
type cursor struct {
	seq uint64
	off int64
}

func segName(seq uint64) string { return fmt.Sprintf("seg-%016d.log", seq) }

func parseSegName(name string) (uint64, bool) {
	const pre, suf = "seg-", ".log"
	if !strings.HasPrefix(name, pre) || !strings.HasSuffix(name, suf) {
		return 0, false
	}
	v, err := strconv.ParseUint(name[len(pre):len(name)-len(suf)], 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func classDir(c int) string { return fmt.Sprintf("p%d", c) }

func clampPriority(p int) int {
	if p < 0 {
		return 0
	}
	if p >= NumPriorities {
		return NumPriorities - 1
	}
	return p
}

// ---------------------------------------------------------------------------
// Cursor file I/O — rewritten atomically via tmp+rename (journal's rotation
// discipline). A kill between the tmp write and the rename leaves the previous
// cursor intact (rename is atomic); the leftover cursor.tmp is ignored on the
// next Open and overwritten (O_TRUNC) by the next Commit.
// ---------------------------------------------------------------------------

const cursorName = "cursor"

func cursorPath(dir string) string { return filepath.Join(dir, cursorName) }

func writeCursor(dir string, c cursor, syncFn func(*os.File) error) error {
	tmp := filepath.Join(dir, cursorName+".tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(fmt.Sprintf("%d %d\n", c.seq, c.off)); err != nil {
		f.Close()
		return err
	}
	if err := syncFn(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, cursorPath(dir)); err != nil {
		return err
	}
	// Best-effort dir fsync so the rename itself is durable; a failure here is
	// not fatal (the content is already fsynced and renamed).
	if d, derr := os.Open(dir); derr == nil {
		_ = syncFn(d)
		_ = d.Close()
	}
	return nil
}

// readCursor returns the persisted cursor. A missing, unreadable, or
// unparseable cursor file returns ok=false — the caller then starts from the
// oldest record (redeliver-everything, the at-least-once-safe default), never
// silently skipping data.
func readCursor(dir string) (cursor, bool) {
	data, err := os.ReadFile(cursorPath(dir))
	if err != nil {
		return cursor{}, false
	}
	var seq uint64
	var off int64
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d %d", &seq, &off); err != nil {
		return cursor{}, false
	}
	if off < 0 {
		return cursor{}, false
	}
	return cursor{seq: seq, off: off}, true
}

// ---------------------------------------------------------------------------
// Open-time recovery scan.
// ---------------------------------------------------------------------------

// scanClass lists the seg-*.log files in one priority directory, oldest first,
// and returns their metadata plus the next sequence to allocate. The newest
// (or any) segment whose file is longer than its last valid record boundary is
// truncated back to that boundary (torn-tail recovery) and torn is set. A real
// I/O error (unreadable dir/file, failed truncate) is returned; a torn tail is
// not an error.
func scanClass(dir string, syncFn func(*os.File) error) (segs []*segment, nextSeq uint64, torn bool, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, false, err
	}
	type ref struct {
		seq  uint64
		path string
	}
	var refs []ref
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if seq, ok := parseSegName(e.Name()); ok {
			refs = append(refs, ref{seq, filepath.Join(dir, e.Name())})
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].seq < refs[j].seq })

	var maxSeq uint64
	for _, r := range refs {
		records, validLen, ferr := scanSegment(r.path)
		if ferr != nil {
			return nil, 0, false, ferr
		}
		info, serr := os.Stat(r.path)
		if serr != nil {
			return nil, 0, false, serr
		}
		if info.Size() > validLen {
			torn = true
			if terr := truncateAndSync(r.path, validLen, syncFn); terr != nil {
				return nil, 0, false, terr
			}
		}
		segs = append(segs, &segment{seq: r.seq, path: r.path, bytes: validLen, records: records})
		if r.seq > maxSeq {
			maxSeq = r.seq
		}
	}
	nextSeq = maxSeq + 1
	return segs, nextSeq, torn, nil
}

// scanSegment reads path record-by-record, counting records and returning the
// byte offset of the end of the last VALID record. Anything past validLen is a
// torn tail. Both a clean EOF and a torn record end the scan at the current
// offset (never a partial count).
func scanSegment(path string) (records int, validLen int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64<<10)
	var off int64
	for {
		sz, derr := skipRecord(br)
		if derr != nil {
			// io.EOF (clean) or errTorn (truncated tail): stop here.
			return records, off, nil
		}
		records++
		off += sz
	}
}

func truncateAndSync(path string, size int64, syncFn func(*os.File) error) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		return err
	}
	if err := syncFn(f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
