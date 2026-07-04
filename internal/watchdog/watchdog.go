// Package watchdog implements a minimal, hand-rolled sd_notify(3) client for
// the systemd watchdog protocol: READY=1 / WATCHDOG=1 datagrams written to
// the unix socket named by $NOTIFY_SOCKET. It exists to avoid pulling in
// coreos/go-systemd as a dependency for a single datagram write (TASK-007
// decision — see PR description).
//
// Kick is meant to be called from Engine.PlanObserver (cmd/hub/main.go),
// which the orchestrator invokes on the engine's own control goroutine on
// every economic tick, and on every safety pass that produces commands (see
// internal/orchestrator/engine.go tick()/safetyTick()). That placement is
// deliberate and load-bearing: a goroutine-timer-based kick would keep
// signalling liveness even after the tick loop itself wedges — deadlocked
// ReadSystemState, a stuck Optimize, or a stalled synchronous MQTT publish
// in executePlan. Riding the tick is what makes the watchdog detect exactly
// the "live but wedged" failure mode it's for (review §11). Do not add an
// independent ticker goroutine here or call Kick from one.
package watchdog

import (
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// writeDeadline bounds every datagram write. Kick runs on the engine's
// control goroutine, so it must never be able to stall real work waiting on
// a slow or wedged NOTIFY_SOCKET peer (systemd itself, normally instant, but
// this is defensive). A dropped kick under real trouble is the intended
// signal — that's the condition WatchdogSec exists to catch.
const writeDeadline = 100 * time.Millisecond

var (
	mu         sync.Mutex
	conn       *net.UnixConn
	dialErr    error
	dialed     bool
	warnedOnce bool
)

// Enabled reports whether the process was launched under systemd
// supervision with a notify socket (NOTIFY_SOCKET set in the environment).
// When false, Ready and Kick are no-ops: the normal case for dev laptops,
// `go test`, and any binary not started via the Type=notify unit.
func Enabled() bool {
	return os.Getenv("NOTIFY_SOCKET") != ""
}

// Ready sends the sd_notify READY=1 datagram. Call this exactly once, right
// after the control loop has started ticking (after eng.Start() in
// cmd/hub/main.go) — Type=notify holds the unit in "activating" until this
// arrives (or TimeoutStartSec elapses and systemd kills it), so it must not
// be sent before the hub can actually do its job.
//
// Errors are swallowed (logged once): a failure to notify readiness must
// not crash or block hub startup. Worst case systemd's start timeout fires
// and restarts the unit — a safe failure mode, not a silent one (the one
// log line records it).
func Ready() {
	send("READY=1")
}

// Kick sends the sd_notify WATCHDOG=1 keepalive datagram. It must be cheap
// and non-blocking (see writeDeadline): a wedged NOTIFY_SOCKET write must
// never itself become the thing that stalls the caller. Errors are dropped
// without logging — Kick can be invoked every tick, and a sick socket is
// precisely the fault under test elsewhere; noisy logging here would just
// add journald pressure during the condition the watchdog exists to catch.
func Kick() {
	send("WATCHDOG=1")
}

// send writes payload to NOTIFY_SOCKET if the process is running under
// watchdog supervision. All state (the lazily-dialed connection, the
// one-shot warning flag) is guarded by mu; there is no other synchronization
// and no background goroutine.
func send(payload string) {
	if !Enabled() {
		return
	}

	mu.Lock()
	defer mu.Unlock()

	c, err := dialLocked()
	if err != nil {
		warnOnceLocked(err)
		return
	}
	_ = c.SetWriteDeadline(time.Now().Add(writeDeadline))
	if _, err := c.Write([]byte(payload)); err != nil {
		warnOnceLocked(err)
	}
}

// dialLocked lazily resolves and connects the NOTIFY_SOCKET unixgram
// address, caching the connection for the life of the process (must be
// called with mu held). Handles both filesystem-path sockets and Linux
// abstract-namespace sockets: a leading '@' in the env var means abstract,
// represented on the wire by a leading NUL byte per the kernel convention.
func dialLocked() (*net.UnixConn, error) {
	if dialed {
		return conn, dialErr
	}
	dialed = true

	addr := os.Getenv("NOTIFY_SOCKET")
	if strings.HasPrefix(addr, "@") {
		addr = "\x00" + addr[1:]
	}
	conn, dialErr = net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"})
	return conn, dialErr
}

// warnOnceLocked logs the first sd_notify failure and suppresses the rest —
// must be called with mu held.
func warnOnceLocked(err error) {
	if warnedOnce {
		return
	}
	warnedOnce = true
	log.Printf("[watchdog] sd_notify: %v (further errors suppressed)", err)
}
