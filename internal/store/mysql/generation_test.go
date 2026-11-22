package mysqlstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"agent-test-platform/internal/generation"
	"agent-test-platform/internal/jobs/mysqlqueue"

	"github.com/DATA-DOG/go-sqlmock"
)

const (
	generationRequestID    = "01K4Z6Y7X8W9V0T1S2R3Q4P5N5"
	generationJobID        = "01K4Z6Y7X8W9V0T1S2R3Q4P5N6"
	generationWorkID       = "01K4Z6Y7X8W9V0T1S2R3Q4P5N7"
	nextWorkID             = "01K4Z6Y7X8W9V0T1S2R3Q4P5N8"
	unusedRequestID        = "01K4Z6Y7X8W9V0T1S2R3Q4P5M4"
	unusedJobID            = "01K4Z6Y7X8W9V0T1S2R3Q4P5M5"
	unusedWorkID           = "01K4Z6Y7X8W9V0T1S2R3Q4P5M6"
	mockScenarioRevisionID = "01K4Z6Y7X8W9V0T1S2R3Q4P5P1"
	mockDatasetRevisionID  = "01K4Z6Y7X8W9V0T1S2R3Q4P5P2"
)

func TestCreateGenerationJobAtomicallyEnqueuesInitialWork(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := newGenerationStore(database, sequenceIDs(generationRequestID, generationJobID, generationWorkID))
	input := validCreateGenerationJobInput()
	input.Kind = generation.KindTestCases
	input.RequestedTestCaseCount = 3
	input.InputScenarioRevisionID = ptr(mockScenarioRevisionID)
	input.InputDatasetRevisionID = ptr(mockDatasetRevisionID)
	now := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	workPayload, err := json.Marshal(generationWorkPayload{
		JobID: generationJobID, JobKind: "TEST_CASES",
		InputScenarioRevisionID: input.InputScenarioRevisionID, InputDatasetRevisionID: input.InputDatasetRevisionID,
	})
	if err != nil {
		t.Fatal(err)
	}

	expectRegisteredTarget(mock, seededValidationDBID)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(upsertGenerationRequestSQL)).WithArgs(
		generationRequestID, input.IdempotencyKey, []byte(input.Request), input.RequestSHA256, input.DescriptionTokens,
		input.TargetAgentVersionID, input.GenerateDataset, input.DatabaseProfileID,
	).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationRequestSQL)).WithArgs(input.IdempotencyKey).WillReturnRows(
		sqlmock.NewRows([]string{"id", "request_sha256"}).AddRow(generationRequestID, input.RequestSHA256),
	)
	mock.ExpectExec(regexp.QuoteMeta(insertGenerationJobSQL)).WithArgs(
		generationJobID, generationRequestID, string(generation.KindTestCases), input.InputScenarioRevisionID, input.InputDatasetRevisionID,
		input.RequestedTestCaseCount, input.GeneratorConfigSHA256, input.RequireManualReview,
	).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta(insertGenerationWorkSQL)).WithArgs(
		generationWorkID, generationJobID, "PENDING:repair-0", 0, 3,
		workPayload,
	).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(regexp.QuoteMeta(selectGenerationJobByRequestSQL)).WithArgs(generationRequestID).WillReturnRows(
		generationJobRows("PENDING", 3, 0, 0, now),
	)
	mock.ExpectCommit()

	job, err := store.CreateGenerationJob(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if job.ID != generationJobID || job.Status != generation.StatusPending || job.RequestedTestCaseCount != 3 {
		t.Fatalf("job = %#v", job)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCreateGenerationJobReturnsIdempotentReplay(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := newGenerationStore(database, sequenceIDs(unusedRequestID, unusedJobID, unusedWorkID))
	input := validCreateGenerationJobInput()
	now := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)

	expectRegisteredTarget(mock, seededValidationDBID)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(upsertGenerationRequestSQL)).WithArgs(
		unusedRequestID, input.IdempotencyKey, []byte(input.Request), input.RequestSHA256, input.DescriptionTokens,
		input.TargetAgentVersionID, input.GenerateDataset, input.DatabaseProfileID,
	).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationRequestSQL)).WithArgs(input.IdempotencyKey).WillReturnRows(
		sqlmock.NewRows([]string{"id", "request_sha256"}).AddRow(generationRequestID, input.RequestSHA256),
	)
	mock.ExpectQuery(regexp.QuoteMeta(selectGenerationJobByRequestSQL)).WithArgs(generationRequestID).WillReturnRows(
		generationJobRows("GENERATING_SCENARIO", 3, 0, 0, now),
	)
	mock.ExpectCommit()

	job, err := store.CreateGenerationJob(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if job.ID != generationJobID || job.Status != generation.StatusGeneratingScenario {
		t.Fatalf("job = %#v", job)
	}
}

func TestCreateGenerationJobRejectsIdempotencyConflict(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := newGenerationStore(database, sequenceIDs(unusedRequestID, unusedJobID, unusedWorkID))
	input := validCreateGenerationJobInput()

	expectRegisteredTarget(mock, seededValidationDBID)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(upsertGenerationRequestSQL)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationRequestSQL)).WithArgs(input.IdempotencyKey).WillReturnRows(
		sqlmock.NewRows([]string{"id", "request_sha256"}).AddRow(generationRequestID, strings.Repeat("b", 64)),
	)
	mock.ExpectRollback()

	_, err = store.CreateGenerationJob(context.Background(), input)
	if !errors.Is(err, generation.ErrIdempotencyConflict) {
		t.Fatalf("error = %v", err)
	}
}

func TestCancelGenerationJobStopsWork(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := newGenerationStore(database, sequenceIDs())
	now := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	updated := now.Add(time.Minute)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationJobDetailsSQL)).WithArgs(generationJobID).WillReturnRows(
		generationJobRows("GENERATING_DATASET", 3, 0, 0, now),
	)
	mock.ExpectExec(regexp.QuoteMeta(cancelGenerationJobSQL)).WithArgs(generationJobID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(cancelGenerationWorkSQL)).WithArgs(generationJobID).WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(regexp.QuoteMeta(settleGenerationPipelineSQL)).WithArgs("CANCELLED", generationJobID).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(selectGenerationJobSQL)).WithArgs(generationJobID).WillReturnRows(
		generationJobRowsWithUpdatedAt("CANCELLED", 3, 0, 0, now, updated),
	)
	mock.ExpectCommit()

	job, err := store.CancelGenerationJob(context.Background(), generationJobID, "cancel-key")
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != generation.StatusCancelled || !job.UpdatedAt.Equal(updated) {
		t.Fatalf("job = %#v", job)
	}
}

func TestCancelGenerationJobIsIdempotent(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := newGenerationStore(database, sequenceIDs())
	now := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationJobDetailsSQL)).WithArgs(generationJobID).WillReturnRows(
		generationJobRows("CANCELLED", 3, 0, 0, now),
	)
	mock.ExpectCommit()

	job, err := store.CancelGenerationJob(context.Background(), generationJobID, "another-cancel-key")
	if err != nil || job.Status != generation.StatusCancelled {
		t.Fatalf("job=%#v error=%v", job, err)
	}
}

func TestCancelGenerationJobRejectsPublishing(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := newGenerationStore(database, sequenceIDs())
	now := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationJobDetailsSQL)).WithArgs(generationJobID).WillReturnRows(
		generationJobRows("PUBLISHING", 3, 3, 0, now),
	)
	mock.ExpectRollback()

	_, err = store.CancelGenerationJob(context.Background(), generationJobID, "cancel-key")
	if !errors.Is(err, generation.ErrConflict) {
		t.Fatalf("error = %v", err)
	}
}

func validCreateGenerationJobInput() generation.CreateJobInput {
	return generation.CreateJobInput{
		IdempotencyKey:        "generation-request-key",
		Kind:                  generation.KindScenario,
		Request:               []byte(`{"description":"Generate cancellation tests.","test_case_count":3}`),
		RequestSHA256:         strings.Repeat("a", 64),
		DescriptionTokens:     8,
		GeneratorConfigSHA256: strings.Repeat("c", 64),
		GenerateDataset:       true,
		ValidationDBIDs:       []string{"0b6d6f3e-2c4a-4f8e-9a1b-7c5d3e2f1a90"},
	}
}

// expectRegisteredTarget expects the validation MySQL registry check that runs before a generation job is created.
func expectRegisteredTarget(mock sqlmock.Sqlmock, targetID string) {
	mock.ExpectQuery(regexp.QuoteMeta(selectRegisteredTargetSQL)).WithArgs(targetID).WillReturnRows(
		sqlmock.NewRows([]string{"id"}).AddRow(targetID),
	)
}

func ptr(value string) *string { return &value }

func TestValidateCreateGenerationJobDescriptionTokenBoundary(t *testing.T) {
	t.Parallel()

	input := validCreateGenerationJobInput()
	input.DescriptionTokens = generation.MaxDescriptionTokens
	if err := validateCreateGenerationJob(input); err != nil {
		t.Fatalf("maximum token count rejected: %v", err)
	}
	input.DescriptionTokens++
	if err := validateCreateGenerationJob(input); err == nil {
		t.Fatal("token count above maximum was accepted")
	}
}

func sequenceIDs(ids ...string) IDGenerator {
	index := 0
	return func() (string, error) {
		if index >= len(ids) {
			return "", fmt.Errorf("ID sequence exhausted")
		}
		id := ids[index]
		index++
		return id, nil
	}
}

func generationJobRows(status string, requested, accepted, repair int, timestamp time.Time) *sqlmock.Rows {
	return generationJobRowsWithUpdatedAt(status, requested, accepted, repair, timestamp, timestamp)
}

func generationJobRowsWithUpdatedAt(status string, requested, accepted, repair int, createdAt, updatedAt time.Time) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "job_kind", "pipeline_id", "require_manual_review", "status", "requested_test_case_count", "accepted_test_case_count",
		"repair_round", "error_json", "created_at", "updated_at",
	}).AddRow(generationJobID, "TEST_CASES", nil, false, status, requested, accepted, repair, nil, createdAt, updatedAt)
}

// lockJobRows is lockGenerationJobSQL's result row; the input revision references are NULL.
func lockJobRows(kind, status string, requested, accepted, repair int, requireManualReview bool) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"job_kind", "status", "requested_test_case_count", "accepted_test_case_count", "repair_round",
		"input_scenario_revision_id", "input_dataset_revision_id", "require_manual_review",
	}).AddRow(kind, status, requested, accepted, repair, nil, nil, requireManualReview)
}

func TestFailGenerationSchedulesRetry(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := newGenerationStore(database, sequenceIDs())
	lease := GenerationLease{WorkItemID: generationWorkID, Owner: "worker", FencingToken: 2, AttemptCount: 1, MaxAttempts: 3}
	failure := generation.JobFailure{Code: "PROVIDER_UNAVAILABLE", Message: "Provider is temporarily unavailable.", Retryable: true}
	failureJSON, _ := json.Marshal(failure)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationJobSQL)).WithArgs(generationJobID).WillReturnRows(lockJobRows("TEST_CASES", "GENERATING_SCENARIO", 3, 0, 0, false))
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationLeaseSQL)).WithArgs(generationWorkID, "worker", uint64(2)).WillReturnRows(sqlmock.NewRows([]string{"aggregate_type", "aggregate_id"}).AddRow("generation_job", generationJobID))
	mock.ExpectExec(regexp.QuoteMeta(retryGenerationWorkSQL)).WithArgs(int64((2 * time.Second).Microseconds()), failureJSON, generationWorkID, "worker", uint64(2)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	terminal, err := store.FailGeneration(context.Background(), generationJobID, lease, failure, 2*time.Second)
	if err != nil || terminal {
		t.Fatalf("terminal=%v error=%v", terminal, err)
	}
}

func TestFailGenerationTerminallyFailsJobAndWork(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := newGenerationStore(database, sequenceIDs())
	lease := GenerationLease{WorkItemID: generationWorkID, Owner: "worker", FencingToken: 3, AttemptCount: 3, MaxAttempts: 3}
	failure := generation.JobFailure{Code: "STAGE_ATTEMPTS_EXHAUSTED", Message: "Generation stage attempts were exhausted.", Retryable: true}
	failureJSON, _ := json.Marshal(failure)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationJobSQL)).WithArgs(generationJobID).WillReturnRows(lockJobRows("TEST_CASES", "GENERATING_DATASET", 3, 0, 0, false))
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationLeaseSQL)).WithArgs(generationWorkID, "worker", uint64(3)).WillReturnRows(sqlmock.NewRows([]string{"aggregate_type", "aggregate_id"}).AddRow("generation_job", generationJobID))
	mock.ExpectExec(regexp.QuoteMeta(failGenerationJobSQL)).WithArgs(failureJSON, generationJobID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(settleGenerationPipelineSQL)).WithArgs("FAILED", generationJobID).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(deadGenerationWorkSQL)).WithArgs(failureJSON, generationWorkID, "worker", uint64(3)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	terminal, err := store.FailGeneration(context.Background(), generationJobID, lease, failure, 0)
	if err != nil || !terminal {
		t.Fatalf("terminal=%v error=%v", terminal, err)
	}
}

func TestFailGenerationRejectsStaleLease(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := newGenerationStore(database, sequenceIDs())
	lease := GenerationLease{WorkItemID: generationWorkID, Owner: "old-worker", FencingToken: 1, AttemptCount: 1, MaxAttempts: 3}
	failure := generation.JobFailure{Code: "PROVIDER_UNAVAILABLE", Message: "Provider is temporarily unavailable.", Retryable: true}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationJobSQL)).WithArgs(generationJobID).WillReturnRows(lockJobRows("TEST_CASES", "GENERATING_SCENARIO", 3, 0, 0, false))
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationLeaseSQL)).WithArgs(generationWorkID, "old-worker", uint64(1)).WillReturnRows(sqlmock.NewRows([]string{"aggregate_type", "aggregate_id"}))
	mock.ExpectRollback()

	_, err = store.FailGeneration(context.Background(), generationJobID, lease, failure, time.Second)
	if !errors.Is(err, mysqlqueue.ErrStaleLease) {
		t.Fatalf("error = %v", err)
	}
}

func TestAdvanceGenerationEnqueuesNextStage(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := newGenerationStore(database, func() (string, error) { return nextWorkID, nil })
	lease := GenerationLease{WorkItemID: generationWorkID, Owner: "worker-1", FencingToken: 2}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationJobSQL)).WithArgs(generationJobID).WillReturnRows(lockJobRows("TEST_CASES", "PENDING", 3, 0, 0, false))
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationLeaseSQL)).WithArgs(generationWorkID, "worker-1", uint64(2)).WillReturnRows(sqlmock.NewRows([]string{"aggregate_type", "aggregate_id"}).AddRow("generation_job", generationJobID))
	mock.ExpectExec(regexp.QuoteMeta(updateGenerationJobSQL)).WithArgs("VALIDATING_INPUT", "VALIDATING_INPUT", 0, 0, generation.GenerationJobBudget.Microseconds(), false, generationJobID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(completeGenerationWorkSQL)).WithArgs(generationWorkID, "worker-1", uint64(2)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(insertGenerationWorkSQL)).WithArgs(nextWorkID, generationJobID, "VALIDATING_INPUT:repair-0", 0, 3, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	next, err := store.AdvanceGeneration(context.Background(), generationJobID, lease, generation.TransitionInput{Event: generation.EventInputAccepted})
	if err != nil {
		t.Fatal(err)
	}
	if next.Status != generation.StatusValidatingInput {
		t.Fatalf("next = %#v", next)
	}
}

func TestAdvanceGenerationTerminalDoesNotEnqueue(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := newGenerationStore(database, func() (string, error) { return nextWorkID, nil })
	lease := GenerationLease{WorkItemID: generationWorkID, Owner: "worker", FencingToken: 7}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationJobSQL)).WithArgs(generationJobID).WillReturnRows(lockJobRows("TEST_CASES", "PUBLISHING", 1, 1, 0, false))
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationLeaseSQL)).WithArgs(generationWorkID, "worker", uint64(7)).WillReturnRows(sqlmock.NewRows([]string{"aggregate_type", "aggregate_id"}).AddRow("generation_job", generationJobID))
	mock.ExpectExec(regexp.QuoteMeta(updateGenerationJobSQL)).WithArgs("COMPLETED", "", 1, 0, generation.GenerationJobBudget.Microseconds(), true, generationJobID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(completeGenerationWorkSQL)).WithArgs(generationWorkID, "worker", uint64(7)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	next, err := store.AdvanceGeneration(context.Background(), generationJobID, lease, generation.TransitionInput{Event: generation.EventPublishCommitted})
	if err != nil {
		t.Fatal(err)
	}
	if next.Status != generation.StatusCompleted {
		t.Fatalf("next = %#v", next)
	}
}

func TestAdvanceGenerationRejectsStaleLease(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := newGenerationStore(database, func() (string, error) { return nextWorkID, nil })
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationJobSQL)).WithArgs(generationJobID).WillReturnRows(lockJobRows("TEST_CASES", "PENDING", 1, 0, 0, false))
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationLeaseSQL)).WithArgs(generationWorkID, "old-worker", uint64(1)).WillReturnRows(sqlmock.NewRows([]string{"aggregate_type", "aggregate_id"}))
	mock.ExpectRollback()

	_, err = store.AdvanceGeneration(context.Background(), generationJobID, GenerationLease{WorkItemID: generationWorkID, Owner: "old-worker", FencingToken: 1}, generation.TransitionInput{Event: generation.EventInputAccepted})
	if !errors.Is(err, mysqlqueue.ErrStaleLease) {
		t.Fatalf("error = %v", err)
	}
}

func TestAdvanceGenerationRollsBackInvalidTransition(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := newGenerationStore(database, func() (string, error) { return nextWorkID, nil })
	lease := GenerationLease{WorkItemID: generationWorkID, Owner: "worker", FencingToken: 2}
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationJobSQL)).WithArgs(generationJobID).WillReturnRows(lockJobRows("TEST_CASES", "PENDING", 1, 0, 0, false))
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationLeaseSQL)).WithArgs(generationWorkID, "worker", uint64(2)).WillReturnRows(sqlmock.NewRows([]string{"aggregate_type", "aggregate_id"}).AddRow("generation_job", generationJobID))
	mock.ExpectRollback()

	if _, err := store.AdvanceGeneration(context.Background(), generationJobID, lease, generation.TransitionInput{Event: generation.EventDatasetStored}); err == nil {
		t.Fatal("invalid transition succeeded")
	}
}
