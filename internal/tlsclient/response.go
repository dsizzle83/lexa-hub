package tlsclient

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// HTTPResponse is a parsed HTTP/1.1 response. Deliberately minimal:
// status, content-type, and body. As we add more 2030.5 features
// (ETag, Location, etc.) those become fields here.
type HTTPResponse struct {
	StatusCode    int
	ContentType   string
	Location      string // populated for 201 Created responses
	ContentLength int    // -1 if header absent
	ConnClose     bool   // true if server sent Connection: close
	Body          []byte
}

// parseHTTPResponse parses a raw HTTP/1.1 response. Pure function with
// no I/O — tested directly with byte slices in response_test.go.
//
// This is deliberately not net/http: net/http wants a net.Conn, and
// we have a wolfSSL session pretending to be a connection. Writing
// the parser ourselves is ~30 lines and avoids the impedance mismatch.
// When we eventually wrap wolfSSL in a net.Conn-compatible interface
// (Milestone 4 or later), we can switch to net/http if it makes sense.
func parseHTTPResponse(raw []byte) (*HTTPResponse, error) {
	headerEnd := bytes.Index(raw, []byte("\r\n\r\n"))
	if headerEnd < 0 {
		return nil, fmt.Errorf("malformed response: no header/body separator")
	}
	headerBlock := raw[:headerEnd]
	body := raw[headerEnd+4:]

	lines := bytes.Split(headerBlock, []byte("\r\n"))
	if len(lines) == 0 {
		return nil, fmt.Errorf("empty response")
	}

	// Status line: "HTTP/1.1 200 OK"
	statusParts := bytes.SplitN(lines[0], []byte(" "), 3)
	if len(statusParts) < 3 {
		return nil, fmt.Errorf("malformed status line: %q", lines[0])
	}
	statusCode, err := strconv.Atoi(string(statusParts[1]))
	if err != nil {
		return nil, fmt.Errorf("non-numeric status code: %q", statusParts[1])
	}

	// Header lines
	resp := &HTTPResponse{
		StatusCode:    statusCode,
		ContentLength: -1,
		Body:          body,
	}
	for _, line := range lines[1:] {
		colon := bytes.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(string(line[:colon])))
		value := strings.TrimSpace(string(line[colon+1:]))
		switch name {
		case "content-type":
			// Strip parameters like "; charset=utf-8" so callers can
			// compare against bare media types.
			if semi := strings.IndexByte(value, ';'); semi >= 0 {
				value = strings.TrimSpace(value[:semi])
			}
			resp.ContentType = value
		case "location":
			resp.Location = value
		case "content-length":
			n, err := strconv.Atoi(value)
			if err == nil {
				resp.ContentLength = n
			}
		case "connection":
			resp.ConnClose = strings.EqualFold(value, "close")
		}
	}

	return resp, nil
}
