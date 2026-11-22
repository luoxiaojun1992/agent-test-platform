package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"agent-test-platform/internal/generation"

	"github.com/oklog/ulid/v2"
)

const selectDatabaseProfileConnectionSQL = `SELECT id, config_sha256, host, port, database_name, username,
password_secret_ref, tls_mode, ca_certificate_pem, snapshot_mode, snapshot_timeout_seconds
FROM database_profile WHERE id = ?`

// LoadDatabaseProfileConnection reads the connection settings a Dataset import needs. It returns the
// Secret Reference, never a secret value.
func (store *GenerationStore) LoadDatabaseProfileConnection(ctx context.Context, id string) (generation.DatabaseProfileConnection, error) {
	if _, err := ulid.ParseStrict(id); err != nil {
		return generation.DatabaseProfileConnection{}, generation.ErrNotFound
	}
	var profile generation.DatabaseProfileConnection
	var caPEM sql.NullString
	err := store.database.QueryRowContext(ctx, selectDatabaseProfileConnectionSQL, id).Scan(
		&profile.ID, &profile.ConfigSHA256, &profile.Host, &profile.Port, &profile.Database, &profile.Username,
		&profile.PasswordSecretRef, &profile.TLSMode, &caPEM, &profile.SnapshotMode, &profile.SnapshotTimeoutSeconds,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return generation.DatabaseProfileConnection{}, generation.ErrNotFound
	}
	if err != nil {
		return generation.DatabaseProfileConnection{}, fmt.Errorf("load database profile connection: %w", err)
	}
	profile.CACertificatePEM = caPEM.String
	return profile, nil
}
