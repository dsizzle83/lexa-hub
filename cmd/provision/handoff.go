package main

// handoff.go closes GAP-2: the authenticated hand-back of the API connection
// material — {api_cert_fp, token} — over the PoP-authenticated sec1 channel.
// Units B1–B3 built the encrypted transport and streamed a joined handoff
// carrying {serial, ip, port}; B4 fills the last two fields the app needs to
// reach the hub's HTTPS API without a trust-on-first-use leap or a
// hand-typed secret (ADR-0002 "Join handoff").
//
// The fingerprint is computed EXACTLY as cmd/api/tlscert.go's fingerprintOf
// does — SHA-256 of the leaf certificate's DER bytes, lowercase hex, no colon
// separators — so the value handed to the app equals lexa-api's own
// /status.api_cert_fp for the same box, with no coordination between the two
// services beyond sharing the cert file. (fingerprintOf hashes
// cert.Certificate[0]; for a PEM file that is exactly the first CERTIFICATE
// block's DER, which pem.Decode returns — the same bytes, the same digest.
// lexactl's trust.go relies on this identical equivalence.) We replicate the
// ~5 lines here rather than import cmd/api, which is package main.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"lexa-hub/internal/provision/sec1"
)

// handoffSource reads the two secrets the join handoff carries — the API
// server's leaf-cert fingerprint and its bearer token — FRESH at handoff time
// (not at process start): the cert and token files exist and are stable by
// commissioning time, and reading them late means a cert regenerated or a
// token rotated between boot and commissioning is still delivered correctly.
type handoffSource struct {
	certFile  string // API HTTPS leaf-cert PEM (lexa-api CertDir/cert.pem)
	tokenFile string // API bearer-token file (lexa-api api_token_file)
}

// fill populates h.APICertFP and h.Token, reading both sources fresh. It is
// deliberately FAIL-SOFT: an unreadable cert or token leaves that one field
// empty and logs a WARN, but never blocks the join. A missing fingerprint just
// means the app falls back to trust-on-first-use pinning (accept-on-first-
// HTTPS-contact); a missing token means the homeowner types it from the label.
// Failing the whole commissioning because a secret file is momentarily
// unreadable would be strictly worse than a graceful degrade to the pre-B4
// behavior, so we never do it.
func (s handoffSource) fill(h *sec1.HandoffInfo) {
	if fp, err := fingerprintFromCertFile(s.certFile); err != nil {
		slog.Warn("handoff: API cert fingerprint unavailable — app falls back to TOFU pinning",
			"cert_file", s.certFile, "err", err)
	} else {
		h.APICertFP = fp
	}
	if tok, err := readToken(s.tokenFile); err != nil {
		slog.Warn("handoff: API bearer token unavailable — homeowner must type it from the label",
			"token_file", s.tokenFile, "err", err)
	} else {
		h.Token = tok
	}
}

// handoffRunner wraps an inner join runner so every joined state it emits is
// enriched with the fresh API cert fingerprint + bearer token BEFORE it reaches
// the app. This keeps the B3 netmgr→state translation (updateToState) unchanged
// — it still emits an empty-fp/token handoff — and layers the B4 secret fill on
// top as a separate, independently testable decorator. Non-joined states (and a
// joined state with no handoff) pass through untouched. The peripheral separately
// fills the serial (withSerial), so between the two every handoff field is set.
func handoffRunner(inner sec1.JoinRunner, src handoffSource) sec1.JoinRunner {
	return func(ctx context.Context, req sec1.Join, emit func(sec1.StateMessage)) {
		inner(ctx, req, func(sm sec1.StateMessage) {
			if sm.State == sec1.StateJoined && sm.Handoff != nil {
				src.fill(sm.Handoff)
			}
			emit(sm)
		})
	}
}

// fingerprintFromCertFile returns the lowercase-hex SHA-256 of the leaf DER in
// a PEM certificate file — byte-identical to cmd/api/tlscert.go's fingerprintOf
// for the same cert. The first CERTIFICATE block is the leaf.
func fingerprintFromCertFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("%s: not a PEM certificate", path)
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:]), nil
}

// readToken reads the bearer-token file, trimmed. A present-but-blank file is
// an error (fail loud, not a silently empty token), the same discipline
// cmd/api/config.go's LoadAPIToken and lexactl's loadToken apply.
func readToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", fmt.Errorf("%s: token file is empty", path)
	}
	return tok, nil
}
