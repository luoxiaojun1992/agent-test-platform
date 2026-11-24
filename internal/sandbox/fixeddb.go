package sandbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrLeftoverTestData reports that test tables from a previous attempt remain in the Agent's
// fixed database, so the Run cannot start safely.
var ErrLeftoverTestData = errors.New("leftover test data in the Agent database")

// ProvisionFixedDatabase checks for leftover test tables and applies the validated schema and
// seed to the Agent's fixed database. It never creates or drops the database itself (08).
func ProvisionFixedDatabase(ctx context.Context, database *sql.DB, tables []string, schemaStatements, seedStatements []string) error {
	leftover, err := findExistingTables(ctx, database, tables)
	if err != nil {
		return err
	}
	if len(leftover) > 0 {
		return fmt.Errorf("%w: %s", ErrLeftoverTestData, strings.Join(leftover, ", "))
	}
	statements := append(append([]string(nil), schemaStatements...), seedStatements...)
	for index, statement := range statements {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply Agent database statement %d: %w", index+1, err)
		}
	}
	return nil
}

// CleanupFixedDatabase drops the test tables from the Agent's fixed database.
func CleanupFixedDatabase(ctx context.Context, database *sql.DB, tables []string) error {
	for _, table := range tables {
		quoted, err := QuoteIdentifier(table)
		if err != nil {
			return err
		}
		if _, err := database.ExecContext(ctx, "DROP TABLE IF EXISTS "+quoted); err != nil {
			return fmt.Errorf("drop table %s: %w", table, err)
		}
	}
	return nil
}

// findExistingTables returns the subset of tables that already exist in the connected database.
func findExistingTables(ctx context.Context, database *sql.DB, tables []string) ([]string, error) {
	if len(tables) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(tables)), ",")
	args := make([]any, len(tables))
	for index, table := range tables {
		args[index] = table
	}
	rows, err := database.QueryContext(ctx,
		`SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("check for leftover test tables: %w", err)
	}
	defer rows.Close()
	var existing []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		existing = append(existing, name)
	}
	return existing, rows.Err()
}
