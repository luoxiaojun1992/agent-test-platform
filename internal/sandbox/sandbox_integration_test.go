package sandbox

import (
	"context"
	"crypto/rand"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/oklog/ulid/v2"
)

// integrationAdmin connects with the administrator DSN in ATP_TEST_MYSQL_DSN, standing in for a registered Target
// administrator. Tests never drop what they create: the databases stay on validation MySQL for traceability.
func integrationAdmin(t *testing.T) (*sql.DB, *mysql.Config) {
	t.Helper()
	dsn := os.Getenv("ATP_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set ATP_TEST_MYSQL_DSN to run MySQL 8.4 sandbox integration tests")
	}
	config, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse ATP_TEST_MYSQL_DSN: %v", err)
	}
	config.DBName = ""
	admin, err := sql.Open("mysql", config.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var version string
	if err := admin.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		t.Fatalf("query MySQL version: %v", err)
	}
	if !strings.HasPrefix(version, "8.4.") {
		t.Fatalf("MySQL version = %q, want 8.4.x", version)
	}
	return admin, config
}

func newTestULID(t *testing.T) string {
	t.Helper()
	id, err := ulid.New(ulid.Timestamp(time.Now()), rand.Reader)
	if err != nil {
		t.Fatalf("generate test ULID: %v", err)
	}
	return id.String()
}

func assertDatabaseExists(ctx context.Context, t *testing.T, admin *sql.DB, name string) {
	t.Helper()
	var count int
	if err := admin.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name = ?`, name).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("database %s does not exist", name)
	}
}

const (
	lifecycleSchema = "CREATE TABLE orders (id VARCHAR(32) PRIMARY KEY, status VARCHAR(32) NOT NULL, updated_at DATETIME(6) NOT NULL) ENGINE=InnoDB"
	lifecycleSeed   = "INSERT INTO orders (id, status, updated_at) VALUES ('O10001', 'PAID', '2026-09-11 09:00:00.000000')"
)

// TestRunDatabaseStateDiffMySQL84 provisions a Run database and captures the state before and after an
// allowed mutation made through the administrator's connection. The diff must match the rule.
func TestRunDatabaseStateDiffMySQL84(t *testing.T) {
	admin, config := integrationAdmin(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	target := AdminConnection{Admin: admin, Base: config}

	identifiers, err := ProvisionRun(ctx, target, newTestULID(t), []string{lifecycleSchema}, []string{lifecycleSeed})
	if err != nil {
		t.Fatal(err)
	}
	database, err := target.OpenDatabase(ctx, identifiers.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	table := []TableSpec{{
		Name: "orders",
		Columns: []ColumnSpec{
			{Name: "id", Type: CellString},
			{Name: "status", Type: CellString},
			{Name: "updated_at", Type: CellDateTime},
		},
		PrimaryKey: []string{"id"},
	}}
	before, err := CaptureState(ctx, database, table)
	if err != nil {
		t.Fatalf("capture before state: %v", err)
	}
	if _, err := database.ExecContext(ctx, `UPDATE orders SET status = 'CANCELLED', updated_at = '2026-09-11 10:00:00.000000' WHERE id = 'O10001'`); err != nil {
		t.Fatalf("execute allowed mutation: %v", err)
	}
	after, err := CaptureState(ctx, database, table)
	if err != nil {
		t.Fatalf("capture after state: %v", err)
	}
	diff, err := DiffStates(before, after)
	if err != nil {
		t.Fatal(err)
	}
	report, err := EvaluateMutations(diff, []MutationRule{{
		ID: "cancel-order", Table: "orders", Operation: OperationUpdate,
		Columns:         []string{"status", "updated_at"},
		Predicate:       Predicate{Column: "id", Operator: "=", Value: "O10001"},
		MinAffectedRows: 1, MaxAffectedRows: 1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Pass {
		t.Fatalf("allowed mutation failed: %#v", report)
	}
	// The run database is kept after the run, for traceability.
	assertDatabaseExists(ctx, t, admin, identifiers.Database)
}
