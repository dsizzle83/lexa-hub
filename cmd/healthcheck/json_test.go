package main

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

// keysOf marshals v and returns its top-level JSON object keys, sorted —
// used to pin the summary/result schema shape so a future refactor can't
// silently rename/drop a field a downstream consumer (a dashboard, an
// operator's jq script) depends on.
func keysOf(t *testing.T, v any) []string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal into map: %v", err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestSummaryJSONSchemaStable(t *testing.T) {
	sum := Summary{
		V:         SummaryV,
		Ts:        "2026-07-09T00:00:00Z",
		BudgetS:   120,
		Commit:    true,
		Attempts:  2,
		DurationS: 5.5,
		Pass:      true,
		Checks: []Result{
			{Name: "systemd", Status: StatusPass, Detail: "7 units active"},
		},
	}
	want := []string{"attempts", "budget_s", "checks", "commit", "duration_s", "pass", "ts", "v"}
	got := keysOf(t, sum)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Summary JSON keys = %v, want %v", got, want)
	}
}

func TestResultJSONSchemaStable(t *testing.T) {
	r := Result{Name: "api", Status: StatusFail, Detail: "unreachable"}
	want := []string{"detail", "name", "status"}
	got := keysOf(t, r)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Result JSON keys = %v, want %v", got, want)
	}
}

func TestResultJSONOmitsEmptyDetail(t *testing.T) {
	r := Result{Name: "modbus", Status: StatusSkip, Detail: ""}
	want := []string{"name", "status"} // detail must be omitted entirely, not ""
	got := keysOf(t, r)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Result with empty Detail: JSON keys = %v, want %v (omitempty)", got, want)
	}
}

func TestSummaryV1(t *testing.T) {
	if SummaryV != 1 {
		t.Fatalf("SummaryV = %d; bumping it is a breaking-schema-change decision, not a passive test fix — update this test deliberately if so", SummaryV)
	}
}

func TestStatusValuesAreExactlyThreeLiterals(t *testing.T) {
	// Pin the exact wire strings the spec's stderr format
	// ("PASS|FAIL|SKIP") and any downstream consumer depend on.
	if StatusPass != "PASS" || StatusFail != "FAIL" || StatusSkip != "SKIP" {
		t.Fatalf("Status literals changed: pass=%q fail=%q skip=%q", StatusPass, StatusFail, StatusSkip)
	}
}
