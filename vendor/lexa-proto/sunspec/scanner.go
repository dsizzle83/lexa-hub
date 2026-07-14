package sunspec

import (
	"fmt"

	"lexa-proto/modbus"
)

// Block describes a single SunSpec model block discovered on a device.
type Block struct {
	ModelID  uint16 // SunSpec model number (e.g. 103 for three-phase inverter)
	BaseAddr uint16 // 0-based Modbus address of the first data register
	Length   uint16 // number of data registers in this block
}

// probeBases are the base addresses the SunSpec Device Information Model
// (§6.2) permits for the "SunS" header, in probe order: 40000 first (the vast
// majority of commercial hardware — fast path), then the spec-permitted
// alternates 0 and 50000.
var probeBases = []uint16{SunSpecBase, 0, 50000}

// Scan reads the SunSpec model block layout from t, probing the
// spec-permitted header base addresses (40000 first, then 0 and 50000) and
// scanning at the first one presenting a SunS header. It reads only model ID
// and length registers — not the data — so it is fast and does not consume a
// large read burst. Returns blocks in device order.
//
// Scan returns an error if no probed base presents a SunS header, or if a
// Modbus read fails mid-scan once a header has been found. Callers that need
// to know WHICH base matched use ScanProbe; callers with a known base use
// ScanAt.
func Scan(t modbus.Transport) ([]Block, error) {
	blocks, _, err := ScanProbe(t)
	return blocks, err
}

// ScanProbe is Scan, additionally reporting the base address whose SunS
// header matched (meaningful only when err is nil).
func ScanProbe(t modbus.Transport) ([]Block, uint16, error) {
	var firstErr error
	for _, base := range probeBases {
		hdr, err := t.ReadHolding(base, 2)
		if err != nil {
			// An unmapped address commonly returns a Modbus exception
			// (IllegalDataAddress) rather than zeros — treat a read failure
			// at a probe base as "not here" and keep probing. A device that
			// is genuinely unreachable fails every probe and reports below.
			if firstErr == nil {
				firstErr = fmt.Errorf("sunspec scan: read header at %d: %w", base, err)
			}
			continue
		}
		if hdr[0] != SunSMagic0 || hdr[1] != SunSMagic1 {
			continue
		}
		// Header found: this IS the device's base. A mid-scan failure past
		// this point is a real error, never a reason to probe elsewhere.
		blocks, err := scanModels(t, base)
		return blocks, base, err
	}
	if firstErr != nil {
		return nil, 0, fmt.Errorf("sunspec scan: no SunS header at bases %v (first read error: %w)",
			probeBases, firstErr)
	}
	return nil, 0, fmt.Errorf("sunspec scan: no SunS header at bases %v", probeBases)
}

// ScanAt reads the SunSpec model block layout from t at one explicit base
// address, with no probing — the pre-probe single-base behavior, for callers
// that already know (e.g. from a prior ScanProbe or a commissioning record)
// where the device's header lives.
//
// ScanAt returns an error if no SunS header is found at base, or if a Modbus
// read fails mid-scan.
func ScanAt(t modbus.Transport, base uint16) ([]Block, error) {
	hdr, err := t.ReadHolding(base, 2)
	if err != nil {
		return nil, fmt.Errorf("sunspec scan: read header at %d: %w", base, err)
	}
	if hdr[0] != SunSMagic0 || hdr[1] != SunSMagic1 {
		return nil, fmt.Errorf("sunspec scan: no SunS header at %d (got 0x%04x 0x%04x)",
			base, hdr[0], hdr[1])
	}
	return scanModels(t, base)
}

// scanModels walks the model list that follows a verified SunS header at base.
func scanModels(t modbus.Transport, base uint16) ([]Block, error) {
	var blocks []Block
	cursor := base + 2 // first model ID register

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
