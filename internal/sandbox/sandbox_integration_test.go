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
