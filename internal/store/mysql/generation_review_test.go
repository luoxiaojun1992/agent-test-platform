package mysqlstore

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"testing"
	"time"

	"agent-test-platform/internal/generation"

	"github.com/DATA-DOG/go-sqlmock"
)

func reviewLockRows(kind, status string, requested, accepted int, reviewStatus any) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"job_kind", "status", "requested_test_case_count", "accepted_test_case_count", "repair_round",
		"require_manual_review", "review_status", "input_scenario_revision_id", "input_dataset_revision_id",
	}).AddRow(kind, status, requested, accepted, 0, true, reviewStatus, mockScenarioRevisionID, mockDatasetRevisionID)
}

func newReviewMock(t *testing.T, ids ...string) (*GenerationStore, sqlmock.Sqlmock) {
	t.Helper()
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store, err := newGenerationStore(database, sequenceIDs(ids...))
	if err != nil {
		t.Fatal(err)
	}
	return store, mock
}

func TestReviewGenerationJobApproveEnqueuesPublish(t *testing.T) {
	t.Parallel()

	store, mock := newReviewMock(t, nextWorkID)
	now := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	payload, err := json.Marshal(generationWorkPayload{
		JobID: generationJobID, JobKind: "TEST_CASES",
		InputScenarioRevisionID: ptr(mockScenarioRevisionID), InputDatasetRevisionID: ptr(mockDatasetRevisionID),
	})
	if err != nil {
		t.Fatal(err)
	}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationJobForReviewSQL)).WithArgs(generationJobID).WillReturnRows(
		reviewLockRows("TEST_CASES", "AWAITING_REVIEW", 1, 1, "PENDING"),
	)
	mock.ExpectExec(regexp.QuoteMeta(approveGenerationReviewSQL)).WithArgs("looks good", generationJobID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(insertGenerationWorkSQL)).WithArgs(
		nextWorkID, generationJobID, "PUBLISHING:repair-0", 0, 3, payload,
	).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(regexp.QuoteMeta(selectGenerationJobSQL)).WithArgs(generationJobID).WillReturnRows(
		generationJobRows("PUBLISHING", 1, 1, 0, now),
	)
	mock.ExpectCommit()

	job, err := store.ReviewGenerationJob(context.Background(), generationJobID, "review-key-1",
		generation.ReviewInput{Decision: generation.ReviewApprove, Note: "looks good"})
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != generation.StatusPublishing {
		t.Fatalf("job = %#v", job)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReviewGenerationJobRejectFailsJobAndPipeline(t *testing.T) {
	t.Parallel()

	store, mock := newReviewMock(t)
	now := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	failureJSON, err := json.Marshal(generation.JobFailure{Code: "REVIEW_REJECTED", Message: "wrong data", Retryable: false})
	if err != nil {
		t.Fatal(err)
	}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationJobForReviewSQL)).WithArgs(generationJobID).WillReturnRows(
		reviewLockRows("TEST_CASES", "AWAITING_REVIEW", 1, 1, "PENDING"),
	)
	mock.ExpectExec(regexp.QuoteMeta(rejectGenerationReviewSQL)).WithArgs(failureJSON, "wrong data", generationJobID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(settleGenerationPipelineSQL)).WithArgs("FAILED", generationJobID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(selectGenerationJobSQL)).WithArgs(generationJobID).WillReturnRows(
		generationJobRows("FAILED", 1, 1, 0, now),
	)
	mock.ExpectCommit()

	job, err := store.ReviewGenerationJob(context.Background(), generationJobID, "review-key-1",
		generation.ReviewInput{Decision: generation.ReviewReject, Note: "wrong data"})
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != generation.StatusFailed {
		t.Fatalf("job = %#v", job)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReviewGenerationJobRejectWithoutNoteUsesDefaultMessage(t *testing.T) {
	t.Parallel()

	store, mock := newReviewMock(t)
	now := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	failureJSON, err := json.Marshal(generation.JobFailure{Code: "REVIEW_REJECTED", Message: "Rejected during manual review.", Retryable: false})
	if err != nil {
		t.Fatal(err)
	}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationJobForReviewSQL)).WithArgs(generationJobID).WillReturnRows(
		reviewLockRows("TEST_CASES", "AWAITING_REVIEW", 1, 1, "PENDING"),
	)
	mock.ExpectExec(regexp.QuoteMeta(rejectGenerationReviewSQL)).WithArgs(failureJSON, nil, generationJobID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(settleGenerationPipelineSQL)).WithArgs("FAILED", generationJobID).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(selectGenerationJobSQL)).WithArgs(generationJobID).WillReturnRows(
		generationJobRows("FAILED", 1, 1, 0, now),
	)
	mock.ExpectCommit()

	if _, err := store.ReviewGenerationJob(context.Background(), generationJobID, "review-key-1",
		generation.ReviewInput{Decision: generation.ReviewReject}); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReviewGenerationJobReplayReturnsJobWithoutWriting(t *testing.T) {
	t.Parallel()

	store, mock := newReviewMock(t)
	now := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationJobForReviewSQL)).WithArgs(generationJobID).WillReturnRows(
		reviewLockRows("TEST_CASES", "PUBLISHING", 1, 1, "APPROVED"),
	)
	mock.ExpectQuery(regexp.QuoteMeta(selectGenerationJobSQL)).WithArgs(generationJobID).WillReturnRows(
		generationJobRows("PUBLISHING", 1, 1, 0, now),
	)
	mock.ExpectCommit()

	job, err := store.ReviewGenerationJob(context.Background(), generationJobID, "review-key-2",
		generation.ReviewInput{Decision: generation.ReviewApprove})
	if err != nil || job.Status != generation.StatusPublishing {
		t.Fatalf("job=%#v error=%v", job, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReviewGenerationJobConflicts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		status       string
		reviewStatus any
		decision     generation.ReviewDecision
	}{
		{name: "never awaited review", status: "GENERATING_DATASET", reviewStatus: nil, decision: generation.ReviewApprove},
		{name: "reject after approve", status: "PUBLISHING", reviewStatus: "APPROVED", decision: generation.ReviewReject},
		{name: "approve after reject", status: "FAILED", reviewStatus: "REJECTED", decision: generation.ReviewApprove},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store, mock := newReviewMock(t)
			mock.ExpectBegin()
			mock.ExpectQuery(regexp.QuoteMeta(lockGenerationJobForReviewSQL)).WithArgs(generationJobID).WillReturnRows(
				reviewLockRows("SCENARIO", test.status, 0, 0, test.reviewStatus),
			)
			mock.ExpectRollback()

			_, err := store.ReviewGenerationJob(context.Background(), generationJobID, "review-key-3", generation.ReviewInput{Decision: test.decision})
			if !errors.Is(err, generation.ErrConflict) {
				t.Fatalf("error = %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReviewGenerationJobNotFoundAndInvalidInput(t *testing.T) {
	t.Parallel()

	store, mock := newReviewMock(t)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationJobForReviewSQL)).WithArgs(generationJobID).WillReturnRows(
		sqlmock.NewRows([]string{
			"job_kind", "status", "requested_test_case_count", "accepted_test_case_count", "repair_round",
			"require_manual_review", "review_status", "input_scenario_revision_id", "input_dataset_revision_id",
		}),
	)
	mock.ExpectRollback()
	if _, err := store.ReviewGenerationJob(context.Background(), generationJobID, "review-key-4",
		generation.ReviewInput{Decision: generation.ReviewApprove}); !errors.Is(err, generation.ErrNotFound) {
		t.Fatalf("missing job error = %v", err)
	}

	// None of these reach the database: no Begin is expected on the mock.
	approve := generation.ReviewInput{Decision: generation.ReviewApprove}
	if _, err := store.ReviewGenerationJob(context.Background(), "not-a-ulid", "review-key-4", approve); !errors.Is(err, generation.ErrNotFound) {
		t.Fatalf("malformed ID error = %v", err)
	}
	if _, err := store.ReviewGenerationJob(context.Background(), generationJobID, " ", approve); err == nil {
		t.Fatal("blank idempotency key was accepted")
	}
	if _, err := store.ReviewGenerationJob(context.Background(), generationJobID, "review-key-4", generation.ReviewInput{Decision: "MAYBE"}); err == nil {
		t.Fatal("unknown decision was accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAdvanceGenerationToAwaitingReviewEnqueuesNothing(t *testing.T) {
	t.Parallel()

	store, mock := newReviewMock(t, nextWorkID)
	lease := GenerationLease{WorkItemID: generationWorkID, Owner: "worker", FencingToken: 2}

	// No insertGenerationWorkSQL is expected: sqlmock fails the test if one is attempted.
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationJobSQL)).WithArgs(generationJobID).WillReturnRows(lockJobRows("SCENARIO", "VALIDATING_SCENARIO", 0, 0, 0, true))
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationLeaseSQL)).WithArgs(generationWorkID, "worker", uint64(2)).WillReturnRows(sqlmock.NewRows([]string{"aggregate_type", "aggregate_id"}).AddRow("generation_job", generationJobID))
	mock.ExpectExec(regexp.QuoteMeta(updateGenerationJobSQL)).WithArgs("AWAITING_REVIEW", "AWAITING_REVIEW", 0, 0, generation.GenerationJobBudget.Microseconds(), false, generationJobID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(completeGenerationWorkSQL)).WithArgs(generationWorkID, "worker", uint64(2)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(markGenerationReviewPendingSQL)).WithArgs(generationJobID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	next, err := store.AdvanceGeneration(context.Background(), generationJobID, lease, generation.TransitionInput{Event: generation.EventValidationAccepted})
	if err != nil {
		t.Fatal(err)
	}
	if next.Status != generation.StatusAwaitingReview {
		t.Fatalf("next = %#v", next)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAdvanceGenerationWithoutReviewGoesStraightToPublishing(t *testing.T) {
	t.Parallel()

	store, mock := newReviewMock(t, nextWorkID)
	lease := GenerationLease{WorkItemID: generationWorkID, Owner: "worker", FencingToken: 2}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationJobSQL)).WithArgs(generationJobID).WillReturnRows(lockJobRows("SCENARIO", "VALIDATING_SCENARIO", 0, 0, 0, false))
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationLeaseSQL)).WithArgs(generationWorkID, "worker", uint64(2)).WillReturnRows(sqlmock.NewRows([]string{"aggregate_type", "aggregate_id"}).AddRow("generation_job", generationJobID))
	mock.ExpectExec(regexp.QuoteMeta(updateGenerationJobSQL)).WithArgs("PUBLISHING", "PUBLISHING", 0, 0, generation.GenerationJobBudget.Microseconds(), false, generationJobID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(completeGenerationWorkSQL)).WithArgs(generationWorkID, "worker", uint64(2)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(insertGenerationWorkSQL)).WithArgs(nextWorkID, generationJobID, "PUBLISHING:repair-0", 0, 3, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	next, err := store.AdvanceGeneration(context.Background(), generationJobID, lease, generation.TransitionInput{Event: generation.EventValidationAccepted})
	if err != nil || next.Status != generation.StatusPublishing {
		t.Fatalf("next=%#v error=%v", next, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAdvanceGenerationFailureSettlesPipeline(t *testing.T) {
	t.Parallel()

	store, mock := newReviewMock(t)
	lease := GenerationLease{WorkItemID: generationWorkID, Owner: "worker", FencingToken: 2}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationJobSQL)).WithArgs(generationJobID).WillReturnRows(lockJobRows("TEST_CASES", "VALIDATING_TEST_CASES", 2, 0, 0, false))
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationLeaseSQL)).WithArgs(generationWorkID, "worker", uint64(2)).WillReturnRows(sqlmock.NewRows([]string{"aggregate_type", "aggregate_id"}).AddRow("generation_job", generationJobID))
	mock.ExpectExec(regexp.QuoteMeta(updateGenerationJobSQL)).WithArgs("FAILED", "", 0, 0, generation.GenerationJobBudget.Microseconds(), true, generationJobID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(completeGenerationWorkSQL)).WithArgs(generationWorkID, "worker", uint64(2)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(settleGenerationPipelineSQL)).WithArgs("FAILED", generationJobID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	next, err := store.AdvanceGeneration(context.Background(), generationJobID, lease,
		generation.TransitionInput{Event: generation.EventValidationRejected, AllErrorsRetryable: false})
	if err != nil || next.Status != generation.StatusFailed {
		t.Fatalf("next=%#v error=%v", next, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
