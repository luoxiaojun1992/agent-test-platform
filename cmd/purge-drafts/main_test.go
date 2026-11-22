package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"agent-test-platform/internal/generation"
	mysqlstore "agent-test-platform/internal/store/mysql"
	"agent-test-platform/migrations"

	"github.com/go-sql-driver/mysql"
	"github.com/oklog/ulid/v2"
)

func TestPlatformDSNIsRequired(t *testing.T) {
	t.Parallel()

	if _, err := platformDSN(func(string) (string, bool) { return "", false }); err == nil {
		t.Fatal("missing PLATFORM_MYSQL_DSN was accepted")
	}
	if _, err := platformDSN(func(string) (string, bool) { return "  ", true }); err == nil {
		t.Fatal("blank PLATFORM_MYSQL_DSN was accepted")
	}
	dsn, err := platformDSN(func(string) (string, bool) { return " root:pw@tcp(127.0.0.1:3306)/platform ", true })
	if err != nil || dsn != "root:pw@tcp(127.0.0.1:3306)/platform" {
		t.Fatalf("dsn = %q error = %v", dsn, err)
	}
}

func TestOpenPlatformDatabaseRequiresDatabaseName(t *testing.T) {
	t.Parallel()

	if _, err := openPlatformDatabase("root:pw@tcp(127.0.0.1:3306)/"); err == nil {
		t.Fatal("a DSN without a database name was accepted")
	}
}

// TestPurgeDraftsOnMySQL84 runs the purge on a freshly migrated MySQL 8.4 database. Only expired drafts
// of finished Jobs are deleted. A Job awaiting review keeps its expired drafts, and a second run deletes
// nothing more.
func TestPurgeDraftsOnMySQL84(t *testing.T) {
	dsn := os.Getenv("ATP_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set ATP_TEST_MYSQL_DSN to run the MySQL 8.4 staging purge test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	adminConfig, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse ATP_TEST_MYSQL_DSN: %v", err)
	}
	adminConfig.DBName = ""
	admin, err := sql.Open("mysql", adminConfig.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	name := "atp_purge_" + strings.ToLower(ulid.Make().String())
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatalf("create test database: %v", err)
	}

	databaseConfig := adminConfig.Clone()
	databaseConfig.DBName = name
	dsnForCommand := databaseConfig.FormatDSN()
	database, err := openPlatformDatabase(dsnForCommand)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := migrations.Apply(ctx, database); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	store, err := mysqlstore.NewGenerationStore(database)
	if err != nil {
		t.Fatal(err)
	}
	seedPurgeTarget(t, ctx, database)

	finishedJob := createPurgeJob(t, ctx, store, false)
	awaitingJob := createPurgeJob(t, ctx, store, true)
	setPurgeJobStatus(t, ctx, database, finishedJob, "COMPLETED")
	setPurgeJobStatus(t, ctx, database, awaitingJob, "AWAITING_REVIEW")
	insertPurgeDraft(t, ctx, database, finishedJob, "expired", -1)
	insertPurgeDraft(t, ctx, database, finishedJob, "fresh", 1)
	insertPurgeDraft(t, ctx, database, awaitingJob, "waiting", -1)

	purged, err := purgeDrafts(ctx, dsnForCommand)
	if err != nil {
		t.Fatalf("purge drafts: %v", err)
	}
	if purged.ArtifactDrafts != 1 || purged.TestCaseDrafts != 0 {
		t.Fatalf("purged = %#v, want only the finished Job's expired draft", purged)
	}
	for key, want := range map[string]int{"expired": 0, "fresh": 1, "waiting": 1} {
		if got := countPurgeDrafts(t, ctx, database, key); got != want {
			t.Fatalf("drafts named %q = %d, want %d", key, got, want)
		}
	}
	again, err := purgeDrafts(ctx, dsnForCommand)
	if err != nil || again.ArtifactDrafts != 0 || again.TestCaseDrafts != 0 {
		t.Fatalf("second purge = %#v error = %v, want nothing left to delete", again, err)
	}
}

// purgeValidationDBID is the registered validation MySQL the purge fixtures bind their generation requests to.
const purgeValidationDBID = "0b6d6f3e-2c4a-4f8e-9a1b-7c5d3e2f1a90"

// seedPurgeTarget registers the validation MySQL row and the admin password secret it names, so generation requests
// can name a registered Target. The secret ciphertext is a placeholder that purging never decrypts.
func seedPurgeTarget(t *testing.T, ctx context.Context, database *sql.DB) {
	t.Helper()
	if _, err := database.ExecContext(ctx,
		"INSERT INTO secret (name, key_id, nonce, ciphertext) VALUES ('purge-target-admin-password', 'k1', ?, ?)",
		make([]byte, 12), []byte("placeholder"),
	); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO validation_mysql (
		id, name, host, port, admin_username, admin_password_secret_name, tls_mode, config_sha256, idempotency_key
	) VALUES (?, 'purge-target', 'purge-target.internal', 3306, 'atp_admin', 'purge-target-admin-password', 'DISABLED_LOCAL_ONLY', REPEAT('a', 64), 'purge-target')`,
		purgeValidationDBID,
	); err != nil {
		t.Fatalf("seed target: %v", err)
	}
}

func createPurgeJob(t *testing.T, ctx context.Context, store *mysqlstore.GenerationStore, manualReview bool) string {
	t.Helper()
	request, err := json.Marshal(map[string]any{
		"description": "Generate cancellation tests.", "require_manual_review": manualReview,
		"validation_db_ids": []string{purgeValidationDBID},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, digest, err := generation.CanonicalJSON(request)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateGenerationJob(ctx, generation.CreateJobInput{
		IdempotencyKey: "purge-" + ulid.Make().String(),
		Request:        request, RequestSHA256: digest, DescriptionTokens: 8,
		RequestedTestCaseCount: 2, GeneratorConfigSHA256: strings.Repeat("a", 64), GenerateDataset: true,
		RequireManualReview: manualReview,
		ValidationDBIDs:     []string{purgeValidationDBID},
	})
	if err != nil {
		t.Fatalf("create generation job: %v", err)
	}
	return created.ID
}

func setPurgeJobStatus(t *testing.T, ctx context.Context, database *sql.DB, jobID, status string) {
	t.Helper()
	if _, err := database.ExecContext(ctx, "UPDATE generation_job SET status = ?, current_stage = ? WHERE id = ?", status, status, jobID); err != nil {
		t.Fatalf("set job status %s: %v", status, err)
	}
}

// insertPurgeDraft stores one artifact draft that expires hoursFromNow hours from now.
func insertPurgeDraft(t *testing.T, ctx context.Context, database *sql.DB, jobID, key string, hoursFromNow int) {
	t.Helper()
	if _, err := database.ExecContext(ctx, `INSERT INTO generation_artifact_draft (
id, generation_job_id, artifact_kind, artifact_key, content_json, content_sha256, expires_at
) VALUES (?, ?, 'SCENARIO', ?, '{}', ?, TIMESTAMPADD(HOUR, ?, UTC_TIMESTAMP(6)))`,
		ulid.Make().String(), jobID, key, strings.Repeat("c", 64), hoursFromNow); err != nil {
		t.Fatalf("insert draft %s: %v", key, err)
	}
}

func countPurgeDrafts(t *testing.T, ctx context.Context, database *sql.DB, key string) int {
	t.Helper()
	var count int
	if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM generation_artifact_draft WHERE artifact_key = ?", key).Scan(&count); err != nil {
		t.Fatalf("count drafts %s: %v", key, err)
	}
	return count
}
