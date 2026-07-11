package main

import (
	"os"
	"path/filepath"
	"testing"

	"lexa-hub/internal/buildinfo"
)

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestMDNSAdvertiser_TXT_ClaimedFlip pins the TXT record construction table:
// claimed=0/1 must track a.claimed, independent of everything else.
func TestMDNSAdvertiser_TXT_ClaimedFlip(t *testing.T) {
	a := &mdnsAdvertiser{serial: "SN123", port: 9100, tlsOn: true}

	a.claimed.Store(false)
	got := a.txt()
	want := []string{"serial=SN123", "fw=dev", "claimed=0", "api=https", "contract=1"}
	if !equalStringSlices(got, want) {
		t.Fatalf("unclaimed TXT = %v, want %v", got, want)
	}

	a.claimed.Store(true)
	got = a.txt()
	want = []string{"serial=SN123", "fw=dev", "claimed=1", "api=https", "contract=1"}
	if !equalStringSlices(got, want) {
		t.Fatalf("claimed TXT = %v, want %v", got, want)
	}
}

func TestMDNSAdvertiser_TXT_APISchemeFollowsTLS(t *testing.T) {
	cases := []struct {
		tlsOn bool
		want  string
	}{
		{true, "https"},
		{false, "http"},
	}
	for _, c := range cases {
		a := &mdnsAdvertiser{serial: "SN1", port: 9100, tlsOn: c.tlsOn}
		got := a.txt()
		want := []string{"serial=SN1", "fw=dev", "claimed=0", "api=" + c.want, "contract=1"}
		if !equalStringSlices(got, want) {
			t.Errorf("tlsOn=%v: TXT = %v, want %v", c.tlsOn, got, want)
		}
	}
}

// TestMDNSAdvertiser_TXT_FWReflectsBuildinfoVersion pins that the TXT
// record's "fw=" value is a live read of internal/buildinfo.Version
// (GAP-5), not the old hardcoded placeholder — so a real -ldflags -X build
// stamp reaches mDNS discovery too.
func TestMDNSAdvertiser_TXT_FWReflectsBuildinfoVersion(t *testing.T) {
	orig := buildinfo.Version
	defer func() { buildinfo.Version = orig }()
	buildinfo.Version = "1.2.3-test"

	a := &mdnsAdvertiser{serial: "SN123", port: 9100, tlsOn: true}
	got := a.txt()
	want := []string{"serial=SN123", "fw=1.2.3-test", "claimed=0", "api=https", "contract=1"}
	if !equalStringSlices(got, want) {
		t.Fatalf("TXT = %v, want %v", got, want)
	}
}

// TestPortOf covers the ListenAddr forms Config.ListenAddr actually takes.
func TestPortOf(t *testing.T) {
	cases := []struct {
		addr    string
		want    int
		wantErr bool
	}{
		{":9100", 9100, false},
		{"0.0.0.0:9100", 9100, false},
		{"127.0.0.1:9100", 9100, false},
		{"69.0.0.1:9100", 9100, false},
		{"bogus", 0, true},
		{"127.0.0.1:", 0, true},
		{"127.0.0.1:notaport", 0, true},
		{"127.0.0.1:0", 0, true},
		{"127.0.0.1:99999", 0, true},
	}
	for _, c := range cases {
		got, err := portOf(c.addr)
		if c.wantErr {
			if err == nil {
				t.Errorf("portOf(%q): want error, got port %d", c.addr, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("portOf(%q): unexpected error %v", c.addr, err)
			continue
		}
		if got != c.want {
			t.Errorf("portOf(%q) = %d, want %d", c.addr, got, c.want)
		}
	}
}

// TestIsCommissioned pins the presence-check semantics against a
// test-local marker path (commissionedMarkerPath is a var for exactly this
// reason — see mdns.go).
func TestIsCommissioned(t *testing.T) {
	orig := commissionedMarkerPath
	defer func() { commissionedMarkerPath = orig }()

	commissionedMarkerPath = filepath.Join(t.TempDir(), "commissioned")
	if isCommissioned() {
		t.Fatal("isCommissioned() = true before the marker file exists")
	}
	if err := os.WriteFile(commissionedMarkerPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !isCommissioned() {
		t.Fatal("isCommissioned() = false after the marker file was created")
	}
}

// TestStartMDNS_BadListenAddrDisablesNonFatally pins that a malformed
// listen_addr degrades to "mDNS disabled" (nil advertiser) rather than
// panicking or erroring the caller — startMDNS has no error return by
// design (DEVICE_ROADMAP.md §4.4: mDNS failure is always non-fatal).
func TestStartMDNS_BadListenAddrDisablesNonFatally(t *testing.T) {
	if a := startMDNS("SN1", "not-a-valid-addr", true); a != nil {
		t.Fatalf("startMDNS with malformed listen_addr = %+v, want nil", a)
	}
}

// TestMDNSAdvertiser_NilReceiverMethodsAreNoOps pins that every method on
// *mdnsAdvertiser tolerates a nil receiver — main.go relies on this to call
// refreshLoop/Shutdown unconditionally rather than needing an "if mdnsAdv
// != nil" guard at every call site.
func TestMDNSAdvertiser_NilReceiverMethodsAreNoOps(t *testing.T) {
	var a *mdnsAdvertiser
	a.Shutdown() // must not panic

	stop := make(chan struct{})
	close(stop)
	a.refreshLoop(stop) // must return immediately, not panic
}
