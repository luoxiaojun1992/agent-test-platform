package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agent-test-platform/contracts"
	"agent-test-platform/internal/generation"
	"agent-test-platform/internal/llm"

	"github.com/DATA-DOG/go-sqlmock"
)

const (
	testCaseScenarioID   = "01TESTSCENARIOID0000000000"
	testCaseDatasetRevID = "01JTESTDATASETREV000000000"
	testCaseDatabaseName = "dataset_01jtestdatasetrev000000000"
)

// fakeTestCaseStore is a Store fake with proper (kind, key) indexing, matching the real
// store's write-once-per-key, idempotent-if-same-digest, conflict-if-different semantics.
// The shared fakeStore in processor_test.go only indexes by kind, which is not enough once
// a job needs many distinct keys of the same kind (one Test Case per client_case_id, one
// manifest per repair round).
type fakeTestCaseStore struct {
	request   generation.RequestSnapshot
	artifacts map[string]map[string]generation.ArtifactSnapshot
	saveErr   error
}

func newFakeTestCaseStore() *fakeTestCaseStore {
	return &fakeTestCaseStore{artifacts: map[string]map[string]generation.ArtifactSnapshot{}}
}

func (store *fakeTestCaseStore) seed(kind, key string, contentJSON []byte, validation []byte) {
	canonical, digest, err := generation.CanonicalJSON(contentJSON)
	if err != nil {
		panic(err)
	}
	if store.artifacts[kind] == nil {
		store.artifacts[kind] = map[string]generation.ArtifactSnapshot{}
	}
	store.artifacts[kind][key] = generation.ArtifactSnapshot{Kind: kind, Key: key, ContentJSON: canonical, SHA256: digest, Validation: validation}
}

func (store *fakeTestCaseStore) LoadGenerationRequest(context.Context, string) (generation.RequestSnapshot, error) {
	return store.request, nil
}

func (store *fakeTestCaseStore) LoadGenerationArtifact(_ context.Context, _ string, kind, key string) (generation.ArtifactSnapshot, error) {
	byKey, ok := store.artifacts[kind]
	if !ok {
		return generation.ArtifactSnapshot{}, generation.ErrNotFound
	}
	snapshot, ok := byKey[key]
	if !ok {
		return generation.ArtifactSnapshot{}, generation.ErrNotFound
	}
	return snapshot, nil
}

func (store *fakeTestCaseStore) SaveGenerationArtifact(_ context.Context, _ string, _ generation.WorkLease, draft generation.ArtifactDraft) error {
	if store.saveErr != nil {
		return store.saveErr
	}
	if byKey, ok := store.artifacts[draft.Kind]; ok {
		if existing, ok := byKey[draft.Key]; ok {
			if existing.SHA256 != draft.SHA256 {
				return generation.ErrConflict
			}
			return nil
		}
	}
	if store.artifacts[draft.Kind] == nil {
		store.artifacts[draft.Kind] = map[string]generation.ArtifactSnapshot{}
	}
	store.artifacts[draft.Kind][draft.Key] = generation.ArtifactSnapshot{
		Kind: draft.Kind, Key: draft.Key, ContentJSON: draft.ContentJSON, SHA256: draft.SHA256, Validation: draft.Validation,
	}
	return nil
}

func (store *fakeTestCaseStore) LoadPublishedScenarioRevision(context.Context, string) (generation.ScenarioRevisionSnapshot, error) {
	return generation.ScenarioRevisionSnapshot{}, generation.ErrNotFound
}

func (store *fakeTestCaseStore) LoadDatabaseProfileConnection(context.Context, string) (generation.DatabaseProfileConnection, error) {
	return generation.DatabaseProfileConnection{}, generation.ErrNotFound
}

func (store *fakeTestCaseStore) LoadPublishedDatasetRevision(context.Context, string) (generation.DatasetRevisionSnapshot, error) {
	return generation.DatasetRevisionSnapshot{}, generation.ErrNotFound
}

type fakeTestCaseGateway struct {
	responses []llm.CompletionResponse
	requests  []llm.CompletionRequest
	err       error
}

func (gateway *fakeTestCaseGateway) Complete(_ context.Context, _ string, request llm.CompletionRequest) (llm.CompletionResponse, error) {
	gateway.requests = append(gateway.requests, request)
	if gateway.err != nil {
		return llm.CompletionResponse{}, gateway.err
	}
	if len(gateway.responses) == 0 {
		return llm.CompletionResponse{StopReason: "stop", Content: "no more candidates"}, nil
	}
	response := gateway.responses[0]
	gateway.responses = gateway.responses[1:]
	return response, nil
}

func toolCallResponse(callID string, arguments []byte) llm.CompletionResponse {
	return llm.CompletionResponse{
		StopReason: "tool_calls",
		ToolCalls:  []llm.ModelToolCall{{ID: callID, Name: testCaseToolName, Arguments: arguments}},
	}
}

// candidateFixture returns the canonical create_test_case example, adapted to this test
// harness's fixed job/scenario/dataset IDs and SESSION_LLM_ONLY mode (no Tool Catalog).
func candidateFixture(t *testing.T, clientCaseID string) []byte {
	t.Helper()
	raw, err := contracts.Files.ReadFile("examples/create-test-case.json")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	document["job_id"] = pipelineJobID
	document["client_case_id"] = clientCaseID
	document["scenario_revision_id"] = testCaseScenarioID
	document["dataset_revision_id"] = testCaseDatasetRevID
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func newTestCaseHarness(t *testing.T, requestedCount int) (*fakeTestCaseStore, *fakeValidationDB, sqlmock.Sqlmock) {
	t.Helper()
	store := newFakeTestCaseStore()

	request := []byte(`{"schema_version":"1.1","validation_db_ids":["0b6d6f3e-2c4a-4f8e-9a1b-7c5d3e2f1a90"],"description":"Generate order cancellation tests.","target_agent_id":null,"generate_dataset":true,"database_profile_id":null,"domain":"e-commerce","capabilities":["MYSQL_QUERY","MYSQL_EXECUTE"],"difficulty":{"min":"MEDIUM","max":"HARD"},"test_case_count":` + itoa(requestedCount) + `,"coverage":["NORMAL"],"auto_activate":true}`)
	canonicalRequest, requestDigest, err := generation.CanonicalJSON(request)
	if err != nil {
		t.Fatal(err)
	}
	store.request = generation.RequestSnapshot{Document: canonicalRequest, SHA256: requestDigest, RequestedTestCaseCount: requestedCount}
	store.seed(generation.ArtifactKindNormalizedRequest, normalizedRequestKey, canonicalRequest, nil)

	scenario, err := contracts.Files.ReadFile("examples/scenario.json")
	if err != nil {
		t.Fatal(err)
	}
	store.seed(generation.ArtifactKindScenario, scenarioArtifactKey, scenario, nil)
	store.seed(generation.ArtifactKindScenarioID, scenarioIDKey, []byte(`{"id":"`+testCaseScenarioID+`"}`), nil)

	dataset := []byte(`{
		"schema_sql": "CREATE TABLE orders (id VARCHAR(32) PRIMARY KEY, status VARCHAR(32) NOT NULL, updated_at DATETIME(6) NOT NULL)",
		"seed_sql": "INSERT INTO orders (id, status, updated_at) VALUES ('O10001', 'PAID', '2026-09-11 09:00:00.000000')",
		"constraints": {"version": "1.0", "checks": []},
		"metadata": {
			"schema_version": "1.0", "mysql_version": "8.4", "timezone": "UTC", "charset": "utf8mb4",
			"collation": "utf8mb4_0900_ai_ci", "sql_mode": ["STRICT_TRANS_TABLES"],
			"tables": [{"name": "orders", "columns": [
				{"name": "id", "mysql_type": "VARCHAR(32)", "nullable": false},
				{"name": "status", "mysql_type": "VARCHAR(32)", "nullable": false},
				{"name": "updated_at", "mysql_type": "DATETIME(6)", "nullable": false}
			], "primary_key": ["id"], "foreign_keys": [], "row_count": 1}],
			"difficulty_profile": {"data_complexity":1,"reasoning_complexity":1,"tool_complexity":1,"ambiguity":1,"multi_step":1,"state_change_complexity":1,"adversarial_complexity":1},
			"coverage_tags": ["NORMAL"],
			"content_sha256": "` + strings.Repeat("a", 64) + `"
		}
	}`)
	store.seed(generation.ArtifactKindDataset, datasetArtifactKey, dataset, nil)
	store.seed(generation.ArtifactKindDatasetExecution, datasetExecutionKey, []byte(`{"dataset_revision_id":"`+testCaseDatasetRevID+`","targets":[{"validation_db_id":"0b6d6f3e-2c4a-4f8e-9a1b-7c5d3e2f1a90","database":"`+testCaseDatabaseName+`"}]}`), nil)

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	target := &fakeValidationDB{loader: database}
	return store, target, mock
}

func itoa(value int) string {
	digits, err := json.Marshal(value)
	if err != nil {
		return "0"
	}
	return string(digits)
}

func newTestCaseProcessor(t *testing.T, store Store, gateway TestCaseGateway, target ValidationDB) *Processor {
	t.Helper()
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	processor, err := New(store, validator,
		WithTestCaseGeneration(gateway, TestCaseConfig{
			ModelAlias: "generation-primary", MaxOutputTokens: 4_096, CompactionPolicy: scenarioPolicy(),
		}),
		WithValidationDB(singleTarget{target}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return processor
}

func TestGenerateTestCasesAcceptsValidCandidate(t *testing.T) {
	t.Parallel()

	store, target, mock := newTestCaseHarness(t, 1)

	gateway := &fakeTestCaseGateway{responses: []llm.CompletionResponse{toolCallResponse("call-1", candidateFixture(t, "cancel-paid-order"))}}
	processor := newTestCaseProcessor(t, store, gateway, target)
	result, err := processor.Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusGeneratingTestCases, RepairRound: 0, Attempt: 1, Lease: validLease(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Failure != nil || result.Transition.Event != generation.EventCandidatesGenerated {
		t.Fatalf("result = %#v", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	manifest, err := processor.loadTestCaseManifest(context.Background(), pipelineJobID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Accepted) != 1 || manifest.Accepted[0] != "cancel-paid-order" || len(manifest.Rejected) != 0 {
		t.Fatalf("manifest = %#v", manifest)
	}
	saved, err := store.LoadGenerationArtifact(context.Background(), pipelineJobID, generation.ArtifactKindTestCase, "cancel-paid-order")
	if err != nil || len(saved.Validation) == 0 {
		t.Fatalf("saved candidate = %#v, err = %v", saved, err)
	}
}

func TestGenerateTestCasesRecordsRejectionButStillTransitions(t *testing.T) {
	t.Parallel()

	store, target, _ := newTestCaseHarness(t, 1)
	invalidCandidate := candidateFixture(t, "cancel-paid-order")
	var document map[string]any
	if err := json.Unmarshal(invalidCandidate, &document); err != nil {
		t.Fatal(err)
	}
	document["job_id"] = "different-job"
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}

	gateway := &fakeTestCaseGateway{responses: []llm.CompletionResponse{toolCallResponse("call-1", encoded)}}
	processor := newTestCaseProcessor(t, store, gateway, target)
	result, err := processor.Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusGeneratingTestCases, RepairRound: 0, Attempt: 1, Lease: validLease(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Failure != nil || result.Transition.Event != generation.EventCandidatesGenerated {
		t.Fatalf("result = %#v", result)
	}
	manifest, err := processor.loadTestCaseManifest(context.Background(), pipelineJobID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Accepted) != 0 || len(manifest.Rejected) != 1 || manifest.Rejected[0].ClientCaseID != "cancel-paid-order" {
		t.Fatalf("manifest = %#v", manifest)
	}
	found := false
	for _, validationErr := range manifest.Rejected[0].Errors {
		if validationErr.Code == "REFERENCE_SCOPE_MISMATCH" {
			found = true
		}
	}
	if !found {
		t.Fatalf("errors = %#v", manifest.Rejected[0].Errors)
	}
}
