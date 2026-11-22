package toolmatch

import "testing"

func TestNormalizeAndMatchExpectedChain(t *testing.T) {
	t.Parallel()

	rows := int64(1)
	calls, err := NormalizeAgentCalls([]Call{
		{Sequence: 1, CallID: "q1", ToolName: "MYSQL.QUERY", Arguments: map[string]any{"sql": "SELECT id, status FROM orders WHERE id = 'O10001'"}, Result: Result{Status: StatusSuccess, Digest: "q"}},
		{Sequence: 2, CallID: "w1", ToolName: "mysql.execute", Arguments: map[string]any{"sql": "UPDATE orders SET status = 'CANCELLED' WHERE id = 'O10001'"}, Result: Result{Status: StatusSuccess, AffectedRows: &rows, Digest: "w"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := Match(Chain{
		AllowExtraReadCalls: true, MaxActualCalls: 6,
		Steps: []Step{
			{ID: "read", ToolName: "mysql.query", Required: true, Critical: true, SQL: &SQLMatch{Operation: "SELECT", RequiredTables: []string{"orders"}, RequiredColumns: []string{"id", "status"}, RequiredPredicates: []Predicate{{Column: "id", Operator: "=", Value: "O10001"}}}},
			{ID: "write", ToolName: "mysql.execute", Required: true, Critical: true, SQL: &SQLMatch{Operation: "UPDATE", RequiredTables: []string{"orders"}, RequiredPredicates: []Predicate{{Column: "id", Operator: "=", Value: "O10001"}}, RequiredAssignments: []Assignment{{Column: "status", Value: "CANCELLED"}}}, Result: &ResultMatch{Status: StatusSuccess, MinAffectedRows: int64Pointer(1), MaxAffectedRows: int64Pointer(1)}},
		},
	}, calls, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Pass || report.CriticalFailure || report.Score != 100 {
		t.Fatalf("report = %#v", report)
	}
}

func TestFieldLevelSQLMismatchFails(t *testing.T) {
	t.Parallel()

	rows := int64(1)
	calls, err := NormalizeAgentCalls([]Call{{
		Sequence: 1, CallID: "w1", ToolName: "mysql.execute",
		Arguments: map[string]any{"sql": "UPDATE orders SET status = 'CANCELLED', customer_id = 'C2' WHERE id = 'O10001'"},
		Result:    Result{Status: StatusSuccess, AffectedRows: &rows},
	}})
	if err != nil {
		t.Fatal(err)
	}
	report, err := Match(Chain{
		MaxActualCalls: 1,
		Steps: []Step{{
			ID: "write", ToolName: "mysql.execute", Required: true, Critical: true,
			SQL: &SQLMatch{
				Operation: "UPDATE", RequiredTables: []string{"orders"},
				RequiredPredicates:  []Predicate{{Column: "id", Operator: "=", Value: "O10001"}},
				RequiredAssignments: []Assignment{{Column: "status", Value: "CANCELLED"}},
			},
		}},
	}, calls, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.Pass || !hasCode(report, "MISSING_TOOL") {
		t.Fatalf("extra assignment passed field matcher: %#v", report)
	}
}

func TestComputedPredicateFailsClosed(t *testing.T) {
	t.Parallel()

	calls, err := NormalizeAgentCalls([]Call{{
		Sequence: 1, CallID: "q1", ToolName: "mysql.query",
		Arguments: map[string]any{"sql": "SELECT id FROM orders WHERE LOWER(id) = 'o10001'"},
		Result:    Result{Status: StatusSuccess},
	}})
	if err != nil {
		t.Fatal(err)
	}
	report, err := Match(Chain{
		MaxActualCalls: 1,
		Steps: []Step{{
			ID: "read", ToolName: "mysql.query", Required: true, Critical: true,
			SQL: &SQLMatch{Operation: "SELECT", RequiredTables: []string{"orders"}, RequiredPredicates: []Predicate{{Column: "id", Operator: "=", Value: "O10001"}}},
		}},
	}, calls, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.Pass || !report.CriticalFailure {
		t.Fatalf("computed predicate passed critical matcher: %#v", report)
	}
}

func TestArgumentsSchemaAndOptionalWrite(t *testing.T) {
	t.Parallel()

	rows := int64(1)
	calls, err := NormalizeAgentCalls([]Call{{
		Sequence: 1, CallID: "w1", ToolName: "mysql.execute",
		Arguments: map[string]any{"sql": "UPDATE orders SET status = 'CANCELLED' WHERE id = 'O1'", "dry_run": false},
		Result:    Result{Status: StatusSuccess, AffectedRows: &rows},
	}})
	if err != nil {
		t.Fatal(err)
	}
	report, err := Match(Chain{
		MaxActualCalls: 1,
		Steps: []Step{{
			ID: "optional-write", ToolName: "mysql.execute", Required: false,
			ArgumentsSchema: map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []any{"sql", "dry_run"},
				"properties": map[string]any{
					"sql":     map[string]any{"type": "string"},
					"dry_run": map[string]any{"const": false},
				},
			},
			SQL: &SQLMatch{Operation: "UPDATE", RequiredTables: []string{"orders"}},
		}},
	}, calls, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Pass || len(report.MatchedSteps) != 1 {
		t.Fatalf("optional write was not authorized: %#v", report)
	}
}

func TestArgumentsSchemaRejectsWrongOrExternalSchema(t *testing.T) {
	t.Parallel()

	calls, err := NormalizeAgentCalls([]Call{{
		Sequence: 1, CallID: "q1", ToolName: "mysql.query",
		Arguments: map[string]any{"sql": "SELECT 1", "limit": "wrong"},
		Result:    Result{Status: StatusSuccess},
	}})
	if err != nil {
		t.Fatal(err)
	}
	chain := Chain{MaxActualCalls: 1, Steps: []Step{{
		ID: "read", ToolName: "mysql.query", Required: true, Critical: true,
		ArgumentsSchema: map[string]any{"type": "object", "properties": map[string]any{"limit": map[string]any{"type": "integer"}}},
	}}}
	report, err := Match(chain, calls, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.Pass || !hasCode(report, "MISSING_TOOL") {
		t.Fatalf("wrong arguments passed: %#v", report)
	}

	chain.Steps[0].ArgumentsSchema = map[string]any{"$ref": "https://attacker.example/schema.json"}
	if _, err := Match(chain, calls, nil); err == nil {
		t.Fatal("external arguments schema reference was accepted")
	}
}

func TestOptionalStepCannotBeCritical(t *testing.T) {
	t.Parallel()

	if _, err := Match(Chain{MaxActualCalls: 1, Steps: []Step{{ID: "bad", ToolName: "mysql.query", Critical: true}}}, nil, nil); err == nil {
		t.Fatal("critical optional step was accepted")
	}
}

func TestExtraReadAllowedButExtraWriteFails(t *testing.T) {
	t.Parallel()

	rows := int64(1)
	calls, err := NormalizeAgentCalls([]Call{
		{Sequence: 1, CallID: "q0", ToolName: "mysql.query", Arguments: map[string]any{"sql": "SELECT id FROM customer WHERE id = 'C1'"}, Result: Result{Status: StatusSuccess}},
		{Sequence: 2, CallID: "w1", ToolName: "mysql.execute", Arguments: map[string]any{"sql": "UPDATE orders SET status = 'CANCELLED' WHERE id = 'O1'"}, Result: Result{Status: StatusSuccess, AffectedRows: &rows}},
		{Sequence: 3, CallID: "w2", ToolName: "mysql.execute", Arguments: map[string]any{"sql": "DELETE FROM payment WHERE id = 'P1'"}, Result: Result{Status: StatusSuccess, AffectedRows: &rows}},
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := Match(Chain{
		AllowExtraReadCalls: true, MaxActualCalls: 4,
		Steps: []Step{{ID: "write", ToolName: "mysql.execute", Required: true, Critical: true, SQL: &SQLMatch{Operation: "UPDATE", RequiredTables: []string{"orders"}}}},
	}, calls, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.Pass || !report.CriticalFailure || !hasCode(report, "UNAUTHORIZED_TOOL_CALL") {
		t.Fatalf("extra write did not hard fail: %#v", report)
	}
}

func TestMissingAndReorderedCriticalStepFails(t *testing.T) {
	t.Parallel()

	rows := int64(1)
	calls, err := NormalizeAgentCalls([]Call{
		{Sequence: 1, CallID: "w1", ToolName: "mysql.execute", Arguments: map[string]any{"sql": "UPDATE orders SET status = 'CANCELLED' WHERE id = 'O1'"}, Result: Result{Status: StatusSuccess, AffectedRows: &rows}},
		{Sequence: 2, CallID: "q1", ToolName: "mysql.query", Arguments: map[string]any{"sql": "SELECT status FROM orders WHERE id = 'O1'"}, Result: Result{Status: StatusSuccess}},
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := Match(Chain{
		AllowExtraReadCalls: true, MaxActualCalls: 4,
		Steps: []Step{
			{ID: "read", ToolName: "mysql.query", Required: true, Critical: true},
			{ID: "write", ToolName: "mysql.execute", Required: true, Critical: true},
		},
	}, calls, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.Pass || !report.CriticalFailure || !hasCode(report, "MISSING_TOOL") {
		t.Fatalf("reordered chain did not fail: %#v", report)
	}
}

func TestForbiddenRuleAndCallLimitFail(t *testing.T) {
	t.Parallel()

	calls, err := NormalizeAgentCalls([]Call{{
		Sequence: 1, CallID: "w1", ToolName: "mysql.execute",
		Arguments: map[string]any{"sql": "DELETE FROM payment WHERE id = 'P1'"},
		Result:    Result{Status: StatusSuccess},
	}})
	if err != nil {
		t.Fatal(err)
	}
	report, err := Match(Chain{
		MaxActualCalls: 1,
		Forbidden:      []ForbiddenCall{{ToolName: "mysql.*", SQL: &SQLMatch{ForbiddenTables: []string{"payment"}}}},
	}, calls, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.Pass || !hasCode(report, "UNAUTHORIZED_TOOL_CALL") {
		t.Fatalf("forbidden call passed: %#v", report)
	}
}

func TestForbiddenTableDoesNotMatchOtherTable(t *testing.T) {
	t.Parallel()

	calls, err := NormalizeAgentCalls([]Call{{
		Sequence: 1, CallID: "q1", ToolName: "mysql.query",
		Arguments: map[string]any{"sql": "SELECT id FROM orders"},
		Result:    Result{Status: StatusSuccess},
	}})
	if err != nil {
		t.Fatal(err)
	}
	report, err := Match(Chain{
		AllowExtraReadCalls: true,
		MaxActualCalls:      1,
		Forbidden:           []ForbiddenCall{{ToolName: "mysql.*", SQL: &SQLMatch{ForbiddenTables: []string{"payment"}}}},
	}, calls, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Pass {
		t.Fatalf("unrelated table matched forbidden rule: %#v", report)
	}
}

func TestWrongSameToolArgumentsCannotBeSkipped(t *testing.T) {
	t.Parallel()

	rows := int64(1)
	calls, err := NormalizeAgentCalls([]Call{
		{Sequence: 1, CallID: "wrong", ToolName: "mysql.execute", Arguments: map[string]any{"sql": "UPDATE payment SET status = 'VOID' WHERE id = 'P1'"}, Result: Result{Status: StatusSuccess, AffectedRows: &rows}},
		{Sequence: 2, CallID: "right", ToolName: "mysql.execute", Arguments: map[string]any{"sql": "UPDATE orders SET status = 'CANCELLED' WHERE id = 'O1'"}, Result: Result{Status: StatusSuccess, AffectedRows: &rows}},
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := Match(Chain{
		MaxActualCalls: 2,
		Steps:          []Step{{ID: "write", ToolName: "mysql.execute", Required: true, Critical: true, SQL: &SQLMatch{Operation: "UPDATE", RequiredTables: []string{"orders"}}}},
	}, calls, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.Pass || !report.CriticalFailure || !hasCode(report, "MISSING_TOOL") {
		t.Fatalf("wrong first write was skipped: %#v", report)
	}
}

func TestNormalizeRejectsDuplicateAndNonContiguousCalls(t *testing.T) {
	t.Parallel()

	for _, calls := range [][]Call{
		{{Sequence: 2, CallID: "q1", ToolName: "mysql.query", Arguments: map[string]any{}, Result: Result{Status: StatusSuccess}}},
		{{Sequence: 1, CallID: "same", ToolName: "mysql.query", Arguments: map[string]any{}, Result: Result{Status: StatusSuccess}}, {Sequence: 2, CallID: "same", ToolName: "mysql.query", Arguments: map[string]any{}, Result: Result{Status: StatusSuccess}}},
	} {
		if _, err := NormalizeAgentCalls(calls); err == nil {
			t.Fatalf("invalid calls passed: %#v", calls)
		}
	}
}

func TestCanonicalJSONDigestStable(t *testing.T) {
	t.Parallel()

	left, err := CanonicalJSONDigest(map[string]any{"b": true, "a": "x"})
	if err != nil {
		t.Fatal(err)
	}
	right, err := CanonicalJSONDigest(map[string]any{"a": "x", "b": true})
	if err != nil {
		t.Fatal(err)
	}
	if left != right {
		t.Fatalf("digests differ: %s != %s", left, right)
	}
}

func int64Pointer(value int64) *int64 { return &value }

func hasCode(report MatchReport, code string) bool {
	for _, violation := range report.Violations {
		if violation.Code == code {
			return true
		}
	}
	return false
}
