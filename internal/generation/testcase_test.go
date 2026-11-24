package generation

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agent-test-platform/contracts"
)

func TestCreateTestCaseValidatorAcceptsValidCandidate(t *testing.T) {
	t.Parallel()

	validator := newTestCaseValidator(t)
	payload := readCandidate(t)
	scope := validScope()
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
	result := validator.Validate(context.Background(), encoded, validScope())
	if result.Accepted || !containsCode(result.Errors, "REFERENCE_SCOPE_MISMATCH") || !containsCode(result.Errors, "EVALUATION_WEIGHT_INVALID") {
		t.Fatalf("result = %#v", result)
	}
}

func TestCreateTestCaseValidatorRejectsDuplicateFingerprint(t *testing.T) {
	t.Parallel()

	validator := newTestCaseValidator(t)
	scope := validScope()
	first := validator.Validate(context.Background(), readCandidate(t), scope)
	scope.ExistingPrompts = map[string]struct{}{first.PromptFingerprint: {}}
	scope.ExistingOracles = map[string]struct{}{first.OracleFingerprint: {}}
	result := validator.Validate(context.Background(), readCandidate(t), scope)
	if result.Accepted || !containsCode(result.Errors, "DUPLICATE_CASE") {
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
	result := validator.Validate(context.Background(), encoded, validScope())
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

func validScope() Scope {
	return Scope{
		JobID: "01K4Z6Y7X8W9V0T1S2R3Q4P5N6", ScenarioRevisionID: "SC-DRAFT-001", DatasetRevisionID: "DS-DRAFT-001",
		ExistingPrompts: map[string]struct{}{}, ExistingOracles: map[string]struct{}{},
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
