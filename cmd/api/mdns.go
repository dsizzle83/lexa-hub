package main

// mdns.go implements DEVICE_ROADMAP.md §4.4: advertise lexa-api's REST
// surface over mDNS (_lexa-hub._tcp) so an installer's phone/app on the
// bench or home LAN can discover the unit without knowing its IP. This is
// the registration mirror of internal/northbound/dnssd's existing Browse —
// same vendored github.com/grandcat/zeroconf, no go.mod change.
//
// mDNS failure (Register erroring, e.g. no multicast-capable interface) is
// NON-FATAL: multicast-hostile networks are expected in the field, and the
// cloud-relay discovery path (lexa-cloudlink, a separate unit) covers that
// case. This file only ever logs a WARN and disables advertisement.
import (
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/grandcat/zeroconf"
)

// mdnsServiceType is the DNS-SD service type this unit advertises under.
const mdnsServiceType = "_lexa-hub._tcp"

// apiVersion is the value reported in the mDNS TXT record's "fw=" field.
//
// Coordination note: no build-injected version variable exists anywhere in
// this repo today — grepped Makefile and every cmd/*/main.go for
// -ldflags/-X/BuildVersion and found nothing (confirmed against the
// Makefile's build/build-arm64 targets, which pass no -ldflags at all).
// "dev" is a placeholder until a real version stamp lands; see this task's
// report for the follow-up recommendation.
const apiVersion = "dev"

// commissionedMarkerPath is the presence-checked marker file
// (DEVICE_ROADMAP.md §9): its existence means the unit has been claimed by
// an installer. A var (not const) so tests can point it at a temp path
// instead of the real /etc/lexa/commissioned.
var commissionedMarkerPath = "/etc/lexa/commissioned"

// mdnsRefreshPeriod is how often the claimed marker is re-stat'd. TXT is
// only actually re-published (SetText) when the flag has flipped since the
// last check — a cheap stat every period, no per-tick mDNS churn.
const mdnsRefreshPeriod = 60 * time.Second

// mdnsAdvertiser owns the registered zeroconf.Server for the process
// lifetime. The zero value's methods are all safe to call on a nil
// *mdnsAdvertiser (mDNS disabled, or registration failed at startup) so
// main.go never needs an "if configured" guard around them.
type mdnsAdvertiser struct {
	srv    *zeroconf.Server
	serial string
	port   int
	tlsOn  bool

	claimed atomic.Bool
}

// startMDNS registers "lexa-<serial>" under mdnsServiceType on the port
// parsed from listenAddr (Config.ListenAddr's host:port form). A parse or
// registration failure logs a WARN and returns nil — never fatal (see this
// file's package doc).
func startMDNS(serial, listenAddr string, tlsOn bool) *mdnsAdvertiser {
	port, err := portOf(listenAddr)
	if err != nil {
		log.Printf("lexa-api: mdns: cannot parse listen_addr %q for advertised port: %v — mDNS disabled", listenAddr, err)
		return nil
	}

	a := &mdnsAdvertiser{serial: serial, port: port, tlsOn: tlsOn}
	a.claimed.Store(isCommissioned())

	srv, err := zeroconf.Register("lexa-"+serial, mdnsServiceType, "local.", port, a.txt(), nil)
	if err != nil {
		log.Printf("lexa-api: mdns: register failed (multicast-hostile network?): %v — mDNS discovery unavailable, cloud-relay path unaffected", err)
		return nil
	}
	a.srv = srv
	log.Printf("lexa-api: mdns advertising lexa-%s as %s on port %d (claimed=%v, api=%s)",
		serial, mdnsServiceType, port, a.claimed.Load(), a.apiScheme())
	return a
}

// apiScheme is the TXT record's "api=" value.
func (a *mdnsAdvertiser) apiScheme() string {
	if a.tlsOn {
		return "https"
	}
	return "http"
}

// txt builds the current TXT record set from the advertiser's fields.
func (a *mdnsAdvertiser) txt() []string {
	claimed := "0"
	if a.claimed.Load() {
		claimed = "1"
	}
	return []string{
		"serial=" + a.serial,
		"fw=" + apiVersion,
		"claimed=" + claimed,
		"api=" + a.apiScheme(),
	}
}

// refreshLoop re-stats the commissioned marker every mdnsRefreshPeriod and
// calls SetText ONLY when the claimed flag actually flipped since the last
// check — a cheap stat, never per-tick TXT churn. Intended to run in its
// own goroutine for the process lifetime; returns when stop is closed. Safe
// to call on a nil receiver (mDNS disabled or registration failed).
func (a *mdnsAdvertiser) refreshLoop(stop <-chan struct{}) {
	if a == nil || a.srv == nil {
		return
	}
	ticker := time.NewTicker(mdnsRefreshPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			now := isCommissioned()
			if now != a.claimed.Load() {
				a.claimed.Store(now)
				a.srv.SetText(a.txt())
				log.Printf("lexa-api: mdns: claimed flag flipped to %v — TXT record refreshed", now)
			}
		}
	}
}

// Shutdown unregisters the mDNS service, if one was successfully
// registered. Safe to call on a nil receiver.
func (a *mdnsAdvertiser) Shutdown() {
	if a == nil || a.srv == nil {
		return
	}
	a.srv.Shutdown()
}

// isCommissioned reports whether the commissioned marker file exists
// (DEVICE_ROADMAP.md §9). Any stat error (including "not exist") is treated
// as unclaimed — this is a cheap presence check, not a filesystem-health
// probe, and "unclaimed" is the fail-safe reading (an installer will see
// claimed=0 and can proceed; a false claimed=1 would hide a unit that
// genuinely needs commissioning).
func isCommissioned() bool {
	_, err := os.Stat(commissionedMarkerPath)
	return err == nil
}

// portOf extracts the numeric port from a host:port ListenAddr — the same
// form Config.ListenAddr takes (":9100", "0.0.0.0:9100", "127.0.0.1:9100").
func portOf(listenAddr string) (int, error) {
	_, portStr, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return 0, err
	}
	p, err := strconv.Atoi(portStr)
	if err != nil {
		return 0, fmt.Errorf("non-numeric port %q: %w", portStr, err)
	}
	if p <= 0 || p > 65535 {
		return 0, fmt.Errorf("port %d out of range", p)
	}
	return p, nil
}
