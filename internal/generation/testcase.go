package generation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"agent-test-platform/contracts"
)

const createTestCaseSchema = "schemas/create-test-case.schema.json"
const maxKnowledgeTextBytes = 128 * 1024

type ValidationError struct {
	Code      string `json:"code"`
	Path      string `json:"path"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type ValidationResult struct {
	Accepted          bool
	Candidate         Candidate
	ContentDigest     string
	PromptFingerprint string
	OracleFingerprint string
	Errors            []ValidationError
}

type Scope struct {
	JobID              string
	ScenarioRevisionID string
	DatasetRevisionID  string
	ExistingPrompts    map[string]struct{}
	ExistingOracles    map[string]struct{}
}

type Candidate struct {
	SchemaVersion         string              `json:"schema_version"`
	JobID                 string              `json:"job_id"`
	ClientCaseID          string              `json:"client_case_id"`
	ScenarioRevisionID    string              `json:"scenario_revision_id"`
	DatasetRevisionID     string              `json:"dataset_revision_id"`
	Prompt                string              `json:"prompt"`
	ExpectedOutput        json.RawMessage     `json:"expected_output"`
	KnowledgeDocuments    []KnowledgeDocument `json:"knowledge_documents"`
	EvaluationCriteria    []Criterion         `json:"evaluation_criteria"`
	Difficulty            string              `json:"difficulty"`
	DifficultyProfile     map[string]int      `json:"difficulty_profile"`
	GenerationAssumptions []string            `json:"generation_assumptions"`
}

type KnowledgeDocument struct {
	Name          string            `json:"name"`
	Text          string            `json:"text"`
	ContentSHA256 string            `json:"content_sha256"`
	Metadata      map[string]string `json:"metadata"`
}

type Criterion struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Weight      float64 `json:"weight"`
}

type TestCaseValidator struct {
	contracts *contracts.Validator
}

func NewTestCaseValidator(contractValidator *contracts.Validator) (*TestCaseValidator, error) {
	if contractValidator == nil {
		return nil, fmt.Errorf("contract validator is required")
	}
	return &TestCaseValidator{contracts: contractValidator}, nil
}

func (validator *TestCaseValidator) Validate(ctx context.Context, payload []byte, scope Scope) ValidationResult {
	result := ValidationResult{}
	if err := validator.contracts.ValidateJSON(createTestCaseSchema, payload); err != nil {
		result.Errors = append(result.Errors, validationFailure("SCHEMA_INVALID", "$", err.Error(), true))
		return result
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(&result.Candidate); err != nil {
		result.Errors = append(result.Errors, validationFailure("SCHEMA_INVALID", "$", err.Error(), true))
		return result
	}

	candidate := &result.Candidate
	if candidate.JobID != scope.JobID || candidate.ScenarioRevisionID != scope.ScenarioRevisionID || candidate.DatasetRevisionID != scope.DatasetRevisionID {
		result.Errors = append(result.Errors, validationFailure("REFERENCE_SCOPE_MISMATCH", "$", "candidate references do not belong to this staging scope", false))
	}
	validateUniqueIDs(candidate, &result)
	validateWeights(candidate, &result)
	validateKnowledgeDocuments(candidate.KnowledgeDocuments, &result)

	result.ContentDigest = digestBytes(payload)
	result.PromptFingerprint = digestBytes([]byte(strings.ToLower(strings.TrimSpace(candidate.Prompt))))
	result.OracleFingerprint = oracleFingerprint(*candidate)
	if _, exists := scope.ExistingPrompts[result.PromptFingerprint]; exists {
		result.Errors = append(result.Errors, validationFailure("DUPLICATE_CASE", "$.prompt", "prompt duplicates an accepted case", true))
	}
	if _, exists := scope.ExistingOracles[result.OracleFingerprint]; exists {
		result.Errors = append(result.Errors, validationFailure("DUPLICATE_CASE", "$.evaluation_criteria", "oracle duplicates an accepted case", true))
	}
	result.Accepted = len(result.Errors) == 0
	return result
}

func validateKnowledgeDocuments(documents []KnowledgeDocument, result *ValidationResult) {
	seenNames := make(map[string]struct{}, len(documents))
	totalBytes := 0
	for index, document := range documents {
		path := fmt.Sprintf("$.knowledge_documents[%d]", index)
		name := strings.ToLower(strings.TrimSpace(document.Name))
		if _, exists := seenNames[name]; exists {
			result.Errors = append(result.Errors, validationFailure("KNOWLEDGE_DOCUMENT_INVALID", path+".name", "knowledge document names must be unique", true))
		}
		seenNames[name] = struct{}{}
		totalBytes += len([]byte(document.Text))
		digest := sha256.Sum256([]byte(document.Text))
		if hex.EncodeToString(digest[:]) != document.ContentSHA256 {
			result.Errors = append(result.Errors, validationFailure("KNOWLEDGE_DIGEST_MISMATCH", path+".content_sha256", "knowledge content digest does not match text", true))
		}
	}
	if totalBytes > maxKnowledgeTextBytes {
		result.Errors = append(result.Errors, validationFailure("LENGTH_LIMIT_EXCEEDED", "$.knowledge_documents", fmt.Sprintf("knowledge text totals %d bytes, maximum is %d", totalBytes, maxKnowledgeTextBytes), true))
	}
}

func validateUniqueIDs(candidate *Candidate, result *ValidationResult) {
	criteria := make(map[string]struct{}, len(candidate.EvaluationCriteria))
	for index, criterion := range candidate.EvaluationCriteria {
		if _, exists := criteria[criterion.Name]; exists {
			result.Errors = append(result.Errors, validationFailure("EVALUATION_WEIGHT_INVALID", fmt.Sprintf("$.evaluation_criteria[%d].name", index), "criterion names must be unique", true))
		}
		criteria[criterion.Name] = struct{}{}
	}
}

func validateWeights(candidate *Candidate, result *ValidationResult) {
	total := 0.0
	for _, criterion := range candidate.EvaluationCriteria {
		total += criterion.Weight
	}
	if math.Abs(total-1) > 1e-6 {
		result.Errors = append(result.Errors, validationFailure("EVALUATION_WEIGHT_INVALID", "$.evaluation_criteria", fmt.Sprintf("weights sum to %v, want 1", total), true))
	}
}

func validationFailure(code, path, message string, retryable bool) ValidationError {
	return ValidationError{Code: code, Path: path, Message: message, Retryable: retryable}
}

func digestBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func oracleFingerprint(candidate Candidate) string {
	value := struct {
		ExpectedOutput json.RawMessage `json:"expected_output"`
		Criteria       []Criterion     `json:"evaluation_criteria"`
	}{candidate.ExpectedOutput, candidate.EvaluationCriteria}
	data, _ := json.Marshal(value)
	return digestBytes(data)
}
