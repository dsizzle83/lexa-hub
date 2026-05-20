// Package identity implements IEEE 2030.5 device identifier derivation.
//
// Every CSIP client device has two identifiers derived from its X.509
// client certificate: the LFDI (Long-Form Device Identifier) and the
// SFDI (Short-Form Device Identifier). Both are deterministic functions
// of the certificate, defined in IEEE 2030.5-2018 section 6.3.4.
//
// The LFDI is the leftmost 160 bits of SHA-256 over the certificate's
// DER encoding. The SFDI is the leftmost 36 bits of the LFDI interpreted
// as a decimal value, with a sum-of-digits checksum digit appended so
// that the total digit sum is divisible by 10.
//
// Utilities use the LFDI to recognize a device across reconnections.
// The SFDI is short enough to be entered by a human installer during
// commissioning (12 decimal digits at most).
package identity

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"strings"
)

// LFDI is the Long-Form Device Identifier: the leftmost 160 bits of the
// SHA-256 hash of a device's X.509 certificate in DER form.
type LFDI [20]byte

// String returns the LFDI as an uppercase hex string with no separators.
// Example: "3E4F45AB31EDFE5B67E343E5F1A8E8DCB89AC56D".
func (l LFDI) String() string {
	return strings.ToUpper(hex.EncodeToString(l[:]))
}

// SFDI is the Short-Form Device Identifier: the leftmost 36 bits of the
// LFDI as a decimal value, with a sum-of-digits checksum digit appended.
// At most 12 decimal digits.
type SFDI uint64

// String returns the SFDI as a decimal string.
func (s SFDI) String() string {
	return fmt.Sprintf("%d", uint64(s))
}

// FromCertificate derives the LFDI and SFDI from a parsed X.509
// certificate. The certificate's DER encoding is what gets hashed.
func FromCertificate(cert *x509.Certificate) (LFDI, SFDI) {
	return FromCertificateDER(cert.Raw)
}

// FromCertificateDER derives the LFDI and SFDI from raw DER-encoded
// certificate bytes. Use this when you already have the DER bytes and
// don't need to parse the cert into a struct.
func FromCertificateDER(der []byte) (LFDI, SFDI) {
	sum := sha256.Sum256(der)
	var lfdi LFDI
	copy(lfdi[:], sum[:20])
	return lfdi, sfdiFromLFDI(lfdi)
}

// sfdiFromLFDI extracts the leftmost 36 bits of the LFDI, treats them as
// an unsigned integer, and appends a sum-of-digits checksum digit.
//
// The first 5 bytes of the LFDI hold the leftmost 40 bits. We want only
// the leftmost 36 of those, so we drop the bottom 4 bits of byte 4.
func sfdiFromLFDI(lfdi LFDI) SFDI {
	top := uint64(lfdi[0])<<28 |
		uint64(lfdi[1])<<20 |
		uint64(lfdi[2])<<12 |
		uint64(lfdi[3])<<4 |
		uint64(lfdi[4])>>4

	check := checksum(top)
	return SFDI(top*10 + uint64(check))
}

// checksum returns the digit (0-9) that, when appended to n, makes the
// total digit sum of the resulting number divisible by 10.
func checksum(n uint64) int {
	sum := 0
	for x := n; x > 0; x /= 10 {
		sum += int(x % 10)
	}
	return (10 - sum%10) % 10
}
