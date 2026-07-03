package tlsclient

import "time"

// DefaultCipherList is the IEEE 2030.5 / CSIP §5.2.1.1 mandated cipher
// suite. Production code should always use this constant.
const DefaultCipherList = "ECDHE-ECDSA-AES128-CCM-8"

// DefaultDialTimeout is used when Config.DialTimeout is zero.
const DefaultDialTimeout = 30 * time.Second

// DefaultReadTimeout is used when Config.ReadTimeout is zero. It bounds each
// socket read during a response, so a wedged server (accepts, then never
// answers) surfaces as a request error instead of blocking the caller's
// goroutine forever — for the northbound that would silently stall every
// future discovery walk while the last-published control ages (QA 2026-07-02:
// northbound-hang). Per-read, not per-response: a slow-but-flowing large
// resource never trips it.
const DefaultReadTimeout = 15 * time.Second

// Config holds everything needed to construct a Client.
type Config struct {
	// ServerAddr is the host:port to connect to.
	ServerAddr string

	// CACertPath is the PEM-encoded CA cert used to verify the server's
	// certificate during the mTLS handshake.
	CACertPath string

	// ClientCertPath is the PEM-encoded client (DER device) cert
	// presented to the server during mTLS.
	ClientCertPath string

	// ClientKeyPath is the PEM-encoded private key matching ClientCertPath.
	ClientKeyPath string

	// CipherList is the wolfSSL cipher list. Empty means DefaultCipherList.
	// Override only for negative testing — production code should leave
	// this empty to enforce CSIP compliance.
	CipherList string

	// DialTimeout is the timeout for TCP connection setup. Zero uses
	// DefaultDialTimeout (30 s). This bounds the TCP dial and wolfSSL
	// handshake.
	DialTimeout time.Duration

	// ReadTimeout bounds each socket read (and write) on the established
	// session, enforced with SO_RCVTIMEO/SO_SNDTIMEO on the socket wolfSSL
	// owns — Go net.Conn deadlines cannot reach wolfSSL's blocking read(2) on
	// the dup'ed fd. Zero uses DefaultReadTimeout; negative disables (reads
	// may block forever — test/debug only). On timeout the request fails and
	// the fetcher discards the session and re-dials.
	ReadTimeout time.Duration
}
