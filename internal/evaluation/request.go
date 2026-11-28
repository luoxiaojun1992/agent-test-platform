package evaluation

import (
	"encoding/json"
	"fmt"
)

// RequestInput is the persisted evidence needed to build one Judge request. SessionContext carries
// the items pulled from the Target's paginated context API up to the current turn.
type RequestInput struct {
	Outcome        string
	TurnNo         int
	TotalTurns     int
	CaseJSON       json.RawMessage
	SessionContext []json.RawMessage
}

// Prepared is a validated-by-construction Judge request plus the criteria to score.
type Prepared struct {
	RequestJSON []byte
	Criteria    []Criterion
}

type caseContent struct {
	Turns                []turnContent `json:"turns"`
	GenerationAssumption []string      `json:"generation_assumptions"`
}

type turnContent struct {
	Turn               int             `json:"turn"`
	Prompt             string          `json:"prompt"`
	ExpectedOutput     json.RawMessage `json:"expected_output"`
	EvaluationCriteria []Criterion     `json:"evaluation_criteria"`
}

// BuildRequest assembles the Judge request for one turn from the Test Case's turn expectation and
// the accumulated session context. actual_output is no longer a platform snapshot — the Judge reads
// the answer from session_context — so it is always null.
func BuildRequest(input RequestInput) (Prepared, error) {
	var content caseContent
	if err := json.Unmarshal(input.CaseJSON, &content); err != nil {
		return Prepared{}, fmt.Errorf("decode test case content: %w", err)
	}
	if input.TurnNo < 1 || input.TurnNo > len(content.Turns) {
		return Prepared{}, fmt.Errorf("turn %d out of range (turns=%d)", input.TurnNo, len(content.Turns))
	}
	turn := content.Turns[input.TurnNo-1]
	context := input.SessionContext
	if context == nil {
		context = []json.RawMessage{}
	}
	assumptions := content.GenerationAssumption
	if assumptions == nil {
		assumptions = []string{}
	}
	request := map[string]any{
		"schema_version": "1.0", "execution_outcome": input.Outcome, "prompt": turn.Prompt,
		"expected_output": turn.ExpectedOutput, "actual_output": nil, "generation_assumptions": assumptions,
		"session_context": context, "evaluation_criteria": turn.EvaluationCriteria,
		"turn": input.TurnNo, "total_turns": input.TotalTurns,
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return Prepared{}, fmt.Errorf("encode Judge request: %w", err)
	}
	return Prepared{RequestJSON: encoded, Criteria: turn.EvaluationCriteria}, nil
}
