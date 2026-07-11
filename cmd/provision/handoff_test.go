package main

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lexa-hub/internal/provision/gatt"
	"lexa-hub/internal/provision/netmgr"
	"lexa-hub/internal/provision/sec1"
)

// fixtureCert is the testdata API leaf cert. fixtureFP is its SHA-256-of-leaf-
// DER fingerprint computed INDEPENDENTLY (outside Go) with:
//
//	openssl x509 -in testdata/api-cert.pem -outform DER | sha256sum
//
// so this literal is a third-party check on fingerprintFromCertFile, not a
// value produced by the code under test.
const (
	fixtureCert = "testdata/api-cert.pem"
	fixtureFP   = "31000f8db6eb7893d48c88c9ac841748d95130d44e3a351109700e787a3bb45a"
)

// TestFingerprintFromCertFile_MatchesIndependentAndAPIAlgo proves three
// digests agree: (1) fingerprintFromCertFile (the code under test),
// (2) the openssl-derived literal above, and (3) an inline replication of
// cmd/api/tlscert.go's fingerprintOf — SHA-256 of the parsed leaf's DER
// (cert.Raw), lowercase hex. (3) is the exact algorithm lexa-api uses to fill
// /status.api_cert_fp, so agreement here is the guarantee the handed-off
// fingerprint equals what the app will pin against on the same box.
func TestFingerprintFromCertFile_MatchesIndependentAndAPIAlgo(t *testing.T) {
	got, err := fingerprintFromCertFile(fixtureCert)
	if err != nil {
		t.Fatalf("fingerprintFromCertFile: %v", err)
	}
	if got != fixtureFP {
		t.Fatalf("fingerprint = %s, want independent openssl digest %s", got, fixtureFP)
	}

	// (3) Replicate tlscert.go fingerprintOf: parse the leaf, hash cert.Raw.
	pemBytes, err := os.ReadFile(fixtureCert)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		t.Fatal("fixture is not PEM")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	sum := sha256.Sum256(leaf.Raw) // fingerprintOf hashes cert.Certificate[0] == leaf DER
	apiAlgo := hex.EncodeToString(sum[:])
	if apiAlgo != got {
		t.Fatalf("tlscert.go-algo digest %s != helper digest %s", apiAlgo, got)
	}
}

func TestFingerprintFromCertFile_Errors(t *testing.T) {
	if _, err := fingerprintFromCertFile(filepath.Join(t.TempDir(), "absent.pem")); err == nil {
		t.Fatal("absent cert file must error")
	}
	notPEM := filepath.Join(t.TempDir(), "garbage.pem")
	if err := os.WriteFile(notPEM, []byte("not a pem"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := fingerprintFromCertFile(notPEM); err == nil {
		t.Fatal("non-PEM content must error")
	}
}

func TestReadToken(t *testing.T) {
	dir := t.TempDir()
	t.Run("present, trailing newline trimmed", func(t *testing.T) {
		p := filepath.Join(dir, "tok")
		if err := os.WriteFile(p, []byte("  s3cr3t-token\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := readToken(p)
		if err != nil {
			t.Fatalf("readToken: %v", err)
		}
		if got != "s3cr3t-token" {
			t.Fatalf("token = %q, want s3cr3t-token", got)
		}
	})
	t.Run("absent → error", func(t *testing.T) {
		if _, err := readToken(filepath.Join(dir, "nope")); err == nil {
			t.Fatal("absent token file must error")
		}
	})
	t.Run("present-but-blank → error", func(t *testing.T) {
		p := filepath.Join(dir, "blank")
		if err := os.WriteFile(p, []byte("  \n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readToken(p); err == nil {
			t.Fatal("blank token file must error (fail loud)")
		}
	})
}

// TestHandoffSourceFill_FailSoft: an unreadable cert/token leaves that field
// empty rather than failing — the app degrades to TOFU + typed token.
func TestHandoffSourceFill_FailSoft(t *testing.T) {
	// Both present → both filled.
	tokFile := filepath.Join(t.TempDir(), "api.token")
	if err := os.WriteFile(tokFile, []byte("live-token-42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	full := handoffSource{certFile: fixtureCert, tokenFile: tokFile}
	h := &sec1.HandoffInfo{IP: "10.0.0.9", Port: 9100}
	full.fill(h)
	if h.APICertFP != fixtureFP {
		t.Fatalf("fp = %q, want %q", h.APICertFP, fixtureFP)
	}
	if h.Token != "live-token-42" {
		t.Fatalf("token = %q, want live-token-42", h.Token)
	}

	// Neither present → both empty, no panic/error (fail-soft).
	miss := handoffSource{certFile: "/no/such/cert", tokenFile: "/no/such/token"}
	h2 := &sec1.HandoffInfo{IP: "10.0.0.9"}
	miss.fill(h2)
	if h2.APICertFP != "" || h2.Token != "" {
		t.Fatalf("missing sources must leave fields empty, got %+v", h2)
	}

	// Cert only → fp filled, token empty.
	certOnly := handoffSource{certFile: fixtureCert, tokenFile: "/no/such/token"}
	h3 := &sec1.HandoffInfo{}
	certOnly.fill(h3)
	if h3.APICertFP != fixtureFP || h3.Token != "" {
		t.Fatalf("cert-only fill = %+v", h3)
	}
}

// TestHandoffRunner_FillsJoinedOnly checks the decorator fills the joined
// handoff and leaves non-joined states untouched.
func TestHandoffRunner_FillsJoinedOnly(t *testing.T) {
	tokFile := filepath.Join(t.TempDir(), "api.token")
	if err := os.WriteFile(tokFile, []byte("tok-abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := handoffSource{certFile: fixtureCert, tokenFile: tokFile}

	inner := func(ctx context.Context, req sec1.Join, emit func(sec1.StateMessage)) {
		emit(updateToState(netmgr.JoinUpdate{State: sec1.StateJoining}, 9100))
		emit(updateToState(netmgr.JoinUpdate{State: sec1.StateJoined, IP: "192.168.1.7"}, 9100))
	}

	var got []sec1.StateMessage
	handoffRunner(inner, src)(context.Background(), sec1.Join{SSID: "x"}, func(sm sec1.StateMessage) {
		got = append(got, sm)
	})

	if len(got) != 2 {
		t.Fatalf("want 2 states, got %d", len(got))
	}
	if got[0].Handoff != nil {
		t.Fatalf("joining state must have no handoff: %+v", got[0])
	}
	j := got[1]
	if j.State != sec1.StateJoined || j.Handoff == nil {
		t.Fatalf("terminal = %+v", j)
	}
	if j.Handoff.APICertFP != fixtureFP || j.Handoff.Token != "tok-abc" {
		t.Fatalf("joined handoff not filled: %+v", j.Handoff)
	}
	if j.Handoff.IP != "192.168.1.7" || j.Handoff.Port != 9100 {
		t.Fatalf("ip/port lost in decoration: %+v", j.Handoff)
	}
}

// TestFullFlow_QRToHandoff is the software proof of GAP-2: the complete
// commissioning flow through the real gatt.Dispatcher + sec1 peripheral —
// QR parse → PoP handshake → scan → join (fake netmgr runner emitting a real
// IP, wrapped by handoffRunner) → assert the streamed handoff carries a
// non-empty api_cert_fp matching the fixture's independently-computed digest,
// plus token + serial + ip → done. The radio proof is Phase C.
func TestFullFlow_QRToHandoff(t *testing.T) {
	// 1. Parse the label QR to get the pop + serial (as the app does).
	qr := "LEXA:v1;serial=" + testSerial + ";pop=" + testPop
	payload, ok := sec1.ParseQR(qr)
	if !ok {
		t.Fatalf("ParseQR(%q) failed", qr)
	}
	if payload.Pop != testPop || payload.Serial != testSerial {
		t.Fatalf("QR parse = %+v", payload)
	}

	// The API secrets the handoff must carry.
	tokFile := filepath.Join(t.TempDir(), "api.token")
	if err := os.WriteFile(tokFile, []byte("bearer-xyz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := handoffSource{certFile: fixtureCert, tokenFile: tokFile}

	// Fake netmgr join: two joining ticks, then joined with a real IP. Wrapped
	// by the same handoffRunner main() uses, so this exercises the B4 fill path.
	fakeInner := func(ctx context.Context, req sec1.Join, emit func(sec1.StateMessage)) {
		if req.SSID != "HomeNet" {
			t.Errorf("runner ssid = %q", req.SSID)
		}
		emit(updateToState(netmgr.JoinUpdate{State: sec1.StateJoining}, 9100))
		emit(updateToState(netmgr.JoinUpdate{State: sec1.StateJoined, IP: "192.168.1.42"}, 9100))
	}

	outCh := make(chan sec1.Outbound, 16)
	d := gatt.NewDispatcher(func() *sec1.Peripheral {
		return sec1.NewPeripheral(sec1.PeripheralConfig{
			Pop:            payload.Pop,
			Serial:         payload.Serial,
			AttPayloadSize: testAtt,
			ScanResults:    []sec1.WifiAp{{SSID: "HomeNet", RSSI: -40, Sec: "wpa2"}},
			JoinBehavior:   sec1.JoinLive{Run: handoffRunner(fakeInner, src)},
			AsyncSend:      func(o sec1.Outbound) { outCh <- o },
		})
	}, gatt.Observer{})

	a := newAppSide(t, d)

	// 2. Handshake with the PoP from the QR.
	a.handshake(payload.Pop)

	// 3. Scan (synchronous scan_result).
	scan := a.write(sec1.UUIDWifi, &sec1.ScanRequest{}, true)
	if len(scan) != 1 {
		t.Fatalf("want 1 scan_result, got %d", len(scan))
	}
	sr, ok := scan[0].(*sec1.WifiScanResult)
	if !ok || len(sr.APs) != 1 || sr.APs[0].SSID != "HomeNet" {
		t.Fatalf("scan_result = %+v", scan[0])
	}

	// 4. Join — answered async on status.
	psk := "hunter22"
	if sync := a.write(sec1.UUIDConfig, &sec1.Join{SSID: "HomeNet", PSK: &psk}, true); len(sync) != 0 {
		t.Fatalf("join must return no synchronous status, got %d", len(sync))
	}

	// Collect streamed states until joined.
	var states []*sec1.StateMessage
	deadline := time.After(2 * time.Second)
	for {
		select {
		case o := <-outCh:
			for _, m := range a.decode([]sec1.Outbound{o}) {
				sm, isState := m.(*sec1.StateMessage)
				if !isState {
					t.Fatalf("async message not a state: %T", m)
				}
				states = append(states, sm)
			}
		case <-deadline:
			t.Fatalf("timed out; got %d states", len(states))
		}
		if len(states) > 0 && states[len(states)-1].State == sec1.StateJoined {
			break
		}
	}

	fin := states[len(states)-1]
	if fin.State != sec1.StateJoined || fin.Handoff == nil {
		t.Fatalf("terminal = %+v", fin)
	}
	h := fin.Handoff
	// The GAP-2 assertions: every handoff field is present and correct.
	if h.Serial != testSerial {
		t.Errorf("handoff serial = %q, want %q (peripheral-filled)", h.Serial, testSerial)
	}
	if h.IP != "192.168.1.42" || h.Port != 9100 {
		t.Errorf("handoff ip/port = %s:%d", h.IP, h.Port)
	}
	if h.APICertFP != fixtureFP {
		t.Errorf("handoff api_cert_fp = %q, want fixture digest %q", h.APICertFP, fixtureFP)
	}
	if h.Token != "bearer-xyz" {
		t.Errorf("handoff token = %q, want bearer-xyz", h.Token)
	}

	// 5. done — session complete; the peripheral records it.
	if msgs := a.write(sec1.UUIDConfig, &sec1.Done{}, true); len(msgs) != 0 {
		t.Fatalf("done must produce no response, got %d", len(msgs))
	}
	if !d.DoneReceived() {
		t.Fatal("peripheral must record DoneReceived after done")
	}
}
