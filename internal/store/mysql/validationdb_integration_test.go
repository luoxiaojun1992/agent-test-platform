package mysqlstore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"agent-test-platform/internal/secrets"
	validationdb "agent-test-platform/internal/validationdb"
	"agent-test-platform/migrations"

	"github.com/go-sql-driver/mysql"
)

// TestTargetRegistryOnMySQL84 registers validation MySQL instances against a real MySQL 8.4 instance: the
// admin password comes from the secret table, replays are idempotent, name and endpoint are unique, the
// CA certificate is stored, and listing pages newest first.
func TestTargetRegistryOnMySQL84(t *testing.T) {
	dsn := os.Getenv("ATP_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set ATP_TEST_MYSQL_DSN to run the MySQL 8.4 Target registry integration test")
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

	databaseName := "atp_target_" + randomPublishHex(t, 8)
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

	keyring, err := secrets.NewKeyring("k1", map[string][]byte{"k1": bytes.Repeat([]byte{6}, secrets.KeyBytes)})
	if err != nil {
		t.Fatal(err)
	}
	secretStore, err := NewSecretStore(database)
	if err != nil {
		t.Fatal(err)
	}
	secretService, err := secrets.NewService(secretStore, keyring)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := secretService.Put(ctx, "target-a-admin-password", "admin-secret-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := secretService.Put(ctx, "target-b-admin-password", "admin-secret-b"); err != nil {
		t.Fatal(err)
	}

	targetStore, err := NewTargetStore(database)
	if err != nil {
		t.Fatal(err)
	}
	service, err := validationdb.NewService(targetStore, secretService, validationdb.Endpoint{Host: "platform-mysql", Port: 3306})
	if err != nil {
		t.Fatal(err)
	}

	input := validationdb.Input{
		Name: "target-a", Host: "target-a.internal", Port: 3306, AdminUsername: "atp_admin",
		AdminPasswordSecretName: "target-a-admin-password", TLSMode: validationdb.TLSDisabledLocalOnly,
	}
	created, err := service.Register(ctx, "register-target-a", input)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.CreatedAt.IsZero() || created.CACertificatePEM != "" {
		t.Fatalf("created = %#v", created)
	}
	replayed, err := service.Register(ctx, "register-target-a", input)
	if err != nil || replayed.ID != created.ID {
		t.Fatalf("replay = %#v, %v", replayed, err)
	}
	changed := input
	changed.Port = 3307
	if _, err := service.Register(ctx, "register-target-a", changed); !errors.Is(err, validationdb.ErrIdempotencyConflict) {
		t.Fatalf("same key, changed request = %v", err)
	}

	duplicate := input
	duplicate.Host = "other.internal"
	if _, err := service.Register(ctx, "register-target-dup", duplicate); !errors.Is(err, validationdb.ErrConflict) {
		t.Fatalf("duplicate name = %v", err)
	}

	second := validationdb.Input{
		Name: "target-b", Host: "target-b.internal", Port: 3306, AdminUsername: "atp_admin",
		AdminPasswordSecretName: "target-b-admin-password", TLSMode: validationdb.TLSVerifyIdentity,
		CACertificatePEM: "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n",
	}
	if _, err := service.Register(ctx, "register-target-b", second); !errors.Is(err, validationdb.ErrCACertificateInvalid) {
		t.Fatalf("invalid CA PEM stored: %v", err)
	}
	second.CACertificatePEM = ""
	if _, err := service.Register(ctx, "register-target-b", second); err != nil {
		t.Fatal(err)
	}

	first, next, err := service.List(ctx, "", 1)
	if err != nil || len(first) != 1 || first[0].Name != "target-b" || next == "" {
		t.Fatalf("first page = %#v next = %q err = %v", first, next, err)
	}
	rest, next, err := service.List(ctx, next, 1)
	if err != nil || len(rest) != 1 || rest[0].Name != "target-a" || next != "" {
		t.Fatalf("second page = %#v next = %q err = %v", rest, next, err)
	}

	var ciphertext []byte
	if err := database.QueryRowContext(ctx, "SELECT ciphertext FROM secret WHERE name = ?", "target-a-admin-password").Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, []byte("admin-secret-a")) {
		t.Fatal("admin password stored in plaintext")
	}
	var conflictErr *mysql.MySQLError
	if _, err := database.ExecContext(ctx, "INSERT INTO validation_mysql (id, name, host, port, admin_username, admin_password_secret_name, tls_mode, config_sha256, idempotency_key) VALUES ('3f9a1c2e-5b7d-4e8f-9a0b-1c2d3e4f5a6b', 'x-target', 'x.internal', 3306, 'u', 'missing-secret', 'DISABLED_LOCAL_ONLY', REPEAT('a', 64), 'raw-key')"); !errors.As(err, &conflictErr) {
		t.Fatalf("foreign key to a missing secret was not enforced: %v", err)
	}
}

// seededValidationDBID is the validation MySQL that the generation integration tests register, so their requests
// and Dataset bindings name a registered Target.
const seededValidationDBID = "0b6d6f3e-2c4a-4f8e-9a1b-7c5d3e2f1a90"

// seedValidationDB registers a validation MySQL row and the admin password secret it names. The secret ciphertext
// is a placeholder: request registration, Dataset binding, and publish never decrypt it.
func seedValidationDB(t *testing.T, ctx context.Context, database *sql.DB, id, name, host string) {
	t.Helper()
	if _, err := database.ExecContext(ctx,
		"INSERT INTO secret (name, key_id, nonce, ciphertext) VALUES (?, 'k1', ?, ?)",
		name+"-admin-password", make([]byte, 12), []byte("placeholder"),
	); err != nil {
		t.Fatalf("seed secret for %s: %v", name, err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO validation_mysql (
		id, name, host, port, admin_username, admin_password_secret_name, tls_mode, config_sha256, idempotency_key
	) VALUES (?, ?, ?, 3306, 'atp_admin', ?, 'DISABLED_LOCAL_ONLY', REPEAT('a', 64), ?)`,
		id, name, host, name+"-admin-password", "seed-"+name,
	); err != nil {
		t.Fatalf("seed target %s: %v", name, err)
	}
}
