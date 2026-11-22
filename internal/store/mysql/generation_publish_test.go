package mysqlstore

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"agent-test-platform/internal/generation"
	"agent-test-platform/internal/jobs/mysqlqueue"

	"github.com/DATA-DOG/go-sqlmock"
)

func publishLockRows(kind, status string, requestedCount int, requestID string, inputScenarioRevisionID, inputDatasetRevisionID any) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"job_kind", "pipeline_id", "status", "repair_round", "requested_test_case_count",
		"accepted_test_case_count", "request_id", "input_scenario_revision_id", "input_dataset_revision_id", "generator_config_sha256",
		"require_manual_review",
	}).AddRow(kind, nil, status, 0, requestedCount, 0, requestID, inputScenarioRevisionID, inputDatasetRevisionID, strings.Repeat("c", 64), false)
}

func TestPublishGenerationRejectsNonPublishingStatus(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := newGenerationStore(database, sequenceIDs())
	lease := artifactLease()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationJobForPublishSQL)).WithArgs(generationJobID).WillReturnRows(
		publishLockRows("SCENARIO", "GENERATING_SCENARIO", 0, generationRequestID, nil, nil),
	)
	mock.ExpectRollback()

	if _, err := store.PublishGeneration(context.Background(), generationJobID, lease); !errors.Is(err, generation.ErrConflict) {
		t.Fatalf("error = %v", err)
	}
}

func TestPublishGenerationRejectsMissingJob(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := newGenerationStore(database, sequenceIDs())
	lease := artifactLease()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationJobForPublishSQL)).WithArgs(generationJobID).WillReturnRows(
		sqlmock.NewRows([]string{
			"job_kind", "pipeline_id", "status", "repair_round", "requested_test_case_count",
			"accepted_test_case_count", "request_id", "input_scenario_revision_id", "input_dataset_revision_id", "generator_config_sha256",
			"require_manual_review",
		}),
	)
	mock.ExpectRollback()

	if _, err := store.PublishGeneration(context.Background(), generationJobID, lease); !errors.Is(err, generation.ErrNotFound) {
		t.Fatalf("error = %v", err)
	}
}

func TestPublishGenerationRejectsStaleLease(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := newGenerationStore(database, sequenceIDs())
	lease := artifactLease()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationJobForPublishSQL)).WithArgs(generationJobID).WillReturnRows(
		publishLockRows("SCENARIO", "PUBLISHING", 0, generationRequestID, nil, nil),
	)
	mock.ExpectQuery(regexp.QuoteMeta(lockGenerationLeaseSQL)).WithArgs(lease.WorkItemID, lease.Owner, lease.FencingToken).WillReturnRows(
		sqlmock.NewRows([]string{"aggregate_type", "aggregate_id"}),
	)
	mock.ExpectRollback()

	if _, err := store.PublishGeneration(context.Background(), generationJobID, lease); !errors.Is(err, mysqlqueue.ErrStaleLease) {
		t.Fatalf("error = %v", err)
	}
}
