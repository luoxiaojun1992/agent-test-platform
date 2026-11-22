package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"agent-test-platform/internal/generation"

	"github.com/oklog/ulid/v2"
)

// selectGenerationBudgetSQL measures the remaining budget on the database clock, so Workers on
// different hosts agree on it. The budget keeps running while the Job waits in AWAITING_REVIEW.
const selectGenerationBudgetSQL = `SELECT TIMESTAMPDIFF(MICROSECOND, UTC_TIMESTAMP(6), deadline_at)
FROM generation_job WHERE id = ?`

// LoadGenerationBudget reports the processing time left in a Job's budget. bounded is
// false until the Job first advances past acceptance.
func (store *GenerationStore) LoadGenerationBudget(ctx context.Context, jobID string) (time.Duration, bool, error) {
	if _, err := ulid.ParseStrict(jobID); err != nil {
		return 0, false, generation.ErrNotFound
	}
	var remainingMicros sql.NullInt64
	if err := store.database.QueryRowContext(ctx, selectGenerationBudgetSQL, jobID).Scan(&remainingMicros); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, generation.ErrNotFound
		}
		return 0, false, fmt.Errorf("load generation budget: %w", err)
	}
	if !remainingMicros.Valid {
		return 0, false, nil
	}
	return time.Duration(remainingMicros.Int64) * time.Microsecond, true, nil
}
