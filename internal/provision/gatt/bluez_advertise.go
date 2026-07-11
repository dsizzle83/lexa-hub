package gatt

import (
	"fmt"
	"sync"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
)

// This file is the HARDWARE layer's advertising half (unit B2, Phase C): an
// org.bluez.LEAdvertisement1 object plus a bluezAdManager that registers /
// unregisters it with org.bluez.LEAdvertisingManager1. The advertise/silence
// POLICY (advertise iff uncommissioned) lives in the pure Advertiser +
// MarkerGate (advertise.go) and is unit-tested with a fake AdManager;
// bluezAdManager is the real AdManager the Advertiser drives on the dev kit.

const (
	leAdvertisement1    = "org.bluez.LEAdvertisement1"
	leAdvertisingMgr1   = "org.bluez.LEAdvertisingManager1"
	advertisementPath   = dbus.ObjectPath("/com/lexa/provision/advertisement0")
	registerAdvertise   = leAdvertisingMgr1 + ".RegisterAdvertisement"
	unregisterAdvertise = leAdvertisingMgr1 + ".UnregisterAdvertisement"
)

// advertisement is the exported org.bluez.LEAdvertisement1 object.
type advertisement struct {
	localName   string
	serviceUUID string
	onRelease   func()
}

func (a *advertisement) propMap() map[string]dbus.Variant {
	return map[string]dbus.Variant{
		"Type":         dbus.MakeVariant("peripheral"),
		"ServiceUUIDs": dbus.MakeVariant([]string{a.serviceUUID}),
		"LocalName":    dbus.MakeVariant(a.localName),
		// tx-power is in the adapter's SupportedIncludes on the dev kit
		// (confirmed by busctl probe); it helps the app estimate proximity.
		"Includes": dbus.MakeVariant([]string{"tx-power"}),
	}
}

// Release implements org.bluez.LEAdvertisement1.Release: BlueZ calls it when it
// drops the advertisement on its own (e.g. adapter reset).
func (a *advertisement) Release() *dbus.Error {
	if a.onRelease != nil {
		a.onRelease()
	}
	return nil
}

func (a *advertisement) Get(iface, prop string) (dbus.Variant, *dbus.Error) {
	if iface != leAdvertisement1 {
		return dbus.Variant{}, errUnknownInterface
	}
	v, ok := a.propMap()[prop]
	if !ok {
		return dbus.Variant{}, errUnknownProperty
	}
	return v, nil
}

func (a *advertisement) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	if iface != leAdvertisement1 {
		return nil, errUnknownInterface
	}
	return a.propMap(), nil
}

func (a *advertisement) Set(_ string, _ string, _ dbus.Variant) *dbus.Error {
	return errReadOnly
}

// bluezAdManager is the production AdManager: it exports the LEAdvertisement1
// object on Register and calls LEAdvertisingManager1.RegisterAdvertisement /
// UnregisterAdvertisement. Register/Unregister are idempotent, as the
// Advertiser contract requires.
type bluezAdManager struct {
	conn    *dbus.Conn
	adapter dbus.ObjectPath
	ad      *advertisement

	mu       sync.Mutex
	exported bool
	active   bool
}

// NewBluezAdManager builds the production AdManager for adapter, advertising
// localName + the service UUID.
func NewBluezAdManager(conn *dbus.Conn, adapter dbus.ObjectPath, localName, serviceUUID string) *bluezAdManager {
	return &bluezAdManager{
		conn:    conn,
		adapter: adapter,
		ad:      &advertisement{localName: localName, serviceUUID: serviceUUID},
	}
}

// Register exports the advertisement object (once) and registers it with the
// adapter.
func (m *bluezAdManager) Register() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active {
		return nil
	}
	if !m.exported {
		if err := m.conn.Export(m.ad, advertisementPath, leAdvertisement1); err != nil {
			return fmt.Errorf("export advertisement: %w", err)
		}
		if err := m.conn.Export(m.ad, advertisementPath, ifaceProperties); err != nil {
			return fmt.Errorf("export advertisement props: %w", err)
		}
		if err := m.conn.Export(introspect.NewIntrospectable(advertisementNode()), advertisementPath, ifaceIntrospect); err != nil {
			return fmt.Errorf("export advertisement introspect: %w", err)
		}
		m.exported = true
	}
	obj := m.conn.Object(bluezService, m.adapter)
	call := obj.Call(registerAdvertise, 0, advertisementPath, map[string]dbus.Variant{})
	if call.Err != nil {
		return fmt.Errorf("RegisterAdvertisement on %s: %w", m.adapter, call.Err)
	}
	m.active = true
	return nil
}

// Unregister removes the advertisement from the adapter (the exported object
// stays exported, ready to re-register cheaply).
func (m *bluezAdManager) Unregister() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.active {
		return nil
	}
	obj := m.conn.Object(bluezService, m.adapter)
	call := obj.Call(unregisterAdvertise, 0, advertisementPath)
	m.active = false
	if call.Err != nil {
		return fmt.Errorf("UnregisterAdvertisement on %s: %w", m.adapter, call.Err)
	}
	return nil
}
