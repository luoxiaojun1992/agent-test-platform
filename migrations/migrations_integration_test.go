package migrations

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func TestApplyToMySQL84(t *testing.T) {
	dsn := os.Getenv("ATP_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set ATP_TEST_MYSQL_DSN to run the MySQL 8.4 migration integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	adminConfig, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse ATP_TEST_MYSQL_DSN: %v", err)
	}
	adminConfig.DBName = ""
	adminDB := openMySQL(t, adminConfig)
	t.Cleanup(func() { _ = adminDB.Close() })

	if err := adminDB.PingContext(ctx); err != nil {
		t.Fatalf("ping MySQL: %v", err)
	}
	var version string
	if err := adminDB.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		t.Fatalf("query MySQL version: %v", err)
	}
	if !strings.HasPrefix(version, "8.4.") {
		t.Fatalf("MySQL version = %q, want 8.4.x", version)
	}

	// The database keeps its unique name and is not dropped afterwards, so the test data stays for traceability.
	databaseName := "atp_migration_" + randomHex(t, 8)
	quotedDatabase := quoteIdentifier(databaseName)
	if _, err := adminDB.ExecContext(ctx, "CREATE DATABASE "+quotedDatabase+" CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatalf("create test database: %v", err)
	}

	databaseConfig := *adminConfig
	databaseConfig.DBName = databaseName
	databaseConfig.ParseTime = true
	databaseConfig.Loc = time.UTC
	databaseDB := openMySQL(t, &databaseConfig)
	defer databaseDB.Close()

	if _, err := databaseDB.ExecContext(ctx, "SET time_zone = '+00:00'"); err != nil {
		t.Fatalf("set UTC session timezone: %v", err)
	}
	if err := Apply(ctx, databaseDB); err != nil {
		t.Fatal(err)
	}

	wantTables := []string{
		"agent",
		"agent_session_event",
		"database_profile",
		"dataset",
		"dataset_revision",
		"dataset_revision_validation_db",
		"evaluation_attempt",
		"generation_artifact_draft",
		"generation_job",
		"generation_package",
		"generation_package_test_case",
		"generation_pipeline",
		"generation_request",
		"generation_test_case_draft",
		"judge_calibration",
		"model_capability_probe",
		"model_config",
		"scenario",
		"scenario_revision",
		"secret",
		"target_knowledge_document",
		"test_case",
		"test_case_revision",
		"test_run",
		"tool_call_record",
		"validation_mysql",
		"work_item",
	}
	gotTables := readTableNames(t, ctx, databaseDB, databaseName)
	if strings.Join(gotTables, "\n") != strings.Join(wantTables, "\n") {
		t.Fatalf("tables mismatch\ngot:\n%s\nwant:\n%s", strings.Join(gotTables, "\n"), strings.Join(wantTables, "\n"))
	}

	var stageKeyCount int
	if err := databaseDB.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM information_schema.columns
		WHERE table_schema = ? AND table_name = 'work_item' AND column_name = 'stage_key'
	`, databaseName).Scan(&stageKeyCount); err != nil {
		t.Fatalf("inspect work_item.stage_key: %v", err)
	}
	if stageKeyCount != 1 {
		t.Fatalf("work_item.stage_key count = %d, want 1", stageKeyCount)
	}

	for _, column := range []struct{ table, name string }{
		{"scenario_revision", "lifecycle_status"},
		{"dataset_revision", "lifecycle_status"},
		{"generation_job", "provenance_json"},
	} {
		var count int
		if err := databaseDB.QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM information_schema.columns
			WHERE table_schema = ? AND table_name = ? AND column_name = ?
		`, databaseName, column.table, column.name).Scan(&count); err != nil {
			t.Fatalf("inspect %s.%s: %v", column.table, column.name, err)
		}
		if count != 1 {
			t.Fatalf("%s.%s count = %d, want 1", column.table, column.name, count)
		}
	}
}

func openMySQL(t *testing.T, config *mysql.Config) *sql.DB {
	t.Helper()
	connector, err := mysql.NewConnector(config)
	if err != nil {
		t.Fatalf("create MySQL connector: %v", err)
	}
	database := sql.OpenDB(connector)
	database.SetMaxOpenConns(2)
	database.SetMaxIdleConns(2)
	database.SetConnMaxLifetime(time.Minute)
	return database
}

func readTableNames(t *testing.T, ctx context.Context, database *sql.DB, databaseName string) []string {
	t.Helper()
	rows, err := database.QueryContext(ctx, `
		SELECT table_name
		FROM information_schema.tables
		WHERE table_schema = ? AND table_type = 'BASE TABLE'
		ORDER BY table_name
	`, databaseName)
	if err != nil {
		t.Fatalf("list migrated tables: %v", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan migrated table: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate migrated tables: %v", err)
	}
	sort.Strings(names)
	return names
}

func randomHex(t *testing.T, byteCount int) string {
	t.Helper()
	value := make([]byte, byteCount)
	if _, err := rand.Read(value); err != nil {
		t.Fatalf("generate random database suffix: %v", err)
	}
	return hex.EncodeToString(value)
}

func quoteIdentifier(identifier string) string {
	return fmt.Sprintf("`%s`", strings.ReplaceAll(identifier, "`", "``"))
}
