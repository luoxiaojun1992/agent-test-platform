package generation

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"agent-test-platform/contracts"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestCreateTestCaseValidatorAcceptsValidCandidate(t *testing.T) {
	t.Parallel()

	validator := newTestCaseValidator(t)
	payload := readCandidate(t)
	scope, cleanup := validScope(t)
	defer cleanup()
	result := validator.Validate(context.Background(), payload, scope)
	if !result.Accepted || len(result.Errors) != 0 || len(result.ContentDigest) != 64 || len(result.PromptFingerprint) != 64 || len(result.OracleFingerprint) != 64 {
		t.Fatalf("result = %#v", result)
	}
}

func TestCreateTestCaseValidatorRejectsWeightsReferencesAndDuplicates(t *testing.T) {
	t.Parallel()

	validator := newTestCaseValidator(t)
	payload := candidateDocument(t)
	payload["job_id"] = "OTHER-JOB"
	criteria := payload["evaluation_criteria"].([]any)
	criteria[0].(map[string]any)["weight"] = 0.1
	criteria[1].(map[string]any)["name"] = criteria[0].(map[string]any)["name"]
	encoded, _ := json.Marshal(payload)
	scope, cleanup := validScope(t)
	defer cleanup()
	result := validator.Validate(context.Background(), encoded, scope)
	if result.Accepted || !containsCode(result.Errors, "REFERENCE_SCOPE_MISMATCH") || !containsCode(result.Errors, "EVALUATION_WEIGHT_INVALID") {
		t.Fatalf("result = %#v", result)
	}
}

func TestCreateTestCaseValidatorRejectsUnknownEntitiesAndCapabilities(t *testing.T) {
	t.Parallel()

	validator := newTestCaseValidator(t)
	payload := candidateDocument(t)
	mutations := payload["allowed_state_changes"].([]any)
	mutations[0].(map[string]any)["table"] = "production_secrets"
	steps := payload["expected_tool_call_chain"].(map[string]any)["steps"].([]any)
	steps[1].(map[string]any)["tool_name"] = "email.send"
	encoded, _ := json.Marshal(payload)
	scope, cleanup := validScope(t)
	defer cleanup()
	result := validator.Validate(context.Background(), encoded, scope)
	if result.Accepted || !containsCode(result.Errors, "ENTITY_NOT_FOUND") || !containsCode(result.Errors, "TOOL_UNKNOWN") {
		t.Fatalf("result = %#v", result)
	}
}

func TestCreateTestCaseValidatorRejectsPreconditionMismatch(t *testing.T) {
	t.Parallel()

	validator := newTestCaseValidator(t)
	scope, cleanup := validScopeWithStatus(t, "SHIPPED")
	defer cleanup()
	result := validator.Validate(context.Background(), readCandidate(t), scope)
	if result.Accepted || !containsCode(result.Errors, "PRECONDITION_MISMATCH") {
		t.Fatalf("result = %#v", result)
	}
}

func TestCreateTestCaseValidatorRejectsDuplicateFingerprint(t *testing.T) {
	t.Parallel()

	validator := newTestCaseValidator(t)
	scope, cleanup := validScope(t)
	defer cleanup()
	first := validator.Validate(context.Background(), readCandidate(t), scope)
	scope.ExistingPrompts = map[string]struct{}{first.PromptFingerprint: {}}
	scope.ExistingOracles = map[string]struct{}{first.OracleFingerprint: {}}
	result := validator.Validate(context.Background(), readCandidate(t), scope)
	if result.Accepted || !containsCode(result.Errors, "DUPLICATE_CASE") {
		t.Fatalf("result = %#v", result)
	}
}

func TestCreateTestCaseValidatorAcceptsSessionLLMOnlyWithoutToolCatalog(t *testing.T) {
	t.Parallel()

	validator := newTestCaseValidator(t)
	payload := candidateDocument(t)
	payload["tool_evaluation_mode"] = ToolEvaluationSessionLLM
	payload["expected_tool_call_chain"] = nil
	encoded, _ := json.Marshal(payload)
	scope, cleanup := validScope(t)
	defer cleanup()
	scope.ToolEvaluationMode = ToolEvaluationSessionLLM
	scope.Capabilities = nil
	result := validator.Validate(context.Background(), encoded, scope)
	if !result.Accepted || len(result.Errors) != 0 {
		t.Fatalf("result = %#v", result)
	}
}

func TestCreateTestCaseValidatorRejectsToolEvaluationModeDowngrade(t *testing.T) {
	t.Parallel()

	validator := newTestCaseValidator(t)
	payload := candidateDocument(t)
	payload["tool_evaluation_mode"] = ToolEvaluationSessionLLM
	payload["expected_tool_call_chain"] = nil
	encoded, _ := json.Marshal(payload)
	scope, cleanup := validScope(t)
	defer cleanup()
	result := validator.Validate(context.Background(), encoded, scope)
	if result.Accepted || !containsCode(result.Errors, "TOOL_EVALUATION_MODE_MISMATCH") {
		t.Fatalf("result = %#v", result)
	}
}

func TestCreateTestCaseValidatorChecksKnowledgeDigestAndNames(t *testing.T) {
	t.Parallel()

	validator := newTestCaseValidator(t)
	payload := candidateDocument(t)
	payload["knowledge_documents"] = []any{
		map[string]any{"name": "policy.txt", "text": "valid text", "content_sha256": strings.Repeat("a", 64), "metadata": map[string]any{}},
		map[string]any{"name": "POLICY.TXT", "text": "other text", "content_sha256": strings.Repeat("b", 64), "metadata": map[string]any{}},
	}
	encoded, _ := json.Marshal(payload)
	scope, cleanup := validScope(t)
	defer cleanup()
	result := validator.Validate(context.Background(), encoded, scope)
	if result.Accepted || !containsCode(result.Errors, "KNOWLEDGE_DIGEST_MISMATCH") || !containsCode(result.Errors, "KNOWLEDGE_DOCUMENT_INVALID") {
		t.Fatalf("result = %#v", result)
	}
}

func newTestCaseValidator(t *testing.T) *TestCaseValidator {
	t.Helper()
	contractValidator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	validator, err := NewTestCaseValidator(contractValidator)
	if err != nil {
		t.Fatal(err)
	}
	return validator
}

func readCandidate(t *testing.T) []byte {
	t.Helper()
	data, err := contracts.Files.ReadFile("examples/create-test-case.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func candidateDocument(t *testing.T) map[string]any {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(readCandidate(t), &document); err != nil {
		t.Fatal(err)
	}
	return document
}

func validScope(t *testing.T) (Scope, func()) {
	return validScopeWithStatus(t, "PAID")
}

func validScopeWithStatus(t *testing.T, status string) (Scope, func()) {
	t.Helper()
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(regexp.QuoteMeta("select `status` from orders where id = 'O10001'")).WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow(status))
	return Scope{
			JobID: "01K4Z6Y7X8W9V0T1S2R3Q4P5N6", ScenarioRevisionID: "SC-DRAFT-001", DatasetRevisionID: "DS-DRAFT-001",
			ToolEvaluationMode: ToolEvaluationDeterministic,
			Capabilities:       map[string]bool{"MYSQL_QUERY": true, "MYSQL_EXECUTE": true},
			Tables: map[string]TableCatalog{
				"orders":   {Columns: map[string]struct{}{"id": {}, "status": {}, "updated_at": {}}},
				"customer": {Columns: map[string]struct{}{"id": {}}},
				"payment":  {Columns: map[string]struct{}{"id": {}}},
				"shipment": {Columns: map[string]struct{}{"id": {}}},
			},
			PreconditionDB: database,
		}, func() {
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Error(err)
			}
			database.Close()
		}
}

func containsCode(errors []ValidationError, code string) bool {
	for _, failure := range errors {
		if failure.Code == code {
			return true
		}
	}
	return false
}
