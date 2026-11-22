package evaluation

import (
	"encoding/json"
	"testing"

	"agent-test-platform/contracts"
	"agent-test-platform/internal/evidence"
)

func TestBuildRequestMatchesJudgeContract(t *testing.T) {
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	output := "done"
	caseJSON := `{"expected_output":{"type":"EXACT","value":"ok"},"tool_evaluation_mode":"SESSION_LLM_ONLY","expected_tool_call_chain":null,
		"evaluation_criteria":[{"name":"correct","description":"answer is right","weight":1}],"generation_assumptions":[]}`
	deterministic := `[{"id":"state_diff","category":"STATE","critical":true,"weight":1,"pass":true,"expected":{},"actual":{}}]`
	prepared, err := BuildRequest(RequestInput{Outcome: "COMPLETED", Prompt: "hello", CaseJSON: json.RawMessage(caseJSON), FinalOutput: &output,
		Deterministic: json.RawMessage(deterministic), ToolCalls: []evidence.Record{{Sequence: 1, CallID: "c1", ToolName: "query_db",
			Arguments: json.RawMessage(`{"q":1}`), Result: json.RawMessage(`[1]`), Status: evidence.StatusSuccess, Source: evidence.SourceAgentReported}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := validator.ValidateJSON(judgeRequestSchemaPath, prepared.RequestJSON); err != nil {
		t.Fatalf("request violates contract: %v\n%s", err, prepared.RequestJSON)
	}
	if len(prepared.Checks) != 1 || len(prepared.Criteria) != 1 {
		t.Fatalf("unexpected prepared inputs: %+v", prepared)
	}
}
