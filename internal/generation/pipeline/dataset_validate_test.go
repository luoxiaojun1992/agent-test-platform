package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"testing"

	"agent-test-platform/contracts"
	"agent-test-platform/internal/generation"

	"github.com/DATA-DOG/go-sqlmock"
)

type fakeResult struct{}

func (fakeResult) LastInsertId() (int64, error) { return 0, nil }
func (fakeResult) RowsAffected() (int64, error) { return 0, nil }

type fakeValidationDB struct {
	execErr   error
	execCalls []string
	loader    *sql.DB
	openErr   error
}

func (target *fakeValidationDB) ExecContext(_ context.Context, query string, _ ...any) (sql.Result, error) {
	target.execCalls = append(target.execCalls, query)
	if target.execErr != nil {
		return nil, target.execErr
	}
	return fakeResult{}, nil
}

func (target *fakeValidationDB) OpenDatabase(context.Context, string) (*sql.DB, error) {
	if target.openErr != nil {
		return nil, target.openErr
	}
	return target.loader, nil
}

const validatingDatasetSchemaSQL = "CREATE TABLE orders (id VARCHAR(32) PRIMARY KEY, status VARCHAR(32) NOT NULL)"
const validatingDatasetSeedSQL = "INSERT INTO orders (id, status) VALUES ('O10001', 'PAID')"
const validatingDatasetConstraintQuery = "SELECT id FROM orders WHERE status = 'CANCELLED'"

func validatingDatasetDraft(t *testing.T) string {
	t.Helper()
	return `{
  "schema_sql": "` + validatingDatasetSchemaSQL + `",
  "seed_sql": "` + validatingDatasetSeedSQL + `",
  "constraints": {
    "version": "1.0",
    "checks": [
      {
        "id": "no-cancelled-orders-yet",
        "phases": ["AFTER_SEED"],
        "query": "` + validatingDatasetConstraintQuery + `",
        "expectation": {"type": "ROW_COUNT", "equals": 0},
        "critical": true,
        "rule_id": "cancel-paid-order"
      }
    ]
  },
  "metadata": {
    "schema_version": "1.0", "mysql_version": "8.4", "timezone": "UTC", "charset": "utf8mb4",
    "collation": "utf8mb4_0900_ai_ci", "sql_mode": ["STRICT_TRANS_TABLES"],
    "tables": [{"name": "orders", "columns": [{"name": "id", "mysql_type": "VARCHAR(32)", "nullable": false}, {"name": "status", "mysql_type": "VARCHAR(32)", "nullable": false}], "primary_key": ["id"], "foreign_keys": [], "row_count": 1}],
    "difficulty_profile": {"data_complexity":1,"reasoning_complexity":1,"tool_complexity":1,"ambiguity":1,"multi_step":1,"state_change_complexity":1,"adversarial_complexity":1},
    "coverage_tags": ["NORMAL"],
    "content_sha256": "` + strings.Repeat("a", 64) + `"
  }
}`
}

func datasetValidationFakeStore(t *testing.T) *fakeStore {
	t.Helper()
	document := []byte(validatingDatasetDraft(t))
	canonical, digest, err := generation.CanonicalJSON(document)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeStore{
		artifact: generation.ArtifactSnapshot{
			Kind: generation.ArtifactKindDataset, Key: datasetArtifactKey, ContentJSON: canonical, SHA256: digest,
		},
		artifactsByKind: map[string]generation.ArtifactSnapshot{generation.ArtifactKindNormalizedRequest: normalizedArtifact(t)},
	}
}

func newValidateDatasetProcessor(t *testing.T, store Store, target ValidationDB) *Processor {
	t.Helper()
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	processor, err := New(store, validator, WithValidationDB(singleTarget{target}))
	if err != nil {
		t.Fatal(err)
	}
	return processor
}

func TestValidateDatasetLoadsFixtureAndPassesConstraints(t *testing.T) {
	t.Parallel()

	loader, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec(regexp.QuoteMeta(validatingDatasetSchemaSQL)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(validatingDatasetSeedSQL)).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(regexp.QuoteMeta(validatingDatasetConstraintQuery)).WillReturnRows(sqlmock.NewRows([]string{"id"}))

	store := datasetValidationFakeStore(t)
	target := &fakeValidationDB{loader: loader}
	processor := newValidateDatasetProcessor(t, store, target)
	result, err := processor.Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusValidatingDataset, Lease: validLease(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Failure != nil || result.Transition.Event != generation.EventValidationAccepted {
		t.Fatalf("result = %#v", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
	if store.saved == nil || store.saved.Kind != generation.ArtifactKindDatasetExecution {
		t.Fatalf("execution artifact = %#v", store.saved)
	}
	for _, call := range target.execCalls {
		if strings.Contains(strings.ToUpper(call), "DROP ") {
			t.Fatalf("dataset database was dropped on success: %v", target.execCalls)
		}
	}
	if len(target.execCalls) != 1 || !strings.HasPrefix(target.execCalls[0], "CREATE DATABASE `dataset_") {
		t.Fatalf("Target statements = %v, want one CREATE DATABASE for the dataset", target.execCalls)
	}
}

func TestValidateDatasetRejectsCriticalConstraintViolation(t *testing.T) {
	t.Parallel()

	loader, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec(regexp.QuoteMeta(validatingDatasetSchemaSQL)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(validatingDatasetSeedSQL)).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(regexp.QuoteMeta(validatingDatasetConstraintQuery)).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("O10001"))

	store := datasetValidationFakeStore(t)
	target := &fakeValidationDB{loader: loader}
	processor := newValidateDatasetProcessor(t, store, target)
	result, err := processor.Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusValidatingDataset, Lease: validLease(),
	})
	if err != nil || result.Failure != nil || result.Transition.Event != generation.EventValidationRejected || !result.Transition.AllErrorsRetryable {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	for _, call := range target.execCalls {
		if strings.Contains(strings.ToUpper(call), "DROP ") {
			t.Fatalf("a rejected dataset database was dropped: %v", target.execCalls)
		}
	}
}

func TestValidateDatasetRejectsExecutionFailure(t *testing.T) {
	t.Parallel()

	loader, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec(regexp.QuoteMeta(validatingDatasetSchemaSQL)).WillReturnError(errors.New("syntax error"))

	store := datasetValidationFakeStore(t)
	target := &fakeValidationDB{loader: loader}
	processor := newValidateDatasetProcessor(t, store, target)
	result, err := processor.Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusValidatingDataset, Lease: validLease(),
	})
	if err != nil || result.Failure != nil || result.Transition.Event != generation.EventValidationRejected || !result.Transition.AllErrorsRetryable {
		t.Fatalf("result=%#v error=%v", result, err)
	}
}

func TestValidateDatasetRejectsProvisioningFailure(t *testing.T) {
	t.Parallel()

	store := datasetValidationFakeStore(t)
	target := &fakeValidationDB{execErr: errors.New("target mysql unreachable")}
	processor := newValidateDatasetProcessor(t, store, target)
	result, err := processor.Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusValidatingDataset, Lease: validLease(),
	})
	if err != nil || result.Failure == nil || result.Failure.Code != "VALIDATION_DB_UNAVAILABLE" {
		t.Fatalf("result=%#v error=%v", result, err)
	}
}

func TestValidateDatasetRejectsMissingDraft(t *testing.T) {
	t.Parallel()

	processor := newValidateDatasetProcessor(t, &fakeStore{artifactErr: generation.ErrNotFound}, &fakeValidationDB{})
	result, err := processor.Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusValidatingDataset, Lease: validLease(),
	})
	if err != nil || result.Failure == nil || result.Failure.Code != "REFERENCE_NOT_FOUND" {
		t.Fatalf("result=%#v error=%v", result, err)
	}
}
