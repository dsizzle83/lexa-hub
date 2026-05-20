// Package tlsclient implements an IEEE 2030.5 / CSIP DER client over
// mTLS via wolfSSL. It is the package under active development — the
// server package exists primarily to validate this client.
package tlsclient

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"unsafe"

	"lexa-hub/internal/wolfssl"
)

const (
	maxResponseBody = 10 << 20 // 10 MiB — safe ceiling for embedded targets
)

// Client is a CSIP-compliant mTLS client. It owns a wolfSSL context
// and, after Dial, a connected SSL session.
//
// Lifecycle: New → Dial → Get... → Close.
//
// Why split New and Dial: New does the cert loading and cipher
// configuration once. Dial establishes a TCP connection and TLS
// session. In future milestones we may want to reuse the same Client
// across multiple Dials (e.g., reconnect after a server restart),
// which is why the ctx and the per-connection state are separated.
type Client struct {
	cfg Config
	ctx unsafe.Pointer

	// Per-connection state, populated by Dial, cleared by Close.
	ssl  unsafe.Pointer
	conn net.Conn
	file *os.File
}

// New constructs a Client and configures its wolfSSL context with the
// CA, client cert, and client key. Does not open a network connection;
// call Dial for that.
func New(cfg Config) (*Client, error) {
	if cfg.CipherList == "" {
		cfg.CipherList = DefaultCipherList
	}

	ctx, err := wolfssl.NewClientCtx()
	if err != nil {
		return nil, err
	}

	ok := false
	defer func() {
		if !ok {
			wolfssl.FreeCtx(ctx)
		}
	}()

	if err := wolfssl.SetCipherList(ctx, cfg.CipherList); err != nil {
		return nil, fmt.Errorf("set cipher list: %w", err)
	}
	if err := wolfssl.LoadVerifyLocations(ctx, cfg.CACertPath); err != nil {
		return nil, fmt.Errorf("load CA cert: %w", err)
	}
	if err := wolfssl.UseCertFile(ctx, cfg.ClientCertPath); err != nil {
		return nil, fmt.Errorf("load client cert: %w", err)
	}
	if err := wolfssl.UseKeyFile(ctx, cfg.ClientKeyPath); err != nil {
		return nil, fmt.Errorf("load client key: %w", err)
	}

	ok = true
	return &Client{cfg: cfg, ctx: ctx}, nil
}

// Dial opens a TCP connection to the configured server and performs
// the mTLS handshake. After Dial returns successfully, the Client
// holds an open TLS session ready for Get/Post requests.
//
// On error, all partially-allocated resources are released — the
// Client remains usable for a retry Dial without needing reconstruction.
func (c *Client) Dial() error {
	if c.ssl != nil {
		return errors.New("client already connected; call Close first")
	}

	timeout := c.cfg.DialTimeout
	if timeout <= 0 {
		timeout = DefaultDialTimeout
	}
	conn, err := net.DialTimeout("tcp", c.cfg.ServerAddr, timeout)
	if err != nil {
		return fmt.Errorf("tcp dial %s: %w", c.cfg.ServerAddr, err)
	}

	ok := false
	defer func() {
		if !ok {
			conn.Close()
		}
	}()

	tcpConn, isTCP := conn.(*net.TCPConn)
	if !isTCP {
		return errors.New("dial returned non-TCP connection")
	}
	file, err := tcpConn.File()
	if err != nil {
		return fmt.Errorf("get file from tcp conn: %w", err)
	}
	defer func() {
		if !ok {
			file.Close()
		}
	}()

	ssl, err := wolfssl.NewSSL(c.ctx)
	if err != nil {
		return fmt.Errorf("new SSL session: %w", err)
	}
	defer func() {
		if !ok {
			wolfssl.FreeSSL(ssl)
		}
	}()

	if err := wolfssl.SetFD(ssl, int(file.Fd())); err != nil {
		return err
	}
	if err := wolfssl.Connect(ssl); err != nil {
		return fmt.Errorf("TLS handshake: %w", err)
	}

	c.ssl = ssl
	c.conn = conn
	c.file = file
	ok = true
	return nil
}

// Close shuts down the TLS session and releases the connection. After
// Close, the Client may be reused by calling Dial again. To fully
// release the wolfSSL context as well, call Free.
func (c *Client) Close() {
	if c.ssl != nil {
		wolfssl.Shutdown(c.ssl)
		wolfssl.FreeSSL(c.ssl)
		c.ssl = nil
	}
	if c.file != nil {
		c.file.Close()
		c.file = nil
	}
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}

// Free releases the wolfSSL context. Implicitly calls Close. After
// Free, the Client may not be reused. Test cleanup typically uses
// defer client.Free.
func (c *Client) Free() {
	c.Close()
	if c.ctx != nil {
		wolfssl.FreeCtx(c.ctx)
		c.ctx = nil
	}
}

// Cipher returns the negotiated cipher suite name. Only meaningful
// after a successful Dial.
func (c *Client) Cipher() string {
	if c.ssl == nil {
		return ""
	}
	return wolfssl.CipherName(c.ssl)
}

// Version returns the negotiated TLS protocol version string. Only
// meaningful after a successful Dial.
func (c *Client) Version() string {
	if c.ssl == nil {
		return ""
	}
	return wolfssl.Version(c.ssl)
}

// Post sends an HTTP POST request with body and returns the raw response.
// Uses the same Content-Length-aware reader as Get; connection is kept
// alive after the round trip.
func (c *Client) Post(path string, body []byte, contentType string) ([]byte, error) {
	if c.ssl == nil {
		return nil, errors.New("client not connected; call Dial first")
	}
	req := buildPostRequest(path, c.cfg.ServerAddr, body, contentType)
	if _, err := wolfssl.Write(c.ssl, req); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}
	return c.readResponse()
}

// Get sends an HTTP GET request for the given path and returns the raw
// response (headers + body). Uses Content-Length to determine when the
// response is complete, leaving the TLS session open for subsequent
// requests on the same connection (persistent / keep-alive).
func (c *Client) Get(path string) ([]byte, error) {
	if c.ssl == nil {
		return nil, errors.New("client not connected; call Dial first")
	}
	req := buildGetRequest(path, c.cfg.ServerAddr)
	if _, err := wolfssl.Write(c.ssl, req); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}
	return c.readResponse()
}

// readResponse reads one complete HTTP response from the open TLS
// session. It reads headers until \r\n\r\n, then reads exactly
// Content-Length body bytes so the connection can be reused.
// Falls back to read-until-close if Content-Length is absent.
func (c *Client) readResponse() ([]byte, error) {
	scratch := make([]byte, 4096)
	var buf []byte
	headerEnd := -1

	// Phase 1: buffer until the header block is complete.
	for headerEnd < 0 {
		n, err := wolfssl.Read(c.ssl, scratch)
		if n > 0 {
			buf = append(buf, scratch[:n]...)
			if idx := bytes.Index(buf, []byte("\r\n\r\n")); idx >= 0 {
				headerEnd = idx + 4
			}
		}
		if err != nil || n == 0 {
			break
		}
	}
	if headerEnd < 0 {
		return nil, fmt.Errorf("incomplete HTTP response: no header terminator")
	}

	// Phase 2: read exactly Content-Length body bytes.
	cl := responseContentLength(buf[:headerEnd])
	if cl < 0 {
		// No Content-Length: fall back to reading until server closes.
		for {
			n, err := wolfssl.Read(c.ssl, scratch)
			if n > 0 {
				buf = append(buf, scratch[:n]...)
			}
			if err != nil || n == 0 {
				break
			}
		}
		return buf, nil
	}

	if cl > maxResponseBody {
		return nil, fmt.Errorf("response body too large: %d bytes (max %d)", cl, maxResponseBody)
	}

	need := headerEnd + cl
	for len(buf) < need {
		n, err := wolfssl.Read(c.ssl, scratch)
		if n > 0 {
			buf = append(buf, scratch[:n]...)
		}
		if err != nil || n == 0 {
			break
		}
	}
	if len(buf) < need {
		return nil, fmt.Errorf("truncated response: got %d bytes, expected %d", len(buf), need)
	}
	return buf[:need], nil
}

// responseContentLength returns the Content-Length value from a raw
// HTTP header block (the bytes up to and including \r\n\r\n).
// Returns -1 if the header is absent or cannot be parsed.
func responseContentLength(headers []byte) int {
	lower := bytes.ToLower(headers)
	for _, line := range bytes.Split(lower, []byte("\r\n")) {
		if bytes.HasPrefix(line, []byte("content-length:")) {
			val := bytes.TrimSpace(line[len("content-length:"):])
			n, err := strconv.Atoi(strings.TrimSpace(string(val)))
			if err == nil {
				return n
			}
		}
	}
	return -1
}
