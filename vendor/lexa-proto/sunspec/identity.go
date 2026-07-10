package sunspec

import (
	"bytes"
	"errors"
	"fmt"
)

// Common is the SunSpec model 1 ("Common") identity block: the manufacturer,
// model, options, firmware version and serial-number strings every SunSpec
// device publishes, plus its Modbus device address. It is the first block a
// commissioning tool reads to identify unknown hardware on the bus.
type Common struct {
	Manufacturer string // "Mn"  — manufacturer name
	Model        string // "Md"  — device model
	Options      string // "Opt" — device options / configuration string
	Version      string // "Vr"  — firmware / software version
	Serial       string // "SN"  — device serial number
	DeviceAddr   uint16 // "DA"  — Modbus device (unit) address; 0 when absent
}

// Sentinel errors returned by ReadCommon. Both are errors.Is-able.
var (
	// ErrNoCommonModel means the device advertises no SunSpec model 1 block.
	// Callers use errors.Is to distinguish "this device has no common model"
	// from a transport or decode failure.
	ErrNoCommonModel = errors.New("sunspec: common model (1) not present on device")

	// ErrShortCommonModel means model 1 is present but its reported length is
	// too short to even contain the serial-number field — a malformed or
	// non-conformant device. ReadCommon wraps it with the got/want register
	// counts.
	ErrShortCommonModel = errors.New("sunspec: common model (1) shorter than the SunSpec layout requires")
)

// SunSpec model 1 (Common) register layout, as 0-based offsets WITHIN the
// model's data block — i.e. into the slice Reader.ReadModel(ModelCommon)
// returns. That slice begins AFTER the two-register model header (model ID + L)
// which Scan has already consumed: reader.go's ReadModel reads Block.Length
// registers starting at Block.BaseAddr, and Scan sets Block.BaseAddr to two
// registers past the model ID/length pair. So offset 0 below is the first
// register of the Manufacturer string.
//
//	Field         regs  offset
//	Mn  (mfr)      16    0
//	Md  (model)    16    16
//	Opt (options)  8     32
//	Vr  (version)  8     40
//	SN  (serial)   16    48
//	DA  (address)  1     64
//	Pad            1     65   (optional — some devices report L=65 without it)
//
// The standard total data length L is 66 (with Pad) or 65 (without). A longer L
// is tolerated by ignoring the trailing registers; an L shorter than
// commonSNEnd (the end of the serial-number field) is rejected as malformed.
const (
	commonMnReg  = 0
	commonMdReg  = 16
	commonOptReg = 32
	commonVrReg  = 40
	commonSNReg  = 48
	commonSNEnd  = 64 // one past the last SN register; the minimum valid model length
	commonDAReg  = 64 // Modbus device address; present only when L > commonSNEnd
)

// ReadCommon reads and decodes the SunSpec model 1 (Common) identity block
// through r.
//
// It returns ErrNoCommonModel (errors.Is-able) when the device has no model 1,
// and a wrapped ErrShortCommonModel when model 1 is present but too short to
// hold the serial-number field. A longer-than-standard model is accepted, its
// trailing registers ignored.
//
// ReadCommon issues NO writes — it is safe against unknown, possibly energized
// hardware.
func ReadCommon(r *Reader) (Common, error) {
	if !r.HasModel(ModelCommon) {
		return Common{}, ErrNoCommonModel
	}
	data, err := r.ReadModel(ModelCommon)
	if err != nil {
		return Common{}, fmt.Errorf("sunspec: read common model: %w", err)
	}
	if len(data) < commonSNEnd {
		return Common{}, fmt.Errorf(
			"sunspec: read common model: got %d data registers, want at least %d: %w",
			len(data), commonSNEnd, ErrShortCommonModel)
	}

	c := Common{
		Manufacturer: regString(data[commonMnReg:commonMdReg]),
		Model:        regString(data[commonMdReg:commonOptReg]),
		Options:      regString(data[commonOptReg:commonVrReg]),
		Version:      regString(data[commonVrReg:commonSNReg]),
		Serial:       regString(data[commonSNReg:commonSNEnd]),
	}
	// DA is optional: it exists only when the model is long enough to include
	// it (L >= 65). A device reporting exactly commonSNEnd registers has none.
	if len(data) > commonDAReg {
		c.DeviceAddr = data[commonDAReg]
	}
	return c, nil
}

// regString decodes a fixed-length SunSpec string field. Each register holds
// two bytes, big-endian (high byte first). The field is NUL-padded on the
// right; per the SunSpec spec the value is trimmed at the first NUL byte and
// then of any trailing spaces.
//
// Non-ASCII bytes are passed through UNMODIFIED. This is an identity read: we
// report the device's bytes verbatim (a mojibake serial is still diagnostic)
// rather than sanitizing or validating UTF-8.
func regString(regs []uint16) string {
	buf := make([]byte, 0, len(regs)*2)
	for _, r := range regs {
		buf = append(buf, byte(r>>8), byte(r))
	}
	if i := bytes.IndexByte(buf, 0); i >= 0 {
		buf = buf[:i]
	}
	buf = bytes.TrimRight(buf, " ")
	return string(buf)
}
