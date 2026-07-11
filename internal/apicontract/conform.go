package apicontract

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Mismatch is one structural conformance failure: a JSON path (jq-ish, rooted
// at "$") and a human-readable reason. A non-empty []Mismatch from Conform is
// a breaking drift of the live wire shape away from the golden contract.
type Mismatch struct {
	// Path is the JSON location of the failure, e.g. "$.power.solar_W" or
	// "$.evse_stations[0].station_id". Object keys are joined with ".", array
	// element 0 (the only element inspected — see Conform's doc) with "[0]".
	Path string
	// Reason states what differs, phrased golden-relative ("golden has X, live
	// has Y") so a reader can tell which side moved.
	Reason string
}

// String renders a Mismatch as "path: reason" for test output.
func (m Mismatch) String() string { return m.Path + ": " + m.Reason }

// Conform structurally checks that live (the JSON a real handler emitted)
// still satisfies golden (the pinned contract shape), returning every
// divergence. An empty result means live conforms.
//
// Both arguments are normalized to generic JSON values first: json.RawMessage
// / []byte are unmarshaled; any other value (a typed response struct, a
// map[string]any) is round-tripped through encoding/json so it becomes the
// same map[string]any / []any / string / float64 / bool / nil shape a decoded
// document has. This makes Conform indifferent to whether a caller hands it
// raw bytes or a struct.
//
// THE ADDITIVE-EVOLUTION RULE, encoded:
//
//   - The walk is driven by GOLDEN. For every object key present in golden,
//     live MUST contain that key, with a value of the SAME JSON kind (object,
//     array, string, number, bool, or null). Objects recurse key-by-key;
//     arrays recurse into element 0 only (arrays are treated as homogeneous).
//   - EXTRA keys in live are allowed and ignored — a new hub field never
//     breaks the app (the app parses tolerantly, ignoring unknown keys).
//   - A golden key MISSING from live, or present with a different JSON kind
//     (string↔number, object↔array, …), is a Mismatch.
//   - NULL in golden means "key present, value nullable": live may hold null
//     OR any other kind there; only the key's PRESENCE is asserted. (The app
//     models optional quantities as nullable, e.g. *float64.)
//   - JSON has a single number kind: Conform does NOT distinguish integer from
//     floating point (unix-seconds, percentages, and watt counts all arrive as
//     numbers and the app parses either).
//   - An EMPTY golden array asserts only that live is an array (its element
//     shape is unpinned); a NON-empty golden array whose live counterpart is
//     empty is a Mismatch (the element shape cannot be verified).
//
// Values in golden are irrelevant beyond their JSON kind — the fixtures are
// shape templates, not expected payloads.
func Conform(golden, live any) []Mismatch {
	g, err := normalize(golden)
	if err != nil {
		return []Mismatch{{Path: "$", Reason: "golden is not valid JSON: " + err.Error()}}
	}
	l, err := normalize(live)
	if err != nil {
		return []Mismatch{{Path: "$", Reason: "live is not valid JSON: " + err.Error()}}
	}
	var ms []Mismatch
	conform("$", g, l, &ms)
	return ms
}

// normalize turns any input into a generic JSON value (map[string]any / []any
// / string / float64 / bool / nil). Raw bytes are unmarshaled directly;
// everything else is marshaled then unmarshaled so a typed struct collapses to
// the same generic shape a decoded document has.
func normalize(v any) (any, error) {
	var raw []byte
	switch t := v.(type) {
	case nil:
		return nil, nil
	case json.RawMessage:
		raw = []byte(t)
	case []byte:
		raw = t
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// conform walks golden and appends a Mismatch for every way live fails to
// carry golden's shape at path.
func conform(path string, golden, live any, ms *[]Mismatch) {
	// null in golden ⇒ "present, nullable": the caller already asserted the
	// key's presence by recursing here, so any live kind (null included) is
	// acceptable and there is nothing further to check.
	if golden == nil {
		return
	}

	switch g := golden.(type) {
	case map[string]any:
		lm, ok := live.(map[string]any)
		if !ok {
			*ms = append(*ms, Mismatch{path, fmt.Sprintf("golden is object, live is %s", jsonKind(live))})
			return
		}
		// Sorted keys keep the mismatch list deterministic (stable test output).
		keys := make([]string, 0, len(g))
		for k := range g {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			child := path + "." + k
			lv, present := lm[k]
			if !present {
				*ms = append(*ms, Mismatch{child, "key present in golden is missing from live"})
				continue
			}
			conform(child, g[k], lv, ms)
		}

	case []any:
		la, ok := live.([]any)
		if !ok {
			*ms = append(*ms, Mismatch{path, fmt.Sprintf("golden is array, live is %s", jsonKind(live))})
			return
		}
		if len(g) == 0 {
			// Empty golden array asserts only "live is an array" — element
			// shape is intentionally unpinned.
			return
		}
		if len(la) == 0 {
			*ms = append(*ms, Mismatch{path + "[0]", "golden array is non-empty but live array is empty (element shape cannot be verified)"})
			return
		}
		conform(path+"[0]", g[0], la[0], ms)

	case string:
		if _, ok := live.(string); !ok {
			*ms = append(*ms, Mismatch{path, fmt.Sprintf("golden is string, live is %s", jsonKind(live))})
		}

	case float64:
		if _, ok := live.(float64); !ok {
			*ms = append(*ms, Mismatch{path, fmt.Sprintf("golden is number, live is %s", jsonKind(live))})
		}

	case bool:
		if _, ok := live.(bool); !ok {
			*ms = append(*ms, Mismatch{path, fmt.Sprintf("golden is bool, live is %s", jsonKind(live))})
		}

	default:
		// Unreachable for normalized JSON, but fail loud rather than silently
		// pass if it ever happens (e.g. a future non-JSON input path).
		*ms = append(*ms, Mismatch{path, fmt.Sprintf("golden has unsupported kind %T", golden)})
	}
}

// jsonKind names the JSON kind of a normalized value, for mismatch messages.
func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return fmt.Sprintf("%T", v)
	}
}
