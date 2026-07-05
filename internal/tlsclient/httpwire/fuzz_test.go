package httpwire

// Go-native fuzz targets for the CGo-free HTTP response parsing core
// extracted from tlsclient (TASK-047, D9/§10.2). These MUST live in
// httpwire, not internal/tlsclient: `go test` compiles the whole package it
// sits in, and tlsclient imports internal/wolfssl (CGo), so a fuzz test
// there could never run on the sysroot-less CI runner this task's own
// constraint names. httpwire imports nothing beyond stdlib.
//
// Run locally (nightly CI runs the same three at 15m each):
//
//	go test -fuzz=FuzzReadHTTPResponse       -fuzztime=15m ./internal/tlsclient/httpwire/
//	go test -fuzz=FuzzResponseContentLength  -fuzztime=15m ./internal/tlsclient/httpwire/
//	go test -fuzz=FuzzIsChunkedEncoding       -fuzztime=15m ./internal/tlsclient/httpwire/

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// corpusFiles returns the raw response bytes captured in testdata/fuzz/
// (real gridsim responses — see the README in that directory for
// provenance and the capture method). Missing/unreadable dir is not fatal:
// f.Add seeds below still cover the structural cases even without it.
func corpusFiles(f *testing.F) [][]byte {
	f.Helper()
	var out [][]byte
	entries, err := os.ReadDir("testdata/fuzz")
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".raw") {
			continue
		}
		data, err := os.ReadFile(filepath.Join("testdata/fuzz", e.Name()))
		if err != nil {
			continue
		}
		out = append(out, data)
	}
	return out
}

// varyingChunkReader delivers data to ReadHTTPResponse in small, data-
// dependent chunks instead of all at once. The header scan
// (bytes.Index(buf, "\r\n\r\n")) re-runs on the whole growing buffer after
// every read; a single-shot reader that hands over all of data in one call
// would never exercise the "terminator spans two reads" / "cap check fires
// mid-scan" code paths, which is exactly where off-by-one and resume bugs
// live. The chunk length at each step is derived from the next unread byte
// of data itself (not a separate fuzz parameter), so every mutation the
// fuzzer makes to data also mutates how it gets split across reads,
// keeping corpus files a single flat byte blob per the task's corpus
// format.
func varyingChunkReader(data []byte) func([]byte) (int, error) {
	pos := 0
	return func(p []byte) (int, error) {
		if pos >= len(data) {
			return 0, io.EOF
		}
		n := int(data[pos])%7 + 1 // 1..7 bytes per read
		if pos+n > len(data) {
			n = len(data) - pos
		}
		if n > len(p) {
			n = len(p)
		}
		copy(p, data[pos:pos+n])
		pos += n
		return n, nil
	}
}

func FuzzReadHTTPResponse(f *testing.F) {
	for _, seed := range corpusFiles(f) {
		f.Add(seed)
	}
	// Structural cases the task calls out explicitly, in case the corpus
	// dir above is ever unavailable, plus a few of the trickiest edges.
	f.Add([]byte("HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello"))                            // minimal valid 200
	f.Add([]byte("HTTP/1.1 200 OK\r\n\r\nno-content-length-read-until-close"))                    // missing CL
	f.Add([]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\n\r\n")) // chunked header
	f.Add([]byte("HTTP/1.1 200 OK\r\nContent-Length: -5\r\n\r\nbody"))                            // negative CL
	f.Add([]byte("HTTP/1.1 200 OK\r\nContent-Length: 999999999999\r\n\r\nbody"))                  // huge CL
	f.Add(bytes.Repeat([]byte("X-Junk: filler-header-line\r\n"), 3000))                           // header-only flood, no terminator
	f.Add([]byte(""))                                                                             // empty input
	f.Add([]byte("\r\n\r\n"))                                                                     // terminator only, no status line
	f.Add([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"))                                 // zero-length body
	f.Add([]byte("HTTP/1.1 200 OK\r\nContent-Length: 5\r\nContent-Length: 999\r\n\r\nhello"))     // duplicate CL

	f.Fuzz(func(t *testing.T, data []byte) {
		resp, err := ReadHTTPResponse(varyingChunkReader(data), prodMaxHeader, prodMaxBody)

		// Property: error XOR a valid, size-bounded result. Never both a
		// non-nil error and a non-nil response.
		if err != nil {
			if resp != nil {
				t.Fatalf("non-nil error %v AND non-nil response %q", err, resp)
			}
			return
		}
		if resp == nil {
			t.Fatalf("nil error AND nil response — should be impossible")
		}

		// Property: bounded total size.
		if len(resp) > prodMaxHeader+prodMaxBody {
			t.Fatalf("response length %d exceeds maxHeader+maxBody (%d+%d)", len(resp), prodMaxHeader, prodMaxBody)
		}

		// Property: a successful parse always contains the header
		// terminator (that's the only way headerEnd gets set >= 0).
		idx := bytes.Index(resp, headerTerminator)
		if idx < 0 {
			t.Fatalf("successful response %q does not contain %q", resp, headerTerminator)
		}
		headerEnd := idx + len(headerTerminator)

		// Property: on the Content-Length path, the result is exactly
		// headerEnd+cl bytes — not more (trimmed), not less (would have
		// errored as truncated).
		cl := responseContentLength(resp[:headerEnd])
		if cl >= 0 {
			want := headerEnd + cl
			if len(resp) != want {
				t.Fatalf("Content-Length path: got %d bytes, want exactly headerEnd+cl=%d (headerEnd=%d cl=%d)",
					len(resp), want, headerEnd, cl)
			}
		}
	})
}

func FuzzResponseContentLength(f *testing.F) {
	for _, seed := range corpusFiles(f) {
		f.Add(seed)
	}
	f.Add([]byte("HTTP/1.1 200 OK\r\nContent-Length: 42\r\n\r\n"))
	f.Add([]byte("HTTP/1.1 200 OK\r\nContent-Length: -1\r\n\r\n"))
	f.Add([]byte("HTTP/1.1 200 OK\r\nContent-Length: not-a-number\r\n\r\n"))
	f.Add([]byte("HTTP/1.1 200 OK\r\nContent-Length: 5\r\nContent-Length: 999\r\n\r\n"))
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, headers []byte) {
		got := responseContentLength(headers)
		// Determinism: a pure function of its input must return the same
		// value every time (guards against any accidental hidden state).
		if again := responseContentLength(headers); again != got {
			t.Fatalf("non-deterministic: first call %d, second call %d", got, again)
		}
	})
}

func FuzzIsChunkedEncoding(f *testing.F) {
	for _, seed := range corpusFiles(f) {
		f.Add(seed)
	}
	f.Add([]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"))
	f.Add([]byte("HTTP/1.1 200 OK\r\ntransfer-encoding: CHUNKED\r\n\r\n"))
	f.Add([]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: identity\r\n\r\n"))
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, headers []byte) {
		got := isChunkedEncoding(headers)
		if again := isChunkedEncoding(headers); again != got {
			t.Fatalf("non-deterministic: first call %v, second call %v", got, again)
		}
	})
}
