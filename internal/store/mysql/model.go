package mysqlstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/oklog/ulid/v2"

	"agent-test-platform/internal/modelcfg"
)

// ModelStore persists registered models in model_config.
type ModelStore struct{ database *sql.DB }

func NewModelStore(database *sql.DB) (*ModelStore, error) {
	if database == nil {
		return nil, errors.New("Platform MySQL database is required")
	}
	return &ModelStore{database: database}, nil
}

// Register records a model alias for a role when absent. The first model of a role becomes active.
func (store *ModelStore) Register(ctx context.Context, role, alias, provider, model, baseURL string) error {
	config, _ := json.Marshal(map[string]string{"role": role, "alias": alias, "provider": provider, "model": model, "base_url": baseURL})
	sum := sha256.Sum256(config)
	digest := hex.EncodeToString(sum[:])
	tx, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT id FROM model_config WHERE config_sha256 = ?`, digest).Scan(&existing)
	if err == nil {
		return activateIfNone(ctx, tx, existing, role)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	id := ulid.Make().String()
	if _, err := tx.ExecContext(ctx, `INSERT INTO model_config (id, alias_name, revision_no, model_role, provider, model_name, base_url, capabilities_json, config_json, config_sha256, enabled)
		SELECT ?, ?, COALESCE(MAX(revision_no), 0) + 1, ?, ?, ?, ?, JSON_OBJECT(), ?, ?, FALSE FROM model_config WHERE alias_name = ?`,
		id, alias, role, provider, model, baseURL, string(config), digest, alias); err != nil {
		if isDuplicate(err) {
			return nil
		}
		return fmt.Errorf("register model: %w", err)
	}
	return activateIfNone(ctx, tx, id, role)
}

func activateIfNone(ctx context.Context, tx *sql.Tx, id, role string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE model_config SET enabled = TRUE WHERE id = ? AND NOT EXISTS (SELECT 1 FROM (SELECT id FROM model_config WHERE model_role = ? AND enabled) x)`, id, role); err != nil {
		return fmt.Errorf("activate first model: %w", err)
	}
	return tx.Commit()
}

func (store *ModelStore) List(ctx context.Context) ([]modelcfg.Model, error) {
	rows, err := store.database.QueryContext(ctx, `SELECT m.alias_name, m.model_role, m.provider, m.model_name, m.enabled, m.created_at FROM model_config m
		WHERE m.revision_no = (SELECT MAX(x.revision_no) FROM model_config x WHERE x.alias_name = m.alias_name AND x.model_role = m.model_role)
		ORDER BY m.model_role, m.alias_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []modelcfg.Model{}
	for rows.Next() {
		var item modelcfg.Model
		if err := rows.Scan(&item.Alias, &item.Role, &item.Provider, &item.Model, &item.Active, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (store *ModelStore) Activate(ctx context.Context, role, alias string) error {
	tx, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM model_config WHERE model_role = ? AND alias_name = ? ORDER BY revision_no DESC LIMIT 1 FOR UPDATE`, role, alias).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return modelcfg.ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE model_config SET enabled = FALSE WHERE model_role = ? AND enabled`, role); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE model_config SET enabled = TRUE WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// JudgeAliasExists reports whether alias is a registered Judge model.
func (store *RunStore) JudgeAliasExists(ctx context.Context, alias string) (bool, error) {
	var one int
	err := store.database.QueryRowContext(ctx, `SELECT 1 FROM model_config WHERE model_role = 'judge' AND alias_name = ? LIMIT 1`, alias).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// ActiveAlias returns the alias of the active model of a role, or modelcfg.ErrNotFound when none is selected.
func (store *ModelStore) ActiveAlias(ctx context.Context, role string) (string, error) {
	var alias string
	err := store.database.QueryRowContext(ctx, `SELECT alias_name FROM model_config WHERE model_role = ? AND enabled ORDER BY revision_no DESC LIMIT 1`, role).Scan(&alias)
	if errors.Is(err, sql.ErrNoRows) {
		return "", modelcfg.ErrNotFound
	}
	return alias, err
}
