package evaluation

import (
	"encoding/json"
	"fmt"
)

// RequestInput is the persisted evidence needed to build one Judge request. SessionContext carries
// the items pulled from the Target's paginated context API.
type RequestInput struct {
	Outcome        string
	Prompt         string
	CaseJSON       json.RawMessage
	FinalOutput    *string
	SessionContext []json.RawMessage
}

// Prepared is a validated-by-construction Judge request plus the criteria to score.
type Prepared struct {
	RequestJSON []byte
	Criteria    []Criterion
}

type caseContent struct {
	ExpectedOutput       json.RawMessage `json:"expected_output"`
	EvaluationCriteria   []Criterion     `json:"evaluation_criteria"`
	GenerationAssumption []string        `json:"generation_assumptions"`
}

// BuildRequest assembles the Judge request from the Test Case expectation and the session context.
func BuildRequest(input RequestInput) (Prepared, error) {
	var content caseContent
	if err := json.Unmarshal(input.CaseJSON, &content); err != nil {
		return Prepared{}, fmt.Errorf("decode test case content: %w", err)
	}
	context := input.SessionContext
	if context == nil {
		context = []json.RawMessage{}
	}
	assumptions := content.GenerationAssumption
	if assumptions == nil {
		assumptions = []string{}
	}
	request := map[string]any{
		"schema_version": "1.0", "execution_outcome": input.Outcome, "prompt": input.Prompt,
		"expected_output": content.ExpectedOutput, "actual_output": input.FinalOutput, "generation_assumptions": assumptions,
		"session_context": context, "evaluation_criteria": content.EvaluationCriteria,
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return Prepared{}, fmt.Errorf("encode Judge request: %w", err)
	}
	return Prepared{RequestJSON: encoded, Criteria: content.EvaluationCriteria}, nil
}
