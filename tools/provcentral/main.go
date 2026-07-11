// Command provcentral is a DEV/BENCH BLE CENTRAL test harness for hardware-
// validating the lexa-provision GATT peripheral (ADR-0002, "LEXA Provision
// v1"). It is NOT a shipped service and is not wired into any build/deploy — it
// is the Phase C2 over-the-air counterpart to the pure-Go unit tests: it drives
// the sec1 CLIENT (app/central) role against a REAL BlueZ radio and a live
// peripheral.
//
// It imports the production sec1 (session crypto + messages) and frame (GATT
// chunk framing) packages verbatim, so a green run proves the shipped client
// crypto interoperates with the shipped peripheral over Bluetooth LE — not just
// against an in-process fake.
//
// It uses the DESKTOP's own BlueZ adapter (hci0) as the central via
// github.com/godbus/dbus/v5 on the system bus: Adapter1.StartDiscovery to find
// the peripheral, Device1.Connect + ServicesResolved, GattCharacteristic1
// ReadValue/WriteValue/StartNotify, and Properties.PropertiesChanged(Value) for
// indications.
//
// SAFETY: the join step (mode=full) deliberately targets a NON-EXISTENT SSID so
// the peripheral's NetworkManager join fails (state:failed, reason:not_found)
// WITHOUT reconfiguring any real network. The harness refuses to send a join for
// an SSID that does not look intentionally-nonexistent unless -allow-unsafe-ssid
// is passed. Never point this at a real SSID against a bench hub.
//
// Modes:
//
//	-mode full      discover → connect → info → handshake → scan → join(fail)
//	-mode wrongpop  discover → connect → N wrong-PoP handshakes (throttle test)
//	-mode discover  discover-only within a timeout (advertising-state probe;
//	                exit 0 = found, 3 = not found — for gate/throttle scripting)
//
// Usage examples:
//
//	go run ./tools/provcentral -mode full
//	go run ./tools/provcentral -mode wrongpop -pop WRONGPOP -attempts 3
//	go run ./tools/provcentral -mode discover -discover-timeout 20s
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"

	"lexa-hub/internal/provision/frame"
	"lexa-hub/internal/provision/sec1"
)

// D-Bus / BlueZ constants.
const (
	bluezSvc     = "org.bluez"
	adapterIface = "org.bluez.Adapter1"
	deviceIface  = "org.bluez.Device1"
	charIface    = "org.bluez.GattCharacteristic1"
	propsIface   = "org.freedesktop.DBus.Properties"
	objMgrIface  = "org.freedesktop.DBus.ObjectManager"
	propsChanged = "org.freedesktop.DBus.Properties.PropertiesChanged"
)

type opts struct {
	adapter         string
	name            string
	service         string
	pop             string
	mode            string
	attempts        int
	joinSSID        string
	joinPSK         string
	doJoin          bool
	doScan          bool
	discoverTimeout time.Duration
	connectTimeout  time.Duration
	msgTimeout      time.Duration
	joinTimeout     time.Duration
	allowUnsafeSSID bool
}

func main() {
	o := opts{}
	flag.StringVar(&o.adapter, "adapter", "hci0", "local BlueZ adapter to use as central")
	flag.StringVar(&o.name, "name", "LEXA-000001", "peripheral advertised LocalName to match")
	flag.StringVar(&o.service, "service", sec1.UUIDService, "provisioning service UUID to match")
	flag.StringVar(&o.pop, "pop", "LEXA-DEVKIT-POP", "proof-of-possession (HKDF salt)")
	flag.StringVar(&o.mode, "mode", "full", "full | wrongpop | discover")
	flag.IntVar(&o.attempts, "attempts", 3, "wrongpop: number of wrong-PoP handshakes")
	flag.StringVar(&o.joinSSID, "join-ssid", "LEXA-NONEXISTENT-TEST-SSID", "SSID for the join step (MUST be nonexistent)")
	flag.StringVar(&o.joinPSK, "join-psk", "x", "PSK for the join step")
	flag.BoolVar(&o.doJoin, "do-join", true, "full: perform the (safe, failing) join step")
	flag.BoolVar(&o.doScan, "do-scan", true, "full: perform the wifi scan step")
	flag.DurationVar(&o.discoverTimeout, "discover-timeout", 25*time.Second, "discovery timeout")
	flag.DurationVar(&o.connectTimeout, "connect-timeout", 25*time.Second, "connect + ServicesResolved timeout")
	flag.DurationVar(&o.msgTimeout, "msg-timeout", 15*time.Second, "per-response timeout")
	flag.DurationVar(&o.joinTimeout, "join-timeout", 70*time.Second, "whole-join stream timeout")
	flag.BoolVar(&o.allowUnsafeSSID, "allow-unsafe-ssid", false, "permit a join SSID that is not obviously nonexistent (DANGEROUS)")
	flag.Parse()

	logf("provcentral starting: mode=%s adapter=%s name=%s service=%s", o.mode, o.adapter, o.name, o.service)

	var err error
	switch o.mode {
	case "full":
		err = runFull(o)
	case "wrongpop":
		err = runWrongPop(o)
	case "discover":
		err = runDiscover(o)
	default:
		err = fmt.Errorf("unknown mode %q", o.mode)
	}
	if err != nil {
		logf("FAIL: %v", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// BlueZ central plumbing
// ---------------------------------------------------------------------------

type central struct {
	conn    *dbus.Conn
	adapter dbus.ObjectPath
	o       opts

	devPath  dbus.ObjectPath
	charPath map[string]dbus.ObjectPath // uuid -> char object path
	attPay   int

	cli   *sec1.Session
	reasm map[dbus.ObjectPath]*charStream // notify char path -> stream
}

type charStream struct {
	uuid string
	r    *frame.Reassembler
	ch   chan inMsg
}

type inMsg struct {
	data []byte
	enc  bool
}

func newCentral(o opts) (*central, error) {
	conn, err := dbus.SystemBus()
	if err != nil {
		return nil, fmt.Errorf("connect system bus: %w", err)
	}
	c := &central{
		conn:     conn,
		adapter:  dbus.ObjectPath("/org/bluez/" + o.adapter),
		o:        o,
		charPath: map[string]dbus.ObjectPath{},
		attPay:   sec1.DefaultAttPayloadSize,
		reasm:    map[dbus.ObjectPath]*charStream{},
	}
	return c, nil
}

func (c *central) adapterObj() dbus.BusObject { return c.conn.Object(bluezSvc, c.adapter) }

func (c *central) call(path dbus.ObjectPath, method string, args ...any) error {
	return c.conn.Object(bluezSvc, path).Call(method, 0, args...).Err
}

func (c *central) getProp(path dbus.ObjectPath, iface, prop string) (dbus.Variant, error) {
	var v dbus.Variant
	err := c.conn.Object(bluezSvc, path).Call(propsIface+".Get", 0, iface, prop).Store(&v)
	return v, err
}

func (c *central) managedObjects() (map[dbus.ObjectPath]map[string]map[string]dbus.Variant, error) {
	var objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	err := c.conn.Object(bluezSvc, dbus.ObjectPath("/")).Call(objMgrIface+".GetManagedObjects", 0).Store(&objs)
	return objs, err
}

// findDevice returns the object path + address of the advertising peripheral, or
// ("","") if not yet present. When requireRSSI is set, a match is accepted only
// if the device object currently carries an RSSI property — BlueZ populates RSSI
// only for devices from which it has received an advertisement during the active
// discovery, so this distinguishes a FRESH advertisement from a stale cached
// Device1 object (used by the advertising-state probe in discover mode).
func (c *central) findDevice(requireRSSI bool) (dbus.ObjectPath, string, int16) {
	objs, err := c.managedObjects()
	if err != nil {
		return "", "", 0
	}
	want := strings.ToLower(c.o.service)
	for path, ifaces := range objs {
		dev, ok := ifaces[deviceIface]
		if !ok || !strings.HasPrefix(string(path), string(c.adapter)+"/") {
			continue
		}
		name := variantStr(dev["Name"])
		alias := variantStr(dev["Alias"])
		match := name == c.o.name || alias == c.o.name
		if !match {
			if uuids, ok := dev["UUIDs"].Value().([]string); ok {
				for _, u := range uuids {
					if strings.ToLower(u) == want {
						match = true
						break
					}
				}
			}
		}
		if !match {
			continue
		}
		rssiV, hasRSSI := dev["RSSI"]
		if requireRSSI && !hasRSSI {
			continue // cached but not freshly advertising
		}
		var rssi int16
		if r, ok := rssiV.Value().(int16); ok {
			rssi = r
		}
		return path, variantStr(dev["Address"]), rssi
	}
	return "", "", 0
}

// discover starts LE discovery and waits until the peripheral appears. When
// requireRSSI is set, only a freshly-advertising device (RSSI present) counts —
// the advertising-state probe used by discover mode.
func (c *central) discover(requireRSSI bool) error {
	// Best-effort: power on + LE-only filter. Failures are non-fatal.
	_ = c.conn.Object(bluezSvc, c.adapter).Call(propsIface+".Set", 0, adapterIface, "Powered", dbus.MakeVariant(true)).Err
	filter := map[string]dbus.Variant{
		"Transport": dbus.MakeVariant("le"),
		"UUIDs":     dbus.MakeVariant([]string{c.o.service}),
	}
	_ = c.call(c.adapter, adapterIface+".SetDiscoveryFilter", filter)
	if err := c.call(c.adapter, adapterIface+".StartDiscovery"); err != nil {
		// "Operation already in progress" is fine.
		if !strings.Contains(err.Error(), "InProgress") {
			logf("StartDiscovery: %v (continuing)", err)
		}
	}
	deadline := time.Now().Add(c.o.discoverTimeout)
	for time.Now().Before(deadline) {
		if path, addr, rssi := c.findDevice(requireRSSI); path != "" {
			logf("DISCOVERY: found %q at %s (%s) rssi=%d", c.o.name, addr, path, rssi)
			c.devPath = path
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("discovery timed out after %s (peripheral not advertising as %q)", c.o.discoverTimeout, c.o.name)
}

func (c *central) stopDiscovery() { _ = c.call(c.adapter, adapterIface+".StopDiscovery") }

// removeDevice drops any cached device object so a subsequent discovery reflects
// a FRESH advertisement (needed for gate/throttle probes). It disconnects first:
// a still-connected device from a prior session stays in the object tree
// regardless of advertising and would be a false positive. BlueZ does not
// resurrect a purged, non-advertising device, so after this a match means a
// fresh advertisement.
func (c *central) removeDevice() {
	if path, _, _ := c.findDevice(false); path != "" {
		_ = c.call(path, deviceIface+".Disconnect")
		_ = c.call(c.adapter, adapterIface+".RemoveDevice", path)
		time.Sleep(500 * time.Millisecond)
	}
}

func (c *central) connect() error {
	logf("CONNECT: %s", c.devPath)
	c.stopDiscovery()
	// Connect can transiently fail (le-connection-abort-by-local); retry a few.
	var lastErr error
	for i := 0; i < 4; i++ {
		if err := c.call(c.devPath, deviceIface+".Connect"); err != nil {
			lastErr = err
			logf("Connect attempt %d: %v (retrying)", i+1, err)
			time.Sleep(1500 * time.Millisecond)
			continue
		}
		lastErr = nil
		break
	}
	if lastErr != nil {
		return fmt.Errorf("connect: %w", lastErr)
	}
	// Wait for ServicesResolved.
	deadline := time.Now().Add(c.o.connectTimeout)
	for time.Now().Before(deadline) {
		if v, err := c.getProp(c.devPath, deviceIface, "ServicesResolved"); err == nil {
			if resolved, _ := v.Value().(bool); resolved {
				logf("CONNECT: services resolved")
				return c.resolveChars()
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("services not resolved within %s", c.o.connectTimeout)
}

func (c *central) resolveChars() error {
	objs, err := c.managedObjects()
	if err != nil {
		return err
	}
	wanted := map[string]bool{
		sec1.UUIDInfo: true, sec1.UUIDSession: true, sec1.UUIDWifi: true,
		sec1.UUIDConfig: true, sec1.UUIDStatus: true,
	}
	for path, ifaces := range objs {
		ch, ok := ifaces[charIface]
		if !ok || !strings.HasPrefix(string(path), string(c.devPath)+"/") {
			continue
		}
		uuid := strings.ToLower(variantStr(ch["UUID"]))
		if wanted[uuid] {
			c.charPath[uuid] = path
		}
	}
	var missing []string
	for u := range wanted {
		if _, ok := c.charPath[u]; !ok {
			missing = append(missing, u)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing characteristics: %v", missing)
	}
	logf("CHARS: resolved all 5 characteristic paths")
	// Negotiated ATT MTU (BlueZ >=5.62 exposes it on the char). Fall back to a
	// small safe payload so writes fit even the minimal 23-byte ATT MTU.
	c.attPay = 20
	if v, err := c.getProp(c.charPath[sec1.UUIDSession], charIface, "MTU"); err == nil {
		if mtu, ok := v.Value().(uint16); ok && int(mtu) > 23 {
			c.attPay = int(mtu) - 3
			logf("CHARS: negotiated ATT MTU=%d -> payload=%d", mtu, c.attPay)
		}
	}
	return nil
}

// startNotify subscribes to the notifiable characteristics and wires their
// PropertiesChanged(Value) indications into per-char frame reassemblers.
func (c *central) startNotify(uuids ...string) {
	if err := c.conn.AddMatchSignal(
		dbus.WithMatchInterface(propsIface),
		dbus.WithMatchMember("PropertiesChanged"),
	); err != nil {
		logf("AddMatchSignal: %v", err)
	}
	sigCh := make(chan *dbus.Signal, 256)
	c.conn.Signal(sigCh)

	for _, u := range uuids {
		path := c.charPath[u]
		cs := &charStream{uuid: u, r: &frame.Reassembler{}, ch: make(chan inMsg, 16)}
		c.reasm[path] = cs
		if err := c.call(path, charIface+".StartNotify"); err != nil {
			logf("StartNotify(%s): %v", shortUUID(u), err)
		} else {
			logf("NOTIFY: subscribed %s", shortUUID(u))
		}
	}
	go c.signalLoop(sigCh)
}

func (c *central) signalLoop(sigCh chan *dbus.Signal) {
	for sig := range sigCh {
		if sig.Name != propsChanged || len(sig.Body) < 2 {
			continue
		}
		iface, _ := sig.Body[0].(string)
		if iface != charIface {
			continue
		}
		changed, _ := sig.Body[1].(map[string]dbus.Variant)
		val, ok := changed["Value"]
		if !ok {
			continue
		}
		b, ok := val.Value().([]byte)
		if !ok || len(b) == 0 {
			continue
		}
		cs := c.reasm[sig.Path]
		if cs == nil {
			continue
		}
		enc := b[0]&frame.FlagENC != 0
		msg, done, err := cs.r.Add(b)
		if err != nil {
			logf("frame error on %s: %v", shortUUID(cs.uuid), err)
			continue
		}
		if done {
			cs.ch <- inMsg{data: msg, enc: enc}
		}
	}
}

// writeMessage encodes, optionally sec1-encrypts, frames, and writes m on uuid.
func (c *central) writeMessage(uuid string, m sec1.Message, encrypt bool) error {
	payload, err := m.Encode()
	if err != nil {
		return fmt.Errorf("encode %s: %w", m.Op(), err)
	}
	if encrypt {
		payload, err = c.cli.Encrypt(payload)
		if err != nil {
			return fmt.Errorf("encrypt %s: %w", m.Op(), err)
		}
	}
	chunks, err := frame.Chunk(payload, c.attPay, encrypt)
	if err != nil {
		return fmt.Errorf("chunk %s: %w", m.Op(), err)
	}
	path := c.charPath[uuid]
	opt := map[string]dbus.Variant{"type": dbus.MakeVariant("request")}
	for _, ch := range chunks {
		if err := c.call(path, charIface+".WriteValue", ch, opt); err != nil {
			return fmt.Errorf("write %s chunk: %w", m.Op(), err)
		}
	}
	return nil
}

// readMessage waits for the next reassembled message on uuid, decrypts it if the
// ENC flag was set, and decodes it.
func (c *central) readMessage(uuid string, timeout time.Duration) (sec1.Message, error) {
	cs := c.reasm[c.charPath[uuid]]
	if cs == nil {
		return nil, fmt.Errorf("no notify stream for %s", shortUUID(uuid))
	}
	select {
	case in := <-cs.ch:
		data := in.data
		if in.enc {
			clear, err := c.cli.Decrypt(in.data)
			if err != nil {
				return nil, fmt.Errorf("decrypt on %s: %w", shortUUID(uuid), err)
			}
			data = clear
		}
		return sec1.Decode(data)
	case <-time.After(timeout):
		return nil, fmt.Errorf("timeout (%s) waiting for message on %s", timeout, shortUUID(uuid))
	}
}

func (c *central) disconnect() {
	if c.devPath != "" {
		_ = c.call(c.devPath, deviceIface+".Disconnect")
	}
}

// ---------------------------------------------------------------------------
// Handshake (sec1 client / RoleApp)
// ---------------------------------------------------------------------------

// handshake runs HelloApp → HelloHub → DeriveKey(pop) → Confirm → Ok. A wrong
// PoP surfaces as a plaintext Err{pop_mismatch}, which is returned (not an
// error) so callers can assert on it.
func (c *central) handshake(pop string) (established bool, errCode string, err error) {
	c.cli, err = sec1.Generate(sec1.RoleApp)
	if err != nil {
		return false, "", fmt.Errorf("generate client session: %w", err)
	}
	if err := c.writeMessage(sec1.UUIDSession, &sec1.HelloApp{Pub: c.cli.PublicKey()}, false); err != nil {
		return false, "", err
	}
	msg, err := c.readMessage(sec1.UUIDSession, c.o.msgTimeout)
	if err != nil {
		return false, "", fmt.Errorf("await HelloHub: %w", err)
	}
	hub, ok := msg.(*sec1.HelloHub)
	if !ok {
		return false, "", fmt.Errorf("expected HelloHub, got %s", msg.Op())
	}
	if err := c.cli.DeriveKey(hub.Pub, pop); err != nil {
		return false, "", fmt.Errorf("derive key: %w", err)
	}
	if err := c.writeMessage(sec1.UUIDSession, &sec1.Confirm{Challenge: hub.Challenge}, true); err != nil {
		return false, "", err
	}
	reply, err := c.readMessage(sec1.UUIDSession, c.o.msgTimeout)
	if err != nil {
		return false, "", fmt.Errorf("await Ok/Err: %w", err)
	}
	switch r := reply.(type) {
	case *sec1.Ok:
		return true, "", nil
	case *sec1.Err:
		return false, r.Code, nil
	default:
		return false, "", fmt.Errorf("expected Ok or Err, got %s", reply.Op())
	}
}

// ---------------------------------------------------------------------------
// Modes
// ---------------------------------------------------------------------------

func runFull(o opts) error {
	c, err := newCentral(o)
	if err != nil {
		return err
	}
	defer c.disconnect()

	if err := c.discover(false); err != nil {
		return err
	}
	if err := c.connect(); err != nil {
		return err
	}

	// Step 3: info read (plaintext).
	if err := c.readInfo(); err != nil {
		return err
	}

	// Step 4: subscribe to indication characteristics. config (0005) is
	// write-only in the ADR GATT layout — StartNotify is attempted for
	// completeness and is expected to be rejected; its replies come on status.
	c.startNotify(sec1.UUIDSession, sec1.UUIDWifi, sec1.UUIDStatus)
	if err := c.call(c.charPath[sec1.UUIDConfig], charIface+".StartNotify"); err != nil {
		logf("NOTIFY: config (0005) not notifiable as expected: %v", err)
	}

	// Step 5: handshake with correct PoP.
	logf("HANDSHAKE: begin (pop=%q)", o.pop)
	est, code, err := c.handshake(o.pop)
	if err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	if !est {
		return fmt.Errorf("handshake NOT established (err code=%q) — expected success with correct PoP", code)
	}
	logf("HANDSHAKE: session established (X25519 + HKDF + AES-128-GCM over the air)")

	// Step 6: wifi scan.
	if o.doScan {
		if err := c.scan(); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
	}

	// Step 7: safe join (nonexistent SSID) → expect failure.
	if o.doJoin {
		if err := c.join(); err != nil {
			return fmt.Errorf("join: %w", err)
		}
	}

	logf("FULL: over-the-air commissioning flow complete")
	return nil
}

func (c *central) readInfo() error {
	var raw []byte
	err := c.conn.Object(bluezSvc, c.charPath[sec1.UUIDInfo]).
		Call(charIface+".ReadValue", 0, map[string]dbus.Variant{}).Store(&raw)
	if err != nil {
		return fmt.Errorf("read info: %w", err)
	}
	logf("INFO: raw = %s", string(raw))
	var doc struct {
		V            int      `json:"v"`
		Serial       string   `json:"serial"`
		Fw           string   `json:"fw"`
		Commissioned bool     `json:"commissioned"`
		Sec          []string `json:"sec"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse info json: %w", err)
	}
	// Assertions.
	var problems []string
	if doc.Serial != "lexa-devkit-000001" {
		problems = append(problems, fmt.Sprintf("serial=%q want lexa-devkit-000001", doc.Serial))
	}
	if doc.Fw == "" {
		problems = append(problems, "fw is empty")
	}
	if doc.Commissioned {
		problems = append(problems, "commissioned=true want false")
	}
	if !contains(doc.Sec, "sec1") {
		problems = append(problems, fmt.Sprintf("sec=%v does not include sec1", doc.Sec))
	}
	if len(problems) > 0 {
		return fmt.Errorf("info assertions failed: %s", strings.Join(problems, "; "))
	}
	logf("INFO: assertions PASS (serial=%s fw=%s commissioned=%v sec=%v)",
		doc.Serial, doc.Fw, doc.Commissioned, doc.Sec)
	return nil
}

func (c *central) scan() error {
	logf("SCAN: requesting AP scan")
	if err := c.writeMessage(sec1.UUIDWifi, &sec1.ScanRequest{}, true); err != nil {
		return err
	}
	msg, err := c.readMessage(sec1.UUIDWifi, 20*time.Second)
	if err != nil {
		return err
	}
	res, ok := msg.(*sec1.WifiScanResult)
	if !ok {
		return fmt.Errorf("expected scan_result, got %s", msg.Op())
	}
	logf("SCAN: %d access point(s):", len(res.APs))
	for i, ap := range res.APs {
		logf("  [%d] ssid=%q rssi=%d sec=%s", i, ap.SSID, ap.RSSI, ap.Sec)
	}
	return nil
}

func (c *central) join() error {
	if !looksNonexistent(c.o.joinSSID) && !c.o.allowUnsafeSSID {
		return fmt.Errorf("refusing join: SSID %q does not look intentionally nonexistent; "+
			"a real join could reconfigure the hub. Pass -allow-unsafe-ssid to override (DANGEROUS)", c.o.joinSSID)
	}
	logf("JOIN: sending SAFE join for NONEXISTENT ssid=%q (expect state:failed)", c.o.joinSSID)
	psk := c.o.joinPSK
	if err := c.writeMessage(sec1.UUIDConfig, &sec1.Join{SSID: c.o.joinSSID, PSK: &psk}, true); err != nil {
		return err
	}
	// Stream state messages on status until a terminal state.
	deadline := time.Now().Add(c.o.joinTimeout)
	for {
		remain := time.Until(deadline)
		if remain <= 0 {
			return fmt.Errorf("no terminal join state within %s", c.o.joinTimeout)
		}
		msg, err := c.readMessage(sec1.UUIDStatus, remain)
		if err != nil {
			return err
		}
		sm, ok := msg.(*sec1.StateMessage)
		if !ok {
			return fmt.Errorf("expected state, got %s", msg.Op())
		}
		switch sm.State {
		case sec1.StateJoining:
			logf("JOIN: state=joining")
		case sec1.StateFailed:
			reason := "?"
			if sm.Reason != nil {
				reason = sm.Reason.Wire()
			}
			logf("JOIN: state=FAILED reason=%s (expected — safe nonexistent SSID)", reason)
			return nil
		case sec1.StateJoined:
			// Should be impossible for a nonexistent SSID.
			return fmt.Errorf("UNEXPECTED state=joined for nonexistent SSID (handoff serial=%q ip=%q) — "+
				"a real network was reconfigured", safeSerial(sm), safeIP(sm))
		default:
			logf("JOIN: state=%s", sm.State.Wire())
		}
	}
}

func runWrongPop(o opts) error {
	c, err := newCentral(o)
	if err != nil {
		return err
	}
	defer c.disconnect()

	if err := c.discover(false); err != nil {
		return err
	}
	if err := c.connect(); err != nil {
		return err
	}
	c.startNotify(sec1.UUIDSession)

	pop := o.pop
	if pop == "LEXA-DEVKIT-POP" {
		pop = "WRONGPOP" // guard: wrongpop mode must use a wrong PoP
	}
	logf("WRONGPOP: driving %d handshake(s) with wrong pop=%q", o.attempts, pop)
	for i := 1; i <= o.attempts; i++ {
		est, code, err := c.handshake(pop)
		if err != nil {
			return fmt.Errorf("attempt %d: %w", i, err)
		}
		if est {
			return fmt.Errorf("attempt %d: session ESTABLISHED with wrong PoP — SECURITY FAILURE", i)
		}
		if code != "pop_mismatch" {
			return fmt.Errorf("attempt %d: expected err code pop_mismatch, got %q", i, code)
		}
		logf("WRONGPOP: attempt %d correctly REJECTED (err code=pop_mismatch)", i)
		time.Sleep(500 * time.Millisecond)
	}
	logf("WRONGPOP: all %d attempts rejected; peripheral should now be at %d pop failures", o.attempts, o.attempts)
	return nil
}

func runDiscover(o opts) error {
	c, err := newCentral(o)
	if err != nil {
		return err
	}
	// Drop any cached device so a hit means a FRESH advertisement.
	_ = c.call(c.adapter, adapterIface+".SetDiscoveryFilter", map[string]dbus.Variant{
		"Transport": dbus.MakeVariant("le"),
		"UUIDs":     dbus.MakeVariant([]string{o.service}),
	})
	c.removeDevice()
	err = c.discover(false)
	c.stopDiscovery()
	if err != nil {
		logf("DISCOVER: NOT FOUND within %s (peripheral not advertising)", o.discoverTimeout)
		os.Exit(3)
	}
	logf("DISCOVER: FOUND (peripheral is advertising)")
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func logf(format string, a ...any) {
	fmt.Printf("%s  %s\n", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, a...))
}

func variantStr(v dbus.Variant) string {
	s, _ := v.Value().(string)
	return s
}

func shortUUID(u string) string {
	if len(u) >= 8 {
		return u[4:8]
	}
	return u
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// looksNonexistent is a safety heuristic: only SSIDs that clearly signal "this
// network does not exist" are allowed for the join test without an override.
func looksNonexistent(ssid string) bool {
	u := strings.ToUpper(ssid)
	return strings.Contains(u, "NONEXISTENT") || strings.Contains(u, "DOES-NOT-EXIST") || strings.Contains(u, "TEST-SSID")
}

func safeSerial(sm *sec1.StateMessage) string {
	if sm.Handoff != nil {
		return sm.Handoff.Serial
	}
	return ""
}

func safeIP(sm *sec1.StateMessage) string {
	if sm.Handoff != nil {
		return sm.Handoff.IP
	}
	return ""
}
