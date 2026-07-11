package sec1

import (
	"bytes"
	"crypto/ecdh"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"lexa-hub/internal/provision/frame"
)

// vectorFile mirrors testdata/sec1_test_vectors.json — the byte-for-byte
// cross-language contract shared with the Dart app (packages/lexa_core). Every
// value below is reproduced from the seeds by the Go implementation; any drift
// fails loud.
type vectorFile struct {
	KeyDerivation struct {
		AppSeedHex      string `json:"app_seed_hex"`
		HubSeedHex      string `json:"hub_seed_hex"`
		AppPublicHex    string `json:"app_public_hex"`
		HubPublicHex    string `json:"hub_public_hex"`
		SharedSecretHex string `json:"shared_secret_hex"`
		Pop             string `json:"pop"`
		HkdfInfo        string `json:"hkdf_info"`
		DerivedKeyHex   string `json:"derived_key_hex"`
	} `json:"key_derivation"`
	AEAD struct {
		DirectionBytes struct {
			AppToHub int `json:"app_to_hub"`
			HubToApp int `json:"hub_to_app"`
		} `json:"direction_bytes"`
	} `json:"aead"`
	Messages []struct {
		Direction     string `json:"direction"`
		Counter       uint64 `json:"counter"`
		NonceHex      string `json:"nonce_hex"`
		Plaintext     string `json:"plaintext"`
		CiphertextHex string `json:"ciphertext_hex"`
	} `json:"messages"`
	Chunking struct {
		AttPayloadSize int      `json:"att_payload_size"`
		MessageHex     string   `json:"message_hex"`
		ChunksHex      []string `json:"chunks_hex"`
	} `json:"chunking"`
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/sec1_test_vectors.json")
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var vf vectorFile
	if err := json.Unmarshal(raw, &vf); err != nil {
		t.Fatalf("parse vectors: %v", err)
	}
	return vf
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

func eqHex(t *testing.T, label string, got []byte, wantHex string) {
	t.Helper()
	if g := hex.EncodeToString(got); g != wantHex {
		t.Fatalf("%s mismatch:\n got  %s\n want %s", label, g, wantHex)
	}
}

func TestVectors_KeyDerivation(t *testing.T) {
	vf := loadVectors(t)
	kd := vf.KeyDerivation

	appSeed := mustHex(t, kd.AppSeedHex)
	hubSeed := mustHex(t, kd.HubSeedHex)

	app, err := FromSeed(RoleApp, appSeed)
	if err != nil {
		t.Fatal(err)
	}
	hub, err := FromSeed(RoleHub, hubSeed)
	if err != nil {
		t.Fatal(err)
	}

	// Public keys — crypto/ecdh must clamp the seed exactly as Dart does.
	eqHex(t, "app_public", app.PublicKey(), kd.AppPublicHex)
	eqHex(t, "hub_public", hub.PublicKey(), kd.HubPublicHex)

	// Shared secret, recomputed independently through crypto/ecdh.
	appPriv, err := ecdh.X25519().NewPrivateKey(appSeed)
	if err != nil {
		t.Fatal(err)
	}
	hubPriv, err := ecdh.X25519().NewPrivateKey(hubSeed)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := appPriv.ECDH(hubPriv.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	eqHex(t, "shared_secret", shared, kd.SharedSecretHex)
	// ECDH is symmetric — the hub reaches the same secret.
	sharedRev, err := hubPriv.ECDH(appPriv.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	eqHex(t, "shared_secret (reversed)", sharedRev, kd.SharedSecretHex)

	// Derived key — both sides derive the same K; read the unexported key
	// (internal test) to reproduce the vector directly.
	if err := app.DeriveKey(hub.PublicKey(), kd.Pop); err != nil {
		t.Fatal(err)
	}
	if err := hub.DeriveKey(app.PublicKey(), kd.Pop); err != nil {
		t.Fatal(err)
	}
	eqHex(t, "derived_key (app)", app.key, kd.DerivedKeyHex)
	eqHex(t, "derived_key (hub)", hub.key, kd.DerivedKeyHex)

	if kd.HkdfInfo != HkdfInfo {
		t.Fatalf("hkdf_info %q != %q", kd.HkdfInfo, HkdfInfo)
	}
}

func TestVectors_AEADMessages(t *testing.T) {
	vf := loadVectors(t)
	kd := vf.KeyDerivation

	if vf.AEAD.DirectionBytes.AppToHub != int(DirectionAppToHub) ||
		vf.AEAD.DirectionBytes.HubToApp != int(DirectionHubToApp) {
		t.Fatalf("direction bytes drifted: %+v", vf.AEAD.DirectionBytes)
	}

	app, err := FromSeed(RoleApp, mustHex(t, kd.AppSeedHex))
	if err != nil {
		t.Fatal(err)
	}
	hub, err := FromSeed(RoleHub, mustHex(t, kd.HubSeedHex))
	if err != nil {
		t.Fatal(err)
	}
	if err := app.DeriveKey(hub.PublicKey(), kd.Pop); err != nil {
		t.Fatal(err)
	}
	if err := hub.DeriveKey(app.PublicKey(), kd.Pop); err != nil {
		t.Fatal(err)
	}

	// Messages are ordered by counter within each direction; encrypting /
	// decrypting them in file order keeps the per-direction counters aligned.
	for i, m := range vf.Messages {
		var sender, receiver *Session
		var dir byte
		switch m.Direction {
		case "app_to_hub":
			sender, receiver, dir = app, hub, DirectionAppToHub
		case "hub_to_app":
			sender, receiver, dir = hub, app, DirectionHubToApp
		default:
			t.Fatalf("message %d: unknown direction %q", i, m.Direction)
		}

		// Nonce.
		eqHex(t, "nonce", NonceFor(dir, m.Counter), m.NonceHex)

		// Encrypt reproduces the committed ciphertext exactly.
		if sender.SendCounter() != m.Counter {
			t.Fatalf("message %d: sender at counter %d, vector expects %d",
				i, sender.SendCounter(), m.Counter)
		}
		ct, err := sender.Encrypt([]byte(m.Plaintext))
		if err != nil {
			t.Fatal(err)
		}
		eqHex(t, "ciphertext["+m.Direction+"]", ct, m.CiphertextHex)

		// Decrypt the committed ciphertext back to the plaintext.
		if receiver.ReceiveCounter() != m.Counter {
			t.Fatalf("message %d: receiver at counter %d, vector expects %d",
				i, receiver.ReceiveCounter(), m.Counter)
		}
		clear, err := receiver.Decrypt(mustHex(t, m.CiphertextHex))
		if err != nil {
			t.Fatalf("message %d: decrypt: %v", i, err)
		}
		if string(clear) != m.Plaintext {
			t.Fatalf("message %d: decrypted %q, want %q", i, clear, m.Plaintext)
		}
	}
}

func TestVectors_Chunking(t *testing.T) {
	vf := loadVectors(t)
	ch := vf.Chunking

	msg := mustHex(t, ch.MessageHex)
	chunks, err := frame.Chunk(msg, ch.AttPayloadSize, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != len(ch.ChunksHex) {
		t.Fatalf("got %d chunks, want %d", len(chunks), len(ch.ChunksHex))
	}
	for i, c := range chunks {
		eqHex(t, "chunk", c, ch.ChunksHex[i])
	}

	// And the committed chunks reassemble to the committed message.
	var r frame.Reassembler
	var got []byte
	for _, hexChunk := range ch.ChunksHex {
		m, done, err := r.Add(mustHex(t, hexChunk))
		if err != nil {
			t.Fatal(err)
		}
		if done {
			got = m
		}
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("reassembled message mismatch:\n got  %x\n want %s", got, ch.MessageHex)
	}
}
