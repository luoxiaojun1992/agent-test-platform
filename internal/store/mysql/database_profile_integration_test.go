package mysqlstore

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"agent-test-platform/internal/generation"
	"agent-test-platform/migrations"

	"github.com/go-sql-driver/mysql"
)

// TestDatabaseProfileLifecycleOnMySQL84 exercises Database Profile creation, idempotent replay,
// conflicts, and cursor pagination against a real MySQL 8.4 instance (docs/architecture.md 17.2).
func TestDatabaseProfileLifecycleOnMySQL84(t *testing.T) {
	dsn := os.Getenv("ATP_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set ATP_TEST_MYSQL_DSN to run the MySQL 8.4 Database Profile integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	adminConfig, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse ATP_TEST_MYSQL_DSN: %v", err)
	}
	adminConfig.DBName = ""
	adminDB := openPublishMySQL(t, adminConfig)
	t.Cleanup(func() { _ = adminDB.Close() })

	databaseName := "atp_profile_" + randomPublishHex(t, 8)
	quoted := "`" + databaseName + "`"
	if _, err := adminDB.ExecContext(ctx, "CREATE DATABASE "+quoted+" CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatalf("create test database: %v", err)
	}

	databaseConfig := *adminConfig
	databaseConfig.DBName = databaseName
	databaseConfig.ParseTime = true
	databaseConfig.Loc = time.UTC
	database := openPublishMySQL(t, &databaseConfig)
	defer database.Close()
	if _, err := database.ExecContext(ctx, "SET time_zone = '+00:00'"); err != nil {
		t.Fatalf("set UTC session timezone: %v", err)
	}
	if err := migrations.Apply(ctx, database); err != nil {
		t.Fatal(err)
	}
	store, err := NewGenerationStore(database)
	if err != nil {
		t.Fatal(err)
	}

	request := generation.DatabaseProfileRequest{
		Name: "reference-sales", Engine: "MYSQL", Host: "target-mysql", Port: 3306, Database: "reference_sales",
		Username: "atp_reader", PasswordSecretRef: "sales-reader-password", TLSMode: "VERIFY_IDENTITY",
		SnapshotMode: "SCHEMA_AND_DATA", SnapshotTimeoutSeconds: 120,
	}
	configJSON := []byte(`{"name":"reference-sales","engine":"MYSQL","host":"target-mysql","port":3306,` +
		`"database":"reference_sales","username":"atp_reader","password_secret_ref":"sales-reader-password",` +
		`"tls_mode":"VERIFY_IDENTITY","snapshot_mode":"SCHEMA_AND_DATA","snapshot_timeout_seconds":120}`)
	digest := strings.Repeat("a", 64)
	input := generation.CreateDatabaseProfileInput{
		IdempotencyKey: "profile-create-key-1", ConfigJSON: configJSON, ConfigSHA256: digest, Request: request,
	}

	created, err := store.CreateDatabaseProfile(ctx, input)
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	if created.Name != "reference-sales" || created.SnapshotMode != "SCHEMA_AND_DATA" || created.CreatedAt.IsZero() {
		t.Fatalf("created = %#v", created)
	}

	replayed, err := store.CreateDatabaseProfile(ctx, input)
	if err != nil || replayed.ID != created.ID {
		t.Fatalf("replay = %#v error=%v, want profile %s", replayed, err, created.ID)
	}

	otherBody := input
	otherBody.ConfigSHA256 = strings.Repeat("b", 64)
	if _, err := store.CreateDatabaseProfile(ctx, otherBody); !errors.Is(err, generation.ErrIdempotencyConflict) {
		t.Fatalf("same key, different configuration error = %v, want ErrIdempotencyConflict", err)
	}

	sameName := input
	sameName.IdempotencyKey = "profile-create-key-2"
	sameName.ConfigSHA256 = strings.Repeat("c", 64)
	if _, err := store.CreateDatabaseProfile(ctx, sameName); !errors.Is(err, generation.ErrConflict) {
		t.Fatalf("duplicate name error = %v, want ErrConflict", err)
	}

	sameConfig := input
	sameConfig.IdempotencyKey = "profile-create-key-3"
	if _, err := store.CreateDatabaseProfile(ctx, sameConfig); !errors.Is(err, generation.ErrConflict) {
		t.Fatalf("duplicate configuration under new key error = %v, want ErrConflict", err)
	}

	for index := 0; index < 2; index++ {
		next := input
		next.IdempotencyKey = "profile-create-more-" + string(rune('a'+index))
		next.Request.Name = "reference-extra-" + string(rune('a'+index))
		next.ConfigSHA256 = strings.Repeat(string(rune('d'+index)), 64)
		if _, err := store.CreateDatabaseProfile(ctx, next); err != nil {
			t.Fatalf("create extra profile %d: %v", index, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	first, cursor, err := store.ListDatabaseProfiles(ctx, "", 2)
	if err != nil || len(first) != 2 || cursor == "" {
		t.Fatalf("first page = %d profiles cursor=%q error=%v", len(first), cursor, err)
	}
	second, nextCursor, err := store.ListDatabaseProfiles(ctx, cursor, 2)
	if err != nil || len(second) != 1 || nextCursor != "" {
		t.Fatalf("second page = %d profiles cursor=%q error=%v", len(second), nextCursor, err)
	}
	seen := map[string]bool{}
	for _, profile := range append(first, second...) {
		if seen[profile.ID] {
			t.Fatalf("profile %s listed twice", profile.ID)
		}
		seen[profile.ID] = true
	}
	if len(seen) != 3 || !seen[created.ID] {
		t.Fatalf("listed profiles = %v, want 3 distinct including %s", seen, created.ID)
	}

	if _, _, err := store.ListDatabaseProfiles(ctx, "not-a-cursor!", 2); !errors.Is(err, generation.ErrInvalidCursor) {
		t.Fatalf("invalid cursor error = %v, want ErrInvalidCursor", err)
	}

	var secretRef string
	if err := database.QueryRowContext(ctx, "SELECT password_secret_ref FROM database_profile WHERE id = ?", created.ID).Scan(&secretRef); err != nil {
		t.Fatal(err)
	}
	if secretRef != "sales-reader-password" {
		t.Fatalf("stored secret reference = %q", secretRef)
	}

	connection, err := store.LoadDatabaseProfileConnection(ctx, created.ID)
	if err != nil || connection.PasswordSecretRef != "sales-reader-password" || connection.ConfigSHA256 != digest ||
		connection.SnapshotMode != "SCHEMA_AND_DATA" || connection.SnapshotTimeoutSeconds != 120 || connection.Host != "target-mysql" {
		t.Fatalf("connection = %#v error = %v", connection, err)
	}
	if _, err := store.LoadDatabaseProfileConnection(ctx, "not-a-ulid"); !errors.Is(err, generation.ErrNotFound) {
		t.Fatalf("invalid id error = %v, want ErrNotFound", err)
	}
}
