package main

// tlscert.go implements DEVICE_ROADMAP.md §4.1's per-device HTTPS server
// certificate for lexa-api: a self-signed ECDSA P-256 leaf, generated once
// on first boot and persisted under Config.CertDir so restarts (and
// firmware updates) don't churn the fingerprint a TOFU-pinning consumer
// (installer label, lexactl, the mobile app) has already recorded.
//
// Why self-signed + fingerprint pinning instead of a real CA chain: this is
// a LOCAL-LAN API (the product default binds loopback; the bench binds LAN
// only on an air-gapped 69.0.0.x network) with no public hostname to get a
// CA-issued cert for. The northbound wolfSSL stack's CA-issued CSIP identity
// is a completely separate concern (utility-facing, internet-routable in
// principle) and is untouched by this file.
import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	certFileName = "cert.pem"
	keyFileName  = "key.pem"

	// defaultSerialFile is Config.SerialFile's default (config.go) — the
	// device-identity serial used for both the cert's CN/SAN here and the
	// mDNS TXT record (mdns.go).
	defaultSerialFile = "/etc/lexa/identity/serial"

	// certValidity is the self-signed leaf's lifetime. Ten years: there is
	// no rotation mechanism for this LOCAL-API cert (unlike the CSIP
	// client cert's TASK-073 Reload seam) — regenerating it would churn
	// every TOFU pin for no security benefit, so it is sized to outlive
	// the device rather than to be rotated.
	certValidity = 10 * 365 * 24 * time.Hour

	// certClockSkewSlack backdates NotBefore so a box whose clock hasn't
	// synced yet (§9's clock-trust concerns) doesn't mint a
	// not-yet-valid certificate.
	certClockSkewSlack = 1 * time.Hour
)

// ensureServerCert loads dir/cert.pem + dir/key.pem, generating them on
// first boot (resolving the device serial from defaultSerialFile, with a
// hostname fallback — see resolveSerial) if either file is missing. It
// returns the loaded tls.Certificate plus the lowercase-hex SHA-256
// fingerprint of the leaf's DER bytes (see the fingerprint-format note
// below), used for the startup log line and /status's "api_cert_fp" field.
//
// This is the entry point the stability/permission tests call directly, and
// matches the resolution main() falls back to when Config.SerialFile is
// unset. main() itself calls ensureServerCertFor with Config.SerialFile so
// the config key's override actually takes effect — see that function's
// doc for why the two-argument form exists alongside this one.
//
// Deterministic across restarts: once cert.pem/key.pem exist on disk, every
// subsequent call (from this process or a future one) loads them unchanged
// — the fingerprint is stable for as long as the files are.
func ensureServerCert(dir string) (tls.Certificate, string, error) {
	return ensureServerCertFor(dir, defaultSerialFile)
}

// ensureServerCertFor is ensureServerCert with an explicit serial-file path
// — the seam Config.SerialFile (config.go) feeds through from main(), so a
// non-default "serial_file" config key actually changes the generated
// cert's CN/SAN. (The task's literal ensureServerCert(dir) signature has no
// room for this parameter; splitting it out keeps that exact signature
// available — and stable for the determinism/permission tests — while
// still letting the config key do something.)
//
// SAN handling: SANs are computed ONCE, at generation time, from
// os.Hostname(), "<serial>.local", and the box's current non-loopback
// unicast IPs (best-effort — a lookup failure just yields fewer SANs, never
// an error). If the box's IP changes later (DHCP lease renewal, a new NIC)
// the certificate is NOT regenerated: pinning is by FINGERPRINT, not by
// hostname/IP chain verification, so SAN drift costs nothing and
// regenerating on every IP change would invalidate every existing TOFU pin
// for no security benefit.
func ensureServerCertFor(dir, serialFile string) (tls.Certificate, string, error) {
	if dir == "" {
		return tls.Certificate{}, "", errors.New("tlscert: empty cert dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return tls.Certificate{}, "", fmt.Errorf("tlscert: mkdir %s: %w", dir, err)
	}

	certPath := filepath.Join(dir, certFileName)
	keyPath := filepath.Join(dir, keyFileName)

	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	if certErr != nil || keyErr != nil {
		serial := resolveSerial(serialFile)
		var err error
		certPEM, keyPEM, err = generateSelfSignedCert(serial)
		if err != nil {
			return tls.Certificate{}, "", fmt.Errorf("tlscert: generate: %w", err)
		}
		// Key before cert, both atomic (tmp+rename, same discipline as
		// cmd/hub/snapshot.go's saveHubSnapshot): a crash between the two
		// writes leaves at most a missing cert.pem, which just re-triggers
		// generation next boot — never a mismatched key/cert pair on disk.
		if err := writeFileAtomic(keyPath, keyPEM, 0o600); err != nil {
			return tls.Certificate{}, "", fmt.Errorf("tlscert: write key: %w", err)
		}
		if err := writeFileAtomic(certPath, certPEM, 0o644); err != nil {
			return tls.Certificate{}, "", fmt.Errorf("tlscert: write cert: %w", err)
		}
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("tlscert: parse keypair in %s: %w", dir, err)
	}
	fp, err := fingerprintOf(cert)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("tlscert: fingerprint: %w", err)
	}
	return cert, fp, nil
}

// resolveSerial reads serialFile (trimmed, first non-empty line's worth of
// content) as the device identity serial; an unreadable or empty file (no
// identity partition provisioned yet — a bench box, or a factory unit ahead
// of commissioning) falls back to os.Hostname(), and a hostname lookup
// failure falls back to the fixed literal "unknown" rather than erroring —
// the certificate must still get generated even on a box with no identity
// story at all yet.
func resolveSerial(serialFile string) string {
	if serialFile == "" {
		serialFile = defaultSerialFile
	}
	if data, err := os.ReadFile(serialFile); err == nil {
		if s := strings.TrimSpace(string(data)); s != "" {
			return s
		}
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "unknown"
}

// generateSelfSignedCert builds a 10-year self-signed ECDSA P-256
// certificate for CN "lexa-<serial>", PEM-encoding both the certificate and
// its EC private key.
func generateSelfSignedCert(serial string) (certPEM, keyPEM []byte, err error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate ECDSA key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate x509 serial number: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber:          serialNumber,
		Subject:               pkix.Name{CommonName: "lexa-" + serial},
		NotBefore:             time.Now().Add(-certClockSkewSlack),
		NotAfter:              time.Now().Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		DNSNames:              sanDNSNames(serial),
		IPAddresses:           sanIPs(),
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, fmt.Errorf("create certificate: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal EC private key: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// sanDNSNames returns the DNS-form SANs: the box's hostname (best-effort;
// omitted on lookup failure) and "<serial>.local", deduplicated.
func sanDNSNames(serial string) []string {
	seen := make(map[string]bool)
	var out []string
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	if h, err := os.Hostname(); err == nil {
		add(h)
	}
	add(serial + ".local")
	return out
}

// sanIPs returns the box's current non-loopback unicast IPs, best-effort —
// an interface enumeration error yields an empty (not erroring) result,
// since a missing SAN IP is cosmetic under fingerprint-pinned TOFU (see
// ensureServerCertFor's doc).
func sanIPs() []net.IP {
	var ips []net.IP
	ifaces, err := net.Interfaces()
	if err != nil {
		return ips
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			ips = append(ips, ip)
		}
	}
	return ips
}

// fingerprintOf returns the lowercase-hex SHA-256 digest of cert's leaf DER
// bytes.
//
// Format decision: plain lowercase hex (no colon separators). This is the
// form compared programmatically (VerifyPeerCertificate callbacks, the
// /status JSON field, a future lexactl/app equality check) far more often
// than it is read character-by-character by a human, so the simplest
// representation wins; a colon-separated display form (the openssl/SSH
// convention) is a two-line formatting job for whichever caller wants it
// for display (e.g. a future `lexactl fingerprint`).
func fingerprintOf(cert tls.Certificate) (string, error) {
	if len(cert.Certificate) == 0 {
		return "", errors.New("tlscert: certificate has no leaf DER bytes")
	}
	sum := sha256.Sum256(cert.Certificate[0])
	return hex.EncodeToString(sum[:]), nil
}

// writeFileAtomic writes data to path via a same-directory ".tmp" file,
// fsync, then rename — the same discipline as cmd/hub/snapshot.go's
// saveHubSnapshot, so a reader (or a crash mid-write) never observes a
// partial cert/key file. perm is applied via an explicit Chmod after
// creation, since OpenFile's mode argument is subject to the process
// umask and callers here (specifically the 0600 key file) need the exact
// requested bits regardless of umask.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("create tmp %s: %w", tmp, err)
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("chmod tmp %s: %w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write tmp %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("fsync tmp %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close tmp %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}
