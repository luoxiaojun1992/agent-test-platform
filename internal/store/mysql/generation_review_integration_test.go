package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"agent-test-platform/internal/generation"
	"agent-test-platform/migrations"

	"github.com/go-sql-driver/mysql"
)

// TestGenerationReviewGateLifecycle exercises the manual review gate against a real MySQL 8.4
// instance on a Generation Pipeline created with require_manual_review: a Job pauses in
// AWAITING_REVIEW without any queued work, approval re-queues it for a Worker to publish, the
// flag is inherited by the next chained stage, and rejecting a later stage fails the Job and
// its Pipeline while publishing nothing. It also pins the addressing rule: a review is bound to
// the exact Job ID it names and never reaches a later stage of the same Pipeline.
func TestGenerationReviewGateLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	database := openMigratedReviewDatabase(t, ctx)
	store, err := NewGenerationStore(database)
	if err != nil {
		t.Fatal(err)
	}

	requestDocument, err := json.Marshal(map[string]any{
		"description": "Generate cancellation tests.", "require_manual_review": true,
		"validation_db_ids": []string{seededValidationDBID},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, requestDigest, err := generation.CanonicalJSON(requestDocument)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateGenerationJob(ctx, generation.CreateJobInput{
		IdempotencyKey: "review-gate-" + randomPublishHex(t, 8),
		Request:        requestDocument, RequestSHA256: requestDigest, DescriptionTokens: 8,
		RequestedTestCaseCount: 2, GeneratorConfigSHA256: strings.Repeat("a", 64), GenerateDataset: true,
		RequireManualReview: true,
		ValidationDBIDs:     []string{seededValidationDBID},
	})
	if err != nil {
		t.Fatalf("create generation pipeline: %v", err)
	}
	if !created.RequireManualReview || created.PipelineID == nil {
		t.Fatalf("created = %#v, want require_manual_review and a pipeline", created)
	}
	scenarioJobID, pipelineID := created.ID, *created.PipelineID

	// SCENARIO validation passes: the Job must pause, with no work queued for it.
	setReviewJobStatus(t, ctx, database, scenarioJobID, "VALIDATING_SCENARIO")
	seedPublishDraft(t, ctx, database, scenarioJobID, "SCENARIO", "scenario", []byte(`{
		"business_rules": [{"id": "r1", "source": "SOURCE_REQUIREMENT"}]
	}`))
	validationLease := seedPublishWork(t, ctx, database, scenarioJobID, "VALIDATING_SCENARIO:repair-0")
	next, err := store.AdvanceGeneration(ctx, scenarioJobID, validationLease, generation.TransitionInput{Event: generation.EventValidationAccepted})
	if err != nil {
		t.Fatalf("advance scenario to review: %v", err)
	}
	if next.Status != generation.StatusAwaitingReview {
		t.Fatalf("next.Status = %s, want AWAITING_REVIEW", next.Status)
	}
	assertReviewJobColumns(t, ctx, database, scenarioJobID, "AWAITING_REVIEW", "PENDING")
	if count := countReviewWork(t, ctx, database, scenarioJobID, "AWAITING_REVIEW%", "PUBLISHING%"); count != 0 {
		t.Fatalf("%d work items were queued for a Job awaiting review", count)
	}

	// The reviewer can read exactly what an approval would publish.
	scenarioDraft, err := store.GetGenerationJobDraft(ctx, scenarioJobID)
	if err != nil {
		t.Fatalf("read scenario draft: %v", err)
	}
	if scenarioDraft.Kind != generation.KindScenario || !strings.Contains(string(scenarioDraft.Scenario), `"business_rules"`) ||
		scenarioDraft.Dataset != nil || scenarioDraft.TestCases != nil {
		t.Fatalf("scenario draft = %#v", scenarioDraft)
	}

	// Approval re-queues the Job for publishing.
	approved, err := store.ReviewGenerationJob(ctx, scenarioJobID, "review-key-001",
		generation.ReviewInput{Decision: generation.ReviewApprove, Note: "scenario reads right"})
	if err != nil {
		t.Fatalf("approve scenario: %v", err)
	}
	if approved.Status != generation.StatusPublishing {
		t.Fatalf("approved.Status = %s, want PUBLISHING", approved.Status)
	}
	assertReviewJobColumns(t, ctx, database, scenarioJobID, "PUBLISHING", "APPROVED")
	if _, err := store.GetGenerationJobDraft(ctx, scenarioJobID); !errors.Is(err, generation.ErrConflict) {
		t.Fatalf("draft read after approval error = %v, want ErrConflict", err)
	}
	var note sql.NullString
	var reviewedAt sql.NullTime
	if err := database.QueryRowContext(ctx, "SELECT review_note, reviewed_at FROM generation_job WHERE id = ?", scenarioJobID).Scan(&note, &reviewedAt); err != nil {
		t.Fatal(err)
	}
	if note.String != "scenario reads right" || !reviewedAt.Valid {
		t.Fatalf("review_note=%q reviewed_at valid=%v", note.String, reviewedAt.Valid)
	}
	publishLease := leaseReadyReviewWork(t, ctx, database, scenarioJobID, "PUBLISHING:repair-0")
	if _, err := store.PublishGeneration(ctx, scenarioJobID, publishLease); err != nil {
		t.Fatalf("publish approved scenario: %v", err)
	}
	assertPublishJobCompleted(t, ctx, database, scenarioJobID)

	// The chained DATASET Job inherits the review requirement.
	var datasetJobID string
	var inheritsReview bool
	if err := database.QueryRowContext(ctx, `SELECT job.id, job.require_manual_review FROM generation_pipeline AS pipeline
		JOIN generation_job AS job ON job.id = pipeline.dataset_job_id WHERE pipeline.id = ?`, pipelineID).Scan(&datasetJobID, &inheritsReview); err != nil {
		t.Fatalf("load chained dataset job: %v", err)
	}
	if !inheritsReview {
		t.Fatal("chained DATASET job did not inherit require_manual_review")
	}

	// A review aimed at the finished SCENARIO Job must not reach the DATASET Job: repeating its
	// own decision is a no-op replay, and the opposite decision conflicts.
	replay, err := store.ReviewGenerationJob(ctx, scenarioJobID, "review-key-002", generation.ReviewInput{Decision: generation.ReviewApprove})
	if err != nil || replay.ID != scenarioJobID || replay.Status != generation.StatusCompleted {
		t.Fatalf("replay = %#v err=%v", replay, err)
	}
	if _, err := store.ReviewGenerationJob(ctx, scenarioJobID, "review-key-003", generation.ReviewInput{Decision: generation.ReviewReject}); !errors.Is(err, generation.ErrConflict) {
		t.Fatalf("stale reject error = %v, want ErrConflict", err)
	}
	assertReviewJobColumns(t, ctx, database, datasetJobID, "PENDING", "")

	// DATASET validation passes: it pauses too, and polling the entry ID now shows it.
	setReviewJobStatus(t, ctx, database, datasetJobID, "VALIDATING_DATASET")
	seedPublishDraft(t, ctx, database, datasetJobID, "DATASET", "dataset", []byte(`{
		"schema_sql": "CREATE TABLE orders (id VARCHAR(32) PRIMARY KEY)",
		"seed_sql": "INSERT INTO orders (id) VALUES ('O1')",
		"constraints": {"version": "1.0", "checks": []},
		"metadata": {"schema_version": "1.0", "tables": []}
	}`))
	datasetValidationLease := seedPublishWork(t, ctx, database, datasetJobID, "VALIDATING_DATASET:repair-0")
	if next, err := store.AdvanceGeneration(ctx, datasetJobID, datasetValidationLease, generation.TransitionInput{Event: generation.EventValidationAccepted}); err != nil || next.Status != generation.StatusAwaitingReview {
		t.Fatalf("advance dataset to review: next=%#v err=%v", next, err)
	}
	polled, err := store.GetGenerationJob(ctx, scenarioJobID)
	if err != nil || polled.ID != datasetJobID || polled.Status != generation.StatusAwaitingReview || !polled.RequireManualReview {
		t.Fatalf("polled = %#v err=%v", polled, err)
	}

	// The draft read is bound to the stage Job too: the finished SCENARIO entry Job has none.
	if _, err := store.GetGenerationJobDraft(ctx, scenarioJobID); !errors.Is(err, generation.ErrConflict) {
		t.Fatalf("entry Job draft read error = %v, want ErrConflict", err)
	}
	var scenarioRevisionID string
	if err := database.QueryRowContext(ctx, "SELECT id FROM scenario_revision WHERE generation_job_id = ?", scenarioJobID).Scan(&scenarioRevisionID); err != nil {
		t.Fatal(err)
	}
	datasetDraft, err := store.GetGenerationJobDraft(ctx, datasetJobID)
	if err != nil {
		t.Fatalf("read dataset draft: %v", err)
	}
	if datasetDraft.Kind != generation.KindDataset || !strings.Contains(string(datasetDraft.Dataset), "CREATE TABLE orders") ||
		datasetDraft.Scenario != nil || datasetDraft.InputScenarioRevisionID == nil || *datasetDraft.InputScenarioRevisionID != scenarioRevisionID {
		t.Fatalf("dataset draft = %#v", datasetDraft)
	}

	// Rejecting the DATASET stage fails it and the Pipeline, and publishes nothing.
	rejected, err := store.ReviewGenerationJob(ctx, datasetJobID, "review-key-004",
		generation.ReviewInput{Decision: generation.ReviewReject, Note: "schema is wrong"})
	if err != nil {
		t.Fatalf("reject dataset: %v", err)
	}
	if rejected.Status != generation.StatusFailed || rejected.Error == nil || rejected.Error.Code != "REVIEW_REJECTED" || rejected.Error.Message != "schema is wrong" {
		t.Fatalf("rejected = %#v", rejected)
	}
	assertReviewJobColumns(t, ctx, database, datasetJobID, "FAILED", "REJECTED")
	if _, err := store.GetGenerationJobDraft(ctx, datasetJobID); !errors.Is(err, generation.ErrConflict) {
		t.Fatalf("draft read after rejection error = %v, want ErrConflict", err)
	}
	var pipelineStatus string
	var pipelineErrorCode sql.NullString
	if err := database.QueryRowContext(ctx, `SELECT status, JSON_UNQUOTE(JSON_EXTRACT(error_json, '$.code'))
		FROM generation_pipeline WHERE id = ?`, pipelineID).Scan(&pipelineStatus, &pipelineErrorCode); err != nil {
		t.Fatal(err)
	}
	if pipelineStatus != "FAILED" || pipelineErrorCode.String != "REVIEW_REJECTED" {
		t.Fatalf("pipeline status=%q error code=%q", pipelineStatus, pipelineErrorCode.String)
	}
	assertPublishRowCount(t, ctx, database, "dataset_revision", 0)
	if count := countReviewWork(t, ctx, database, datasetJobID, "PUBLISHING%", "PUBLISHING%"); count != 0 {
		t.Fatalf("%d publish work items were queued for a rejected Job", count)
	}
	if again, err := store.ReviewGenerationJob(ctx, datasetJobID, "review-key-005", generation.ReviewInput{Decision: generation.ReviewReject}); err != nil || again.Status != generation.StatusFailed {
		t.Fatalf("repeated reject = %#v err=%v", again, err)
	}
	if _, err := store.ReviewGenerationJob(ctx, datasetJobID, "review-key-006", generation.ReviewInput{Decision: generation.ReviewApprove}); !errors.Is(err, generation.ErrConflict) {
		t.Fatalf("approve after reject error = %v, want ErrConflict", err)
	}
}

// TestGenerationJobEndingSettlesPipeline covers the other two ways a Pipeline stops early:
// a terminal stage failure and a cancellation.
func TestGenerationJobEndingSettlesPipeline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	database := openMigratedReviewDatabase(t, ctx)
	store, err := NewGenerationStore(database)
	if err != nil {
		t.Fatal(err)
	}

	newPipeline := func(t *testing.T) generation.Job {
		t.Helper()
		requestDocument, err := json.Marshal(map[string]any{
			"description":       "Generate tests " + randomPublishHex(t, 8),
			"validation_db_ids": []string{seededValidationDBID},
		})
		if err != nil {
			t.Fatal(err)
		}
		_, requestDigest, err := generation.CanonicalJSON(requestDocument)
		if err != nil {
			t.Fatal(err)
		}
		created, err := store.CreateGenerationJob(ctx, generation.CreateJobInput{
			IdempotencyKey: "settle-" + randomPublishHex(t, 8),
			Request:        requestDocument, RequestSHA256: requestDigest, DescriptionTokens: 8,
			RequestedTestCaseCount: 2, GeneratorConfigSHA256: strings.Repeat("a", 64), GenerateDataset: true,
			ValidationDBIDs: []string{seededValidationDBID},
		})
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	pipelineStatus := func(t *testing.T, id string) string {
		t.Helper()
		var status string
		if err := database.QueryRowContext(ctx, "SELECT status FROM generation_pipeline WHERE id = ?", id).Scan(&status); err != nil {
			t.Fatal(err)
		}
		return status
	}

	failing := newPipeline(t)
	setReviewJobStatus(t, ctx, database, failing.ID, "GENERATING_SCENARIO")
	lease := seedPublishWork(t, ctx, database, failing.ID, "GENERATING_SCENARIO:repair-0")
	lease.AttemptCount, lease.MaxAttempts = 3, 3
	terminal, err := store.FailGeneration(ctx, failing.ID, lease,
		generation.JobFailure{Code: "SCENARIO_SCHEMA_INVALID", Message: "Scenario output does not match its contract.", Retryable: true}, 0)
	if err != nil || !terminal {
		t.Fatalf("terminal=%v err=%v", terminal, err)
	}
	if status := pipelineStatus(t, *failing.PipelineID); status != "FAILED" {
		t.Fatalf("pipeline status after terminal failure = %q, want FAILED", status)
	}

	cancelling := newPipeline(t)
	if _, err := store.CancelGenerationJob(ctx, cancelling.ID, "cancel-key-001"); err != nil {
		t.Fatal(err)
	}
	if status := pipelineStatus(t, *cancelling.PipelineID); status != "CANCELLED" {
		t.Fatalf("pipeline status after cancel = %q, want CANCELLED", status)
	}
}

func openMigratedReviewDatabase(t *testing.T, ctx context.Context) *sql.DB {
	t.Helper()
	dsn := os.Getenv("ATP_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set ATP_TEST_MYSQL_DSN to run the MySQL 8.4 review integration test")
	}
	adminConfig, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse ATP_TEST_MYSQL_DSN: %v", err)
	}
	adminConfig.DBName = ""
	adminDB := openPublishMySQL(t, adminConfig)
	t.Cleanup(func() { _ = adminDB.Close() })
	if err := adminDB.PingContext(ctx); err != nil {
		t.Fatalf("ping MySQL: %v", err)
	}

	databaseName := "atp_review_" + randomPublishHex(t, 8)
	quoted := "`" + databaseName + "`"
	if _, err := adminDB.ExecContext(ctx, "CREATE DATABASE "+quoted+" CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatalf("create test database: %v", err)
	}

	databaseConfig := *adminConfig
	databaseConfig.DBName = databaseName
	databaseConfig.ParseTime = true
	databaseConfig.Loc = time.UTC
	database := openPublishMySQL(t, &databaseConfig)
	t.Cleanup(func() { _ = database.Close() })
	if _, err := database.ExecContext(ctx, "SET time_zone = '+00:00'"); err != nil {
		t.Fatalf("set UTC session timezone: %v", err)
	}
	if err := migrations.Apply(ctx, database); err != nil {
		t.Fatal(err)
	}
	seedValidationDB(t, ctx, database, seededValidationDBID, "target-a", "target-a.internal")
	return database
}

func setReviewJobStatus(t *testing.T, ctx context.Context, database *sql.DB, jobID, status string) {
	t.Helper()
	if _, err := database.ExecContext(ctx, "UPDATE generation_job SET status = ?, current_stage = ? WHERE id = ?", status, status, jobID); err != nil {
		t.Fatalf("set job status %s: %v", status, err)
	}
}

// assertReviewJobColumns checks a Job's status and review_status; an empty wantReviewStatus
// means review_status must still be NULL.
func assertReviewJobColumns(t *testing.T, ctx context.Context, database *sql.DB, jobID, wantStatus, wantReviewStatus string) {
	t.Helper()
	var status string
	var reviewStatus sql.NullString
	if err := database.QueryRowContext(ctx, "SELECT status, review_status FROM generation_job WHERE id = ?", jobID).Scan(&status, &reviewStatus); err != nil {
		t.Fatalf("load job %s: %v", jobID, err)
	}
	if status != wantStatus || reviewStatus.String != wantReviewStatus || reviewStatus.Valid != (wantReviewStatus != "") {
		t.Fatalf("job %s: status=%q review_status=%v, want status=%q review_status=%q", jobID, status, reviewStatus, wantStatus, wantReviewStatus)
	}
}

// countReviewWork counts a Job's work items whose stage_key matches either LIKE pattern.
func countReviewWork(t *testing.T, ctx context.Context, database *sql.DB, jobID, firstPattern, secondPattern string) int {
	t.Helper()
	var count int
	if err := database.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM work_item WHERE aggregate_id = ? AND (stage_key LIKE ? OR stage_key LIKE ?)",
		jobID, firstPattern, secondPattern,
	).Scan(&count); err != nil {
		t.Fatalf("count work items: %v", err)
	}
	return count
}

// leaseReadyReviewWork leases the READY work item a Job was re-queued with, as a Worker would
// claim it, so PublishGeneration can run under that item's lease.
func leaseReadyReviewWork(t *testing.T, ctx context.Context, database *sql.DB, jobID, stageKey string) generation.WorkLease {
	t.Helper()
	var id string
	if err := database.QueryRowContext(ctx,
		"SELECT id FROM work_item WHERE aggregate_id = ? AND stage_key = ? AND status = 'READY'", jobID, stageKey,
	).Scan(&id); err != nil {
		t.Fatalf("find READY work item %s: %v", stageKey, err)
	}
	owner := "review-test-worker"
	if _, err := database.ExecContext(ctx, `UPDATE work_item SET status = 'LEASED', lease_owner = ?,
		lease_until = TIMESTAMPADD(MINUTE, 5, UTC_TIMESTAMP(6)), fencing_token = 1, attempt_count = 1 WHERE id = ?`, owner, id); err != nil {
		t.Fatalf("lease work item: %v", err)
	}
	return generation.WorkLease{WorkItemID: id, Owner: owner, FencingToken: 1, AttemptCount: 1, MaxAttempts: 3}
}
