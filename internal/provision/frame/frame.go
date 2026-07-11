// Package frame implements the GATT chunk framing for "LEXA Provision v1"
// (ADR-0002, docs/gap-close/ADR-0002-ble-commissioning-protocol.md).
//
// It is a faithful, transport-agnostic port of the Dart reference
// (packages/lexa_core/lib/src/ble/protocol.dart: FrameFlags, chunkMessage,
// FrameReassembler) — same invariants, same error conditions. It has no
// dependency on the sec1 crypto or on any BLE/D-Bus machinery, so unit B2 can
// import it for transport wiring on its own.
//
// Wire layout of each chunk:
//
//	byte 0   flags: bit0 = FIN (last chunk), bit1 = ENC (ciphertext)
//	byte 1   seq  (per-message, wraps mod 256, resets after FIN)
//	byte 2+  payload chunk
//
// A whole (JSON, optionally AES-128-GCM-encrypted) message is split into
// chunks sized to the negotiated ATT payload (MTU minus the 3-byte ATT
// header) and reassembled on the far side. The ENC flag is constant within a
// message and the reassembled size is capped at 8 KiB.
package frame

import (
	"errors"
	"fmt"
)

// Frame flag bits (protocol.dart FrameFlags).
const (
	// FlagFIN marks the last chunk of a message.
	FlagFIN byte = 0x01
	// FlagENC marks the payload as sec1 ciphertext (ciphertext‖tag).
	FlagENC byte = 0x02
)

// HeaderLength is the fixed per-chunk header size (flags + seq).
const HeaderLength = 2

// MaxMessageLength caps the reassembled message size (ADR-0002).
const MaxMessageLength = 8 * 1024

// Sentinel errors. Callers that only need "something was malformed" can treat
// any non-nil error uniformly; these exist for errors.Is discrimination.
var (
	// ErrPayloadTooSmall is returned by Chunk when attPayloadSize leaves no
	// room for even one payload byte after the 2-byte header.
	ErrPayloadTooSmall = errors.New("provision/frame: attPayloadSize too small for frame header")
	// ErrMessageTooLong is returned when a message exceeds MaxMessageLength,
	// on either the chunking or the reassembly side.
	ErrMessageTooLong = errors.New("provision/frame: message exceeds size limit")
	// ErrShortChunk is returned by (*Reassembler).Add for a chunk that is
	// shorter than the frame header.
	ErrShortChunk = errors.New("provision/frame: chunk shorter than frame header")
	// ErrSeqGap is returned by (*Reassembler).Add when a chunk's seq byte is
	// not the next expected value (a dropped, duplicated, or reordered chunk).
	ErrSeqGap = errors.New("provision/frame: unexpected chunk seq")
	// ErrEncChanged is returned when the ENC flag flips partway through a
	// message.
	ErrEncChanged = errors.New("provision/frame: ENC flag changed mid-message")
)

// Chunk splits message into GATT-sized chunks for an ATT payload of
// attPayloadSize bytes. Mirrors chunkMessage: an empty message still yields
// exactly one FIN chunk; seq starts at 0 and increments per chunk (wrapping
// mod 256). Every chunk carries ENC iff encrypted is true.
func Chunk(message []byte, attPayloadSize int, encrypted bool) ([][]byte, error) {
	chunkData := attPayloadSize - HeaderLength
	if chunkData < 1 {
		return nil, fmt.Errorf("%w (attPayloadSize=%d)", ErrPayloadTooSmall, attPayloadSize)
	}
	if len(message) > MaxMessageLength {
		return nil, fmt.Errorf("%w (%d bytes)", ErrMessageTooLong, len(message))
	}

	var flags byte
	if encrypted {
		flags |= FlagENC
	}

	var chunks [][]byte
	seq := 0
	offset := 0
	// do/while: run at least once so an empty message emits one FIN chunk.
	for {
		end := offset + chunkData
		if end > len(message) {
			end = len(message)
		}
		last := end == len(message)

		chunk := make([]byte, HeaderLength+(end-offset))
		chunk[0] = flags
		if last {
			chunk[0] |= FlagFIN
		}
		chunk[1] = byte(seq)
		copy(chunk[HeaderLength:], message[offset:end])
		chunks = append(chunks, chunk)

		offset = end
		seq++
		if offset >= len(message) {
			break
		}
	}
	return chunks, nil
}

// Reassembler rebuilds a message from chunks produced by Chunk. Feed chunks in
// order via Add; it returns done=true with the full (possibly empty) message
// once a FIN chunk completes it, and resets so the same value can assemble the
// next message. It mirrors FrameReassembler, including its error conditions:
// short chunk, seq gap, mid-message ENC change, and oversize overflow.
//
// The zero value is ready to use.
type Reassembler struct {
	buf         []byte
	expectedSeq int
	started     bool
	encrypted   bool
}

// Encrypted reports whether the message currently being assembled carries the
// ENC flag. It is meaningful only between the first chunk and the FIN chunk;
// it resets to false after a completed message, matching the Dart getter.
func (r *Reassembler) Encrypted() bool { return r.started && r.encrypted }

// Add feeds one chunk. When the chunk completes a message it returns
// (message, true, nil); otherwise (nil, false, nil). A malformed chunk returns
// a non-nil error and leaves the reassembler poisoned for the current message
// (callers should treat a framing error as fatal to the session, as the hub
// harness does).
func (r *Reassembler) Add(chunk []byte) (message []byte, done bool, err error) {
	if len(chunk) < HeaderLength {
		return nil, false, ErrShortChunk
	}
	flags := chunk[0]
	seq := chunk[1]
	if seq != byte(r.expectedSeq) {
		return nil, false, fmt.Errorf("%w: expected %d, got %d", ErrSeqGap, byte(r.expectedSeq), seq)
	}
	enc := flags&FlagENC != 0
	if r.started && r.encrypted != enc {
		return nil, false, ErrEncChanged
	}
	r.started = true
	r.encrypted = enc

	r.buf = append(r.buf, chunk[HeaderLength:]...)
	if len(r.buf) > MaxMessageLength {
		return nil, false, ErrMessageTooLong
	}
	r.expectedSeq++

	if flags&FlagFIN != 0 {
		msg := r.buf
		if msg == nil {
			msg = []byte{}
		}
		r.reset()
		return msg, true, nil
	}
	return nil, false, nil
}

func (r *Reassembler) reset() {
	r.buf = nil
	r.expectedSeq = 0
	r.started = false
	r.encrypted = false
}
