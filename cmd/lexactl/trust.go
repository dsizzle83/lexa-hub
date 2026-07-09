package main

// trust.go implements lexactl's trust model for talking to the local
// lexa-api over HTTPS. lexa-api serves a per-device, self-signed leaf
// certificate (cmd/api/tlscert.go) — there is no CA chain for anyone but
// this box to verify, so lexactl always makes its trust decision by
// FINGERPRINT (sha256 of the leaf DER, lowercase hex, no colons — the exact
// format cmd/api/tlscert.go's fingerprintOf produces and /status's
// "api_cert_fp" field reports; pinned by the 4.1 review's contract, see
// docs/extension/00_PROGRESS.md), never by hostname/chain-of-trust:
//
//   - `-addr http://...`   — plain HTTP, no TLS in play at all (the bench
//     escape hatch api.json's "tls":false key documents).
//   - `-insecure`          — skip verification entirely. Only ever
//     appropriate over loopback on a box already trusted for other reasons
//     (e.g. an operator SSH'd into the unit who doesn't want to fuss with
//     the fingerprint flag).
//   - `-fingerprint <hex>` — pin exactly that value. This is the correct
//     flag for an OFF-BOX invocation: an installer's laptop on the same
//     LAN, or a CI job driving a bench unit, reads the fingerprint off the
//     unit's printed label/QR code (or a prior `lexactl fingerprint` run)
//     and passes it explicitly.
//   - neither given        — DEFAULT: lexactl reads the SAME on-disk cert
//     file lexa-api itself serves (defaultCertFile) and pins ITS
//     fingerprint automatically. This is what makes a zero-flag `lexactl
//     status` work for an on-box/SSH'd-in invocation: the CLI and the API
//     share a filesystem, so the CLI can read the exact cert the API
//     presents and pin it without the operator typing anything. Off-box,
//     that file doesn't exist, so this path fails LOUDLY (never silently
//     degrading to no verification) — the error message tells the caller
//     to pass -fingerprint or -insecure instead.
//
// In every pinned case (explicit flag or the local-file default) lexactl
// sets InsecureSkipVerify and substitutes its own check in
// VerifyPeerCertificate: Go's normal chain verification would always fail
// against a self-signed leaf anyway, and the fingerprint comparison IS the
// trust decision here — deliberately narrower than chain trust ("is this
// EXACTLY the cert I already know about", not "is this signed by someone I
// trust").
import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
)

// defaultCertFile is lexa-api's own on-disk HTTPS server certificate path
// (cmd/api/tlscert.go's CertDir default + its certFileName constant).
const defaultCertFile = "/var/lib/lexa/api/cert.pem"

// defaultTokenFile is where lexa-api's bearer secret lives on-box
// (manufacturing-provisioned; api.json's api_token_file default).
const defaultTokenFile = "/etc/lexa/api-secret"

// trustConfig is the resolved outcome of the addr/insecure/fingerprint
// trio: tlsConfig is nil for a plain http:// addr (no TLS in play at all).
type trustConfig struct {
	tlsConfig *tls.Config
	// source records which rule produced tlsConfig — surfaced only for
	// tests/diagnostics, never printed on the normal success path.
	source string // "none" | "insecure" | "flag" | "local-cert-file"
}

// resolveTrust implements the precedence documented in this file's package
// doc. certFile is the local-cert-file path to fall back to when neither
// insecure nor fingerprintFlag is set — production code always passes
// defaultCertFile; tests pass a fixture path.
func resolveTrust(addr string, insecure bool, fingerprintFlag string, certFile string) (trustConfig, error) {
	switch {
	case strings.HasPrefix(addr, "http://"):
		return trustConfig{source: "none"}, nil
	case strings.HasPrefix(addr, "https://"):
		// fall through to the TLS-trust logic below
	default:
		return trustConfig{}, fmt.Errorf("-addr %q must start with http:// or https://", addr)
	}

	if insecure {
		return trustConfig{
			tlsConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // explicit -insecure, loopback dev only
			source:    "insecure",
		}, nil
	}

	if fp := strings.ToLower(strings.TrimSpace(fingerprintFlag)); fp != "" {
		if err := validateFingerprint(fp); err != nil {
			return trustConfig{}, fmt.Errorf("-fingerprint: %w", err)
		}
		return trustConfig{tlsConfig: pinnedTLSConfig(fp), source: "flag"}, nil
	}

	fp, err := fingerprintFromCertFile(certFile)
	if err != nil {
		return trustConfig{}, fmt.Errorf(
			"no -fingerprint given and the local cert file is unavailable (%s: %w) — "+
				"off-box invocations must pass -fingerprint <hex> (from the unit's label, or a prior "+
				"on-box `lexactl fingerprint`) or -insecure", certFile, err)
	}
	return trustConfig{tlsConfig: pinnedTLSConfig(fp), source: "local-cert-file"}, nil
}

// pinnedTLSConfig returns a tls.Config that accepts a server ONLY if its
// leaf certificate's sha256 fingerprint equals want (lowercase hex).
func pinnedTLSConfig(want string) *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // verified below via VerifyPeerCertificate instead
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("lexactl: server presented no certificate")
			}
			sum := sha256.Sum256(rawCerts[0])
			got := hex.EncodeToString(sum[:])
			if got != want {
				return fmt.Errorf("lexactl: server certificate fingerprint mismatch: got %s, want %s "+
					"(possible impostor, or the unit regenerated its cert — re-verify out of band before retrying)",
					got, want)
			}
			return nil
		},
	}
}

// validateFingerprint rejects an obviously-malformed -fingerprint value
// (wrong length or non-hex) up front, rather than silently pinning a value
// that could never match anything.
func validateFingerprint(fp string) error {
	if len(fp) != sha256.Size*2 {
		return fmt.Errorf("expected %d lowercase hex characters (sha256), got %d", sha256.Size*2, len(fp))
	}
	if _, err := hex.DecodeString(fp); err != nil {
		return fmt.Errorf("not valid hex: %w", err)
	}
	return nil
}

// fingerprintFromCertFile loads a PEM certificate file and returns the
// lowercase-hex sha256 of its leaf DER bytes — the exact value
// cmd/api/tlscert.go's fingerprintOf computes for the same file, so an
// on-box lexactl and lexa-api always agree with no coordination needed.
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
