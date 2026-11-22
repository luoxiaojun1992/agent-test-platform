package mysqlstore

import (
	"context"
	"fmt"

	"agent-test-platform/internal/generation"
)

// Only drafts of Jobs that reached a terminal status are purged. A Job in AWAITING_REVIEW still
// needs its draft for the review decision, and an in-progress Job needs it to continue.
const (
	purgeExpiredArtifactDraftsSQL = `DELETE FROM generation_artifact_draft
WHERE expires_at < UTC_TIMESTAMP(6) AND generation_job_id IN (
    SELECT id FROM generation_job WHERE status IN ('COMPLETED', 'FAILED', 'CANCELLED'))
LIMIT ?`
	purgeExpiredTestCaseDraftsSQL = `DELETE FROM generation_test_case_draft
WHERE expires_at < UTC_TIMESTAMP(6) AND generation_job_id IN (
    SELECT id FROM generation_job WHERE status IN ('COMPLETED', 'FAILED', 'CANCELLED'))
LIMIT ?`
)

// PurgeExpiredDrafts hard-deletes at most limit expired staging rows of each kind.
func (store *GenerationStore) PurgeExpiredDrafts(ctx context.Context, limit int) (generation.DraftPurge, error) {
	if limit < 1 {
		return generation.DraftPurge{}, fmt.Errorf("purge batch size must be positive")
	}
	artifacts, err := store.database.ExecContext(ctx, purgeExpiredArtifactDraftsSQL, limit)
	if err != nil {
		return generation.DraftPurge{}, fmt.Errorf("purge expired artifact drafts: %w", err)
	}
	artifactRows, err := artifacts.RowsAffected()
	if err != nil {
		return generation.DraftPurge{}, fmt.Errorf("count purged artifact drafts: %w", err)
	}
	testCases, err := store.database.ExecContext(ctx, purgeExpiredTestCaseDraftsSQL, limit)
	if err != nil {
		return generation.DraftPurge{}, fmt.Errorf("purge expired test case drafts: %w", err)
	}
	testCaseRows, err := testCases.RowsAffected()
	if err != nil {
		return generation.DraftPurge{}, fmt.Errorf("count purged test case drafts: %w", err)
	}
	return generation.DraftPurge{ArtifactDrafts: artifactRows, TestCaseDrafts: testCaseRows}, nil
}
