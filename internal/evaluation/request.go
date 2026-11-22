package evaluation

import (
	"encoding/json"
	"errors"
	"fmt"

	"agent-test-platform/internal/evidence"
)

const (
	ToolModeChain = "DETERMINISTIC_CHAIN"
	toolCheckID   = "tool_chain"
)

// RequestInput is the persisted evidence needed to build one Judge request.
type RequestInput struct {
	Outcome       string
	Prompt        string
	CaseJSON      json.RawMessage
	FinalOutput   *string
	Deterministic json.RawMessage
	ToolCalls     []evidence.Record
}

// Prepared is a validated-by-construction Judge request plus the inputs Aggregate needs.
type Prepared struct {
	RequestJSON []byte
	Criteria    []Criterion
	Checks      []DeterministicCheck
}

type storedCheck struct {
	ID       string          `json:"id"`
	Category string          `json:"category"`
	Critical bool            `json:"critical"`
	Weight   float64         `json:"weight"`
	Pass     bool            `json:"pass"`
	Expected json.RawMessage `json:"expected"`
	Actual   json.RawMessage `json:"actual"`
	Reason   string          `json:"reason"`
}

type caseContent struct {
	ExpectedOutput       json.RawMessage `json:"expected_output"`
	ToolEvaluationMode   string          `json:"tool_evaluation_mode"`
	ExpectedToolChain    json.RawMessage `json:"expected_tool_call_chain"`
	EvaluationCriteria   []Criterion     `json:"evaluation_criteria"`
	GenerationAssumption []string        `json:"generation_assumptions"`
}

type storedMatch struct {
	Pass            bool
	CriticalFailure bool
	Score           float64
	TraceTrust      string
	MatchedSteps    []string
	Violations      []struct {
		Code     string
		StepID   string
		Sequence int
		Message  string
	}
}

type chainMatch struct {
	Pass            bool     `json:"pass"`
	CriticalFailure bool     `json:"critical_failure"`
	MatchedSteps    []string `json:"matched_steps"`
	Violations      []string `json:"violations"`
	Score           float64  `json:"score"`
	TraceTrust      string   `json:"trace_trust"`
}

func orNull(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return raw
}

func asObject(raw json.RawMessage) json.RawMessage {
	trimmed := raw
	for len(trimmed) > 0 && (trimmed[0] == ' ' || trimmed[0] == '\n' || trimmed[0] == '\t' || trimmed[0] == '\r') {
		trimmed = trimmed[1:]
	}
	if len(trimmed) > 0 && trimmed[0] == '{' {
		return raw
	}
	wrapped, _ := json.Marshal(map[string]json.RawMessage{"value": orNull(raw)})
	return wrapped
}

// BuildRequest assembles the Judge request from stored Run evidence without truncating any field.
func BuildRequest(input RequestInput) (Prepared, error) {
	var content caseContent
	if err := json.Unmarshal(input.CaseJSON, &content); err != nil {
		return Prepared{}, fmt.Errorf("decode test case content: %w", err)
	}
	var stored []storedCheck
	if len(input.Deterministic) > 0 {
		if err := json.Unmarshal(input.Deterministic, &stored); err != nil {
			return Prepared{}, fmt.Errorf("decode deterministic results: %w", err)
		}
	}
	checks := make([]DeterministicCheck, 0, len(stored))
	results := make([]map[string]any, 0, len(stored))
	var match *chainMatch
	for _, check := range stored {
		checks = append(checks, DeterministicCheck{ID: check.ID, Category: check.Category, Critical: check.Critical, Weight: check.Weight,
			Pass: check.Pass, Expected: orNull(check.Expected), Actual: orNull(check.Actual), Reason: check.Reason})
		results = append(results, map[string]any{"id": check.ID, "category": check.Category, "critical": check.Critical, "weight": check.Weight,
			"pass": check.Pass, "expected": orNull(check.Expected), "actual": orNull(check.Actual), "reason": check.Reason})
		if check.ID == toolCheckID {
			var report storedMatch
			if err := json.Unmarshal(check.Actual, &report); err != nil {
				return Prepared{}, fmt.Errorf("decode tool chain match: %w", err)
			}
			mapped := chainMatch{Pass: report.Pass, CriticalFailure: report.CriticalFailure, Score: report.Score, TraceTrust: report.TraceTrust,
				MatchedSteps: append([]string{}, report.MatchedSteps...), Violations: []string{}}
			for _, violation := range report.Violations {
				mapped.Violations = append(mapped.Violations, fmt.Sprintf("%s (step %q, call %d): %s", violation.Code, violation.StepID, violation.Sequence, violation.Message))
			}
			match = &mapped
		}
	}

	var expectedChain, matchJSON json.RawMessage = json.RawMessage("null"), json.RawMessage("null")
	if content.ToolEvaluationMode == ToolModeChain {
		if match == nil || len(content.ExpectedToolChain) == 0 {
			return Prepared{}, errors.New("deterministic tool chain evidence is missing")
		}
		expectedChain = content.ExpectedToolChain
		encoded, err := json.Marshal(match)
		if err != nil {
			return Prepared{}, err
		}
		matchJSON = encoded
	}

	calls := make([]map[string]any, 0, len(input.ToolCalls))
	transcript := make([]map[string]any, 0, 2*len(input.ToolCalls)+2)
	add := func(kind string, summary any) {
		transcript = append(transcript, map[string]any{"sequence": len(transcript) + 1, "type": kind, "summary": summary})
	}
	for _, call := range input.ToolCalls {
		summary := call.Result
		if call.Status == evidence.StatusError {
			summary = call.Error
		}
		calls = append(calls, map[string]any{"sequence": call.Sequence, "call_id": call.CallID, "tool_name": call.ToolName,
			"arguments": asObject(call.Arguments), "status": string(call.Status), "result_summary": asObject(summary), "trace_source": string(call.Source)})
		add("TOOL_CALL", map[string]any{"call_id": call.CallID, "tool_name": call.ToolName, "arguments": asObject(call.Arguments)})
		add("TOOL_RESULT", map[string]any{"call_id": call.CallID, "status": string(call.Status), "result": asObject(summary)})
	}
	if input.FinalOutput != nil {
		add("OUTPUT_TEXT", map[string]any{"text": *input.FinalOutput})
	}
	if input.Outcome != "COMPLETED" {
		add("ERROR", map[string]any{"execution_outcome": input.Outcome})
	}
	assumptions := content.GenerationAssumption
	if assumptions == nil {
		assumptions = []string{}
	}
	request := map[string]any{
		"schema_version": "1.0", "execution_outcome": input.Outcome, "prompt": input.Prompt,
		"expected_output": content.ExpectedOutput, "actual_output": input.FinalOutput, "generation_assumptions": assumptions,
		"deterministic_results": results, "tool_evaluation_mode": content.ToolEvaluationMode,
		"expected_tool_call_chain": expectedChain, "actual_tool_calls": calls, "tool_chain_match": matchJSON,
		"session_transcript": transcript, "evaluation_criteria": content.EvaluationCriteria,
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return Prepared{}, fmt.Errorf("encode Judge request: %w", err)
	}
	return Prepared{RequestJSON: encoded, Criteria: content.EvaluationCriteria, Checks: checks}, nil
}
