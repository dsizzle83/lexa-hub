// Package buildinfo holds the process-wide build-version string every
// service can stamp into its own surfaces (today: lexa-api's mDNS TXT
// "fw=", GET /site.fw, and GET /status.fw — see cmd/api). It exists so a
// single build-injected value has one home instead of each service
// inventing its own placeholder.
//
// Version is deliberately a var, not a const: the Makefile's lexa-api (and,
// generically, any other service) build passes
//
//	-ldflags "-X lexa-hub/internal/buildinfo.Version=$(VERSION)"
//
// which the Go linker resolves by overwriting this package-level string
// variable's data at link time — `-X` only works on vars of type string,
// never on untyped consts, since a const has no address for the linker to
// patch. A binary built WITHOUT that flag (a bare `go build`, `go test`,
// or `go run`) keeps the "dev" default below, so every non-release build
// (and every test in this repo) sees a stable, obviously-a-placeholder
// value rather than an empty string or a stale stamp from someone else's
// last release build.
package buildinfo

// Version is the build-injected firmware/service version string. See the
// package doc for how the Makefile's -ldflags -X sets this on a real
// build; "dev" is the unstamped default.
var Version = "dev"

// Full returns the version string this build reports. It exists as a
// stable call site now so a later addition (e.g. appending a short git
// SHA, `Version + "+" + gitSHA`) doesn't require touching every caller —
// today it's simply Version.
func Full() string {
	return Version
}
