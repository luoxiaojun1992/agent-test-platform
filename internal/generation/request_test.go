package generation

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestNormalizeRequestAppliesOnlyDeterministicDefaults(t *testing.T) {
	t.Parallel()

	normalized, err := NormalizeRequest([]byte(`{"validation_db_ids":["0b6d6f3e-2c4a-4f8e-9a1b-7c5d3e2f1a90"],"description":"Generate cancellation tests."}`))
	if err != nil {
		t.Fatal(err)
	}
	if normalized.AgentMode != "TASK" || normalized.TestCaseCount != 10 || !normalized.AutoActivate {
		t.Fatalf("defaults = %#v", normalized)
	}
	if !normalized.GenerateDataset || normalized.DatabaseProfileID != nil || normalized.TargetAgentID != nil {
		t.Fatalf("target defaults = %#v", normalized)
	}
	if normalized.Domain != nil || normalized.Capabilities != nil || normalized.Difficulty != nil || normalized.Coverage != nil {
		t.Fatalf("AI-inferred fields were defaulted: %#v", normalized)
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(encoded, &object); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"target_agent_id", "database_profile_id", "domain", "capabilities", "difficulty", "coverage"} {
		if value, exists := object[field]; !exists || value != nil {
			t.Fatalf("%s = %#v, want explicit null", field, value)
		}
	}
}

func TestNormalizeRequestRequiresProfileWhenDatasetGenerationDisabled(t *testing.T) {
	t.Parallel()

	profileID := "01K4Z6Y7X8W9V0T1S2R3Q4P5N6"
	normalized, err := NormalizeRequest([]byte(`{"validation_db_ids":["0b6d6f3e-2c4a-4f8e-9a1b-7c5d3e2f1a90"],"description":"Use the configured fixture.","generate_dataset":false,"database_profile_id":"` + profileID + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	if normalized.GenerateDataset || normalized.DatabaseProfileID == nil || *normalized.DatabaseProfileID != profileID {
		t.Fatalf("normalized = %#v", normalized)
	}

	if _, err := NormalizeRequest([]byte(`{"validation_db_ids":["0b6d6f3e-2c4a-4f8e-9a1b-7c5d3e2f1a90"],"description":"Use the configured fixture.","generate_dataset":false}`)); err == nil {
		t.Fatal("missing Database Profile was accepted")
	}
	if _, err := NormalizeRequest([]byte(`{"validation_db_ids":["0b6d6f3e-2c4a-4f8e-9a1b-7c5d3e2f1a90"],"description":"Generate a fixture.","database_profile_id":"` + profileID + `"}`)); err == nil {
		t.Fatal("unused Database Profile was accepted")
	}
}

func TestNormalizeRequestSortsSetsAndPreservesExplicitEmpty(t *testing.T) {
	t.Parallel()

	normalized, err := NormalizeRequest([]byte(`{
		"validation_db_ids": ["0b6d6f3e-2c4a-4f8e-9a1b-7c5d3e2f1a90"],
		"description":"Generate tests.",
		"capabilities":["MYSQL_QUERY","HTTP"],
		"coverage":[],
		"difficulty":{"min":"MEDIUM","max":"EXTREME"},
		"auto_activate":false
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Capabilities == nil || !reflect.DeepEqual(*normalized.Capabilities, []string{"HTTP", "MYSQL_QUERY"}) {
		t.Fatalf("capabilities = %#v", normalized.Capabilities)
	}
	if normalized.Coverage == nil || len(*normalized.Coverage) != 0 || normalized.AutoActivate {
		t.Fatalf("coverage/activation = %#v/%v", normalized.Coverage, normalized.AutoActivate)
	}
}

func TestNormalizeRequestRejectsSemanticErrors(t *testing.T) {
	t.Parallel()

	for _, document := range []string{
		`{"validation_db_ids":["0b6d6f3e-2c4a-4f8e-9a1b-7c5d3e2f1a90"],"description":"   "}`,
		`{"validation_db_ids":["0b6d6f3e-2c4a-4f8e-9a1b-7c5d3e2f1a90"],"description":"valid","difficulty":{"min":"HARD","max":"EASY"}}`,
		`{"validation_db_ids":["0b6d6f3e-2c4a-4f8e-9a1b-7c5d3e2f1a90"],"description":"valid","capabilities":["MYSQL_QUERY","MYSQL_QUERY"]}`,
	} {
		if _, err := NormalizeRequest([]byte(document)); err == nil {
			t.Fatalf("invalid request accepted: %s", document)
		}
	}
}

func TestNormalizeRequestAcceptsRoutingFieldsAndKindSpecificTargets(t *testing.T) {
	t.Parallel()

	const scenario = "01HZZZZZZZZZZZZZZZZZZZZZZ1"
	for name, test := range map[string]struct {
		document string
		valid    bool
	}{
		"scenario without targets":    {`{"description":"d","job_kind":"SCENARIO"}`, true},
		"test cases without targets":  {`{"description":"d","job_kind":"TEST_CASES","input_scenario_revision_id":"` + scenario + `","input_dataset_revision_id":"` + scenario + `"}`, true},
		"dataset needs a target":      {`{"description":"d","job_kind":"DATASET","input_scenario_revision_id":"` + scenario + `"}`, false},
		"full package needs a target": {`{"description":"d"}`, false},
	} {
		_, err := NormalizeRequest([]byte(test.document))
		if (err == nil) != test.valid {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
