package gatt

import "github.com/godbus/dbus/v5/introspect"

// Introspection nodes for the exported objects. BlueZ's GATT registration
// itself relies on ObjectManager.GetManagedObjects, not Introspect, but the
// D-Bus daemon and tools (busctl, bluetoothctl) introspect the tree, and
// known-good Go BlueZ servers export these — so we provide faithful data.
// introspect.NewIntrospectable auto-appends the Introspectable + Peer
// interfaces, so they are omitted here.

// propertiesIface is the standard org.freedesktop.DBus.Properties interface
// description, shared by every object that exports properties.
func propertiesIface() introspect.Interface {
	return introspect.Interface{
		Name: ifaceProperties,
		Methods: []introspect.Method{
			{Name: "Get", Args: []introspect.Arg{
				{Name: "interface", Type: "s", Direction: "in"},
				{Name: "name", Type: "s", Direction: "in"},
				{Name: "value", Type: "v", Direction: "out"},
			}},
			{Name: "GetAll", Args: []introspect.Arg{
				{Name: "interface", Type: "s", Direction: "in"},
				{Name: "properties", Type: "a{sv}", Direction: "out"},
			}},
			{Name: "Set", Args: []introspect.Arg{
				{Name: "interface", Type: "s", Direction: "in"},
				{Name: "name", Type: "s", Direction: "in"},
				{Name: "value", Type: "v", Direction: "in"},
			}},
		},
		Signals: []introspect.Signal{
			{Name: "PropertiesChanged", Args: []introspect.Arg{
				{Name: "interface", Type: "s"},
				{Name: "changed_properties", Type: "a{sv}"},
				{Name: "invalidated_properties", Type: "as"},
			}},
		},
	}
}

func appRootNode() *introspect.Node {
	return &introspect.Node{
		Interfaces: []introspect.Interface{
			{
				Name: ifaceObjectMgr,
				Methods: []introspect.Method{
					{Name: "GetManagedObjects", Args: []introspect.Arg{
						{Name: "objects", Type: "a{oa{sa{sv}}}", Direction: "out"},
					}},
				},
				Signals: []introspect.Signal{
					{Name: "InterfacesAdded", Args: []introspect.Arg{
						{Name: "object", Type: "o"},
						{Name: "interfaces", Type: "a{sa{sv}}"},
					}},
					{Name: "InterfacesRemoved", Args: []introspect.Arg{
						{Name: "object", Type: "o"},
						{Name: "interfaces", Type: "as"},
					}},
				},
			},
		},
	}
}

func serviceNode() *introspect.Node {
	return &introspect.Node{
		Interfaces: []introspect.Interface{
			{
				Name: gattService1,
				Properties: []introspect.Property{
					{Name: "UUID", Type: "s", Access: "read"},
					{Name: "Primary", Type: "b", Access: "read"},
				},
			},
			propertiesIface(),
		},
	}
}

func characteristicNode() *introspect.Node {
	return &introspect.Node{
		Interfaces: []introspect.Interface{
			{
				Name: gattChar1,
				Methods: []introspect.Method{
					{Name: "ReadValue", Args: []introspect.Arg{
						{Name: "options", Type: "a{sv}", Direction: "in"},
						{Name: "value", Type: "ay", Direction: "out"},
					}},
					{Name: "WriteValue", Args: []introspect.Arg{
						{Name: "value", Type: "ay", Direction: "in"},
						{Name: "options", Type: "a{sv}", Direction: "in"},
					}},
					{Name: "StartNotify"},
					{Name: "StopNotify"},
				},
				Properties: []introspect.Property{
					{Name: "UUID", Type: "s", Access: "read"},
					{Name: "Service", Type: "o", Access: "read"},
					{Name: "Flags", Type: "as", Access: "read"},
					{Name: "Value", Type: "ay", Access: "read"},
					{Name: "Notifying", Type: "b", Access: "read"},
				},
			},
			propertiesIface(),
		},
	}
}

func advertisementNode() *introspect.Node {
	return &introspect.Node{
		Interfaces: []introspect.Interface{
			{
				Name:    leAdvertisement1,
				Methods: []introspect.Method{{Name: "Release"}},
				Properties: []introspect.Property{
					{Name: "Type", Type: "s", Access: "read"},
					{Name: "ServiceUUIDs", Type: "as", Access: "read"},
					{Name: "LocalName", Type: "s", Access: "read"},
					{Name: "Includes", Type: "as", Access: "read"},
				},
			},
			propertiesIface(),
		},
	}
}
