package sunspec

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"lexa-proto/modbus"
)

// Modbus bus-sweep primitives — commissioning-time discovery of unknown SunSpec
// devices on an Ethernet (Modbus/TCP) or serial (Modbus RTU) segment.
//
// (This file is named bussweep.go, not sweep.go, because sweep.go already holds
// the unrelated TASK-053 scale-factor "Sweep*" helpers.)
//
// SAFETY / SCOPE. This is a COMMISSIONING-ONLY tool and is read-only BY
// CONSTRUCTION: it issues Modbus reads only (holding-register reads for the
// SunSpec header, the model chain, and the model-1 identity block) and NEVER a
// single write — there is no WriteHolding call anywhere in this file, by
// design, and that is a maintained invariant. A sweep touches unknown, possibly
// energized electrical equipment; a stray write could actuate hardware. It is
// also bounded (a CIDR no larger than a /24; a per-request timeout) and, on
// serial, strictly sequential with a mandatory quiet gap between probes.
//
// The caller (lexa-modbus' commissioning wizard) is responsible for ARMING a
// sweep only when the bus is NOT under live control — this package cannot know
// the plant's control state, so it does not police it. Do not run a sweep
// concurrently with normal polling/actuation on the same segment.
//
// The two entry points differ only in bus etiquette:
//   - SweepTCP fans out across HOSTS with a small bounded worker pool; unit IDs
//     are probed sequentially within each host. Several pool goroutines may
//     discover hits at once, but the caller's hit callback is invoked from a
//     SINGLE goroutine — hits are serialized through a channel — so the callback
//     need not be goroutine-safe.
//   - SweepRTU is strictly sequential end to end (one shared serial line): one
//     transport at a time, reopened per baud, one unit ID at a time, with a
//     mandatory quiet gap between probes. Its callback is invoked inline from
//     the calling goroutine.
//
// Both abort promptly on context cancellation.

// SweepHit is one discovered device — or a device that answered the SunSpec
// marker but could not be fully read. Blocks is populated whenever the model
// chain scanned; Common is non-nil only when the identity block also decoded;
// Err is non-empty when the marker matched but scanning or identity decoding
// then failed (such a device is still reported, never silently dropped).
type SweepHit struct {
	URL    string  // transport URL the device answered on (e.g. "tcp://10.0.0.5:502")
	UnitID uint8   // Modbus unit/device address that answered
	Blocks []Block // SunSpec model chain, when Scan succeeded
	Common *Common // decoded model-1 identity, when it read and decoded; else nil
	Err    string  // set when the SunSpec marker matched but scan/identity failed
}

// newTransport and newSerialTransport are package-level seams so the sweeps are
// testable without real sockets or serial hardware. Overriding them is
// TEST-ONLY; production wires them to the modbus package constructors. RTU needs
// its own seam because the baud rate must reach the transport and the two-arg
// NewTransport cannot carry it (see modbus.NewSerialTransport).
var (
	newTransport       = modbus.NewTransport
	newSerialTransport = modbus.NewSerialTransport
)

// rtuQuietGap is the mandatory inter-probe quiet time on the RTU serial bus
// (RS-485 line turnaround plus slave inter-frame settle). It is a package var
// so tests can shrink it; do NOT lower the production default — 50 ms is
// deliberately conservative for a shared multidrop bus of unknown devices.
var rtuQuietGap = 50 * time.Millisecond

const (
	maxSweepWorkers = 8   // upper bound on concurrent TCP host probes
	minUnitID       = 1   // Modbus addressable range is 1..247; 0 is broadcast,
	maxUnitID       = 247 // and 248..255 are reserved
)

// probeOne probes a single unit ID on an already-open transport. It returns a
// hit and true when the unit answers the SunSpec marker, and a zero hit and
// false when the unit does not respond or responds without the marker. A device
// that matches the marker but then fails to scan or identify still returns a
// hit (true) with Err set — a sweep records such devices, it never aborts on
// them.
//
// probeOne is READ-ONLY (SetUnitID plus holding-register reads). It does not set
// SweepHit.URL — the caller, which knows the transport URL, fills that in.
func probeOne(t modbus.Transport, unitID uint8) (SweepHit, bool) {
	if err := t.SetUnitID(unitID); err != nil {
		return SweepHit{}, false // cannot address this unit; treat as absent
	}
	hdr, err := t.ReadHolding(SunSpecBase, 2)
	if err != nil || len(hdr) < 2 {
		return SweepHit{}, false // no response at this unit
	}
	if hdr[0] != SunSMagic0 || hdr[1] != SunSMagic1 {
		return SweepHit{}, false // answered, but not a SunSpec device
	}
	// Marker matched: this IS a SunSpec device. From here every failure yields a
	// hit-with-Err, never a dropped device.
	hit := SweepHit{UnitID: unitID}
	blocks, err := Scan(t)
	if err != nil {
		hit.Err = fmt.Sprintf("scan: %v", err)
		return hit, true
	}
	hit.Blocks = blocks
	// Reuse the blocks just scanned rather than re-scanning inside NewReader.
	common, err := ReadCommon(&Reader{t: t, blocks: blocks})
	if err != nil {
		hit.Err = fmt.Sprintf("identity: %v", err)
		return hit, true
	}
	hit.Common = &common
	return hit, true
}

// SweepTCP discovers SunSpec devices across the hosts of an IPv4 CIDR by opening
// a Modbus/TCP connection to each host:port and probing each unit ID.
//
// cidr must be IPv4 and no larger than a /24 (254 host addresses); a larger CIDR
// is rejected, never silently truncated. Network and broadcast addresses are
// skipped. Reachability is not a separate step — each host is simply dialed via
// the Modbus transport with the (short) timeout, and an unreachable host is
// skipped silently. On a SunSpec marker match the device's model chain is
// scanned and its identity read; a per-device failure yields a SweepHit with Err
// set and never aborts the sweep.
//
// If unitIDs is empty it defaults to {1} (the usual Modbus/TCP unit); scanning
// the full 1..247 range across a whole /24 would mean tens of thousands of
// connections, so that default is deliberately narrow. Every supplied unit ID
// must be in 1..247.
//
// Hosts are probed concurrently by a bounded worker pool (at most 8); unit IDs
// are probed sequentially within a host. hit is invoked from a SINGLE goroutine
// — discovered hits are serialized through a channel — so it need not be
// goroutine-safe. SweepTCP returns when every host has been probed, or promptly
// with ctx.Err() when ctx is cancelled.
func SweepTCP(ctx context.Context, cidr string, port int, unitIDs []uint8, timeout time.Duration, hit func(SweepHit)) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("sweep tcp: invalid port %d", port)
	}
	hosts, err := expandCIDR(cidr)
	if err != nil {
		return err
	}
	ids, err := resolveUnitIDs(unitIDs, false)
	if err != nil {
		return err
	}
	if len(hosts) == 0 {
		return nil
	}

	workers := maxSweepWorkers
	if len(hosts) < workers {
		workers = len(hosts)
	}

	hostCh := make(chan string)
	hitCh := make(chan SweepHit)

	// Feeder: hand hosts to workers, stopping early on cancellation.
	go func() {
		defer close(hostCh)
		for _, h := range hosts {
			select {
			case <-ctx.Done():
				return
			case hostCh <- h:
			}
		}
	}()

	// emit delivers a hit to the serializing pump, or reports cancellation.
	emit := func(h SweepHit) bool {
		select {
		case <-ctx.Done():
			return false
		case hitCh <- h:
			return true
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for host := range hostCh {
				if ctx.Err() != nil {
					return
				}
				probeHostTCP(ctx, fmt.Sprintf("tcp://%s:%d", host, port), ids, timeout, emit)
			}
		}()
	}

	// Close the hit channel once every worker has finished.
	go func() {
		wg.Wait()
		close(hitCh)
	}()

	// Single-goroutine callback pump: the caller's hit() is only ever invoked
	// from here, which is the goroutine that called SweepTCP — the single-
	// goroutine callback contract. It drains hitCh continuously, so a worker's
	// emit never blocks indefinitely; on cancel emit unblocks via ctx.Done.
	for h := range hitCh {
		hit(h)
	}
	return ctx.Err()
}

// probeHostTCP opens one Modbus/TCP transport to url and probes each unit ID in
// order, emitting a hit for every SunSpec device found. It returns when the host
// is exhausted or emit reports cancellation. An unreachable host (transport
// build or Open failure) is skipped silently. Read-only throughout.
func probeHostTCP(ctx context.Context, url string, ids []uint8, timeout time.Duration, emit func(SweepHit) bool) {
	t, err := newTransport(url, timeout)
	if err != nil {
		return // malformed URL / client build failure — skip this host
	}
	if err := t.Open(); err != nil {
		return // host unreachable or port closed — skip silently
	}
	defer t.Close()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		h, ok := probeOne(t, id)
		if !ok {
			continue
		}
		h.URL = url
		if !emit(h) {
			return // cancelled
		}
	}
}

// SweepRTU discovers SunSpec devices on a single Modbus RTU serial line. It is
// strictly sequential: the serial bus is a shared multidrop medium, so exactly
// one transaction is ever in flight. For each baud in bauds the port is reopened
// at that speed and every unit ID is probed in order, with a mandatory quiet gap
// between probes (RS-485 line turnaround plus slave inter-frame settle).
//
// dev is the serial device: a bare path such as "/dev/ttyUSB0" is wrapped as an
// "rtu://" URL, or a full "rtu://…"/"rtuovertcp://…" URL may be passed as-is.
// bauds must be non-empty. If unitIDs is empty it defaults to the full
// addressable range 1..247 (the whole bus is scanned); every supplied unit ID
// must be in 1..247.
//
// hit is invoked inline from the calling goroutine — there is no callback
// concurrency for RTU. SweepRTU checks ctx between probes and returns promptly
// with ctx.Err() on cancellation.
func SweepRTU(ctx context.Context, dev string, bauds []int, unitIDs []uint8, timeout time.Duration, hit func(SweepHit)) error {
	if len(bauds) == 0 {
		return fmt.Errorf("sweep rtu: no bauds given")
	}
	for _, b := range bauds {
		if b <= 0 {
			return fmt.Errorf("sweep rtu: invalid baud %d", b)
		}
	}
	ids, err := resolveUnitIDs(unitIDs, true)
	if err != nil {
		return err
	}

	url := dev
	if !strings.Contains(url, "://") {
		url = "rtu://" + url
	}

	// The quiet gap sits BETWEEN probes: skip it before the very first probe of
	// the whole sweep (nothing has been transmitted yet to settle).
	firstProbe := true

	for _, baud := range bauds {
		if err := ctx.Err(); err != nil {
			return err
		}
		t, err := newSerialTransport(url, baud, timeout)
		if err != nil {
			return fmt.Errorf("sweep rtu: build %s @ %d bps: %w", url, baud, err)
		}
		if err := t.Open(); err != nil {
			// A busy port or an adapter that cannot set this baud: skip this
			// baud and try the next. (A merely-wrong baud usually surfaces as
			// per-probe CRC/timeout errors, not an Open failure.)
			t.Close()
			continue
		}
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				t.Close()
				return err
			}
			if !firstProbe {
				if !sleepCtx(ctx, rtuQuietGap) {
					t.Close()
					return ctx.Err()
				}
			}
			firstProbe = false

			h, ok := probeOne(t, id)
			if !ok {
				continue
			}
			h.URL = url
			hit(h)
		}
		t.Close()
	}
	return ctx.Err()
}

// sleepCtx sleeps for d, or returns early if ctx is cancelled. It reports true
// when the full duration elapsed and false when ctx was cancelled. A non-
// positive d does not sleep and just reports the current ctx state (used so
// tests can zero out rtuQuietGap).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-tm.C:
		return true
	}
}

// expandCIDR returns the host addresses of an IPv4 CIDR, excluding the network
// and broadcast addresses for /24../30 prefixes (/31 and /32 have none, per RFC
// 3021 / host routes). It refuses anything larger than a /24 — more than 256
// addresses — rather than silently truncating: a bus-sweep of thousands of
// hosts is never what a commissioning tool wants and would hammer the segment.
func expandCIDR(cidr string) ([]string, error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("sweep: parse cidr %q: %w", cidr, err)
	}
	ip4 := ipnet.IP.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("sweep: only IPv4 CIDRs are supported, got %q", cidr)
	}
	ones, bits := ipnet.Mask.Size()
	hostBits := bits - ones
	if hostBits > 8 {
		return nil, fmt.Errorf(
			"sweep: cidr %q spans %d addresses; refusing to sweep larger than a /24 (254 hosts)",
			cidr, 1<<uint(hostBits))
	}
	base := binary.BigEndian.Uint32(ip4)
	count := uint32(1) << uint(hostBits)
	hosts := make([]string, 0, count)
	for i := uint32(0); i < count; i++ {
		// Skip network (.0) and broadcast (last) for /24../30 (hostBits >= 2).
		if hostBits >= 2 && (i == 0 || i == count-1) {
			continue
		}
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], base+i)
		hosts = append(hosts, net.IP(b[:]).String())
	}
	return hosts, nil
}

// resolveUnitIDs validates unitIDs (each must be an addressable Modbus unit,
// 1..247) and, when the list is empty, substitutes a default: the full 1..247
// range when allByDefault is true (RTU, where the whole bus is scanned), or just
// {1} otherwise (TCP).
func resolveUnitIDs(unitIDs []uint8, allByDefault bool) ([]uint8, error) {
	if len(unitIDs) == 0 {
		if allByDefault {
			out := make([]uint8, 0, maxUnitID-minUnitID+1)
			for id := minUnitID; id <= maxUnitID; id++ {
				out = append(out, uint8(id))
			}
			return out, nil
		}
		return []uint8{1}, nil
	}
	for _, id := range unitIDs {
		if id < minUnitID || id > maxUnitID {
			return nil, fmt.Errorf("sweep: invalid Modbus unit id %d (must be %d..%d)", id, minUnitID, maxUnitID)
		}
	}
	return unitIDs, nil
}
