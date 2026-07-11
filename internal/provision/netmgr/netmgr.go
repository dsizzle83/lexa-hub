// Package netmgr is the NetworkManager D-Bus client for the LEXA Provision v1
// commissioning service (ADR-0002, unit B3). It gives cmd/provision the two
// network operations a commissioning phone drives over BLE:
//
//   - Scan: enumerate nearby WiFi access points — deduped by SSID (strongest
//     wins), sorted by signal descending, top 20 — as []sec1.WifiAp.
//   - Join: create + activate a NetworkManager WiFi profile for a given
//     SSID/PSK and STREAM the join progress (joining… → joined{ip} /
//     failed{reason}) as it happens, mapping NetworkManager's device
//     state-reason codes to the ADR-0002 WifiFailureReason enum.
//
// All NetworkManager access goes through the small nmConn seam, so the scan
// dedup/sort/classification, the join state machine, and the reason mapping are
// unit-tested against a fake with no live system bus. The real godbus-backed
// implementation (nmDBus, nmdbus.go) is exercised on hardware in Phase C.
//
// Concurrency: godbus dispatches each incoming D-Bus method call on its own
// goroutine (vendor/.../dbus/v5/conn.go), so Join blocking in its goroutine
// while it polls NetworkManager never stalls the GATT server; the streamed
// state indications go out on the status characteristic as they are produced.
package netmgr

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/godbus/dbus/v5"

	"lexa-hub/internal/provision/sec1"
)

// NetworkManager well-known D-Bus names (org.freedesktop.NetworkManager). These
// are the stable D-Bus ABI; the numeric enums below are equally stable
// (nm-dbus-interface.h) and cannot change without breaking every NM client.
const (
	nmBusName           = "org.freedesktop.NetworkManager"
	nmRootPath          = dbus.ObjectPath("/org/freedesktop/NetworkManager")
	nmIface             = "org.freedesktop.NetworkManager"
	nmDeviceIface       = "org.freedesktop.NetworkManager.Device"
	nmWirelessIface     = "org.freedesktop.NetworkManager.Device.Wireless"
	nmAPIface           = "org.freedesktop.NetworkManager.AccessPoint"
	nmActiveIface       = "org.freedesktop.NetworkManager.Connection.Active"
	nmIP4Iface          = "org.freedesktop.NetworkManager.IP4Config"
	nmSettingsConnIface = "org.freedesktop.NetworkManager.Settings.Connection"
)

// deviceTypeWiFi is NM_DEVICE_TYPE_WIFI (Device.DeviceType) — confirmed on the
// dev kit: eth0=1, wlan0=2.
const deviceTypeWiFi uint32 = 2

// NMActiveConnectionState (Connection.Active.State).
const (
	activeStateActivating  uint32 = 1
	activeStateActivated   uint32 = 2
	activeStateDeactivated uint32 = 4
)

// deviceStateFailed is NM_DEVICE_STATE_FAILED (Device.State) — the terminal
// failure we watch for so an auth failure surfaces before the active-connection
// state flips to DEACTIVATED.
const deviceStateFailed uint32 = 120

// NMDeviceStateReason codes we map to the ADR reason enum (Device.StateReason
// second member). Stable NetworkManager D-Bus ABI values.
const (
	reasonIPConfigUnavailable  uint32 = 5
	reasonIPConfigExpired      uint32 = 6
	reasonNoSecrets            uint32 = 7
	reasonSupplicantDisconnect uint32 = 8
	reasonSupplicantConfigFail uint32 = 9
	reasonSupplicantFailed     uint32 = 10
	reasonSupplicantTimeout    uint32 = 11
	reasonDHCPStartFailed      uint32 = 15
	reasonDHCPError            uint32 = 16
	reasonDHCPFailed           uint32 = 17
	reasonSSIDNotFound         uint32 = 53
)

// apSecKeyMgmtSAE is NM_802_11_AP_SEC_KEY_MGMT_SAE (WPA3-Personal) in
// Wpa/RsnFlags.
const apSecKeyMgmtSAE uint32 = 0x400

const (
	// maxScanResults caps a scan_result payload (ADR-0002 §WiFi scan).
	maxScanResults = 20
	// connectionID names the WiFi profile created by a join. NetworkManager
	// auto-suffixes on collision; a failed join deletes its own profile so
	// retries never accumulate stale profiles.
	connectionID = "lexa-wifi"
)

const (
	defaultJoinTimeout  = 45 * time.Second
	defaultPollInterval = 750 * time.Millisecond
	defaultScanSettle   = 2500 * time.Millisecond
)

// apRaw is the raw NetworkManager AccessPoint properties netmgr classifies. The
// interesting logic (ay→string SSID, Strength→rssi, flags→sec) stays in this
// package so it is unit-tested; the D-Bus decode into apRaw is the only part in
// the (Phase-C-validated) real impl.
type apRaw struct {
	SsidBytes []byte
	Strength  uint8
	WpaFlags  uint32
	RsnFlags  uint32
}

// nmConn is the minimal NetworkManager surface netmgr needs. The production impl
// (nmDBus) wraps godbus/dbus/v5 on the system bus; tests supply a fake with no
// live bus.
type nmConn interface {
	// WifiDevice returns the object path of the first WiFi device (NM device
	// type 2).
	WifiDevice(ctx context.Context) (dbus.ObjectPath, error)
	// RequestScan asks NetworkManager to scan; callers treat its error as
	// non-fatal (a scan may already be in progress).
	RequestScan(ctx context.Context, dev dbus.ObjectPath) error
	// AccessPoints lists the device's currently-known AP object paths.
	AccessPoints(ctx context.Context, dev dbus.ObjectPath) ([]dbus.ObjectPath, error)
	// ReadAP reads the raw properties of one AccessPoint.
	ReadAP(ctx context.Context, ap dbus.ObjectPath) (apRaw, error)
	// AddAndActivate creates + activates a connection profile on dev, returning
	// the new profile path (for cleanup) and the active-connection path (to
	// watch).
	AddAndActivate(ctx context.Context, settings map[string]map[string]dbus.Variant, dev dbus.ObjectPath) (conn dbus.ObjectPath, active dbus.ObjectPath, err error)
	// ActiveConnectionState reads Connection.Active.State.
	ActiveConnectionState(ctx context.Context, active dbus.ObjectPath) (uint32, error)
	// DeviceStateReason reads Device.StateReason (state, reason) — the pollable
	// source of the rich NMDeviceStateReason (the active connection exposes no
	// reason property, only a signal).
	DeviceStateReason(ctx context.Context, dev dbus.ObjectPath) (state uint32, reason uint32, err error)
	// ActiveConnectionIP4 reads the first IPv4 address of the active connection
	// (empty string if none yet).
	ActiveConnectionIP4(ctx context.Context, active dbus.ObjectPath) (string, error)
	// DeleteConnection deletes a connection profile (failed-join cleanup).
	DeleteConnection(ctx context.Context, conn dbus.ObjectPath) error
}

// clock abstracts time for the join timeout so the timeout path is unit-tested
// with a virtual clock (no real waiting).
type clock interface {
	Now() time.Time
	// Sleep blocks for d unless ctx is cancelled first, in which case it returns
	// ctx.Err().
	Sleep(ctx context.Context, d time.Duration) error
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Options tunes a Client. Zero fields take the documented defaults.
type Options struct {
	// JoinTimeout bounds a single join: no ACTIVATED within this window yields
	// state:failed reason=timeout. Default 45s.
	JoinTimeout time.Duration
	// PollInterval is the active-connection poll cadence during a join.
	// Default 750ms.
	PollInterval time.Duration
	// ScanSettle is the wait after RequestScan before reading access points
	// (NM scans are asynchronous). Default 2.5s.
	ScanSettle time.Duration
}

func (o Options) withDefaults() Options {
	if o.JoinTimeout <= 0 {
		o.JoinTimeout = defaultJoinTimeout
	}
	if o.PollInterval <= 0 {
		o.PollInterval = defaultPollInterval
	}
	if o.ScanSettle <= 0 {
		o.ScanSettle = defaultScanSettle
	}
	return o
}

// Client is the NetworkManager client cmd/provision drives.
type Client struct {
	nm  nmConn
	clk clock

	joinTimeout  time.Duration
	pollInterval time.Duration
	scanSettle   time.Duration
}

// New builds a Client backed by the real godbus NetworkManager connection on the
// system bus conn.
func New(conn *dbus.Conn, opt Options) *Client {
	opt = opt.withDefaults()
	return &Client{
		nm:           newDBus(conn),
		clk:          realClock{},
		joinTimeout:  opt.JoinTimeout,
		pollInterval: opt.PollInterval,
		scanSettle:   opt.ScanSettle,
	}
}

// newClient is the test constructor: it injects a fake nmConn + clock and takes
// options verbatim (no defaults, so a test may set ScanSettle=0).
func newClient(nm nmConn, clk clock, opt Options) *Client {
	return &Client{
		nm:           nm,
		clk:          clk,
		joinTimeout:  opt.JoinTimeout,
		pollInterval: opt.PollInterval,
		scanSettle:   opt.ScanSettle,
	}
}

// Scan performs a WiFi scan and returns the deduped, RSSI-sorted, top-20 access
// points as sec1.WifiAp. RequestScan errors are non-fatal (results are read
// regardless); a hard error reading the device/AP list is returned.
func (c *Client) Scan(ctx context.Context) ([]sec1.WifiAp, error) {
	dev, err := c.nm.WifiDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("netmgr: wifi device: %w", err)
	}
	// Best-effort active scan; NM rejects a request while one is already
	// running, which is harmless — the cached AccessPoints are still returned.
	_ = c.nm.RequestScan(ctx, dev)
	if c.scanSettle > 0 {
		// A cancelled/expired settle is fine: read whatever APs are known.
		_ = c.clk.Sleep(ctx, c.scanSettle)
	}
	paths, err := c.nm.AccessPoints(ctx, dev)
	if err != nil {
		return nil, fmt.Errorf("netmgr: access points: %w", err)
	}
	return c.collectAPs(ctx, paths), nil
}

// collectAPs reads each AP, classifies it, dedupes by SSID keeping the strongest,
// sorts by RSSI descending (SSID ascending as a stable tie-break), and caps at
// maxScanResults. Hidden/empty-SSID APs are dropped (unusable for a name-based
// join and undisplayable).
func (c *Client) collectAPs(ctx context.Context, paths []dbus.ObjectPath) []sec1.WifiAp {
	best := make(map[string]sec1.WifiAp, len(paths))
	for _, p := range paths {
		raw, err := c.nm.ReadAP(ctx, p)
		if err != nil {
			continue
		}
		ssid := string(raw.SsidBytes)
		if ssid == "" {
			continue
		}
		ap := sec1.WifiAp{
			SSID: ssid,
			RSSI: rssiFromStrength(raw.Strength),
			Sec:  classifySec(raw.WpaFlags, raw.RsnFlags),
		}
		if cur, ok := best[ssid]; !ok || ap.RSSI > cur.RSSI {
			best[ssid] = ap
		}
	}
	out := make([]sec1.WifiAp, 0, len(best))
	for _, ap := range best {
		out = append(out, ap)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RSSI != out[j].RSSI {
			return out[i].RSSI > out[j].RSSI
		}
		return out[i].SSID < out[j].SSID
	})
	if len(out) > maxScanResults {
		out = out[:maxScanResults]
	}
	return out
}

// JoinRequest is a single join attempt. PSK is nil for an open network (no
// security section is written).
type JoinRequest struct {
	SSID string
	PSK  *string
}

// JoinUpdate is one streamed join transition. State is StateJoining (progress),
// StateJoined (IP populated), or StateFailed (Reason populated).
type JoinUpdate struct {
	State  sec1.ProvisioningState
	IP     string
	Reason sec1.WifiFailureReason
}

// Join creates + activates a WiFi profile for req and streams progress to emit:
// one or more StateJoining, then exactly one terminal StateJoined{IP} or
// StateFailed{Reason}. It returns when a terminal state is reached, the overall
// timeout fires, or ctx is cancelled (a retry/handshake superseded it — no
// terminal is emitted in that case, the superseding attempt emits its own). A
// failed/timed-out/cancelled attempt deletes its NetworkManager profile so
// retries never accumulate stale profiles; a successful join keeps it (WiFi must
// persist across reboot). The returned error is for logging; the authoritative
// outcome is always delivered via emit unless ctx was cancelled.
func (c *Client) Join(ctx context.Context, req JoinRequest, emit func(JoinUpdate)) error {
	dev, err := c.nm.WifiDevice(ctx)
	if err != nil {
		emit(JoinUpdate{State: sec1.StateFailed, Reason: sec1.ReasonInternal})
		return fmt.Errorf("netmgr: wifi device: %w", err)
	}
	connPath, active, err := c.nm.AddAndActivate(ctx, wifiSettings(req), dev)
	if err != nil {
		emit(JoinUpdate{State: sec1.StateFailed, Reason: sec1.ReasonInternal})
		return fmt.Errorf("netmgr: add+activate: %w", err)
	}

	emit(JoinUpdate{State: sec1.StateJoining})
	deadline := c.clk.Now().Add(c.joinTimeout)
	lastDev := uint32(0)
	haveLastDev := false

	for {
		if err := ctx.Err(); err != nil {
			c.cleanup(connPath) // superseded — abandon the half-activated profile
			return err
		}

		devState, devReason, derr := c.nm.DeviceStateReason(ctx, dev)
		acState, aerr := c.nm.ActiveConnectionState(ctx, active)

		if aerr == nil {
			switch acState {
			case activeStateActivated:
				ip := ""
				if s, e := c.nm.ActiveConnectionIP4(ctx, active); e == nil {
					ip = s
				}
				emit(JoinUpdate{State: sec1.StateJoined, IP: ip})
				return nil
			case activeStateDeactivated:
				emit(JoinUpdate{State: sec1.StateFailed, Reason: reasonOr(derr, devReason)})
				c.cleanup(connPath)
				return nil
			}
		}
		if derr == nil && devState == deviceStateFailed {
			emit(JoinUpdate{State: sec1.StateFailed, Reason: mapReason(devReason)})
			c.cleanup(connPath)
			return nil
		}

		// Progress heartbeat on each device-state edge while still activating.
		if derr == nil {
			if !haveLastDev {
				lastDev, haveLastDev = devState, true
			} else if devState != lastDev {
				lastDev = devState
				emit(JoinUpdate{State: sec1.StateJoining})
			}
		}

		if !c.clk.Now().Before(deadline) {
			emit(JoinUpdate{State: sec1.StateFailed, Reason: sec1.ReasonTimeout})
			c.cleanup(connPath)
			return nil
		}
		if err := c.clk.Sleep(ctx, c.pollInterval); err != nil {
			c.cleanup(connPath)
			return err
		}
	}
}

// cleanup deletes a connection profile best-effort using a background context
// (the join ctx may itself be cancelled). Called only on the failure/timeout/
// cancel paths — a successful join keeps its profile.
func (c *Client) cleanup(connPath dbus.ObjectPath) {
	if connPath == "" || connPath == "/" {
		return
	}
	_ = c.nm.DeleteConnection(context.Background(), connPath)
}

// reasonOr maps a device state-reason to the ADR enum, degrading to internal
// when the reason could not be read.
func reasonOr(readErr error, reason uint32) sec1.WifiFailureReason {
	if readErr != nil {
		return sec1.ReasonInternal
	}
	return mapReason(reason)
}

// mapReason maps an NMDeviceStateReason to the ADR-0002 WifiFailureReason.
//
//	SSID_NOT_FOUND (53)                                 → not_found
//	NO_SECRETS (7), SUPPLICANT_* (8,9,10,11)            → auth_failed
//	IP_CONFIG_UNAVAILABLE/EXPIRED (5,6), DHCP_* (15-17) → dhcp_timeout
//	(overall timeout, handled in Join)                  → timeout
//	anything else                                       → internal
//
// NOTE on NO_SECRETS→auth_failed: the B3 brief's shorthand grouped NO_SECRETS
// under not_found, but empirically a WRONG PSK in an embedded (agent-less)
// AddAndActivate profile surfaces as NM_DEVICE_STATE_REASON_NO_SECRETS — NM
// asks for a secret, none is forthcoming, and it gives up. Reporting that as
// "network not found" would tell the user the wrong thing; auth_failed ("wrong
// password") is the correct guidance. SSID genuinely out of range is the
// distinct SSID_NOT_FOUND (53) → not_found. See the report's open questions.
func mapReason(reason uint32) sec1.WifiFailureReason {
	switch reason {
	case reasonSSIDNotFound:
		return sec1.ReasonNotFound
	case reasonNoSecrets, reasonSupplicantDisconnect, reasonSupplicantConfigFail,
		reasonSupplicantFailed, reasonSupplicantTimeout:
		return sec1.ReasonAuthFailed
	case reasonIPConfigUnavailable, reasonIPConfigExpired,
		reasonDHCPStartFailed, reasonDHCPError, reasonDHCPFailed:
		return sec1.ReasonDHCPTimeout
	default:
		return sec1.ReasonInternal
	}
}

// classifySec maps AccessPoint Wpa/RsnFlags to the ADR sec string. SAE in
// RsnFlags is WPA3-Personal; any other WPA/RSN key-mgmt is treated as wpa2; no
// WPA/RSN flags is open. (A privacy-only WEP AP reports no WPA/RSN flags and is
// classified open — WEP is effectively dead and unjoinable via wpa-psk anyway.)
func classifySec(wpa, rsn uint32) string {
	if rsn&apSecKeyMgmtSAE != 0 {
		return "wpa3"
	}
	if wpa != 0 || rsn != 0 {
		return "wpa2"
	}
	return "open"
}

// rssiFromStrength maps NetworkManager's Strength (0..100 percent — the only
// signal metric the AccessPoint D-Bus API exposes; there is no raw dBm) to an
// approximate dBm: Strength/2 - 100, giving 100→-50 dBm and 0→-100 dBm. The app
// only sorts by rssi descending and displays it, so a monotonic, familiar-scale
// value is what matters; the mapping is documented as an approximation.
func rssiFromStrength(strength uint8) int {
	return int(strength)/2 - 100
}

// firstIP4Address extracts the first IPv4 address string from an IP4Config
// AddressData property (aa{sv} with "address"/"prefix" entries).
func firstIP4Address(addrData []map[string]dbus.Variant) string {
	for _, a := range addrData {
		if v, ok := a["address"]; ok {
			if s, ok := v.Value().(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// wifiSettings builds the AddAndActivateConnection settings dict for req: a
// 802-11-wireless infrastructure profile, plus a wpa-psk security section when a
// PSK is present (omitted for open networks). key-mgmt=wpa-psk associates to
// WPA2 and WPA2/WPA3-transition APs; a WPA3-only (SAE-only) AP is a known edge
// case (the join request carries no security hint) — see the report.
func wifiSettings(req JoinRequest) map[string]map[string]dbus.Variant {
	s := map[string]map[string]dbus.Variant{
		"connection": {
			"id":   dbus.MakeVariant(connectionID),
			"type": dbus.MakeVariant("802-11-wireless"),
		},
		"802-11-wireless": {
			"ssid": dbus.MakeVariant([]byte(req.SSID)),
			"mode": dbus.MakeVariant("infrastructure"),
		},
	}
	if req.PSK != nil {
		s["802-11-wireless-security"] = map[string]dbus.Variant{
			"key-mgmt": dbus.MakeVariant("wpa-psk"),
			"psk":      dbus.MakeVariant(*req.PSK),
		}
	}
	return s
}
