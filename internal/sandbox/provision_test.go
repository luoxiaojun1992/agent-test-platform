package sandbox

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

type fakeAdmin struct {
	statements []string
	failOn     string
	failErr    error
}

func (fake *fakeAdmin) ExecContext(_ context.Context, statement string, _ ...any) (sql.Result, error) {
	fake.statements = append(fake.statements, statement)
	if fake.failOn != "" && strings.Contains(statement, fake.failOn) {
		if fake.failErr != nil {
			return nil, fake.failErr
		}
		return nil, errors.New("injected failure")
	}
	return fakeResult{}, nil
}

type fakeResult struct{}

func (fakeResult) LastInsertId() (int64, error) { return 0, nil }
func (fakeResult) RowsAffected() (int64, error) { return 0, nil }

// assertNoDestructiveStatement fails when any statement drops a validation MySQL object or account. The
// platform never removes validation MySQL objects; an operator does that by hand.
func assertNoDestructiveStatement(t *testing.T, statements []string) {
	t.Helper()
	for _, statement := range statements {
		if strings.Contains(strings.ToUpper(statement), "DROP ") {
			t.Fatalf("statement %q removes a validation MySQL object", statement)
		}
	}
}

func TestProvisionDatasetCreatesOnlyTheDatabase(t *testing.T) {
	t.Parallel()

	admin := &fakeAdmin{}
	identifiers, err := ProvisionDataset(context.Background(), admin, testDatasetRevisionID)
	if err != nil {
		t.Fatal(err)
	}
	if identifiers.Database != "dataset_01k4z6y7x8w9v0t1s2r3q4p5n6" {
		t.Fatalf("database = %q", identifiers.Database)
	}
	if len(admin.statements) != 1 || !strings.HasPrefix(admin.statements[0], "CREATE DATABASE `dataset_") {
		t.Fatalf("statements = %q, want one CREATE DATABASE", admin.statements)
	}
	for _, statement := range admin.statements {
		if strings.Contains(strings.ToUpper(statement), "USER") || strings.Contains(strings.ToUpper(statement), "GRANT") {
			t.Fatalf("statement %q creates an account", statement)
		}
	}
}

// A failed database creation changes nothing else. The platform never drops a database on failure.
func TestProvisionDatasetFailureStopsWithoutCleanup(t *testing.T) {
	t.Parallel()

	admin := &fakeAdmin{failOn: "CREATE DATABASE", failErr: errors.New("access denied")}
	if _, err := ProvisionDataset(context.Background(), admin, testDatasetRevisionID); err == nil {
		t.Fatal("a failed CREATE DATABASE succeeded")
	}
	if len(admin.statements) != 1 {
		t.Fatalf("statements = %q, want only the failed CREATE DATABASE", admin.statements)
	}
	assertNoDestructiveStatement(t, admin.statements)
}
