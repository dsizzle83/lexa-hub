package sec1

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"sync"

	"lexa-hub/internal/provision/frame"
)

// DefaultAttPayloadSize is the fallback ATT payload used for framing outgoing
// indications when a config does not set one (MTU 247 minus the 3-byte ATT
// header).
const DefaultAttPayloadSize = 244

// Outbound is one framed chunk the hub wants to send on a characteristic. The
// transport layer (unit B2) writes Chunk as an indication on UUID.
type Outbound struct {
	UUID  string
	Chunk []byte
}

// JoinBehavior scripts how the hub answers a join request. Mirrors the Dart
// JoinBehavior sealed hierarchy used by FakeHubPeripheral.
type JoinBehavior interface{ isJoinBehavior() }

// JoinSucceeds emits JoiningEvents "joining" states then "joined" with
// Handoff. A handoff with an empty serial is filled with the peripheral's.
type JoinSucceeds struct {
	Handoff       HandoffInfo
	JoiningEvents int
}

// JoinFails emits JoiningEvents "joining" states then "failed" with Reason.
type JoinFails struct {
	Reason        WifiFailureReason
	JoiningEvents int
}

// JoinHangs emits JoiningEvents "joining" states then goes silent (exercises
// the client's join timeout).
type JoinHangs struct {
	JoiningEvents int
}

func (JoinSucceeds) isJoinBehavior() {}
func (JoinFails) isJoinBehavior()    {}
func (JoinHangs) isJoinBehavior()    {}

// PeripheralConfig configures a hub-side session harness.
type PeripheralConfig struct {
	// Pop is the setup code (HKDF salt) the hub authenticates against.
	Pop string
	// Serial is the device serial, used to fill an empty handoff serial and to
	// build the info document.
	Serial string
	// Fw is the firmware/build version reported in the info document's "fw"
	// field. Empty preserves the original B1 placeholder ("sec1-go") — unit B2
	// wires the real internal/buildinfo.Version here so a plaintext info read
	// reports build truth instead of the placeholder.
	Fw string
	// Commissioned is the info document's "commissioned" flag. A hub that is
	// already commissioned still answers info reads (e.g. during a re-provision
	// window) but reports true; the default zero value (false) matches a
	// factory-fresh, uncommissioned unit.
	Commissioned bool
	// ScanResults answers a scan request. Nil yields an empty list.
	ScanResults []WifiAp
	// ScanFunc answers a scan request with a LIVE result (unit B3's
	// NetworkManager scan). When non-nil it takes precedence over ScanResults;
	// if it returns an error the peripheral falls back to ScanResults (possibly
	// empty) and records the error in LastError. Nil ⇒ the static ScanResults.
	ScanFunc func() ([]WifiAp, error)
	// JoinBehavior scripts join outcomes. Nil defaults to JoinHangs{}.
	JoinBehavior JoinBehavior
	// AsyncSend delivers a framed indication produced OUTSIDE the HandleChunk
	// return path — the streaming transport a JoinLive runner uses for its
	// status updates (unit B2's gatt.Server pushes each Outbound as an
	// indication via Server.Notify). Nil ⇒ live async emit is disabled; the
	// scripted behaviors never use it, so all B1/B2 tests leave it nil.
	AsyncSend func(Outbound)
	// AttPayloadSize frames outgoing indications. Zero uses
	// DefaultAttPayloadSize.
	AttPayloadSize int
	// Rand sources the handshake challenge. Nil uses crypto/rand.
	Rand io.Reader
}

// Peripheral is the hub (GATT peripheral) side of "LEXA Provision v1" as a
// pure, transport-free state machine: feed it the app-side chunks arriving on
// each characteristic via HandleChunk and it returns the framed hub responses
// to send. It runs the REAL sec1 crypto (X25519 + HKDF + AES-128-GCM),
// rejects wrong-PoP confirms with err pop_mismatch, enforces its own receive
// counters, and rejects post-handshake plaintext — a faithful port of
// FakeHubPeripheral. Unit B2 wires this to BlueZ; the crypto and protocol are
// entirely here.
//
// Not safe for concurrent use — a transport must serialize HandleChunk calls
// in characteristic-arrival order (the receive counters depend on it).
type Peripheral struct {
	pop            string
	serial         string
	fw             string
	commissioned   bool
	scanResults    []WifiAp
	scanFunc       func() ([]WifiAp, error)
	asyncSend      func(Outbound)
	attPayloadSize int
	rand           io.Reader

	// mu serializes session use (encryption counters + the abort flag) across
	// the serialized HandleChunk path and the asynchronous JoinLive emit
	// goroutine. Before B3 the peripheral was single-goroutine; a live join now
	// streams status indications from its own goroutine, so both paths take mu.
	mu sync.Mutex
	// joinCancel cancels the in-flight JoinLive runner when a new join (retry)
	// or a fresh handshake supersedes it.
	joinCancel context.CancelFunc

	// JoinBehavior may be reassigned between join retries within a session,
	// mirroring the mutable Dart field.
	JoinBehavior JoinBehavior

	reassemblers map[string]*frame.Reassembler
	session      *Session
	challenge    []byte
	confirmed    bool

	out []Outbound

	// PopFailures counts wrong-PoP confirms (a real hub throttles advertising
	// after 3).
	PopFailures int
	// JoinRequests records every decrypted join request, in order.
	JoinRequests []*Join
	// DoneReceived is set once the app sends done.
	DoneReceived bool
	// SessionAborted is set when the peripheral's own receive counters aborted
	// the session (e.g. a replayed write); it then goes silent, like a real
	// hub.
	SessionAborted bool
	// LastError holds the last swallowed protocol violation (kept instead of
	// failing the pump so callers can assert on it).
	LastError error
}

// NewPeripheral builds a hub-side harness from cfg.
func NewPeripheral(cfg PeripheralConfig) *Peripheral {
	att := cfg.AttPayloadSize
	if att == 0 {
		att = DefaultAttPayloadSize
	}
	rnd := cfg.Rand
	if rnd == nil {
		rnd = rand.Reader
	}
	jb := cfg.JoinBehavior
	if jb == nil {
		jb = JoinHangs{}
	}
	fw := cfg.Fw
	if fw == "" {
		fw = "sec1-go"
	}
	return &Peripheral{
		pop:            cfg.Pop,
		serial:         cfg.Serial,
		fw:             fw,
		commissioned:   cfg.Commissioned,
		scanResults:    cfg.ScanResults,
		scanFunc:       cfg.ScanFunc,
		asyncSend:      cfg.AsyncSend,
		attPayloadSize: att,
		rand:           rnd,
		JoinBehavior:   jb,
		reassemblers:   map[string]*frame.Reassembler{},
	}
}

// SessionEstablished reports whether the sec1 handshake completed.
func (p *Peripheral) SessionEstablished() bool { return p.confirmed }

// InfoDoc is what a plaintext read of the info characteristic returns.
func (p *Peripheral) InfoDoc() map[string]any {
	return map[string]any{
		"v":            1,
		"serial":       p.serial,
		"fw":           p.fw,
		"commissioned": p.commissioned,
		"sec":          []string{"sec1"},
	}
}

// HandleChunk feeds one app-side chunk arriving on characteristic uuid and
// returns the framed hub responses to send (possibly none). A returned error
// is also recorded in LastError; framing/decoding faults are non-fatal to the
// process, matching the Dart pump that catches into lastError.
func (p *Peripheral) HandleChunk(uuid string, chunk []byte) ([]Outbound, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.out = nil
	err := p.process(uuid, chunk)
	if err != nil {
		p.LastError = err
	}
	out := p.out
	p.out = nil
	return out, err
}

func (p *Peripheral) process(uuid string, chunk []byte) error {
	r := p.reassemblers[uuid]
	if r == nil {
		r = &frame.Reassembler{}
		p.reassemblers[uuid] = r
	}
	message, done, err := r.Add(chunk)
	if err != nil {
		return err
	}
	if !done {
		return nil
	}
	// ENC is constant within a message; the FIN chunk's flag is authoritative.
	encrypted := chunk[0]&frame.FlagENC != 0
	switch uuid {
	case UUIDSession:
		return p.handleSession(message, encrypted)
	case UUIDWifi:
		return p.handleWifi(message, encrypted)
	case UUIDConfig:
		return p.handleConfig(message, encrypted)
	default:
		return fmt.Errorf("provision: unexpected write to characteristic %s", uuid)
	}
}

func (p *Peripheral) handleSession(message []byte, encrypted bool) error {
	if !encrypted {
		msg, err := Decode(message)
		if err != nil {
			return err
		}
		hello, ok := msg.(*HelloApp)
		if !ok {
			return p.sendErr(UUIDSession, "bad_request")
		}
		// Fresh handshake (also allows a client retry after pop_mismatch). A new
		// handshake supersedes any in-flight live join from a prior session.
		p.cancelJoin()
		session, err := Generate(RoleHub)
		if err != nil {
			return err
		}
		if err := session.DeriveKey(hello.Pub, p.pop); err != nil {
			return err
		}
		p.session = session
		p.confirmed = false
		p.challenge, err = p.randomBytes(8)
		if err != nil {
			return err
		}
		return p.send(UUIDSession, &HelloHub{Pub: session.PublicKey(), Challenge: p.challenge}, false)
	}

	session := p.session
	challenge := p.challenge
	if session == nil || challenge == nil {
		return p.sendErr(UUIDSession, "bad_request")
	}
	clear, err := session.Decrypt(message)
	if err != nil {
		if errors.Is(err, ErrSessionAborted) {
			// Wrong PoP (or MITM): the app's K differs, so its confirm cannot
			// authenticate. Indistinguishable from tampering — by design.
			p.rejectPop()
			return p.sendErr(UUIDSession, "pop_mismatch")
		}
		return err
	}
	msg, err := Decode(clear)
	if err != nil {
		return err
	}
	confirm, ok := msg.(*Confirm)
	if ok && subtle.ConstantTimeCompare(confirm.Challenge, challenge) == 1 {
		p.confirmed = true
		return p.send(UUIDSession, &Ok{}, true)
	}
	p.rejectPop()
	return p.sendErr(UUIDSession, "pop_mismatch")
}

func (p *Peripheral) handleWifi(message []byte, encrypted bool) error {
	session := p.sessionFor(encrypted)
	if session == nil {
		return nil
	}
	clear, err := session.Decrypt(message)
	if err != nil {
		if errors.Is(err, ErrSessionAborted) {
			p.SessionAborted = true // go silent, like a real hub
			return nil
		}
		return err
	}
	msg, err := Decode(clear)
	if err != nil {
		return err
	}
	if _, ok := msg.(*ScanRequest); ok {
		return p.send(UUIDWifi, &WifiScanResult{APs: p.scanAPs()}, true)
	}
	return p.send(UUIDWifi, &Err{Code: "bad_request"}, true)
}

// scanAPs answers a scan request: the live ScanFunc when wired (falling back to
// the static ScanResults on error), else the static ScanResults.
func (p *Peripheral) scanAPs() []WifiAp {
	if p.scanFunc == nil {
		return p.scanResults
	}
	aps, err := p.scanFunc()
	if err != nil {
		p.LastError = err
		return p.scanResults
	}
	return aps
}

func (p *Peripheral) handleConfig(message []byte, encrypted bool) error {
	session := p.sessionFor(encrypted)
	if session == nil {
		return nil
	}
	clear, err := session.Decrypt(message)
	if err != nil {
		if errors.Is(err, ErrSessionAborted) {
			p.SessionAborted = true
			return nil
		}
		return err
	}
	msg, err := Decode(clear)
	if err != nil {
		return err
	}
	switch m := msg.(type) {
	case *Join:
		p.JoinRequests = append(p.JoinRequests, m)
		return p.runJoin()
	case *Done:
		p.DoneReceived = true
		return nil
	default:
		return p.send(UUIDStatus, &Err{Code: "bad_request"}, true)
	}
}

func (p *Peripheral) runJoin() error {
	emitJoining := func(count int) error {
		for i := 0; i < count; i++ {
			if err := p.send(UUIDStatus, &StateMessage{State: StateJoining}, true); err != nil {
				return err
			}
		}
		return nil
	}
	switch b := p.JoinBehavior.(type) {
	case JoinSucceeds:
		if err := emitJoining(b.JoiningEvents); err != nil {
			return err
		}
		return p.send(UUIDStatus, &StateMessage{
			State:   StateJoined,
			Handoff: p.withSerial(b.Handoff),
		}, true)
	case JoinFails:
		if err := emitJoining(b.JoiningEvents); err != nil {
			return err
		}
		reason := b.Reason
		return p.send(UUIDStatus, &StateMessage{State: StateFailed, Reason: &reason}, true)
	case JoinHangs:
		return emitJoining(b.JoiningEvents)
		// …and never conclude: the client's join timeout must fire.
	case JoinLive:
		// A real join streams over time. Answer the config write immediately
		// (no synchronous status) and run the driver in its own goroutine; each
		// state is encrypted, framed, and pushed on the status characteristic
		// via emitStatusAsync. godbus dispatches each GATT write on its own
		// goroutine, so this never blocks the transport.
		p.cancelJoin()
		if b.Run == nil || len(p.JoinRequests) == 0 {
			return nil
		}
		req := *p.JoinRequests[len(p.JoinRequests)-1]
		ctx, cancel := context.WithCancel(context.Background())
		p.joinCancel = cancel
		run := b.Run
		go func() {
			defer cancel()
			run(ctx, req, p.emitStatusAsync)
		}()
		return nil
	default:
		return nil
	}
}

// sessionFor returns the session if this message may be processed, else records
// the violation in LastError and returns nil (silent: an unauthenticated peer
// learns nothing). Mirrors _sessionFor — the post-handshake downgrade guard.
func (p *Peripheral) sessionFor(encrypted bool) *Session {
	if p.session == nil || !p.confirmed || !encrypted {
		p.LastError = errors.New("provision: message before established session, " +
			"or plaintext where ciphertext is required")
		return nil
	}
	return p.session
}

func (p *Peripheral) rejectPop() {
	p.PopFailures++
	p.session = nil
	p.challenge = nil
	p.confirmed = false
}

func (p *Peripheral) withSerial(h HandoffInfo) *HandoffInfo {
	if h.Serial == "" {
		h.Serial = p.serial
	}
	return &h
}

func (p *Peripheral) sendErr(uuid, code string) error {
	return p.send(uuid, &Err{Code: code}, false)
}

func (p *Peripheral) send(uuid string, message Message, encrypt bool) error {
	payload, err := message.Encode()
	if err != nil {
		return err
	}
	if encrypt {
		payload, err = p.session.Encrypt(payload)
		if err != nil {
			return err
		}
	}
	chunks, err := frame.Chunk(payload, p.attPayloadSize, encrypt)
	if err != nil {
		return err
	}
	for _, c := range chunks {
		p.out = append(p.out, Outbound{UUID: uuid, Chunk: c})
	}
	return nil
}

func (p *Peripheral) randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(p.rand, b); err != nil {
		return nil, fmt.Errorf("provision: read random: %w", err)
	}
	return b, nil
}
