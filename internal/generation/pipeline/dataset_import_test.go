package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"agent-test-platform/contracts"
	"agent-test-platform/internal/generation"
	"agent-test-platform/internal/llm"
)

const (
	importedProfileID   = "01K4Z6Y7X8W9V0T1S2R3Q4P5NC"
	importedSchemaSQL   = "CREATE TABLE orders (id VARCHAR(32) PRIMARY KEY, status VARCHAR(32) NOT NULL)"
	importedSeedSQL     = "INSERT INTO orders (id, status) VALUES ('O-REF-1', 'SNAPSHOT-ROW-VALUE-77')"
	businessFieldsReply = `{
  "constraints": {"version": "1.0", "checks": []},
  "difficulty_profile": {"data_complexity": 2, "reasoning_complexity": 2, "tool_complexity": 2, "ambiguity": 1, "multi_step": 1, "state_change_complexity": 1, "adversarial_complexity": 1},
  "coverage_tags": ["NORMAL"]
}`
)

type fakeSnapshotSource struct {
	snapshot generation.DatasetSnapshot
	err      error
	profile  generation.DatabaseProfileConnection
	calls    int
}

func (source *fakeSnapshotSource) Snapshot(_ context.Context, profile generation.DatabaseProfileConnection) (generation.DatasetSnapshot, error) {
	source.calls++
	source.profile = profile
	return source.snapshot, source.err
}

func importedProfile() generation.DatabaseProfileConnection {
	return generation.DatabaseProfileConnection{
		ID: importedProfileID, ConfigSHA256: strings.Repeat("c", 64), Host: "reference", Port: 3306,
		Database: "reference", Username: "atp_reader", PasswordSecretRef: "reference-password",
		TLSMode: "VERIFY_IDENTITY", SnapshotMode: "SCHEMA_AND_DATA", SnapshotTimeoutSeconds: 60,
	}
}

func canonicalArtifact(t *testing.T, kind, key string, document []byte) generation.ArtifactSnapshot {
	t.Helper()
	canonical, digest, err := generation.CanonicalJSON(document)
	if err != nil {
		t.Fatal(err)
	}
	return generation.ArtifactSnapshot{Kind: kind, Key: key, ContentJSON: canonical, SHA256: digest}
}

// importStore is a store whose request names a Database Profile instead of an AI-generated Dataset.
func importStore(t *testing.T) *fakeStore {
	t.Helper()
	data, err := contracts.Files.ReadFile("examples/normalized-generation-request.json")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	document["generate_dataset"] = false
	document["database_profile_id"] = importedProfileID
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	store := datasetFakeStore(t)
	store.artifactsByKind[generation.ArtifactKindNormalizedRequest] = canonicalArtifact(t,
		generation.ArtifactKindNormalizedRequest, normalizedRequestKey, encoded)
	store.profile = importedProfile()
	return store
}

func businessGateway() *fakeDatasetGateway {
	return &fakeDatasetGateway{response: llm.CompletionResponse{
		Content: businessFieldsReply, StopReason: "stop",
		Model: llm.ModelRef{Provider: llm.ProviderOpenAI, Model: "gpt-test"},
	}}
}

func importedDraftArtifact(t *testing.T) generation.ArtifactSnapshot {
	t.Helper()
	document, err := json.Marshal(map[string]any{
		"schema_sql": importedSchemaSQL, "seed_sql": importedSeedSQL,
		"constraints": map[string]any{"version": "1.0", "checks": []any{}},
		"metadata": map[string]any{"tables": []any{map[string]any{
			"name": "orders", "columns": []any{map[string]any{"name": "id", "mysql_type": "VARCHAR(32)", "nullable": false}},
			"primary_key": []string{"id"}, "foreign_keys": []any{}, "row_count": 1,
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return canonicalArtifact(t, generation.ArtifactKindDataset, datasetDraftKey(0), document)
}

func TestDatasetStageImportsReferenceSchemaWithModelBusinessFields(t *testing.T) {
	t.Parallel()

	store := importStore(t)
	gateway := businessGateway()
	source := &fakeSnapshotSource{snapshot: generation.DatasetSnapshot{SchemaSQL: importedSchemaSQL, SeedSQL: importedSeedSQL}}
	processor := newDatasetProcessor(t, store, gateway, scenarioPolicy())
	processor.snapshotSource = source

	result, err := processor.Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusGeneratingDataset, Attempt: 1, Lease: validLease(),
	})
	if err != nil || result.Failure != nil || result.Transition.Event != generation.EventDatasetStored {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	if source.calls != 1 || source.profile.ID != importedProfileID {
		t.Fatalf("snapshot calls=%d profile=%#v", source.calls, source.profile)
	}

	if store.saved == nil {
		t.Fatal("no Dataset draft was saved")
	}
	var draft struct {
		SchemaSQL string `json:"schema_sql"`
		SeedSQL   string `json:"seed_sql"`
		Metadata  struct {
			Tables []struct {
				Name     string `json:"name"`
				RowCount int    `json:"row_count"`
			} `json:"tables"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(store.saved.ContentJSON, &draft); err != nil {
		t.Fatal(err)
	}
	if draft.SchemaSQL != importedSchemaSQL || draft.SeedSQL != importedSeedSQL {
		t.Fatalf("draft SQL was not taken from the reference snapshot: %#v", draft)
	}
	if len(draft.Metadata.Tables) != 1 || draft.Metadata.Tables[0].Name != "orders" || draft.Metadata.Tables[0].RowCount != 1 {
		t.Fatalf("metadata tables = %#v", draft.Metadata.Tables)
	}

	var record map[string]any
	if err := json.Unmarshal(store.saved.Validation, &record); err != nil {
		t.Fatal(err)
	}
	digest, _ := record["snapshot_sha256"].(string)
	if record["source"] != "SNAPSHOT" || record["business_fields_source"] != "LLM" ||
		record["database_profile_id"] != importedProfileID || len(digest) != 64 {
		t.Fatalf("validation record = %#v", record)
	}

	// The model sees table structure only: no row value of the reference data reaches its prompt.
	prompt := gateway.request.Messages[1].Content
	if prompt == nil || !strings.Contains(*prompt, "orders") ||
		strings.Contains(*prompt, "SNAPSHOT-ROW-VALUE-77") || strings.Contains(*prompt, "O-REF-1") {
		t.Fatalf("model prompt = %v", prompt)
	}
	schema := string(gateway.request.ResponseSchema)
	if strings.Contains(schema, `"schema_sql"`) || !strings.Contains(schema, `"coverage_tags"`) {
		t.Fatal("snapshot Dataset model response must cover business fields only")
	}
	if system := gateway.request.Messages[0].Content; system == nil || *system != snapshotSystemPrompt {
		t.Fatal("snapshot Dataset did not use the snapshot system prompt")
	}
}

func TestDatasetStageClassifiesSnapshotFailures(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name      string
		err       error
		code      string
		retryable bool
	}{
		{"deadline", context.DeadlineExceeded, "DATASET_CREATION_TIMEOUT", true},
		{"unsupported", fmt.Errorf("%w: partitions", generation.ErrDatasetSourceUnsupported), "DATASET_SNAPSHOT_UNSUPPORTED", false},
		{"limit", fmt.Errorf("%w: rows", generation.ErrDatasetSourceLimit), "DATASET_LIMIT_EXCEEDED", false},
		{"config", fmt.Errorf("%w: tls", generation.ErrDatasetSourceConfig), "DATASET_SOURCE_CONFIG_INVALID", false},
		{"unavailable", fmt.Errorf("%w: refused", generation.ErrDatasetSourceUnavailable), "DATASET_SOURCE_UNAVAILABLE", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			gateway := &fakeDatasetGateway{err: errors.New("model must not be called")}
			processor := newDatasetProcessor(t, importStore(t), gateway, scenarioPolicy())
			processor.snapshotSource = &fakeSnapshotSource{err: test.err}
			result, err := processor.Process(context.Background(), generation.StageRequest{
				JobID: pipelineJobID, Stage: generation.StatusGeneratingDataset, Attempt: 1, Lease: validLease(),
			})
			if err != nil || result.Failure == nil || result.Failure.Code != test.code || result.Failure.Retryable != test.retryable {
				t.Fatalf("result=%#v error=%v, want %s retryable=%v", result, err, test.code, test.retryable)
			}
			if gateway.request.Messages != nil {
				t.Fatal("the model was called for a failed import")
			}
		})
	}

	// An error the source did not classify is an infrastructure error for the worker to retry.
	unclassified := errors.New("connection reset by peer")
	processor := newDatasetProcessor(t, importStore(t), businessGateway(), scenarioPolicy())
	processor.snapshotSource = &fakeSnapshotSource{err: unclassified}
	if _, err := processor.Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusGeneratingDataset, Attempt: 1, Lease: validLease(),
	}); !errors.Is(err, unclassified) {
		t.Fatalf("unclassified error = %v, want the original error", err)
	}
}

func TestDatasetStageImportsSchemaOnlySnapshotWithoutRows(t *testing.T) {
	t.Parallel()

	store := importStore(t)
	source := &fakeSnapshotSource{snapshot: generation.DatasetSnapshot{SchemaSQL: importedSchemaSQL}}
	processor := newDatasetProcessor(t, store, businessGateway(), scenarioPolicy())
	processor.snapshotSource = source
	result, err := processor.Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusGeneratingDataset, Attempt: 1, Lease: validLease(),
	})
	if err != nil || result.Failure != nil || result.Transition.Event != generation.EventDatasetStored {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	var draft struct {
		SeedSQL  string `json:"seed_sql"`
		Metadata struct {
			Tables []struct {
				RowCount int `json:"row_count"`
			} `json:"tables"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(store.saved.ContentJSON, &draft); err != nil {
		t.Fatal(err)
	}
	if draft.SeedSQL != "" || len(draft.Metadata.Tables) != 1 || draft.Metadata.Tables[0].RowCount != 0 {
		t.Fatalf("SCHEMA_ONLY draft = %#v", draft)
	}
}

func TestDatasetStageRejectsSnapshotSQLOutsideTheAllowlist(t *testing.T) {
	t.Parallel()

	gateway := &fakeDatasetGateway{err: errors.New("model must not be called")}
	processor := newDatasetProcessor(t, importStore(t), gateway, scenarioPolicy())
	processor.snapshotSource = &fakeSnapshotSource{snapshot: generation.DatasetSnapshot{
		SchemaSQL: "CREATE VIEW orders_view AS SELECT 1", SeedSQL: importedSeedSQL,
	}}
	result, err := processor.Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusGeneratingDataset, Attempt: 1, Lease: validLease(),
	})
	if err != nil || result.Failure == nil || result.Failure.Code != "DATASET_SNAPSHOT_UNSUPPORTED" || result.Failure.Retryable {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	if gateway.request.Messages != nil {
		t.Fatal("the model was called for a reference schema that cannot be copied")
	}
}

func TestDatasetStageRequiresConfiguredSnapshotSource(t *testing.T) {
	t.Parallel()

	gateway := &fakeDatasetGateway{err: errors.New("model must not be called")}
	processor := newDatasetProcessor(t, importStore(t), gateway, scenarioPolicy())
	result, err := processor.Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusGeneratingDataset, Attempt: 1, Lease: validLease(),
	})
	if err != nil || result.Failure == nil || result.Failure.Code != "DATASET_SOURCE_NOT_CONFIGURED" || result.Failure.Retryable {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	if gateway.request.Messages != nil {
		t.Fatal("an unconfigured import must not fall back to AI generation")
	}
}

func TestRepairImportedDatasetKeepsReferenceSQL(t *testing.T) {
	t.Parallel()

	store := importStore(t)
	store.artifactsByKey[generation.ArtifactKindDataset+"/"+datasetDraftKey(0)] = importedDraftArtifact(t)
	store.artifactsByKey[generation.ArtifactKindDataset+"/"+datasetRejectionKey(0)] = canonicalArtifact(t,
		generation.ArtifactKindDataset, datasetRejectionKey(0),
		[]byte(`{"reasons":["DATASET_INVARIANT_VIOLATION: check orders-paid failed"]}`))
	gateway := businessGateway()
	processor := newDatasetProcessor(t, store, gateway, scenarioPolicy())

	result, err := processor.Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusRepairingDataset, RepairRound: 0, Attempt: 1, Lease: validLease(),
	})
	if err != nil || result.Failure != nil || result.Transition.Event != generation.EventRepairGenerated {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	if store.saved == nil || store.saved.Key != datasetDraftKey(1) {
		t.Fatalf("repaired draft key = %#v, want %s", store.saved, datasetDraftKey(1))
	}
	var draft struct {
		SchemaSQL string `json:"schema_sql"`
		SeedSQL   string `json:"seed_sql"`
	}
	if err := json.Unmarshal(store.saved.ContentJSON, &draft); err != nil {
		t.Fatal(err)
	}
	if draft.SchemaSQL != importedSchemaSQL || draft.SeedSQL != importedSeedSQL {
		t.Fatalf("repair changed reference SQL: %#v", draft)
	}
	prompt := gateway.request.Messages[1].Content
	if prompt == nil || !strings.Contains(*prompt, "DATASET_INVARIANT_VIOLATION: check orders-paid failed") ||
		strings.Contains(*prompt, "SNAPSHOT-ROW-VALUE-77") {
		t.Fatalf("repair prompt = %v", prompt)
	}
}

// Regression: validation must read the draft of the round it is validating. Before this, every
// round validated the original draft, so a repaired Dataset was never checked.
func TestValidateDatasetLoadsTheRepairedRoundDraft(t *testing.T) {
	t.Parallel()

	store := datasetFakeStore(t)
	store.artifactsByKey[generation.ArtifactKindDataset+"/"+datasetDraftKey(1)] = canonicalArtifact(t,
		generation.ArtifactKindDataset, datasetDraftKey(1),
		[]byte(`{"schema_sql":"CREATE VIEW orders_view AS SELECT 1","seed_sql":"","constraints":{"version":"1.0","checks":[]}}`))
	processor := newDatasetProcessor(t, store, &fakeDatasetGateway{}, scenarioPolicy())

	// Called directly: the stage dispatcher requires a validation MySQL connection before validating,
	// and this test only exercises which draft the stage reads.
	_, err := processor.validateDataset(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusValidatingDataset, RepairRound: 1, Attempt: 1, Lease: validLease(),
	})
	if err == nil {
		t.Fatal("validation accepted the CREATE VIEW draft of round 1")
	}
	if len(store.loadedKeys) == 0 || store.loadedKeys[0] != generation.ArtifactKindDataset+"/"+datasetDraftKey(1) {
		t.Fatalf("validation loaded %v, want the round 1 draft first", store.loadedKeys)
	}
}

func TestBusinessDatasetResponseSchemaOmitsReferenceSQL(t *testing.T) {
	t.Parallel()

	encoded, err := businessDatasetResponseSchema()
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(encoded, &schema); err != nil {
		t.Fatal(err)
	}
	want := []string{"constraints", "difficulty_profile", "coverage_tags"}
	if strings.Join(schema.Required, ",") != strings.Join(want, ",") || len(schema.Properties) != len(want) {
		t.Fatalf("required = %v properties = %d", schema.Required, len(schema.Properties))
	}
	for _, name := range want {
		if _, ok := schema.Properties[name]; !ok {
			t.Fatalf("business schema lacks %s", name)
		}
	}
}

func TestDatabaseProfileOfReadsOnlyProfileSourcedRequests(t *testing.T) {
	t.Parallel()

	for document, want := range map[string]string{
		`{"generate_dataset":false,"database_profile_id":"` + importedProfileID + `"}`: importedProfileID,
		`{"generate_dataset":true,"database_profile_id":null}`:                         "",
		`{"description":"no dataset source fields"}`:                                   "",
		`{"generate_dataset":false,"database_profile_id":null}`:                        "",
	} {
		if got := databaseProfileOf([]byte(document)); got != want {
			t.Errorf("databaseProfileOf(%s) = %q, want %q", document, got, want)
		}
	}
}
