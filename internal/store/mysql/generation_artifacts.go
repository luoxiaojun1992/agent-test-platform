package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"agent-test-platform/internal/generation"

	"github.com/oklog/ulid/v2"
)

const (
	selectGenerationRequestSnapshotSQL = `SELECT request.request_json, request.request_sha256, job.requested_test_case_count
FROM generation_job AS job
JOIN generation_request AS request ON request.id = job.request_id
WHERE job.id = ?`
	upsertGenerationArtifactSQL = `INSERT INTO generation_artifact_draft (
id, generation_job_id, artifact_kind, artifact_key, content_json, content_text,
content_sha256, validation_json, expires_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, TIMESTAMPADD(MICROSECOND, ?, UTC_TIMESTAMP(6)))
ON DUPLICATE KEY UPDATE id = id`
	selectGenerationArtifactDigestSQL = `SELECT content_sha256
FROM generation_artifact_draft
WHERE generation_job_id = ? AND artifact_kind = ? AND artifact_key = ?
FOR UPDATE`
	selectGenerationArtifactSQL = `SELECT content_json, content_text, content_sha256, validation_json
FROM generation_artifact_draft
WHERE generation_job_id = ? AND artifact_kind = ? AND artifact_key = ?`
)

func (store *GenerationStore) LoadGenerationRequest(ctx context.Context, jobID string) (generation.RequestSnapshot, error) {
	if _, err := ulid.ParseStrict(jobID); err != nil {
		return generation.RequestSnapshot{}, generation.ErrNotFound
	}
	var snapshot generation.RequestSnapshot
	var document []byte
	if err := store.database.QueryRowContext(ctx, selectGenerationRequestSnapshotSQL, jobID).Scan(
		&document, &snapshot.SHA256, &snapshot.RequestedTestCaseCount,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return generation.RequestSnapshot{}, generation.ErrNotFound
		}
		return generation.RequestSnapshot{}, fmt.Errorf("load generation request: %w", err)
	}
	if !json.Valid(document) || !validSHA256(snapshot.SHA256) || snapshot.RequestedTestCaseCount < 0 || snapshot.RequestedTestCaseCount > 20 {
		return generation.RequestSnapshot{}, fmt.Errorf("stored generation request is invalid")
	}
	snapshot.Document = append([]byte(nil), document...)
	return snapshot, nil
}

func (store *GenerationStore) LoadGenerationArtifact(ctx context.Context, jobID, kind, key string) (generation.ArtifactSnapshot, error) {
	if _, err := ulid.ParseStrict(jobID); err != nil {
		return generation.ArtifactSnapshot{}, generation.ErrNotFound
	}
	if kind == "" || key == "" {
		return generation.ArtifactSnapshot{}, fmt.Errorf("artifact kind and key are required")
	}
	snapshot := generation.ArtifactSnapshot{Kind: kind, Key: key}
	var contentJSON, contentText, validation []byte
	if err := store.database.QueryRowContext(ctx, selectGenerationArtifactSQL, jobID, kind, key).Scan(
		&contentJSON, &contentText, &snapshot.SHA256, &validation,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return generation.ArtifactSnapshot{}, generation.ErrNotFound
		}
		return generation.ArtifactSnapshot{}, fmt.Errorf("load generation artifact: %w", err)
	}
	hasJSON := len(contentJSON) > 0
	hasText := len(contentText) > 0
	if hasJSON == hasText || !validSHA256(snapshot.SHA256) || (hasJSON && !json.Valid(contentJSON)) || (len(validation) > 0 && !json.Valid(validation)) {
		return generation.ArtifactSnapshot{}, fmt.Errorf("stored generation artifact is invalid")
	}
	if hasJSON {
		snapshot.ContentJSON = append([]byte(nil), contentJSON...)
	} else {
		value := string(contentText)
		snapshot.ContentText = &value
	}
	snapshot.Validation = append([]byte(nil), validation...)
	return snapshot, nil
}

func (store *GenerationStore) SaveGenerationArtifact(ctx context.Context, jobID string, lease generation.WorkLease, draft generation.ArtifactDraft) error {
	if _, err := ulid.ParseStrict(jobID); err != nil {
		return fmt.Errorf("invalid generation job ID: %w", err)
	}
	if lease.WorkItemID == "" || lease.Owner == "" || lease.FencingToken == 0 {
		return fmt.Errorf("complete generation lease is required")
	}
	if err := draft.Validate(); err != nil {
		return err
	}
	artifactID, err := store.newID()
	if err != nil {
		return fmt.Errorf("generate artifact ID: %w", err)
	}
	if _, err := ulid.ParseStrict(artifactID); err != nil {
		return fmt.Errorf("generate artifact ID: %w", err)
	}

	transaction, err := store.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin generation artifact save: %w", err)
	}
	defer transaction.Rollback()
	state, _, _, err := lockGenerationState(ctx, transaction, jobID)
	if err != nil {
		return err
	}
	if state.Terminal() {
		return fmt.Errorf("%w: job status %s is terminal", generation.ErrConflict, state.Status)
	}
	if err := lockGenerationLease(ctx, transaction, jobID, lease); err != nil {
		return err
	}

	var contentJSON any
	if len(draft.ContentJSON) > 0 {
		contentJSON = []byte(draft.ContentJSON)
	}
	var contentText any
	if draft.ContentText != nil {
		contentText = *draft.ContentText
	}
	var validation any
	if len(draft.Validation) > 0 {
		validation = []byte(draft.Validation)
	}
	if _, err := transaction.ExecContext(ctx, upsertGenerationArtifactSQL,
		artifactID, jobID, draft.Kind, draft.Key, contentJSON, contentText,
		draft.SHA256, validation, draft.TTL.Microseconds(),
	); err != nil {
		return fmt.Errorf("upsert generation artifact: %w", err)
	}
	var storedDigest string
	if err := transaction.QueryRowContext(ctx, selectGenerationArtifactDigestSQL, jobID, draft.Kind, draft.Key).Scan(&storedDigest); err != nil {
		return fmt.Errorf("load generation artifact digest: %w", err)
	}
	if storedDigest != draft.SHA256 {
		return fmt.Errorf("%w: generation artifact %s/%s has a different digest", generation.ErrConflict, draft.Kind, draft.Key)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit generation artifact save: %w", err)
	}
	return nil
}
