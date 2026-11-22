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
	"agent-test-platform/internal/evaluation"
	"agent-test-platform/internal/sandbox"
	"agent-test-platform/internal/toolmatch"
	"agent-test-platform/internal/validation"
)

const createTestCaseSchema = "schemas/create-test-case.schema.json"
const maxKnowledgeTextBytes = 128 * 1024

const (
	ToolEvaluationDeterministic = "DETERMINISTIC_CHAIN"
	ToolEvaluationSessionLLM    = "SESSION_LLM_ONLY"
)

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
	Capabilities       map[string]bool
	ToolEvaluationMode string
	Tables             map[string]TableCatalog
	PreconditionDB     evaluation.Queryer
	ExistingPrompts    map[string]struct{}
	ExistingOracles    map[string]struct{}
}

type TableCatalog struct {
	Columns map[string]struct{}
}

type Candidate struct {
	SchemaVersion         string              `json:"schema_version"`
	JobID                 string              `json:"job_id"`
	ClientCaseID          string              `json:"client_case_id"`
	ScenarioRevisionID    string              `json:"scenario_revision_id"`
	DatasetRevisionID     string              `json:"dataset_revision_id"`
	AgentMode             string              `json:"agent_mode"`
	Prompt                string              `json:"prompt"`
	Preconditions         []Assertion         `json:"preconditions"`
	ExpectedOutput        json.RawMessage     `json:"expected_output"`
	Postconditions        []Assertion         `json:"postconditions"`
	AllowedStateChanges   []MutationRule      `json:"allowed_state_changes"`
	KnowledgeDocuments    []KnowledgeDocument `json:"knowledge_documents"`
	ToolEvaluationMode    string              `json:"tool_evaluation_mode"`
	ExpectedToolCallChain *ToolChain          `json:"expected_tool_call_chain"`
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

type Assertion struct {
	ID               string               `json:"id"`
	Query            string               `json:"query"`
	Expected         evaluation.SQLResult `json:"expected"`
	Critical         bool                 `json:"critical"`
	Weight           *float64             `json:"weight"`
	DecimalTolerance string               `json:"decimal_tolerance"`
}

type Predicate struct {
	Column   string `json:"column"`
	Operator string `json:"operator"`
	Value    any    `json:"value"`
}

type Assignment struct {
	Column string `json:"column"`
	Value  any    `json:"value"`
}

type MutationRule struct {
	Table           string    `json:"table"`
	Operation       string    `json:"operation"`
	Columns         []string  `json:"columns"`
	Predicate       Predicate `json:"predicate"`
	MinAffectedRows int       `json:"min_affected_rows"`
	MaxAffectedRows int       `json:"max_affected_rows"`
}

type SQLMatch struct {
	Operation           string       `json:"operation"`
	RequiredTables      []string     `json:"required_tables"`
	ForbiddenTables     []string     `json:"forbidden_tables"`
	RequiredColumns     []string     `json:"required_columns"`
	RequiredPredicates  []Predicate  `json:"required_predicates"`
	RequiredAssignments []Assignment `json:"required_assignments"`
}

type AffectedRows struct {
	Min *int64 `json:"min"`
	Max *int64 `json:"max"`
}

type ResultMatch struct {
	Status       string        `json:"status"`
	AffectedRows *AffectedRows `json:"affected_rows"`
}

type ToolStep struct {
	ID              string         `json:"id"`
	ToolName        string         `json:"tool_name"`
	Required        bool           `json:"required"`
	Critical        bool           `json:"critical"`
	Weight          *float64       `json:"weight"`
	Intent          string         `json:"intent"`
	ArgumentsSchema map[string]any `json:"arguments_schema"`
	SQLMatch        *SQLMatch      `json:"sql_match"`
	ResultMatch     *ResultMatch   `json:"result_match"`
}

type ForbiddenCall struct {
	ToolName string    `json:"tool_name"`
	SQLMatch *SQLMatch `json:"sql_match"`
}

type ToolChain struct {
	MatchMode           string          `json:"match_mode"`
	AllowExtraReadCalls bool            `json:"allow_extra_read_calls"`
	MaxActualCalls      int             `json:"max_actual_calls"`
	Steps               []ToolStep      `json:"steps"`
	Forbidden           []ForbiddenCall `json:"forbidden"`
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
	if candidate.ToolEvaluationMode != scope.ToolEvaluationMode {
		result.Errors = append(result.Errors, validationFailure("TOOL_EVALUATION_MODE_MISMATCH", "$.tool_evaluation_mode", "candidate Tool evaluation mode does not match the Target Agent configuration", false))
	}
	validateUniqueIDs(candidate, &result)
	validateWeights(candidate, &result)
	validateAssertions(ctx, candidate.Preconditions, true, scope, &result)
	validateAssertions(ctx, candidate.Postconditions, false, scope, &result)
	validateMutationRules(candidate, scope, &result)
	validateKnowledgeDocuments(candidate.KnowledgeDocuments, &result)
	if candidate.ToolEvaluationMode == ToolEvaluationDeterministic && candidate.ExpectedToolCallChain != nil {
		chain := compileToolChain(*candidate.ExpectedToolCallChain, scope, &result)
		if len(result.Errors) == 0 {
			if _, err := toolmatch.Match(chain, nil, nil); err != nil {
				result.Errors = append(result.Errors, validationFailure("TOOL_MATCHER_INVALID", "$.expected_tool_call_chain", err.Error(), true))
			}
		}
	}

	result.ContentDigest = digestBytes(payload)
	result.PromptFingerprint = digestBytes([]byte(strings.ToLower(strings.TrimSpace(candidate.Prompt))))
	result.OracleFingerprint = oracleFingerprint(*candidate)
	if _, exists := scope.ExistingPrompts[result.PromptFingerprint]; exists {
		result.Errors = append(result.Errors, validationFailure("DUPLICATE_CASE", "$.prompt", "prompt duplicates an accepted case", true))
	}
	if _, exists := scope.ExistingOracles[result.OracleFingerprint]; exists {
		result.Errors = append(result.Errors, validationFailure("DUPLICATE_CASE", "$.postconditions", "oracle duplicates an accepted case", true))
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
	seen := make(map[string]string)
	for index, assertion := range append(append([]Assertion(nil), candidate.Preconditions...), candidate.Postconditions...) {
		path := fmt.Sprintf("$.assertions[%d].id", index)
		if previous, exists := seen[assertion.ID]; exists {
			result.Errors = append(result.Errors, validationFailure("SCHEMA_INVALID", path, "duplicate ID also used at "+previous, true))
		} else {
			seen[assertion.ID] = path
		}
	}
	if candidate.ExpectedToolCallChain == nil {
		return
	}
	for index, step := range candidate.ExpectedToolCallChain.Steps {
		path := fmt.Sprintf("$.expected_tool_call_chain.steps[%d].id", index)
		if previous, exists := seen[step.ID]; exists {
			result.Errors = append(result.Errors, validationFailure("TOOL_CHAIN_SCHEMA_INVALID", path, "duplicate ID also used at "+previous, true))
		} else {
			seen[step.ID] = path
		}
	}
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
	validateOptionalWeights(candidate.Preconditions, "$.preconditions", result)
	validateOptionalWeights(candidate.Postconditions, "$.postconditions", result)
}

func validateOptionalWeights(assertions []Assertion, path string, result *ValidationResult) {
	weighted := false
	for _, assertion := range assertions {
		weighted = weighted || assertion.Weight != nil
	}
	if !weighted || len(assertions) == 0 {
		return
	}
	total := 0.0
	for index, assertion := range assertions {
		if assertion.Weight == nil {
			result.Errors = append(result.Errors, validationFailure("EVALUATION_WEIGHT_INVALID", fmt.Sprintf("%s[%d].weight", path, index), "all assertions need weights when one weight is supplied", true))
			continue
		}
		total += *assertion.Weight
	}
	if math.Abs(total-1) > 1e-6 {
		result.Errors = append(result.Errors, validationFailure("EVALUATION_WEIGHT_INVALID", path, fmt.Sprintf("weights sum to %v, want 1", total), true))
	}
}

func validateAssertions(ctx context.Context, assertions []Assertion, execute bool, scope Scope, result *ValidationResult) {
	for index, assertion := range assertions {
		path := fmt.Sprintf("$.postconditions[%d]", index)
		if execute {
			path = fmt.Sprintf("$.preconditions[%d]", index)
		}
		info, err := validation.ValidateSQL(assertion.Query, validation.SQLAssertion)
		if err != nil {
			result.Errors = append(result.Errors, validationFailure(sqlErrorCode(err), path+".query", err.Error(), true))
			continue
		}
		for _, table := range info.Tables {
			if _, exists := scope.Tables[strings.ToLower(table)]; !exists {
				result.Errors = append(result.Errors, validationFailure("ENTITY_NOT_FOUND", path+".query", "query references unknown table "+table, true))
			}
		}
		if len(assertion.Expected.Rows) > 1 && !info.Ordered {
			result.Errors = append(result.Errors, validationFailure("SQL_NONDETERMINISTIC", path+".query", "multi-row assertion requires ORDER BY", true))
		}
		if execute {
			if scope.PreconditionDB == nil {
				result.Errors = append(result.Errors, validationFailure("SANDBOX_UNAVAILABLE", path, "precondition database is unavailable", false))
				continue
			}
			execution, err := evaluation.ExecuteSQLAssertion(ctx, scope.PreconditionDB, evaluation.SQLAssertion{
				ID: assertion.ID, Query: assertion.Query, Expected: assertion.Expected, Critical: assertion.Critical,
				Weight: assertion.Weight, DecimalTolerance: assertion.DecimalTolerance,
			})
			if err != nil {
				result.Errors = append(result.Errors, validationFailure("SQL_EXECUTION_ERROR", path, err.Error(), true))
			} else if !execution.Pass {
				result.Errors = append(result.Errors, validationFailure("PRECONDITION_MISMATCH", path, "seed state does not match expected result", true))
			}
		}
	}
}

func validateMutationRules(candidate *Candidate, scope Scope, result *ValidationResult) {
	for index, rule := range candidate.AllowedStateChanges {
		path := fmt.Sprintf("$.allowed_state_changes[%d]", index)
		table, exists := scope.Tables[strings.ToLower(rule.Table)]
		if !exists {
			result.Errors = append(result.Errors, validationFailure("ENTITY_NOT_FOUND", path+".table", "mutation references unknown table", true))
			continue
		}
		if rule.MinAffectedRows > rule.MaxAffectedRows {
			result.Errors = append(result.Errors, validationFailure("MUTATION_SCOPE_INVALID", path, "minimum affected rows exceeds maximum", true))
		}
		for _, column := range append(append([]string(nil), rule.Columns...), rule.Predicate.Column) {
			if _, exists := table.Columns[strings.ToLower(column)]; !exists {
				result.Errors = append(result.Errors, validationFailure("ENTITY_NOT_FOUND", path, "mutation references unknown column "+column, true))
			}
		}
		_, err := sandbox.EvaluateMutations(sandbox.StateDiff{}, []sandbox.MutationRule{{
			ID: fmt.Sprintf("rule-%d", index), Table: rule.Table, Operation: sandbox.Operation(rule.Operation), Columns: rule.Columns,
			Predicate:       sandbox.Predicate{Column: rule.Predicate.Column, Operator: rule.Predicate.Operator, Value: rule.Predicate.Value},
			MinAffectedRows: 0, MaxAffectedRows: rule.MaxAffectedRows,
		}})
		if err != nil {
			result.Errors = append(result.Errors, validationFailure("MUTATION_SCOPE_INVALID", path, err.Error(), true))
		}
	}
}

func compileToolChain(source ToolChain, scope Scope, result *ValidationResult) toolmatch.Chain {
	chain := toolmatch.Chain{AllowExtraReadCalls: source.AllowExtraReadCalls, MaxActualCalls: source.MaxActualCalls}
	for index, step := range source.Steps {
		path := fmt.Sprintf("$.expected_tool_call_chain.steps[%d]", index)
		if !toolAllowed(step.ToolName, scope.Capabilities) {
			result.Errors = append(result.Errors, validationFailure("TOOL_UNKNOWN", path+".tool_name", "tool is not granted by Agent capabilities", true))
		}
		compiled := toolmatch.Step{ID: step.ID, ToolName: step.ToolName, Required: step.Required, Critical: step.Critical, Weight: step.Weight, ArgumentsSchema: step.ArgumentsSchema}
		compiled.SQL = compileSQLMatch(step.SQLMatch, scope, path+".sql_match", result)
		if step.ResultMatch != nil {
			compiled.Result = &toolmatch.ResultMatch{Status: toolmatch.CallStatus(step.ResultMatch.Status)}
			if step.ResultMatch.AffectedRows != nil {
				compiled.Result.MinAffectedRows = step.ResultMatch.AffectedRows.Min
				compiled.Result.MaxAffectedRows = step.ResultMatch.AffectedRows.Max
				if compiled.Result.MinAffectedRows != nil && compiled.Result.MaxAffectedRows != nil && *compiled.Result.MinAffectedRows > *compiled.Result.MaxAffectedRows {
					result.Errors = append(result.Errors, validationFailure("TOOL_MATCHER_INVALID", path+".result_match", "minimum affected rows exceeds maximum", true))
				}
			}
		}
		chain.Steps = append(chain.Steps, compiled)
	}
	for index, forbidden := range source.Forbidden {
		path := fmt.Sprintf("$.expected_tool_call_chain.forbidden[%d].sql_match", index)
		chain.Forbidden = append(chain.Forbidden, toolmatch.ForbiddenCall{ToolName: forbidden.ToolName, SQL: compileSQLMatch(forbidden.SQLMatch, scope, path, result)})
	}
	return chain
}

func compileSQLMatch(source *SQLMatch, scope Scope, path string, result *ValidationResult) *toolmatch.SQLMatch {
	if source == nil {
		return nil
	}
	compiled := &toolmatch.SQLMatch{Operation: source.Operation, RequiredTables: source.RequiredTables, ForbiddenTables: source.ForbiddenTables, RequiredColumns: source.RequiredColumns}
	for _, tableName := range append(append([]string(nil), source.RequiredTables...), source.ForbiddenTables...) {
		if _, exists := scope.Tables[strings.ToLower(tableName)]; !exists {
			result.Errors = append(result.Errors, validationFailure("ENTITY_NOT_FOUND", path, "matcher references unknown table "+tableName, true))
		}
	}
	for _, predicate := range source.RequiredPredicates {
		compiled.RequiredPredicates = append(compiled.RequiredPredicates, toolmatch.Predicate{Column: predicate.Column, Operator: predicate.Operator, Value: predicate.Value})
	}
	for _, assignment := range source.RequiredAssignments {
		compiled.RequiredAssignments = append(compiled.RequiredAssignments, toolmatch.Assignment{Column: assignment.Column, Value: assignment.Value})
	}
	return compiled
}

func toolAllowed(toolName string, capabilities map[string]bool) bool {
	switch strings.ToLower(toolName) {
	case "mysql.query":
		return capabilities["MYSQL_QUERY"]
	case "mysql.execute":
		return capabilities["MYSQL_EXECUTE"]
	default:
		return false
	}
}

func sqlErrorCode(err error) string {
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "exactly one"):
		return "SQL_MULTIPLE_STATEMENTS"
	case strings.Contains(message, "cross-database"):
		return "SQL_CROSS_DATABASE_REFERENCE"
	case strings.Contains(message, "nondeterministic"):
		return "SQL_NONDETERMINISTIC"
	case strings.Contains(message, "only allows") || strings.Contains(message, "not allowed"):
		return "SQL_STATEMENT_NOT_ALLOWED"
	default:
		return "SQL_PARSE_ERROR"
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
		Postconditions []Assertion     `json:"postconditions"`
		ToolMode       string          `json:"tool_evaluation_mode"`
		ToolChain      *ToolChain      `json:"expected_tool_call_chain"`
	}{candidate.ExpectedOutput, candidate.Postconditions, candidate.ToolEvaluationMode, candidate.ExpectedToolCallChain}
	data, _ := json.Marshal(value)
	return digestBytes(data)
}
