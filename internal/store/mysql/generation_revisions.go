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
	selectPublishedScenarioRevisionSQL = `SELECT id, scenario_id, content_json, content_sha256, lifecycle_status
FROM scenario_revision WHERE id = ?`
	selectPublishedDatasetRevisionSQL = `SELECT id, dataset_id, scenario_revision_id, schema_sql, seed_sql,
constraints_json, metadata_json, content_sha256, execution_database, lifecycle_status
FROM dataset_revision WHERE id = ?`
)

// LoadPublishedScenarioRevision reads a published Scenario Revision for a DATASET or TEST_CASES
// kind Job's input_scenario_revision_id, including its lifecycle_status. Whether that status is
// usable as an input is decided by the caller's VALIDATING_INPUT stage, not here
// (docs/technical/01-storage-and-workflows.md section 3).
func (store *GenerationStore) LoadPublishedScenarioRevision(ctx context.Context, revisionID string) (generation.ScenarioRevisionSnapshot, error) {
	if _, err := ulid.ParseStrict(revisionID); err != nil {
		return generation.ScenarioRevisionSnapshot{}, generation.ErrNotFound
	}
	var snapshot generation.ScenarioRevisionSnapshot
	var content []byte
	if err := store.database.QueryRowContext(ctx, selectPublishedScenarioRevisionSQL, revisionID).Scan(
		&snapshot.ID, &snapshot.ScenarioID, &content, &snapshot.SHA256, &snapshot.LifecycleStatus,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return generation.ScenarioRevisionSnapshot{}, generation.ErrNotFound
		}
		return generation.ScenarioRevisionSnapshot{}, fmt.Errorf("load published Scenario Revision: %w", err)
	}
	if !json.Valid(content) || !validSHA256(snapshot.SHA256) || !generation.RevisionLifecycleKnown(snapshot.LifecycleStatus) {
		return generation.ScenarioRevisionSnapshot{}, fmt.Errorf("stored Scenario Revision is invalid")
	}
	snapshot.ContentJSON = append([]byte(nil), content...)
	return snapshot, nil
}

// LoadPublishedDatasetRevision reads a published Dataset Revision for a TEST_CASES kind Job's
// input_dataset_revision_id (or a DATASET-kind reference), including its lifecycle_status.
func (store *GenerationStore) LoadPublishedDatasetRevision(ctx context.Context, revisionID string) (generation.DatasetRevisionSnapshot, error) {
	if _, err := ulid.ParseStrict(revisionID); err != nil {
		return generation.DatasetRevisionSnapshot{}, generation.ErrNotFound
	}
	var snapshot generation.DatasetRevisionSnapshot
	var constraints, metadata []byte
	if err := store.database.QueryRowContext(ctx, selectPublishedDatasetRevisionSQL, revisionID).Scan(
		&snapshot.ID, &snapshot.DatasetID, &snapshot.ScenarioRevisionID, &snapshot.SchemaSQL, &snapshot.SeedSQL,
		&constraints, &metadata, &snapshot.SHA256, &snapshot.ExecutionDatabase, &snapshot.LifecycleStatus,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return generation.DatasetRevisionSnapshot{}, generation.ErrNotFound
		}
		return generation.DatasetRevisionSnapshot{}, fmt.Errorf("load published Dataset Revision: %w", err)
	}
	if !json.Valid(constraints) || !json.Valid(metadata) || !validSHA256(snapshot.SHA256) || !generation.RevisionLifecycleKnown(snapshot.LifecycleStatus) {
		return generation.DatasetRevisionSnapshot{}, fmt.Errorf("stored Dataset Revision is invalid")
	}
	snapshot.ConstraintsJSON = append([]byte(nil), constraints...)
	snapshot.MetadataJSON = append([]byte(nil), metadata...)
	targetIDs, err := store.publishedDatasetTargets(ctx, revisionID)
	if err != nil {
		return generation.DatasetRevisionSnapshot{}, err
	}
	snapshot.ValidationDBIDs = targetIDs
	return snapshot, nil
}
