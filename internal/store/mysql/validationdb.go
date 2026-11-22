package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	validationdb "agent-test-platform/internal/validationdb"

	"github.com/go-sql-driver/mysql"
)

const (
	insertTargetSQL = `INSERT INTO validation_mysql (id, name, host, port, admin_username, admin_password_secret_name,
tls_mode, ca_certificate_pem, config_sha256, idempotency_key) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	targetColumnsSQL     = `id, name, host, port, admin_username, admin_password_secret_name, tls_mode, ca_certificate_pem, created_at`
	selectTargetByIDSQL  = `SELECT ` + targetColumnsSQL + ` FROM validation_mysql WHERE id = ?`
	selectTargetByKeySQL = `SELECT ` + targetColumnsSQL + `, config_sha256, idempotency_key FROM validation_mysql WHERE idempotency_key = ?`
	selectTargetFirstSQL = `SELECT ` + targetColumnsSQL + ` FROM validation_mysql ORDER BY created_at DESC, id DESC LIMIT ?`
	selectTargetAfterSQL = `SELECT ` + targetColumnsSQL + ` FROM validation_mysql
WHERE (created_at < ? OR (created_at = ? AND id < ?)) ORDER BY created_at DESC, id DESC LIMIT ?`
)

// TargetStore persists validation MySQL registrations in Platform MySQL.
type TargetStore struct {
	database *sql.DB
}

// NewTargetStore returns a store over the Platform MySQL database.
func NewTargetStore(database *sql.DB) (*TargetStore, error) {
	if database == nil {
		return nil, errors.New("Platform MySQL database is required")
	}
	return &TargetStore{database: database}, nil
}

// Insert stores a registration and returns the row as stored. A unique violation (name, endpoint, or
// Idempotency-Key) is reported as target.ErrDuplicate.
func (store *TargetStore) Insert(ctx context.Context, record validationdb.Record) (validationdb.Target, error) {
	target := record.Target
	_, err := store.database.ExecContext(ctx, insertTargetSQL,
		target.ID, target.Name, target.Host, target.Port, target.AdminUsername, target.AdminPasswordSecretName,
		target.TLSMode, nullableText(target.CACertificatePEM), record.ConfigSHA256, record.IdempotencyKey,
	)
	if err != nil {
		var mysqlErr *mysql.MySQLError
		if errors.As(err, &mysqlErr) && mysqlErr.Number == mysqlDuplicateKey {
			return validationdb.Target{}, validationdb.ErrDuplicate
		}
		return validationdb.Target{}, fmt.Errorf("insert Target: %w", err)
	}
	stored, err := scanTarget(store.database.QueryRowContext(ctx, selectTargetByIDSQL, target.ID))
	if err != nil {
		return validationdb.Target{}, fmt.Errorf("load inserted Target: %w", err)
	}
	return stored, nil
}

// FindByID returns the registered Target with id, or target.ErrNotFound.
func (store *TargetStore) FindByID(ctx context.Context, id string) (validationdb.Target, error) {
	target, err := scanTarget(store.database.QueryRowContext(ctx, selectTargetByIDSQL, id))
	if errors.Is(err, sql.ErrNoRows) {
		return validationdb.Target{}, validationdb.ErrNotFound
	}
	if err != nil {
		return validationdb.Target{}, fmt.Errorf("load Target: %w", err)
	}
	return target, nil
}

// FindByIdempotencyKey returns the registration stored with key, or target.ErrNotFound.
func (store *TargetStore) FindByIdempotencyKey(ctx context.Context, key string) (validationdb.Record, error) {
	var (
		record    validationdb.Record
		target    validationdb.Target
		caPEM     sql.NullString
		storedKey string
	)
	err := store.database.QueryRowContext(ctx, selectTargetByKeySQL, key).Scan(
		&target.ID, &target.Name, &target.Host, &target.Port, &target.AdminUsername, &target.AdminPasswordSecretName,
		&target.TLSMode, &caPEM, &target.CreatedAt, &record.ConfigSHA256, &storedKey,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return validationdb.Record{}, validationdb.ErrNotFound
	}
	if err != nil {
		return validationdb.Record{}, fmt.Errorf("load Target by idempotency key: %w", err)
	}
	target.CACertificatePEM = caPEM.String
	record.Target = target
	record.IdempotencyKey = storedKey
	return record, nil
}

// ListPage returns up to limit Targets, newest first. A non-nil after resumes after that position.
func (store *TargetStore) ListPage(ctx context.Context, after *validationdb.Cursor, limit int) ([]validationdb.Target, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if after == nil {
		rows, err = store.database.QueryContext(ctx, selectTargetFirstSQL, limit)
	} else {
		rows, err = store.database.QueryContext(ctx, selectTargetAfterSQL, after.CreatedAt, after.CreatedAt, after.ID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("query Targets: %w", err)
	}
	defer rows.Close()
	items := make([]validationdb.Target, 0, limit)
	for rows.Next() {
		target, err := scanTarget(rows)
		if err != nil {
			return nil, fmt.Errorf("scan Target: %w", err)
		}
		items = append(items, target)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read Targets: %w", err)
	}
	return items, nil
}

type targetRowScanner interface {
	Scan(dest ...any) error
}

func scanTarget(row targetRowScanner) (validationdb.Target, error) {
	var (
		target validationdb.Target
		caPEM  sql.NullString
	)
	if err := row.Scan(&target.ID, &target.Name, &target.Host, &target.Port, &target.AdminUsername,
		&target.AdminPasswordSecretName, &target.TLSMode, &caPEM, &target.CreatedAt); err != nil {
		return validationdb.Target{}, err
	}
	target.CACertificatePEM = caPEM.String
	return target, nil
}

func nullableText(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}
