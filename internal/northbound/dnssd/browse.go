// Package dnssd discovers IEEE 2030.5 servers using DNS Service Discovery
// (RFC 6763) over mDNS (RFC 6762).
//
// # Service type
//
// The IEEE 2030.5 service type is _ieee2030._tls._tcp. A compliant server
// advertises itself on the local mDNS domain (.local) with this service type.
// The SRV record gives the hostname and port; the TXT record may contain:
//
//	path=/dcap    — path of the DeviceCapability endpoint (default: /dcap)
//
// # Usage
//
//	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
//	defer cancel()
//	servers, err := dnssd.Browse(ctx)
//	for _, s := range servers {
//	    log.Printf("Found %s at %s (dcap: %s)", s.Instance, s.Addr(), s.DCAPPath)
//	}
//
// # WSL2 note
//
// mDNS uses UDP multicast (224.0.0.251:5353). On WSL2 the virtual NIC may
// not forward multicast to the host network. Browse will time out cleanly
// with an empty list; use --server to specify the address directly when
// developing on WSL2. On actual hardware (Pi-to-Pi via a switch) mDNS works
// without any additional configuration.
package dnssd

import (
	"context"
	"fmt"
	"strings"

	"github.com/grandcat/zeroconf"
)

// ServiceType is the IEEE 2030.5 DNS-SD service type.
const ServiceType = "_ieee2030._tls._tcp"

// Server is a discovered IEEE 2030.5 server.
type Server struct {
	// Instance is the DNS-SD service instance name (e.g. "Utility-CSMS-1").
	Instance string

	// Host is the resolved IPv4 address (or hostname if resolution failed).
	Host string

	// Port is the TCP port the server listens on.
	Port int

	// DCAPPath is the path of the DeviceCapability endpoint.
	// Taken from the TXT record "path=<value>"; defaults to "/dcap".
	DCAPPath string
}

// Addr returns the "host:port" string suitable for use as a dial address.
func (s Server) Addr() string {
	return fmt.Sprintf("%s:%d", s.Host, s.Port)
}

// Browse discovers IEEE 2030.5 servers on the local mDNS domain until ctx
// expires or is cancelled. It returns all servers found before the deadline.
// An empty slice (no error) means no servers were advertising within the
// timeout; call ctx.Err() to distinguish timeout from cancellation.
func Browse(ctx context.Context) ([]Server, error) {
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return nil, fmt.Errorf("create mDNS resolver: %w", err)
	}

	entries := make(chan *zeroconf.ServiceEntry)

	// Start browsing — runs asynchronously, closes entries when ctx done.
	if err := resolver.Browse(ctx, ServiceType, "local.", entries); err != nil {
		return nil, fmt.Errorf("browse %s: %w", ServiceType, err)
	}

	var servers []Server
	for entry := range entries {
		servers = append(servers, entryToServer(entry))
	}

	return servers, nil
}

func entryToServer(e *zeroconf.ServiceEntry) Server {
	s := Server{
		Instance: e.Instance,
		Port:     e.Port,
		DCAPPath: "/dcap", // spec default
	}

	// Prefer a resolved IPv4 address; fall back to the advertised hostname.
	if len(e.AddrIPv4) > 0 {
		s.Host = e.AddrIPv4[0].String()
	} else {
		// Strip trailing dot from FQDN.
		s.Host = strings.TrimSuffix(e.HostName, ".")
	}

	// TXT record convention: "path=/dcap"
	for _, txt := range e.Text {
		if strings.HasPrefix(txt, "path=") {
			s.DCAPPath = strings.TrimPrefix(txt, "path=")
		}
	}

	return s
}
