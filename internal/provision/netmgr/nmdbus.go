package netmgr

// This file is the HARDWARE layer of netmgr (unit B3, Phase C): the real
// NetworkManager nmConn over godbus/dbus/v5 on the system bus. None of it is
// exercised in CI — it needs a live NetworkManager — so the LOGIC it feeds
// (scan dedup/sort/classify, join state machine, reason mapping) lives in
// netmgr.go against the nmConn seam and IS unit-tested. Keep it that way.
//
// The D-Bus shapes below were confirmed by read-only introspection of the dev
// kit's NetworkManager 1.46.6 (busctl introspect org.freedesktop.NetworkManager
// …). Property reads use StoreProperty; the (uu) StateReason struct decodes into
// a two-field Go struct.

import (
	"context"
	"errors"

	"github.com/godbus/dbus/v5"
)

// nmDBus is the production nmConn: godbus calls against org.freedesktop.
// NetworkManager on the system bus.
type nmDBus struct{ conn *dbus.Conn }

func newDBus(conn *dbus.Conn) *nmDBus { return &nmDBus{conn: conn} }

func (n *nmDBus) obj(path dbus.ObjectPath) dbus.BusObject {
	return n.conn.Object(nmBusName, path)
}

// deviceStateReasonTuple decodes the Device.StateReason property (D-Bus "(uu)").
type deviceStateReasonTuple struct {
	State  uint32
	Reason uint32
}

func (n *nmDBus) WifiDevice(ctx context.Context) (dbus.ObjectPath, error) {
	var devices []dbus.ObjectPath
	if err := n.obj(nmRootPath).CallWithContext(ctx, nmIface+".GetDevices", 0).Store(&devices); err != nil {
		return "", err
	}
	for _, d := range devices {
		var t uint32
		if err := n.obj(d).StoreProperty(nmDeviceIface+".DeviceType", &t); err != nil {
			continue
		}
		if t == deviceTypeWiFi {
			return d, nil
		}
	}
	return "", errors.New("netmgr: no WiFi device (NM device type 2) found")
}

func (n *nmDBus) RequestScan(ctx context.Context, dev dbus.ObjectPath) error {
	return n.obj(dev).CallWithContext(ctx, nmWirelessIface+".RequestScan", 0, map[string]dbus.Variant{}).Err
}

func (n *nmDBus) AccessPoints(ctx context.Context, dev dbus.ObjectPath) ([]dbus.ObjectPath, error) {
	var aps []dbus.ObjectPath
	err := n.obj(dev).StoreProperty(nmWirelessIface+".AccessPoints", &aps)
	return aps, err
}

func (n *nmDBus) ReadAP(ctx context.Context, ap dbus.ObjectPath) (apRaw, error) {
	var r apRaw
	o := n.obj(ap)
	if err := o.StoreProperty(nmAPIface+".Ssid", &r.SsidBytes); err != nil {
		return r, err
	}
	// Signal/security properties degrade gracefully to zero (→ open, -100 dBm).
	_ = o.StoreProperty(nmAPIface+".Strength", &r.Strength)
	_ = o.StoreProperty(nmAPIface+".WpaFlags", &r.WpaFlags)
	_ = o.StoreProperty(nmAPIface+".RsnFlags", &r.RsnFlags)
	return r, nil
}

func (n *nmDBus) AddAndActivate(ctx context.Context, settings map[string]map[string]dbus.Variant, dev dbus.ObjectPath) (dbus.ObjectPath, dbus.ObjectPath, error) {
	var connPath, activePath dbus.ObjectPath
	// specific_object "/" lets NetworkManager pick the AP for the SSID.
	call := n.obj(nmRootPath).CallWithContext(ctx, nmIface+".AddAndActivateConnection", 0,
		settings, dev, dbus.ObjectPath("/"))
	if call.Err != nil {
		return "", "", call.Err
	}
	if err := call.Store(&connPath, &activePath); err != nil {
		return "", "", err
	}
	return connPath, activePath, nil
}

func (n *nmDBus) ActiveConnectionState(ctx context.Context, active dbus.ObjectPath) (uint32, error) {
	var s uint32
	err := n.obj(active).StoreProperty(nmActiveIface+".State", &s)
	return s, err
}

func (n *nmDBus) DeviceStateReason(ctx context.Context, dev dbus.ObjectPath) (uint32, uint32, error) {
	var sr deviceStateReasonTuple
	if err := n.obj(dev).StoreProperty(nmDeviceIface+".StateReason", &sr); err != nil {
		return 0, 0, err
	}
	return sr.State, sr.Reason, nil
}

func (n *nmDBus) ActiveConnectionIP4(ctx context.Context, active dbus.ObjectPath) (string, error) {
	var ip4 dbus.ObjectPath
	if err := n.obj(active).StoreProperty(nmActiveIface+".Ip4Config", &ip4); err != nil {
		return "", err
	}
	if ip4 == "" || ip4 == "/" {
		return "", nil
	}
	var addrData []map[string]dbus.Variant
	if err := n.obj(ip4).StoreProperty(nmIP4Iface+".AddressData", &addrData); err != nil {
		return "", err
	}
	return firstIP4Address(addrData), nil
}

func (n *nmDBus) DeleteConnection(ctx context.Context, conn dbus.ObjectPath) error {
	return n.obj(conn).CallWithContext(ctx, nmSettingsConnIface+".Delete", 0).Err
}
