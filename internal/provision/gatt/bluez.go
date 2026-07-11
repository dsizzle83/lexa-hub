package gatt

import (
	"fmt"
	"sync"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
)

// This file is the HARDWARE layer (unit B2, Phase C on the dev kit): a BlueZ
// GATT server over godbus/dbus/v5. It exports the ADR-0002 object tree onto the
// system bus and registers it with org.bluez.GattManager1 so BlueZ presents it
// as a real GATT service. None of this is exercised in CI — it needs a live
// BlueZ D-Bus — so the LOGIC it drives lives in the pure layer (dispatch.go /
// advertise.go), which is unit-tested. Keep it that way.
//
// Object hierarchy registered with GattManager1.RegisterApplication:
//
//	/com/lexa/provision                      (ObjectManager — the "application")
//	  /com/lexa/provision/service0           (org.bluez.GattService1)
//	    /com/lexa/provision/service0/char0   (org.bluez.GattCharacteristic1) info
//	    /com/lexa/provision/service0/char1   session
//	    /com/lexa/provision/service0/char2   wifi
//	    /com/lexa/provision/service0/char3   config
//	    /com/lexa/provision/service0/char4   status
//
// BlueZ enumerates the whole tree once by calling ObjectManager.GetManagedObjects
// at RegisterApplication time, so every characteristic's UUID/Flags/Value is
// reported there. There are FIVE characteristics under one service (info,
// session, wifi, config, status — the sec1.UUID* set); together with the
// service UUID that is six LEXA UUIDs total.
//
// Notification path (chosen per the B2 brief): the "Value + PropertiesChanged"
// path, not AcquireNotify FDs. To push a framed chunk out on an indicate/notify
// characteristic we set its Value property and emit
// org.freedesktop.DBus.Properties.PropertiesChanged; BlueZ forwards it to the
// subscribed central as an ATT notification/indication. This is simpler than
// owning a SEQPACKET FD and is MTU-adequate: sec1 already frames every message
// into ATT-sized chunks (frame.Chunk against the negotiated payload), so each
// Value update carries exactly one on-wire frame and no chunk ever exceeds the
// MTU.

// D-Bus interface + object-path constants for the GATT application.
const (
	bluezService     = "org.bluez"
	gattService1     = "org.bluez.GattService1"
	gattChar1        = "org.bluez.GattCharacteristic1"
	gattManager1     = "org.bluez.GattManager1"
	ifaceObjectMgr   = "org.freedesktop.DBus.ObjectManager"
	ifaceProperties  = "org.freedesktop.DBus.Properties"
	ifaceIntrospect  = "org.freedesktop.DBus.Introspectable"
	propChangedSig   = "org.freedesktop.DBus.Properties.PropertiesChanged"
	appRootPath      = dbus.ObjectPath("/com/lexa/provision")
	appServicePath   = dbus.ObjectPath("/com/lexa/provision/service0")
	adapterPathBase  = "/org/bluez/"
	defaultAdapter   = "hci0"
	getManagedMethod = gattManager1 + ".RegisterApplication"
)

// Standard D-Bus property/method errors returned to BlueZ.
var (
	errUnknownInterface = dbus.NewError("org.freedesktop.DBus.Error.UnknownInterface", nil)
	errUnknownProperty  = dbus.NewError("org.freedesktop.DBus.Error.UnknownProperty", nil)
	errReadOnly         = dbus.NewError("org.freedesktop.DBus.Error.PropertyReadOnly", nil)
)

// AdapterPath maps an adapter name (e.g. "hci0") to its BlueZ object path
// (/org/bluez/hci0). An empty name defaults to hci0.
func AdapterPath(adapter string) dbus.ObjectPath {
	if adapter == "" {
		adapter = defaultAdapter
	}
	return dbus.ObjectPath(adapterPathBase + adapter)
}

// characteristic is one GATT characteristic object exported on the bus. It
// implements org.bluez.GattCharacteristic1 (ReadValue/WriteValue/StartNotify/
// StopNotify) and org.freedesktop.DBus.Properties (Get/GetAll/Set) on its path.
type characteristic struct {
	server  *Server
	path    dbus.ObjectPath
	uuid    string
	flags   []string
	isWrite bool // WriteValue routes to the dispatcher
	isInfo  bool // ReadValue returns the live info document

	mu        sync.Mutex
	value     []byte
	notifying bool
}

// hasNotify reports whether this characteristic advertises notify/indicate (so
// its property set includes Notifying and it can push Value updates).
func (c *characteristic) hasNotify() bool {
	for _, f := range c.flags {
		if f == "notify" || f == "indicate" {
			return true
		}
	}
	return false
}

// ReadValue implements org.bluez.GattCharacteristic1.ReadValue. Only info is
// readable; it returns the live info document (build version + serial +
// commissioned truth). A read of any other characteristic returns its current
// Value (normally empty — the encrypted characteristics are push-only).
func (c *characteristic) ReadValue(_ map[string]dbus.Variant) ([]byte, *dbus.Error) {
	if c.isInfo {
		v, err := c.server.disp.InfoValue()
		if err != nil {
			return nil, dbus.MakeFailedError(err)
		}
		return v, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.value...), nil
}

// WriteValue implements org.bluez.GattCharacteristic1.WriteValue: one GATT
// write is one sec1 frame. It feeds the chunk to the dispatcher and pushes any
// framed responses back out on their characteristics as notifications.
func (c *characteristic) WriteValue(value []byte, _ map[string]dbus.Variant) *dbus.Error {
	if !c.isWrite {
		return errReadOnly
	}
	outs := c.server.disp.OnWrite(c.uuid, value)
	for _, o := range outs {
		c.server.push(o.UUID, o.Chunk)
	}
	return nil
}

// StartNotify / StopNotify implement the CCCD subscription BlueZ mediates.
func (c *characteristic) StartNotify() *dbus.Error {
	c.mu.Lock()
	c.notifying = true
	c.mu.Unlock()
	return nil
}

func (c *characteristic) StopNotify() *dbus.Error {
	c.mu.Lock()
	c.notifying = false
	c.mu.Unlock()
	return nil
}

// setValueNotify updates Value and emits PropertiesChanged so BlueZ delivers
// the chunk to the subscribed central.
func (c *characteristic) setValueNotify(chunk []byte) {
	c.mu.Lock()
	c.value = append([]byte(nil), chunk...)
	c.mu.Unlock()
	changed := map[string]dbus.Variant{"Value": dbus.MakeVariant(chunk)}
	// Best-effort: a failed emit is a lost indication, which the protocol
	// tolerates (the central re-drives on timeout); it must never panic the
	// service.
	_ = c.server.conn.Emit(c.path, propChangedSig, gattChar1, changed, []string{})
}

// propMap is the GattCharacteristic1 property set reported to BlueZ (via both
// GetManagedObjects and Properties.GetAll).
func (c *characteristic) propMap() map[string]dbus.Variant {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := map[string]dbus.Variant{
		"UUID":    dbus.MakeVariant(c.uuid),
		"Service": dbus.MakeVariant(appServicePath),
		"Flags":   dbus.MakeVariant(c.flags),
		"Value":   dbus.MakeVariant(append([]byte(nil), c.value...)),
	}
	if c.hasNotify() {
		p["Notifying"] = dbus.MakeVariant(c.notifying)
	}
	return p
}

// Get / GetAll / Set implement org.freedesktop.DBus.Properties for this char.
func (c *characteristic) Get(iface, prop string) (dbus.Variant, *dbus.Error) {
	if iface != gattChar1 {
		return dbus.Variant{}, errUnknownInterface
	}
	v, ok := c.propMap()[prop]
	if !ok {
		return dbus.Variant{}, errUnknownProperty
	}
	return v, nil
}

func (c *characteristic) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	if iface != gattChar1 {
		return nil, errUnknownInterface
	}
	return c.propMap(), nil
}

func (c *characteristic) Set(_ string, _ string, _ dbus.Variant) *dbus.Error {
	return errReadOnly // BlueZ never writes our characteristic properties
}

// gattServiceObj implements the GattService1 property object + its Properties
// interface.
type gattServiceObj struct {
	uuid string
}

func (s *gattServiceObj) propMap() map[string]dbus.Variant {
	return map[string]dbus.Variant{
		"UUID":    dbus.MakeVariant(s.uuid),
		"Primary": dbus.MakeVariant(true),
	}
}

func (s *gattServiceObj) Get(iface, prop string) (dbus.Variant, *dbus.Error) {
	if iface != gattService1 {
		return dbus.Variant{}, errUnknownInterface
	}
	v, ok := s.propMap()[prop]
	if !ok {
		return dbus.Variant{}, errUnknownProperty
	}
	return v, nil
}

func (s *gattServiceObj) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	if iface != gattService1 {
		return nil, errUnknownInterface
	}
	return s.propMap(), nil
}

func (s *gattServiceObj) Set(_ string, _ string, _ dbus.Variant) *dbus.Error {
	return errReadOnly
}

// Server is the exported GATT application. It wires characteristic WriteValue
// callbacks to the Dispatcher and pushes framed responses back out. Construct
// with NewServer, then Register/Unregister against the adapter.
type Server struct {
	conn        *dbus.Conn
	adapter     dbus.ObjectPath
	serviceUUID string
	disp        *Dispatcher

	svc        *gattServiceObj
	chars      []*characteristic
	charByUUID map[string]*characteristic

	mu         sync.Mutex
	registered bool
}

// NewServer builds the GATT application object tree for the ADR-0002 service
// and exports every object onto conn. It does NOT register with BlueZ yet — call
// Register for that. serviceUUID is sec1.UUIDService; the five characteristic
// UUIDs are the sec1.UUID* set.
func NewServer(conn *dbus.Conn, adapter dbus.ObjectPath, disp *Dispatcher, serviceUUID string, charDefs []CharDef) (*Server, error) {
	s := &Server{
		conn:        conn,
		adapter:     adapter,
		serviceUUID: serviceUUID,
		disp:        disp,
		svc:         &gattServiceObj{uuid: serviceUUID},
		charByUUID:  map[string]*characteristic{},
	}
	for i, def := range charDefs {
		c := &characteristic{
			server:  s,
			path:    dbus.ObjectPath(fmt.Sprintf("%s/char%d", appServicePath, i)),
			uuid:    def.UUID,
			flags:   def.Flags,
			isWrite: def.Write,
			isInfo:  def.Info,
		}
		s.chars = append(s.chars, c)
		s.charByUUID[def.UUID] = c
	}
	if err := s.export(); err != nil {
		return nil, err
	}
	return s, nil
}

// CharDef declares one characteristic's UUID, its BlueZ flags, and how the
// server routes it. cmd/provision builds this table from the sec1 UUIDs.
type CharDef struct {
	UUID  string
	Flags []string
	Write bool // route WriteValue to the dispatcher
	Info  bool // ReadValue returns the live info document
}

// export publishes the app root (ObjectManager), the service object, and every
// characteristic object onto the bus, each with its Properties + Introspectable
// interfaces.
func (s *Server) export() error {
	// App root: ObjectManager only.
	if err := s.conn.Export(objectManager{s}, appRootPath, ifaceObjectMgr); err != nil {
		return fmt.Errorf("export ObjectManager: %w", err)
	}
	if err := s.conn.Export(introspect.NewIntrospectable(appRootNode()), appRootPath, ifaceIntrospect); err != nil {
		return fmt.Errorf("export app introspect: %w", err)
	}

	// Service object: GattService1 has no methods, only properties.
	if err := s.conn.Export(s.svc, appServicePath, ifaceProperties); err != nil {
		return fmt.Errorf("export service props: %w", err)
	}
	if err := s.conn.Export(introspect.NewIntrospectable(serviceNode()), appServicePath, ifaceIntrospect); err != nil {
		return fmt.Errorf("export service introspect: %w", err)
	}

	// Characteristic objects.
	for _, c := range s.chars {
		if err := s.conn.Export(c, c.path, gattChar1); err != nil {
			return fmt.Errorf("export char %s: %w", c.uuid, err)
		}
		if err := s.conn.Export(c, c.path, ifaceProperties); err != nil {
			return fmt.Errorf("export char props %s: %w", c.uuid, err)
		}
		if err := s.conn.Export(introspect.NewIntrospectable(characteristicNode()), c.path, ifaceIntrospect); err != nil {
			return fmt.Errorf("export char introspect %s: %w", c.uuid, err)
		}
	}
	return nil
}

// push routes a framed outbound chunk to the characteristic that owns uuid and
// emits it as a notification. Unknown UUIDs (shouldn't happen) are dropped.
func (s *Server) push(uuid string, chunk []byte) {
	if c := s.charByUUID[uuid]; c != nil {
		c.setValueNotify(chunk)
	}
}

// Register registers the application with the adapter's GattManager1. Idempotent.
func (s *Server) Register() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.registered {
		return nil
	}
	obj := s.conn.Object(bluezService, s.adapter)
	call := obj.Call(getManagedMethod, 0, appRootPath, map[string]dbus.Variant{})
	if call.Err != nil {
		return fmt.Errorf("RegisterApplication on %s: %w", s.adapter, call.Err)
	}
	s.registered = true
	return nil
}

// Unregister removes the application from the adapter's GattManager1. Idempotent.
func (s *Server) Unregister() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.registered {
		return nil
	}
	obj := s.conn.Object(bluezService, s.adapter)
	call := obj.Call(gattManager1+".UnregisterApplication", 0, appRootPath)
	s.registered = false
	if call.Err != nil {
		return fmt.Errorf("UnregisterApplication on %s: %w", s.adapter, call.Err)
	}
	return nil
}

// objectManager implements org.freedesktop.DBus.ObjectManager for the app root.
type objectManager struct{ s *Server }

// GetManagedObjects returns the service + characteristic objects and their
// interface properties — the tree BlueZ reads at RegisterApplication time.
func (o objectManager) GetManagedObjects() (map[dbus.ObjectPath]map[string]map[string]dbus.Variant, *dbus.Error) {
	out := map[dbus.ObjectPath]map[string]map[string]dbus.Variant{
		appServicePath: {
			gattService1: o.s.svc.propMap(),
		},
	}
	for _, c := range o.s.chars {
		out[c.path] = map[string]map[string]dbus.Variant{
			gattChar1: c.propMap(),
		}
	}
	return out, nil
}
