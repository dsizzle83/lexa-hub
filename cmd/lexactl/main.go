// Command lexactl is the power-user/engineer CLI for a LEXA hub, shipped in
// the device image (DEVICE_ROADMAP.md §7, unit 7.1). It is a thin HTTP
// client of the LOCAL lexa-api (cmd/api) and has NO OTHER path into the
// hub: every write it makes goes through POST /intent — the same choke
// point the mobile app and lexa-cloudlink use — so every lexactl-issued
// command is journaled exactly like any other intent
// (cmd/hub/intent.go's adopter, per DEVICE_ROADMAP.md §3.1). This is
// deliberate: lexactl bypassing that path would defeat the entire
// audit-trail property the intent system exists to provide.
//
// # Trust model (read this before scripting against an unfamiliar unit)
//
// lexa-api serves HTTPS with a per-device, self-signed certificate
// (cmd/api/tlscert.go) — there is no CA chain to verify against, so lexactl
// always makes its trust decision by FINGERPRINT, never by hostname/chain:
//
//   - `-addr http://...`   plain HTTP, no TLS at all (the bench-only escape
//     hatch api.json's "tls":false key documents).
//   - `-insecure`          skip verification entirely. Only ever
//     appropriate over loopback on a box already trusted for other
//     reasons.
//   - `-fingerprint <hex>` pin exactly that sha256 (lowercase hex, no
//     colons — printed at lexa-api startup, by `lexactl fingerprint`, and
//     on the unit's installer label/QR code). This is the flag for an
//     OFF-BOX invocation: an installer's laptop, or a CI job driving a
//     bench unit.
//   - (none of the above)  DEFAULT: lexactl reads the SAME on-disk cert
//     file lexa-api itself serves (/var/lib/lexa/api/cert.pem) and pins
//     its own fingerprint automatically. This is what makes a zero-flag
//     `lexactl status` work for an operator SSH'd into the unit — no
//     chain-of-trust HTTPS needed, because the CLI and the API share a
//     filesystem. Off-box, with no such file present, this fails LOUDLY
//     (never silently falling back to no verification) — pass
//     -fingerprint or -insecure instead.
//
// # Exit codes
//
//	0  success
//	1  API error, or a locally-validated request was rejected/invalid
//	2  usage error (bad flags/arguments)
//
// Usage: lexactl [global flags] <command> [args]
//
//	lexactl status [-json]
//	lexactl mode get [-json]
//	lexactl mode set optimizer|gateway [-json]
//	lexactl intent <kind> --json '<body>'
//	lexactl ev goal --target-kwh N --departure RFC3339|+2h [--initial-kwh N] [--station ID] [-json]
//	lexactl ev chargenow --ttl 90m [--station ID] [-json]
//	lexactl reserve set <pct> [-json]
//	lexactl scan run [--cidr X] [--watch] [-json]
//	lexactl scan show [-json]
//	lexactl fingerprint
//	lexactl telemetry [--minutes N] [-json]
//
// Global flags: -addr (default https://127.0.0.1:9100), -token-file
// (default /etc/lexa/api-secret), -insecure, -fingerprint <hex>.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main's testable body: parse global flags, dispatch to the named
// subcommand, return the process exit code. Every subcommand func has the
// shape func(*client, []string, io.Writer) int — main (this file) is flag
// dispatch only, per this unit's testability requirement.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("lexactl", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usageText) }

	addr := fs.String("addr", "https://127.0.0.1:9100", "lexa-api base URL (http:// for a plain, unencrypted client)")
	tokenFile := fs.String("token-file", defaultTokenFile, "path to the bearer token file")
	insecure := fs.Bool("insecure", false, "skip TLS certificate verification entirely (loopback dev only)")
	fingerprint := fs.String("fingerprint", "", "pin the server's leaf certificate sha256 (lowercase hex)")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return 2
	}
	cmdName, cmdArgs := rest[0], rest[1:]

	// Recognize the command name BEFORE doing any token/trust resolution:
	// an unknown command is a usage error (exit 2) regardless of whether
	// -addr/-token-file happen to be resolvable in this environment — a
	// typo'd subcommand on a box with no cert file yet must not be
	// misreported as an API/trust error.
	switch cmdName {
	case "fingerprint", "status", "mode", "intent", "ev", "reserve", "scan", "telemetry":
		// recognized; falls through to token/trust resolution below
	default:
		fmt.Fprintf(stderr, "lexactl: unknown command %q\n", cmdName)
		fs.Usage()
		return 2
	}

	// `fingerprint` is the one subcommand documented to work with no API up
	// and no token — resolve it before anything network- or trust-related.
	if cmdName == "fingerprint" {
		return cmdFingerprint(&client{certFile: defaultCertFile}, cmdArgs, stdout)
	}

	token, err := loadToken(*tokenFile)
	if err != nil {
		fmt.Fprintf(stdout, "error: %v\n", err)
		return 1
	}

	trust, err := resolveTrust(*addr, *insecure, *fingerprint, defaultCertFile)
	if err != nil {
		fmt.Fprintf(stdout, "error: %v\n", err)
		return 1
	}

	c := newClient(*addr, token, defaultCertFile, trust.tlsConfig)

	switch cmdName {
	case "status":
		return cmdStatus(c, cmdArgs, stdout)
	case "mode":
		return dispatchMode(c, cmdArgs, stdout)
	case "intent":
		return cmdIntent(c, cmdArgs, stdout)
	case "ev":
		return dispatchEV(c, cmdArgs, stdout)
	case "reserve":
		return dispatchReserve(c, cmdArgs, stdout)
	case "scan":
		return dispatchScan(c, cmdArgs, stdout)
	case "telemetry":
		return cmdTelemetry(c, cmdArgs, stdout)
	default:
		// Unreachable: cmdName was already validated against this exact set
		// above, before token/trust resolution ran.
		panic("lexactl: unreachable: unvalidated command " + cmdName)
	}
}

// loadToken reads the bearer token file. A MISSING file is not fatal —
// read routes work token-less against a unit still in the staged
// bearer-token rollout (api.json's api_token_file unset, mirroring
// cmd/api/auth.go's requireBearer empty-token-open default) — but an empty
// (existing-but-blank) file IS an error, the same fail-loud-not-silently-
// open discipline cmd/api/config.go's LoadAPIToken applies.
func loadToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read token file %s: %w", path, err)
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", fmt.Errorf("token file %s exists but is empty", path)
	}
	return tok, nil
}

const usageText = `lexactl — power-user/engineer CLI for a LEXA hub.

lexactl talks ONLY to the local lexa-api over HTTP(S); every write goes
through POST /intent, so every lexactl command is journaled exactly like an
app or cloud intent (nothing bypasses the intent journal).

Trust model: lexa-api serves a self-signed HTTPS certificate, so lexactl
verifies it by FINGERPRINT, not by hostname/chain:
  - https:// addr, no flags   -> reads the local cert file
                                  (/var/lib/lexa/api/cert.pem) and pins its
                                  own fingerprint automatically (on-box use).
  - -fingerprint <hex>        -> pin exactly that value (off-box use: read
                                  it off the unit's label, or a prior
                                  on-box "lexactl fingerprint" run).
  - -insecure                 -> skip verification entirely (loopback dev).
  - http:// addr              -> plain, unencrypted client (bench only).

Global flags:
  -addr string         lexa-api base URL (default "https://127.0.0.1:9100")
  -token-file string    bearer token file (default "/etc/lexa/api-secret")
  -insecure             skip TLS certificate verification
  -fingerprint string   pin the server leaf cert's sha256 (lowercase hex)

Commands:
  status                                    GET /status, human summary
  mode get                                   GET /mode
  mode set optimizer|gateway                 POST /intent kind=mode
  intent <kind> --json '<body>'              POST /intent, any whitelisted kind
  ev goal --target-kwh N --departure T       POST /intent kind=evgoal
     [--initial-kwh N] [--station ID]          (T: RFC3339 or +duration, e.g. +2h)
  ev chargenow --ttl 90m [--station ID]      POST /intent kind=chargenow
  reserve set <pct>                          POST /intent kind=reserve
  scan run [--cidr X] [--watch]              POST /scan
  scan show                                  GET /scan
  fingerprint                                print the local cert's sha256
  telemetry [--minutes N]                    GET /telemetry/recent

Every command accepts -json to print the raw API response body verbatim
instead of the human-readable summary.

Exit codes: 0 success, 1 API/validation error, 2 usage error.
`
