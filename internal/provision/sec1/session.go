// Package sec1 implements the "LEXA Provision v1" BLE commissioning session
// crypto and hub-side handshake (ADR-0002,
// docs/gap-close/ADR-0002-ble-commissioning-protocol.md).
//
// It is a byte-for-byte port of the Dart reference in
// packages/lexa_core/lib/src/ble/ (session.dart, messages.dart, protocol.dart,
// fake_peripheral.dart). Correctness is pinned by the shared conformance
// vectors in testdata/sec1_test_vectors.json, which both sides must reproduce
// exactly.
//
// Crypto, stdlib only:
//
//   - X25519 ECDH via crypto/ecdh. Seeds are raw 32-byte private scalars;
//     crypto/ecdh clamps per RFC 7748 during scalar multiplication, identical
//     to Dart's package:cryptography newKeyPairFromSeed.
//   - K = HKDF-SHA256(ikm = shared secret, salt = UTF8(pop),
//     info = UTF8("lexa-prov-v1"), len = 16) via crypto/hkdf. The PoP is the
//     HKDF *salt*, not the AEAD nonce.
//   - AES-128-GCM (crypto/aes + crypto/cipher). Wire = ciphertext‖tag(16B).
//     The 12-byte nonce is never transmitted: direction(1B) ‖ 0x000000 ‖
//     counter(8B big-endian), an independent counter per direction starting at
//     0 and incremented once per message. Any GCM authentication failure
//     permanently aborts the session.
package sec1

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

// HkdfInfo is the HKDF info string (ADR-0002 handshake step 3).
const HkdfInfo = "lexa-prov-v1"

// KeyLength is the derived AES-128 key length in bytes.
const KeyLength = 16

// TagLength is the AES-GCM authentication tag appended to every ciphertext.
const TagLength = 16

// NonceLength is the AES-GCM nonce length in bytes.
const NonceLength = 12

// Nonce direction bytes (ADR-0002).
const (
	// DirectionAppToHub is the nonce direction byte for app → hub messages.
	DirectionAppToHub byte = 0x01
	// DirectionHubToApp is the nonce direction byte for hub → app messages.
	DirectionHubToApp byte = 0x02
)

// Role fixes which nonce direction byte this endpoint sends and receives.
type Role int

const (
	// RoleApp is the phone/central end: sends app→hub, receives hub→app.
	RoleApp Role = iota
	// RoleHub is the device/peripheral end: sends hub→app, receives app→hub.
	RoleHub
)

func (r Role) sendDirection() byte {
	if r == RoleHub {
		return DirectionHubToApp
	}
	return DirectionAppToHub
}

func (r Role) receiveDirection() byte {
	if r == RoleHub {
		return DirectionAppToHub
	}
	return DirectionHubToApp
}

// ErrSessionAborted is returned once a session is unrecoverable: an incoming
// message failed authenticated decryption under the expected nonce counter
// (replay, reorder, tampering, or a mismatched key), or the session was used
// after such a failure. It mirrors Dart's SessionAbortedException. Distinguish
// it with errors.Is — the hub harness maps it to "go silent" / pop_mismatch.
var ErrSessionAborted = errors.New("sec1: session aborted")

// ErrNotEstablished is returned by Encrypt/Decrypt before DeriveKey has
// completed. It mirrors the Dart StateError and is deliberately distinct from
// ErrSessionAborted (a usage bug, not an attack).
var ErrNotEstablished = errors.New("sec1: DeriveKey must complete before encrypt/decrypt")

// Session holds one endpoint's sec1 state: its ephemeral X25519 keypair, the
// derived AES-GCM key, and the per-direction message counters. One Session per
// endpoint per BLE session. It is not safe for concurrent use; the hub harness
// serializes access.
type Session struct {
	role           Role
	priv           *ecdh.PrivateKey
	aead           cipher.AEAD
	key            []byte
	sendCounter    uint64
	receiveCounter uint64
	aborted        bool
}

// Generate builds a Session with a fresh ephemeral X25519 keypair — the
// production path.
func Generate(role Role) (*Session, error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("sec1: generate keypair: %w", err)
	}
	return &Session{role: role, priv: priv}, nil
}

// FromSeed builds a Session from a fixed 32-byte X25519 private scalar. For
// deterministic test vectors only. The scalar is clamped per RFC 7748 during
// scalar multiplication, exactly as the Dart reference does, so the same seed
// yields the same keys in both implementations.
func FromSeed(role Role, seed []byte) (*Session, error) {
	priv, err := ecdh.X25519().NewPrivateKey(seed)
	if err != nil {
		return nil, fmt.Errorf("sec1: private key from seed: %w", err)
	}
	return &Session{role: role, priv: priv}, nil
}

// Role reports which end of the session this endpoint is.
func (s *Session) Role() Role { return s.role }

// PublicKey returns the raw 32-byte X25519 public key for the hello message.
func (s *Session) PublicKey() []byte { return s.priv.PublicKey().Bytes() }

// DeriveKey performs ECDH against peerPublic (the peer's raw 32-byte hello key)
// and HKDF-SHA256 with the PoP as salt to derive the 16-byte AES-128 session
// key (ADR-0002 step 3). It must complete before Encrypt/Decrypt.
func (s *Session) DeriveKey(peerPublic []byte, pop string) error {
	peer, err := ecdh.X25519().NewPublicKey(peerPublic)
	if err != nil {
		return fmt.Errorf("sec1: peer public key: %w", err)
	}
	shared, err := s.priv.ECDH(peer)
	if err != nil {
		return fmt.Errorf("sec1: ECDH: %w", err)
	}
	key, err := hkdf.Key(sha256.New, shared, []byte(pop), HkdfInfo, KeyLength)
	if err != nil {
		return fmt.Errorf("sec1: HKDF: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return fmt.Errorf("sec1: aes: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("sec1: gcm: %w", err)
	}
	s.key = key
	s.aead = aead
	return nil
}

// IsEstablished reports whether DeriveKey has completed.
func (s *Session) IsEstablished() bool { return s.aead != nil }

// IsAborted reports whether a decryption failure has poisoned the session.
func (s *Session) IsAborted() bool { return s.aborted }

// SendCounter is the counter the next Encrypt will use (== messages sent).
func (s *Session) SendCounter() uint64 { return s.sendCounter }

// ReceiveCounter is the counter the next Decrypt expects (== messages received).
func (s *Session) ReceiveCounter() uint64 { return s.receiveCounter }

// NonceFor builds the 12-byte nonce: direction ‖ 0x000000 ‖ counter(8B BE).
// Exposed for the shared test vectors.
func NonceFor(direction byte, counter uint64) []byte {
	nonce := make([]byte, NonceLength)
	nonce[0] = direction
	binary.BigEndian.PutUint64(nonce[4:], counter)
	return nonce
}

// Encrypt seals one whole message and returns ciphertext‖tag(16B), ready for
// framing with the ENC flag. Consumes one send-counter value. Encrypting after
// an abort returns ErrSessionAborted; before DeriveKey, ErrNotEstablished.
func (s *Session) Encrypt(plaintext []byte) ([]byte, error) {
	if err := s.checkUsable(); err != nil {
		return nil, err
	}
	nonce := NonceFor(s.role.sendDirection(), s.sendCounter)
	out := s.aead.Seal(nil, nonce, plaintext, nil)
	s.sendCounter++
	return out, nil
}

// Decrypt opens one whole reassembled ciphertext‖tag message under the exact
// next expected receive counter. ANY failure — a short message, a counter that
// is not the expected value (replay/reorder), tampering, or a peer that
// derived a different key — permanently aborts the session and returns
// ErrSessionAborted. Before DeriveKey it returns ErrNotEstablished without
// aborting.
func (s *Session) Decrypt(message []byte) ([]byte, error) {
	if err := s.checkUsable(); err != nil {
		return nil, err
	}
	if len(message) < TagLength {
		s.aborted = true
		return nil, fmt.Errorf("%w: ciphertext shorter than GCM tag", ErrSessionAborted)
	}
	nonce := NonceFor(s.role.receiveDirection(), s.receiveCounter)
	clear, err := s.aead.Open(nil, nonce, message, nil)
	if err != nil {
		s.aborted = true
		return nil, fmt.Errorf("%w: message failed authentication for expected counter %d "+
			"(replayed, reordered, tampered, or wrong key)", ErrSessionAborted, s.receiveCounter)
	}
	s.receiveCounter++
	return clear, nil
}

// checkUsable mirrors the Dart _checkUsable ordering: aborted first, then
// established.
func (s *Session) checkUsable() error {
	if s.aborted {
		return fmt.Errorf("%w: session already aborted", ErrSessionAborted)
	}
	if s.aead == nil {
		return ErrNotEstablished
	}
	return nil
}
