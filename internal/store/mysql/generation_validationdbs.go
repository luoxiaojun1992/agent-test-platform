package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"agent-test-platform/internal/generation"
)

const (
	selectRegisteredTargetSQL       = `SELECT id FROM validation_mysql WHERE id = ?`
	selectDatasetRevisionTargetsSQL = `SELECT validation_db_id FROM dataset_revision_validation_db
WHERE dataset_revision_id = ? ORDER BY ordinal`
	insertDatasetRevisionTargetSQL = `INSERT INTO dataset_revision_validation_db (dataset_revision_id, validation_db_id, ordinal)
VALUES (?, ?, ?) ON DUPLICATE KEY UPDATE ordinal = ordinal`
)

// requireRegisteredTargets reports generation.ErrTargetNotFound when a validation MySQL ID that a request names is
// not registered. The check runs when the request is accepted, so a Dataset is never validated against an
// unknown Target.
func requireRegisteredTargets(ctx context.Context, querier sqlQuerier, targetIDs []string) error {
	for _, targetID := range targetIDs {
		var found string
		err := querier.QueryRowContext(ctx, selectRegisteredTargetSQL, targetID).Scan(&found)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", generation.ErrTargetNotFound, targetID)
		}
		if err != nil {
			return fmt.Errorf("check validation MySQL %s: %w", targetID, err)
		}
	}
	return nil
}

// publishedDatasetTargets returns the validation MySQL IDs a Dataset Revision is bound to, in binding order.
func (store *GenerationStore) publishedDatasetTargets(ctx context.Context, revisionID string) ([]string, error) {
	rows, err := store.database.QueryContext(ctx, selectDatasetRevisionTargetsSQL, revisionID)
	if err != nil {
		return nil, fmt.Errorf("query Dataset Revision Targets: %w", err)
	}
	defer rows.Close()
	var targetIDs []string
	for rows.Next() {
		var targetID string
		if err := rows.Scan(&targetID); err != nil {
			return nil, fmt.Errorf("scan Dataset Revision Target: %w", err)
		}
		targetIDs = append(targetIDs, targetID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read Dataset Revision Targets: %w", err)
	}
	return targetIDs, nil
}
