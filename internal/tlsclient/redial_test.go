package tlsclient

import (
	"testing"
	"time"
)

// TestShouldRedialAfter pins the ED-2 retry classifier: a fast failure (dead
// keep-alive) is worth a same-path retry; a failure that ran to the read
// deadline is a stall and must NOT be retried (it would just stall again,
// doubling the walk-loop blackout). The predicate is pure — no cgo, no socket.
func TestShouldRedialAfter(t *testing.T) {
	const rt = 15 * time.Second
	cases := []struct {
		name    string
		elapsed time.Duration
		rt      time.Duration
		want    bool
	}{
		{"instant dead keep-alive", 0, rt, true},
		{"fast reset", 10 * time.Millisecond, rt, true},
		{"half the deadline", rt / 2, rt, true},
		{"just under the 3/4 threshold", rt*3/4 - time.Millisecond, rt, true},
		{"at the 3/4 threshold is a stall", rt * 3 / 4, rt, false},
		{"ran to the deadline", rt, rt, false},
		{"over the deadline (scheduling jitter)", rt + time.Second, rt, false},
		{"no deadline configured falls back to retry", rt, 0, true},
		{"negative deadline (reads disabled) retries", rt, -1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldRedialAfter(c.elapsed, c.rt); got != c.want {
				t.Errorf("shouldRedialAfter(%v, %v) = %v, want %v", c.elapsed, c.rt, got, c.want)
			}
		})
	}
}
