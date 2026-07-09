package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"lexa-hub/internal/metrics"
	"lexa-hub/internal/spool"
)

func newTestMetrics() *cloudlinkMetrics { return newCloudlinkMetrics(metrics.New()) }

// -----------------------------------------------------------------------------
// fakeCloud — recording cloudPublisher with controllable connectivity/failure.
// -----------------------------------------------------------------------------

type publishedFrame struct {
	topic   string
	payload []byte
}

type fakeCloud struct {
	mu        sync.Mutex
	serial    string
	connected bool
	failAll   bool
	failNext  int
	published []publishedFrame
}

func (f *fakeCloud) Connected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}
func (f *fakeCloud) Serial() string { return f.serial }
func (f *fakeCloud) PublishFrame(topic string, payload []byte, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAll || f.failNext > 0 {
		if f.failNext > 0 {
			f.failNext--
		}
		return errors.New("fake publish failure")
	}
	f.published = append(f.published, publishedFrame{topic: topic, payload: append([]byte(nil), payload...)})
	return nil
}
func (f *fakeCloud) frames() []publishedFrame {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]publishedFrame(nil), f.published...)
}

// helpers --------------------------------------------------------------------

func newTestBatcher(t *testing.T, sp *spool.Spool, fc *fakeCloud) *batcher {
	t.Helper()
	seqs, err := openSeqStore(t.TempDir())
	if err != nil {
		t.Fatalf("openSeqStore: %v", err)
	}
	return &batcher{
		spool:       sp,
		cloud:       fc,
		seqs:        seqs,
		m:           newTestMetrics(),
		notify:      make(chan struct{}, 1),
		telInterval: time.Hour,
		coalesce:    time.Millisecond,
		pubTimeout:  time.Second,
		bo:          backoff{cur: time.Microsecond, min: time.Microsecond, max: time.Millisecond},
		now:         func() time.Time { return time.Unix(1751990000, 0) },
	}
}

// appendRecord frames payload as a recordFrame and appends it to the spool,
// mirroring what a collector does.
func appendRecord(t *testing.T, sp *spool.Spool, stream string, prio int, topic, payload string) {
	t.Helper()
	rf := recordFrame{Topic: topic, Ts: 1, Payload: json.RawMessage(payload)}
	b, err := json.Marshal(rf)
	if err != nil {
		t.Fatalf("marshal recordFrame: %v", err)
	}
	if err := sp.Append(spool.Record{Stream: stream, Priority: prio, Ts: 1, Payload: b}); err != nil {
		t.Fatalf("spool.Append: %v", err)
	}
}

// assertDrained checks the spool is LOGICALLY empty (Peek returns nothing).
// Bytes() is not used: it counts physical on-disk segment bytes, and a fully
// consumed but not-yet-rotated active segment lingers until GC — the spool's
// own tests likewise assert drain via Peek, never Bytes.
func assertDrained(t *testing.T, sp *spool.Spool) {
	t.Helper()
	recs, err := sp.Peek(1, 0)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("spool not logically drained: %d record(s) still pending", len(recs))
	}
}

func assertPending(t *testing.T, sp *spool.Spool) {
	t.Helper()
	recs, err := sp.Peek(1, 0)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(recs) == 0 {
		t.Error("spool unexpectedly drained — records lost")
	}
}

func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	gr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	out, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return out
}

// -----------------------------------------------------------------------------
// Tests.
// -----------------------------------------------------------------------------

// PUBACK ⇒ Commit: a connected cloud drains the spool clean; frames go to the
// right topic; metrics + lastUplink move.
func TestBatcher_PubackCommitsAndDrains(t *testing.T) {
	dir := t.TempDir()
	sp, _ := spool.Open(dir, 1<<20, nil)
	defer sp.Close()
	appendRecord(t, sp, streamTelemetry, prioTelemetry, "lexa/measurements/a", `{"v":1,"w":1}`)
	appendRecord(t, sp, streamTelemetry, prioTelemetry, "lexa/measurements/b", `{"v":1,"w":2}`)

	fc := &fakeCloud{serial: "SER", connected: true}
	b := newTestBatcher(t, sp, fc)
	b.drainAll(context.Background())

	assertDrained(t, sp)
	frames := fc.frames()
	if len(frames) != 1 {
		t.Fatalf("published %d frames, want 1", len(frames))
	}
	if frames[0].topic != "lexa/v1/SER/telemetry" {
		t.Errorf("topic = %q, want lexa/v1/SER/telemetry", frames[0].topic)
	}
	var uf uplinkFrame
	if err := json.Unmarshal(frames[0].payload, &uf); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if uf.V != 1 || uf.Serial != "SER" || uf.Stream != streamTelemetry || uf.Seq != 0 || uf.Count != 2 {
		t.Errorf("frame header = %+v, want v1/SER/telemetry/seq0/count2", uf)
	}
	if b.LastUplinkTs() != 1751990000 {
		t.Errorf("LastUplinkTs = %d, want 1751990000", b.LastUplinkTs())
	}
	// seq advanced to 1 after the committed frame.
	if got := b.seqs.peek(streamTelemetry); got != 1 {
		t.Errorf("telemetry seq = %d, want 1 after commit", got)
	}
}

// Events (P0) drain before telemetry (P2) and go to the events topic.
func TestBatcher_EventsDrainBeforeTelemetry(t *testing.T) {
	dir := t.TempDir()
	sp, _ := spool.Open(dir, 1<<20, nil)
	defer sp.Close()
	appendRecord(t, sp, streamTelemetry, prioTelemetry, "lexa/measurements/a", `{"v":1}`)
	appendRecord(t, sp, streamEvents, prioEvents, "lexa/csip/compliance/alert", `{"v":1,"active":true}`)

	fc := &fakeCloud{serial: "S", connected: true}
	b := newTestBatcher(t, sp, fc)
	b.drainAll(context.Background())

	frames := fc.frames()
	if len(frames) != 2 {
		t.Fatalf("published %d frames, want 2", len(frames))
	}
	// P0 events first.
	if frames[0].topic != "lexa/v1/S/events" {
		t.Errorf("first frame topic = %q, want events", frames[0].topic)
	}
	if frames[1].topic != "lexa/v1/S/telemetry" {
		t.Errorf("second frame topic = %q, want telemetry", frames[1].topic)
	}
}

// A P1 peek holding two streams (plan + health) yields one frame per stream,
// each with its own seq namespace.
func TestBatcher_ContiguousStreamSplitWithinClass(t *testing.T) {
	dir := t.TempDir()
	sp, _ := spool.Open(dir, 1<<20, nil)
	defer sp.Close()
	appendRecord(t, sp, streamPlan, prioPlanHealth, "lexa/hub/plan", `{"v":1,"p":1}`)
	appendRecord(t, sp, streamHealth, prioPlanHealth, "lexa/hub/mode", `{"v":1,"m":"opt"}`)

	fc := &fakeCloud{serial: "S", connected: true}
	b := newTestBatcher(t, sp, fc)
	b.drainAll(context.Background())

	frames := fc.frames()
	if len(frames) != 2 {
		t.Fatalf("published %d frames, want 2 (one per stream)", len(frames))
	}
	streams := map[string]uint64{}
	for _, f := range frames {
		var uf uplinkFrame
		if err := json.Unmarshal(f.payload, &uf); err != nil {
			t.Fatalf("decode: %v", err)
		}
		streams[uf.Stream] = uf.Seq
		if uf.Count != 1 {
			t.Errorf("stream %s frame count = %d, want 1", uf.Stream, uf.Count)
		}
	}
	if _, ok := streams[streamPlan]; !ok {
		t.Error("no plan frame")
	}
	if _, ok := streams[streamHealth]; !ok {
		t.Error("no health frame")
	}
	// Both go to the events cloud topic (P0/P1 share it).
	for _, f := range frames {
		if f.topic != "lexa/v1/S/events" {
			t.Errorf("plan/health topic = %q, want events", f.topic)
		}
	}
}

// Publish failure ⇒ no Commit, no seq advance; a later success reuses the SAME
// seq (the cloud dedupe key holds).
func TestBatcher_FailureRedeliversSameSeq(t *testing.T) {
	dir := t.TempDir()
	sp, _ := spool.Open(dir, 1<<20, nil)
	defer sp.Close()
	appendRecord(t, sp, streamTelemetry, prioTelemetry, "lexa/measurements/a", `{"v":1,"w":1}`)

	fc := &fakeCloud{serial: "S", connected: true, failAll: true}
	b := newTestBatcher(t, sp, fc)

	seqBefore := b.seqs.peek(streamTelemetry)
	// First (failing) attempt: no commit, no seq advance.
	if _, err := b.drainOne(context.Background()); err == nil {
		t.Fatal("drainOne returned nil error on a failing publish")
	}
	assertPending(t, sp) // not committed despite the failure
	if got := b.seqs.peek(streamTelemetry); got != seqBefore {
		t.Errorf("seq advanced to %d on failure, want unchanged %d", got, seqBefore)
	}
	if len(fc.frames()) != 0 {
		t.Errorf("recorded %d frames on a failing cloud, want 0", len(fc.frames()))
	}

	// Recover; the redelivered frame must carry the SAME seq.
	fc.mu.Lock()
	fc.failAll = false
	fc.mu.Unlock()
	if _, err := b.drainOne(context.Background()); err != nil {
		t.Fatalf("drainOne after recovery: %v", err)
	}
	frames := fc.frames()
	if len(frames) != 1 {
		t.Fatalf("published %d frames after recovery, want 1", len(frames))
	}
	var uf uplinkFrame
	json.Unmarshal(frames[0].payload, &uf)
	if uf.Seq != seqBefore {
		t.Errorf("redelivered seq = %d, want same as pre-failure %d", uf.Seq, seqBefore)
	}
	assertDrained(t, sp)
}

// Disconnected ⇒ drainAll is a no-op (collectors keep spooling).
func TestBatcher_DisconnectedSkipsDrain(t *testing.T) {
	dir := t.TempDir()
	sp, _ := spool.Open(dir, 1<<20, nil)
	defer sp.Close()
	appendRecord(t, sp, streamEvents, prioEvents, "lexa/intent/result", `{"v":1}`)

	fc := &fakeCloud{serial: "S", connected: false}
	b := newTestBatcher(t, sp, fc)
	b.drainAll(context.Background())

	if len(fc.frames()) != 0 {
		t.Error("published a frame while disconnected")
	}
	assertPending(t, sp) // records held, not lost
}

// buildFrame split loop: many small records exceed 96 KiB collectively → n<len;
// a single over-budget record → dropped.
func TestBuildFrame_SplitAndDrop(t *testing.T) {
	// Poorly-compressible payloads so several frames are needed.
	mkRec := func(n int) spool.Record {
		buf := make([]byte, n)
		rand.Read(buf)
		return spool.Record{Stream: streamTelemetry, Priority: prioTelemetry, Payload: buf}
	}

	t.Run("splits under budget", func(t *testing.T) {
		var recs []spool.Record
		for i := 0; i < 40; i++ {
			recs = append(recs, mkRec(8*1024)) // 40×8KiB ≈ 320KiB raw, incompressible
		}
		frame, n, dropped := buildFrame("S", streamTelemetry, 0, recs)
		if dropped {
			t.Fatal("dropped a set that should split")
		}
		if n < 1 || n >= len(recs) {
			t.Fatalf("split n = %d, want in [1,%d)", n, len(recs))
		}
		if len(frame) > maxFrameBytes {
			t.Errorf("frame %d bytes exceeds budget %d", len(frame), maxFrameBytes)
		}
	})

	t.Run("single oversize record dropped", func(t *testing.T) {
		recs := []spool.Record{mkRec(200 * 1024)} // one 200KiB incompressible record
		frame, n, dropped := buildFrame("S", streamTelemetry, 0, recs)
		if !dropped {
			t.Fatalf("single 200KiB record not dropped (n=%d, frame=%d)", n, len(frame))
		}
		if n != 1 {
			t.Errorf("drop n = %d, want 1", n)
		}
	})

	t.Run("terminates and fits when possible", func(t *testing.T) {
		recs := []spool.Record{mkRec(1024)}
		frame, n, dropped := buildFrame("S", streamTelemetry, 0, recs)
		if dropped || n != 1 || len(frame) == 0 {
			t.Errorf("small single record: dropped=%v n=%d len=%d", dropped, n, len(frame))
		}
	})
}

// A single over-budget record on the spool is dropped+counted by the batcher
// (not an infinite retry).
func TestBatcher_DropsSingleOversizeRecord(t *testing.T) {
	dir := t.TempDir()
	sp, _ := spool.Open(dir, 4<<20, nil)
	defer sp.Close()
	big := make([]byte, 200*1024)
	rand.Read(big)
	if err := sp.Append(spool.Record{Stream: streamTelemetry, Priority: prioTelemetry, Payload: big}); err != nil {
		t.Fatalf("append: %v", err)
	}

	fc := &fakeCloud{serial: "S", connected: true}
	b := newTestBatcher(t, sp, fc)
	b.drainAll(context.Background())

	assertDrained(t, sp) // dropped record consumed, drain not wedged
	if len(fc.frames()) != 0 {
		t.Error("published a frame for an undeliverable oversize record")
	}
}

// Frame golden: decode gz → the original record payloads verbatim, and the
// innermost payload is the original envelope byte-for-byte.
func TestFrame_GzGoldenVerbatim(t *testing.T) {
	orig := `{"v":1,"voltage_v":240.5,"unknown_future":{"x":[1,2,3]}}`
	rf := recordFrame{Topic: "lexa/measurements/x", Ts: 42, Payload: json.RawMessage(orig)}
	rfBytes, _ := json.Marshal(rf)
	recs := []spool.Record{{Stream: streamTelemetry, Payload: rfBytes}}

	frame, n, dropped := buildFrame("S", streamTelemetry, 7, recs)
	if dropped || n != 1 {
		t.Fatalf("buildFrame dropped=%v n=%d", dropped, n)
	}
	var uf uplinkFrame
	if err := json.Unmarshal(frame, &uf); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	nd := gunzip(t, uf.GZ)
	if !bytes.Equal(nd, rfBytes) {
		t.Fatalf("gz body = %q, want the recordFrame bytes verbatim %q", nd, rfBytes)
	}
	var back recordFrame
	if err := json.Unmarshal(nd, &back); err != nil {
		t.Fatalf("decode inner recordFrame: %v", err)
	}
	if string(back.Payload) != orig {
		t.Errorf("innermost payload = %q, want original envelope verbatim %q", string(back.Payload), orig)
	}
}

// seq persistence across a batcher restart (reopen the seq store).
func TestSeqStore_PersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	s1, err := openSeqStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := s1.bump(streamEvents); err != nil {
			t.Fatalf("bump: %v", err)
		}
	}
	if err := s1.bump(streamTelemetry); err != nil {
		t.Fatalf("bump: %v", err)
	}

	s2, err := openSeqStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := s2.peek(streamEvents); got != 3 {
		t.Errorf("events seq after restart = %d, want 3", got)
	}
	if got := s2.peek(streamTelemetry); got != 1 {
		t.Errorf("telemetry seq after restart = %d, want 1", got)
	}
	if got := s2.peek(streamPlan); got != 0 {
		t.Errorf("untouched plan seq = %d, want 0", got)
	}
}

// backoff caps at max and jitter stays in [cur/2, cur].
func TestBackoff_CapsAndJitter(t *testing.T) {
	b := backoff{cur: time.Second, min: time.Second, max: 60 * time.Second}
	for i := 0; i < 100; i++ {
		b.advance()
	}
	if b.cur != 60*time.Second {
		t.Errorf("backoff cur = %s after many advances, want capped at 60s", b.cur)
	}
	b2 := backoff{cur: 8 * time.Second, min: time.Second, max: 60 * time.Second}
	for i := 0; i < 50; i++ {
		d := b2.delay()
		if d < 4*time.Second || d > 8*time.Second {
			t.Fatalf("delay %s out of [4s,8s] equal-jitter band", d)
		}
	}
}

// P0-notify wakes the batcher promptly (well before the telemetry interval),
// and ctx-cancel makes run() exit (no goroutine leak).
func TestBatcher_NotifyBeatsIntervalAndShutsDown(t *testing.T) {
	dir := t.TempDir()
	sp, _ := spool.Open(dir, 1<<20, nil)
	defer sp.Close()

	fc := &fakeCloud{serial: "S", connected: true}
	notify := make(chan struct{}, 1)
	seqs, _ := openSeqStore(t.TempDir())
	b := &batcher{
		spool:       sp,
		cloud:       fc,
		seqs:        seqs,
		m:           newTestMetrics(),
		notify:      notify,
		telInterval: time.Hour, // would never fire in this test
		coalesce:    time.Millisecond,
		pubTimeout:  time.Second,
		bo:          backoff{cur: time.Microsecond, min: time.Microsecond, max: time.Millisecond},
		now:         time.Now,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.run(ctx); close(done) }()

	// Append a P0 event and poke.
	appendRecord(t, sp, streamEvents, prioEvents, "lexa/csip/compliance/alert", `{"v":1,"active":true}`)
	notify <- struct{}{}

	deadline := time.After(2 * time.Second)
	for {
		if len(fc.frames()) > 0 {
			break
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("P0 event was not uplinked within 2s (notify path)")
		case <-time.After(2 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("batcher.run did not exit on ctx cancel (goroutine leak)")
	}
}
