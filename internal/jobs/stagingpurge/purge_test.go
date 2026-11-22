package stagingpurge

import (
	"context"
	"errors"
	"testing"

	"agent-test-platform/internal/generation"
)

type purgeStep struct {
	result generation.DraftPurge
	err    error
}

// fakePurgeStore answers each call with the next scripted step and records the batch size it was
// asked for.
type fakePurgeStore struct {
	steps  []purgeStep
	limits []int
}

func (store *fakePurgeStore) PurgeExpiredDrafts(_ context.Context, limit int) (generation.DraftPurge, error) {
	store.limits = append(store.limits, limit)
	if len(store.steps) == 0 {
		return generation.DraftPurge{}, nil
	}
	step := store.steps[0]
	store.steps = store.steps[1:]
	return step.result, step.err
}

func TestPurgeOnceDeletesInBatchesUntilABatchIsShort(t *testing.T) {
	t.Parallel()

	store := &fakePurgeStore{steps: []purgeStep{
		{result: generation.DraftPurge{ArtifactDrafts: 500, TestCaseDrafts: 500}},
		{result: generation.DraftPurge{ArtifactDrafts: 500, TestCaseDrafts: 120}},
		{result: generation.DraftPurge{ArtifactDrafts: 40}},
	}}
	purger, err := New(store, DefaultBatchSize)
	if err != nil {
		t.Fatal(err)
	}
	total, err := purger.PurgeOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if total.ArtifactDrafts != 1040 || total.TestCaseDrafts != 620 {
		t.Fatalf("total = %#v", total)
	}
	if len(store.limits) != 3 {
		t.Fatalf("batches = %d, want 3", len(store.limits))
	}
	for _, limit := range store.limits {
		if limit != DefaultBatchSize {
			t.Fatalf("batch size = %d, want %d", limit, DefaultBatchSize)
		}
	}
}

// Rows deleted before a failing batch are reported with the error, so the operator sees how far the
// purge got.
func TestPurgeOnceReportsRowsDeletedBeforeAnError(t *testing.T) {
	t.Parallel()

	store := &fakePurgeStore{steps: []purgeStep{
		{result: generation.DraftPurge{ArtifactDrafts: 500}},
		{err: errors.New("lock wait timeout")},
	}}
	purger, err := New(store, DefaultBatchSize)
	if err != nil {
		t.Fatal(err)
	}
	total, err := purger.PurgeOnce(context.Background())
	if err == nil || total.ArtifactDrafts != 500 {
		t.Fatalf("total=%#v error=%v, want 500 rows reported with the error", total, err)
	}
}

func TestNewRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()

	if _, err := New(nil, DefaultBatchSize); err == nil {
		t.Fatal("nil store was accepted")
	}
	for _, batchSize := range []int{0, maxBatchSize + 1} {
		if _, err := New(&fakePurgeStore{}, batchSize); err == nil {
			t.Fatalf("batch size %d was accepted", batchSize)
		}
	}
}
