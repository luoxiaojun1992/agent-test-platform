package generation

import (
	"encoding/json"
	"fmt"
)

// ManifestSchema is the normative contract every Generation Manifest must satisfy.
const ManifestSchema = "schemas/generation-manifest.schema.json"

// ManifestStage is one Job of a completed Generation Pipeline together with the provenance its
// publish recorded (see Job.Provenance). Stages are always SCENARIO, DATASET, TEST_CASES in order.
type ManifestStage struct {
	JobID      string          `json:"job_id"`
	JobKind    JobKind         `json:"job_kind"`
	Provenance json.RawMessage `json:"provenance"`
}

// ManifestRevision names one published Revision and the content digest it was published with.
type ManifestRevision struct {
	RevisionID string `json:"revision_id"`
	SHA256     string `json:"sha256"`
}

// ManifestTestCase is one published Test Case Revision. Ordinal starts at 1 in acceptance order.
type ManifestTestCase struct {
	Ordinal    int    `json:"ordinal"`
	RevisionID string `json:"revision_id"`
	SHA256     string `json:"sha256"`
}

// ManifestInput is everything a Generation Manifest records. Every field comes from a durable row
// (the originating request, the Jobs' published provenance, and the published Revisions), never
// from staging drafts, which expire.
type ManifestInput struct {
	PipelineID            string
	RequestSHA256         string
	DescriptionTokens     int
	RequestConfig         json.RawMessage
	GeneratorConfigSHA256 string
	Stages                []ManifestStage
	Scenario              ManifestRevision
	Dataset               ManifestRevision
	TestCases             []ManifestTestCase
}

// Package is the immutable result of a completed Generation Pipeline as served by
// GET /generation-jobs/{id}/package (contracts/openapi.yaml GenerationPackage).
type Package struct {
	JobID               string          `json:"job_id"`
	ScenarioRevisionID  string          `json:"scenario_revision_id"`
	DatasetRevisionID   string          `json:"dataset_revision_id"`
	TestCaseRevisionIDs []string        `json:"test_case_revision_ids"`
	Manifest            json.RawMessage `json:"manifest"`
	ManifestSHA256      string          `json:"manifest_sha256"`
}

type manifestRequest struct {
	RequestSHA256     string          `json:"request_sha256"`
	DescriptionTokens int             `json:"description_tokens"`
	TokenizerVersion  string          `json:"tokenizer_version"`
	Config            json.RawMessage `json:"config"`
}

type manifestArtifacts struct {
	Scenario  ManifestRevision   `json:"scenario"`
	Dataset   ManifestRevision   `json:"dataset"`
	TestCases []ManifestTestCase `json:"test_cases"`
}

type manifestDocument struct {
	SchemaVersion         string            `json:"schema_version"`
	PipelineID            string            `json:"pipeline_id"`
	Request               manifestRequest   `json:"request"`
	GeneratorConfigSHA256 string            `json:"generator_config_sha256"`
	Stages                []ManifestStage   `json:"stages"`
	Artifacts             manifestArtifacts `json:"artifacts"`
}

// BuildGenerationManifest assembles the canonical Generation Manifest and its SHA-256 digest
// (docs/architecture.md section 6.3). It carries no timestamps and no retry state, so publishing
// the same Pipeline again reproduces the same bytes and digest.
func BuildGenerationManifest(input ManifestInput) (json.RawMessage, string, error) {
	if len(input.Stages) != 3 {
		return nil, "", fmt.Errorf("manifest requires exactly three pipeline stages, got %d", len(input.Stages))
	}
	expectedKinds := []JobKind{KindScenario, KindDataset, KindTestCases}
	for index, stage := range input.Stages {
		if stage.JobKind != expectedKinds[index] {
			return nil, "", fmt.Errorf("manifest stage %d is %s, want %s", index, stage.JobKind, expectedKinds[index])
		}
	}
	if len(input.TestCases) < 1 || len(input.TestCases) > 20 {
		return nil, "", fmt.Errorf("manifest requires 1-20 test cases, got %d", len(input.TestCases))
	}
	for index, testCase := range input.TestCases {
		if testCase.Ordinal != index+1 {
			return nil, "", fmt.Errorf("manifest test case %d has ordinal %d", index, testCase.Ordinal)
		}
	}
	document := manifestDocument{
		SchemaVersion: "1.0",
		PipelineID:    input.PipelineID,
		Request: manifestRequest{
			RequestSHA256: input.RequestSHA256, DescriptionTokens: input.DescriptionTokens,
			TokenizerVersion: DescriptionTokenizerVersion, Config: input.RequestConfig,
		},
		GeneratorConfigSHA256: input.GeneratorConfigSHA256,
		Stages:                input.Stages,
		Artifacts:             manifestArtifacts{Scenario: input.Scenario, Dataset: input.Dataset, TestCases: input.TestCases},
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, "", fmt.Errorf("encode generation manifest: %w", err)
	}
	return CanonicalJSON(encoded)
}
