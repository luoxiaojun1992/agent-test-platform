package mysqlstore

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"agent-test-platform/internal/generation"
	"agent-test-platform/internal/jobs/mysqlqueue"

	"github.com/DATA-DOG/go-sqlmock"
)

const generationArtifactID = "01K4Z6Y7X8W9V0T1S2R3Q4P5N9"

func TestLoadGenerationRequest(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := newGenerationStore(database, sequenceIDs())
	document := []byte(`{"description":"Generate tests.","test_case_count":3}`)
	digest := strings.Repeat("a", 64)
	mock.ExpectQuery(regexp.QuoteMeta(selectGenerationRequestSnapshotSQL)).WithArgs(generationJobID).WillReturnRows(
		sqlmock.NewRows([]string{"request_json", "request_sha256", "requested_test_case_count"}).AddRow(document, digest, 3),
	)

	snapshot, err := store.LoadGenerationRequest(context.Background(), generationJobID)
	if err != nil {
		t.Fatal(err)
	}
	if string(snapshot.Document) != string(document) || snapshot.SHA256 != digest || snapshot.RequestedTestCaseCount != 3 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestLoadGenerationArtifact(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := newGenerationStore(database, sequenceIDs())
	document := []byte(`{"schema_version":"1.0","description":"Generate tests."}`)
	digest := strings.Repeat("a", 64)
	validation := []byte(`{"valid":true}`)
	mock.ExpectQuery(regexp.QuoteMeta(selectGenerationArtifactSQL)).WithArgs(
		generationJobID, generation.ArtifactKindNormalizedRequest, "request",
	).WillReturnRows(sqlmock.NewRows([]string{"content_json", "content_text", "content_sha256", "validation_json"}).AddRow(document, nil, digest, validation))

	snapshot, err := store.LoadGenerationArtifact(context.Background(), generationJobID, generation.ArtifactKindNormalizedRequest, "request")
	if err != nil {
		t.Fatal(err)
	}
	if string(snapshot.ContentJSON) != string(document) || snapshot.ContentText != nil || snapshot.SHA256 != digest || string(snapshot.Validation) != string(validation) {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestLoadGenerationArtifactReturnsNotFound(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := newGenerationStore(database, sequenceIDs())
	mock.ExpectQuery(regexp.QuoteMeta(selectGenerationArtifactSQL)).WithArgs(generationJobID, "SCENARIO", "scenario").WillReturnRows(
		sqlmock.NewRows([]string{"content_json", "content_text", "content_sha256", "validation_json"}),
	)
	_, err = store.LoadGenerationArtifact(context.Background(), generationJobID, "SCENARIO", "scenario")
	if !errors.Is(err, generation.ErrNotFound) {
		t.Fatalf("error = %v", err)
	}
}

func TestSaveGenerationArtifactIsIdempotent(t *testing.T) {
	t.Parallel()

	for _, replay := range []bool{false, true} {
		replay := replay
		t.Run(map[bool]string{false: "new", true: "replay"}[replay], func(t *testing.T) {
			t.Parallel()
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			store, _ := newGenerationStore(database, sequenceIDs(generationArtifactID))
			lease := artifactLease()
			draft := normalizedRequestDraft()

			mock.ExpectBegin()
			mock.ExpectQuery(regexp.QuoteMeta(lockGenerationJobSQL)).WithArgs(generationJobID).WillReturnRows(
				lockJobRows("TEST_CASES", "VALIDATING_INPUT", 3, 0, 0, false),
			)
			mock.ExpectQuery(regexp.QuoteMeta(lockGenerationLeaseSQL)).WithArgs(generationWorkID, "worker", uint64(2)).WillReturnRows(
				sqlmock.NewRows([]string{"aggregate_type", "aggregate_id"}).AddRow("generation_job", generationJobID),
			)
			insertID := int64(1)
			if replay {
				insertID = 0
			}
			mock.ExpectExec(regexp.QuoteMeta(upsertGenerationArtifactSQL)).WithArgs(
				generationArtifactID, generationJobID, draft.Kind, draft.Key, []byte(draft.ContentJSON), nil,
				draft.SHA256, []byte(draft.Validation), draft.TTL.Microseconds(),
			).WillReturnResult(sqlmock.NewResult(insertID, 1))
			mock.ExpectQuery(regexp.QuoteMeta(selectGenerationArtifactDigestSQL)).WithArgs(generationJobID, draft.Kind, draft.Key).WillReturnRows(
				sqlmock.NewRows([]string{"content_sha256"}).AddRow(draft.SHA256),
			)
			mock.ExpectCommit()

			if err := store.SaveGenerationArtifact(context.Background(), generationJobID, lease, draft); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSaveGenerationArtifactRejectsDigestConflict(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := newGenerationStore(database, sequenceIDs(generationArtifactID))
	draft := normalizedRequestDraft()
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationJobSQL)).WithArgs(generationJobID).WillReturnRows(
		lockJobRows("TEST_CASES", "VALIDATING_INPUT", 3, 0, 0, false),
	)
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationLeaseSQL)).WithArgs(generationWorkID, "worker", uint64(2)).WillReturnRows(
		sqlmock.NewRows([]string{"aggregate_type", "aggregate_id"}).AddRow("generation_job", generationJobID),
	)
	mock.ExpectExec(regexp.QuoteMeta(upsertGenerationArtifactSQL)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(selectGenerationArtifactDigestSQL)).WithArgs(generationJobID, draft.Kind, draft.Key).WillReturnRows(
		sqlmock.NewRows([]string{"content_sha256"}).AddRow(strings.Repeat("b", 64)),
	)
	mock.ExpectRollback()

	err = store.SaveGenerationArtifact(context.Background(), generationJobID, artifactLease(), draft)
	if !errors.Is(err, generation.ErrConflict) {
		t.Fatalf("error = %v", err)
	}
}

func TestSaveGenerationArtifactRejectsStaleLease(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := newGenerationStore(database, sequenceIDs(generationArtifactID))
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationJobSQL)).WithArgs(generationJobID).WillReturnRows(
		lockJobRows("TEST_CASES", "VALIDATING_INPUT", 3, 0, 0, false),
	)
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationLeaseSQL)).WithArgs(generationWorkID, "worker", uint64(2)).WillReturnRows(
		sqlmock.NewRows([]string{"aggregate_type", "aggregate_id"}),
	)
	mock.ExpectRollback()

	err = store.SaveGenerationArtifact(context.Background(), generationJobID, artifactLease(), normalizedRequestDraft())
	if !errors.Is(err, mysqlqueue.ErrStaleLease) {
		t.Fatalf("error = %v", err)
	}
}

func artifactLease() generation.WorkLease {
	return generation.WorkLease{WorkItemID: generationWorkID, Owner: "worker", FencingToken: 2, AttemptCount: 1, MaxAttempts: 3}
}

func normalizedRequestDraft() generation.ArtifactDraft {
	return generation.ArtifactDraft{
		Kind: generation.ArtifactKindNormalizedRequest, Key: "request",
		ContentJSON: []byte(`{"schema_version":"1.0","description":"Generate tests."}`),
		SHA256:      strings.Repeat("a", 64), Validation: []byte(`{"schema":"normalized-generation-request.schema.json","valid":true}`),
		TTL: 24 * time.Hour,
	}
}
