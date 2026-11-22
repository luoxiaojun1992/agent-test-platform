package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"agent-test-platform/internal/generation"

	"github.com/go-sql-driver/mysql"
	"github.com/oklog/ulid/v2"
)

const (
	insertDatabaseProfileSQL = `INSERT INTO database_profile (
id, name, engine, host, port, database_name, username, password_secret_ref, tls_mode, ca_certificate_pem,
snapshot_mode, snapshot_timeout_seconds, config_json, config_sha256, idempotency_key
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	selectDatabaseProfileByIdempotencySQL = `SELECT config_sha256 FROM database_profile WHERE idempotency_key = ?`
	selectDatabaseProfileByDigestSQL      = `SELECT id FROM database_profile WHERE config_sha256 = ?`
	selectDatabaseProfileByIDSQL          = `SELECT ` + databaseProfileColumns + ` FROM database_profile WHERE id = ?`
	selectDatabaseProfilePageSQL          = `SELECT ` + databaseProfileColumns + ` FROM database_profile
WHERE (created_at < ? OR (created_at = ? AND id < ?)) ORDER BY created_at DESC, id DESC LIMIT ?`
	selectDatabaseProfileFirstPageSQL = `SELECT ` + databaseProfileColumns + ` FROM database_profile ORDER BY created_at DESC, id DESC LIMIT ?`
)

// databaseProfileColumns lists the columns scanDatabaseProfile reads, in order.
const databaseProfileColumns = `id, name, engine, host, port, database_name, username, tls_mode, ca_certificate_pem,
snapshot_mode, snapshot_timeout_seconds, created_at`

// mysqlDuplicateKey is MySQL error 1062 (ER_DUP_ENTRY).
const mysqlDuplicateKey = 1062

// CreateDatabaseProfile stores a profile. Replaying the same Idempotency-Key with the same
// configuration returns the stored profile; the same key with a different configuration is an
// idempotency conflict; a configuration or name that already exists is a conflict.
func (store *GenerationStore) CreateDatabaseProfile(ctx context.Context, input generation.CreateDatabaseProfileInput) (generation.DatabaseProfile, error) {
	id, err := store.newID()
	if err != nil {
		return generation.DatabaseProfile{}, fmt.Errorf("generate database profile ID: %w", err)
	}
	if _, err := ulid.ParseStrict(id); err != nil {
		return generation.DatabaseProfile{}, fmt.Errorf("generate database profile ID: %w", err)
	}
	request := input.Request
	_, err = store.database.ExecContext(ctx, insertDatabaseProfileSQL,
		id, request.Name, request.Engine, request.Host, request.Port, request.Database, request.Username,
		request.PasswordSecretRef, request.TLSMode, nullableText(request.CACertificatePEM), request.SnapshotMode, request.SnapshotTimeoutSeconds,
		[]byte(input.ConfigJSON), input.ConfigSHA256, input.IdempotencyKey,
	)
	if err == nil {
		return store.loadDatabaseProfile(ctx, id)
	}
	var mysqlErr *mysql.MySQLError
	if !errors.As(err, &mysqlErr) || mysqlErr.Number != mysqlDuplicateKey {
		return generation.DatabaseProfile{}, fmt.Errorf("insert database profile: %w", err)
	}
	return store.resolveDatabaseProfileConflict(ctx, input)
}

// resolveDatabaseProfileConflict decides what a duplicate-key insert means: a replay of the
// same Idempotency-Key (same configuration) returns the stored profile, anything else conflicts.
func (store *GenerationStore) resolveDatabaseProfileConflict(ctx context.Context, input generation.CreateDatabaseProfileInput) (generation.DatabaseProfile, error) {
	var storedDigest string
	err := store.database.QueryRowContext(ctx, selectDatabaseProfileByIdempotencySQL, input.IdempotencyKey).Scan(&storedDigest)
	if err == nil {
		if storedDigest != input.ConfigSHA256 {
			return generation.DatabaseProfile{}, generation.ErrIdempotencyConflict
		}
		var id string
		if err := store.database.QueryRowContext(ctx, selectDatabaseProfileByDigestSQL, input.ConfigSHA256).Scan(&id); err != nil {
			return generation.DatabaseProfile{}, fmt.Errorf("load replayed database profile: %w", err)
		}
		return store.loadDatabaseProfile(ctx, id)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return generation.DatabaseProfile{}, fmt.Errorf("check database profile idempotency: %w", err)
	}
	return generation.DatabaseProfile{}, fmt.Errorf("%w: a database profile with this name or configuration already exists", generation.ErrConflict)
}

func (store *GenerationStore) loadDatabaseProfile(ctx context.Context, id string) (generation.DatabaseProfile, error) {
	profile, err := scanDatabaseProfile(store.database.QueryRowContext(ctx, selectDatabaseProfileByIDSQL, id))
	if err != nil {
		return generation.DatabaseProfile{}, fmt.Errorf("load database profile: %w", err)
	}
	return profile, nil
}

// ListDatabaseProfiles returns one page, most recently created first. The cursor is an opaque
// encoding of the last returned profile's (created_at, id); an empty next cursor means the last
// page.
func (store *GenerationStore) ListDatabaseProfiles(ctx context.Context, cursor string, limit int) ([]generation.DatabaseProfile, string, error) {
	if limit < 1 || limit > 200 {
		return nil, "", fmt.Errorf("list limit must be between 1 and 200")
	}
	var rows *sql.Rows
	var err error
	if cursor == "" {
		rows, err = store.database.QueryContext(ctx, selectDatabaseProfileFirstPageSQL, limit+1)
	} else {
		createdAt, id, decodeErr := decodeProfileCursor(cursor)
		if decodeErr != nil {
			return nil, "", generation.ErrInvalidCursor
		}
		rows, err = store.database.QueryContext(ctx, selectDatabaseProfilePageSQL, createdAt, createdAt, id, limit+1)
	}
	if err != nil {
		return nil, "", fmt.Errorf("list database profiles: %w", err)
	}
	defer rows.Close()

	var profiles []generation.DatabaseProfile
	for rows.Next() {
		profile, err := scanDatabaseProfile(rows)
		if err != nil {
			return nil, "", fmt.Errorf("scan database profile: %w", err)
		}
		profiles = append(profiles, profile)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("read database profiles: %w", err)
	}
	if len(profiles) <= limit {
		return profiles, "", nil
	}
	profiles = profiles[:limit]
	last := profiles[len(profiles)-1]
	return profiles, encodeProfileCursor(last.CreatedAt, last.ID), nil
}

func encodeProfileCursor(createdAt time.Time, id string) string {
	raw := createdAt.UTC().Format("2006-01-02T15:04:05.000000Z") + "|" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeProfileCursor(cursor string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", err
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, "", fmt.Errorf("cursor has no separator")
	}
	createdAt, err := time.Parse("2006-01-02T15:04:05.000000Z", parts[0])
	if err != nil {
		return time.Time{}, "", err
	}
	if _, err := ulid.ParseStrict(parts[1]); err != nil {
		return time.Time{}, "", err
	}
	return createdAt.UTC(), parts[1], nil
}

func scanDatabaseProfile(scanner rowScanner) (generation.DatabaseProfile, error) {
	var profile generation.DatabaseProfile
	var caPEM sql.NullString
	if err := scanner.Scan(
		&profile.ID, &profile.Name, &profile.Engine, &profile.Host, &profile.Port, &profile.Database, &profile.Username,
		&profile.TLSMode, &caPEM, &profile.SnapshotMode, &profile.SnapshotTimeoutSeconds, &profile.CreatedAt,
	); err != nil {
		return generation.DatabaseProfile{}, err
	}
	profile.CACertificatePEM = caPEM.String
	profile.CreatedAt = profile.CreatedAt.UTC()
	return profile, nil
}
