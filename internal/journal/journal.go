// Package journal is an append-only, newline-delimited-JSON (NDJSON) event
// journal for lexa-hub (AD-005): controls adopted/released, dispatches,
// breach episodes, and CannotComply POSTs, on their own size-rotated file
// set with batched fsync. Retained MQTT stays the *bus* recovery mechanism
// (AD-005/AD-013); this journal is the *record* — a utility's dispatch/
// compliance audit trail, and the raw material for a future compliance
// report generator (docs/refactor/10_BACKLOG.md, csip-tls-test repo).
//
// This is a pure library: nothing in lexa-hub imports it yet (TASK-040
// wires the first caller). It has no goroutines of its own — Append does a
// lazy time-check on the caller's goroutine, never spawns a ticker (05 §4
// ownership rules; ownership of a hard real-time flush requirement, if a
// caller ever needs one, belongs to that caller via its own ticker calling
// Flush()).
//
// # Write budget (RSK-14)
//
// This journal's quota must fit *inside* the existing journald flash
// budget, not stack a second one on top of it
// (lexa-hub/docs/FLASH_BUDGET.md, "Related budgets"). journald's own
// measured hub rate (FLASH_BUDGET.md, 2026-07-05 P0-exit measurement, FAST
// mode) is 108 lines/min ≈ 155k lines/day — but that is journald's
// per-tick operational log, which this journal deliberately does NOT
// duplicate: journal Events are transitions only (control adopted/
// released, a dispatch that actually changed post-dedupe, a breach
// begin/end edge, a CannotComply POST) — there is no per-tick "tick" event
// on purpose (05 §9).
//
// Line size: measured (TestRepresentativeLineSize, schema_test.go — pins
// these exact byte counts so a field change to any payload type shows up
// here) for representative fully-populated payloads, envelope + newline
// included: control_adopted 229 B, dispatch (one field set) 124 B,
// breach_begin/breach_end 252 B — the largest event type this package
// defines. 260 B/line is used below as a conservative round number above
// every payload type today, including future SrvT population (currently
// omitted from these samples since it is `omitempty`).
//
// Events/day, worst case (FAST, 3 s hub tick ⇒ 28,800 ticks/day):
//   - dispatch: 3 devices × 1 change/tick worst case (real traffic is far
//     below this — dispatches are journaled only on change post-dedupe, so
//     a healthy steady-state plant logs near zero of these) = 86,400/day.
//   - control_adopted/control_released: bounded by CSIP event/DefaultDERControl
//     churn; a pathological flapping-control fault-storm scenario, at most
//     1 transition/tick = 28,800/day.
//   - breach_begin/breach_end: a pathological breach-flicker scenario (the
//     Mayhem "flicker" class, csip-tls-test docs/QA_GAPS_20260701.md) at
//     worst 2 edges/tick (begin+end) = 57,600/day.
//   - cannot_comply_posted: at most one per breach_begin = 28,800/day.
//
// Summed pathological ceiling: 86,400 + 28,800 + 57,600 + 28,800 =
// 201,600 events/day (≈2.33 events/sec sustained) — a system transitioning
// every single tick on every axis simultaneously, which is itself a
// fault-storm signature, not normal operation. At 260 B/line that is
// ≈52.4 MB/day of *input* volume if nothing ever rotated it away.
//
// Total resident cap (rotation, not growth): MaxBytes × (MaxFiles+1) =
// 1 MiB × 5 = 5 MiB with the package defaults, REGARDLESS of input volume —
// rotation guarantees this ceiling always; a heavier storm just rotates the
// 5 MiB window faster (≈5 MiB / 52.4 MB/day ≈ 2.3 h retention at the
// pathological ceiling above, self-healing back to weeks once the storm
// clears) rather than the journal ever growing past 5 MiB. 5 MiB is a
// rounding error against journald's own 200 MB SystemMaxUse budget
// (FLASH_BUDGET.md) — this quota sits comfortably inside that budget rather
// than competing with it for RSK-14's flash-wear concern.
//
// fsyncs/day: batched on FlushEvery=32 events OR FlushInterval=5 s elapsed,
// whichever comes first (checked lazily on Append; no background ticker by
// default). At the pathological ceiling's ≈2.33 events/sec, 32 events
// accumulate in ≈13.7 s — slower than the 5 s time boundary — so the time
// boundary governs: at most one flush every 5 s while events are actively
// arriving, ⌈86,400 s/day ÷ 5 s⌉ = 17,280 fsyncs/day. The crossover to the
// count boundary governing instead happens at 32 events / 5 s = 6.4
// events/sec sustained, well above this system's pathological ceiling; if
// traffic is idle, no flush happens at all (nothing pending to flush).
// os.File.Sync() per Append (sync-per-write) would defeat all of this and
// is exactly what the batching exists to avoid — never "simplify" to it
// (see "Common mistakes to avoid" in TASK-039).
package journal

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"lexa-hub/internal/metrics"
)

// Defaults for Config's zero-valued fields (Config.withDefaults).
const (
	DefaultMaxBytes      = 1 << 20 // 1 MiB
	DefaultMaxFiles      = 4
	DefaultFlushEvery    = 32
	DefaultFlushInterval = 5 * time.Second
	DefaultName          = "journal.ndjson"
)

// Config configures a Writer. Zero-valued fields take the Default* constants
// above (withDefaults) except Dir, which is required.
type Config struct {
	Dir  string // directory the journal lives in; created 0755 if missing
	Name string // active file name, default DefaultName

	MaxBytes int64 // rotate when the active file would exceed this size
	MaxFiles int   // rotated files kept (name.1 .. name.MaxFiles); oldest dropped

	FlushEvery    int           // flush+fsync after this many unflushed Appends
	FlushInterval time.Duration // flush+fsync if this much time elapsed since the first unflushed Append

	// Now stubs the wall clock for tests (fsync-boundary and Ts-stamping
	// determinism). Defaults to time.Now.
	Now func() time.Time

	// Metrics is an optional hook exposing journal activity via
	// internal/metrics (TASK-044's registry). Nil is a complete no-op — every
	// field is a *metrics.Counter, which is nil-receiver-safe, so leaving
	// Metrics unset costs nothing and needs no call-site guard.
	Metrics *Metrics
}

// Metrics is the optional counter set a caller can wire into Config to make
// journal writes/rotations/errors observable via internal/metrics. Wire a
// Registry's counters in directly, e.g.:
//
//	reg := metrics.New()
//	cfg.Metrics = &journal.Metrics{
//	    Writes:    reg.Counter("lexa_hub_journal_writes_total"),
//	    Rotations: reg.Counter("lexa_hub_journal_rotations_total"),
//	    Errors:    reg.Counter("lexa_hub_journal_errors_total"),
//	    Dropped:   reg.Counter("lexa_hub_journal_dropped_total"),
//	}
type Metrics struct {
	Writes    *metrics.Counter // successful Append calls
	Rotations *metrics.Counter // rotations performed
	Errors    *metrics.Counter // write/rotate/flush failures
	Dropped   *metrics.Counter // Appends that returned an error (event not recorded)
}

func (c Config) withDefaults() Config {
	if c.Name == "" {
		c.Name = DefaultName
	}
	if c.MaxBytes <= 0 {
		c.MaxBytes = DefaultMaxBytes
	}
	if c.MaxFiles <= 0 {
		c.MaxFiles = DefaultMaxFiles
	}
	if c.FlushEvery <= 0 {
		c.FlushEvery = DefaultFlushEvery
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = DefaultFlushInterval
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// Writer owns one open journal file plus rotation/fsync-batching state.
// Safe for concurrent use from multiple goroutines (single mutex) — TASK-040
// wires both planObserver and MQTT subscription callbacks to call Append
// concurrently.
type Writer struct {
	cfg Config

	mu   sync.Mutex
	f    *os.File
	w    *bufio.Writer
	size int64
	seq  uint64

	unflushed      int
	firstUnflushed time.Time

	failing bool // edge-triggered error state: log/report only on transitions
	dropped uint64
}

// Open creates cfg.Dir (0755) if needed, opens (or creates, 0644) the active
// journal file for append, and resumes the per-writer Seq counter by
// scanning the tail of that file only (bounded by MaxBytes — rotated files
// are never re-read at startup, only the active one).
func Open(cfg Config) (*Writer, error) {
	cfg = cfg.withDefaults()
	if cfg.Dir == "" {
		return nil, errors.New("journal: Config.Dir is required")
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("journal: mkdir %s: %w", cfg.Dir, err)
	}

	w := &Writer{cfg: cfg}
	if err := w.openActive(); err != nil {
		return nil, err
	}

	seq, err := resumeSeq(filepath.Join(cfg.Dir, cfg.Name))
	if err != nil {
		// Resume is best-effort: a corrupt/unreadable active file must not
		// prevent the service from starting (crash-only ≠ crash-on-log,
		// AD-011). Worst case Seq restarts at 0, which is observable (a Seq
		// regression in the journal itself) rather than fatal.
		slog.Warn("journal: could not resume sequence from active file; starting at 0",
			"dir", cfg.Dir, "name", cfg.Name, "err", err)
	} else {
		w.seq = seq
	}
	return w, nil
}

// openActive (re)opens cfg.Dir/cfg.Name for append and syncs w.size to its
// current on-disk length. Must be called with w.mu held except from Open.
//
// If the file's existing tail does not end in a newline — the on-disk
// signature of a torn write from a prior crash (resumeSeq/reader.Scan both
// tolerate exactly this) — a newline is padded on now, before any new
// Append can happen. Without this, the next Append's bytes would land
// directly after the torn bytes with no line separator, silently
// concatenating a well-formed new event onto garbage and making Scan skip
// (and thus lose) that new event too, not just the torn one.
func (w *Writer) openActive() error {
	path := filepath.Join(w.cfg.Dir, w.cfg.Name)
	// O_RDWR, not O_WRONLY: the tail-newline check below uses f.ReadAt,
	// which a write-only fd cannot service (returns EBADF, silently
	// skipping the pad if this were O_WRONLY — caught by
	// TestScanTruncatedFinalLineTolerance).
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("journal: open %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("journal: stat %s: %w", path, err)
	}
	w.f = f
	w.w = bufio.NewWriter(f)
	w.size = info.Size()

	if w.size > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], w.size-1); err == nil && last[0] != '\n' {
			if n, werr := f.Write([]byte{'\n'}); werr == nil {
				w.size += int64(n)
			}
			// A failure to pad is not itself fatal here: the next Append
			// will still be attempted and, if the underlying disk problem
			// persists, will fail and go through the normal fail()/recovery
			// path anyway.
		}
	}
	return nil
}

// resumeSeq scans path (the active file only — a bounded read, capped at
// MaxBytes by construction since that is when rotation moves content out of
// it) for the last line that parses as an Event, returning its Seq. A
// truncated/corrupt final line (a torn write from a prior power cut) is
// tolerated exactly like reader.Scan does: skipped, not fatal.
func resumeSeq(path string) (uint64, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var last uint64
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			continue // tolerate a torn tail line; resume from the last good one
		}
		last = e.Seq
	}
	if err := sc.Err(); err != nil {
		return last, err
	}
	return last, nil
}

// Append marshals e as one NDJSON line and writes it to the active file.
// The Writer assigns e.Seq (per-writer monotonic) itself, overwriting
// anything the caller set; e.V and e.Ts default to SchemaV and
// cfg.Now().Unix() respectively if left zero. Append never blocks on fsync
// unless a flush boundary (FlushEvery/FlushInterval) is reached.
//
// On any failure (marshal, rotate, write, or a boundary-triggered flush) the
// event is considered dropped: the internal dropped-counter increments, the
// failure is logged edge-triggered (once on onset, once on recovery — never
// once per Append, which would blow the journald budget this package is
// trying not to compete with), and the error is returned. Callers must never
// crash on a journal failure (AD-011); this method never panics.
func (w *Writer) Append(e Event) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if e.V == 0 {
		e.V = SchemaV
	}
	if e.Ts == 0 {
		e.Ts = w.cfg.Now().Unix()
	}
	nextSeq := w.seq + 1
	e.Seq = nextSeq

	b, err := json.Marshal(e)
	if err != nil {
		return w.fail(fmt.Errorf("journal: marshal event: %w", err))
	}
	b = append(b, '\n')

	if err := w.ensureOpen(); err != nil {
		return w.fail(fmt.Errorf("journal: reopen after prior failure: %w", err))
	}
	if err := w.rotateIfNeeded(int64(len(b))); err != nil {
		return w.fail(fmt.Errorf("journal: rotate: %w", err))
	}

	n, werr := w.w.Write(b)
	w.size += int64(n)
	if werr != nil {
		return w.fail(fmt.Errorf("journal: write: %w", werr))
	}
	// The write only actually "happened" once we know it didn't error, so
	// only now commit the sequence advance.
	w.seq = nextSeq

	if w.unflushed == 0 {
		w.firstUnflushed = w.cfg.Now()
	}
	w.unflushed++

	due := w.unflushed >= w.cfg.FlushEvery || w.cfg.Now().Sub(w.firstUnflushed) >= w.cfg.FlushInterval
	if due {
		if err := w.flushLocked(); err != nil {
			return w.fail(fmt.Errorf("journal: flush: %w", err))
		}
	}

	w.recovered()
	if w.cfg.Metrics != nil {
		w.cfg.Metrics.Writes.Inc()
	}
	return nil
}

// ensureOpen reopens the active file if a prior failure left it closed
// (self-heal path: e.g. a rotation that failed to open a fresh file because
// the directory was briefly read-only/full). A no-op when already open.
func (w *Writer) ensureOpen() error {
	if w.f != nil {
		return nil
	}
	return w.openActive()
}

// rotateIfNeeded closes and rotates the active file when appending nextLen
// more bytes would exceed cfg.MaxBytes, then opens a fresh active file. A
// single event larger than MaxBytes is still written whole after rotating
// (never split a line) — the size cap is a rotation trigger, not a hard
// per-line refusal.
//
// Must be called with w.mu held and w.f non-nil (Append calls ensureOpen
// first).
func (w *Writer) rotateIfNeeded(nextLen int64) error {
	if w.size+nextLen <= w.cfg.MaxBytes {
		return nil
	}
	if err := w.flushLocked(); err != nil {
		return err
	}
	if err := w.f.Close(); err != nil {
		w.f, w.w = nil, nil
		return err
	}
	w.f, w.w = nil, nil

	if err := rotateFiles(w.cfg.Dir, w.cfg.Name, w.cfg.MaxFiles); err != nil {
		return err
	}
	if err := w.openActive(); err != nil {
		return err
	}
	if w.cfg.Metrics != nil {
		w.cfg.Metrics.Rotations.Inc()
	}
	return nil
}

// rotateFiles performs the rename-then-create shift chain (never copy —
// copying would double writes and break tail-readers mid-rotation):
// name.maxFiles is deleted (if present), then name.(maxFiles-1)..name.1 each
// shift up by one, then the just-closed active file becomes name.1.
func rotateFiles(dir, name string, maxFiles int) error {
	base := filepath.Join(dir, name)

	oldest := fmt.Sprintf("%s.%d", base, maxFiles)
	if err := os.Remove(oldest); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("journal: remove oldest rotation %s: %w", oldest, err)
	}
	for i := maxFiles - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", base, i)
		dst := fmt.Sprintf("%s.%d", base, i+1)
		if _, err := os.Stat(src); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("journal: stat %s: %w", src, err)
		}
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("journal: rotate %s -> %s: %w", src, dst, err)
		}
	}
	if err := os.Rename(base, base+".1"); err != nil {
		return fmt.Errorf("journal: rotate active -> %s.1: %w", base, err)
	}
	return nil
}

// flushLocked writes buffered bytes and fsyncs the active file. Must be
// called with w.mu held and w.f/w.w non-nil.
func (w *Writer) flushLocked() error {
	if err := w.w.Flush(); err != nil {
		return err
	}
	if err := w.f.Sync(); err != nil {
		return err
	}
	w.unflushed = 0
	w.firstUnflushed = time.Time{}
	return nil
}

// Flush forces a flush+fsync of any buffered, unflushed Appends outside the
// normal FlushEvery/FlushInterval lazy check. The library never calls this
// on a background timer itself (05 §4) — a caller wanting hard real-time
// flushing owns a ticker that calls Flush().
func (w *Writer) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil // nothing open, nothing to flush
	}
	if err := w.flushLocked(); err != nil {
		return w.fail(fmt.Errorf("journal: flush: %w", err))
	}
	w.recovered()
	return nil
}

// Close flushes and closes the active file. Safe to call once; a second
// Close returns the underlying os.File's own "already closed" error.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	ferr := w.flushLocked()
	cerr := w.f.Close()
	w.f, w.w = nil, nil
	if ferr != nil {
		return fmt.Errorf("journal: close: flush: %w", ferr)
	}
	if cerr != nil {
		return fmt.Errorf("journal: close: %w", cerr)
	}
	return nil
}

// Dropped returns the number of Appends that returned an error (and so were
// not recorded), for tests and any caller wanting a metrics-free readout.
func (w *Writer) Dropped() uint64 {
	return atomic.LoadUint64(&w.dropped)
}

// fail counts, alarms (metrics.Errors/Dropped if wired), and edge-triggers a
// log line for a write/rotate/flush failure, then returns err unchanged so
// every call site can `return w.fail(err)`.
func (w *Writer) fail(err error) error {
	atomic.AddUint64(&w.dropped, 1)
	if w.cfg.Metrics != nil {
		w.cfg.Metrics.Errors.Inc()
		w.cfg.Metrics.Dropped.Inc()
	}
	if !w.failing {
		w.failing = true
		slog.Error("journal: write failing", "dir", w.cfg.Dir, "name", w.cfg.Name, "err", err)
	}
	return err
}

// recovered clears the edge-triggered failure state and logs the recovery
// exactly once, the first time an operation succeeds after a failure.
func (w *Writer) recovered() {
	if w.failing {
		w.failing = false
		slog.Info("journal: write recovered", "dir", w.cfg.Dir, "name", w.cfg.Name)
	}
}
