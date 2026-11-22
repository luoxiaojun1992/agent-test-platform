package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"agent-test-platform/internal/generation"

	"github.com/oklog/ulid/v2"
)

const selectGenerationJobForDraftSQL = `SELECT job_kind, status, repair_round, input_scenario_revision_id, input_dataset_revision_id
FROM generation_job WHERE id = ?`

// GetGenerationJobDraft returns the draft content of a Job paused in AWAITING_REVIEW so a
// reviewer can see what an approval would publish. Drafts are otherwise internal staging data;
// this read-only view exists only for that decision, so any other status is a conflict. Like
// ReviewGenerationJob it addresses exactly the Job ID given, never a later Pipeline stage.
func (store *GenerationStore) GetGenerationJobDraft(ctx context.Context, id string) (generation.DraftView, error) {
	if _, err := ulid.ParseStrict(id); err != nil {
		return generation.DraftView{}, generation.ErrNotFound
	}
	// One snapshot, so the status check and the draft rows cannot disagree.
	transaction, err := store.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return generation.DraftView{}, fmt.Errorf("begin generation draft read: %w", err)
	}
	defer transaction.Rollback()

	var kind, status string
	var repairRound int
	var inputScenarioRevisionID, inputDatasetRevisionID sql.NullString
	if err := transaction.QueryRowContext(ctx, selectGenerationJobForDraftSQL, id).Scan(
		&kind, &status, &repairRound, &inputScenarioRevisionID, &inputDatasetRevisionID,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return generation.DraftView{}, generation.ErrNotFound
		}
		return generation.DraftView{}, fmt.Errorf("load generation job for draft: %w", err)
	}
	if generation.JobStatus(status) != generation.StatusAwaitingReview {
		return generation.DraftView{}, fmt.Errorf("%w: job status %s is not AWAITING_REVIEW", generation.ErrConflict, status)
	}

	view := generation.DraftView{
		JobID: id, Kind: generation.JobKind(kind),
		InputScenarioRevisionID: nullStringPtr(inputScenarioRevisionID), InputDatasetRevisionID: nullStringPtr(inputDatasetRevisionID),
	}
	switch view.Kind {
	case generation.KindScenario:
		view.Scenario, _, err = store.loadPublishArtifact(ctx, transaction, id, generation.ArtifactKindScenario, publishDraftKey("scenario", repairRound))
		if err != nil {
			return generation.DraftView{}, err
		}
	case generation.KindDataset:
		view.Dataset, _, err = store.loadPublishArtifact(ctx, transaction, id, generation.ArtifactKindDataset, publishDraftKey("dataset", repairRound))
		if err != nil {
			return generation.DraftView{}, err
		}
	case generation.KindTestCases:
		manifest, err := store.loadAcceptedManifest(ctx, transaction, id, repairRound)
		if err != nil {
			return generation.DraftView{}, err
		}
		for _, clientCaseID := range manifest {
			content, _, err := store.loadPublishArtifact(ctx, transaction, id, generation.ArtifactKindTestCase, clientCaseID)
			if err != nil {
				return generation.DraftView{}, fmt.Errorf("load accepted candidate %s: %w", clientCaseID, err)
			}
			view.TestCases = append(view.TestCases, content)
		}
	default:
		return generation.DraftView{}, fmt.Errorf("unknown job kind %q", kind)
	}
	return view, nil
}
