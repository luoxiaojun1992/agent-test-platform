package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"agent-test-platform/internal/generation"

	"github.com/DATA-DOG/go-sqlmock"
)

// cancelOnCreateTarget ends the stage context as soon as the execution schema is created, so the
// loader's first statement runs under a context that has already ended.
type cancelOnCreateTarget struct {
	*fakeValidationDB
	cancel context.CancelFunc
}

func (target *cancelOnCreateTarget) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if strings.HasPrefix(query, "CREATE DATABASE") {
		target.cancel()
	}
	return target.fakeValidationDB.ExecContext(ctx, query, args...)
}

func timeoutStage() generation.StageRequest {
	return generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusValidatingDataset, RepairRound: 0, Attempt: 1, Lease: validLease(),
	}
}

// A stage whose deadline passes while the execution schema is provisioned is a timeout. The stage
// returns the deadline error, so the worker records DATASET_VALIDATION_TIMEOUT rather than
// VALIDATION_DB_UNAVAILABLE, which is reserved for infrastructure errors that are not timeouts.
func TestValidateDatasetDeadlineDuringProvisioningIsATimeout(t *testing.T) {
	t.Parallel()

	store := datasetFakeStore(t)
	store.artifactsByKey[generation.ArtifactKindDataset+"/"+datasetDraftKey(0)] = canonicalArtifact(t,
		generation.ArtifactKindDataset, datasetDraftKey(0), []byte(validDatasetDraft))
	processor := newDatasetProcessor(t, store, &fakeDatasetGateway{}, scenarioPolicy())
	processor.targets = singleTarget{&fakeValidationDB{execErr: context.DeadlineExceeded}}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	result, err := processor.validateDataset(ctx, timeoutStage())
	if !errors.Is(err, context.DeadlineExceeded) || result.Failure != nil {
		t.Fatalf("result=%#v error=%v, want the deadline error and no failure", result, err)
	}
}

// Cancelling the stage while the schema loads is an interruption. It must not reject the Dataset,
// which would spend a repair round, and it drops nothing.
func TestValidateDatasetInterruptedLoadIsNotARejectionAndDropsNothing(t *testing.T) {
	t.Parallel()

	store := datasetFakeStore(t)
	store.artifactsByKey[generation.ArtifactKindDataset+"/"+datasetDraftKey(0)] = canonicalArtifact(t,
		generation.ArtifactKindDataset, datasetDraftKey(0), []byte(validDatasetDraft))
	loader, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = loader.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	target := &cancelOnCreateTarget{fakeValidationDB: &fakeValidationDB{loader: loader}, cancel: cancel}
	processor := newDatasetProcessor(t, store, &fakeDatasetGateway{}, scenarioPolicy())
	processor.targets = singleTarget{target}

	result, err := processor.validateDataset(ctx, timeoutStage())
	if !errors.Is(err, context.Canceled) || result.Failure != nil || result.Transition.Event != "" {
		t.Fatalf("result=%#v error=%v, want the cancellation error", result, err)
	}
	if store.saved != nil {
		t.Fatalf("an interrupted load recorded a rejection: %#v", store.saved)
	}
	for _, call := range target.execCalls {
		if strings.Contains(strings.ToUpper(call), "DROP ") {
			t.Fatalf("an interrupted load dropped a Target object: %v", target.execCalls)
		}
	}
}
