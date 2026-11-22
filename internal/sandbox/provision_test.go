package sandbox

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-sql-driver/mysql"
)

const (
	runFixtureSchema = "CREATE TABLE orders (id VARCHAR(32) PRIMARY KEY, status VARCHAR(32) NOT NULL)"
	runFixtureSeed   = "INSERT INTO orders (id, status) VALUES ('O10001', 'PAID')"
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

// fakeRunTarget records administrative statements and hands out one scripted database connection.
type fakeRunTarget struct {
	fakeAdmin
	database *sql.DB
	opened   []string
}

func (target *fakeRunTarget) OpenDatabase(_ context.Context, databaseName string) (*sql.DB, error) {
	target.opened = append(target.opened, databaseName)
	return target.database, nil
}

// newRunDatabase returns a database connection that expects the given statements in order. The
// statement equal to failing returns an error.
func newRunDatabase(t *testing.T, failing string, statements ...string) *sql.DB {
	t.Helper()
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	for _, statement := range statements {
		expectation := mock.ExpectExec(regexp.QuoteMeta(statement))
		if statement == failing {
			expectation.WillReturnError(errors.New("injected load failure"))
		} else {
			expectation.WillReturnResult(sqlmock.NewResult(0, 0))
		}
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet statement expectations: %v", err)
		}
	})
	return database
}

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

func TestProvisionRunCreatesDatabaseAndLoadsSchemaAndSeed(t *testing.T) {
	t.Parallel()

	target := &fakeRunTarget{database: newRunDatabase(t, "", runFixtureSchema, runFixtureSeed)}
	identifiers, err := ProvisionRun(context.Background(), target, testRunID, []string{runFixtureSchema}, []string{runFixtureSeed})
	if err != nil {
		t.Fatal(err)
	}
	if identifiers.Database != "run_01arz3ndektsv4rrffq69g5fav" {
		t.Fatalf("database = %q", identifiers.Database)
	}
	if len(target.statements) != 1 || !strings.HasPrefix(target.statements[0], "CREATE DATABASE `run_01arz3ndektsv4rrffq69g5fav`") {
		t.Fatalf("administrative statements = %q", target.statements)
	}
	if len(target.opened) != 1 || target.opened[0] != identifiers.Database {
		t.Fatalf("opened = %q, want the run database", target.opened)
	}
	assertNoDestructiveStatement(t, target.statements)
}

// A Run database that already exists may hold changes from an earlier attempt. It is never reused:
// nothing is loaded, and the database stays unchanged.
func TestProvisionRunExistingDatabaseIsRejectedWithoutChanges(t *testing.T) {
	t.Parallel()

	target := &fakeRunTarget{fakeAdmin: fakeAdmin{
		failOn:  "CREATE DATABASE",
		failErr: &mysql.MySQLError{Number: mysqlErrDatabaseExists, Message: "Can't create database; database exists"},
	}}
	_, err := ProvisionRun(context.Background(), target, testRunID, []string{runFixtureSchema}, []string{runFixtureSeed})
	if !errors.Is(err, ErrDatabaseExists) {
		t.Fatalf("error = %v, want ErrDatabaseExists", err)
	}
	if len(target.statements) != 1 || len(target.opened) != 0 {
		t.Fatalf("an existing run database led to further work: statements=%q opened=%q", target.statements, target.opened)
	}
}

// A failed load leaves the run database in place for manual deletion and returns the error.
func TestProvisionRunLoadFailureKeepsDatabase(t *testing.T) {
	t.Parallel()

	target := &fakeRunTarget{database: newRunDatabase(t, runFixtureSeed, runFixtureSchema, runFixtureSeed)}
	if _, err := ProvisionRun(context.Background(), target, testRunID, []string{runFixtureSchema}, []string{runFixtureSeed}); err == nil {
		t.Fatal("a failed seed load succeeded")
	}
	assertNoDestructiveStatement(t, target.statements)
	if len(target.statements) != 1 {
		t.Fatalf("statements = %q, want only the CREATE DATABASE", target.statements)
	}
}

func TestProvisionRunRejectsUnsafeRunID(t *testing.T) {
	t.Parallel()

	target := &fakeRunTarget{database: newRunDatabase(t, "")}
	if _, err := ProvisionRun(context.Background(), target, "not-a-ulid", []string{runFixtureSchema}, nil); err == nil {
		t.Fatal("an invalid run ID was accepted")
	}
	if len(target.statements) != 0 {
		t.Fatalf("SQL executed for an invalid run ID: %q", target.statements)
	}
}
