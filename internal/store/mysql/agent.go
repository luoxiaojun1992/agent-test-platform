package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/go-sql-driver/mysql"

	"agent-test-platform/internal/agentregistry"
)

const (
	agentColumnsSQL   = `id, name, description, database_profile_id, created_at`
	versionColumnsSQL = `id, agent_id, version, chat_endpoint, target_model, contract_version, tool_evaluation_mode,
tool_catalog_json, created_at`
)

// AgentStore persists Agents and Agent Versions in Platform MySQL.
type AgentStore struct {
	database *sql.DB
}

// NewAgentStore returns a store over the Platform MySQL database.
func NewAgentStore(database *sql.DB) (*AgentStore, error) {
	if database == nil {
		return nil, errors.New("Platform MySQL database is required")
	}
	return &AgentStore{database: database}, nil
}

func isDuplicate(err error) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == mysqlDuplicateKey
}

func (store *AgentStore) InsertAgent(ctx context.Context, record agentregistry.AgentRecord) (agentregistry.Agent, error) {
	agent := record.Agent
	_, err := store.database.ExecContext(ctx,
		`INSERT INTO agent (id, name, description, database_profile_id, config_sha256, idempotency_key) VALUES (?, ?, ?, ?, ?, ?)`,
		agent.ID, agent.Name, nullableText(agent.Description), nullableProfile(agent.DatabaseProfileID), record.ConfigSHA256, record.IdempotencyKey)
	if isDuplicate(err) {
		return agentregistry.Agent{}, agentregistry.ErrDuplicate
	}
	if err != nil {
		return agentregistry.Agent{}, fmt.Errorf("insert Agent: %w", err)
	}
	return store.GetAgent(ctx, agent.ID)
}

func (store *AgentStore) FindAgentByIdempotencyKey(ctx context.Context, key string) (agentregistry.AgentRecord, error) {
	var (
		record  agentregistry.AgentRecord
		agent   agentregistry.Agent
		desc    sql.NullString
		profile sql.NullString
		digest  sql.NullString
	)
	err := store.database.QueryRowContext(ctx,
		`SELECT `+agentColumnsSQL+`, config_sha256 FROM agent WHERE idempotency_key = ?`, key).
		Scan(&agent.ID, &agent.Name, &desc, &profile, &agent.CreatedAt, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return record, agentregistry.ErrNotFound
	}
	if err != nil {
		return record, fmt.Errorf("load Agent by idempotency key: %w", err)
	}
	agent.Description = desc.String
	if profile.Valid {
		agent.DatabaseProfileID = &profile.String
	}
	return agentregistry.AgentRecord{Agent: agent, IdempotencyKey: key, ConfigSHA256: digest.String}, nil
}

func (store *AgentStore) GetAgent(ctx context.Context, id string) (agentregistry.Agent, error) {
	agent, err := scanAgent(store.database.QueryRowContext(ctx, `SELECT `+agentColumnsSQL+` FROM agent WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return agentregistry.Agent{}, agentregistry.ErrNotFound
	}
	if err != nil {
		return agentregistry.Agent{}, fmt.Errorf("load Agent: %w", err)
	}
	return agent, nil
}

func (store *AgentStore) ListAgents(ctx context.Context, after *agentregistry.Cursor, limit int) ([]agentregistry.Agent, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if after == nil {
		rows, err = store.database.QueryContext(ctx, `SELECT `+agentColumnsSQL+` FROM agent ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	} else {
		rows, err = store.database.QueryContext(ctx, `SELECT `+agentColumnsSQL+` FROM agent
WHERE (created_at < ? OR (created_at = ? AND id < ?)) ORDER BY created_at DESC, id DESC LIMIT ?`,
			after.CreatedAt, after.CreatedAt, after.ID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("query Agents: %w", err)
	}
	defer rows.Close()
	items := make([]agentregistry.Agent, 0, limit)
	for rows.Next() {
		agent, err := scanAgent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan Agent: %w", err)
		}
		items = append(items, agent)
	}
	return items, rows.Err()
}

func (store *AgentStore) InsertVersion(ctx context.Context, record agentregistry.VersionRecord) (agentregistry.Version, error) {
	version := record.Version
	var catalog any
	if version.ToolCatalog != nil {
		encoded, err := json.Marshal(version.ToolCatalog)
		if err != nil {
			return agentregistry.Version{}, fmt.Errorf("encode Tool Catalog: %w", err)
		}
		catalog = string(encoded)
	}
	config, err := json.Marshal(struct {
		agentregistry.Version
		TargetBearerTokenRef string `json:"target_bearer_token_ref"`
	}{version, version.TargetBearerTokenRef})
	if err != nil {
		return agentregistry.Version{}, fmt.Errorf("encode Agent Version config: %w", err)
	}
	_, err = store.database.ExecContext(ctx, `INSERT INTO agent_version (id, agent_id, version, chat_endpoint,
target_bearer_token_ref, target_model, contract_version, tool_evaluation_mode, tool_catalog_json,
config_json, config_sha256, idempotency_key) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		version.ID, version.AgentID, version.Version, version.ChatEndpoint, version.TargetBearerTokenRef, version.TargetModel,
		version.ContractVersion, version.ToolEvaluationMode, catalog, string(config), record.ConfigSHA256, record.IdempotencyKey)
	if isDuplicate(err) {
		return agentregistry.Version{}, agentregistry.ErrDuplicate
	}
	if err != nil {
		return agentregistry.Version{}, fmt.Errorf("insert Agent Version: %w", err)
	}
	stored, err := scanVersion(store.database.QueryRowContext(ctx, `SELECT `+versionColumnsSQL+` FROM agent_version WHERE id = ?`, version.ID))
	if err != nil {
		return agentregistry.Version{}, fmt.Errorf("load inserted Agent Version: %w", err)
	}
	return stored, nil
}

func (store *AgentStore) FindVersionByIdempotencyKey(ctx context.Context, key string) (agentregistry.VersionRecord, error) {
	row := store.database.QueryRowContext(ctx, `SELECT `+versionColumnsSQL+`, config_sha256 FROM agent_version WHERE idempotency_key = ?`, key)
	var digest string
	version, err := scanVersion(row, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return agentregistry.VersionRecord{}, agentregistry.ErrNotFound
	}
	if err != nil {
		return agentregistry.VersionRecord{}, fmt.Errorf("load Agent Version by idempotency key: %w", err)
	}
	return agentregistry.VersionRecord{Version: version, IdempotencyKey: key, ConfigSHA256: digest}, nil
}

func (store *AgentStore) ListVersions(ctx context.Context, agentID string, after *agentregistry.Cursor, limit int) ([]agentregistry.Version, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if after == nil {
		rows, err = store.database.QueryContext(ctx, `SELECT `+versionColumnsSQL+` FROM agent_version WHERE agent_id = ?
ORDER BY created_at DESC, id DESC LIMIT ?`, agentID, limit)
	} else {
		rows, err = store.database.QueryContext(ctx, `SELECT `+versionColumnsSQL+` FROM agent_version WHERE agent_id = ?
AND (created_at < ? OR (created_at = ? AND id < ?)) ORDER BY created_at DESC, id DESC LIMIT ?`,
			agentID, after.CreatedAt, after.CreatedAt, after.ID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("query Agent Versions: %w", err)
	}
	defer rows.Close()
	items := make([]agentregistry.Version, 0, limit)
	for rows.Next() {
		version, err := scanVersion(rows)
		if err != nil {
			return nil, fmt.Errorf("scan Agent Version: %w", err)
		}
		items = append(items, version)
	}
	return items, rows.Err()
}

func (store *AgentStore) DatabaseProfileExists(ctx context.Context, id string) (bool, error) {
	var found int
	err := store.database.QueryRowContext(ctx, `SELECT 1 FROM database_profile WHERE id = ?`, id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func scanAgent(row interface{ Scan(...any) error }) (agentregistry.Agent, error) {
	var (
		agent   agentregistry.Agent
		desc    sql.NullString
		profile sql.NullString
	)
	if err := row.Scan(&agent.ID, &agent.Name, &desc, &profile, &agent.CreatedAt); err != nil {
		return agentregistry.Agent{}, err
	}
	agent.Description = desc.String
	if profile.Valid {
		agent.DatabaseProfileID = &profile.String
	}
	return agent, nil
}

func scanVersion(row interface{ Scan(...any) error }, extra ...any) (agentregistry.Version, error) {
	var (
		version agentregistry.Version
		catalog sql.NullString
	)
	dest := append([]any{&version.ID, &version.AgentID, &version.Version, &version.ChatEndpoint, &version.TargetModel,
		&version.ContractVersion, &version.ToolEvaluationMode, &catalog, &version.CreatedAt}, extra...)
	if err := row.Scan(dest...); err != nil {
		return agentregistry.Version{}, err
	}
	if catalog.Valid {
		if err := json.Unmarshal([]byte(catalog.String), &version.ToolCatalog); err != nil {
			return agentregistry.Version{}, fmt.Errorf("decode Tool Catalog: %w", err)
		}
	}
	return version, nil
}

func nullableProfile(id *string) any {
	if id == nil {
		return nil
	}
	return *id
}
