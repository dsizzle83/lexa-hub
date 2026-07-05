package metrics

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestCounterRegistrationIdempotent(t *testing.T) {
	r := New()
	a := r.Counter("lexa_test_total")
	b := r.Counter("lexa_test_total")
	if a != b {
		t.Fatalf("Counter(name) returned different instances on repeated calls")
	}
	a.Inc()
	if got := b.value(); got != 1 {
		t.Fatalf("b.value() = %d, want 1 (a and b must be the same counter)", got)
	}
}

func TestGaugeRegistrationIdempotent(t *testing.T) {
	r := New()
	a := r.Gauge("lexa_test_gauge")
	b := r.Gauge("lexa_test_gauge")
	if a != b {
		t.Fatalf("Gauge(name) returned different instances on repeated calls")
	}
	a.Set(42)
	if got := b.value(); got != 42 {
		t.Fatalf("b.value() = %v, want 42", got)
	}
}

func TestFormatGoldenCounterAndGauge(t *testing.T) {
	r := New()
	r.Counter("lexa_b_total").Add(5)
	r.Counter("lexa_a_total").Inc()
	r.Gauge("lexa_z_gauge").Set(3.5)
	r.Gauge("lexa_y_gauge").Set(0)

	want := "# TYPE lexa_a_total counter\n" +
		"lexa_a_total 1\n" +
		"# TYPE lexa_b_total counter\n" +
		"lexa_b_total 5\n" +
		"# TYPE lexa_y_gauge gauge\n" +
		"lexa_y_gauge 0\n" +
		"# TYPE lexa_z_gauge gauge\n" +
		"lexa_z_gauge 3.5\n"

	got := r.Format()
	if got != want {
		t.Fatalf("Format() =\n%q\nwant\n%q", got, want)
	}
}

func TestFormatSkipsNaNGauge(t *testing.T) {
	r := New()
	r.Gauge("lexa_nan_gauge").Set(math.NaN())
	r.Gauge("lexa_fine_gauge").Set(1)

	got := r.Format()
	if strings.Contains(got, "lexa_nan_gauge") {
		t.Fatalf("Format() emitted a NaN gauge, want it skipped entirely:\n%s", got)
	}
	if !strings.Contains(got, "lexa_fine_gauge 1") {
		t.Fatalf("Format() dropped a non-NaN gauge:\n%s", got)
	}
}

func TestHandlerRunsCollectorsAndServesText(t *testing.T) {
	r := New()
	r.Collect(func(reg *Registry) {
		reg.Gauge("lexa_collected").Set(7)
	})

	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain prefix", ct)
	}

	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])
	if !strings.Contains(body, "lexa_collected 7") {
		t.Fatalf("response body missing collected gauge:\n%s", body)
	}
}

func TestConcurrentIncrements(t *testing.T) {
	r := New()
	c := r.Counter("lexa_concurrent_total")
	g := r.Gauge("lexa_concurrent_gauge")

	const goroutines = 50
	const perGoroutine = 200

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				c.Inc()
				g.Add(1)
			}
		}()
	}
	wg.Wait()

	want := uint64(goroutines * perGoroutine)
	if got := c.value(); got != want {
		t.Fatalf("counter value = %d, want %d", got, want)
	}
	if got := g.value(); got != float64(want) {
		t.Fatalf("gauge value = %v, want %v", got, float64(want))
	}
}

func TestNilCounterAndGaugeAreNoOps(t *testing.T) {
	var c *Counter
	var g *Gauge
	// None of these must panic — a struct field typed *Counter/*Gauge that a
	// test constructs without wiring metrics (see cmd/hub/actuators_test.go)
	// must be safe to call unconditionally.
	c.Inc()
	c.Add(5)
	c.Set(10)
	g.Set(1)
	g.Add(1)
}

func TestCounterSetOverwritesForExternalMirroring(t *testing.T) {
	r := New()
	c := r.Counter("lexa_mirrored_total")
	c.Set(10)
	c.Set(7) // an external monotonic source can (rarely) be re-read lower across a
	// process restart of the thing it mirrors; Set must still just overwrite.
	if got := c.value(); got != 7 {
		t.Fatalf("value() = %d, want 7", got)
	}
}

func TestServeOffAndEmptyAreNoOps(t *testing.T) {
	r := New()
	// Neither call should panic, block, or bind a listener. There's no
	// observable side effect to assert beyond "returns promptly", which the
	// test framework's own timeout enforces.
	Serve("", r)
	Serve("off", r)
	Serve("OFF", r)
}

func TestStandardGaugesSetsLexaUpAndProcessGauges(t *testing.T) {
	r := New()
	StandardGauges(r)
	out := r.Format()
	if !strings.Contains(out, "lexa_up 1") {
		t.Fatalf("Format() missing lexa_up 1:\n%s", out)
	}
	// Collect hooks only run via Handler/runCollectors, not Format; run them
	// explicitly to verify the process gauges are wired at all.
	r.runCollectors()
	out = r.Format()
	for _, name := range []string{"lexa_goroutines", "lexa_open_fds", "lexa_rss_bytes"} {
		if !strings.Contains(out, "# TYPE "+name+" gauge") {
			t.Fatalf("Format() missing %s after runCollectors:\n%s", name, out)
		}
	}
}
