package pipeline

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"agent-test-platform/contracts"
	"agent-test-platform/internal/generation"

	"github.com/oklog/ulid/v2"
)

const (
	generationRequestSchema = "schemas/generation-request.schema.json"
	normalizedRequestSchema = "schemas/normalized-generation-request.schema.json"
	normalizedRequestKey    = "request"
	draftTTL                = 24 * time.Hour
)

var ErrStageNotImplemented = errors.New("generation stage not implemented")

type Store interface {
	LoadGenerationRequest(context.Context, string) (generation.RequestSnapshot, error)
	LoadGenerationArtifact(context.Context, string, string, string) (generation.ArtifactSnapshot, error)
	SaveGenerationArtifact(context.Context, string, generation.WorkLease, generation.ArtifactDraft) error
	LoadPublishedScenarioRevision(context.Context, string) (generation.ScenarioRevisionSnapshot, error)
	LoadPublishedDatasetRevision(context.Context, string) (generation.DatasetRevisionSnapshot, error)
	LoadDatabaseProfileConnection(context.Context, string) (generation.DatabaseProfileConnection, error)
}

type Processor struct {
	store             Store
	validator         *contracts.Validator
	scenarioGateway   ScenarioGateway
	scenarioConfig    ScenarioConfig
	datasetGateway    DatasetGateway
	datasetConfig     DatasetConfig
	snapshotSource    DatasetSnapshotSource
	targets           TargetResolver
	testCaseGateway   TestCaseGateway
	testCaseValidator *generation.TestCaseValidator
	testCaseConfig    TestCaseConfig
	criticGateway     CriticGateway
	criticConfig      CriticConfig
}

type Option func(*Processor) error

func New(store Store, validator *contracts.Validator, options ...Option) (*Processor, error) {
	if store == nil || validator == nil {
		return nil, fmt.Errorf("generation store and contract validator are required")
	}
	processor := &Processor{store: store, validator: validator}
	for _, option := range options {
		if option == nil {
			return nil, fmt.Errorf("generation processor option is required")
		}
		if err := option(processor); err != nil {
			return nil, err
		}
	}
	return processor, nil
}

func (processor *Processor) Process(ctx context.Context, request generation.StageRequest) (generation.StageResult, error) {
	if result, reused, err := processor.reuseStoredOutput(ctx, request); err != nil || reused {
		return result, err
	}
	switch request.Stage {
	case generation.StatusPending:
		return generation.StageResult{Transition: generation.TransitionInput{Event: generation.EventInputAccepted}}, nil
	case generation.StatusValidatingInput:
		return processor.validateInput(ctx, request)
	case generation.StatusGeneratingScenario:
		if processor.scenarioGateway == nil {
			return generation.StageResult{}, fmt.Errorf("%w: %s", ErrStageNotImplemented, request.Stage)
		}
		return processor.generateScenario(ctx, request)
	case generation.StatusValidatingScenario:
		if processor.scenarioGateway == nil {
			return generation.StageResult{}, fmt.Errorf("%w: %s", ErrStageNotImplemented, request.Stage)
		}
		return processor.validateScenario(ctx, request)
	case generation.StatusRepairingScenario:
		if processor.scenarioGateway == nil {
			return generation.StageResult{}, fmt.Errorf("%w: %s", ErrStageNotImplemented, request.Stage)
		}
		return processor.repairScenario(ctx, request)
	case generation.StatusGeneratingDataset:
		if processor.datasetGateway == nil {
			return generation.StageResult{}, fmt.Errorf("%w: %s", ErrStageNotImplemented, request.Stage)
		}
		return processor.generateDataset(ctx, request)
	case generation.StatusValidatingDataset:
		if processor.targets == nil {
			return generation.StageResult{}, fmt.Errorf("%w: %s", ErrStageNotImplemented, request.Stage)
		}
		return processor.validateDataset(ctx, request)
	case generation.StatusRepairingDataset:
		if processor.datasetGateway == nil {
			return generation.StageResult{}, fmt.Errorf("%w: %s", ErrStageNotImplemented, request.Stage)
		}
		return processor.repairDataset(ctx, request)
	case generation.StatusGeneratingTestCases:
		if processor.testCaseGateway == nil {
			return generation.StageResult{}, fmt.Errorf("%w: %s", ErrStageNotImplemented, request.Stage)
		}
		return processor.generateTestCases(ctx, request)
	case generation.StatusValidatingTestCases:
		if processor.testCaseGateway == nil {
			return generation.StageResult{}, fmt.Errorf("%w: %s", ErrStageNotImplemented, request.Stage)
		}
		return processor.validateTestCases(ctx, request)
	case generation.StatusRepairingTestCases:
		if processor.testCaseGateway == nil {
			return generation.StageResult{}, fmt.Errorf("%w: %s", ErrStageNotImplemented, request.Stage)
		}
		return processor.repairTestCases(ctx, request)
	default:
		return generation.StageResult{}, fmt.Errorf("%w: %s", ErrStageNotImplemented, request.Stage)
	}
}

func (processor *Processor) validateInputRevisions(ctx context.Context, request generation.StageRequest) (*generation.StageResult, error) {
	if request.InputScenarioRevisionID != nil {
		scenario, err := processor.store.LoadPublishedScenarioRevision(ctx, *request.InputScenarioRevisionID)
		if errors.Is(err, generation.ErrNotFound) {
			result := failure("REFERENCE_NOT_FOUND", "Referenced Scenario Revision was not found.", false, nil)
			return &result, nil
		}
		if err != nil {
			return nil, err
		}
		if !generation.RevisionLifecycleUsable(scenario.LifecycleStatus) {
			result := failure("INPUT_REVISION_NOT_USABLE", "Referenced Scenario Revision must be VALIDATED or ACTIVE.", false, map[string]any{"lifecycle_status": scenario.LifecycleStatus})
			return &result, nil
		}
	}
	if request.InputDatasetRevisionID != nil {
		dataset, err := processor.store.LoadPublishedDatasetRevision(ctx, *request.InputDatasetRevisionID)
		if errors.Is(err, generation.ErrNotFound) {
			result := failure("REFERENCE_NOT_FOUND", "Referenced Dataset Revision was not found.", false, nil)
			return &result, nil
		}
		if err != nil {
			return nil, err
		}
		if !generation.RevisionLifecycleUsable(dataset.LifecycleStatus) {
			result := failure("INPUT_REVISION_NOT_USABLE", "Referenced Dataset Revision must be VALIDATED or ACTIVE.", false, map[string]any{"lifecycle_status": dataset.LifecycleStatus})
			return &result, nil
		}
	}
	return nil, nil
}

func (processor *Processor) validateInput(ctx context.Context, request generation.StageRequest) (generation.StageResult, error) {
	snapshot, err := processor.store.LoadGenerationRequest(ctx, request.JobID)
	if errors.Is(err, generation.ErrNotFound) {
		return failure("REFERENCE_NOT_FOUND", "Generation request was not found.", false, nil), nil
	}
	if err != nil {
		return generation.StageResult{}, err
	}
	_, sourceDigest, err := generation.CanonicalJSON(snapshot.Document)
	if err != nil || sourceDigest != snapshot.SHA256 {
		return failure("REQUEST_INTEGRITY_ERROR", "Stored Generation request failed its integrity check.", false, nil), nil
	}
	if err := processor.validator.ValidateJSON(generationRequestSchema, snapshot.Document); err != nil {
		return failure("INPUT_SCHEMA_INVALID", "Stored Generation request does not match its contract.", false, nil), nil
	}
	normalized, err := generation.NormalizeRequest(snapshot.Document)
	if err != nil {
		return failure("INPUT_SEMANTIC_INVALID", "Generation request failed semantic validation.", false, map[string]any{"reason": err.Error()}), nil
	}
	// Only a TEST_CASES Job (the only kind with both input Revisions) stores a requested count; Scenario and Dataset Jobs store 0.
	hasCount := request.InputDatasetRevisionID != nil || (request.InputScenarioRevisionID == nil && request.InputDatasetRevisionID == nil && snapshot.RequestedTestCaseCount != 0)
	if hasCount && normalized.TestCaseCount != snapshot.RequestedTestCaseCount {
		return failure("REQUEST_INTEGRITY_ERROR", "Stored Test Case count does not match the Generation Job.", false, nil), nil
	}
	inputResult, err := processor.validateInputRevisions(ctx, request)
	if err != nil {
		return generation.StageResult{}, err
	}
	if inputResult != nil {
		return *inputResult, nil
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return generation.StageResult{}, fmt.Errorf("encode normalized generation request: %w", err)
	}
	canonical, digest, err := generation.CanonicalJSON(encoded)
	if err != nil {
		return generation.StageResult{}, fmt.Errorf("canonicalize normalized generation request: %w", err)
	}
	if err := processor.validator.ValidateJSON(normalizedRequestSchema, canonical); err != nil {
		return failure("NORMALIZED_REQUEST_INVALID", "Normalized Generation request failed its contract.", false, nil), nil
	}
	validation, err := json.Marshal(map[string]any{
		"schema": normalizedRequestSchema, "source_sha256": snapshot.SHA256, "valid": true,
	})
	if err != nil {
		return generation.StageResult{}, fmt.Errorf("encode normalized request validation: %w", err)
	}
	if err := processor.store.SaveGenerationArtifact(ctx, request.JobID, request.Lease, generation.ArtifactDraft{
		Kind: generation.ArtifactKindNormalizedRequest, Key: normalizedRequestKey,
		ContentJSON: canonical, SHA256: digest, Validation: validation, TTL: draftTTL,
	}); err != nil {
		if errors.Is(err, generation.ErrConflict) {
			return failure("ARTIFACT_CONFLICT", "Normalized request conflicts with the existing draft.", false, nil), nil
		}
		return generation.StageResult{}, err
	}
	return generation.StageResult{Transition: generation.TransitionInput{Event: generation.EventInputValidated}}, nil
}

func failure(code, message string, retryable bool, details map[string]any) generation.StageResult {
	return generation.StageResult{Failure: &generation.JobFailure{
		Code: code, Message: message, Retryable: retryable, Details: details,
	}}
}

// loadScenarioContext resolves the Scenario a Dataset or Test Case stage needs as input: when
// the Job carries input_scenario_revision_id (DATASET/TEST_CASES kind with the reference
// wired), it reads the already-published Scenario Revision; otherwise it falls back to this
// same Job's own Scenario draft (still how job_kind-unaware callers work today - see
// docs/architecture.md 6.2).
func (processor *Processor) loadScenarioContext(ctx context.Context, stage generation.StageRequest) (json.RawMessage, string, *generation.StageResult, error) {
	if stage.InputScenarioRevisionID != nil {
		revision, err := processor.store.LoadPublishedScenarioRevision(ctx, *stage.InputScenarioRevisionID)
		if errors.Is(err, generation.ErrNotFound) {
			result := failure("REFERENCE_NOT_FOUND", "Referenced Scenario Revision was not found.", false, nil)
			return nil, "", &result, nil
		}
		if err != nil {
			return nil, "", nil, err
		}
		return revision.ContentJSON, revision.SHA256, nil, nil
	}
	snapshot, err := processor.store.LoadGenerationArtifact(ctx, stage.JobID, generation.ArtifactKindScenario, scenarioDraftKey(0))
	if errors.Is(err, generation.ErrNotFound) {
		result := failure("REFERENCE_NOT_FOUND", "Scenario draft was not found.", false, nil)
		return nil, "", &result, nil
	}
	if err != nil {
		return nil, "", nil, err
	}
	canonical, digest, err := generation.CanonicalJSON(snapshot.ContentJSON)
	if err != nil || digest != snapshot.SHA256 {
		result := failure("ARTIFACT_INTEGRITY_ERROR", "Scenario draft failed its integrity check.", false, nil)
		return nil, "", &result, nil
	}
	return canonical, snapshot.SHA256, nil, nil
}

// datasetContext is the subset of a Dataset (draft or published) a Test Case stage needs.
type datasetContext struct {
	MetadataJSON      json.RawMessage
	Database          string
	DatasetRevisionID string
	ValidationDBIDs   []string
}

// loadDatasetContext resolves the Dataset a Test Case stage needs as input, mirroring
// loadScenarioContext: published Dataset Revision when input_dataset_revision_id is wired,
// same-Job draft (Dataset + Dataset execution record) otherwise.
func (processor *Processor) loadDatasetContext(ctx context.Context, stage generation.StageRequest) (datasetContext, *generation.StageResult, error) {
	if stage.InputDatasetRevisionID != nil {
		revision, err := processor.store.LoadPublishedDatasetRevision(ctx, *stage.InputDatasetRevisionID)
		if errors.Is(err, generation.ErrNotFound) {
			result := failure("REFERENCE_NOT_FOUND", "Referenced Dataset Revision was not found.", false, nil)
			return datasetContext{}, &result, nil
		}
		if err != nil {
			return datasetContext{}, nil, err
		}
		return datasetContext{MetadataJSON: revision.MetadataJSON, Database: revision.ExecutionDatabase, DatasetRevisionID: revision.ID, ValidationDBIDs: revision.ValidationDBIDs}, nil, nil
	}
	datasetSnapshot, err := processor.store.LoadGenerationArtifact(ctx, stage.JobID, generation.ArtifactKindDataset, datasetDraftKey(0))
	if errors.Is(err, generation.ErrNotFound) {
		result := failure("REFERENCE_NOT_FOUND", "Dataset draft was not found.", false, nil)
		return datasetContext{}, &result, nil
	}
	if err != nil {
		return datasetContext{}, nil, err
	}
	canonicalDataset, datasetDigest, err := generation.CanonicalJSON(datasetSnapshot.ContentJSON)
	if err != nil || datasetDigest != datasetSnapshot.SHA256 {
		result := failure("ARTIFACT_INTEGRITY_ERROR", "Dataset draft failed its integrity check.", false, nil)
		return datasetContext{}, &result, nil
	}
	var dataset struct {
		Metadata json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal(canonicalDataset, &dataset); err != nil {
		return datasetContext{}, nil, fmt.Errorf("decode validated Dataset draft: %w", err)
	}
	executionSnapshot, err := processor.store.LoadGenerationArtifact(ctx, stage.JobID, generation.ArtifactKindDatasetExecution, datasetExecutionKey)
	if errors.Is(err, generation.ErrNotFound) {
		result := failure("REFERENCE_NOT_FOUND", "Dataset execution record was not found.", false, nil)
		return datasetContext{}, &result, nil
	}
	if err != nil {
		return datasetContext{}, nil, err
	}
	execution, err := generation.DecodeDatasetExecutionRecord(executionSnapshot.ContentJSON)
	if err != nil {
		return datasetContext{}, nil, fmt.Errorf("decode Dataset execution record: %w", err)
	}
	targetIDs := make([]string, 0, len(execution.Targets))
	for _, target := range execution.Targets {
		targetIDs = append(targetIDs, target.ValidationDBID)
	}
	return datasetContext{MetadataJSON: dataset.Metadata, Database: execution.Targets[0].Database, DatasetRevisionID: execution.DatasetRevisionID, ValidationDBIDs: targetIDs}, nil, nil
}

func newULID() (string, error) {
	id, err := ulid.New(ulid.Timestamp(time.Now().UTC()), rand.Reader)
	if err != nil {
		return "", fmt.Errorf("generate ID: %w", err)
	}
	return id.String(), nil
}

type allocatedID struct {
	ID string `json:"id"`
}

// getOrAllocateID idempotently allocates a bookkeeping ID that has no external side effect
// of its own (for example a pre-published Scenario/Test Case identity used only to
// cross-reference drafts). It must never be used for an ID that also names an external
// resource created by a non-idempotent operation (for example a validation MySQL database),
// because retrying the allocation would not retry that resource's creation safely.
func (processor *Processor) getOrAllocateID(ctx context.Context, stage generation.StageRequest, kind, key string) (string, error) {
	snapshot, err := processor.store.LoadGenerationArtifact(ctx, stage.JobID, kind, key)
	if err == nil {
		var allocated allocatedID
		if err := json.Unmarshal(snapshot.ContentJSON, &allocated); err != nil {
			return "", fmt.Errorf("decode allocated %s ID: %w", kind, err)
		}
		return allocated.ID, nil
	}
	if !errors.Is(err, generation.ErrNotFound) {
		return "", err
	}
	id, err := newULID()
	if err != nil {
		return "", err
	}
	content, err := json.Marshal(allocatedID{ID: id})
	if err != nil {
		return "", fmt.Errorf("encode allocated %s ID: %w", kind, err)
	}
	canonical, digest, err := generation.CanonicalJSON(content)
	if err != nil {
		return "", fmt.Errorf("canonicalize allocated %s ID: %w", kind, err)
	}
	if err := processor.store.SaveGenerationArtifact(ctx, stage.JobID, stage.Lease, generation.ArtifactDraft{
		Kind: kind, Key: key, ContentJSON: canonical, SHA256: digest, TTL: draftTTL,
	}); err != nil {
		if !errors.Is(err, generation.ErrConflict) {
			return "", err
		}
		// Another attempt raced us and already allocated one; use that instead of ours.
		snapshot, reloadErr := processor.store.LoadGenerationArtifact(ctx, stage.JobID, kind, key)
		if reloadErr != nil {
			return "", reloadErr
		}
		var allocated allocatedID
		if err := json.Unmarshal(snapshot.ContentJSON, &allocated); err != nil {
			return "", fmt.Errorf("decode allocated %s ID: %w", kind, err)
		}
		return allocated.ID, nil
	}
	return id, nil
}
