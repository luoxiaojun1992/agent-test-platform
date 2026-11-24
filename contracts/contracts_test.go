package contracts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"testing"
)

func TestKnowledgeUploadExampleDigestMatchesText(t *testing.T) {
	t.Parallel()

	data, err := Files.ReadFile("examples/target-knowledge-upload-request.json")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Text          string `json:"text"`
		ContentSHA256 string `json:"content_sha256"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(document.Text))
	if hex.EncodeToString(digest[:]) != document.ContentSHA256 {
		t.Fatalf("content_sha256 does not match text")
	}
}

func TestEmbeddedJSONDocumentsParse(t *testing.T) {
	t.Parallel()

	schemas, err := fs.Glob(Files, "schemas/*.json")
	if err != nil {
		t.Fatalf("glob embedded schemas: %v", err)
	}
	examples, err := fs.Glob(Files, "examples/*.json")
	if err != nil {
		t.Fatalf("glob embedded examples: %v", err)
	}
	paths := append(schemas, examples...)
	if len(paths) == 0 {
		t.Fatal("no embedded JSON documents found")
	}

	for _, path := range paths {
		path := path
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			data, err := Files.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}

			var document any
			if err := json.Unmarshal(data, &document); err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
		})
	}
}

func TestEveryExampleHasSchema(t *testing.T) {
	t.Parallel()

	examples, err := fs.Glob(Files, "examples/*.json")
	if err != nil {
		t.Fatalf("glob examples: %v", err)
	}
	if len(examples) != len(ExampleSchemas) {
		t.Fatalf("embedded examples = %d, mappings = %d", len(examples), len(ExampleSchemas))
	}

	for _, example := range examples {
		schema, ok := ExampleSchemas[example]
		if !ok {
			t.Errorf("example %s has no schema mapping", example)
			continue
		}
		if _, err := fs.Stat(Files, schema); err != nil {
			t.Errorf("schema mapping for %s: %v", example, err)
		}
	}
}

func TestSchemasCompile(t *testing.T) {
	t.Parallel()

	schemas, err := CompileSchemas()
	if err != nil {
		t.Fatal(err)
	}
	if len(schemas) == 0 {
		t.Fatal("no schemas compiled")
	}
}

func TestPositiveExamplesValidate(t *testing.T) {
	t.Parallel()

	if err := ValidateExamples(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenAPIValidates(t *testing.T) {
	t.Parallel()

	document, err := LoadOpenAPI(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if document.Info == nil || document.Info.Title == "" {
		t.Fatal("OpenAPI info title is empty")
	}
}

func TestTargetOpenAPIValidates(t *testing.T) {
	t.Parallel()

	document, err := LoadTargetOpenAPI(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if document.Info == nil || document.Info.Title == "" {
		t.Fatal("Target OpenAPI info title is empty")
	}
}

func TestSchemasRejectInvalidDocuments(t *testing.T) {
	t.Parallel()

	schemas, err := CompileSchemas()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		examplePath string
		mutate      func(map[string]any)
	}{
		{
			name:        "unknown generation request field",
			examplePath: "examples/generation-request.json",
			mutate: func(document map[string]any) {
				document["unexpected"] = true
			},
		},
		{
			name:        "test case criterion weight exceeds maximum",
			examplePath: "examples/test-case.json",
			mutate: func(document map[string]any) {
				criteria := document["evaluation_criteria"].([]any)
				criteria[0].(map[string]any)["weight"] = 1.5
			},
		},
		{
			name:        "judge criterion score exceeds maximum",
			examplePath: "examples/judge-result.json",
			mutate: func(document map[string]any) {
				criteria := document["criteria"].([]any)
				criteria[0].(map[string]any)["score"] = float64(101)
			},
		},
		{
			name:        "manifest stages out of order",
			examplePath: "examples/generation-manifest.json",
			mutate: func(document map[string]any) {
				stages := document["stages"].([]any)
				stages[0], stages[1] = stages[1], stages[0]
			},
		},
		{
			name:        "manifest provenance without critic record",
			examplePath: "examples/generation-manifest.json",
			mutate: func(document map[string]any) {
				provenance := document["stages"].([]any)[2].(map[string]any)["provenance"].(map[string]any)
				delete(provenance, "critic")
			},
		},
		{
			name:        "manifest digest is not lowercase hexadecimal",
			examplePath: "examples/generation-manifest.json",
			mutate: func(document map[string]any) {
				artifacts := document["artifacts"].(map[string]any)
				scenario := artifacts["scenario"].(map[string]any)
				scenario["sha256"] = "ABCDEF" + scenario["sha256"].(string)[6:]
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			document := readExampleObject(t, test.examplePath)
			test.mutate(document)
			schemaPath := ExampleSchemas[test.examplePath]
			if err := schemas[schemaPath].Validate(document); err == nil {
				t.Fatalf("invalid document passed %s", schemaPath)
			}
		})
	}
}

func TestValidatorValidatesArbitraryJSON(t *testing.T) {
	t.Parallel()

	validator, err := NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	data, err := Files.ReadFile("examples/target-model-list-response.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := validator.ValidateJSON("schemas/target-model-list-response.schema.json", data); err != nil {
		t.Fatal(err)
	}
	if err := validator.ValidateJSON("schemas/missing.json", data); err == nil {
		t.Fatal("unknown schema was accepted")
	}
}

func readExampleObject(t *testing.T, path string) map[string]any {
	t.Helper()

	document, err := readJSON(path)
	if err != nil {
		t.Fatal(err)
	}
	object, ok := document.(map[string]any)
	if !ok {
		t.Fatalf("example %s root is %T, want object", path, document)
	}
	return object
}

func TestGenerationRequestKindRules(t *testing.T) {
	t.Parallel()

	validator, err := NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	const scenario = "01HZZZZZZZZZZZZZZZZZZZZZZ1"
	const dataset = "01HZZZZZZZZZZZZZZZZZZZZZZ2"
	const target = "0b6d6f3e-2c4a-4f8e-9a1b-7c5d3e2f1a90"
	tests := map[string]struct {
		document string
		valid    bool
	}{
		"full package needs a target":            {`{"description":"d"}`, false},
		"full package with target":               {`{"description":"d","validation_db_ids":["` + target + `"]}`, true},
		"full package rejects inputs":            {`{"description":"d","validation_db_ids":["` + target + `"],"input_scenario_revision_id":"` + scenario + `"}`, false},
		"scenario needs no target":               {`{"description":"d","job_kind":"SCENARIO"}`, true},
		"scenario rejects inputs":                {`{"description":"d","job_kind":"SCENARIO","input_scenario_revision_id":"` + scenario + `"}`, false},
		"dataset needs a scenario":               {`{"description":"d","job_kind":"DATASET","validation_db_ids":["` + target + `"]}`, false},
		"dataset needs a target":                 {`{"description":"d","job_kind":"DATASET","input_scenario_revision_id":"` + scenario + `"}`, false},
		"dataset with scenario and target":       {`{"description":"d","job_kind":"DATASET","input_scenario_revision_id":"` + scenario + `","validation_db_ids":["` + target + `"]}`, true},
		"dataset rejects a dataset input":        {`{"description":"d","job_kind":"DATASET","input_scenario_revision_id":"` + scenario + `","input_dataset_revision_id":"` + dataset + `","validation_db_ids":["` + target + `"]}`, false},
		"test cases need both inputs":            {`{"description":"d","job_kind":"TEST_CASES","input_scenario_revision_id":"` + scenario + `"}`, false},
		"test cases with both inputs, no target": {`{"description":"d","job_kind":"TEST_CASES","input_scenario_revision_id":"` + scenario + `","input_dataset_revision_id":"` + dataset + `"}`, true},
		"unknown kind":                           {`{"description":"d","job_kind":"OTHER"}`, false},
	}
	for name, test := range tests {
		err := validator.ValidateJSON("schemas/generation-request.schema.json", []byte(test.document))
		if (err == nil) != test.valid {
			t.Errorf("%s: valid=%v err=%v", name, test.valid, err)
		}
	}
}
