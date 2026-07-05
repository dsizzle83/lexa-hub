// Package metrics is a minimal, in-repo Prometheus text-exposition-format
// library (TASK-044). It is a deliberate alternative to
// github.com/prometheus/client_golang: the repo's dependency posture is
// already lean by choice (cmd/mqttproxy/inject.go hand-rolls MQTT 3.1.1
// rather than import paho into the harness; internal/watchdog hand-rolls
// sd_notify rather than import coreos/go-systemd), client_golang pulls a
// sizeable dependency tree (procfs, protobuf, etc.) into six
// CGO_ENABLED=0 services for what is, today, ~40 metric series, and the
// text exposition format itself is trivial: `# TYPE name counter|gauge`
// followed by `name value`.
//
// If a reviewer prefers client_golang, the swap is mechanical: Registry ≈
// prometheus.Registry, Counter/Gauge ≈ prometheus.Counter/Gauge (this
// package's Counter.Inc/Add and Gauge.Set/Add match client_golang's method
// names on purpose), Collect ≈ a prometheus.Collector's Collect method, and
// Handler ≈ promhttp.HandlerFor. No call site outside this package would
// need to change shape, only its import.
//
// internal/metrics imports nothing from lexa-hub (stdlib only) — it is a
// leaf package. Anything that needs metrics informed by lexa-hub state
// (bus.VersionRejects, orchestrator plan state, …) wires that in from the
// calling cmd/*/main.go via Collect, a Counter/Gauge reference, or a plain
// function value — never by this package importing that state.
package metrics

import (
	"bytes"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Counter is a monotonically-increasing value, safe for concurrent use.
//
// Add/Inc are for hot-path instrumentation (a publish failure, a reconnect).
// Set is for the different case of mirroring an externally-maintained
// monotonic counter (e.g. bus.VersionRejects(), summed at scrape time via a
// Collect hook) — it overwrites rather than accumulates, so it must not be
// mixed with Add/Inc on the same Counter.
//
// All three methods are nil-receiver-safe (a no-op on a nil *Counter) so a
// struct field typed *Counter that a test constructs without wiring metrics
// (see cmd/hub/actuators_test.go) can call Inc()/Add() unconditionally
// instead of needing a nil check at every call site.
type Counter struct{ v uint64 }

// Inc increments the counter by 1.
func (c *Counter) Inc() {
	if c == nil {
		return
	}
	atomic.AddUint64(&c.v, 1)
}

// Add increments the counter by delta.
func (c *Counter) Add(delta uint64) {
	if c == nil {
		return
	}
	atomic.AddUint64(&c.v, delta)
}

// Set overwrites the counter's value. See the type doc — for mirroring an
// external monotonic source at scrape time, not for hot-path increments.
func (c *Counter) Set(v uint64) {
	if c == nil {
		return
	}
	atomic.StoreUint64(&c.v, v)
}

func (c *Counter) value() uint64 { return atomic.LoadUint64(&c.v) }

// Gauge is a value that may go up or down, safe for concurrent use. Set/Add
// are nil-receiver-safe, as Counter's methods are above.
type Gauge struct{ bits uint64 }

// Set overwrites the gauge's current value.
func (g *Gauge) Set(v float64) {
	if g == nil {
		return
	}
	atomic.StoreUint64(&g.bits, math.Float64bits(v))
}

// Add adds delta to the gauge's current value (CAS retry loop — Gauge has no
// hot-path use in this codebase, so a loop is preferable to a mutex only for
// symmetry with Counter/Set below).
func (g *Gauge) Add(delta float64) {
	if g == nil {
		return
	}
	for {
		old := atomic.LoadUint64(&g.bits)
		next := math.Float64bits(math.Float64frombits(old) + delta)
		if atomic.CompareAndSwapUint64(&g.bits, old, next) {
			return
		}
	}
}

func (g *Gauge) value() float64 { return math.Float64frombits(atomic.LoadUint64(&g.bits)) }

// Registry holds a process's named counters and gauges and renders them in
// Prometheus text exposition format. The zero value is not usable; use New.
type Registry struct {
	mu       sync.Mutex
	counters map[string]*Counter
	gauges   map[string]*Gauge

	collectMu  sync.Mutex
	collectors []func(*Registry)
}

// New returns an empty Registry.
func New() *Registry {
	return &Registry{
		counters: make(map[string]*Counter),
		gauges:   make(map[string]*Gauge),
	}
}

// Counter returns the named counter, creating it (initialized to 0) on
// first use. Registration is idempotent: repeated calls with the same name
// return the same *Counter. Calling Counter and Gauge with the same name is
// a programming error (the second call silently shadows the first kind in
// output ordering, see writeTo) — every metric name in this codebase's
// inventory is used as exactly one kind.
func (r *Registry) Counter(name string) *Counter {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.counters[name]; ok {
		return c
	}
	c := &Counter{}
	r.counters[name] = c
	return c
}

// Gauge returns the named gauge, creating it (initialized to 0) on first
// use. Registration is idempotent, as Counter above.
func (r *Registry) Gauge(name string) *Gauge {
	r.mu.Lock()
	defer r.mu.Unlock()
	if g, ok := r.gauges[name]; ok {
		return g
	}
	g := &Gauge{}
	r.gauges[name] = g
	return g
}

// Collect registers fn to run at the start of every scrape (Handler ServeHTTP
// call), before rendering. Use it for values that are cheap to read only at
// scrape time and would be wasted work on every tick: process gauges
// (goroutines/fds/RSS — see StandardGauges), or mirroring an external
// monotonic counter like bus.VersionRejects(). fn receives the same Registry
// so it can call Counter/Gauge and Set the scrape-time value. Collect itself
// is safe to call from any goroutine but is normally only called during
// service startup, before Serve.
func (r *Registry) Collect(fn func(*Registry)) {
	r.collectMu.Lock()
	r.collectors = append(r.collectors, fn)
	r.collectMu.Unlock()
}

func (r *Registry) runCollectors() {
	r.collectMu.Lock()
	fns := make([]func(*Registry), len(r.collectors))
	copy(fns, r.collectors)
	r.collectMu.Unlock()
	for _, fn := range fns {
		fn(r)
	}
}

// writeTo renders the registry in Prometheus text exposition format:
//
//	# TYPE <name> counter
//	<name> <value>
//
// Counters are emitted before gauges; within each kind, names are sorted so
// output is deterministic (golden-test friendly and diff-friendly across
// scrapes). A gauge whose current value is NaN is skipped entirely (no TYPE
// line either) — Prometheus's text format has no valid encoding for NaN as
// itself meaningful (it renders as the literal "NaN", which promtool accepts,
// but every metric in this codebase's inventory is either always-finite or
// "not yet known", and the latter is better represented as absent than as a
// value a PromQL rate()/sum() would silently propagate).
func (r *Registry) writeTo(buf *bytes.Buffer) {
	r.mu.Lock()
	counterNames := make([]string, 0, len(r.counters))
	for name := range r.counters {
		counterNames = append(counterNames, name)
	}
	gaugeNames := make([]string, 0, len(r.gauges))
	for name := range r.gauges {
		gaugeNames = append(gaugeNames, name)
	}
	// Snapshot the *Counter/*Gauge pointers while holding r.mu; the values
	// themselves are read after unlocking via their own atomics, so a
	// concurrent Counter()/Gauge() registering a brand new name mid-render
	// cannot deadlock or corrupt this render (it simply appears next scrape).
	counters := r.counters
	gauges := r.gauges
	r.mu.Unlock()

	sort.Strings(counterNames)
	sort.Strings(gaugeNames)

	for _, name := range counterNames {
		fmt.Fprintf(buf, "# TYPE %s counter\n%s %d\n", name, name, counters[name].value())
	}
	for _, name := range gaugeNames {
		v := gauges[name].value()
		if math.IsNaN(v) {
			continue
		}
		fmt.Fprintf(buf, "# TYPE %s gauge\n%s %s\n", name, name, strconv.FormatFloat(v, 'g', -1, 64))
	}
}

// Format renders the registry to a string without running scrape-time
// Collect hooks first — used by tests that want a pure snapshot of
// explicitly-set values (the golden-format test). Handler, below, is the
// production path and does run them.
func (r *Registry) Format() string {
	var buf bytes.Buffer
	r.writeTo(&buf)
	return buf.String()
}

// Handler returns an http.Handler serving the registry's current state at
// GET /metrics in Prometheus text exposition format. Every call runs the
// registered Collect hooks first.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		r.runCollectors()
		var buf bytes.Buffer
		r.writeTo(&buf)
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write(buf.Bytes())
	})
}

// readHeaderTimeout bounds how long Serve's http.Server waits to read a
// request's headers — a defensive ceiling against a slow-loris client on a
// listener nothing else in this codebase hardens (05 §7: new network
// surface).
const readHeaderTimeout = 5 * time.Second

// Serve starts an HTTP server exposing r at addr's /metrics route, in its
// own goroutine, and returns immediately (nil return — Serve never blocks
// the caller). addr's off-switches: an empty string or the literal "off"
// (case-insensitive) mean "do not serve" — Serve simply returns without
// starting anything, so every cmd/*/main.go can call this unconditionally
// right after resolving cfg.MetricsAddr.
//
// A bind failure (port already in use, no permission, …) is logged and
// Serve returns — it must NEVER be fatal. A metrics listener is purely
// additive instrumentation; a port collision on it must not take down a
// production service (05 §7's blast-radius framing, and this task's own
// "don't block anything on the metrics listener" instruction).
func Serve(addr string, r *Registry) {
	if addr == "" || equalFoldOff(addr) {
		return
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", r.Handler())
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
	}
	go func() {
		log.Printf("metrics: serving /metrics on %s", addr)
		// ListenAndServe binds and serves in one call; a bind failure (port
		// already in use, no permission, …) surfaces here, in this goroutine,
		// as an ordinary error — never a panic or a fatal exit. This is the
		// same "log and continue" shape as the process's own /healthz server
		// in cmd/api/main.go, except that one is allowed to be load-bearing
		// (lexa-api's actual API) where this one is deliberately not.
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("metrics: serve %s: %v — metrics endpoint disabled for this run", addr, err)
		}
	}()
}

func equalFoldOff(s string) bool {
	if len(s) != 3 {
		return false
	}
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b) == "off"
}

// StandardGauges sets lexa_up to 1 and registers a Collect hook computing
// the three process gauges every service in the inventory shares:
// lexa_goroutines (runtime.NumGoroutine), lexa_open_fds (entries under
// /proc/self/fd), and lexa_rss_bytes (field 2 of /proc/self/statm ×
// os.Getpagesize()). fd/RSS reads happen only at scrape time (never per
// tick — fd counting opens a directory handle itself, which is fine once
// per scrape and wasteful on a hot path). Linux-only: on any other GOOS the
// fd/RSS readers return 0 rather than erroring (deploy targets are Linux;
// see procGauges.go).
func StandardGauges(r *Registry) {
	r.Gauge("lexa_up").Set(1)
	r.Collect(func(r *Registry) {
		r.Gauge("lexa_goroutines").Set(float64(runtime.NumGoroutine()))
		r.Gauge("lexa_open_fds").Set(float64(openFDs()))
		r.Gauge("lexa_rss_bytes").Set(float64(rssBytes()))
	})
}

// openFDs counts this process's open file descriptors via /proc/self/fd.
// Returns 0 (not an error) on any failure — a metrics reader must never be
// able to crash or destabilize the service it's instrumenting.
func openFDs() int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0
	}
	return len(entries)
}

// rssBytes reads this process's resident set size from /proc/self/statm:
// field 2 (0-indexed: 1), in pages, × os.Getpagesize(). Returns 0 on any
// parse failure (non-Linux, unreadable file, unexpected format) rather than
// erroring — see openFDs' doc for why.
func rssBytes() int64 {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return pages * int64(os.Getpagesize())
}
