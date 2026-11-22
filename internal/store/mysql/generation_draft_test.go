package mysqlstore

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"agent-test-platform/internal/generation"

	"github.com/DATA-DOG/go-sqlmock"
)

func draftJobRows(kind, status string, repairRound int) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"job_kind", "status", "repair_round", "input_scenario_revision_id", "input_dataset_revision_id",
	}).AddRow(kind, status, repairRound, mockScenarioRevisionID, mockDatasetRevisionID)
}

// draftArtifact returns the artifact row selectGenerationArtifactForPublishSQL yields for a
// stored draft, plus the canonical JSON a reader should surface.
func draftArtifact(t *testing.T, document string) (*sqlmock.Rows, string) {
	t.Helper()
	canonical, digest, err := generation.CanonicalJSON([]byte(document))
	if err != nil {
		t.Fatal(err)
	}
	return sqlmock.NewRows([]string{"content_json", "content_sha256"}).AddRow([]byte(canonical), digest), string(canonical)
}

func expectDraftArtifact(t *testing.T, mock sqlmock.Sqlmock, kind, key, document string) string {
	t.Helper()
	rows, canonical := draftArtifact(t, document)
	mock.ExpectQuery(regexp.QuoteMeta(selectGenerationArtifactForPublishSQL)).WithArgs(generationJobID, kind, key).WillReturnRows(rows)
	return canonical
}

func TestGetGenerationJobDraftScenarioUsesAcceptedRound(t *testing.T) {
	t.Parallel()

	store, mock := newReviewMock(t)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(selectGenerationJobForDraftSQL)).WithArgs(generationJobID).WillReturnRows(draftJobRows("SCENARIO", "AWAITING_REVIEW", 1))
	want := expectDraftArtifact(t, mock, "SCENARIO", "scenario-round-1", `{"domain":"e-commerce","business_rules":[]}`)
	mock.ExpectRollback()

	view, err := store.GetGenerationJobDraft(context.Background(), generationJobID)
	if err != nil {
		t.Fatal(err)
	}
	if view.JobID != generationJobID || view.Kind != generation.KindScenario || string(view.Scenario) != want {
		t.Fatalf("view = %#v", view)
	}
	if view.Dataset != nil || view.TestCases != nil {
		t.Fatalf("unrelated draft sections are set: %#v", view)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetGenerationJobDraftDataset(t *testing.T) {
	t.Parallel()

	store, mock := newReviewMock(t)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(selectGenerationJobForDraftSQL)).WithArgs(generationJobID).WillReturnRows(draftJobRows("DATASET", "AWAITING_REVIEW", 0))
	want := expectDraftArtifact(t, mock, "DATASET", "dataset", `{"schema_sql":"CREATE TABLE orders (id VARCHAR(32) PRIMARY KEY)","seed_sql":"INSERT INTO orders (id) VALUES ('O1')"}`)
	mock.ExpectRollback()

	view, err := store.GetGenerationJobDraft(context.Background(), generationJobID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Kind != generation.KindDataset || string(view.Dataset) != want {
		t.Fatalf("view = %#v", view)
	}
	if view.InputScenarioRevisionID == nil || *view.InputScenarioRevisionID != mockScenarioRevisionID {
		t.Fatalf("input scenario revision = %v", view.InputScenarioRevisionID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetGenerationJobDraftTestCasesFollowsAcceptedManifest(t *testing.T) {
	t.Parallel()

	store, mock := newReviewMock(t)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(selectGenerationJobForDraftSQL)).WithArgs(generationJobID).WillReturnRows(draftJobRows("TEST_CASES", "AWAITING_REVIEW", 0))
	expectDraftArtifact(t, mock, "TEST_CASE_MANIFEST", "round-0", `{"accepted":["case-b","case-a"],"rejected":[{"client_case_id":"case-c","errors":[]}]}`)
	second := expectDraftArtifact(t, mock, "TEST_CASE", "case-b", `{"client_case_id":"case-b"}`)
	first := expectDraftArtifact(t, mock, "TEST_CASE", "case-a", `{"client_case_id":"case-a"}`)
	mock.ExpectRollback()

	view, err := store.GetGenerationJobDraft(context.Background(), generationJobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.TestCases) != 2 || string(view.TestCases[0]) != second || string(view.TestCases[1]) != first {
		t.Fatalf("test cases = %q, want acceptance order [%s %s] without the rejected case", view.TestCases, second, first)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetGenerationJobDraftOnlyWhileAwaitingReview(t *testing.T) {
	t.Parallel()

	for _, status := range []string{"GENERATING_SCENARIO", "VALIDATING_SCENARIO", "PUBLISHING", "COMPLETED", "FAILED"} {
		status := status
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			store, mock := newReviewMock(t)
			mock.ExpectBegin()
			mock.ExpectQuery(regexp.QuoteMeta(selectGenerationJobForDraftSQL)).WithArgs(generationJobID).WillReturnRows(draftJobRows("SCENARIO", status, 0))
			mock.ExpectRollback()

			if _, err := store.GetGenerationJobDraft(context.Background(), generationJobID); !errors.Is(err, generation.ErrConflict) {
				t.Fatalf("error = %v, want ErrConflict", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGetGenerationJobDraftNotFoundAndIntegrity(t *testing.T) {
	t.Parallel()

	store, mock := newReviewMock(t)
	if _, err := store.GetGenerationJobDraft(context.Background(), "not-a-ulid"); !errors.Is(err, generation.ErrNotFound) {
		t.Fatalf("malformed ID error = %v", err)
	}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(selectGenerationJobForDraftSQL)).WithArgs(generationJobID).WillReturnRows(
		sqlmock.NewRows([]string{"job_kind", "status", "repair_round", "input_scenario_revision_id", "input_dataset_revision_id"}),
	)
	mock.ExpectRollback()
	if _, err := store.GetGenerationJobDraft(context.Background(), generationJobID); !errors.Is(err, generation.ErrNotFound) {
		t.Fatalf("missing job error = %v", err)
	}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(selectGenerationJobForDraftSQL)).WithArgs(generationJobID).WillReturnRows(draftJobRows("SCENARIO", "AWAITING_REVIEW", 0))
	mock.ExpectQuery(regexp.QuoteMeta(selectGenerationArtifactForPublishSQL)).WithArgs(generationJobID, "SCENARIO", "scenario").WillReturnRows(
		sqlmock.NewRows([]string{"content_json", "content_sha256"}),
	)
	mock.ExpectRollback()
	if _, err := store.GetGenerationJobDraft(context.Background(), generationJobID); !errors.Is(err, generation.ErrNotFound) {
		t.Fatalf("missing draft error = %v", err)
	}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(selectGenerationJobForDraftSQL)).WithArgs(generationJobID).WillReturnRows(draftJobRows("SCENARIO", "AWAITING_REVIEW", 0))
	mock.ExpectQuery(regexp.QuoteMeta(selectGenerationArtifactForPublishSQL)).WithArgs(generationJobID, "SCENARIO", "scenario").WillReturnRows(
		sqlmock.NewRows([]string{"content_json", "content_sha256"}).AddRow([]byte(`{"domain":"tampered"}`), strings.Repeat("0", 64)),
	)
	mock.ExpectRollback()
	_, err := store.GetGenerationJobDraft(context.Background(), generationJobID)
	if err == nil || errors.Is(err, generation.ErrNotFound) || errors.Is(err, generation.ErrConflict) {
		t.Fatalf("tampered draft error = %v, want an integrity failure", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
