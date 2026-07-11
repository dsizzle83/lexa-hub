package sec1

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"
)

// Fixed seeds and PoP from the shared vector generator (test_vectors.dart).
const (
	appSeedHex = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	hubSeedHex = "202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f"
	vectorPop  = "8F3K2M9Q"
)

// handshake builds an (app, hub) pair that has completed key exchange under
// appPop / hubPop. Deterministic seeds make "same key, fresh counters"
// scenarios reproducible. Mirrors the Dart handshake() helper.
func handshake(t *testing.T, appPop, hubPop string) (app, hub *Session) {
	t.Helper()
	seed := func(s string) []byte {
		b, err := hex.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	var err error
	if app, err = FromSeed(RoleApp, seed(appSeedHex)); err != nil {
		t.Fatal(err)
	}
	if hub, err = FromSeed(RoleHub, seed(hubSeedHex)); err != nil {
		t.Fatal(err)
	}
	if err = app.DeriveKey(hub.PublicKey(), appPop); err != nil {
		t.Fatal(err)
	}
	if err = hub.DeriveKey(app.PublicKey(), hubPop); err != nil {
		t.Fatal(err)
	}
	return app, hub
}

func mustEncrypt(t *testing.T, s *Session, pt []byte) []byte {
	t.Helper()
	ct, err := s.Encrypt(pt)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	return ct
}

func TestSession_RoundTripsBothDirections(t *testing.T) {
	app, hub := handshake(t, vectorPop, vectorPop)
	for i := 0; i < 3; i++ {
		toHub := []byte("app message")
		got, err := hub.Decrypt(mustEncrypt(t, app, toHub))
		if err != nil || !bytes.Equal(got, toHub) {
			t.Fatalf("app->hub round trip %d: got %q err %v", i, got, err)
		}
		toApp := []byte("hub message")
		got, err = app.Decrypt(mustEncrypt(t, hub, toApp))
		if err != nil || !bytes.Equal(got, toApp) {
			t.Fatalf("hub->app round trip %d: got %q err %v", i, got, err)
		}
	}
	if app.SendCounter() != 3 || app.ReceiveCounter() != 3 {
		t.Fatalf("counters: send=%d recv=%d, want 3/3", app.SendCounter(), app.ReceiveCounter())
	}
}

func TestSession_GeneratePathAgrees(t *testing.T) {
	app, err := Generate(RoleApp)
	if err != nil {
		t.Fatal(err)
	}
	hub, err := Generate(RoleHub)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.DeriveKey(hub.PublicKey(), "p"); err != nil {
		t.Fatal(err)
	}
	if err := hub.DeriveKey(app.PublicKey(), "p"); err != nil {
		t.Fatal(err)
	}
	msg := []byte(`{"op":"confirm"}`)
	got, err := hub.Decrypt(mustEncrypt(t, app, msg))
	if err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("generate-path round trip: got %q err %v", got, err)
	}
}

func TestSession_SamePlaintextDiffersPerCounter(t *testing.T) {
	app, _ := handshake(t, vectorPop, vectorPop)
	msg := []byte(`{"op":"scan"}`)
	a := mustEncrypt(t, app, msg)
	b := mustEncrypt(t, app, msg)
	if bytes.Equal(a, b) {
		t.Fatal("same plaintext must encrypt differently per counter")
	}
}

func TestSession_TagLength(t *testing.T) {
	app, _ := handshake(t, vectorPop, vectorPop)
	ct := mustEncrypt(t, app, []byte("x"))
	if len(ct) != 1+TagLength {
		t.Fatalf("ciphertext len %d, want %d", len(ct), 1+TagLength)
	}
}

func TestSession_ReplayAbortsPermanently(t *testing.T) {
	app, hub := handshake(t, vectorPop, vectorPop)
	ct := mustEncrypt(t, app, []byte("once"))
	if _, err := hub.Decrypt(ct); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Decrypt(ct); !errors.Is(err, ErrSessionAborted) {
		t.Fatalf("replay: want ErrSessionAborted, got %v", err)
	}
	if !hub.IsAborted() {
		t.Fatal("session must be aborted after replay")
	}
	// Even a legitimate next message is refused after the abort.
	next := mustEncrypt(t, app, []byte("after"))
	if _, err := hub.Decrypt(next); !errors.Is(err, ErrSessionAborted) {
		t.Fatalf("post-abort: want ErrSessionAborted, got %v", err)
	}
}

func TestSession_DecrementedCounterAborts(t *testing.T) {
	app1, hub := handshake(t, vectorPop, vectorPop)
	if _, err := hub.Decrypt(mustEncrypt(t, app1, []byte("counter 0"))); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Decrypt(mustEncrypt(t, app1, []byte("counter 1"))); err != nil {
		t.Fatal(err)
	}
	// Same seed + peer => same K, but a fresh sender restarts at counter 0;
	// the hub (expecting 2) must abort.
	app2, _ := handshake(t, vectorPop, vectorPop)
	stale := mustEncrypt(t, app2, []byte("counter 0 again"))
	if _, err := hub.Decrypt(stale); !errors.Is(err, ErrSessionAborted) {
		t.Fatalf("decremented counter: want ErrSessionAborted, got %v", err)
	}
}

func TestSession_TamperedCiphertextAborts(t *testing.T) {
	app, hub := handshake(t, vectorPop, vectorPop)
	ct := mustEncrypt(t, app, []byte("important"))
	ct[0] ^= 0x01
	if _, err := hub.Decrypt(ct); !errors.Is(err, ErrSessionAborted) {
		t.Fatalf("tamper: want ErrSessionAborted, got %v", err)
	}
}

func TestSession_MismatchedPopUndecryptable(t *testing.T) {
	app, hub := handshake(t, "RIGHT", "WRONG")
	confirm := mustEncrypt(t, app, []byte(`{"op":"confirm"}`))
	if _, err := hub.Decrypt(confirm); !errors.Is(err, ErrSessionAborted) {
		t.Fatalf("pop mismatch: want ErrSessionAborted, got %v", err)
	}
}

func TestSession_TruncatedAborts(t *testing.T) {
	_, hub := handshake(t, vectorPop, vectorPop)
	if _, err := hub.Decrypt(make([]byte, TagLength-1)); !errors.Is(err, ErrSessionAborted) {
		t.Fatalf("truncated: want ErrSessionAborted, got %v", err)
	}
}

func TestSession_UseBeforeDeriveKey(t *testing.T) {
	app, err := Generate(RoleApp)
	if err != nil {
		t.Fatal(err)
	}
	if app.IsEstablished() {
		t.Fatal("must not be established before DeriveKey")
	}
	if _, err := app.Encrypt([]byte{1}); !errors.Is(err, ErrNotEstablished) {
		t.Fatalf("encrypt before derive: want ErrNotEstablished, got %v", err)
	}
	if _, err := app.Decrypt(make([]byte, 20)); !errors.Is(err, ErrNotEstablished) {
		t.Fatalf("decrypt before derive: want ErrNotEstablished, got %v", err)
	}
}

func TestSession_NonceLayout(t *testing.T) {
	if got := hex.EncodeToString(NonceFor(DirectionAppToHub, 0)); got != "010000000000000000000000" {
		t.Fatalf("app nonce c0: %s", got)
	}
	// Large-counter case: direction 0x02, counter 0x0102030405060708.
	if got := hex.EncodeToString(NonceFor(DirectionHubToApp, 0x0102030405060708)); got != "020000000102030405060708" {
		t.Fatalf("hub nonce large counter: %s", got)
	}
}
