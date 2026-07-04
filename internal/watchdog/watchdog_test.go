package watchdog

import (
	"net"
	"os"
	"testing"
	"time"
)

// resetForTest clears the lazily-dialed connection and one-shot warning
// state between test cases. Production code never needs this — a real
// process dials NOTIFY_SOCKET at most once in its lifetime — but tests
// swap NOTIFY_SOCKET repeatedly and must not reuse a stale/closed dial.
func resetForTest() {
	mu.Lock()
	if conn != nil {
		conn.Close()
	}
	conn = nil
	dialErr = nil
	dialed = false
	warnedOnce = false
	mu.Unlock()
}

// withNotifySocket points NOTIFY_SOCKET at addr for the duration of the
// test, resetting the package's dial state before and after so earlier
// tests' connections can't leak in.
func withNotifySocket(t *testing.T, addr string) {
	t.Helper()
	old, hadOld := os.LookupEnv("NOTIFY_SOCKET")
	if err := os.Setenv("NOTIFY_SOCKET", addr); err != nil {
		t.Fatalf("setenv NOTIFY_SOCKET: %v", err)
	}
	resetForTest()
	t.Cleanup(func() {
		if hadOld {
			os.Setenv("NOTIFY_SOCKET", old)
		} else {
			os.Unsetenv("NOTIFY_SOCKET")
		}
		resetForTest()
	})
}

// newFakeNotifySocket starts a unixgram listener standing in for the
// systemd-provided NOTIFY_SOCKET, on a filesystem path under t.TempDir().
func newFakeNotifySocket(t *testing.T) (path string, l *net.UnixConn) {
	t.Helper()
	path = t.TempDir() + "/notify.sock"
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen unixgram %s: %v", path, err)
	}
	t.Cleanup(func() { conn.Close() })
	return path, conn
}

func recvDatagram(t *testing.T, l *net.UnixConn) string {
	t.Helper()
	buf := make([]byte, 256)
	if err := l.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	n, err := l.Read(buf)
	if err != nil {
		t.Fatalf("read notify datagram: %v", err)
	}
	return string(buf[:n])
}

func TestEnabled_UnsetIsDisabled(t *testing.T) {
	old, hadOld := os.LookupEnv("NOTIFY_SOCKET")
	os.Unsetenv("NOTIFY_SOCKET")
	defer func() {
		if hadOld {
			os.Setenv("NOTIFY_SOCKET", old)
		}
	}()

	if Enabled() {
		t.Fatal("Enabled() = true with NOTIFY_SOCKET unset")
	}
}

func TestEnabled_SetIsEnabled(t *testing.T) {
	path, _ := newFakeNotifySocket(t)
	withNotifySocket(t, path)

	if !Enabled() {
		t.Fatal("Enabled() = false with NOTIFY_SOCKET set")
	}
}

func TestReady_SendsReadyDatagram(t *testing.T) {
	path, l := newFakeNotifySocket(t)
	withNotifySocket(t, path)

	Ready()

	if got := recvDatagram(t, l); got != "READY=1" {
		t.Fatalf("datagram = %q, want %q", got, "READY=1")
	}
}

func TestKick_SendsWatchdogDatagram(t *testing.T) {
	path, l := newFakeNotifySocket(t)
	withNotifySocket(t, path)

	Kick()

	if got := recvDatagram(t, l); got != "WATCHDOG=1" {
		t.Fatalf("datagram = %q, want %q", got, "WATCHDOG=1")
	}
}

func TestKick_RepeatedCallsReuseConnection(t *testing.T) {
	path, l := newFakeNotifySocket(t)
	withNotifySocket(t, path)

	for i := 0; i < 3; i++ {
		Kick()
		if got := recvDatagram(t, l); got != "WATCHDOG=1" {
			t.Fatalf("kick %d: datagram = %q, want %q", i, got, "WATCHDOG=1")
		}
	}
}

// TestNoop_WhenDisabled asserts Ready/Kick are silent, non-blocking no-ops
// with NOTIFY_SOCKET unset — the state of every dev laptop and `go test`
// invocation, and the acceptance criterion "NOTIFY_SOCKET unset => behavior
// identical to today."
func TestNoop_WhenDisabled(t *testing.T) {
	old, hadOld := os.LookupEnv("NOTIFY_SOCKET")
	os.Unsetenv("NOTIFY_SOCKET")
	resetForTest()
	defer func() {
		if hadOld {
			os.Setenv("NOTIFY_SOCKET", old)
		}
		resetForTest()
	}()

	done := make(chan struct{})
	go func() {
		Ready()
		Kick()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Ready/Kick blocked with NOTIFY_SOCKET unset")
	}
}

// TestKick_AbstractSocketAddress covers the '@'-prefixed abstract-namespace
// form systemd actually uses on Linux (as opposed to a filesystem path),
// which maps to a leading NUL byte on the wire.
func TestKick_AbstractSocketAddress(t *testing.T) {
	name := "@lexa-hub-watchdog-test-abstract"
	addr := &net.UnixAddr{Name: "\x00" + name[1:], Net: "unixgram"}
	l, err := net.ListenUnixgram("unixgram", addr)
	if err != nil {
		t.Skipf("abstract unixgram sockets unavailable: %v", err)
	}
	defer l.Close()

	withNotifySocket(t, name)

	Kick()

	if got := recvDatagram(t, l); got != "WATCHDOG=1" {
		t.Fatalf("datagram = %q, want %q", got, "WATCHDOG=1")
	}
}

func TestKick_NoListenerDoesNotBlock(t *testing.T) {
	// Point at a socket path that has no listener at all: dial itself
	// should fail (unixgram connect requires an existing peer), and Kick
	// must swallow that and return promptly rather than block or panic.
	withNotifySocket(t, t.TempDir()+"/no-such-socket.sock")

	done := make(chan struct{})
	go func() {
		Kick()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Kick blocked with no listener on NOTIFY_SOCKET")
	}
}
