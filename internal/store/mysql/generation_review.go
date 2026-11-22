package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"agent-test-platform/internal/generation"

	"github.com/oklog/ulid/v2"
)

const (
	lockGenerationJobForReviewSQL = `SELECT job_kind, status, requested_test_case_count, accepted_test_case_count,
repair_round, require_manual_review, review_status, input_scenario_revision_id, input_dataset_revision_id
FROM generation_job WHERE id = ? FOR UPDATE`
	approveGenerationReviewSQL = `UPDATE generation_job
SET status = 'PUBLISHING', current_stage = 'PUBLISHING', review_status = 'APPROVED', review_note = ?,
    reviewed_at = UTC_TIMESTAMP(6), updated_at = UTC_TIMESTAMP(6)
WHERE id = ? AND status = 'AWAITING_REVIEW'`
	rejectGenerationReviewSQL = `UPDATE generation_job
SET status = 'FAILED', current_stage = NULL, error_json = ?, review_status = 'REJECTED', review_note = ?,
    reviewed_at = UTC_TIMESTAMP(6), finished_at = UTC_TIMESTAMP(6), updated_at = UTC_TIMESTAMP(6)
WHERE id = ? AND status = 'AWAITING_REVIEW'`
)

// ReviewGenerationJob records a human decision on a Job paused in AWAITING_REVIEW. Approving
// moves it to PUBLISHING and enqueues the publish work item for a Worker to consume; rejecting
// fails the Job (and its Generation Pipeline) with zero publication.
//
// The review binds to exactly the Job ID addressed and is never redirected to a later stage of
// the same Pipeline, so a stale or retried request can never approve content the reviewer did
// not see. Repeating a decision the Job already carries returns the Job unchanged.
func (store *GenerationStore) ReviewGenerationJob(ctx context.Context, id, idempotencyKey string, input generation.ReviewInput) (generation.Job, error) {
	if _, err := ulid.ParseStrict(id); err != nil {
		return generation.Job{}, generation.ErrNotFound
	}
	if strings.TrimSpace(idempotencyKey) == "" || len(idempotencyKey) > 128 {
		return generation.Job{}, fmt.Errorf("valid idempotency key is required")
	}
	if err := input.Validate(); err != nil {
		return generation.Job{}, err
	}

	transaction, err := store.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return generation.Job{}, fmt.Errorf("begin generation review: %w", err)
	}
	defer transaction.Rollback()

	state := generation.JobState{}
	var reviewStatus, inputScenarioRevisionID, inputDatasetRevisionID sql.NullString
	if err := transaction.QueryRowContext(ctx, lockGenerationJobForReviewSQL, id).Scan(
		&state.Kind, &state.Status, &state.RequestedTestCaseCount, &state.AcceptedTestCaseCount,
		&state.RepairRound, &state.RequireManualReview, &reviewStatus, &inputScenarioRevisionID, &inputDatasetRevisionID,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return generation.Job{}, generation.ErrNotFound
		}
		return generation.Job{}, fmt.Errorf("lock generation job for review: %w", err)
	}

	decidedStatus, event := "APPROVED", generation.EventReviewApproved
	if input.Decision == generation.ReviewReject {
		decidedStatus, event = "REJECTED", generation.EventReviewRejected
	}
	if state.Status != generation.StatusAwaitingReview {
		if reviewStatus.Valid && reviewStatus.String == decidedStatus {
			return store.commitReviewedJob(ctx, transaction, id)
		}
		return generation.Job{}, fmt.Errorf("%w: job status %s is not AWAITING_REVIEW", generation.ErrConflict, state.Status)
	}
	next, err := generation.Transition(state, generation.TransitionInput{Event: event})
	if err != nil {
		return generation.Job{}, fmt.Errorf("%w: %v", generation.ErrConflict, err)
	}

	var note any
	if input.Note != "" {
		note = input.Note
	}
	if next.Status == generation.StatusPublishing {
		if err := store.approveReview(ctx, transaction, id, string(state.Kind), next, note, inputScenarioRevisionID, inputDatasetRevisionID); err != nil {
			return generation.Job{}, err
		}
	} else if err := store.rejectReview(ctx, transaction, id, input.Note, note); err != nil {
		return generation.Job{}, err
	}
	return store.commitReviewedJob(ctx, transaction, id)
}

func (store *GenerationStore) approveReview(
	ctx context.Context, transaction *sql.Tx, jobID, kind string, next generation.JobState, note any,
	inputScenarioRevisionID, inputDatasetRevisionID sql.NullString,
) error {
	workID, err := store.newID()
	if err != nil {
		return fmt.Errorf("generate publish work ID: %w", err)
	}
	if _, err := ulid.ParseStrict(workID); err != nil {
		return fmt.Errorf("generate publish work ID: %w", err)
	}
	payload, err := json.Marshal(generationWorkPayload{
		JobID: jobID, JobKind: kind,
		InputScenarioRevisionID: nullStringPtr(inputScenarioRevisionID), InputDatasetRevisionID: nullStringPtr(inputDatasetRevisionID),
	})
	if err != nil {
		return fmt.Errorf("encode publish work: %w", err)
	}
	result, err := transaction.ExecContext(ctx, approveGenerationReviewSQL, note, jobID)
	if err != nil {
		return fmt.Errorf("approve generation review: %w", err)
	}
	if err := requireSingleRow(result); err != nil {
		return fmt.Errorf("approve generation review: %w", err)
	}
	stageKey := fmt.Sprintf("%s:repair-%d", next.Status, next.RepairRound)
	if _, err := transaction.ExecContext(ctx, insertGenerationWorkSQL, workID, jobID, stageKey, 0, 3, payload); err != nil {
		return fmt.Errorf("enqueue publish work: %w", err)
	}
	return nil
}

func (store *GenerationStore) rejectReview(ctx context.Context, transaction *sql.Tx, jobID, rawNote string, note any) error {
	message := strings.TrimSpace(rawNote)
	if message == "" {
		message = "Rejected during manual review."
	}
	failure := generation.JobFailure{Code: "REVIEW_REJECTED", Message: message, Retryable: false}
	if err := failure.Validate(); err != nil {
		return err
	}
	failureJSON, err := json.Marshal(failure)
	if err != nil {
		return fmt.Errorf("encode review rejection: %w", err)
	}
	result, err := transaction.ExecContext(ctx, rejectGenerationReviewSQL, failureJSON, note, jobID)
	if err != nil {
		return fmt.Errorf("reject generation review: %w", err)
	}
	if err := requireSingleRow(result); err != nil {
		return fmt.Errorf("reject generation review: %w", err)
	}
	if _, err := transaction.ExecContext(ctx, settleGenerationPipelineSQL, "FAILED", jobID); err != nil {
		return fmt.Errorf("fail generation pipeline: %w", err)
	}
	return nil
}

func (store *GenerationStore) commitReviewedJob(ctx context.Context, transaction *sql.Tx, jobID string) (generation.Job, error) {
	job, err := scanGenerationJob(transaction.QueryRowContext(ctx, selectGenerationJobSQL, jobID))
	if err != nil {
		return generation.Job{}, fmt.Errorf("load reviewed generation job: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return generation.Job{}, fmt.Errorf("commit generation review: %w", err)
	}
	return job, nil
}
