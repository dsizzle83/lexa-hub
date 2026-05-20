package dnssd

import (
	"net"
	"testing"

	"github.com/grandcat/zeroconf"
)

func TestServer_Addr(t *testing.T) {
	s := Server{Host: "192.168.1.10", Port: 443}
	if got := s.Addr(); got != "192.168.1.10:443" {
		t.Errorf("Addr() = %q, want 192.168.1.10:443", got)
	}
}

func TestEntryToServer_IPv4(t *testing.T) {
	e := &zeroconf.ServiceEntry{
		ServiceRecord: *zeroconf.NewServiceRecord("Utility-CSMS", "_ieee2030._tls._tcp", "local."),
		HostName:      "csms.local.",
		Port:          8443,
		AddrIPv4:      []net.IP{net.ParseIP("10.0.0.5")},
		Text:          []string{"path=/dcap2"},
	}

	s := entryToServer(e)

	if s.Instance != "Utility-CSMS" {
		t.Errorf("Instance = %q, want Utility-CSMS", s.Instance)
	}
	if s.Host != "10.0.0.5" {
		t.Errorf("Host = %q, want 10.0.0.5 (IPv4 preferred)", s.Host)
	}
	if s.Port != 8443 {
		t.Errorf("Port = %d, want 8443", s.Port)
	}
	if s.DCAPPath != "/dcap2" {
		t.Errorf("DCAPPath = %q, want /dcap2", s.DCAPPath)
	}
}

func TestEntryToServer_NoIPv4_FallsBackToHostname(t *testing.T) {
	e := &zeroconf.ServiceEntry{
		ServiceRecord: *zeroconf.NewServiceRecord("Test", "_ieee2030._tls._tcp", "local."),
		HostName:      "csms.local.",
		Port:          443,
	}

	s := entryToServer(e)

	if s.Host != "csms.local" {
		t.Errorf("Host = %q, want csms.local (trailing dot stripped)", s.Host)
	}
}

func TestEntryToServer_DefaultDCAPPath(t *testing.T) {
	e := &zeroconf.ServiceEntry{
		ServiceRecord: *zeroconf.NewServiceRecord("Test", "_ieee2030._tls._tcp", "local."),
		HostName:      "host.",
		Port:          443,
		AddrIPv4:      []net.IP{net.ParseIP("1.2.3.4")},
	}

	s := entryToServer(e)

	if s.DCAPPath != "/dcap" {
		t.Errorf("DCAPPath = %q, want /dcap (default)", s.DCAPPath)
	}
}

func TestEntryToServer_IgnoresNonPathTXT(t *testing.T) {
	e := &zeroconf.ServiceEntry{
		ServiceRecord: *zeroconf.NewServiceRecord("Test", "_ieee2030._tls._tcp", "local."),
		HostName:      "host.",
		Port:          443,
		AddrIPv4:      []net.IP{net.ParseIP("1.2.3.4")},
		Text:          []string{"version=1.0", "path=/custom"},
	}

	s := entryToServer(e)

	if s.DCAPPath != "/custom" {
		t.Errorf("DCAPPath = %q, want /custom", s.DCAPPath)
	}
}
