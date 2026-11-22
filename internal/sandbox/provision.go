package sandbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/go-sql-driver/mysql"
)

// mysqlErrDatabaseExists is MySQL error 1007, returned by CREATE DATABASE when the database exists.
const mysqlErrDatabaseExists = 1007

// ErrDatabaseExists reports that the database a provisioning call would create is already present.
// An existing database is never reused, because an earlier attempt may have changed its data.
var ErrDatabaseExists = errors.New("database already exists")

// AdminExecer executes fixed administrative statements on validation MySQL.
type AdminExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// RunTarget is what Run provisioning needs from validation MySQL: administrative statements, and a
// connection to one database, both through the configured administrator account.
type RunTarget interface {
	AdminExecer
	OpenDatabase(ctx context.Context, databaseName string) (*sql.DB, error)
}

// ProvisionDataset creates the execution database of one Dataset Revision through the configured
// administrator account. It creates no account. Call it once per validation attempt, with a fresh
// Revision ID. A failed call leaves what it created in place; validation MySQL objects are deleted manually.
func ProvisionDataset(ctx context.Context, admin AdminExecer, datasetRevisionID string) (DatasetIdentifiers, error) {
	identifiers, err := DeriveDatasetIdentifiers(datasetRevisionID)
	if err != nil {
		return DatasetIdentifiers{}, err
	}
	if err := createDatabase(ctx, admin, identifiers.Database); err != nil {
		return DatasetIdentifiers{}, err
	}
	return identifiers, nil
}

// ProvisionRun creates one Test Run's database and loads it with the validated schema and seed
// statements, through the configured administrator account. It creates no account. It never reuses an
// existing database: ErrDatabaseExists is returned and nothing else changes. A failed load leaves the
// database in place for manual deletion.
func ProvisionRun(ctx context.Context, target RunTarget, runID string, schemaStatements, seedStatements []string) (RunIdentifiers, error) {
	identifiers, err := DeriveRunIdentifiers(runID)
	if err != nil {
		return RunIdentifiers{}, err
	}
	if err := createDatabase(ctx, target, identifiers.Database); err != nil {
		return RunIdentifiers{}, err
	}
	database, err := target.OpenDatabase(ctx, identifiers.Database)
	if err != nil {
		return RunIdentifiers{}, fmt.Errorf("open run database: %w", err)
	}
	defer database.Close()
	statements := append(append([]string(nil), schemaStatements...), seedStatements...)
	for index, statement := range statements {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			return RunIdentifiers{}, fmt.Errorf("apply run statement %d: %w", index+1, err)
		}
	}
	return identifiers, nil
}

func createDatabase(ctx context.Context, admin AdminExecer, databaseName string) error {
	quoted, err := QuoteIdentifier(databaseName)
	if err != nil {
		return err
	}
	_, err = admin.ExecContext(ctx, "CREATE DATABASE "+quoted+" CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci")
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == mysqlErrDatabaseExists {
		return fmt.Errorf("%w: %s", ErrDatabaseExists, databaseName)
	}
	if err != nil {
		return fmt.Errorf("create database: %w", err)
	}
	return nil
}
