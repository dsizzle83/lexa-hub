package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// TestSFDIFromKnownLFDI verifies SFDI derivation against a worked example.
//
// Cross-reference this against your SunSpec CSIP Implementation Guide
// (look for the section on device identifier derivation, typically near
// the discussion of certificate provisioning). The math below should
// match the spec's worked example. If your document gives different
// numbers, tell me and we'll align.
//
// Worked example:
//   LFDI hex:        3E4F45AB31EDFE5B67E343E5F1A8E8DCB89AC56D
//   First 36 bits:   0x3E4F45AB3
//   As decimal:      16726121139
//   Digit sum:       1+6+7+2+6+1+2+1+1+3+9 = 39
//   Check digit:     (10 - 39%10) % 10 = 1
//   SFDI:            167261211391

func TestSFDIFromKnownLFDI(t *testing.T) {
	const lfdiHex = "3E4F45AB31EDFE5B67E343E5F1A8E8DCB89AC56D"
	const wantSFDI SFDI = 167261211391

	raw, err := hex.DecodeString(lfdiHex)
	if err != nil {
		t.Fatalf("decode LFDI hex: %v", err)
	}
	var lfdi LFDI
	copy(lfdi[:], raw)

	got := sfdiFromLFDI(lfdi)
	if got != wantSFDI {
		t.Errorf("sfdiFromLFDI() = %d, want %d", got, wantSFDI)
	}
}

// TestLFDIFromKnownDER verifies the LFDI is the leftmost 160 bits of
// SHA-256 over the certificate DER bytes. We use a fixed input so the
// expected hash can be verified by hand or with an external tool.
func TestLFDIFromKnownDER(t *testing.T) {
	input := []byte("test certificate DER bytes")
	sum := sha256.Sum256(input)
	wantLFDI := strings.ToUpper(hex.EncodeToString(sum[:20]))

	lfdi, _ := FromCertificateDER(input)
	if lfdi.String() != wantLFDI {
		t.Errorf("LFDI = %s, want %s", lfdi.String(), wantLFDI)
	}
}

// TestSFDIChecksumProperty exercises the defining invariant of the
// sum-of-digits checksum: for any valid SFDI, the sum of all its
// decimal digits is divisible by 10.
func TestSFDIChecksumProperty(t *testing.T) {
	cases := []uint64{
		0x3E4F45AB3,
		0x000000001,
		0xFFFFFFFFF,
		0x123456789,
		0xABCDEF012,
	}
	for _, top := range cases {
		var lfdi LFDI
		// Pack `top` into the leftmost 36 bits of the LFDI
		lfdi[0] = byte(top >> 28)
		lfdi[1] = byte(top >> 20)
		lfdi[2] = byte(top >> 12)
		lfdi[3] = byte(top >> 4)
		lfdi[4] = byte(top << 4)

		sfdi := sfdiFromLFDI(lfdi)
		if !digitsSumToMultipleOfTen(uint64(sfdi)) {
			t.Errorf("SFDI %d (from top=0x%X) digits do not sum to a multiple of 10", sfdi, top)
		}
	}
}

func digitsSumToMultipleOfTen(n uint64) bool {
	sum := 0
	for x := n; x > 0; x /= 10 {
		sum += int(x % 10)
	}
	return sum%10 == 0
}

// TestGeneratedCertProducesValidIdentifiers does a round-trip: generate
// a cert, derive identifiers, and confirm they have the expected shape
// and are deterministic across repeated calls.
func TestGeneratedCertProducesValidIdentifiers(t *testing.T) {
	cert, _, err := GenerateTestCertificate("test-device")
	println(cert)
	if err != nil {
		t.Fatalf("GenerateTestCertificate: %v", err)
	}

	lfdi, sfdi := FromCertificate(cert)

	if len(lfdi.String()) != 40 {
		t.Errorf("LFDI string length = %d, want 40", len(lfdi.String()))
	}
	if !digitsSumToMultipleOfTen(uint64(sfdi)) {
		t.Errorf("SFDI checksum invalid for generated cert: %d", sfdi)
	}

	// Determinism: same cert in, same identifiers out.
	lfdi2, sfdi2 := FromCertificate(cert)
	if lfdi != lfdi2 || sfdi != sfdi2 {
		t.Error("identifier derivation is not deterministic")
	}
}

// TestParsedAndRawDERAgree ensures that deriving identifiers from a
// parsed *x509.Certificate gives the same result as deriving from the
// raw DER bytes directly. The grid sim computes LFDIs from parsed
// certs it gets off TLS connection state, while file-loading code path
// works from raw bytes — both paths must agree or authentication will
// mysteriously fail in one direction.
func TestParsedAndRawDERAgree(t *testing.T) {
	cert, _, err := GenerateTestCertificate("roundtrip-test")
	if err != nil {
		t.Fatalf("GenerateTestCertificate: %v", err)
	}

	lfdiFromParsed, sfdiFromParsed := FromCertificate(cert)
	lfdiFromRaw, sfdiFromRaw := FromCertificateDER(cert.Raw)

	if lfdiFromParsed != lfdiFromRaw {
		t.Errorf("LFDI mismatch: parsed=%s raw=%s", lfdiFromParsed, lfdiFromRaw)
	}
	if sfdiFromParsed != sfdiFromRaw {
		t.Errorf("SFDI mismatch: parsed=%d raw=%d", sfdiFromParsed, sfdiFromRaw)
	}
}
