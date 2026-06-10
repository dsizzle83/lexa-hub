// Package sunspec — declarative model-layout engine.
//
// SunSpec information models are ordered lists of typed points. Computing
// register offsets by hand is error-prone for the larger DER models (701/702/704
// have 60-110 points spanning a mix of 16-, 32-, and 64-bit values), so this file
// describes each model as an ordered []Field and derives offsets, total length,
// and typed accessors from that description.
//
// All multi-register values are big-endian (most-significant register first),
// per the SunSpec Modbus specification. Each numeric type has a documented
// "not implemented" sentinel; accessors return math.NaN() (for scaled floats)
// or report !ok (for integer getters) when a point is unimplemented on a device,
// so callers never mistake a sentinel for a real value on real hardware.
package sunspec

import "math"

// FieldType enumerates the SunSpec point data types this engine understands.
type FieldType uint8

const (
	Tuint16 FieldType = iota
	Tint16
	Tenum16
	Tbitfield16
	Tsunssf // int16 scale factor
	Tuint32
	Tint32
	Tenum32
	Tbitfield32
	Tacc32 // 32-bit accumulator (sentinel is 0, not 0xFFFFFFFF)
	Tuint64
	Tint64
	Tacc64
	Tstring // Field.Len registers, 2 chars each
	Tpad    // reserved register(s); ignored
)

// regs returns the number of 16-bit registers a field of this type occupies.
// strLen is the declared register length used only for Tstring/Tpad.
func (t FieldType) regs(strLen int) int {
	switch t {
	case Tuint16, Tint16, Tenum16, Tbitfield16, Tsunssf:
		return 1
	case Tuint32, Tint32, Tenum32, Tbitfield32, Tacc32:
		return 2
	case Tuint64, Tint64, Tacc64:
		return 4
	case Tstring, Tpad:
		return strLen
	}
	return 1
}

// Field describes a single point in a model layout.
type Field struct {
	Name string
	Type FieldType
	SF   string // name of this point's scale-factor field; "" if unscaled
	Len  int    // register count for Tstring / Tpad only
}

// F is a terse constructor for an unscaled field.
func F(name string, t FieldType) Field { return Field{Name: name, Type: t} }

// FS is a terse constructor for a scaled field (SF names another Tsunssf field).
func FS(name string, t FieldType, sf string) Field { return Field{Name: name, Type: t, SF: sf} }

// FStr is a terse constructor for a fixed-length string field.
func FStr(name string, regs int) Field { return Field{Name: name, Type: Tstring, Len: regs} }

// Layout is a compiled, offset-indexed model description.
type Layout struct {
	Fields []Field
	off    map[string]int
	typ    map[string]Field
	total  int
}

// NewLayout compiles an ordered field list into an offset-indexed Layout.
// Offsets are 0-based from the start of the model's data block (after the
// ID and L registers, which ReadModel already strips).
func NewLayout(fields ...Field) *Layout {
	l := &Layout{
		Fields: fields,
		off:    make(map[string]int, len(fields)),
		typ:    make(map[string]Field, len(fields)),
	}
	o := 0
	for _, f := range fields {
		if _, dup := l.off[f.Name]; dup && f.Name != "" {
			panic("sunspec: duplicate field " + f.Name)
		}
		l.off[f.Name] = o
		l.typ[f.Name] = f
		o += f.Type.regs(f.Len)
	}
	l.total = o
	return l
}

// Len is the total register count of one instance of this layout.
func (l *Layout) Len() int { return l.total }

// Has reports whether the layout defines a point with this name.
func (l *Layout) Has(name string) bool { _, ok := l.off[name]; return ok }

// Offset returns the 0-based register offset of a named point (-1 if absent).
func (l *Layout) Offset(name string) int {
	if o, ok := l.off[name]; ok {
		return o
	}
	return -1
}

// View binds a layout to a register slice for reading and writing.
func (l *Layout) View(regs []uint16) View { return View{regs: regs, l: l, base: 0} }

// ViewAt binds a layout to a register slice with a base offset, used for
// repeating sub-groups (e.g. one curve within a curve list).
func (l *Layout) ViewAt(regs []uint16, base int) View { return View{regs: regs, l: l, base: base} }

// View is a typed cursor over a register slice for one layout instance.
type View struct {
	regs []uint16
	l    *Layout
	base int
}

// in-bounds register read; returns 0 when out of range so partial models
// (a device that implements fewer optional points) never panic.
func (v View) reg(o int) uint16 {
	i := v.base + o
	if i >= 0 && i < len(v.regs) {
		return v.regs[i]
	}
	return 0
}

func (v View) setReg(o int, val uint16) {
	i := v.base + o
	if i >= 0 && i < len(v.regs) {
		v.regs[i] = val
	}
}

func (v View) fieldOff(name string) (int, Field, bool) {
	o, ok := v.l.off[name]
	if !ok {
		return 0, Field{}, false
	}
	return o, v.l.typ[name], true
}

// Present reports whether the field exists in the layout AND its registers are
// within the bound slice (i.e. the device actually implements this far).
func (v View) Present(name string) bool {
	o, f, ok := v.fieldOff(name)
	if !ok {
		return false
	}
	return v.base+o+f.Type.regs(f.Len) <= len(v.regs)
}

// rawU32/rawU64 assemble multi-register values big-endian (high word first).
func (v View) rawU32(o int) uint32 { return uint32(v.reg(o))<<16 | uint32(v.reg(o+1)) }
func (v View) rawU64(o int) uint64 {
	return uint64(v.reg(o))<<48 | uint64(v.reg(o+1))<<32 | uint64(v.reg(o+2))<<16 | uint64(v.reg(o+3))
}

// ── Sentinels ────────────────────────────────────────────────────────────────

const (
	sentU16 = uint16(0xFFFF)
	sentI16 = uint16(0x8000)
	sentU32 = uint32(0xFFFFFFFF)
	sentI32 = uint32(0x80000000)
	sentU64 = uint64(0xFFFFFFFFFFFFFFFF)
	sentI64 = uint64(0x8000000000000000)
)

// notImpl reports whether the raw value of a field equals its type's
// "not implemented" sentinel.
func (v View) notImpl(o int, f Field) bool {
	switch f.Type {
	case Tuint16, Tenum16, Tbitfield16:
		return v.reg(o) == sentU16
	case Tint16, Tsunssf:
		return v.reg(o) == sentI16
	case Tuint32, Tenum32, Tbitfield32:
		return v.rawU32(o) == sentU32
	case Tint32:
		return v.rawU32(o) == sentI32
	case Tuint64:
		return v.rawU64(o) == sentU64
	case Tint64:
		return v.rawU64(o) == sentI64
	}
	// Accumulators have no NaN sentinel; 0 is a valid (un-accumulated) value.
	return false
}

// ── Integer getters (ok=false on absent or sentinel) ─────────────────────────

func (v View) Enum(name string) (uint16, bool) {
	o, f, ok := v.fieldOff(name)
	if !ok || !v.Present(name) || v.notImpl(o, f) {
		return 0, false
	}
	return v.reg(o), true
}

func (v View) Bool(name string) bool {
	val, ok := v.Enum(name)
	return ok && val == 1
}

func (v View) Bitfield32(name string) uint32 {
	o, f, ok := v.fieldOff(name)
	if !ok || !v.Present(name) || v.notImpl(o, f) {
		return 0
	}
	return v.rawU32(o)
}

func (v View) U32(name string) (uint32, bool) {
	o, f, ok := v.fieldOff(name)
	if !ok || !v.Present(name) || v.notImpl(o, f) {
		return 0, false
	}
	return v.rawU32(o), true
}

func (v View) U64(name string) (uint64, bool) {
	o, f, ok := v.fieldOff(name)
	if !ok || !v.Present(name) || v.notImpl(o, f) {
		return 0, false
	}
	return v.rawU64(o), true
}

// SF returns the int16 scale-factor value of a named sunssf field. ok=false
// when absent or unimplemented.
func (v View) SF(name string) (int16, bool) {
	o, f, ok := v.fieldOff(name)
	if !ok || !v.Present(name) || f.Type != Tsunssf || v.reg(o) == sentI16 {
		return 0, false
	}
	return int16(v.reg(o)), true
}

// ── Scaled float getter ──────────────────────────────────────────────────────

// Float reads a numeric point and applies its scale factor, returning the
// engineering value. Returns NaN when the point or its scale factor is absent
// or carries a not-implemented sentinel. Signed vs unsigned is taken from the
// field's declared type. PF-style ×100 fields are NOT special-cased here; the
// caller divides by 100 where the spec uses centi-units.
func (v View) Float(name string) float64 {
	o, f, ok := v.fieldOff(name)
	if !ok || !v.Present(name) || v.notImpl(o, f) {
		return math.NaN()
	}
	sf := int16(0)
	if f.SF != "" {
		s, ok := v.SF(f.SF)
		if !ok {
			return math.NaN()
		}
		sf = s
	}
	var raw float64
	switch f.Type {
	case Tint16, Tsunssf:
		raw = float64(int16(v.reg(o)))
	case Tuint16, Tenum16:
		raw = float64(v.reg(o))
	case Tint32:
		raw = float64(int32(v.rawU32(o)))
	case Tuint32, Tacc32:
		raw = float64(v.rawU32(o))
	case Tint64:
		raw = float64(int64(v.rawU64(o)))
	case Tuint64, Tacc64:
		raw = float64(v.rawU64(o))
	default:
		return math.NaN()
	}
	return raw * math.Pow10(int(sf))
}

// ── Setters ──────────────────────────────────────────────────────────────────

// SetEnum writes a uint16/enum16 point.
func (v View) SetEnum(name string, val uint16) {
	if o, _, ok := v.fieldOff(name); ok {
		v.setReg(o, val)
	}
}

// SetBool writes 1 (true) or 0 (false) to an enable point.
func (v View) SetBool(name string, b bool) {
	if b {
		v.SetEnum(name, 1)
	} else {
		v.SetEnum(name, 0)
	}
}

// SetU32 writes a big-endian uint32 point.
func (v View) SetU32(name string, val uint32) {
	if o, _, ok := v.fieldOff(name); ok {
		v.setReg(o, uint16(val>>16))
		v.setReg(o+1, uint16(val))
	}
}

// SetFloat writes an engineering value to a scaled point, encoding it with the
// point's scale factor (read live from the slice). Out-of-range values clamp to
// the field's representable range. Unknown/NaN scale factor → no-op.
func (v View) SetFloat(name string, val float64) {
	o, f, ok := v.fieldOff(name)
	if !ok || math.IsNaN(val) {
		return
	}
	sf := int16(0)
	if f.SF != "" {
		s, ok := v.SF(f.SF)
		if !ok {
			return
		}
		sf = s
	}
	scaled := math.Round(val / math.Pow10(int(sf)))
	switch f.Type {
	case Tint16, Tsunssf:
		v.setReg(o, uint16(int16(clamp(scaled, math.MinInt16, math.MaxInt16))))
	case Tuint16, Tenum16:
		v.setReg(o, uint16(clamp(scaled, 0, math.MaxUint16)))
	case Tint32:
		x := int32(clamp(scaled, math.MinInt32, math.MaxInt32))
		v.setReg(o, uint16(uint32(x)>>16))
		v.setReg(o+1, uint16(uint32(x)))
	case Tuint32, Tacc32:
		x := uint32(clamp(scaled, 0, math.MaxUint32))
		v.setReg(o, uint16(x>>16))
		v.setReg(o+1, uint16(x))
	}
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ── Absolute-offset accessors (for repeating curve / port groups) ────────────
//
// Repeating sub-groups (curve points, DC ports) reference scale factors that
// live in the model header, so they cannot use a base-shifted sub-View. These
// helpers read/write a raw register at an absolute model offset and apply a
// scale factor resolved by name from the (full-model) View.

func (v View) U16At(o int) uint16 { return v.reg(o) }
func (v View) I16At(o int) int16  { return int16(v.reg(o)) }
func (v View) U32At(o int) uint32 { return v.rawU32(o) }
func (v View) U64At(o int) uint64 { return v.rawU64(o) }
func (v View) SetU16At(o int, val uint16) { v.setReg(o, val) }

// ScaleSignedAt reads an int16 at offset o and scales it by the named SF.
func (v View) ScaleSignedAt(o int, sfName string) float64 {
	s, ok := v.SF(sfName)
	if !ok || v.reg(o) == sentI16 {
		return math.NaN()
	}
	return float64(int16(v.reg(o))) * math.Pow10(int(s))
}

// ScaleUintAt reads a uint16 at offset o and scales it by the named SF.
func (v View) ScaleUintAt(o int, sfName string) float64 {
	s, ok := v.SF(sfName)
	if !ok || v.reg(o) == sentU16 {
		return math.NaN()
	}
	return float64(v.reg(o)) * math.Pow10(int(s))
}

// ScaleU32At reads a uint32 at offset o (e.g. frequency points) and scales it.
func (v View) ScaleU32At(o int, sfName string) float64 {
	s, ok := v.SF(sfName)
	if !ok || v.rawU32(o) == sentU32 {
		return math.NaN()
	}
	return float64(v.rawU32(o)) * math.Pow10(int(s))
}

// SetScaledSignedAt encodes val as an int16 at offset o using the named SF.
func (v View) SetScaledSignedAt(o int, val float64, sfName string) {
	s, ok := v.SF(sfName)
	if !ok || math.IsNaN(val) {
		return
	}
	r := math.Round(val / math.Pow10(int(s)))
	v.setReg(o, uint16(int16(clamp(r, math.MinInt16, math.MaxInt16))))
}

// SetScaledUintAt encodes val as a uint16 at offset o using the named SF.
func (v View) SetScaledUintAt(o int, val float64, sfName string) {
	s, ok := v.SF(sfName)
	if !ok || math.IsNaN(val) {
		return
	}
	r := math.Round(val / math.Pow10(int(s)))
	v.setReg(o, uint16(clamp(r, 0, math.MaxUint16)))
}

// SetScaledU32At encodes val as a big-endian uint32 at offset o using the SF.
func (v View) SetScaledU32At(o int, val float64, sfName string) {
	s, ok := v.SF(sfName)
	if !ok || math.IsNaN(val) {
		return
	}
	x := uint32(clamp(math.Round(val/math.Pow10(int(s))), 0, math.MaxUint32))
	v.setReg(o, uint16(x>>16))
	v.setReg(o+1, uint16(x))
}
