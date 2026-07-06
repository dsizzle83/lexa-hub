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
// The parser decodes chunked Transfer-Encoding (readChunkedBody): AD-009
// (TASK-069) resolved the shim-vs-harden question in favor of keeping this
// hardened, fuzz-clean parser and closing the one remaining functional gap —
// a conformant IEEE 2030.5 / HTTP/1.1 utility head-end MAY chunk dynamically
// generated resources, and the previous outright rejection failed the walk
// closed against such a server. The chunked decoder is size-capped exactly
// like the Content-Length path (maxBody bounds the decoded body,
// maxChunkLineLen bounds a single size line) and, being here in the CGo-free
// leaf, is covered by the same go-native fuzzing as the rest of the parser.
// The net.Conn-shim-under-http.Transport alternative (option (a)) was
// deferred as a P6-with-time item — it reworks the utility-facing transport
// and needs a conformance dual-run, disproportionate risk under the V1.0
// deadline now that the parser is fuzz-clean and capped.
package httpwire

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// maxChunkLineLen bounds a single chunk-size line (hex length plus any
// chunk extensions and, in the trailer section, a trailer header line) so a
// hostile server cannot stream unbounded bytes without ever sending the CRLF
// the chunked framing requires. Real CSIP chunk-size lines are a handful of
// hex digits; 4 KiB is generous headroom while staying bounded.
const maxChunkLineLen = 4096

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
// Phase 2 handles the body. If the headers declare chunked
// Transfer-Encoding (see isChunkedEncoding) the chunk framing is decoded
// (readChunkedBody) and the returned blob is the header block followed by
// the DECODED body — no Content-Length header is synthesized, so downstream
// re-parsing sees content-length -1 and simply uses the body bytes.
// Otherwise Phase 2 reads exactly Content-Length body bytes, or — when
// Content-Length is absent, negative, or unparsable — reads until read
// reports an error or n == 0 (connection close), capped at maxBody bytes
// total. A parsed Content-Length greater than maxBody is rejected without
// attempting the read.
//
// On success the returned slice is: for the Content-Length path, the exact
// response bytes consumed (header block through the Content-Length body,
// trailing bytes beyond headerEnd+contentLength trimmed); for the
// read-until-close path, the full buffer; for the chunked path, the header
// block plus the reassembled (de-chunked) body. Callers (tlsclient/
// fetcher.go, response.go) parse this blob directly, so the non-chunked
// successful-path bytes stay identical to the pre-extraction implementation.
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
		body, err := readChunkedBody(buf[headerEnd:], read, maxBody)
		if err != nil {
			return nil, err
		}
		out := make([]byte, 0, headerEnd+len(body))
		out = append(out, buf[:headerEnd]...)
		out = append(out, body...)
		return out, nil
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

// readChunkedBody decodes an HTTP/1.1 chunked message body (RFC 7230 §4.1).
// initial is the bytes already buffered past the header terminator; read
// pulls more from the still-open connection with the same contract as
// io.Reader.Read. It returns the reassembled body (all chunk-data
// concatenated, framing removed), consuming the stream exactly through the
// terminating zero-size chunk and the blank line that ends any trailer
// section, so the connection is left positioned for the next response.
//
// It fails closed on malformed framing (bad hex size, missing CRLF after a
// chunk, a premature connection close) and is bounded on every axis a
// hostile server controls: the decoded body may not exceed maxBody, and no
// single chunk-size/trailer line may exceed maxChunkLineLen without its
// CRLF. Chunk extensions (";name=value" after the size) and trailer header
// lines are parsed past but discarded — this client talks to one pinned
// CSIP server and has no use for either.
func readChunkedBody(initial []byte, read func([]byte) (int, error), maxBody int) ([]byte, error) {
	buf := append([]byte(nil), initial...)
	scratch := make([]byte, 4096)
	pos := 0
	var out []byte

	// fill appends one more read into buf; returns false when the stream
	// ends (error or n == 0), matching ReadHTTPResponse's read contract.
	fill := func() bool {
		n, err := read(scratch)
		if n > 0 {
			buf = append(buf, scratch[:n]...)
		}
		return err == nil && n > 0
	}

	// nextLine returns the index of the '\r' of the next CRLF at or after
	// pos, reading more bytes as needed. It errors if the unconsumed span
	// grows past maxChunkLineLen without a CRLF, or the stream ends first.
	nextLine := func() (int, error) {
		for {
			if idx := bytes.Index(buf[pos:], []byte("\r\n")); idx >= 0 {
				return pos + idx, nil
			}
			if len(buf)-pos > maxChunkLineLen {
				return 0, fmt.Errorf("chunked: line exceeds %d bytes without CRLF", maxChunkLineLen)
			}
			if !fill() {
				return 0, fmt.Errorf("chunked: connection closed mid-line")
			}
		}
	}

	for {
		crlf, err := nextLine()
		if err != nil {
			return nil, err
		}
		sizeField := buf[pos:crlf]
		if semi := bytes.IndexByte(sizeField, ';'); semi >= 0 { // strip chunk extensions
			sizeField = sizeField[:semi]
		}
		sizeField = bytes.TrimSpace(sizeField)
		size, perr := strconv.ParseInt(string(sizeField), 16, 64)
		if perr != nil || size < 0 {
			return nil, fmt.Errorf("chunked: invalid chunk size %q", buf[pos:crlf])
		}
		pos = crlf + 2 // consume the size line's CRLF

		if size == 0 {
			// Last chunk: consume optional trailer header lines up to and
			// including the blank line that terminates the message.
			for {
				tEnd, err := nextLine()
				if err != nil {
					return nil, err
				}
				lineLen := tEnd - pos
				pos = tEnd + 2
				if lineLen == 0 {
					return out, nil
				}
			}
		}

		if size > int64(maxBody) || len(out)+int(size) > maxBody {
			return nil, fmt.Errorf("chunked: body too large: exceeded %d bytes", maxBody)
		}

		// Need size data bytes plus the trailing CRLF.
		for int64(len(buf)-pos) < size+2 {
			if !fill() {
				return nil, fmt.Errorf("chunked: connection closed mid-chunk")
			}
		}
		out = append(out, buf[pos:pos+int(size)]...)
		pos += int(size)
		if buf[pos] != '\r' || buf[pos+1] != '\n' {
			return nil, fmt.Errorf("chunked: missing CRLF after chunk data")
		}
		pos += 2
	}
}

// isChunkedEncoding reports whether the raw HTTP header block declares
// Transfer-Encoding: chunked (case-insensitive, bare "chunked" value).
// ReadHTTPResponse decodes such responses via readChunkedBody (AD-009,
// TASK-069). Detection stays deliberately narrow — a bare "chunked" value —
// because this client talks to one pinned CSIP server; a stacked coding like
// "gzip, chunked" is neither sent by that server nor something this client
// decompresses, so it correctly falls through to the length/close path.
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
