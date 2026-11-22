package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"agent-test-platform/contracts"
	"agent-test-platform/internal/generation"
)

const pipelineJobID = "01K4Z6Y7X8W9V0T1S2R3Q4P5N6"

type fakeStore struct {
	snapshot         generation.RequestSnapshot
	loadErr          error
	artifact         generation.ArtifactSnapshot
	artifactErr      error
	artifactsByKind  map[string]generation.ArtifactSnapshot
	artifactsByKey   map[string]generation.ArtifactSnapshot
	loadedKeys       []string
	saved            *generation.ArtifactDraft
	saveErr          error
	lease            generation.WorkLease
	scenarioRevision generation.ScenarioRevisionSnapshot
	datasetRevision  generation.DatasetRevisionSnapshot
	profile          generation.DatabaseProfileConnection
}

func (store *fakeStore) LoadGenerationArtifact(_ context.Context, _ string, kind string, key string) (generation.ArtifactSnapshot, error) {
	store.loadedKeys = append(store.loadedKeys, kind+"/"+key)
	if artifact, ok := store.artifactsByKey[kind+"/"+key]; ok {
		return artifact, store.artifactErr
	}
	if artifact, ok := store.artifactsByKind[kind]; ok {
		return artifact, store.artifactErr
	}
	if store.artifactErr != nil || store.artifact.Kind == kind {
		return store.artifact, store.artifactErr
	}
	return generation.ArtifactSnapshot{}, generation.ErrNotFound
}

func (store *fakeStore) LoadGenerationRequest(context.Context, string) (generation.RequestSnapshot, error) {
	return store.snapshot, store.loadErr
}

func (store *fakeStore) SaveGenerationArtifact(_ context.Context, _ string, lease generation.WorkLease, draft generation.ArtifactDraft) error {
	store.lease = lease
	cloned := draft
	store.saved = &cloned
	return store.saveErr
}

func (store *fakeStore) LoadPublishedScenarioRevision(context.Context, string) (generation.ScenarioRevisionSnapshot, error) {
	if store.scenarioRevision.ID == "" {
		return generation.ScenarioRevisionSnapshot{}, generation.ErrNotFound
	}
	return store.scenarioRevision, nil
}

func (store *fakeStore) LoadPublishedDatasetRevision(context.Context, string) (generation.DatasetRevisionSnapshot, error) {
	if store.datasetRevision.ID == "" {
		return generation.DatasetRevisionSnapshot{}, generation.ErrNotFound
	}
	return store.datasetRevision, nil
}

func (store *fakeStore) LoadDatabaseProfileConnection(context.Context, string) (generation.DatabaseProfileConnection, error) {
	if store.profile.ID == "" {
		return generation.DatabaseProfileConnection{}, generation.ErrNotFound
	}
	return store.profile, nil
}

func TestProcessorRejectsInputRevisionsThatAreNotValidatedOrActive(t *testing.T) {
	t.Parallel()

	document := json.RawMessage(`{"validation_db_ids":["0b6d6f3e-2c4a-4f8e-9a1b-7c5d3e2f1a90"],"description":"Generate tests.","test_case_count":3}`)
	_, digest, _ := generation.CanonicalJSON(document)
	scenarioInput := "01K4Z6Y7X8W9V0T1S2R3Q4P5N7"
	datasetInput := "01K4Z6Y7X8W9V0T1S2R3Q4P5N8"

	for _, test := range []struct {
		name          string
		scenarioState string
		datasetState  string
		wantCode      string
	}{
		{name: "deprecated scenario", scenarioState: "DEPRECATED", wantCode: "INPUT_REVISION_NOT_USABLE"},
		{name: "staging dataset", scenarioState: "VALIDATED", datasetState: "STAGING", wantCode: "INPUT_REVISION_NOT_USABLE"},
		{name: "validated and active inputs", scenarioState: "VALIDATED", datasetState: "ACTIVE"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store := &fakeStore{
				snapshot:         generation.RequestSnapshot{Document: document, SHA256: digest, RequestedTestCaseCount: 3},
				scenarioRevision: generation.ScenarioRevisionSnapshot{ID: scenarioInput, LifecycleStatus: test.scenarioState},
			}
			if test.datasetState != "" {
				store.datasetRevision = generation.DatasetRevisionSnapshot{ID: datasetInput, LifecycleStatus: test.datasetState}
			}
			request := generation.StageRequest{
				JobID: pipelineJobID, Stage: generation.StatusValidatingInput, Lease: validLease(),
				InputScenarioRevisionID: &scenarioInput,
			}
			if test.datasetState != "" {
				request.InputDatasetRevisionID = &datasetInput
			}
			result, err := newTestProcessor(t, store).Process(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if test.wantCode == "" {
				if result.Failure != nil || result.Transition.Event != generation.EventInputValidated {
					t.Fatalf("result = %#v", result)
				}
				return
			}
			if result.Failure == nil || result.Failure.Code != test.wantCode || result.Failure.Retryable {
				t.Fatalf("result = %#v, want non-retryable %s", result, test.wantCode)
			}
		})
	}
}

func TestProcessorAcceptsPendingStage(t *testing.T) {
	t.Parallel()

	store := &fakeStore{}
	processor := newTestProcessor(t, store)
	result, err := processor.Process(context.Background(), generation.StageRequest{Stage: generation.StatusPending})
	if err != nil || result.Transition.Event != generation.EventInputAccepted || store.saved != nil {
		t.Fatalf("result=%#v error=%v", result, err)
	}
}

func TestProcessorValidatesAndStoresNormalizedRequest(t *testing.T) {
	t.Parallel()

	document := json.RawMessage(`{"validation_db_ids":["0b6d6f3e-2c4a-4f8e-9a1b-7c5d3e2f1a90"],"description":"Generate cancellation tests.","capabilities":["MYSQL_QUERY","HTTP"],"test_case_count":3}`)
	_, digest, err := generation.CanonicalJSON(document)
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{snapshot: generation.RequestSnapshot{Document: document, SHA256: digest, RequestedTestCaseCount: 3}}
	processor := newTestProcessor(t, store)
	request := generation.StageRequest{JobID: pipelineJobID, Stage: generation.StatusValidatingInput, Lease: validLease()}

	result, err := processor.Process(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Transition.Event != generation.EventInputValidated || result.Failure != nil {
		t.Fatalf("result = %#v", result)
	}
	if store.saved == nil || store.saved.Kind != generation.ArtifactKindNormalizedRequest || store.saved.TTL != draftTTL {
		t.Fatalf("draft = %#v", store.saved)
	}
	if store.lease != request.Lease {
		t.Fatalf("lease = %#v", store.lease)
	}
	var normalized generation.NormalizedRequest
	if err := json.Unmarshal(store.saved.ContentJSON, &normalized); err != nil {
		t.Fatal(err)
	}
	if normalized.Capabilities == nil || strings.Join(*normalized.Capabilities, ",") != "HTTP,MYSQL_QUERY" {
		t.Fatalf("normalized = %#v", normalized)
	}
}

func TestProcessorRejectsRequestIntegrityMismatch(t *testing.T) {
	t.Parallel()

	store := &fakeStore{snapshot: generation.RequestSnapshot{
		Document: []byte(`{"validation_db_ids":["0b6d6f3e-2c4a-4f8e-9a1b-7c5d3e2f1a90"],"description":"Generate tests."}`), SHA256: strings.Repeat("a", 64), RequestedTestCaseCount: 10,
	}}
	result, err := newTestProcessor(t, store).Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusValidatingInput, Lease: validLease(),
	})
	if err != nil || result.Failure == nil || result.Failure.Code != "REQUEST_INTEGRITY_ERROR" || store.saved != nil {
		t.Fatalf("result=%#v error=%v", result, err)
	}
}

func TestProcessorRejectsStoredCountMismatch(t *testing.T) {
	t.Parallel()

	document := json.RawMessage(`{"validation_db_ids":["0b6d6f3e-2c4a-4f8e-9a1b-7c5d3e2f1a90"],"description":"Generate tests.","test_case_count":3}`)
	_, digest, _ := generation.CanonicalJSON(document)
	store := &fakeStore{snapshot: generation.RequestSnapshot{Document: document, SHA256: digest, RequestedTestCaseCount: 4}}
	result, err := newTestProcessor(t, store).Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusValidatingInput, Lease: validLease(),
	})
	if err != nil || result.Failure == nil || result.Failure.Code != "REQUEST_INTEGRITY_ERROR" {
		t.Fatalf("result=%#v error=%v", result, err)
	}
}

func TestProcessorClassifiesStoreErrors(t *testing.T) {
	t.Parallel()

	processor := newTestProcessor(t, &fakeStore{loadErr: generation.ErrNotFound})
	result, err := processor.Process(context.Background(), generation.StageRequest{JobID: pipelineJobID, Stage: generation.StatusValidatingInput})
	if err != nil || result.Failure == nil || result.Failure.Code != "REFERENCE_NOT_FOUND" {
		t.Fatalf("result=%#v error=%v", result, err)
	}

	databaseErr := errors.New("database unavailable")
	processor = newTestProcessor(t, &fakeStore{loadErr: databaseErr})
	if _, err := processor.Process(context.Background(), generation.StageRequest{JobID: pipelineJobID, Stage: generation.StatusValidatingInput}); !errors.Is(err, databaseErr) {
		t.Fatalf("error = %v", err)
	}
}

func newTestProcessor(t *testing.T, store Store) *Processor {
	t.Helper()
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	processor, err := New(store, validator)
	if err != nil {
		t.Fatal(err)
	}
	return processor
}

func validLease() generation.WorkLease {
	return generation.WorkLease{WorkItemID: "work-1", Owner: "worker-1", FencingToken: 1, AttemptCount: 1, MaxAttempts: 3}
}
