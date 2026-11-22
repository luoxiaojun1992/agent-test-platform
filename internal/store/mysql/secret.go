package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"agent-test-platform/internal/secrets"
)

const (
	upsertSecretSQL = `INSERT INTO secret (name, key_id, nonce, ciphertext)
VALUES (?, ?, ?, ?) AS new
ON DUPLICATE KEY UPDATE key_id = new.key_id, nonce = new.nonce, ciphertext = new.ciphertext, updated_at = CURRENT_TIMESTAMP(6)`
	selectSecretMetadataSQL = `SELECT name, created_at, updated_at FROM secret WHERE name = ?`
	selectSecretRecordSQL   = `SELECT name, key_id, nonce, ciphertext, created_at, updated_at FROM secret WHERE name = ?`
	selectSecretPageSQL     = `SELECT name, created_at, updated_at FROM secret WHERE name > ? ORDER BY name LIMIT ?`
	selectNotUnderKeySQL    = `SELECT name, key_id, nonce, ciphertext, created_at, updated_at FROM secret
WHERE key_id <> ? ORDER BY name LIMIT ?`
	// updated_at = updated_at keeps the timestamp: a rewrap changes the sealed form, not the value.
	replaceSecretSQL = `UPDATE secret SET key_id = ?, nonce = ?, ciphertext = ?, updated_at = updated_at
WHERE name = ? AND key_id = ? AND nonce = ? AND ciphertext = ?`
	countSecretsByKeySQL = `SELECT key_id, COUNT(*) FROM secret GROUP BY key_id`
)

// SecretStore persists sealed secrets in Platform MySQL.
type SecretStore struct {
	database *sql.DB
}

// NewSecretStore returns a store over the Platform MySQL database.
func NewSecretStore(database *sql.DB) (*SecretStore, error) {
	if database == nil {
		return nil, errors.New("Platform MySQL database is required")
	}
	return &SecretStore{database: database}, nil
}

// Put inserts or replaces the sealed value stored under name. created_at is kept when the name already
// exists, and the returned metadata is read back from the row.
func (store *SecretStore) Put(ctx context.Context, name, keyID string, nonce, ciphertext []byte) (secrets.Metadata, error) {
	if _, err := store.database.ExecContext(ctx, upsertSecretSQL, name, keyID, nonce, ciphertext); err != nil {
		return secrets.Metadata{}, fmt.Errorf("upsert secret: %w", err)
	}
	var metadata secrets.Metadata
	if err := store.database.QueryRowContext(ctx, selectSecretMetadataSQL, name).Scan(
		&metadata.Name, &metadata.CreatedAt, &metadata.UpdatedAt,
	); err != nil {
		return secrets.Metadata{}, fmt.Errorf("load secret metadata: %w", err)
	}
	return metadata, nil
}

// Load returns the sealed record stored under name, or secrets.ErrNotFound.
func (store *SecretStore) Load(ctx context.Context, name string) (secrets.Record, error) {
	record, err := scanSecretRecord(store.database.QueryRowContext(ctx, selectSecretRecordSQL, name))
	if errors.Is(err, sql.ErrNoRows) {
		return secrets.Record{}, secrets.ErrNotFound
	}
	if err != nil {
		return secrets.Record{}, fmt.Errorf("load secret: %w", err)
	}
	return record, nil
}

// List returns up to limit metadata rows whose names sort after after, in name order. It never reads the
// sealed value.
func (store *SecretStore) List(ctx context.Context, after string, limit int) ([]secrets.Metadata, error) {
	rows, err := store.database.QueryContext(ctx, selectSecretPageSQL, after, limit)
	if err != nil {
		return nil, fmt.Errorf("query secrets: %w", err)
	}
	defer rows.Close()
	items := make([]secrets.Metadata, 0, limit)
	for rows.Next() {
		var item secrets.Metadata
		if err := rows.Scan(&item.Name, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan secret metadata: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read secret metadata: %w", err)
	}
	return items, nil
}

// ListNotUnder returns up to limit sealed records whose key id differs from keyID.
func (store *SecretStore) ListNotUnder(ctx context.Context, keyID string, limit int) ([]secrets.Record, error) {
	rows, err := store.database.QueryContext(ctx, selectNotUnderKeySQL, keyID, limit)
	if err != nil {
		return nil, fmt.Errorf("query secrets under other keys: %w", err)
	}
	defer rows.Close()
	records := make([]secrets.Record, 0, limit)
	for rows.Next() {
		record, err := scanSecretRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("scan sealed secret: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read sealed secrets: %w", err)
	}
	return records, nil
}

// Replace swaps the sealed value of stale for the given one only if the row still holds exactly the values
// stale was read with. It reports false when a concurrent write changed the row.
func (store *SecretStore) Replace(ctx context.Context, stale secrets.Record, keyID string, nonce, ciphertext []byte) (bool, error) {
	result, err := store.database.ExecContext(ctx, replaceSecretSQL,
		keyID, nonce, ciphertext, stale.Name, stale.KeyID, stale.Nonce, stale.Ciphertext)
	if err != nil {
		return false, fmt.Errorf("replace sealed secret: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read replace result: %w", err)
	}
	return affected == 1, nil
}

// CountByKey returns how many stored secrets are sealed under each key id.
func (store *SecretStore) CountByKey(ctx context.Context) (map[string]int, error) {
	rows, err := store.database.QueryContext(ctx, countSecretsByKeySQL)
	if err != nil {
		return nil, fmt.Errorf("count secrets by key: %w", err)
	}
	defer rows.Close()
	counts := make(map[string]int)
	for rows.Next() {
		var keyID string
		var count int
		if err := rows.Scan(&keyID, &count); err != nil {
			return nil, fmt.Errorf("scan key count: %w", err)
		}
		counts[keyID] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read key counts: %w", err)
	}
	return counts, nil
}

func scanSecretRecord(row rowScanner) (secrets.Record, error) {
	var record secrets.Record
	err := row.Scan(&record.Name, &record.KeyID, &record.Nonce, &record.Ciphertext, &record.CreatedAt, &record.UpdatedAt)
	return record, err
}
