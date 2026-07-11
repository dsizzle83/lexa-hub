package apicontract

import (
	"encoding/json"
	"strings"
	"testing"
)

// hasPath reports whether ms contains a mismatch at exactly path.
func hasPath(ms []Mismatch, path string) bool {
	for _, m := range ms {
		if m.Path == path {
			return true
		}
	}
	return false
}

// TestConform_AdditiveIsOK: an extra key in live never breaks conformance —
// the whole point of the additive-evolution rule.
func TestConform_AdditiveIsOK(t *testing.T) {
	golden := `{"a": 1, "b": "x"}`
	live := `{"a": 42, "b": "y", "c": true, "d": {"nested": 1}}`
	if ms := Conform(json.RawMessage(golden), json.RawMessage(live)); len(ms) != 0 {
		t.Fatalf("additive live must conform, got mismatches: %v", ms)
	}
}

// TestConform_RemovedKeyFails: a golden key missing from live is a Mismatch.
func TestConform_RemovedKeyFails(t *testing.T) {
	golden := `{"a": 1, "b": "x"}`
	live := `{"a": 1}`
	ms := Conform(json.RawMessage(golden), json.RawMessage(live))
	if len(ms) != 1 {
		t.Fatalf("want exactly 1 mismatch for a removed key, got %d: %v", len(ms), ms)
	}
	if !hasPath(ms, "$.b") {
		t.Errorf("mismatch path = %q, want $.b", ms[0].Path)
	}
}

// TestConform_RetypeFails covers each JSON-kind flip the rule forbids.
func TestConform_RetypeFails(t *testing.T) {
	cases := []struct {
		name         string
		golden, live string
		wantPath     string
	}{
		{"string to number", `{"a":"x"}`, `{"a":1}`, "$.a"},
		{"number to string", `{"a":1}`, `{"a":"x"}`, "$.a"},
		{"object to array", `{"a":{"k":1}}`, `{"a":[1]}`, "$.a"},
		{"array to object", `{"a":[1]}`, `{"a":{"k":1}}`, "$.a"},
		{"bool to number", `{"a":true}`, `{"a":1}`, "$.a"},
		{"number to null", `{"a":1}`, `{"a":null}`, "$.a"},
		{"string to null", `{"a":"x"}`, `{"a":null}`, "$.a"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ms := Conform(json.RawMessage(c.golden), json.RawMessage(c.live))
			if len(ms) != 1 {
				t.Fatalf("want exactly 1 mismatch, got %d: %v", len(ms), ms)
			}
			if !hasPath(ms, c.wantPath) {
				t.Errorf("mismatch path = %q, want %q", ms[0].Path, c.wantPath)
			}
		})
	}
}

// TestConform_NumberIntFloatSame: JSON has one number kind — an integer golden
// and a float live (or vice versa) conform. Unix-seconds vs a fractional
// age_s must not trip the checker.
func TestConform_NumberIntFloatSame(t *testing.T) {
	if ms := Conform(json.RawMessage(`{"a":1}`), json.RawMessage(`{"a":1.5}`)); len(ms) != 0 {
		t.Errorf("int golden vs float live must conform, got %v", ms)
	}
	if ms := Conform(json.RawMessage(`{"a":1.5}`), json.RawMessage(`{"a":2}`)); len(ms) != 0 {
		t.Errorf("float golden vs int live must conform, got %v", ms)
	}
}

// TestConform_NullGoldenIsNullable: null in golden asserts presence only —
// live may be null OR any concrete kind there.
func TestConform_NullGoldenIsNullable(t *testing.T) {
	golden := `{"opt": null}`
	for _, live := range []string{`{"opt": null}`, `{"opt": 3.14}`, `{"opt": "x"}`, `{"opt": {"k":1}}`} {
		if ms := Conform(json.RawMessage(golden), json.RawMessage(live)); len(ms) != 0 {
			t.Errorf("null-golden vs live %s must conform, got %v", live, ms)
		}
	}
	// ...but the key must still be PRESENT.
	if ms := Conform(json.RawMessage(golden), json.RawMessage(`{}`)); len(ms) != 1 || !hasPath(ms, "$.opt") {
		t.Errorf("null-golden still requires the key present; got %v", ms)
	}
}

// TestConform_NestedRecursion pins that objects recurse key-by-key and report
// the deepest failing path.
func TestConform_NestedRecursion(t *testing.T) {
	golden := `{"outer": {"inner": {"leaf": "x"}}}`
	live := `{"outer": {"inner": {"leaf": 1}}}`
	ms := Conform(json.RawMessage(golden), json.RawMessage(live))
	if len(ms) != 1 || !hasPath(ms, "$.outer.inner.leaf") {
		t.Fatalf("want 1 mismatch at $.outer.inner.leaf, got %v", ms)
	}
}

// TestConform_Arrays covers the homogeneous-array rules: element-0 recursion,
// empty-golden = "is an array", and non-empty-golden vs empty-live.
func TestConform_Arrays(t *testing.T) {
	t.Run("element 0 shape is checked", func(t *testing.T) {
		golden := `{"xs": [{"k": "s"}]}`
		live := `{"xs": [{"k": 1}, {"k": "s"}]}`
		ms := Conform(json.RawMessage(golden), json.RawMessage(live))
		if len(ms) != 1 || !hasPath(ms, "$.xs[0].k") {
			t.Fatalf("want 1 mismatch at $.xs[0].k, got %v", ms)
		}
	})
	t.Run("element 0 conforming array is ok", func(t *testing.T) {
		golden := `{"xs": [{"k": "s"}]}`
		live := `{"xs": [{"k": "hello"}, {"k": "world"}]}`
		if ms := Conform(json.RawMessage(golden), json.RawMessage(live)); len(ms) != 0 {
			t.Fatalf("conforming array must pass, got %v", ms)
		}
	})
	t.Run("empty golden array asserts only array-ness", func(t *testing.T) {
		golden := `{"xs": []}`
		if ms := Conform(json.RawMessage(golden), json.RawMessage(`{"xs": [1,2,3]}`)); len(ms) != 0 {
			t.Errorf("empty golden array vs populated live must pass, got %v", ms)
		}
		if ms := Conform(json.RawMessage(golden), json.RawMessage(`{"xs": []}`)); len(ms) != 0 {
			t.Errorf("empty golden array vs empty live must pass, got %v", ms)
		}
		ms := Conform(json.RawMessage(golden), json.RawMessage(`{"xs": {"k":1}}`))
		if len(ms) != 1 || !hasPath(ms, "$.xs") {
			t.Errorf("empty golden array vs live object must fail at $.xs, got %v", ms)
		}
	})
	t.Run("non-empty golden vs empty live fails", func(t *testing.T) {
		golden := `{"xs": [{"k": "s"}]}`
		ms := Conform(json.RawMessage(golden), json.RawMessage(`{"xs": []}`))
		if len(ms) != 1 || !hasPath(ms, "$.xs[0]") {
			t.Fatalf("want 1 mismatch at $.xs[0], got %v", ms)
		}
	})
}

// TestConform_NormalizesInputs proves Conform accepts raw bytes, RawMessage,
// and typed structs/maps interchangeably (all collapse to the same generic
// JSON shape).
func TestConform_NormalizesInputs(t *testing.T) {
	type inner struct {
		K string `json:"k"`
	}
	type outer struct {
		A int   `json:"a"`
		B inner `json:"b"`
	}
	goldenStruct := outer{A: 1, B: inner{K: "x"}}
	liveMap := map[string]any{"a": 99, "b": map[string]any{"k": "y"}, "extra": true}

	if ms := Conform(goldenStruct, liveMap); len(ms) != 0 {
		t.Fatalf("struct golden vs map live must conform, got %v", ms)
	}
	// Mixed: []byte golden vs struct live.
	if ms := Conform([]byte(`{"a":1,"b":{"k":"x"}}`), goldenStruct); len(ms) != 0 {
		t.Fatalf("[]byte golden vs struct live must conform, got %v", ms)
	}
}

// TestConform_InvalidJSON reports a single rooted mismatch rather than
// panicking.
func TestConform_InvalidJSON(t *testing.T) {
	if ms := Conform(json.RawMessage(`{not json`), json.RawMessage(`{}`)); len(ms) != 1 || ms[0].Path != "$" {
		t.Errorf("invalid golden should yield one $-rooted mismatch, got %v", ms)
	}
	if ms := Conform(json.RawMessage(`{}`), json.RawMessage(`{not json`)); len(ms) != 1 || ms[0].Path != "$" {
		t.Errorf("invalid live should yield one $-rooted mismatch, got %v", ms)
	}
}

// walkStrings visits every string value in a decoded JSON tree.
func walkStrings(v any, visit func(string)) {
	switch t := v.(type) {
	case string:
		visit(t)
	case []any:
		for _, e := range t {
			walkStrings(e, visit)
		}
	case map[string]any:
		for _, e := range t {
			walkStrings(e, visit)
		}
	}
}

// TestFixtures_IntegrityAndNoNonFinite asserts every embedded golden fixture
// parses as JSON (a successful decode already rules out bare NaN/Inf, which
// are not valid JSON) and carries no QUOTED non-finite token either — the same
// "NaN never on the wire" invariant the bus layer enforces (CLAUDE.md /
// internal/bus/finite.go), applied to the HTTP contract's frozen fixtures.
func TestFixtures_IntegrityAndNoNonFinite(t *testing.T) {
	fixtures := Fixtures()
	if len(fixtures) == 0 {
		t.Fatal("no fixtures embedded — the //go:embed http_v1/*.json glob matched nothing")
	}
	nonFinite := map[string]bool{
		"nan": true, "inf": true, "-inf": true, "+inf": true,
		"infinity": true, "-infinity": true, "+infinity": true,
	}
	for name, raw := range fixtures {
		if !strings.HasSuffix(name, ".json") {
			t.Errorf("fixture %q is not a .json file", name)
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Errorf("fixture %q does not parse as JSON: %v", name, err)
			continue
		}
		walkStrings(v, func(s string) {
			if nonFinite[strings.ToLower(strings.TrimSpace(s))] {
				t.Errorf("fixture %q contains a non-finite token string %q", name, s)
			}
		})
	}
}

// TestGolden_KnownAndUnknown pins the accessor contract.
func TestGolden_KnownAndUnknown(t *testing.T) {
	if b := Golden("status.json"); b == nil {
		t.Error("Golden(status.json) = nil, want the embedded fixture bytes")
	}
	if b := Golden("does-not-exist.json"); b != nil {
		t.Errorf("Golden(missing) = %q, want nil", b)
	}
}
