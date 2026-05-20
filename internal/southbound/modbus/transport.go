// Package modbus provides a transport-layer abstraction for Modbus register
// access. Higher-level packages (sunspec, inverter) program against the
// Transport interface, not against a specific physical medium.
//
// The physical medium is selected by the URL passed to NewTransport:
//
//	"tcp://192.168.1.100:502"        — Modbus/TCP
//	"rtu:///dev/ttyUSB0"             — Modbus RTU over RS-485
//	"rtuovertcp://192.168.1.100:502" — RTU framing over TCP
//
// Adding a new physical layer means implementing Transport — no changes
// needed in the sunspec or inverter packages.
package modbus

import (
	"fmt"
	"time"

	modbuslib "github.com/simonvetter/modbus"
)

// Transport abstracts Modbus register access. Implementations are decoupled
// from any particular physical medium.
type Transport interface {
	// Open establishes the connection. Must be called before any register op.
	Open() error
	// Close releases the connection.
	Close() error
	// SetUnitID selects the target slave/unit (default 1 for most devices).
	SetUnitID(id uint8) error
	// ReadHolding reads quantity holding registers starting at addr (0-based).
	ReadHolding(addr, quantity uint16) ([]uint16, error)
	// WriteHolding writes values to holding registers starting at addr.
	WriteHolding(addr uint16, values []uint16) error
	// ReadInput reads quantity input registers starting at addr (0-based).
	ReadInput(addr, quantity uint16) ([]uint16, error)
}

// client wraps a simonvetter ModbusClient to implement Transport.
type client struct {
	inner *modbuslib.ModbusClient
}

// NewTransport creates a Transport using the given Modbus URL and per-request
// timeout. The returned Transport is not connected — call Open() before use.
func NewTransport(url string, timeout time.Duration) (Transport, error) {
	mc, err := modbuslib.NewClient(&modbuslib.ClientConfiguration{
		URL:     url,
		Timeout: timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("modbus: new client %q: %w", url, err)
	}
	return &client{inner: mc}, nil
}

func (c *client) Open() error {
	return c.inner.Open()
}

func (c *client) Close() error {
	return c.inner.Close()
}

func (c *client) SetUnitID(id uint8) error {
	return c.inner.SetUnitId(id)
}

func (c *client) ReadHolding(addr, quantity uint16) ([]uint16, error) {
	return c.inner.ReadRegisters(addr, quantity, modbuslib.HOLDING_REGISTER)
}

func (c *client) WriteHolding(addr uint16, values []uint16) error {
	return c.inner.WriteRegisters(addr, values)
}

func (c *client) ReadInput(addr, quantity uint16) ([]uint16, error) {
	return c.inner.ReadRegisters(addr, quantity, modbuslib.INPUT_REGISTER)
}
