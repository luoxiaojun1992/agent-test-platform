package evaluation

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agent-test-platform/contracts"
	"agent-test-platform/internal/llm"
)

type fakeGateway struct {
	alias    string
	request  llm.CompletionRequest
	response llm.CompletionResponse
	err      error
}

func (gateway *fakeGateway) Complete(_ context.Context, alias string, request llm.CompletionRequest) (llm.CompletionResponse, error) {
	gateway.alias = alias
	gateway.request = request
	return gateway.response, gateway.err
}

func TestJudgeEvaluate(t *testing.T) {
	t.Parallel()

	validator := newContractValidator(t)
	request := readContract(t, "examples/judge-request.json")
	criteria := criteriaFromRequest(t, request)
	result := judgeResultJSON(t, criteria)
	gateway := &fakeGateway{response: llm.CompletionResponse{Content: string(result)}}
	judge, err := NewJudge(gateway, validator, "judge-primary", 2048)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := judge.Evaluate(context.Background(), request, criteria)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Criteria) != len(criteria) || gateway.alias != "judge-primary" {
		t.Fatalf("result=%#v alias=%s", got, gateway.alias)
	}
	if len(gateway.request.Tools) != 0 || gateway.request.ToolChoice.Mode != llm.ToolChoiceNone || len(gateway.request.ResponseSchema) == 0 {
		t.Fatalf("unsafe Judge request = %#v", gateway.request)
	}
	if !strings.Contains(*gateway.request.Messages[1].Content, "<evaluation_data>") {
		t.Fatal("Judge evidence is not delimited")
	}
}

func TestJudgeRejectsMissingOrUnknownCriterion(t *testing.T) {
	t.Parallel()

	validator := newContractValidator(t)
	request := readContract(t, "examples/judge-request.json")
	criteria := criteriaFromRequest(t, request)
	result := judgeResultJSON(t, criteria)
	var document map[string]any
	if err := json.Unmarshal(result, &document); err != nil {
		t.Fatal(err)
	}
	rows := document["criteria"].([]any)
	document["criteria"] = rows[:len(rows)-1]
	invalid, _ := json.Marshal(document)
	gateway := &fakeGateway{response: llm.CompletionResponse{Content: string(invalid)}}
	judge, _ := NewJudge(gateway, validator, "judge-primary", 2048)
	if _, _, err := judge.Evaluate(context.Background(), request, criteria); err == nil || !strings.Contains(err.Error(), "criteria") {
		t.Fatalf("missing criterion error = %v", err)
	}
}

func TestAggregateHardGateCannotBeOverridden(t *testing.T) {
	t.Parallel()

	criteria := []Criterion{{Name: "correctness", Description: "Correct.", Weight: 1}}
	result := JudgeResult{Criteria: []CriterionResult{{CriterionID: "correctness", Score: 100, Reason: "Perfect."}}}
	verdict, err := Aggregate([]DeterministicCheck{
		{ID: "state", Category: "STATE", Critical: true, Weight: 0.5, Pass: false},
		{ID: "tool", Category: "TOOL", Critical: false, Weight: 0.5, Pass: true},
	}, criteria, result, 70, 70)
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Pass || verdict.CriticalPass || verdict.LLMScore != 100 || verdict.FinalScore != 80 {
		t.Fatalf("hard gate verdict = %#v", verdict)
	}
}

func TestAggregateScoresAndThresholds(t *testing.T) {
	t.Parallel()

	criteria := []Criterion{
		{Name: "correctness", Description: "Correct.", Weight: 0.6},
		{Name: "complete", Description: "Complete.", Weight: 0.4},
	}
	result := JudgeResult{Criteria: []CriterionResult{
		{CriterionID: "complete", Score: 80, Reason: "Mostly complete."},
		{CriterionID: "correctness", Score: 90, Reason: "Correct."},
	}}
	verdict, err := Aggregate([]DeterministicCheck{{ID: "state", Weight: 1, Pass: true, Critical: true}}, criteria, result, 70, 70)
	if err != nil {
		t.Fatal(err)
	}
	if !verdict.Pass || verdict.LLMScore != 86 || verdict.FinalScore != 91.6 {
		t.Fatalf("verdict = %#v", verdict)
	}
}

func TestAggregateRejectsInvalidWeightsAndCriteria(t *testing.T) {
	t.Parallel()

	if _, err := Aggregate([]DeterministicCheck{{ID: "a", Weight: 0.9, Pass: true}}, []Criterion{{Name: "x", Description: "X", Weight: 1}}, JudgeResult{Criteria: []CriterionResult{{CriterionID: "x", Score: 1, Reason: "x"}}}, 70, 70); err == nil {
		t.Fatal("invalid deterministic weights passed")
	}
	if err := validateCriteria([]Criterion{{Name: "x", Description: "X", Weight: 0.5}}); err == nil {
		t.Fatal("invalid Judge weights passed")
	}
}

func newContractValidator(t *testing.T) *contracts.Validator {
	t.Helper()
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	return validator
}

func readContract(t *testing.T, path string) []byte {
	t.Helper()
	data, err := contracts.Files.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func criteriaFromRequest(t *testing.T, request []byte) []Criterion {
	t.Helper()
	var document struct {
		Criteria []Criterion `json:"evaluation_criteria"`
	}
	if err := json.Unmarshal(request, &document); err != nil {
		t.Fatal(err)
	}
	return document.Criteria
}

func judgeResultJSON(t *testing.T, criteria []Criterion) []byte {
	t.Helper()
	result := JudgeResult{
		SchemaVersion: "1.0", ToolFindings: []ToolFinding{}, FailureCategories: []string{},
		OverallReason: "Observable evidence supports the result.",
	}
	for _, criterion := range criteria {
		result.Criteria = append(result.Criteria, CriterionResult{CriterionID: criterion.Name, Score: 100, Reason: "Criterion satisfied."})
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
