// Package stagingpurge hard-deletes expired staging drafts of finished Generation Jobs. It runs only
// when an operator starts it (cmd/purge-drafts); nothing schedules it
// (docs/technical/01-storage-and-workflows.md, section 2).
package stagingpurge

import (
	"context"
	"fmt"

	"agent-test-platform/internal/generation"
)

const (
	DefaultBatchSize = 500
	maxBatchSize     = 5000
)

// Store deletes at most limit expired drafts of each kind.
type Store interface {
	PurgeExpiredDrafts(context.Context, int) (generation.DraftPurge, error)
}

// Purger deletes expired drafts in batches of batchSize rows per kind.
type Purger struct {
	store     Store
	batchSize int
}

func New(store Store, batchSize int) (*Purger, error) {
	if store == nil {
		return nil, fmt.Errorf("staging purge store is required")
	}
	if batchSize < 1 || batchSize > maxBatchSize {
		return nil, fmt.Errorf("staging purge batch size must be in [1,%d]", maxBatchSize)
	}
	return &Purger{store: store, batchSize: batchSize}, nil
}

// PurgeOnce deletes expired drafts in batches until one batch deletes fewer rows than the batch size
// for both kinds. Each batch is its own statement, so a long purge never holds locks on the rows of
// active Jobs for long. On error it reports the rows deleted before the failure.
func (purger *Purger) PurgeOnce(ctx context.Context) (generation.DraftPurge, error) {
	var total generation.DraftPurge
	size := int64(purger.batchSize)
	for {
		batch, err := purger.store.PurgeExpiredDrafts(ctx, purger.batchSize)
		total.ArtifactDrafts += batch.ArtifactDrafts
		total.TestCaseDrafts += batch.TestCaseDrafts
		if err != nil {
			return total, err
		}
		if batch.ArtifactDrafts < size && batch.TestCaseDrafts < size {
			return total, nil
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}
