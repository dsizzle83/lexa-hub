package buildinfo

import "testing"

// TestDefaultVersionIsDev pins the unstamped default: any binary built
// without the Makefile's -ldflags -X (a bare `go build`, `go test`, or
// `go run`) must report "dev", never an empty string.
func TestDefaultVersionIsDev(t *testing.T) {
	if Version != "dev" {
		t.Fatalf("Version = %q, want %q (default before any -ldflags -X stamp)", Version, "dev")
	}
}

// TestFullReturnsVersion pins Full() as a passthrough of Version today —
// the seam a later git-sha suffix would extend.
func TestFullReturnsVersion(t *testing.T) {
	orig := Version
	defer func() { Version = orig }()

	Version = "1.2.3-test"
	if got := Full(); got != "1.2.3-test" {
		t.Fatalf("Full() = %q, want %q", got, "1.2.3-test")
	}
}
