package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"agent-test-platform/contracts"
	"agent-test-platform/internal/generation"
	"agent-test-platform/internal/llm"
)

type fakeDatasetGateway struct {
	response llm.CompletionResponse
	err      error
	alias    string
	request  llm.CompletionRequest
}

func (gateway *fakeDatasetGateway) Complete(_ context.Context, alias string, request llm.CompletionRequest) (llm.CompletionResponse, error) {
	gateway.alias, gateway.request = alias, request
	return gateway.response, gateway.err
}

const validDatasetDraft = `{
  "schema_sql": "CREATE TABLE customers (id VARCHAR(32) PRIMARY KEY, name VARCHAR(128) NOT NULL); CREATE TABLE orders (id VARCHAR(32) PRIMARY KEY, customer_id VARCHAR(32) NOT NULL, status VARCHAR(32) NOT NULL, updated_at DATETIME(6) NOT NULL, CONSTRAINT fk_orders_customer FOREIGN KEY (customer_id) REFERENCES customers (id));",
  "seed_sql": "INSERT INTO customers (id, name) VALUES ('C1', 'Ada'); INSERT INTO orders (id, customer_id, status, updated_at) VALUES ('O10001', 'C1', 'PAID', '2026-09-11 09:00:00.000000');",
  "constraints": {
    "version": "1.0",
    "checks": [
      {
        "id": "cancelled-order-has-no-active-shipment",
        "phases": ["AFTER_SEED"],
        "query": "SELECT id FROM orders WHERE status = 'CANCELLED' AND id NOT IN ('O10001')",
        "expectation": {"type": "ROW_COUNT", "equals": 0},
        "critical": true,
        "rule_id": "cancel-paid-order"
      }
    ]
  },
  "difficulty_profile": {
    "data_complexity": 2, "reasoning_complexity": 3, "tool_complexity": 3,
    "ambiguity": 1, "multi_step": 3, "state_change_complexity": 3, "adversarial_complexity": 1
  },
  "coverage_tags": ["NORMAL", "EDGE_CASE", "AMBIGUOUS"]
}`

func TestDatasetStageGeneratesValidatedDraft(t *testing.T) {
	t.Parallel()

	store := datasetFakeStore(t)
	gateway := &fakeDatasetGateway{response: llm.CompletionResponse{
		ProviderRequestID: "provider-request", Model: llm.ModelRef{Provider: llm.ProviderOpenAI, Model: "gpt-test"},
		StopReason: "stop", Content: validDatasetDraft, RawDigest: strings.Repeat("d", 64),
		Usage: llm.TokenUsage{InputTokens: 100, OutputTokens: 200, Available: true},
	}}
	processor := newDatasetProcessor(t, store, gateway, scenarioPolicy())
	result, err := processor.Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusGeneratingDataset, Attempt: 1, Lease: validLease(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Failure != nil || result.Transition.Event != generation.EventDatasetStored {
		t.Fatalf("result = %#v", result)
	}
	if store.saved == nil || store.saved.Kind != generation.ArtifactKindDataset || store.saved.Key != datasetArtifactKey || len(store.saved.SHA256) != 64 {
		t.Fatalf("draft = %#v", store.saved)
	}
	if gateway.alias != "generation-primary" || len(gateway.request.ResponseSchema) == 0 {
		t.Fatalf("alias=%s request=%#v", gateway.alias, gateway.request)
	}

	var assembled struct {
		SchemaSQL   string          `json:"schema_sql"`
		SeedSQL     string          `json:"seed_sql"`
		Constraints json.RawMessage `json:"constraints"`
		Metadata    struct {
			SchemaVersion string           `json:"schema_version"`
			MySQLVersion  string           `json:"mysql_version"`
			Timezone      string           `json:"timezone"`
			Charset       string           `json:"charset"`
			Tables        []map[string]any `json:"tables"`
			ContentSHA256 string           `json:"content_sha256"`
			CoverageTags  []string         `json:"coverage_tags"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(store.saved.ContentJSON, &assembled); err != nil {
		t.Fatal(err)
	}
	if assembled.Metadata.Timezone != "UTC" || assembled.Metadata.Charset != "utf8mb4" || assembled.Metadata.MySQLVersion != "8.4" {
		t.Fatalf("metadata = %#v", assembled.Metadata)
	}
	if len(assembled.Metadata.Tables) != 2 || len(assembled.Metadata.ContentSHA256) != 64 {
		t.Fatalf("tables = %#v", assembled.Metadata.Tables)
	}
	var orders map[string]any
	for _, table := range assembled.Metadata.Tables {
		if table["name"] == "orders" {
			orders = table
		}
	}
	if orders == nil || orders["row_count"] != float64(1) {
		t.Fatalf("orders table = %#v", orders)
	}
}

func TestDatasetStageRejectsUndefinedForeignKeyTable(t *testing.T) {
	t.Parallel()

	document := mustDecodeDatasetDraft(t)
	document["schema_sql"] = "CREATE TABLE orders (id VARCHAR(32) PRIMARY KEY, customer_id VARCHAR(32) NOT NULL, status VARCHAR(32) NOT NULL, updated_at DATETIME(6) NOT NULL, CONSTRAINT fk_orders_customer FOREIGN KEY (customer_id) REFERENCES customers (id));"
	invalid, _ := json.Marshal(document)
	gateway := &fakeDatasetGateway{response: llm.CompletionResponse{StopReason: "stop", Content: string(invalid)}}
	result, err := newDatasetProcessor(t, datasetFakeStore(t), gateway, scenarioPolicy()).Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusGeneratingDataset, Attempt: 1, Lease: validLease(),
	})
	if err != nil || result.Failure == nil || result.Failure.Code != "DATASET_CONSTRAINT_MISMATCH" {
		t.Fatalf("result=%#v error=%v", result, err)
	}
}

func TestDatasetStageRejectsInvalidSchemaSQL(t *testing.T) {
	t.Parallel()

	document := mustDecodeDatasetDraft(t)
	document["schema_sql"] = "CREATE TABLE orders (id VARCHAR(32))" // no primary key
	invalid, _ := json.Marshal(document)
	gateway := &fakeDatasetGateway{response: llm.CompletionResponse{StopReason: "stop", Content: string(invalid)}}
	result, err := newDatasetProcessor(t, datasetFakeStore(t), gateway, scenarioPolicy()).Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusGeneratingDataset, Attempt: 1, Lease: validLease(),
	})
	if err != nil || result.Failure == nil || result.Failure.Code != "DATASET_SCHEMA_SQL_INVALID" {
		t.Fatalf("result=%#v error=%v", result, err)
	}
}

func TestDatasetStageRejectsInvalidDifficultyProfile(t *testing.T) {
	t.Parallel()

	document := mustDecodeDatasetDraft(t)
	profile := document["difficulty_profile"].(map[string]any)
	profile["data_complexity"] = 9
	invalid, _ := json.Marshal(document)
	gateway := &fakeDatasetGateway{response: llm.CompletionResponse{StopReason: "stop", Content: string(invalid)}}
	result, err := newDatasetProcessor(t, datasetFakeStore(t), gateway, scenarioPolicy()).Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusGeneratingDataset, Attempt: 1, Lease: validLease(),
	})
	if err != nil || result.Failure == nil || result.Failure.Code != "DATASET_CONSTRAINT_MISMATCH" {
		t.Fatalf("result=%#v error=%v", result, err)
	}
}

func TestDatasetStagePropagatesGatewayFailure(t *testing.T) {
	t.Parallel()

	gatewayError := errors.New("provider unavailable")
	processor := newDatasetProcessor(t, datasetFakeStore(t), &fakeDatasetGateway{err: gatewayError}, scenarioPolicy())
	_, err := processor.Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusGeneratingDataset, Attempt: 1, Lease: validLease(),
	})
	if !errors.Is(err, gatewayError) {
		t.Fatalf("error = %v", err)
	}
}

func mustDecodeDatasetDraft(t *testing.T) map[string]any {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal([]byte(validDatasetDraft), &document); err != nil {
		t.Fatal(err)
	}
	return document
}

func datasetFakeStore(t *testing.T) *fakeStore {
	t.Helper()
	requestArtifact := normalizedArtifact(t)
	scenario, err := contracts.Files.ReadFile("examples/scenario.json")
	if err != nil {
		t.Fatal(err)
	}
	canonicalScenario, scenarioDigest, err := generation.CanonicalJSON(scenario)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeStore{
		artifact:       requestArtifact,
		artifactsByKey: map[string]generation.ArtifactSnapshot{},
		artifactsByKind: map[string]generation.ArtifactSnapshot{
			generation.ArtifactKindNormalizedRequest: requestArtifact,
			generation.ArtifactKindScenario: {
				Kind: generation.ArtifactKindScenario, Key: scenarioArtifactKey,
				ContentJSON: canonicalScenario, SHA256: scenarioDigest,
			},
		},
	}
}

func newDatasetProcessor(t *testing.T, store Store, gateway DatasetGateway, policy llm.CompactionPolicy) *Processor {
	t.Helper()
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	processor, err := New(store, validator, WithDatasetGeneration(gateway, DatasetConfig{
		ModelAlias: "generation-primary", MaxOutputTokens: 4_096, CompactionPolicy: policy,
	}))
	if err != nil {
		t.Fatal(err)
	}
	return processor
}
