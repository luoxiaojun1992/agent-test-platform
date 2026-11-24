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
