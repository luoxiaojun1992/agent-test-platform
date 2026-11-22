package sandbox

import (
	"reflect"
	"testing"
)

const schemaDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestDiffStatesAndAllowedMutation(t *testing.T) {
	t.Parallel()

	before := snapshot(row("PAID", "2026-09-11T09:00:00Z"))
	after := snapshot(row("CANCELLED", "2026-09-11T10:00:00Z"))
	diff, err := DiffStates(before, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.Changes) != 1 {
		t.Fatalf("changes = %d, want 1", len(diff.Changes))
	}
	change := diff.Changes[0]
	if change.Operation != OperationUpdate || !reflect.DeepEqual(change.Columns, []string{"status", "updated_at"}) {
		t.Fatalf("change = %#v", change)
	}

	report, err := EvaluateMutations(diff, []MutationRule{{
		ID: "cancel-order", Table: "orders", Operation: OperationUpdate,
		Columns:         []string{"status", "updated_at"},
		Predicate:       Predicate{Column: "id", Operator: "=", Value: "O10001"},
		MinAffectedRows: 1, MaxAffectedRows: 1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Pass || len(report.Violations) != 0 {
		t.Fatalf("report = %#v", report)
	}
}

func TestUnapprovedSideEffectFails(t *testing.T) {
	t.Parallel()

	before := snapshot(row("PAID", "2026-09-11T09:00:00Z"))
	after := snapshot(row("CANCELLED", "2026-09-11T10:00:00Z"))
	after.Tables["customer"] = TableSnapshot{
		"C1": {"id": mustCell(t, CellString, "C1"), "phone": mustCell(t, CellString, "redacted")},
	}
	diff, err := DiffStates(before, after)
	if err != nil {
		t.Fatal(err)
	}
	report, err := EvaluateMutations(diff, []MutationRule{{
		ID: "cancel-order", Table: "orders", Operation: OperationUpdate,
		Columns:         []string{"status", "updated_at"},
		Predicate:       Predicate{Column: "id", Operator: "=", Value: "O10001"},
		MinAffectedRows: 1, MaxAffectedRows: 1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if report.Pass || !hasViolation(report, "UNAPPROVED_MUTATION") {
		t.Fatalf("unapproved side effect passed: %#v", report)
	}
}

func TestSchemaChangeAndAffectedRowsFail(t *testing.T) {
	t.Parallel()

	before := snapshot(row("PAID", "2026-09-11T09:00:00Z"))
	after := snapshot(row("CANCELLED", "2026-09-11T10:00:00Z"))
	after.SchemaDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	diff, err := DiffStates(before, after)
	if err != nil {
		t.Fatal(err)
	}
	report, err := EvaluateMutations(diff, []MutationRule{{
		ID: "requires-two", Table: "orders", Operation: OperationUpdate,
		Columns:         []string{"status", "updated_at"},
		Predicate:       Predicate{Column: "id", Operator: "=", Value: "O10001"},
		MinAffectedRows: 2, MaxAffectedRows: 2,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if report.Pass || !hasViolation(report, "SCHEMA_CHANGED") || !hasViolation(report, "AFFECTED_ROWS_OUT_OF_RANGE") {
		t.Fatalf("hard failures missing: %#v", report)
	}
}

func TestInsertAndDeleteDiffs(t *testing.T) {
	t.Parallel()

	base := snapshot(row("PAID", "2026-09-11T09:00:00Z"))
	after := snapshot(row("PAID", "2026-09-11T09:00:00Z"))
	after.Tables["orders"]["O2"] = Row{
		"id": mustCell(t, CellString, "O2"), "status": mustCell(t, CellString, "PAID"),
	}
	diff, err := DiffStates(base, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.Changes) != 1 || diff.Changes[0].Operation != OperationInsert {
		t.Fatalf("insert diff = %#v", diff)
	}

	diff, err = DiffStates(after, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.Changes) != 1 || diff.Changes[0].Operation != OperationDelete || len(diff.Changes[0].Columns) != 0 {
		t.Fatalf("delete diff = %#v", diff)
	}
}

func TestRowDigestIsStable(t *testing.T) {
	t.Parallel()

	left := Row{"id": mustCell(t, CellString, "O1"), "amount": mustCell(t, CellDecimal, "10.00")}
	right := Row{"amount": mustCell(t, CellDecimal, "10.00"), "id": mustCell(t, CellString, "O1")}
	leftDigest, err := RowDigest(left)
	if err != nil {
		t.Fatal(err)
	}
	rightDigest, err := RowDigest(right)
	if err != nil {
		t.Fatal(err)
	}
	if leftDigest != rightDigest {
		t.Fatalf("digests differ: %s != %s", leftDigest, rightDigest)
	}
}

func TestAmbiguousRulesFail(t *testing.T) {
	t.Parallel()

	diff, err := DiffStates(snapshot(row("PAID", "2026-09-11T09:00:00Z")), snapshot(row("CANCELLED", "2026-09-11T10:00:00Z")))
	if err != nil {
		t.Fatal(err)
	}
	rule := MutationRule{
		Table: "orders", Operation: OperationUpdate, Columns: []string{"status", "updated_at"},
		Predicate:       Predicate{Column: "id", Operator: "=", Value: "O10001"},
		MinAffectedRows: 0, MaxAffectedRows: 1,
	}
	first := rule
	first.ID = "first"
	second := rule
	second.ID = "second"
	report, err := EvaluateMutations(diff, []MutationRule{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if report.Pass || !hasViolation(report, "AMBIGUOUS_MUTATION_RULE") {
		t.Fatalf("ambiguous rules passed: %#v", report)
	}
}

func TestJSONCellRejectsTrailingData(t *testing.T) {
	t.Parallel()

	if _, err := NewCell(CellJSON, `{"ok":true} trailing`); err == nil {
		t.Fatal("JSON cell with trailing data was accepted")
	}
}

func snapshot(order Row) StateSnapshot {
	return StateSnapshot{
		SchemaDigest: schemaDigest,
		Tables: map[string]TableSnapshot{
			"orders": {"O10001": order},
		},
	}
}

func row(status, updatedAt string) Row {
	return Row{
		"id":         {Type: CellString, Value: "O10001"},
		"status":     {Type: CellString, Value: status},
		"updated_at": {Type: CellDateTime, Value: updatedAt},
	}
}

func mustCell(t *testing.T, cellType CellType, value any) Cell {
	t.Helper()
	cell, err := NewCell(cellType, value)
	if err != nil {
		t.Fatal(err)
	}
	return cell
}

func hasViolation(report MutationReport, code string) bool {
	for _, violation := range report.Violations {
		if violation.Code == code {
			return true
		}
	}
	return false
}
