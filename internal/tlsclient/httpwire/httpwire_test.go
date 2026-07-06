package httpwire

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// prodMaxHeader/prodMaxBody mirror the constants tlsclient/client.go passes
// to ReadHTTPResponse in production (maxResponseHeader, maxResponseBody).
// Tests use the real values, not scaled-down stand-ins, so cap behavior is
// verified at the sizes that actually ship.
const (
	prodMaxHeader = 64 << 10
	prodMaxBody   = 10 << 20
)

// bytesReader returns a read func over data that behaves like a real
// blocking-socket read: each call returns up to len(p) bytes with a nil
// error, and once data is exhausted returns (0, io.EOF) — matching the
// "if err != nil || n == 0 { break }" contract ReadHTTPResponse relies on.
func bytesReader(data []byte) func([]byte) (int, error) {
	pos := 0
	return func(p []byte) (int, error) {
		if pos >= len(data) {
			return 0, io.EOF
		}
		n := copy(p, data[pos:])
		pos += n
		return n, nil
	}
}

// chunkedReader delivers data in fixed-size pieces (at most chunkSize bytes
// per call) rather than all at once, so tests exercise the incremental
// header-scan (bytes.Index re-run on the growing buffer each read) instead
// of just the final buffer contents.
func chunkedReader(data []byte, chunkSize int) func([]byte) (int, error) {
	pos := 0
	return func(p []byte) (int, error) {
		if pos >= len(data) {
			return 0, io.EOF
		}
		n := chunkSize
		if n > len(p) {
			n = len(p)
		}
		if pos+n > len(data) {
			n = len(data) - pos
		}
		copy(p, data[pos:pos+n])
		pos += n
		return n, nil
	}
}

// erroringReader returns data then a sentinel non-EOF error — exercises the
// "err != nil" break path distinctly from the "n == 0" / io.EOF path.
func erroringReader(data []byte, failErr error) func([]byte) (int, error) {
	pos := 0
	return func(p []byte) (int, error) {
		if pos >= len(data) {
			return 0, failErr
		}
		n := copy(p, data[pos:])
		pos += n
		if pos >= len(data) {
			return n, failErr
		}
		return n, nil
	}
}

func mustContain(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want containing %q", err, want)
	}
}

// === Happy path ==============================================================

func TestReadHTTPResponse_MinimalOK(t *testing.T) {
	raw := []byte("HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello")
	got, err := ReadHTTPResponse(bytesReader(raw), prodMaxHeader, prodMaxBody)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("got %q, want %q", got, raw)
	}
}

func TestReadHTTPResponse_TrailingBytesTrimmedToContentLength(t *testing.T) {
	// Extra bytes after the declared body (e.g. start of a pipelined next
	// response, or a keep-alive connection's next write already buffered)
	// must be trimmed off, not included — fetcher.go/response.go parse
	// exactly headerEnd+cl bytes.
	raw := []byte("HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhelloEXTRA-JUNK")
	want := []byte("HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello")
	got, err := ReadHTTPResponse(bytesReader(raw), prodMaxHeader, prodMaxBody)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestReadHTTPResponse_SplitAcrossReads(t *testing.T) {
	raw := []byte("HTTP/1.1 200 OK\r\nContent-Type: application/sep+xml\r\nContent-Length: 13\r\n\r\n<DCAP/>hello!")
	for _, chunk := range []int{1, 2, 3, 7, 4096} {
		t.Run(string(rune('0'+chunk%10)), func(t *testing.T) {
			got, err := ReadHTTPResponse(chunkedReader(raw, chunk), prodMaxHeader, prodMaxBody)
			if err != nil {
				t.Fatalf("chunk size %d: unexpected error: %v", chunk, err)
			}
			if !bytes.Equal(got, raw) {
				t.Errorf("chunk size %d: got %q, want %q", chunk, got, raw)
			}
		})
	}
}

func TestReadHTTPResponse_NoContentLengthReadUntilClose(t *testing.T) {
	raw := []byte("HTTP/1.1 200 OK\r\nContent-Type: application/sep+xml\r\n\r\n<DCAP/>the-rest-of-the-body")
	got, err := ReadHTTPResponse(bytesReader(raw), prodMaxHeader, prodMaxBody)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("got %q, want %q", got, raw)
	}
}

func TestReadHTTPResponse_ConnResetMidBodyStillReturnsBufferedBytes(t *testing.T) {
	// No Content-Length + a hard read error (not a clean n==0/io.EOF close):
	// the original code treats "err != nil" the same as "n == 0" in the
	// read-until-close loop — whatever was buffered is returned, not
	// propagated as an error. Preserve that (callers see truncated XML and
	// fail at the unmarshal step, which is the existing error-classification
	// contract wan-outage-hold/northbound-hang depend on).
	raw := []byte("HTTP/1.1 200 OK\r\n\r\npartial-body")
	got, err := ReadHTTPResponse(erroringReader(raw, errors.New("connection reset")), prodMaxHeader, prodMaxBody)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("got %q, want %q", got, raw)
	}
}

// === Header cap (TASK-047 new behavior) =====================================

func TestReadHTTPResponse_HeaderFloodRejected(t *testing.T) {
	// 1 MiB of header junk, no terminator ever sent.
	var b bytes.Buffer
	for b.Len() < 1<<20 {
		b.WriteString("X-Junk: filler-value-to-pad-out-the-header-block\r\n")
	}
	_, err := ReadHTTPResponse(bytesReader(b.Bytes()), prodMaxHeader, prodMaxBody)
	mustContain(t, err, "header block too large")
}

func TestReadHTTPResponse_HeaderJustUnderCapParses(t *testing.T) {
	// Build a header block whose total size (through \r\n\r\n) is exactly
	// prodMaxHeader-1 bytes, followed by a zero-length body. Must parse
	// without tripping the cap.
	status := "HTTP/1.1 200 OK\r\n"
	terminator := "\r\n"
	padHeaderName := "X-Pad: "
	padLineSuffix := "\r\n"
	fixed := len(status) + len(terminator)
	target := prodMaxHeader - 1
	padBodyLen := target - fixed - len(padHeaderName) - len(padLineSuffix)
	if padBodyLen < 0 {
		t.Fatalf("prodMaxHeader too small for this fixture")
	}
	raw := status + padHeaderName + strings.Repeat("a", padBodyLen) + padLineSuffix + terminator
	if len(raw) != target {
		t.Fatalf("fixture construction bug: len(raw)=%d, want %d", len(raw), target)
	}

	got, err := ReadHTTPResponse(bytesReader([]byte(raw)), prodMaxHeader, prodMaxBody)
	if err != nil {
		t.Fatalf("64KiB-1 header block should parse, got error: %v", err)
	}
	if string(got) != raw {
		t.Errorf("got %q, want %q", got, raw)
	}
}

func TestReadHTTPResponse_HeaderJustOverCapRejected(t *testing.T) {
	// Same shape as the -1 test but one byte over the cap and, critically,
	// no terminator anywhere in that first 64KiB+1 span — so the cap must
	// fire before a terminator could ever be found.
	var b bytes.Buffer
	b.WriteString("HTTP/1.1 200 OK\r\n")
	for b.Len() <= prodMaxHeader {
		b.WriteString("X-Pad: filler\r\n")
	}
	_, err := ReadHTTPResponse(bytesReader(b.Bytes()), prodMaxHeader, prodMaxBody)
	mustContain(t, err, "header block too large")
}

func TestReadHTTPResponse_HeaderCapDoesNotPenalizeAttachedBody(t *testing.T) {
	// Headers arrive in the same read as a large body already attached
	// (common when a server flushes headers+body in one write and the
	// client's read granularity happens to catch it all at once). The cap
	// must apply to the header block, not the combined buffer, because the
	// terminator is found before the cap check ever runs.
	headers := "HTTP/1.1 200 OK\r\nContent-Length: 200000\r\n\r\n"
	body := strings.Repeat("b", 200000)
	raw := headers + body
	got, err := ReadHTTPResponse(bytesReader([]byte(raw)), prodMaxHeader, prodMaxBody)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != raw {
		t.Errorf("got len %d, want len %d", len(got), len(raw))
	}
}

// === Content-Length hardening ================================================

func TestReadHTTPResponse_NegativeContentLengthTreatedAsAbsent(t *testing.T) {
	raw := []byte("HTTP/1.1 200 OK\r\nContent-Length: -5\r\n\r\nbody-until-close")
	got, err := ReadHTTPResponse(bytesReader(raw), prodMaxHeader, prodMaxBody)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("got %q, want %q (negative CL should fall back to read-until-close)", got, raw)
	}
}

func TestReadHTTPResponse_NonIntegerContentLengthTreatedAsAbsent(t *testing.T) {
	raw := []byte("HTTP/1.1 200 OK\r\nContent-Length: not-a-number\r\n\r\nbody-until-close")
	got, err := ReadHTTPResponse(bytesReader(raw), prodMaxHeader, prodMaxBody)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("got %q, want %q", got, raw)
	}
}

func TestReadHTTPResponse_OversizedContentLengthRejected(t *testing.T) {
	raw := []byte("HTTP/1.1 200 OK\r\nContent-Length: 999999999\r\n\r\nshort")
	_, err := ReadHTTPResponse(bytesReader(raw), prodMaxHeader, prodMaxBody)
	mustContain(t, err, "too large")
}

func TestReadHTTPResponse_NoContentLengthBodyOverCapRejected(t *testing.T) {
	maxBody := 1024
	var b bytes.Buffer
	b.WriteString("HTTP/1.1 200 OK\r\n\r\n")
	for b.Len() <= maxBody {
		b.WriteString("filler-body-bytes-")
	}
	_, err := ReadHTTPResponse(bytesReader(b.Bytes()), prodMaxHeader, maxBody)
	mustContain(t, err, "too large")
}

func TestReadHTTPResponse_DuplicateContentLengthFirstWins(t *testing.T) {
	// Ambiguous duplicate Content-Length: first parseable value wins
	// (documented in responseContentLength) — a client dedicated to one
	// pinned server doesn't need duplicate-header reconciliation.
	raw := []byte("HTTP/1.1 200 OK\r\nContent-Length: 5\r\nContent-Length: 999\r\n\r\nhelloEXTRA")
	want := []byte("HTTP/1.1 200 OK\r\nContent-Length: 5\r\nContent-Length: 999\r\n\r\nhello")
	got, err := ReadHTTPResponse(bytesReader(raw), prodMaxHeader, prodMaxBody)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("got %q, want %q (first Content-Length should win)", got, want)
	}
}

// === Chunked encoding: decoded (AD-009 → option (b), TASK-069) =============
//
// The returned blob is the header block followed by the DE-CHUNKED body (no
// synthetic Content-Length), so response.go's parseHTTPResponse takes
// body = raw[headerEnd+4:] and gets the reassembled payload directly. These
// tests assert exactly that blob shape.

// wantDechunked builds the expected ReadHTTPResponse output for a chunked
// response: the header block (through \r\n\r\n) plus the concatenated decoded
// body. headers must include the trailing \r\n\r\n.
func wantDechunked(headers, body string) []byte {
	return []byte(headers + body)
}

func TestReadHTTPResponse_ChunkedSingleChunk(t *testing.T) {
	headers := "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"
	raw := headers + "5\r\nhello\r\n0\r\n\r\n"
	got, err := ReadHTTPResponse(bytesReader([]byte(raw)), prodMaxHeader, prodMaxBody)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := wantDechunked(headers, "hello"); !bytes.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestReadHTTPResponse_ChunkedMultipleChunks(t *testing.T) {
	headers := "HTTP/1.1 200 OK\r\nContent-Type: application/sep+xml\r\nTransfer-Encoding: chunked\r\n\r\n"
	// "<DCAP" + "/>" + "!!!" split across three chunks, hex sizes.
	raw := headers + "5\r\n<DCAP\r\n2\r\n/>\r\n3\r\n!!!\r\n0\r\n\r\n"
	got, err := ReadHTTPResponse(bytesReader([]byte(raw)), prodMaxHeader, prodMaxBody)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := wantDechunked(headers, "<DCAP/>!!!"); !bytes.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestReadHTTPResponse_ChunkedCaseInsensitiveHeader(t *testing.T) {
	headers := "HTTP/1.1 200 OK\r\ntransfer-encoding: chunked\r\n\r\n"
	raw := headers + "4\r\nbody\r\n0\r\n\r\n"
	got, err := ReadHTTPResponse(bytesReader([]byte(raw)), prodMaxHeader, prodMaxBody)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := wantDechunked(headers, "body"); !bytes.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestReadHTTPResponse_ChunkedHexSizeAndExtensions(t *testing.T) {
	// A chunk size in multi-digit hex (0x1a = 26) plus a chunk extension the
	// decoder must parse past and discard.
	body := strings.Repeat("z", 26)
	headers := "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"
	raw := headers + "1a;ext=ignored\r\n" + body + "\r\n0\r\n\r\n"
	got, err := ReadHTTPResponse(bytesReader([]byte(raw)), prodMaxHeader, prodMaxBody)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := wantDechunked(headers, body); !bytes.Equal(got, want) {
		t.Errorf("got len %d, want len %d", len(got), len(want))
	}
}

func TestReadHTTPResponse_ChunkedWithTrailers(t *testing.T) {
	// A trailer header line after the zero chunk must be consumed (and
	// discarded) up to the terminating blank line.
	headers := "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"
	raw := headers + "3\r\nabc\r\n0\r\nX-Checksum: deadbeef\r\n\r\n"
	got, err := ReadHTTPResponse(bytesReader([]byte(raw)), prodMaxHeader, prodMaxBody)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := wantDechunked(headers, "abc"); !bytes.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestReadHTTPResponse_ChunkedSplitAcrossReads(t *testing.T) {
	headers := "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"
	raw := headers + "b\r\nhello world\r\n5\r\n!!!!!\r\n0\r\n\r\n"
	for _, chunk := range []int{1, 2, 3, 7, 4096} {
		t.Run(string(rune('0'+chunk%10)), func(t *testing.T) {
			got, err := ReadHTTPResponse(chunkedReader([]byte(raw), chunk), prodMaxHeader, prodMaxBody)
			if err != nil {
				t.Fatalf("chunk size %d: unexpected error: %v", chunk, err)
			}
			if want := wantDechunked(headers, "hello world!!!!!"); !bytes.Equal(got, want) {
				t.Errorf("chunk size %d: got %q, want %q", chunk, got, want)
			}
		})
	}
}

func TestReadHTTPResponse_ChunkedEmptyBody(t *testing.T) {
	// Immediate zero chunk = empty body, valid.
	headers := "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"
	raw := headers + "0\r\n\r\n"
	got, err := ReadHTTPResponse(bytesReader([]byte(raw)), prodMaxHeader, prodMaxBody)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := wantDechunked(headers, ""); !bytes.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestReadHTTPResponse_ChunkedBadHexSizeRejected(t *testing.T) {
	headers := "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"
	raw := headers + "zz\r\ngarbage\r\n0\r\n\r\n"
	_, err := ReadHTTPResponse(bytesReader([]byte(raw)), prodMaxHeader, prodMaxBody)
	mustContain(t, err, "invalid chunk size")
}

func TestReadHTTPResponse_ChunkedMissingCRLFAfterDataRejected(t *testing.T) {
	// Chunk claims 5 bytes but the trailing CRLF is corrupted.
	headers := "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"
	raw := headers + "5\r\nhelloXX0\r\n\r\n"
	_, err := ReadHTTPResponse(bytesReader([]byte(raw)), prodMaxHeader, prodMaxBody)
	mustContain(t, err, "missing CRLF after chunk data")
}

func TestReadHTTPResponse_ChunkedTruncatedMidChunkRejected(t *testing.T) {
	// Declares 100 bytes but the connection closes after 4.
	headers := "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"
	raw := headers + "64\r\nabcd"
	_, err := ReadHTTPResponse(bytesReader([]byte(raw)), prodMaxHeader, prodMaxBody)
	mustContain(t, err, "closed mid-chunk")
}

func TestReadHTTPResponse_ChunkedTruncatedMidLineRejected(t *testing.T) {
	// Size line never terminated by CRLF before the connection closes.
	headers := "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"
	raw := headers + "5"
	_, err := ReadHTTPResponse(bytesReader([]byte(raw)), prodMaxHeader, prodMaxBody)
	mustContain(t, err, "closed mid-line")
}

func TestReadHTTPResponse_ChunkedBodyOverCapRejected(t *testing.T) {
	maxBody := 1024
	headers := "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"
	// One chunk larger than maxBody: rejected before reading the data.
	raw := headers + "800\r\n" + strings.Repeat("x", 0x800) + "\r\n0\r\n\r\n"
	_, err := ReadHTTPResponse(bytesReader([]byte(raw)), prodMaxHeader, maxBody)
	mustContain(t, err, "too large")
}

func TestReadHTTPResponse_ChunkedManySmallChunksOverCapRejected(t *testing.T) {
	// Accumulated small chunks must also trip the cap (not just a single
	// oversized chunk) — the slow-drip-past-budget guard.
	maxBody := 64
	headers := "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"
	var b strings.Builder
	b.WriteString(headers)
	for i := 0; i < 100; i++ {
		b.WriteString("1\r\nx\r\n") // 100 one-byte chunks = 100 > 64
	}
	b.WriteString("0\r\n\r\n")
	_, err := ReadHTTPResponse(bytesReader([]byte(b.String())), prodMaxHeader, maxBody)
	mustContain(t, err, "too large")
}

func TestReadHTTPResponse_ChunkedSizeLineFloodRejected(t *testing.T) {
	// A size "line" that never terminates: bounded by maxChunkLineLen.
	headers := "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"
	raw := headers + strings.Repeat("a", maxChunkLineLen+10)
	_, err := ReadHTTPResponse(bytesReader([]byte(raw)), prodMaxHeader, prodMaxBody)
	mustContain(t, err, "without CRLF")
}

// === No terminator at all ====================================================

func TestReadHTTPResponse_NoTerminatorConnectionCloses(t *testing.T) {
	// Small response, connection closes before \r\n\r\n ever appears —
	// distinct from the header-flood case (this is well under the cap).
	raw := []byte("HTTP/1.1 200 OK\r\nContent-Type: x")
	_, err := ReadHTTPResponse(bytesReader(raw), prodMaxHeader, prodMaxBody)
	mustContain(t, err, "no header terminator")
}

// === isChunkedEncoding / responseContentLength direct unit coverage ========
// (same package: these are unexported, exercised directly plus via fuzzing)

func TestIsChunkedEncoding_Table(t *testing.T) {
	cases := []struct {
		name    string
		headers string
		want    bool
	}{
		{"absent", "HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\n", false},
		{"chunked", "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n", true},
		{"chunked-mixed-case", "HTTP/1.1 200 OK\r\nTransfer-Encoding: ChUnKeD\r\n\r\n", true},
		{"other-value", "HTTP/1.1 200 OK\r\nTransfer-Encoding: identity\r\n\r\n", false},
		{"no-body", "HTTP/1.1 204 No Content\r\n\r\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isChunkedEncoding([]byte(tc.headers)); got != tc.want {
				t.Errorf("isChunkedEncoding(%q) = %v, want %v", tc.headers, got, tc.want)
			}
		})
	}
}

func TestResponseContentLength_Table(t *testing.T) {
	cases := []struct {
		name    string
		headers string
		want    int
	}{
		{"absent", "HTTP/1.1 200 OK\r\n\r\n", -1},
		{"present", "HTTP/1.1 200 OK\r\nContent-Length: 42\r\n\r\n", 42},
		{"negative", "HTTP/1.1 200 OK\r\nContent-Length: -1\r\n\r\n", -1},
		{"non-integer", "HTTP/1.1 200 OK\r\nContent-Length: abc\r\n\r\n", -1},
		{"zero", "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n", 0},
		{"whitespace", "HTTP/1.1 200 OK\r\nContent-Length:    7   \r\n\r\n", 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := responseContentLength([]byte(tc.headers)); got != tc.want {
				t.Errorf("responseContentLength(%q) = %d, want %d", tc.headers, got, tc.want)
			}
		})
	}
}
