package mockllm

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-test-platform/contracts"
	"agent-test-platform/internal/validation"
)

func TestScriptedDatasetPassesSchemaAndSQLValidation(t *testing.T) {
	t.Parallel()
	raw, err := scriptedDataset()
	if err != nil {
		t.Fatal(err)
	}
	var dataset struct {
		SchemaSQL string `json:"schema_sql"`
		SeedSQL   string `json:"seed_sql"`
	}
	if err := json.Unmarshal(raw, &dataset); err != nil {
		t.Fatal(err)
	}
	limits := validation.ScriptLimits{MaxStatementBytes: 4096, MaxScriptBytes: 262144, MaxTables: 20, MaxRowsPerStatement: 1000, MaxTotalRows: 10000}
	if _, err := validation.ValidateSQLScript(dataset.SchemaSQL, validation.SQLSchema, limits); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if _, err := validation.ValidateSQLScript(dataset.SeedSQL, validation.SQLSeed, limits); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestScriptedTestCaseMatchesContract(t *testing.T) {
	t.Parallel()
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	user := `job_id="JOB1" scenario_revision_id="SC1" dataset_revision_id="DS1"`
	for _, mode := range []string{"ANSWER", "TASK"} {
		system := `agent_mode must be "` + mode + `"; more`
		messages := []decodedMessage{{Role: "system", Content: system}, {Role: "user", Content: user}}
		name, arguments, ok, err := scriptedTestCase(messages)
		if err != nil || !ok || name != "create_test_case" {
			t.Fatalf("%s: %v %v %s", mode, err, ok, name)
		}
		encoded, _ := json.Marshal(arguments)
		if err := validator.ValidateJSON("schemas/create-test-case.schema.json", encoded); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if !strings.Contains(string(encoded), "O1") {
			t.Fatalf("%s: first candidate should target order O1: %s", mode, encoded)
		}
	}
}
