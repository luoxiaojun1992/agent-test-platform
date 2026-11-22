package mysqlstore

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-test-platform/internal/generation"

	"github.com/oklog/ulid/v2"
)

// withinSeconds reports whether got is within tolerance seconds of want.
func withinSeconds(got, want time.Duration, tolerance float64) bool {
	difference := got - want
	if difference < 0 {
		difference = -difference
	}
	return difference.Seconds() <= tolerance
}

func countBudgetRows(t *testing.T, ctx context.Context, store *GenerationStore, query string, args ...any) int {
	t.Helper()
	var count int
	if err := store.database.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return count
}

// The budget is wall-clock time from the first advance past acceptance. It keeps running while the
// Job waits in AWAITING_REVIEW, approval does not extend it, and an expired budget stays expired.
// Finished Jobs' expired drafts are purged, while drafts of Jobs still awaiting review or publishing
// are kept.
func TestGenerationJobBudgetCountsReviewTimeAndPurgeKeepsLiveDrafts(t *testing.T) {
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
		IdempotencyKey: "budget-review-" + randomPublishHex(t, 8),
		Request:        requestDocument, RequestSHA256: requestDigest, DescriptionTokens: 8,
		RequestedTestCaseCount: 2, GeneratorConfigSHA256: strings.Repeat("a", 64), GenerateDataset: true,
		RequireManualReview: true,
		ValidationDBIDs:     []string{seededValidationDBID},
	})
	if err != nil {
		t.Fatalf("create generation pipeline: %v", err)
	}
	scenarioJobID := created.ID
	if _, bounded, err := store.LoadGenerationBudget(ctx, scenarioJobID); err != nil || bounded {
		t.Fatalf("budget before the first advance: bounded=%v error=%v, want unbounded", bounded, err)
	}

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
	budget, bounded, err := store.LoadGenerationBudget(ctx, scenarioJobID)
	if err != nil || !bounded || !withinSeconds(budget, generation.GenerationJobBudget, 5) {
		t.Fatalf("budget after the first advance = %s bounded=%v error=%v, want about 10 minutes", budget, bounded, err)
	}

	// The Job has waited six minutes for review, so four minutes of budget are left. Waiting is not
	// paused: the budget follows the wall clock.
	if _, err := database.ExecContext(ctx,
		"UPDATE generation_job SET deadline_at = TIMESTAMPADD(MINUTE, 4, UTC_TIMESTAMP(6)) WHERE id = ?", scenarioJobID); err != nil {
		t.Fatal(err)
	}
	waiting, _, err := store.LoadGenerationBudget(ctx, scenarioJobID)
	if err != nil || !withinSeconds(waiting, 4*time.Minute, 3) {
		t.Fatalf("budget while awaiting review = %s error=%v, want about 4 minutes", waiting, err)
	}

	// Approval does not extend the budget.
	if _, err := store.ReviewGenerationJob(ctx, scenarioJobID, "budget-approve-"+randomPublishHex(t, 4),
		generation.ReviewInput{Decision: generation.ReviewApprove}); err != nil {
		t.Fatalf("approve scenario: %v", err)
	}
	approved, bounded, err := store.LoadGenerationBudget(ctx, scenarioJobID)
	if err != nil || !bounded || !withinSeconds(approved, 4*time.Minute, 3) {
		t.Fatalf("budget after approval = %s bounded=%v error=%v, want still about 4 minutes", approved, bounded, err)
	}

	// Approval after the deadline leaves no budget. The Worker fails the Job when it claims the
	// publish work, instead of publishing.
	if _, err := database.ExecContext(ctx,
		"UPDATE generation_job SET deadline_at = TIMESTAMPADD(MINUTE, -1, UTC_TIMESTAMP(6)) WHERE id = ?", scenarioJobID); err != nil {
		t.Fatal(err)
	}
	spent, bounded, err := store.LoadGenerationBudget(ctx, scenarioJobID)
	if err != nil || !bounded || spent > 0 {
		t.Fatalf("budget past the deadline = %s bounded=%v error=%v, want none left", spent, bounded, err)
	}

	// The scenario Job is PUBLISHING, which is not finished, so its expired drafts stay.
	if _, err := database.ExecContext(ctx, `UPDATE generation_artifact_draft
SET expires_at = TIMESTAMPADD(HOUR, -1, UTC_TIMESTAMP(6)) WHERE generation_job_id = ?`, scenarioJobID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO generation_test_case_draft (
id, generation_job_id, client_case_id, schema_version, content_json, content_sha256, accepted, validation_json, expires_at
) VALUES (?, ?, 'live-case', '1.0', '{"case":"live"}', ?, FALSE, '{"valid":true}', TIMESTAMPADD(HOUR, -1, UTC_TIMESTAMP(6)))`,
		ulid.Make().String(), scenarioJobID, strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}

	finished, err := store.CreateGenerationJob(ctx, generation.CreateJobInput{
		IdempotencyKey: "budget-finished-" + randomPublishHex(t, 8),
		Request:        requestDocument, RequestSHA256: requestDigest, DescriptionTokens: 8,
		RequestedTestCaseCount: 2, GeneratorConfigSHA256: strings.Repeat("a", 64), GenerateDataset: true,
		ValidationDBIDs: []string{seededValidationDBID},
	})
	if err != nil {
		t.Fatalf("create finished job: %v", err)
	}
	setReviewJobStatus(t, ctx, database, finished.ID, "COMPLETED")
	seedPublishDraft(t, ctx, database, finished.ID, "SCENARIO", "expired", []byte(`{"business_rules":[]}`))
	seedPublishDraft(t, ctx, database, finished.ID, "SCENARIO", "fresh", []byte(`{"business_rules":[]}`))
	if _, err := database.ExecContext(ctx, `UPDATE generation_artifact_draft
SET expires_at = TIMESTAMPADD(HOUR, -1, UTC_TIMESTAMP(6)) WHERE generation_job_id = ? AND artifact_key = 'expired'`, finished.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO generation_test_case_draft (
id, generation_job_id, client_case_id, schema_version, content_json, content_sha256, accepted, validation_json, expires_at
) VALUES (?, ?, 'finished-case', '1.0', '{"case":"finished"}', ?, FALSE, '{"valid":true}', TIMESTAMPADD(HOUR, -1, UTC_TIMESTAMP(6)))`,
		ulid.Make().String(), finished.ID, strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}

	if _, err := store.PurgeExpiredDrafts(ctx, 0); err == nil {
		t.Fatal("a non-positive purge batch size was accepted")
	}
	purged, err := store.PurgeExpiredDrafts(ctx, 500)
	if err != nil {
		t.Fatalf("purge expired drafts: %v", err)
	}
	if purged.ArtifactDrafts != 1 || purged.TestCaseDrafts != 1 {
		t.Fatalf("purged = %#v, want only the finished Job's expired drafts", purged)
	}
	if count := countBudgetRows(t, ctx, store, "SELECT COUNT(*) FROM generation_artifact_draft WHERE generation_job_id = ?", scenarioJobID); count != 1 {
		t.Fatalf("PUBLISHING Job artifact drafts = %d, want 1 kept", count)
	}
	if count := countBudgetRows(t, ctx, store, "SELECT COUNT(*) FROM generation_test_case_draft WHERE generation_job_id = ?", scenarioJobID); count != 1 {
		t.Fatalf("PUBLISHING Job test case drafts = %d, want 1 kept", count)
	}
	if count := countBudgetRows(t, ctx, store, "SELECT COUNT(*) FROM generation_artifact_draft WHERE generation_job_id = ? AND artifact_key = 'fresh'", finished.ID); count != 1 {
		t.Fatalf("unexpired draft of the finished Job = %d, want 1 kept", count)
	}
}
