package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"agent-test-platform/internal/generation"
	"agent-test-platform/internal/llm"
	"agent-test-platform/internal/sandbox"
)

// storedExecutionRecord is the execution record a successful validation stores. Its database name
// is derived from the revision, as validation derives it.
func storedExecutionRecord(t *testing.T, revisionID string) []byte {
	t.Helper()
	identifiers, err := sandbox.DeriveDatasetIdentifiers(revisionID)
	if err != nil {
		t.Fatal(err)
	}
	record, err := json.Marshal(map[string]any{"dataset_revision_id": revisionID, "targets": []map[string]string{{"validation_db_id": testTargetID, "database": identifiers.Database}}})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestGeneratingScenarioReusesStoredDraftWithoutModelCall(t *testing.T) {
	t.Parallel()

	store := &fakeStore{artifact: normalizedArtifact(t), artifactsByKey: map[string]generation.ArtifactSnapshot{}}
	store.artifactsByKey[generation.ArtifactKindScenario+"/"+scenarioDraftKey(0)] = canonicalArtifact(t,
		generation.ArtifactKindScenario, scenarioDraftKey(0), []byte(`{}`))
	gateway := &fakeScenarioGateway{err: errors.New("model must not be called")}
	processor := newScenarioProcessor(t, store, gateway, scenarioPolicy())

	result, err := processor.Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusGeneratingScenario, Attempt: 2, Lease: validLease(),
	})
	if err != nil || result.Failure != nil || result.Transition.Event != generation.EventScenarioStored {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	if store.saved != nil || gateway.request.Messages != nil {
		t.Fatalf("reuse saved %#v or called the model with %#v", store.saved, gateway.request)
	}
}

func TestGeneratingDatasetReusesStoredDraftWithoutModelCall(t *testing.T) {
	t.Parallel()

	store := datasetFakeStore(t)
	store.artifactsByKey[generation.ArtifactKindDataset+"/"+datasetDraftKey(0)] = canonicalArtifact(t,
		generation.ArtifactKindDataset, datasetDraftKey(0), []byte(validDatasetDraft))
	gateway := &fakeDatasetGateway{err: errors.New("model must not be called")}
	processor := newDatasetProcessor(t, store, gateway, scenarioPolicy())

	result, err := processor.Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusGeneratingDataset, Attempt: 2, Lease: validLease(),
	})
	if err != nil || result.Failure != nil || result.Transition.Event != generation.EventDatasetStored {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	if store.saved != nil || gateway.request.Messages != nil {
		t.Fatalf("reuse saved %#v or called the model with %#v", store.saved, gateway.request)
	}
}

func TestRepairingDatasetReusesStoredRepairDraftWithoutModelCall(t *testing.T) {
	t.Parallel()

	store := datasetFakeStore(t)
	store.artifactsByKey[generation.ArtifactKindDataset+"/"+datasetDraftKey(1)] = canonicalArtifact(t,
		generation.ArtifactKindDataset, datasetDraftKey(1), []byte(validDatasetDraft))
	gateway := &fakeDatasetGateway{err: errors.New("model must not be called")}
	processor := newDatasetProcessor(t, store, gateway, scenarioPolicy())

	result, err := processor.Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusRepairingDataset, RepairRound: 0, Attempt: 2, Lease: validLease(),
	})
	if err != nil || result.Failure != nil || result.Transition.Event != generation.EventRepairGenerated {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	if store.saved != nil || gateway.request.Messages != nil {
		t.Fatalf("reuse saved %#v or called the model with %#v", store.saved, gateway.request)
	}
}

// The round being repaired already has a draft (the rejected one). It must not be taken as the
// repair output, which is stored under the next round.
func TestRepairingDatasetDoesNotReuseTheRejectedDraft(t *testing.T) {
	t.Parallel()

	store := datasetFakeStore(t)
	store.artifactsByKey[generation.ArtifactKindDataset+"/"+datasetDraftKey(0)] = canonicalArtifact(t,
		generation.ArtifactKindDataset, datasetDraftKey(0), []byte(validDatasetDraft))
	store.artifactsByKey[generation.ArtifactKindDataset+"/"+datasetRejectionKey(0)] = canonicalArtifact(t,
		generation.ArtifactKindDataset, datasetRejectionKey(0),
		[]byte(`{"reasons":["DATASET_INVARIANT_VIOLATION: check failed"]}`))
	gateway := &fakeDatasetGateway{err: errors.New("model called")}
	processor := newDatasetProcessor(t, store, gateway, scenarioPolicy())

	result, err := processor.Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusRepairingDataset, RepairRound: 0, Attempt: 2, Lease: validLease(),
	})
	if gateway.request.Messages == nil {
		t.Fatalf("repair reused the rejected round instead of calling the model: result=%#v error=%v", result, err)
	}
}

func TestGeneratingTestCasesReusesStoredManifestWithoutModelCall(t *testing.T) {
	t.Parallel()

	store, target, _ := newTestCaseHarness(t, 1)
	seedManifest(t, store, 0, testCaseManifest{Accepted: []string{"cancel-paid-order"}})
	gateway := &fakeTestCaseGateway{err: errors.New("model must not be called")}
	processor := newTestCaseProcessor(t, store, gateway, target)

	result, err := processor.Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusGeneratingTestCases, RepairRound: 0, Attempt: 2, Lease: validLease(),
	})
	if err != nil || result.Failure != nil || result.Transition.Event != generation.EventCandidatesGenerated {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	if len(gateway.requests) != 0 {
		t.Fatalf("reused manifest still called the model %d time(s)", len(gateway.requests))
	}
}

// Called directly without a validation MySQL: the stage would fail if it provisioned a new schema
// instead of reusing the stored execution record.
func TestValidateDatasetReusesStoredExecutionWithoutProvisioning(t *testing.T) {
	t.Parallel()

	store := datasetFakeStore(t)
	store.artifactsByKey[generation.ArtifactKindDataset+"/"+datasetDraftKey(0)] = canonicalArtifact(t,
		generation.ArtifactKindDataset, datasetDraftKey(0), []byte(validDatasetDraft))
	store.artifactsByKey[generation.ArtifactKindDatasetExecution+"/"+datasetExecutionArtifactKey(0)] = canonicalArtifact(t,
		generation.ArtifactKindDatasetExecution, datasetExecutionArtifactKey(0),
		storedExecutionRecord(t, "01HZX0000000000000000000AB"))
	processor := newDatasetProcessor(t, store, &fakeDatasetGateway{}, scenarioPolicy())
	target := &fakeValidationDB{}
	processor.targets = singleTarget{target}

	result, err := processor.validateDataset(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusValidatingDataset, RepairRound: 0, Attempt: 2, Lease: validLease(),
	})
	if err != nil || result.Failure != nil || result.Transition.Event != generation.EventValidationAccepted {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	if store.saved != nil {
		t.Fatalf("reused execution record must not be saved again: %#v", store.saved)
	}
	// Reuse makes no validation MySQL statement, so a retry never provisions a second database.
	if len(target.execCalls) != 0 {
		t.Fatalf("reuse ran validation MySQL statements: %v", target.execCalls)
	}
}

// fakeCriticGateway records every Critic call and fails each one.
type fakeCriticGateway struct {
	err      error
	requests []llm.CompletionRequest
}

func (gateway *fakeCriticGateway) Complete(_ context.Context, _ string, request llm.CompletionRequest) (llm.CompletionResponse, error) {
	gateway.requests = append(gateway.requests, request)
	return llm.CompletionResponse{}, gateway.err
}

// criticFixture is a validateDataset run whose execution schema already exists, so the only model
// call left is the Critic's.
func criticFixture(t *testing.T, storedReview bool) (*Processor, *fakeCriticGateway) {
	t.Helper()
	store := datasetFakeStore(t)
	store.artifactsByKey[generation.ArtifactKindDataset+"/"+datasetDraftKey(0)] = canonicalArtifact(t,
		generation.ArtifactKindDataset, datasetDraftKey(0), []byte(validDatasetDraft))
	store.artifactsByKey[generation.ArtifactKindDatasetExecution+"/"+datasetExecutionArtifactKey(0)] = canonicalArtifact(t,
		generation.ArtifactKindDatasetExecution, datasetExecutionArtifactKey(0),
		storedExecutionRecord(t, "01HZX0000000000000000000AB"))
	if storedReview {
		reviewKey := generation.CriticReviewKey(generation.CriticScopeDataset, 0)
		store.artifactsByKey[generation.ArtifactKindCriticReview+"/"+reviewKey] = canonicalArtifact(t,
			generation.ArtifactKindCriticReview, reviewKey, []byte(`{}`))
	}
	processor := newDatasetProcessor(t, store, &fakeDatasetGateway{}, scenarioPolicy())
	critic := &fakeCriticGateway{err: errors.New("critic model call failed")}
	processor.criticGateway = critic
	processor.criticConfig = CriticConfig{ModelAlias: "critic-primary", MaxOutputTokens: 1_024, CompactionPolicy: scenarioPolicy()}
	processor.targets = singleTarget{catalogTarget(t)}
	return processor, critic
}

func TestValidateDatasetSkipsCriticWhenReviewIsStored(t *testing.T) {
	t.Parallel()

	processor, critic := criticFixture(t, true)
	result, err := processor.validateDataset(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusValidatingDataset, RepairRound: 0, Attempt: 2, Lease: validLease(),
	})
	if err != nil || result.Failure != nil || result.Transition.Event != generation.EventValidationAccepted {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	if len(critic.requests) != 0 {
		t.Fatalf("critic called %d time(s) although its accepted review is stored", len(critic.requests))
	}
}

func TestValidateDatasetRunsCriticWhenReviewIsNotStored(t *testing.T) {
	t.Parallel()

	processor, critic := criticFixture(t, false)
	_, err := processor.validateDataset(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusValidatingDataset, RepairRound: 0, Attempt: 2, Lease: validLease(),
	})
	if len(critic.requests) != 1 {
		t.Fatalf("critic called %d time(s), want 1 (error=%v)", len(critic.requests), err)
	}
}
