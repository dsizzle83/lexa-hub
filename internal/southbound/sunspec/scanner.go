package sunspec

import (
	"fmt"

	"lexa-hub/internal/southbound/modbus"
)

// Block describes a single SunSpec model block discovered on a device.
type Block struct {
	ModelID  uint16 // SunSpec model number (e.g. 103 for three-phase inverter)
	BaseAddr uint16 // 0-based Modbus address of the first data register
	Length   uint16 // number of data registers in this block
}

// Scan reads the SunSpec model block layout from t, starting at SunSpecBase.
// It reads only model ID and length registers — not the data — so it is fast
// and does not consume a large read burst. Returns blocks in device order.
//
// Scan returns an error if no SunS header is found at SunSpecBase, or if a
// Modbus read fails mid-scan.
func Scan(t modbus.Transport) ([]Block, error) {
	hdr, err := t.ReadHolding(SunSpecBase, 2)
	if err != nil {
		return nil, fmt.Errorf("sunspec scan: read header at %d: %w", SunSpecBase, err)
	}
	if hdr[0] != SunSMagic0 || hdr[1] != SunSMagic1 {
		return nil, fmt.Errorf("sunspec scan: no SunS header at %d (got 0x%04x 0x%04x)",
			SunSpecBase, hdr[0], hdr[1])
	}

	var blocks []Block
	cursor := SunSpecBase + 2 // first model ID register

	for {
		meta, err := t.ReadHolding(cursor, 2)
		if err != nil {
			return nil, fmt.Errorf("sunspec scan: read model header at %d: %w", cursor, err)
		}
		modelID := meta[0]
		length := meta[1]

		if modelID == EndMarker {
			break
		}
		blocks = append(blocks, Block{
			ModelID:  modelID,
			BaseAddr: cursor + 2, // skip past the ID and length registers
			Length:   length,
		})
		cursor += 2 + length
	}
	return blocks, nil
}

// FindModel returns the first Block with the given model ID from blocks.
// Returns an error if not found — callers can use HasModel to avoid the error.
func FindModel(blocks []Block, modelID uint16) (Block, error) {
	for _, b := range blocks {
		if b.ModelID == modelID {
			return b, nil
		}
	}
	return Block{}, fmt.Errorf("sunspec: model %d not present on device", modelID)
}
