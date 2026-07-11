package frame

import (
	"bytes"
	"errors"
	"testing"
)

// message builds a deterministic n-byte message (byte i == i & 0xff), the same
// fixture the Dart ble_protocol_test uses.
func message(n int) []byte {
	m := make([]byte, n)
	for i := range m {
		m[i] = byte(i)
	}
	return m
}

// reassemble feeds every chunk in order and returns the completed message,
// failing the test on any framing error or a run that never completes.
func reassemble(t *testing.T, chunks [][]byte) []byte {
	t.Helper()
	var r Reassembler
	var got []byte
	done := false
	for i, c := range chunks {
		msg, d, err := r.Add(c)
		if err != nil {
			t.Fatalf("Add(chunk %d): %v", i, err)
		}
		if d {
			got = msg
			done = true
		}
	}
	if !done {
		t.Fatal("reassembly never completed (no FIN)")
	}
	return got
}

func TestChunk_SingleChunkRoundTrip(t *testing.T) {
	msg := message(50)
	chunks, err := Chunk(msg, 100, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(chunks))
	}
	if chunks[0][0]&FlagFIN == 0 {
		t.Fatal("single chunk must carry FIN")
	}
	if chunks[0][0]&FlagENC != 0 {
		t.Fatal("plaintext chunk must not carry ENC")
	}
	if got := reassemble(t, chunks); !bytes.Equal(got, msg) {
		t.Fatalf("round trip mismatch")
	}
}

func TestChunk_MultiChunkPreservesBytesAndOrder(t *testing.T) {
	msg := message(1000)
	chunks, err := Chunk(msg, 100, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) <= 1 {
		t.Fatalf("want multiple chunks, got %d", len(chunks))
	}
	// Every chunk carries ENC; only the last carries FIN; seq is sequential.
	for i, c := range chunks {
		if c[0]&FlagENC == 0 {
			t.Fatalf("chunk %d missing ENC", i)
		}
		last := i == len(chunks)-1
		if fin := c[0]&FlagFIN != 0; fin != last {
			t.Fatalf("chunk %d FIN=%v, want %v", i, fin, last)
		}
		if int(c[1]) != i {
			t.Fatalf("chunk %d seq=%d", i, c[1])
		}
	}
	var r Reassembler
	var got []byte
	for _, c := range chunks {
		msg, done, err := r.Add(c)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			got = msg
		}
	}
	if !bytes.Equal(got, msg) {
		t.Fatal("round trip mismatch")
	}
	if r.Encrypted() {
		t.Fatal("Encrypted() must reset to false after FIN")
	}
}

func TestChunk_EmptyMessageOneFinFrame(t *testing.T) {
	chunks, err := Chunk(nil, 20, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 {
		t.Fatalf("empty message must produce exactly one chunk, got %d", len(chunks))
	}
	if chunks[0][0]&FlagFIN == 0 {
		t.Fatal("the lone empty chunk must carry FIN")
	}
	got := reassemble(t, chunks)
	if len(got) != 0 {
		t.Fatalf("want empty message, got %d bytes", len(got))
	}
	if got == nil {
		t.Fatal("completed empty message must be non-nil (distinguishes done from incomplete)")
	}
}

func TestReassembler_SequenceGap(t *testing.T) {
	chunks, err := Chunk(message(500), 100, false)
	if err != nil {
		t.Fatal(err)
	}
	var r Reassembler
	if _, _, err := r.Add(chunks[0]); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Add(chunks[2]); !errors.Is(err, ErrSeqGap) {
		t.Fatalf("want ErrSeqGap, got %v", err)
	}
}

func TestReassembler_EncFlipMidMessage(t *testing.T) {
	chunks, err := Chunk(message(500), 100, false)
	if err != nil {
		t.Fatal(err)
	}
	var r Reassembler
	if _, _, err := r.Add(chunks[0]); err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Clone(chunks[1])
	tampered[0] |= FlagENC
	if _, _, err := r.Add(tampered); !errors.Is(err, ErrEncChanged) {
		t.Fatalf("want ErrEncChanged, got %v", err)
	}
}

func TestReassembler_ShortChunk(t *testing.T) {
	var r Reassembler
	if _, _, err := r.Add([]byte{FlagFIN}); !errors.Is(err, ErrShortChunk) {
		t.Fatalf("want ErrShortChunk, got %v", err)
	}
}

func TestChunk_OversizeRejected(t *testing.T) {
	_, err := Chunk(make([]byte, MaxMessageLength+1), 100, false)
	if !errors.Is(err, ErrMessageTooLong) {
		t.Fatalf("want ErrMessageTooLong, got %v", err)
	}
	// Exactly at the cap is allowed.
	if _, err := Chunk(make([]byte, MaxMessageLength), 100, false); err != nil {
		t.Fatalf("at-cap message must chunk: %v", err)
	}
}

func TestReassembler_OversizeRejected(t *testing.T) {
	// Frame an over-cap payload with a generous ATT size, forging chunks past
	// the reassembly limit (Chunk itself refuses >cap, so build by hand).
	var r Reassembler
	// One non-FIN chunk of the whole cap, then one more byte -> overflow.
	big := make([]byte, MaxMessageLength)
	first := append([]byte{0x00, 0x00}, big...) // flags=0 (no FIN), seq 0
	if _, _, err := r.Add(first); err != nil {
		t.Fatalf("first at-cap chunk should be accepted: %v", err)
	}
	overflow := []byte{FlagFIN, 0x01, 0x00} // one more payload byte
	if _, _, err := r.Add(overflow); !errors.Is(err, ErrMessageTooLong) {
		t.Fatalf("want ErrMessageTooLong on reassembly, got %v", err)
	}
}

func TestChunk_TooSmallPayload(t *testing.T) {
	if _, err := Chunk(message(4), HeaderLength, false); !errors.Is(err, ErrPayloadTooSmall) {
		t.Fatalf("want ErrPayloadTooSmall, got %v", err)
	}
}

// TestChunk_MTUBoundaries round-trips a range of message sizes at both the
// smallest supported ATT payload (23, MTU 26) and the negotiated ceiling
// (244, MTU 247).
func TestChunk_MTUBoundaries(t *testing.T) {
	for _, att := range []int{23, 244} {
		for _, n := range []int{0, 1, att - HeaderLength - 1, att - HeaderLength, att - HeaderLength + 1, 500, 2000} {
			if n < 0 {
				continue
			}
			msg := message(n)
			for _, enc := range []bool{false, true} {
				chunks, err := Chunk(msg, att, enc)
				if err != nil {
					t.Fatalf("Chunk(n=%d att=%d enc=%v): %v", n, att, enc, err)
				}
				// No chunk may exceed the ATT payload budget.
				for _, c := range chunks {
					if len(c) > att {
						t.Fatalf("chunk %d bytes exceeds att %d", len(c), att)
					}
				}
				if got := reassemble(t, chunks); !bytes.Equal(got, msg) {
					t.Fatalf("round trip mismatch n=%d att=%d enc=%v", n, att, enc)
				}
			}
		}
	}
}
