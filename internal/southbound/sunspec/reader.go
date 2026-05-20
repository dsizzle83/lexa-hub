package sunspec

import (
	"fmt"

	"lexa-hub/internal/southbound/modbus"
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

// ReadModel reads all data registers for the given model and returns them as a
// []uint16 slice indexed by 0-based register offset within the model.
func (r *Reader) ReadModel(modelID uint16) ([]uint16, error) {
	b, err := FindModel(r.blocks, modelID)
	if err != nil {
		return nil, err
	}
	regs, err := r.t.ReadHolding(b.BaseAddr, b.Length)
	if err != nil {
		return nil, fmt.Errorf("sunspec: read model %d at %d+%d: %w",
			modelID, b.BaseAddr, b.Length, err)
	}
	return regs, nil
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
