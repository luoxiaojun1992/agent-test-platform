package mysqlstore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"agent-test-platform/internal/secrets"
	"agent-test-platform/migrations"

	"github.com/go-sql-driver/mysql"
)

// TestSecretLifecycleAndRotationOnMySQL84 stores, replaces, lists, resolves, and rotates encrypted
// credentials against a real MySQL 8.4 instance. It checks that stored rows never hold plaintext and
// that a rewrap moves every row to the current key id.
func TestSecretLifecycleAndRotationOnMySQL84(t *testing.T) {
	dsn := os.Getenv("ATP_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set ATP_TEST_MYSQL_DSN to run the MySQL 8.4 secret integration test")
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

	databaseName := "atp_secret_" + randomPublishHex(t, 8)
	if _, err := adminDB.ExecContext(ctx, "CREATE DATABASE `"+databaseName+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	databaseConfig := *adminConfig
	databaseConfig.DBName = databaseName
	databaseConfig.ParseTime = true
	databaseConfig.Loc = time.UTC
	database := openPublishMySQL(t, &databaseConfig)
	defer database.Close()
	if err := migrations.Apply(ctx, database); err != nil {
		t.Fatal(err)
	}

	store, err := NewSecretStore(database)
	if err != nil {
		t.Fatal(err)
	}
	oldRing, err := secrets.NewKeyring("k1", map[string][]byte{"k1": bytes.Repeat([]byte{3}, secrets.KeyBytes)})
	if err != nil {
		t.Fatal(err)
	}
	service, err := secrets.NewService(store, oldRing)
	if err != nil {
		t.Fatal(err)
	}

	const plaintext = "s3cret-value-for-mysql"
	created, err := service.Put(ctx, "sales-reader-password", plaintext)
	if err != nil {
		t.Fatal(err)
	}
	var ciphertext []byte
	var keyID string
	if err := database.QueryRowContext(ctx, "SELECT ciphertext, key_id FROM secret WHERE name = ?", "sales-reader-password").Scan(&ciphertext, &keyID); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, []byte(plaintext)) || keyID != "k1" {
		t.Fatalf("stored row: plaintext present = %v, key_id = %q", bytes.Contains(ciphertext, []byte(plaintext)), keyID)
	}
	if value, err := service.Resolve(ctx, "sales-reader-password"); err != nil || value != plaintext {
		t.Fatalf("Resolve = %q, %v", value, err)
	}

	replaced, err := service.Put(ctx, "sales-reader-password", "rotated-value")
	if err != nil {
		t.Fatal(err)
	}
	if !replaced.CreatedAt.Equal(created.CreatedAt) || replaced.UpdatedAt.Before(created.UpdatedAt) {
		t.Fatalf("replace metadata = %#v, created = %#v", replaced, created)
	}
	for _, name := range []string{"atp-b", "atp-a", "atp-c"} {
		if _, err := service.Put(ctx, name, "value-"+name); err != nil {
			t.Fatal(err)
		}
	}
	first, next, err := service.List(ctx, "", 2)
	if err != nil || len(first) != 2 || first[0].Name != "atp-a" || first[1].Name != "atp-b" || next == "" {
		t.Fatalf("first page = %#v next = %q err = %v", first, next, err)
	}
	second, next, err := service.List(ctx, next, 2)
	if err != nil || len(second) != 2 || second[0].Name != "atp-c" || second[1].Name != "sales-reader-password" || next != "" {
		t.Fatalf("second page = %#v next = %q err = %v", second, next, err)
	}

	newRing, err := secrets.NewKeyring("k2", map[string][]byte{
		"k1": bytes.Repeat([]byte{3}, secrets.KeyBytes),
		"k2": bytes.Repeat([]byte{4}, secrets.KeyBytes),
	})
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := secrets.NewService(store, newRing)
	if err != nil {
		t.Fatal(err)
	}
	report, err := rotated.Rewrap(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if report.Rewrapped != 4 || report.RemainingByKey["k1"] != 0 || report.RemainingByKey["k2"] != 4 {
		t.Fatalf("rewrap report = %#v", report)
	}
	if value, err := rotated.Resolve(ctx, "sales-reader-password"); err != nil || value != "rotated-value" {
		t.Fatalf("Resolve after rewrap = %q, %v", value, err)
	}
	if _, err := service.Resolve(ctx, "sales-reader-password"); !errors.Is(err, secrets.ErrKeyUnavailable) {
		t.Fatalf("rewrapped value still opens under the old keyring alone: %v", err)
	}

	if _, err := rotated.Resolve(ctx, "missing-secret"); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("Resolve(missing) = %v", err)
	}
	if _, err := rotated.Put(ctx, "Bad_Name", "value"); !errors.Is(err, secrets.ErrInvalidName) {
		t.Fatalf("Put(invalid name) = %v", err)
	}
}
