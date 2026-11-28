package evaluation

import (
	"encoding/json"
	"testing"

	"agent-test-platform/contracts"
)

func TestBuildRequestMatchesJudgeContract(t *testing.T) {
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	caseJSON := `{"turns":[{"turn":1,"prompt":"hello","expected_output":{"type":"EXACT","value":"ok"},
		"evaluation_criteria":[{"name":"correct","description":"answer is right","weight":1}]}],"generation_assumptions":[]}`
	sessionContext := []json.RawMessage{
		json.RawMessage(`{"sequence":1,"type":"MESSAGE","role":"user","content":"hello","created_at":"2026-09-15T07:00:00Z"}`),
		json.RawMessage(`{"sequence":2,"type":"MESSAGE","role":"assistant","content":"done","created_at":"2026-09-15T07:00:01Z"}`),
	}
	prepared, err := BuildRequest(RequestInput{Outcome: "COMPLETED", TurnNo: 1, TotalTurns: 1, CaseJSON: json.RawMessage(caseJSON),
		SessionContext: sessionContext})
	if err != nil {
		t.Fatal(err)
	}
	if err := validator.ValidateJSON(judgeRequestSchemaPath, prepared.RequestJSON); err != nil {
		t.Fatalf("request violates contract: %v\n%s", err, prepared.RequestJSON)
	}
	if len(prepared.Criteria) != 1 {
		t.Fatalf("unexpected prepared inputs: %+v", prepared)
	}
}
