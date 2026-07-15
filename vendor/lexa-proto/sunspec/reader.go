package sunspec

import (
	"fmt"

	"lexa-proto/modbus"
)

// Reader provides typed access to SunSpec model registers on a device.
// Construct with NewReader; it scans the device once at startup and caches
// the block layout so subsequent reads are single Modbus transactions.
type Reader struct {
	t      modbus.Transport
	blocks []Block
}

// NewReader scans t for SunSpec models and returns a Reader. Fails if the
// device has no SunS header or if the initial scan read fails.
func NewReader(t modbus.Transport) (*Reader, error) {
	blocks, err := Scan(t)
	if err != nil {
		return nil, err
	}
	return &Reader{t: t, blocks: blocks}, nil
}

// HasModel returns true if modelID was found on the device during the scan.
func (r *Reader) HasModel(modelID uint16) bool {
	_, err := FindModel(r.blocks, modelID)
	return err == nil
}

// Blocks returns the full list of SunSpec blocks found on this device.
// Useful for logging / diagnostics.
func (r *Reader) Blocks() []Block {
	return r.blocks
}

// maxHoldingRead is the Modbus spec ceiling on holding registers per single
// ReadHolding transaction (PI-MBUS-300: 0x7D = 125). A SunSpec model whose data
// block is wider than this — notably model 701, whose full layout is 137
// registers — MUST be read in consecutive chunks: a single ReadHolding of >125
// is refused by the transport, which would otherwise make the whole model
// (and, since 701 is read during discovery, the whole device) fail to read.
const maxHoldingRead = 125

// ReadModel reads all data registers for the given model and returns them as a
// []uint16 slice indexed by 0-based register offset within the model. Blocks
// wider than the Modbus per-read ceiling (maxHoldingRead) are read in
// consecutive transactions and concatenated, transparently to the caller.
func (r *Reader) ReadModel(modelID uint16) ([]uint16, error) {
	b, err := FindModel(r.blocks, modelID)
	if err != nil {
		return nil, err
	}
	regs, err := r.readChunked(b.BaseAddr, b.Length)
	if err != nil {
		return nil, fmt.Errorf("sunspec: read model %d at %d+%d: %w",
			modelID, b.BaseAddr, b.Length, err)
	}
	return regs, nil
}

// readChunked reads quantity holding registers starting at addr, splitting the
// request into consecutive transactions of at most maxHoldingRead registers and
// concatenating the results — so a model wider than the Modbus per-read ceiling
// is read correctly rather than refused. A quantity <= maxHoldingRead is a
// single ReadHolding, identical to the pre-chunking behaviour.
func (r *Reader) readChunked(addr, quantity uint16) ([]uint16, error) {
	out := make([]uint16, 0, quantity)
	for read := uint16(0); read < quantity; {
		n := quantity - read
		if n > maxHoldingRead {
			n = maxHoldingRead
		}
		chunk, err := r.t.ReadHolding(addr+read, n)
		if err != nil {
			return nil, err
		}
		out = append(out, chunk...)
		read += n
	}
	return out, nil
}

// WriteModel writes values into the given model starting at offset (0-based
// within the model's data block). offset+len(values) must not exceed the
// model's length.
func (r *Reader) WriteModel(modelID uint16, offset uint16, values []uint16) error {
	b, err := FindModel(r.blocks, modelID)
	if err != nil {
		return err
	}
	if uint16(len(values)) > b.Length-offset {
		return fmt.Errorf("sunspec: write model %d: offset %d + %d values exceeds model length %d",
			modelID, offset, len(values), b.Length)
	}
	if err := r.t.WriteHolding(b.BaseAddr+offset, values); err != nil {
		return fmt.Errorf("sunspec: write model %d at offset %d: %w", modelID, offset, err)
	}
	return nil
}
