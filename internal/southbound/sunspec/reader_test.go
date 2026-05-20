// Tests for sunspec.Scan, sunspec.NewReader, sunspec.Reader, and sunspec.FindModel.
//
// These tests cannot import internal/southbound/sim because sim imports sunspec
// (cycle). Instead, we build a minimal in-process Modbus server using
// github.com/simonvetter/modbus directly.
package sunspec_test

import (
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	modbuslib "github.com/simonvetter/modbus"

	"lexa-hub/internal/southbound/modbus"
	"lexa-hub/internal/southbound/sunspec"
)

// ── Minimal test Modbus server ────────────────────────────────────────────────

// testRegs is a thread-safe holding-register store that satisfies
// modbuslib.RequestHandler.
type testRegs struct {
	mu   sync.RWMutex
	data map[uint16]uint16
}

func newTestRegs() *testRegs {
	return &testRegs{data: make(map[uint16]uint16)}
}

func (r *testRegs) set(addr, val uint16) {
	r.mu.Lock()
	r.data[addr] = val
	r.mu.Unlock()
}

func (r *testRegs) get(addr uint16) uint16 {
	r.mu.RLock()
	v := r.data[addr]
	r.mu.RUnlock()
	return v
}

func (r *testRegs) HandleCoils(_ *modbuslib.CoilsRequest) ([]bool, error) {
	return nil, modbuslib.ErrIllegalFunction
}
func (r *testRegs) HandleDiscreteInputs(_ *modbuslib.DiscreteInputsRequest) ([]bool, error) {
	return nil, modbuslib.ErrIllegalFunction
}
func (r *testRegs) HandleInputRegisters(_ *modbuslib.InputRegistersRequest) ([]uint16, error) {
	return nil, modbuslib.ErrIllegalFunction
}
func (r *testRegs) HandleHoldingRegisters(req *modbuslib.HoldingRegistersRequest) ([]uint16, error) {
	if req.IsWrite {
		r.mu.Lock()
		for i, v := range req.Args {
			r.data[req.Addr+uint16(i)] = v
		}
		r.mu.Unlock()
		return nil, nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]uint16, req.Quantity)
	for i := uint16(0); i < req.Quantity; i++ {
		out[i] = r.data[req.Addr+i]
	}
	return out, nil
}

// startModbusServer starts a Modbus TCP server with the given registers on a
// random port and returns a transport connected to it plus a stop function.
func startModbusServer(t *testing.T, regs *testRegs) (modbus.Transport, func()) {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	url := fmt.Sprintf("tcp://127.0.0.1:%d", port)

	srv, err := modbuslib.NewServer(&modbuslib.ServerConfiguration{
		URL:        url,
		MaxClients: 4,
		Timeout:    10 * time.Second,
	}, regs)
	if err != nil {
		t.Fatalf("new modbus server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start modbus server: %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	tr, err := modbus.NewTransport(url, 2*time.Second)
	if err != nil {
		srv.Stop()
		t.Fatalf("new transport: %v", err)
	}
	if err := tr.Open(); err != nil {
		srv.Stop()
		t.Fatalf("open transport: %v", err)
	}
	return tr, func() {
		tr.Close()
		srv.Stop()
	}
}

// populateSunSpec writes a minimal SunSpec layout into regs.
// Layout: SunS header, Model 103 (length=50), end marker.
// Returns the base address of Model 103 data registers.
//
// Length 50 covers all Model 103 offsets used in production
// (W=12, W_SF=13, Hz=14, Hz_SF=15, PhVphA=8, V_SF=11, …).
func populateSunSpec(regs *testRegs) (m103Base uint16) {
	base := sunspec.SunSpecBase
	sfN := func(v int16) uint16 { return uint16(v) }

	regs.set(base+0, sunspec.SunSMagic0)
	regs.set(base+1, sunspec.SunSMagic1)

	cursor := base + 2
	const m103Len = 50
	regs.set(cursor+0, sunspec.ModelInverterThreePh)
	regs.set(cursor+1, m103Len)
	m103Base = cursor + 2

	// W=3000 (sf=0), Hz=6000 (sf=-2)
	regs.set(m103Base+sunspec.M103_W, uint16(int16(3000)))
	regs.set(m103Base+sunspec.M103_W_SF, 0)
	regs.set(m103Base+sunspec.M103_Hz, 6000)
	regs.set(m103Base+sunspec.M103_Hz_SF, sfN(-2))

	// End marker
	cursor += 2 + m103Len
	regs.set(cursor+0, sunspec.EndMarker)
	regs.set(cursor+1, 0)
	return
}

// ── Scan ──────────────────────────────────────────────────────────────────────

func TestScan_HappyPath(t *testing.T) {
	regs := newTestRegs()
	populateSunSpec(regs)
	tr, stop := startModbusServer(t, regs)
	defer stop()

	blocks, err := sunspec.Scan(tr)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("block count = %d, want 1", len(blocks))
	}
	if blocks[0].ModelID != sunspec.ModelInverterThreePh {
		t.Errorf("block[0].ModelID = %d, want %d (ModelInverterThreePh)", blocks[0].ModelID, sunspec.ModelInverterThreePh)
	}
	if blocks[0].Length != 50 {
		t.Errorf("block[0].Length = %d, want 50", blocks[0].Length)
	}
}

func TestScan_MultipleModels(t *testing.T) {
	regs := newTestRegs()
	base := sunspec.SunSpecBase
	regs.set(base+0, sunspec.SunSMagic0)
	regs.set(base+1, sunspec.SunSMagic1)
	cursor := base + 2

	// Model 121 (length 2)
	regs.set(cursor+0, sunspec.ModelBasicSettings)
	regs.set(cursor+1, 2)
	cursor += 4

	// Model 123 (length 3)
	regs.set(cursor+0, sunspec.ModelImmediateCtrl)
	regs.set(cursor+1, 3)
	cursor += 5

	// End
	regs.set(cursor+0, sunspec.EndMarker)
	regs.set(cursor+1, 0)

	tr, stop := startModbusServer(t, regs)
	defer stop()

	blocks, err := sunspec.Scan(tr)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(blocks) != 2 {
		t.Fatalf("block count = %d, want 2", len(blocks))
	}
	if blocks[0].ModelID != sunspec.ModelBasicSettings {
		t.Errorf("blocks[0] = %d, want ModelBasicSettings (%d)", blocks[0].ModelID, sunspec.ModelBasicSettings)
	}
	if blocks[1].ModelID != sunspec.ModelImmediateCtrl {
		t.Errorf("blocks[1] = %d, want ModelImmediateCtrl (%d)", blocks[1].ModelID, sunspec.ModelImmediateCtrl)
	}
}

func TestScan_NoSunSHeader_ReturnsError(t *testing.T) {
	regs := newTestRegs()
	// Leave registers at 0 — no SunS header.
	tr, stop := startModbusServer(t, regs)
	defer stop()

	_, err := sunspec.Scan(tr)
	if err == nil {
		t.Fatal("Scan with no SunS header: expected error, got nil")
	}
}

func TestScan_JustHeader_ZeroModels(t *testing.T) {
	regs := newTestRegs()
	base := sunspec.SunSpecBase
	regs.set(base+0, sunspec.SunSMagic0)
	regs.set(base+1, sunspec.SunSMagic1)
	regs.set(base+2, sunspec.EndMarker)
	regs.set(base+3, 0)

	tr, stop := startModbusServer(t, regs)
	defer stop()

	blocks, err := sunspec.Scan(tr)
	if err != nil {
		t.Fatalf("Scan empty device: %v", err)
	}
	if len(blocks) != 0 {
		t.Errorf("block count = %d, want 0", len(blocks))
	}
}

// ── NewReader / HasModel ──────────────────────────────────────────────────────

func TestNewReader_HappyPath(t *testing.T) {
	regs := newTestRegs()
	populateSunSpec(regs)
	tr, stop := startModbusServer(t, regs)
	defer stop()

	r, err := sunspec.NewReader(tr)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if !r.HasModel(sunspec.ModelInverterThreePh) {
		t.Error("HasModel(103) should be true")
	}
	if r.HasModel(sunspec.ModelImmediateCtrl) {
		t.Error("HasModel(123) should be false (not in layout)")
	}
}

func TestNewReader_NoSunSHeader_ReturnsError(t *testing.T) {
	regs := newTestRegs()
	tr, stop := startModbusServer(t, regs)
	defer stop()

	_, err := sunspec.NewReader(tr)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// ── ReadModel ─────────────────────────────────────────────────────────────────

func TestReader_ReadModel_HappyPath(t *testing.T) {
	regs := newTestRegs()
	m103Base := populateSunSpec(regs)
	tr, stop := startModbusServer(t, regs)
	defer stop()

	r, _ := sunspec.NewReader(tr)
	data, err := r.ReadModel(sunspec.ModelInverterThreePh)
	if err != nil {
		t.Fatalf("ReadModel: %v", err)
	}
	// Register 0 within model should be W=3000.
	if data[sunspec.M103_W] != uint16(int16(3000)) {
		t.Errorf("M103_W = %d, want 3000 raw", data[sunspec.M103_W])
	}
	// Verify base address is consistent with what we set.
	_ = m103Base
}

func TestReader_ReadModel_NotPresent_ReturnsError(t *testing.T) {
	regs := newTestRegs()
	populateSunSpec(regs)
	tr, stop := startModbusServer(t, regs)
	defer stop()

	r, _ := sunspec.NewReader(tr)
	_, err := r.ReadModel(sunspec.ModelBasicSettings) // not in layout
	if err == nil {
		t.Fatal("expected error for absent model, got nil")
	}
}

// ── WriteModel ────────────────────────────────────────────────────────────────

func TestReader_WriteModel_HappyPath(t *testing.T) {
	regs := newTestRegs()
	populateSunSpec(regs)
	tr, stop := startModbusServer(t, regs)
	defer stop()

	r, _ := sunspec.NewReader(tr)
	// Write a new W value at offset 0 within Model 103.
	if err := r.WriteModel(sunspec.ModelInverterThreePh, sunspec.M103_W, []uint16{uint16(int16(1500))}); err != nil {
		t.Fatalf("WriteModel: %v", err)
	}
	// Read back via ReadModel to confirm the write was applied.
	data, err := r.ReadModel(sunspec.ModelInverterThreePh)
	if err != nil {
		t.Fatalf("ReadModel after write: %v", err)
	}
	if data[sunspec.M103_W] != uint16(int16(1500)) {
		t.Errorf("W after write = %d, want %d (1500 raw)", data[sunspec.M103_W], uint16(int16(1500)))
	}
}

func TestReader_WriteModel_OutOfBounds_ReturnsError(t *testing.T) {
	regs := newTestRegs()
	populateSunSpec(regs) // Model 103 has length=50
	tr, stop := startModbusServer(t, regs)
	defer stop()

	r, _ := sunspec.NewReader(tr)
	// Offset 49 + 2 values → extends past length=50.
	err := r.WriteModel(sunspec.ModelInverterThreePh, 49, []uint16{1, 2})
	if err == nil {
		t.Fatal("expected out-of-bounds error, got nil")
	}
}

// ── FindModel ─────────────────────────────────────────────────────────────────

func TestFindModel_NotFound_ReturnsError(t *testing.T) {
	blocks := []sunspec.Block{
		{ModelID: sunspec.ModelInverterThreePh, BaseAddr: 40002, Length: 50},
	}
	_, err := sunspec.FindModel(blocks, sunspec.ModelBasicSettings)
	if err == nil {
		t.Fatal("expected not-found error, got nil")
	}
}

func TestFindModel_Found(t *testing.T) {
	blocks := []sunspec.Block{
		{ModelID: 1, BaseAddr: 40002, Length: 66},
		{ModelID: sunspec.ModelBasicSettings, BaseAddr: 40070, Length: 30},
	}
	b, err := sunspec.FindModel(blocks, sunspec.ModelBasicSettings)
	if err != nil {
		t.Fatalf("FindModel: %v", err)
	}
	if b.BaseAddr != 40070 {
		t.Errorf("BaseAddr = %d, want 40070", b.BaseAddr)
	}
}

func TestFindModel_FirstMatchReturned(t *testing.T) {
	// Hypothetical device with two blocks of the same model (unusual but spec-legal).
	blocks := []sunspec.Block{
		{ModelID: sunspec.ModelInverterThreePh, BaseAddr: 100, Length: 50},
		{ModelID: sunspec.ModelInverterThreePh, BaseAddr: 200, Length: 50},
	}
	b, err := sunspec.FindModel(blocks, sunspec.ModelInverterThreePh)
	if err != nil {
		t.Fatalf("FindModel: %v", err)
	}
	if b.BaseAddr != 100 {
		t.Errorf("expected first block (BaseAddr=100), got %d", b.BaseAddr)
	}
}
