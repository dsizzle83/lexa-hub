package main

// batch.go is unit 2.2's drain half: a single goroutine that pulls records off
// the spool (highest priority first), gzip-batches them into ≤96 KiB frames,
// and publishes each frame QoS 1 to the cloud — committing only on PUBACK, so
// delivery is at-least-once across arbitrary WAN outages and crashes.

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lexa-hub/internal/spool"
)

const (
	// maxFrameBytes is the ceiling on a PUBLISHED frame (JSON wrapper + base64
	// gz), 96 KiB — headroom under IoT Core's 128 KiB message cap (ECOSYSTEM
	// ROADMAP §5 / R9). Checked on the marshaled frame, so it accounts for the
	// base64 inflation and JSON overhead automatically.
	maxFrameBytes = 96 * 1024

	// maxPeekRecords / maxPeekBytes bound one Peek: up to 500 records or 256 KiB
	// of raw on-disk record bytes, whichever comes first. The 256 KiB raw ceiling
	// is deliberately larger than the 96 KiB compressed frame so a well-
	// compressing batch fills a frame; buildFrame splits down whatever does not fit.
	maxPeekRecords = 500
	maxPeekBytes   = 256 * 1024

	// cloudPublishTimeout is the per-frame PUBACK wait (spec: 30 s).
	cloudPublishTimeout = 30 * time.Second

	// coalesceWindow bounds how long a P0-notify wakeup waits to batch a burst
	// of events before draining (spec: ~1 s coalesce).
	coalesceWindow = 1 * time.Second

	backoffMin = 1 * time.Second
	backoffMax = 60 * time.Second
)

// uplinkFrame is the wire shape published to the cloud. GZ is a []byte, which
// encoding/json marshals as base64 — the gzip of the newline-joined record
// payloads (ndjson). The cloud dedupes on (serial, stream, seq).
type uplinkFrame struct {
	V      int    `json:"v"`
	Serial string `json:"serial"`
	Stream string `json:"stream"`
	Seq    uint64 `json:"seq"`
	Count  int    `json:"count"`
	GZ     []byte `json:"gz"`
}

// batcher drains the spool to the cloud. One goroutine (run); its only shared
// mutable state read from elsewhere is lastUplink (atomic), read by status.go.
type batcher struct {
	spool *spool.Spool
	cloud cloudPublisher
	seqs  *seqStore
	m     *cloudlinkMetrics

	notify <-chan struct{} // collectors poke this on a P0 append

	telInterval time.Duration // periodic full drain cadence (measurements_batch_s)
	coalesce    time.Duration // P0-notify coalesce window
	pubTimeout  time.Duration // per-frame PUBACK wait
	bo          backoff

	lastUplink atomic.Int64 // Unix seconds of the last PUBACK'd Commit (status.go reads it)
	now        func() time.Time
}

func newBatcher(sp *spool.Spool, cloud cloudPublisher, cfg *Config, m *cloudlinkMetrics, notify <-chan struct{}) (*batcher, error) {
	seqs, err := openSeqStore(cfg.SpoolDir)
	if err != nil {
		return nil, err
	}
	tel := time.Duration(cfg.Uplink.MeasurementsBatchS) * time.Second
	if tel <= 0 {
		tel = 60 * time.Second
	}
	return &batcher{
		spool:       sp,
		cloud:       cloud,
		seqs:        seqs,
		m:           m,
		notify:      notify,
		telInterval: tel,
		coalesce:    coalesceWindow,
		pubTimeout:  cloudPublishTimeout,
		bo:          backoff{cur: backoffMin, min: backoffMin, max: backoffMax},
		now:         time.Now,
	}, nil
}

// LastUplinkTs reports the Unix time of the last successfully committed frame
// (0 if none this process). status.go overlays it into CloudlinkStatus.
func (b *batcher) LastUplinkTs() int64 { return b.lastUplink.Load() }

// run is the batcher's single goroutine. It drains on a telemetry-cadence
// ticker and on a P0-notify (after a short coalesce), and exits on ctx cancel.
func (b *batcher) run(ctx context.Context) {
	tick := time.NewTicker(b.telInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.notify:
			// A P0 event landed: wait out a brief coalesce window so a burst
			// batches into one frame, then drain (events are the top class, so
			// drainAll handles them first regardless).
			if !sleepCtx(ctx, b.coalesce) {
				return
			}
		case <-tick.C:
		}
		b.drainAll(ctx)
	}
}

// drainAll pulls frames until the spool is empty, the cloud drops, or ctx is
// canceled. A publish failure backs off (jittered, capped) and retries the SAME
// records — redelivery reuses the same seq, so the cloud's dedupe key holds.
func (b *batcher) drainAll(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		if !b.cloud.Connected() {
			return // disconnected: wait for the next trigger (collectors keep spooling)
		}
		progressed, err := b.drainOne(ctx)
		if err != nil {
			b.m.uplinkFail.Inc()
			if !b.bo.wait(ctx) {
				return
			}
			continue // retry same records, same seq
		}
		b.bo.reset()
		if !progressed {
			return // spool empty for now
		}
	}
}

// drainOne peeks the top class, takes the oldest contiguous run of a single
// stream, packs it into a ≤96 KiB frame (splitting if needed), publishes, and
// on PUBACK advances the seq and commits. Returns progressed=true if it did any
// work (published+committed a frame, or dropped a single over-budget record).
//
// Contiguous same-stream prefix: a priority class can hold two streams (plan +
// health share P1). Each frame carries ONE stream (its own seq), so drainOne
// takes only the leading run whose stream matches the oldest record. Interleaved
// streams simply yield more, smaller frames — correct, and rare given plan
// (5 min) and health (15 min) cadences.
func (b *batcher) drainOne(ctx context.Context) (progressed bool, err error) {
	recs, err := b.spool.Peek(maxPeekRecords, maxPeekBytes)
	if err != nil {
		return false, err
	}
	if len(recs) == 0 {
		return false, nil
	}
	stream := recs[0].Stream
	k := 1
	for k < len(recs) && recs[k].Stream == stream {
		k++
	}
	recs = recs[:k]

	seq := b.seqs.peek(stream)
	frame, n, dropped := buildFrame(b.cloud.Serial(), stream, seq, recs)
	if dropped {
		// A single record whose compressed frame alone exceeds 96 KiB can never
		// be delivered: drop it (counted + ERROR) and consume it so the drain
		// cannot wedge. This is the split loop's terminating case.
		slog.Error("cloudlink: single record exceeds frame budget — dropping",
			"stream", stream, "topic_bytes", len(recs[0].Payload))
		b.m.uplinkFail.Inc()
		if cerr := b.spool.Commit(1); cerr != nil {
			return false, cerr
		}
		return true, nil
	}

	if perr := b.cloud.PublishFrame(b.uplinkTopic(stream), frame, b.pubTimeout); perr != nil {
		return false, perr // no seq advance, no commit → redelivery reuses seq
	}

	b.m.batchBytes.Set(float64(len(frame)))

	// Advance (persist) the seq BEFORE committing. Rationale (mission: zero lost
	// P0 events): a crash between persist and commit re-sends the same records
	// under a NEW seq — a benign duplicate the cloud may not dedupe, but
	// at-least-once explicitly permits duplicates and never loss. The reverse
	// order (commit then persist) would, on a crash in its window, let the NEXT
	// batch reuse a seq the cloud already saw and be DROPPED as a dup — silent
	// loss, which the reserve/compliance mission forbids.
	if serr := b.seqs.bump(stream); serr != nil {
		return false, serr // persist failed: no commit, retry same records + same seq
	}
	if cerr := b.spool.Commit(n); cerr != nil {
		return false, cerr
	}
	b.m.uplinkFrames.Inc()
	b.lastUplink.Store(b.now().Unix())
	return true, nil
}

func (b *batcher) uplinkTopic(stream string) string {
	if stream == streamTelemetry {
		return telemetryTopic(b.cloud.Serial())
	}
	return eventsTopic(b.cloud.Serial())
}

// buildFrame packs the first n records (n starts at len(recs), halving until it
// fits) into a marshaled uplinkFrame ≤ maxFrameBytes.
//
// Termination: n starts finite (≤ maxPeekRecords) and strictly decreases by
// integer halving each iteration, so it reaches 1 in ≤log2(len) steps. At n==1
// the frame either fits (returned) or a single record alone busts the budget
// (dropped=true) — never an infinite retry.
func buildFrame(serial, stream string, seq uint64, recs []spool.Record) (frame []byte, n int, dropped bool) {
	n = len(recs)
	for n >= 1 {
		gz, gerr := gzipRecords(recs[:n])
		if gerr == nil {
			f, merr := json.Marshal(uplinkFrame{V: 1, Serial: serial, Stream: stream, Seq: seq, Count: n, GZ: gz})
			if merr == nil && len(f) <= maxFrameBytes {
				return f, n, false
			}
		}
		if n == 1 {
			return nil, 1, true
		}
		n /= 2
	}
	return nil, 0, false
}

// gzipRecords gzips the newline-joined record payloads (ndjson). Each payload is
// one recordFrame ({topic, ts, payload}); the join is the batch's ndjson body.
func gzipRecords(recs []spool.Record) ([]byte, error) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	for i, r := range recs {
		if i > 0 {
			if _, err := gw.Write([]byte{'\n'}); err != nil {
				return nil, err
			}
		}
		if _, err := gw.Write(r.Payload); err != nil {
			return nil, err
		}
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// sleepCtx sleeps d, returning false if ctx is canceled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// -----------------------------------------------------------------------------
// backoff — exponential (1s..60s) with equal jitter.
// -----------------------------------------------------------------------------

type backoff struct {
	cur, min, max time.Duration
}

// delay is the next sleep: equal jitter, cur/2 + rand[0, cur/2], so it stays in
// [cur/2, cur] and never collapses to ~0 (which would hammer a sick broker).
func (b *backoff) delay() time.Duration {
	half := b.cur / 2
	if half <= 0 {
		return b.cur
	}
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// advance doubles cur, capped at max.
func (b *backoff) advance() {
	b.cur *= 2
	if b.cur > b.max {
		b.cur = b.max
	}
}

func (b *backoff) reset() { b.cur = b.min }

// wait sleeps the current jittered delay then advances, returning false if ctx
// is canceled during the sleep (cur is NOT advanced then — a canceled wait is
// not a real backoff step).
func (b *backoff) wait(ctx context.Context) bool {
	if !sleepCtx(ctx, b.delay()) {
		return false
	}
	b.advance()
	return true
}

// -----------------------------------------------------------------------------
// seqStore — one persistent monotonic uint64 per stream, in spool_dir.
// -----------------------------------------------------------------------------

// seqStore holds the next seq to USE per stream. peek reads it; bump advances +
// persists it (tmp+rename+fsync, the spool cursor discipline). A frame's seq is
// the peeked value; bump runs only after a successful publish, so redelivery
// (no bump) reuses the same seq and the cloud dedupes.
type seqStore struct {
	dir string
	mu  sync.Mutex
	cur map[string]uint64
}

func openSeqStore(dir string) (*seqStore, error) {
	s := &seqStore{dir: dir, cur: make(map[string]uint64, len(allStreams))}
	for _, stream := range allStreams {
		v, err := readSeq(seqPath(dir, stream))
		if err != nil {
			return nil, err
		}
		s.cur[stream] = v
	}
	return s, nil
}

func (s *seqStore) peek(stream string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur[stream]
}

func (s *seqStore) bump(stream string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cur[stream] + 1
	if err := writeSeq(seqPath(s.dir, stream), next); err != nil {
		return err
	}
	s.cur[stream] = next
	return nil
}

func seqPath(dir, stream string) string { return filepath.Join(dir, "seq-"+stream) }

// readSeq loads a persisted seq. A missing file is 0 (first run). A garbage file
// falls back to 0 with a warning — redelivery-leaning like the spool's readCursor
// (the tmp+rename write makes a torn file effectively impossible short of external
// tampering).
func readSeq(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	var v uint64
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &v); err != nil {
		slog.Warn("cloudlink: unreadable seq file, restarting stream at 0", "path", path)
		return 0, nil
	}
	return v, nil
}

func writeSeq(path string, v uint64) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(f, "%d\n", v); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	// Best-effort dir fsync so the rename is durable (non-fatal, content already synced).
	if d, derr := os.Open(filepath.Dir(path)); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
