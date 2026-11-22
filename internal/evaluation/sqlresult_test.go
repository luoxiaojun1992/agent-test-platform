package evaluation

import (
	"testing"
)

func TestCompareSQLResults(t *testing.T) {
	t.Parallel()

	expected := SQLResult{
		Columns: []SQLColumn{{Name: "status", Type: ScalarString}, {Name: "amount", Type: ScalarDecimal}, {Name: "metadata", Type: ScalarJSON}},
		Rows: [][]SQLScalar{{
			{Type: ScalarString, Value: "CANCELLED"},
			{Type: ScalarDecimal, Value: "10.00"},
			{Type: ScalarJSON, Value: map[string]any{"a": float64(1), "b": true}},
		}},
		Ordered: true,
	}
	actual := SQLResult{
		Columns: []SQLColumn{{Name: "STATUS", Type: ScalarString}, {Name: "amount", Type: ScalarDecimal}, {Name: "metadata", Type: ScalarJSON}},
		Rows: [][]SQLScalar{{
			{Type: ScalarString, Value: "CANCELLED"},
			{Type: ScalarDecimal, Value: "10.01"},
			{Type: ScalarJSON, Value: map[string]any{"b": true, "a": float64(1)}},
		}},
		Ordered: true,
	}

	comparison, err := CompareSQLResults(expected, actual, "0.01")
	if err != nil {
		t.Fatal(err)
	}
	if !comparison.Pass {
		t.Fatalf("comparison failed: %#v", comparison)
	}

	comparison, err = CompareSQLResults(expected, actual, "")
	if err != nil {
		t.Fatal(err)
	}
	if comparison.Pass || len(comparison.Mismatches) != 1 {
		t.Fatalf("exact decimal mismatch not reported: %#v", comparison)
	}
}

func TestTruncatedAndUnorderedResultsFail(t *testing.T) {
	t.Parallel()

	base := SQLResult{
		Columns: []SQLColumn{{Name: "id", Type: ScalarString}},
		Rows:    [][]SQLScalar{{{Type: ScalarString, Value: "1"}}, {{Type: ScalarString, Value: "2"}}},
		Ordered: true,
	}
	truncated := base
	truncated.Truncated = true
	comparison, err := CompareSQLResults(base, truncated, "")
	if err != nil {
		t.Fatal(err)
	}
	if comparison.Pass {
		t.Fatal("truncated result passed")
	}

	unordered := base
	unordered.Ordered = false
	if err := ValidateSQLResult(unordered); err == nil {
		t.Fatal("unordered multi-row result passed")
	}
}

func TestNullAndTypeMismatch(t *testing.T) {
	t.Parallel()

	expected := SQLResult{
		Columns: []SQLColumn{{Name: "value", Type: ScalarString}},
		Rows:    [][]SQLScalar{{{Type: ScalarNull, Value: nil}}},
		Ordered: true,
	}
	actual := SQLResult{
		Columns: []SQLColumn{{Name: "value", Type: ScalarString}},
		Rows:    [][]SQLScalar{{{Type: ScalarString, Value: ""}}},
		Ordered: true,
	}
	comparison, err := CompareSQLResults(expected, actual, "")
	if err != nil {
		t.Fatal(err)
	}
	if comparison.Pass {
		t.Fatal("NULL matched empty string")
	}
}

func TestDecodeSQLResult(t *testing.T) {
	t.Parallel()

	data := []byte(`{
		"columns":[{"name":"created_at","type":"DATETIME"}],
		"rows":[[{"type":"DATETIME","value":"2026-09-11T10:00:00.000000Z"}]],
		"ordered":true,
		"truncated":false
	}`)
	result, err := DecodeSQLResult(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("rows = %d", len(result.Rows))
	}
	if _, err := DecodeSQLResult(append(data, []byte(` {}`)...)); err == nil {
		t.Fatal("trailing JSON was accepted")
	}
	if _, err := DecodeSQLResult(append(data, []byte(` garbage`)...)); err == nil {
		t.Fatal("trailing garbage was accepted")
	}
}

func TestInvalidCanonicalScalars(t *testing.T) {
	t.Parallel()

	tests := []SQLScalar{
		{Type: ScalarInteger, Value: "01"},
		{Type: ScalarDecimal, Value: "1e2"},
		{Type: ScalarDate, Value: "2026-02-30"},
		{Type: ScalarDateTime, Value: "2026-09-11T10:00:00+01:00"},
		{Type: ScalarBinary, Value: "not base64!"},
		{Type: ScalarBoolean, Value: "true"},
	}
	for _, scalar := range tests {
		result := SQLResult{
			Columns: []SQLColumn{{Name: "value", Type: scalar.Type}},
			Rows:    [][]SQLScalar{{scalar}}, Ordered: true,
		}
		if err := ValidateSQLResult(result); err == nil {
			t.Errorf("invalid scalar passed: %#v", scalar)
		}
	}
}

func TestDuplicateColumnNamesFail(t *testing.T) {
	t.Parallel()

	result := SQLResult{
		Columns: []SQLColumn{{Name: "Status", Type: ScalarString}, {Name: "status", Type: ScalarString}},
		Rows:    [][]SQLScalar{{{Type: ScalarString, Value: "PAID"}, {Type: ScalarString, Value: "PAID"}}},
		Ordered: true,
	}
	if err := ValidateSQLResult(result); err == nil {
		t.Fatal("case-insensitive duplicate columns were accepted")
	}
}
