package netmgr

import (
	"context"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"lexa-hub/internal/provision/sec1"
)

// --- fakes (no live bus) --------------------------------------------------

// joinStep is one poll's worth of NetworkManager state the fake returns.
type joinStep struct {
	ac        uint32 // Connection.Active.State
	devState  uint32 // Device.State
	devReason uint32 // Device.StateReason reason
}

type fakeNM struct {
	wifiDev dbus.ObjectPath
	wifiErr error

	aps    []dbus.ObjectPath
	apInfo map[dbus.ObjectPath]apRaw
	apErr  map[dbus.ObjectPath]error

	addConn   dbus.ObjectPath
	addActive dbus.ObjectPath
	addErr    error

	steps []joinStep
	cur   int
	ip    string

	requestScanned   bool
	capturedSettings map[string]map[string]dbus.Variant
	deleted          []dbus.ObjectPath
}

func (f *fakeNM) WifiDevice(context.Context) (dbus.ObjectPath, error) {
	return f.wifiDev, f.wifiErr
}
func (f *fakeNM) RequestScan(context.Context, dbus.ObjectPath) error {
	f.requestScanned = true
	return nil
}
func (f *fakeNM) AccessPoints(context.Context, dbus.ObjectPath) ([]dbus.ObjectPath, error) {
	return f.aps, nil
}
func (f *fakeNM) ReadAP(_ context.Context, ap dbus.ObjectPath) (apRaw, error) {
	if err := f.apErr[ap]; err != nil {
		return apRaw{}, err
	}
	return f.apInfo[ap], nil
}
func (f *fakeNM) AddAndActivate(_ context.Context, settings map[string]map[string]dbus.Variant, _ dbus.ObjectPath) (dbus.ObjectPath, dbus.ObjectPath, error) {
	f.capturedSettings = settings
	if f.addErr != nil {
		return "", "", f.addErr
	}
	return f.addConn, f.addActive, nil
}
func (f *fakeNM) idx() int {
	if f.cur >= len(f.steps) {
		return len(f.steps) - 1
	}
	return f.cur
}
func (f *fakeNM) DeviceStateReason(context.Context, dbus.ObjectPath) (uint32, uint32, error) {
	s := f.steps[f.idx()]
	return s.devState, s.devReason, nil
}
func (f *fakeNM) ActiveConnectionState(context.Context, dbus.ObjectPath) (uint32, error) {
	s := f.steps[f.idx()]
	if f.cur < len(f.steps)-1 {
		f.cur++ // advance the poll after the active-state read (last read per iter)
	}
	return s.ac, nil
}
func (f *fakeNM) ActiveConnectionIP4(context.Context, dbus.ObjectPath) (string, error) {
	return f.ip, nil
}
func (f *fakeNM) DeleteConnection(_ context.Context, conn dbus.ObjectPath) error {
	f.deleted = append(f.deleted, conn)
	return nil
}

// fakeClock advances virtual time on each Sleep so the timeout path needs no
// real waiting.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }
func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	c.now = c.now.Add(d)
	return nil
}

func testClient(nm nmConn) *Client {
	return newClient(nm, &fakeClock{now: time.Unix(0, 0)}, Options{
		JoinTimeout:  5 * time.Second,
		PollInterval: 1 * time.Second,
		ScanSettle:   0, // no settle in tests
	})
}

// collectJoin runs Join and records the streamed updates.
func collectJoin(t *testing.T, c *Client, req JoinRequest) []JoinUpdate {
	t.Helper()
	var got []JoinUpdate
	if err := c.Join(context.Background(), req, func(u JoinUpdate) { got = append(got, u) }); err != nil {
		// A returned error is fine (internal paths log it); the terminal update
		// is what the tests assert.
		_ = err
	}
	return got
}

func last(u []JoinUpdate) JoinUpdate { return u[len(u)-1] }

func hasJoining(u []JoinUpdate) bool {
	for _, x := range u {
		if x.State == sec1.StateJoining {
			return true
		}
	}
	return false
}

// --- classify / rssi / ip helper tests ------------------------------------

func TestClassifySec(t *testing.T) {
	const psk = 0x100 // NM_802_11_AP_SEC_KEY_MGMT_PSK
	cases := []struct {
		name     string
		wpa, rsn uint32
		want     string
	}{
		{"open", 0, 0, "open"},
		{"wpa2-rsn-psk", 0, psk, "wpa2"},
		{"wpa2-wpa-only", psk, 0, "wpa2"},
		{"wpa2-ccmp-psk", 0, 0x188, "wpa2"}, // real dev-kit RsnFlags=392
		{"wpa3-sae", 0, apSecKeyMgmtSAE, "wpa3"},
		{"wpa3-sae-and-psk", 0, apSecKeyMgmtSAE | psk, "wpa3"}, // SAE wins
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifySec(tc.wpa, tc.rsn); got != tc.want {
				t.Fatalf("classifySec(%#x,%#x) = %q, want %q", tc.wpa, tc.rsn, got, tc.want)
			}
		})
	}
}

func TestRssiFromStrength(t *testing.T) {
	cases := []struct {
		s    uint8
		want int
	}{{0, -100}, {100, -50}, {65, -68}, {50, -75}}
	for _, tc := range cases {
		if got := rssiFromStrength(tc.s); got != tc.want {
			t.Fatalf("rssiFromStrength(%d) = %d, want %d", tc.s, got, tc.want)
		}
	}
	// Sorting by rssi desc must equal sorting by strength desc (monotonic).
	if !(rssiFromStrength(80) > rssiFromStrength(40)) {
		t.Fatal("rssi not monotonic in strength")
	}
}

func TestFirstIP4Address(t *testing.T) {
	addr := []map[string]dbus.Variant{
		{"address": dbus.MakeVariant("192.168.1.42"), "prefix": dbus.MakeVariant(uint32(24))},
		{"address": dbus.MakeVariant("192.168.1.99"), "prefix": dbus.MakeVariant(uint32(24))},
	}
	if got := firstIP4Address(addr); got != "192.168.1.42" {
		t.Fatalf("firstIP4Address = %q, want 192.168.1.42", got)
	}
	if got := firstIP4Address(nil); got != "" {
		t.Fatalf("firstIP4Address(nil) = %q, want empty", got)
	}
}

// --- reason mapping (each documented path) --------------------------------

func TestMapReason(t *testing.T) {
	cases := []struct {
		reason uint32
		want   sec1.WifiFailureReason
	}{
		{reasonSSIDNotFound, sec1.ReasonNotFound},
		{reasonNoSecrets, sec1.ReasonAuthFailed},
		{reasonSupplicantDisconnect, sec1.ReasonAuthFailed},
		{reasonSupplicantConfigFail, sec1.ReasonAuthFailed},
		{reasonSupplicantFailed, sec1.ReasonAuthFailed},
		{reasonSupplicantTimeout, sec1.ReasonAuthFailed},
		{reasonIPConfigUnavailable, sec1.ReasonDHCPTimeout},
		{reasonIPConfigExpired, sec1.ReasonDHCPTimeout},
		{reasonDHCPStartFailed, sec1.ReasonDHCPTimeout},
		{reasonDHCPError, sec1.ReasonDHCPTimeout},
		{reasonDHCPFailed, sec1.ReasonDHCPTimeout},
		{0, sec1.ReasonInternal},
		{9999, sec1.ReasonInternal},
	}
	for _, tc := range cases {
		if got := mapReason(tc.reason); got != tc.want {
			t.Fatalf("mapReason(%d) = %v, want %v", tc.reason, got, tc.want)
		}
	}
}

// --- scan dedup / sort / classify -----------------------------------------

func TestScan_DedupSortClassifyTop20(t *testing.T) {
	const psk = 0x100
	nm := &fakeNM{
		wifiDev: "/dev/wifi",
		apInfo:  map[dbus.ObjectPath]apRaw{},
	}
	add := func(path, ssid string, strength uint8, wpa, rsn uint32) {
		p := dbus.ObjectPath(path)
		nm.aps = append(nm.aps, p)
		nm.apInfo[p] = apRaw{SsidBytes: []byte(ssid), Strength: strength, WpaFlags: wpa, RsnFlags: rsn}
	}
	// HomeNet appears twice; the stronger (70) must win.
	add("/ap/1", "HomeNet", 40, 0, psk)
	add("/ap/2", "HomeNet", 70, 0, psk)
	add("/ap/3", "OpenCafe", 55, 0, 0)
	add("/ap/4", "Secure3", 90, 0, apSecKeyMgmtSAE)
	add("/ap/5", "", 99, 0, psk) // hidden/empty SSID → dropped

	got, err := testClient(nm).Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !nm.requestScanned {
		t.Error("RequestScan was not called")
	}
	if len(got) != 3 {
		t.Fatalf("want 3 deduped APs, got %d: %+v", len(got), got)
	}
	// Sorted by rssi desc: Secure3(90) > HomeNet(70) > OpenCafe(55).
	if got[0].SSID != "Secure3" || got[0].Sec != "wpa3" {
		t.Fatalf("first = %+v, want Secure3/wpa3", got[0])
	}
	if got[1].SSID != "HomeNet" || got[1].RSSI != rssiFromStrength(70) || got[1].Sec != "wpa2" {
		t.Fatalf("second = %+v, want HomeNet@70/wpa2", got[1])
	}
	if got[2].SSID != "OpenCafe" || got[2].Sec != "open" {
		t.Fatalf("third = %+v, want OpenCafe/open", got[2])
	}
}

func TestScan_Top20Cap(t *testing.T) {
	nm := &fakeNM{wifiDev: "/dev/wifi", apInfo: map[dbus.ObjectPath]apRaw{}}
	for i := 0; i < 30; i++ {
		p := dbus.ObjectPath("/ap/" + string(rune('a'+i)))
		nm.aps = append(nm.aps, p)
		nm.apInfo[p] = apRaw{SsidBytes: []byte("net-" + string(rune('a'+i))), Strength: uint8(i + 1)}
	}
	got, err := testClient(nm).Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != maxScanResults {
		t.Fatalf("want top %d, got %d", maxScanResults, len(got))
	}
	// The strongest must survive the cap.
	if got[0].RSSI != rssiFromStrength(30) {
		t.Fatalf("strongest not kept: %+v", got[0])
	}
}

// --- join state machine + reason paths ------------------------------------

func TestJoin_Success(t *testing.T) {
	psk := "hunter22"
	nm := &fakeNM{
		wifiDev:   "/dev/wifi",
		addConn:   "/conn/1",
		addActive: "/active/1",
		ip:        "192.168.1.42",
		steps: []joinStep{
			{ac: activeStateActivating, devState: 40},
			{ac: activeStateActivating, devState: 70}, // device-state edge → a joining
			{ac: activeStateActivated, devState: 100},
		},
	}
	got := collectJoin(t, testClient(nm), JoinRequest{SSID: "HomeNet", PSK: &psk})
	if !hasJoining(got) {
		t.Fatalf("no joining update before terminal: %+v", got)
	}
	fin := last(got)
	if fin.State != sec1.StateJoined || fin.IP != "192.168.1.42" {
		t.Fatalf("terminal = %+v, want joined 192.168.1.42", fin)
	}
	if len(nm.deleted) != 0 {
		t.Fatalf("successful join must NOT delete its profile, deleted=%v", nm.deleted)
	}
	// PSK present ⇒ a wpa-psk security section is written.
	sec, ok := nm.capturedSettings["802-11-wireless-security"]
	if !ok || sec["key-mgmt"].Value() != "wpa-psk" || sec["psk"].Value() != "hunter22" {
		t.Fatalf("security section = %+v", nm.capturedSettings["802-11-wireless-security"])
	}
}

func TestJoin_OpenNetworkNoSecuritySection(t *testing.T) {
	nm := &fakeNM{
		wifiDev: "/dev/wifi", addConn: "/conn/1", addActive: "/active/1", ip: "10.0.0.5",
		steps: []joinStep{{ac: activeStateActivating, devState: 40}, {ac: activeStateActivated, devState: 100}},
	}
	got := collectJoin(t, testClient(nm), JoinRequest{SSID: "OpenCafe", PSK: nil})
	if last(got).State != sec1.StateJoined {
		t.Fatalf("terminal = %+v, want joined", last(got))
	}
	if _, ok := nm.capturedSettings["802-11-wireless-security"]; ok {
		t.Fatal("open network must NOT write a security section")
	}
}

func TestJoin_FailureReasons(t *testing.T) {
	cases := []struct {
		name   string
		reason uint32
		want   sec1.WifiFailureReason
	}{
		{"auth", reasonNoSecrets, sec1.ReasonAuthFailed},
		{"notfound", reasonSSIDNotFound, sec1.ReasonNotFound},
		{"dhcp", reasonDHCPFailed, sec1.ReasonDHCPTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			psk := "x"
			nm := &fakeNM{
				wifiDev: "/dev/wifi", addConn: "/conn/1", addActive: "/active/1",
				steps: []joinStep{
					{ac: activeStateActivating, devState: 40},
					{ac: activeStateDeactivated, devState: deviceStateFailed, devReason: tc.reason},
				},
			}
			got := collectJoin(t, testClient(nm), JoinRequest{SSID: "N", PSK: &psk})
			fin := last(got)
			if fin.State != sec1.StateFailed || fin.Reason != tc.want {
				t.Fatalf("terminal = %+v, want failed/%v", fin, tc.want)
			}
			if len(nm.deleted) != 1 || nm.deleted[0] != "/conn/1" {
				t.Fatalf("failed join must delete its profile, deleted=%v", nm.deleted)
			}
		})
	}
}

// TestJoin_DeviceFailedBeforeActiveFlip: an auth failure often shows on
// Device.State=FAILED while the active connection is still ACTIVATING; the
// reason is read from the device and mapped.
func TestJoin_DeviceFailedBeforeActiveFlip(t *testing.T) {
	psk := "wrong"
	nm := &fakeNM{
		wifiDev: "/dev/wifi", addConn: "/conn/1", addActive: "/active/1",
		steps: []joinStep{
			{ac: activeStateActivating, devState: 40},
			{ac: activeStateActivating, devState: deviceStateFailed, devReason: reasonSupplicantTimeout},
		},
	}
	got := collectJoin(t, testClient(nm), JoinRequest{SSID: "N", PSK: &psk})
	fin := last(got)
	if fin.State != sec1.StateFailed || fin.Reason != sec1.ReasonAuthFailed {
		t.Fatalf("terminal = %+v, want failed/auth_failed", fin)
	}
	if len(nm.deleted) != 1 {
		t.Fatalf("device-failed join must delete its profile, deleted=%v", nm.deleted)
	}
}

// TestJoin_Timeout: NM never reaches ACTIVATED; the bounded timeout fires and the
// profile is cleaned up. Uses the fake clock (no real waiting).
func TestJoin_Timeout(t *testing.T) {
	psk := "x"
	nm := &fakeNM{
		wifiDev: "/dev/wifi", addConn: "/conn/1", addActive: "/active/1",
		steps: []joinStep{{ac: activeStateActivating, devState: 40}}, // stuck forever
	}
	got := collectJoin(t, testClient(nm), JoinRequest{SSID: "N", PSK: &psk})
	fin := last(got)
	if fin.State != sec1.StateFailed || fin.Reason != sec1.ReasonTimeout {
		t.Fatalf("terminal = %+v, want failed/timeout", fin)
	}
	if len(nm.deleted) != 1 {
		t.Fatalf("timed-out join must delete its profile, deleted=%v", nm.deleted)
	}
}

func TestJoin_WifiDeviceError(t *testing.T) {
	nm := &fakeNM{wifiErr: context.DeadlineExceeded}
	got := collectJoin(t, testClient(nm), JoinRequest{SSID: "N"})
	if len(got) != 1 || got[0].State != sec1.StateFailed || got[0].Reason != sec1.ReasonInternal {
		t.Fatalf("want single failed/internal, got %+v", got)
	}
}

func TestJoin_AddActivateError(t *testing.T) {
	nm := &fakeNM{wifiDev: "/dev/wifi", addErr: context.DeadlineExceeded}
	got := collectJoin(t, testClient(nm), JoinRequest{SSID: "N"})
	if last(got).State != sec1.StateFailed || last(got).Reason != sec1.ReasonInternal {
		t.Fatalf("want failed/internal, got %+v", got)
	}
	if len(nm.deleted) != 0 {
		t.Fatalf("no profile was created; nothing to delete, deleted=%v", nm.deleted)
	}
}

// TestJoin_Cancelled: a superseding retry cancels the ctx; Join returns without a
// terminal and cleans up the abandoned profile.
func TestJoin_Cancelled(t *testing.T) {
	psk := "x"
	nm := &fakeNM{
		wifiDev: "/dev/wifi", addConn: "/conn/1", addActive: "/active/1",
		steps: []joinStep{{ac: activeStateActivating, devState: 40}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	var got []JoinUpdate
	_ = testClient(nm).Join(ctx, JoinRequest{SSID: "N", PSK: &psk}, func(u JoinUpdate) { got = append(got, u) })
	for _, u := range got {
		if u.State == sec1.StateJoined || u.State == sec1.StateFailed {
			t.Fatalf("cancelled join must not emit a terminal, got %+v", got)
		}
	}
	if len(nm.deleted) != 1 {
		t.Fatalf("cancelled join must clean up its profile, deleted=%v", nm.deleted)
	}
}
