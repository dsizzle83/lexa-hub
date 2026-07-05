// Package httpwire is the CGo-free parsing core of the hand-rolled HTTP/1.1
// response reader used by internal/tlsclient. It imports nothing beyond the
// standard library so it can be fuzzed on any machine — no wolfSSL sysroot,
// no cgo — unlike internal/tlsclient itself, which links wolfSSL and can
// only be built/tested where that sysroot is available (TASK-047, D9/§10.2).
//
// ReadHTTPResponse is the same algorithm as the former
// (*tlsclient.Client).readResponse: header-loop until "\r\n\r\n", then
// either exactly Content-Length body bytes or read-until-close, capped at
// maxBody (the pre-existing maxResponseBody cap in tlsclient/client.go,
// now caller-supplied) — plus one new behavior, the maxHeader cap: a
// server that streams bytes without ever sending the header terminator no
// longer grows the buffer without bound (TASK-047, D9/§10.2 — "header
// floods ... fuzz it or replace it"). For every response a real CSIP
// server sends, this is unobservable: gridsim's headers run well under
// 1 KiB, verified against the fuzz corpus in testdata/fuzz/.
//
// The parser rejects (does not decode) chunked Transfer-Encoding — see
// isChunkedEncoding. Implementing chunked decoding is a separate decision
// (AD-009, deferred to TASK-069); adding it here would only grow the
// attack surface this task exists to cap.
package httpwire

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// headerTerminator marks the end of the HTTP header block.
var headerTerminator = []byte("\r\n\r\n")

// ReadHTTPResponse reads one complete HTTP/1.1 response by repeatedly
// calling read to pull more bytes from an already-open connection. read has
// the same contract as io.Reader.Read: it returns the number of bytes
// placed in p (0 <= n <= len(p)) and a non-nil error on EOF/failure; a
// short read (n < len(p), err == nil) is valid and must be handled by
// calling read again.
//
// Phase 1 buffers bytes until it finds the "\r\n\r\n" header terminator. If
// the header block grows past maxHeader bytes before a terminator is
// found, ReadHTTPResponse fails closed with an error rather than buffering
// forever. Because the terminator search runs on the whole accumulated
// buffer after every read, a header block that legitimately arrives with
// trailing body bytes attached in the same read is not penalized: the cap
// check only fires when no terminator has been found yet.
//
// Phase 2 rejects chunked Transfer-Encoding (detection only, see
// isChunkedEncoding) and otherwise reads exactly Content-Length body bytes,
// or — when Content-Length is absent, negative, or unparsable — reads until
// read reports an error or n == 0 (connection close), capped at maxBody
// bytes total. A parsed Content-Length greater than maxBody is rejected
// without attempting the read.
//
// On success the returned slice is the exact response bytes consumed:
// header block through the Content-Length body (trailing bytes beyond
// headerEnd+contentLength are trimmed), or the full buffer in the
// read-until-close case. Callers (tlsclient/fetcher.go, response.go) parse
// this blob directly, so the successful-path bytes must stay identical to
// the pre-extraction implementation.
func ReadHTTPResponse(read func([]byte) (int, error), maxHeader, maxBody int) ([]byte, error) {
	scratch := make([]byte, 4096)
	var buf []byte
	headerEnd := -1

	// Phase 1: buffer until the header block is complete.
	for headerEnd < 0 {
		n, err := read(scratch)
		if n > 0 {
			buf = append(buf, scratch[:n]...)
			if idx := bytes.Index(buf, headerTerminator); idx >= 0 {
				headerEnd = idx + len(headerTerminator)
			} else if len(buf) > maxHeader {
				return nil, fmt.Errorf("response header block too large: exceeded %d bytes", maxHeader)
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
	headers := buf[:headerEnd]
	if isChunkedEncoding(headers) {
		return nil, fmt.Errorf("Transfer-Encoding: chunked is not supported")
	}
	cl := responseContentLength(headers)
	if cl < 0 {
		// No Content-Length (absent, negative, or unparsable — all treated
		// alike): read until the server closes, capped at maxBody.
		for len(buf) <= maxBody {
			n, err := read(scratch)
			if n > 0 {
				buf = append(buf, scratch[:n]...)
			}
			if err != nil || n == 0 {
				break
			}
		}
		if len(buf) > maxBody {
			return nil, fmt.Errorf("response body too large: exceeded %d bytes (no Content-Length)", maxBody)
		}
		return buf, nil
	}

	if cl > maxBody {
		return nil, fmt.Errorf("response body too large: %d bytes (max %d)", cl, maxBody)
	}

	need := headerEnd + cl
	for len(buf) < need {
		n, err := read(scratch)
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

// isChunkedEncoding reports whether the raw HTTP header block contains
// Transfer-Encoding: chunked (case-insensitive). This parser does not
// implement chunked decoding, so callers must reject such responses
// (AD-009 defers the shim-vs-harden decision to TASK-069).
func isChunkedEncoding(headers []byte) bool {
	lower := bytes.ToLower(headers)
	for _, line := range bytes.Split(lower, []byte("\r\n")) {
		if bytes.HasPrefix(line, []byte("transfer-encoding:")) {
			val := bytes.TrimSpace(line[len("transfer-encoding:"):])
			if bytes.Equal(val, []byte("chunked")) {
				return true
			}
		}
	}
	return false
}

// responseContentLength returns the Content-Length value from a raw HTTP
// header block (the bytes up to and including \r\n\r\n). Returns -1 if the
// header is absent or cannot be parsed as an integer.
//
// A negative Content-Length parses successfully as an integer but is
// treated as absent by ReadHTTPResponse (cl < 0 falls into the
// read-until-close path) — this is deliberate, not a gap: this client talks
// to one pinned CSIP server, not the open web, so failing safe to
// read-until-close (still capped at maxBody) is preferable to guessing at
// a negative length's intent.
//
// If a response contains more than one Content-Length header (which real
// HTTP servers should never send, and gridsim never does), the first
// parseable value wins — the loop returns as soon as it finds one. This is
// documented behavior, not a decision left open: a client dedicated to a
// single pinned server does not need duplicate-header reconciliation, and
// ReadHTTPResponse's body-length cap bounds the consequence of trusting the
// wrong one either way.
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
