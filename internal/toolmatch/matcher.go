package toolmatch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"agent-test-platform/internal/validation"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	TraceAgentReported TraceSource = "AGENT_REPORTED"

	StatusSuccess CallStatus = "SUCCESS"
	StatusError   CallStatus = "ERROR"
)

type TraceSource string
type CallStatus string

type Result struct {
	Status       CallStatus
	AffectedRows *int64
	Digest       string
}

type Call struct {
	Sequence       int
	CallID         string
	ToolName       string
	Arguments      map[string]any
	ArgumentDigest string
	Result         Result
	Source         TraceSource
}

type SQLMatch struct {
	Operation           string
	RequiredTables      []string
	ForbiddenTables     []string
	RequiredColumns     []string
	RequiredPredicates  []Predicate
	RequiredAssignments []Assignment
}

type Predicate struct {
	Column   string
	Operator string
	Value    any
}

type Assignment struct {
	Column string
	Value  any
}

type ResultMatch struct {
	Status          CallStatus
	MinAffectedRows *int64
	MaxAffectedRows *int64
}

type Step struct {
	ID              string
	ToolName        string
	Required        bool
	Critical        bool
	Weight          *float64
	ArgumentsSchema map[string]any
	SQL             *SQLMatch
	Result          *ResultMatch
}

type ForbiddenCall struct {
	ToolName string
	SQL      *SQLMatch
}

type Chain struct {
	AllowExtraReadCalls bool
	MaxActualCalls      int
	Steps               []Step
	Forbidden           []ForbiddenCall
}

type Violation struct {
	Code     string
	StepID   string
	Sequence int
	Critical bool
	Message  string
}

type MatchReport struct {
	Pass            bool
	CriticalFailure bool
	Score           float64
	TraceTrust      TraceSource
	MatchedSteps    []string
	Violations      []Violation
}

// NormalizeAgentCalls validates ordering, identity, limits, and canonical argument digests.
func NormalizeAgentCalls(calls []Call) ([]Call, error) {
	if len(calls) > 32 {
		return nil, fmt.Errorf("tool call count %d exceeds 32", len(calls))
	}
	normalized := make([]Call, len(calls))
	seenIDs := make(map[string]struct{}, len(calls))
	for index, call := range calls {
		if call.Sequence != index+1 {
			return nil, fmt.Errorf("tool call sequence %d at index %d, want %d", call.Sequence, index, index+1)
		}
		call.CallID = strings.TrimSpace(call.CallID)
		call.ToolName = strings.ToLower(strings.TrimSpace(call.ToolName))
		if call.CallID == "" || call.ToolName == "" {
			return nil, fmt.Errorf("tool call %d is missing call ID or tool name", call.Sequence)
		}
		if _, exists := seenIDs[call.CallID]; exists {
			return nil, fmt.Errorf("duplicate tool call ID %q", call.CallID)
		}
		seenIDs[call.CallID] = struct{}{}
		digest, err := CanonicalJSONDigest(call.Arguments)
		if err != nil {
			return nil, fmt.Errorf("tool call %s arguments: %w", call.CallID, err)
		}
		call.ArgumentDigest = digest
		call.Source = TraceAgentReported
		if call.Result.Status != StatusSuccess && call.Result.Status != StatusError {
			return nil, fmt.Errorf("tool call %s has invalid result status %q", call.CallID, call.Result.Status)
		}
		normalized[index] = call
	}
	return normalized, nil
}

// Match evaluates a linear ordered-subsequence chain and deterministic hard failures.
func Match(chain Chain, calls []Call, initialViolations []Violation) (MatchReport, error) {
	if chain.MaxActualCalls < 1 || chain.MaxActualCalls > 32 {
		return MatchReport{}, fmt.Errorf("max actual calls outside [1,32]")
	}
	weights, err := stepWeights(chain.Steps)
	if err != nil {
		return MatchReport{}, err
	}
	report := MatchReport{Pass: true, TraceTrust: TraceAgentReported, Violations: append([]Violation(nil), initialViolations...)}
	if len(calls) > chain.MaxActualCalls {
		report.Violations = append(report.Violations, Violation{Code: "TOOL_CALL_LIMIT_EXCEEDED", Critical: true, Message: "actual call count exceeds test case limit"})
	}
	for _, call := range calls {
		for _, forbidden := range chain.Forbidden {
			matched, err := forbiddenMatches(forbidden, call)
			if err != nil {
				return MatchReport{}, err
			}
			if matched {
				report.Violations = append(report.Violations, Violation{Code: "UNAUTHORIZED_TOOL_CALL", Sequence: call.Sequence, Critical: true, Message: "call matches forbidden tool rule"})
			}
		}
	}

	nextIndex := 0
	matchedIndexes := make(map[int]struct{})
	matchedWeight := 0.0
	for stepIndex, step := range chain.Steps {
		found := -1
		for callIndex := nextIndex; callIndex < len(calls); callIndex++ {
			matches, err := stepMatches(step, calls[callIndex])
			if err != nil {
				return MatchReport{}, err
			}
			if matches {
				found = callIndex
				break
			}
			if strings.EqualFold(step.ToolName, calls[callIndex].ToolName) || isWriteTool(calls[callIndex].ToolName) {
				break
			}
		}
		if found < 0 {
			if step.Required {
				report.Violations = append(report.Violations, Violation{Code: "MISSING_TOOL", StepID: step.ID, Critical: step.Critical, Message: "required tool step was not matched in order"})
			}
			continue
		}
		matchedIndexes[found] = struct{}{}
		nextIndex = found + 1
		report.MatchedSteps = append(report.MatchedSteps, step.ID)
		if step.Required {
			matchedWeight += weights[stepIndex]
		}
	}

	for index, call := range calls {
		if _, matched := matchedIndexes[index]; matched {
			continue
		}
		if isWriteTool(call.ToolName) {
			report.Violations = append(report.Violations, Violation{Code: "UNAUTHORIZED_TOOL_CALL", Sequence: call.Sequence, Critical: true, Message: "extra write call is not allowed"})
		} else if !chain.AllowExtraReadCalls {
			report.Violations = append(report.Violations, Violation{Code: "UNNECESSARY_TOOL_CALL", Sequence: call.Sequence, Message: "extra read call is not allowed"})
		}
	}
	report.Score = matchedWeight * 100
	for _, violation := range report.Violations {
		if violation.Critical {
			report.CriticalFailure = true
		}
	}
	report.Pass = len(report.Violations) == 0
	return report, nil
}

// CanonicalJSONDigest hashes JSON after stable key ordering by encoding/json.
func CanonicalJSONDigest(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:]), nil
}

func stepMatches(step Step, call Call) (bool, error) {
	if !strings.EqualFold(step.ToolName, call.ToolName) {
		return false, nil
	}
	if step.ArgumentsSchema != nil {
		valid, err := validateArguments(step.ArgumentsSchema, call.Arguments)
		if err != nil || !valid {
			return false, err
		}
	}
	if step.SQL != nil {
		query, ok := call.Arguments["sql"].(string)
		if !ok {
			return false, nil
		}
		context := validation.SQLQuery
		if isWriteTool(call.ToolName) {
			context = validation.SQLExecute
		}
		info, err := validation.ValidateSQL(query, context)
		if err != nil {
			return false, nil
		}
		if !sqlInfoMatches(*step.SQL, info) {
			return false, nil
		}
	}
	if step.Result != nil {
		if step.Result.Status != "" && step.Result.Status != call.Result.Status {
			return false, nil
		}
		if step.Result.MinAffectedRows != nil || step.Result.MaxAffectedRows != nil {
			if call.Result.AffectedRows == nil {
				return false, nil
			}
			if step.Result.MinAffectedRows != nil && *call.Result.AffectedRows < *step.Result.MinAffectedRows {
				return false, nil
			}
			if step.Result.MaxAffectedRows != nil && *call.Result.AffectedRows > *step.Result.MaxAffectedRows {
				return false, nil
			}
		}
	}
	return true, nil
}

func forbiddenMatches(forbidden ForbiddenCall, call Call) (bool, error) {
	if !toolPatternMatches(forbidden.ToolName, call.ToolName) {
		return false, nil
	}
	if forbidden.SQL == nil {
		return true, nil
	}
	query, ok := call.Arguments["sql"].(string)
	if !ok {
		return false, nil
	}
	context := validation.SQLQuery
	if isWriteTool(call.ToolName) {
		context = validation.SQLExecute
	}
	info, err := validation.ValidateSQL(query, context)
	if err != nil {
		return false, nil
	}
	return forbiddenSQLMatches(*forbidden.SQL, info), nil
}

func sqlInfoMatches(match SQLMatch, info validation.SQLInfo) bool {
	if match.Operation != "" && !strings.EqualFold(match.Operation, info.Operation) {
		return false
	}
	actualTables := make(map[string]struct{}, len(info.Tables))
	for _, table := range info.Tables {
		actualTables[strings.ToLower(table)] = struct{}{}
	}
	for _, table := range match.RequiredTables {
		if _, ok := actualTables[strings.ToLower(table)]; !ok {
			return false
		}
	}
	for _, table := range match.ForbiddenTables {
		if _, ok := actualTables[strings.ToLower(table)]; ok {
			return false
		}
	}
	if len(match.RequiredColumns) > 0 || len(match.RequiredPredicates) > 0 || len(match.RequiredAssignments) > 0 {
		if !info.SimpleMatchComplete {
			return false
		}
		if !containsAllStrings(info.SelectedColumns, match.RequiredColumns) ||
			!containsPredicates(info.Predicates, match.RequiredPredicates) ||
			!assignmentsExactlyMatch(info.Assignments, match.RequiredAssignments) {
			return false
		}
	}
	return true
}

func forbiddenSQLMatches(match SQLMatch, info validation.SQLInfo) bool {
	if match.Operation != "" && !strings.EqualFold(match.Operation, info.Operation) {
		return false
	}
	actualTables := make(map[string]struct{}, len(info.Tables))
	for _, table := range info.Tables {
		actualTables[strings.ToLower(table)] = struct{}{}
	}
	for _, table := range match.RequiredTables {
		if _, ok := actualTables[strings.ToLower(table)]; !ok {
			return false
		}
	}
	if len(match.RequiredColumns) > 0 || len(match.RequiredPredicates) > 0 || len(match.RequiredAssignments) > 0 {
		if !info.SimpleMatchComplete || !containsAllStrings(info.SelectedColumns, match.RequiredColumns) ||
			!containsPredicates(info.Predicates, match.RequiredPredicates) ||
			!assignmentsExactlyMatch(info.Assignments, match.RequiredAssignments) {
			return false
		}
	}
	if len(match.ForbiddenTables) == 0 {
		return true
	}
	for _, table := range match.ForbiddenTables {
		if _, ok := actualTables[strings.ToLower(table)]; ok {
			return true
		}
	}
	return false
}

func validateArguments(schemaDocument map[string]any, arguments map[string]any) (bool, error) {
	if err := rejectExternalReferences(schemaDocument); err != nil {
		return false, err
	}
	digest, err := CanonicalJSONDigest(schemaDocument)
	if err != nil {
		return false, fmt.Errorf("arguments schema digest: %w", err)
	}
	resource := "urn:agent-test-platform:tool-arguments:" + digest
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	if err := compiler.AddResource(resource, schemaDocument); err != nil {
		return false, fmt.Errorf("register arguments schema: %w", err)
	}
	compiled, err := compiler.Compile(resource)
	if err != nil {
		return false, fmt.Errorf("compile arguments schema: %w", err)
	}
	if err := compiled.Validate(arguments); err != nil {
		return false, nil
	}
	return true, nil
}

func rejectExternalReferences(value any) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == "$ref" {
				reference, ok := child.(string)
				if !ok || !strings.HasPrefix(reference, "#") {
					return fmt.Errorf("external arguments schema references are not allowed")
				}
			}
			if err := rejectExternalReferences(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range typed {
			if err := rejectExternalReferences(child); err != nil {
				return err
			}
		}
	}
	return nil
}

func containsAllStrings(actual, required []string) bool {
	values := make(map[string]struct{}, len(actual))
	for _, value := range actual {
		values[strings.ToLower(value)] = struct{}{}
	}
	for _, value := range required {
		if _, ok := values[strings.ToLower(value)]; !ok {
			return false
		}
	}
	return true
}

func containsPredicates(actual []validation.Predicate, required []Predicate) bool {
	for _, expected := range required {
		found := false
		for _, candidate := range actual {
			if strings.EqualFold(candidate.Column, expected.Column) && strings.EqualFold(candidate.Operator, expected.Operator) && valuesEqual(candidate.Value, expected.Value) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func assignmentsExactlyMatch(actual []validation.Assignment, required []Assignment) bool {
	if len(required) == 0 {
		return true
	}
	if len(actual) != len(required) {
		return false
	}
	for _, expected := range required {
		found := false
		for _, candidate := range actual {
			if strings.EqualFold(candidate.Column, expected.Column) && valuesEqual(candidate.Value, expected.Value) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func valuesEqual(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func stepWeights(steps []Step) ([]float64, error) {
	weights := make([]float64, len(steps))
	requiredCount := 0
	weighted := false
	for _, step := range steps {
		if step.Critical && !step.Required {
			return nil, fmt.Errorf("optional step %s cannot be critical", step.ID)
		}
		if step.Required {
			requiredCount++
			weighted = weighted || step.Weight != nil
		}
	}
	if requiredCount == 0 {
		return weights, nil
	}
	if !weighted {
		value := 1 / float64(requiredCount)
		for index, step := range steps {
			if step.Required {
				weights[index] = value
			}
		}
		return weights, nil
	}
	total := 0.0
	for index, step := range steps {
		if !step.Required {
			continue
		}
		if step.Weight == nil || *step.Weight <= 0 || *step.Weight > 1 {
			return nil, fmt.Errorf("every required step must have a weight in (0,1]")
		}
		weights[index] = *step.Weight
		total += *step.Weight
	}
	if total < 0.999999 || total > 1.000001 {
		return nil, fmt.Errorf("required step weights sum to %v, want 1", total)
	}
	return weights, nil
}

func toolPatternMatches(pattern, name string) bool {
	pattern = strings.ToLower(pattern)
	name = strings.ToLower(name)
	if strings.HasSuffix(pattern, ".*") {
		return strings.HasPrefix(name, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == name
}

func isWriteTool(name string) bool {
	return strings.EqualFold(name, "mysql.execute")
}
