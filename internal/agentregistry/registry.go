// Package agentregistry stores Agents and their immutable Agent Versions (docs/architecture.md section 16).
// A version records the SSE chat endpoint, the name of the bearer-token secret, and the optional Tool Catalog.
// The token itself is never part of a record.
package agentregistry

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"agent-test-platform/internal/secrets"
)

const (
	MaxPageSize = 200

	ModeDeterministicChain = "DETERMINISTIC_CHAIN"
	ModeSessionLLMOnly     = "SESSION_LLM_ONLY"
	ContractVersion        = "1.0"
)

var (
	ErrInvalidInput        = errors.New("agent request is invalid")
	ErrSecretNotFound      = errors.New("target bearer token secret is not found; enter it on the Credentials page first")
	ErrDatabaseCheckFailed = errors.New("the Agent database check failed")
	ErrConflict            = errors.New("an Agent with this name or an Agent Version with this version already exists")
	ErrIdempotencyConflict = errors.New("Idempotency-Key was already used for a different request")
	ErrDuplicate           = errors.New("agent violates a unique key")
	ErrNotFound            = errors.New("agent is not found")
	ErrInvalidCursor       = errors.New("invalid list cursor")
	ErrInvalidLimit        = errors.New("limit must be from 1 to 200")
)

var toolNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)

// IsInvalidInput reports whether err is a request validation failure, which the API reports as 400.
func IsInvalidInput(err error) bool {
	return errors.Is(err, ErrInvalidInput) || errors.Is(err, secrets.ErrInvalidName)
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidInput, fmt.Sprintf(format, args...))
}

// AgentInput is the body of POST /agents.
type AgentInput struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// DatabaseProfileID is the Agent's own database. The platform uses it only to write test data; it is never sent to the Agent.
	DatabaseProfileID *string `json:"database_profile_id"`
}

// Agent is a stable Agent identity.
type Agent struct {
	ID string `json:"id"`
	AgentInput
	CreatedAt time.Time `json:"created_at"`
}

// ToolDefinition describes one Tool the Agent may call.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// VersionInput is the body of POST /agents/{id}/versions.
type VersionInput struct {
	Version              string           `json:"version"`
	ChatEndpoint         string           `json:"chat_endpoint"`
	TargetBearerTokenRef string           `json:"target_bearer_token_ref"`
	TargetModel          string           `json:"target_model"`
	ContractVersion      string           `json:"contract_version"`
	ToolEvaluationMode   string           `json:"tool_evaluation_mode"`
	ToolCatalog          []ToolDefinition `json:"tool_catalog"`
}

// Version is a stored Agent Version. It omits the token reference.
type Version struct {
	ID                   string           `json:"id"`
	AgentID              string           `json:"agent_id"`
	Version              string           `json:"version"`
	ChatEndpoint         string           `json:"chat_endpoint"`
	TargetModel          string           `json:"target_model"`
	ContractVersion      string           `json:"contract_version"`
	ToolEvaluationMode   string           `json:"tool_evaluation_mode"`
	ToolCatalog          []ToolDefinition `json:"tool_catalog"`
	CreatedAt            time.Time        `json:"created_at"`
	TargetBearerTokenRef string           `json:"-"`
}

// Validate checks the request without touching the database.
func (input AgentInput) Validate() error {
	if n := len([]rune(input.Name)); n < 1 || n > 255 {
		return invalid("name must contain 1-255 characters")
	}
	if len([]rune(input.Description)) > 2000 {
		return invalid("description must not exceed 2000 characters")
	}
	if input.DatabaseProfileID != nil {
		if _, err := ulid.ParseStrict(*input.DatabaseProfileID); err != nil {
			return invalid("database_profile_id must be a ULID")
		}
	}
	return nil
}

// Validate checks the request without touching the database.
func (input VersionInput) Validate() error {
	if n := len([]rune(input.Version)); n < 1 || n > 128 {
		return invalid("version must contain 1-128 characters")
	}
	parsed, err := url.Parse(input.ChatEndpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || len(input.ChatEndpoint) > 2048 {
		return invalid("chat_endpoint must be an http or https URL of at most 2048 characters")
	}
	if err := secrets.ValidateName(input.TargetBearerTokenRef); err != nil {
		return fmt.Errorf("target_bearer_token_ref: %w", err)
	}
	if n := len([]rune(input.TargetModel)); n < 1 || n > 255 {
		return invalid("target_model must contain 1-255 characters")
	}
	if input.ContractVersion != ContractVersion {
		return invalid("contract_version must be %s", ContractVersion)
	}
	switch input.ToolEvaluationMode {
	case ModeDeterministicChain:
		if len(input.ToolCatalog) < 1 {
			return invalid("tool_catalog is required for %s", ModeDeterministicChain)
		}
	case ModeSessionLLMOnly:
		if input.ToolCatalog != nil {
			return invalid("tool_catalog must be null for %s", ModeSessionLLMOnly)
		}
	default:
		return invalid("tool_evaluation_mode must be %s or %s", ModeDeterministicChain, ModeSessionLLMOnly)
	}
	if len(input.ToolCatalog) > 64 {
		return invalid("tool_catalog must not exceed 64 tools")
	}
	seen := map[string]bool{}
	for _, tool := range input.ToolCatalog {
		if !toolNamePattern.MatchString(tool.Name) || seen[tool.Name] {
			return invalid("tool names must be unique and match %s", toolNamePattern)
		}
		seen[tool.Name] = true
		if n := len([]rune(tool.Description)); n < 1 || n > 1000 {
			return invalid("tool %s description must contain 1-1000 characters", tool.Name)
		}
		var schema map[string]any
		if json.Unmarshal(tool.InputSchema, &schema) != nil || schema == nil {
			return invalid("tool %s input_schema must be a JSON object", tool.Name)
		}
	}
	return nil
}

// AgentRecord is an Agent as stored, with the replay-detection data.
type AgentRecord struct {
	Agent          Agent
	IdempotencyKey string
	ConfigSHA256   string
}

// VersionRecord is a Version as stored, with the replay-detection data.
type VersionRecord struct {
	Version        Version
	IdempotencyKey string
	ConfigSHA256   string
}

// Cursor is an opaque list position.
type Cursor struct {
	CreatedAt time.Time
	ID        string
}

// Store persists Agents and Versions. The MySQL implementation lives in the storage layer.
type Store interface {
	InsertAgent(ctx context.Context, record AgentRecord) (Agent, error)
	FindAgentByIdempotencyKey(ctx context.Context, key string) (AgentRecord, error)
	GetAgent(ctx context.Context, id string) (Agent, error)
	ListAgents(ctx context.Context, after *Cursor, limit int) ([]Agent, error)
	InsertVersion(ctx context.Context, record VersionRecord) (Version, error)
	FindVersionByIdempotencyKey(ctx context.Context, key string) (VersionRecord, error)
	ListVersions(ctx context.Context, agentID string, after *Cursor, limit int) ([]Version, error)
	// DatabaseProfileExists and AgentExists support reference checks.
	DatabaseProfileExists(ctx context.Context, id string) (bool, error)
}

// DatabaseChecker proves that the database configured for an Agent accepts what a Run needs: connecting, creating a database and
// writing a row. It is optional; without one only the reference is checked.
type DatabaseChecker interface {
	CheckDatabase(ctx context.Context, profileID string) error
}

// SecretChecker reports whether a secret exists.
type SecretChecker interface {
	Exists(ctx context.Context, name string) (bool, error)
}

// Service manages Agents and Agent Versions.
type Service struct {
	store   Store
	secrets SecretChecker
	checker DatabaseChecker
}

// SetDatabaseChecker makes CreateAgent verify the configured database before storing the Agent.
func (service *Service) SetDatabaseChecker(checker DatabaseChecker) { service.checker = checker }

// NewService returns a Service.
func NewService(store Store, secrets SecretChecker) (*Service, error) {
	if store == nil || secrets == nil {
		return nil, errors.New("agent store and secret checker are required")
	}
	return &Service{store: store, secrets: secrets}, nil
}

// CreateAgent stores a new Agent. Replaying a key with the same request returns the stored Agent.
func (service *Service) CreateAgent(ctx context.Context, key string, input AgentInput) (Agent, error) {
	if err := input.Validate(); err != nil {
		return Agent{}, err
	}
	if input.DatabaseProfileID != nil {
		found, err := service.store.DatabaseProfileExists(ctx, *input.DatabaseProfileID)
		if err != nil {
			return Agent{}, fmt.Errorf("check Database Profile: %w", err)
		}
		if !found {
			return Agent{}, invalid("database_profile_id does not exist")
		}
		if service.checker != nil {
			if err := service.checker.CheckDatabase(ctx, *input.DatabaseProfileID); err != nil {
				return Agent{}, fmt.Errorf("%w: %v", ErrDatabaseCheckFailed, err)
			}
		}
	}
	digest, err := digestOf(input)
	if err != nil {
		return Agent{}, err
	}
	record := AgentRecord{Agent: Agent{ID: ulid.Make().String(), AgentInput: input}, IdempotencyKey: key, ConfigSHA256: digest}
	stored, err := service.store.InsertAgent(ctx, record)
	if err == nil {
		return stored, nil
	}
	if !errors.Is(err, ErrDuplicate) {
		return Agent{}, err
	}
	replay, findErr := service.store.FindAgentByIdempotencyKey(ctx, key)
	if findErr == nil {
		if replay.ConfigSHA256 != digest {
			return Agent{}, ErrIdempotencyConflict
		}
		return replay.Agent, nil
	}
	if !errors.Is(findErr, ErrNotFound) {
		return Agent{}, findErr
	}
	return Agent{}, ErrConflict
}

// GetAgent returns one Agent or ErrNotFound.
func (service *Service) GetAgent(ctx context.Context, id string) (Agent, error) {
	if _, err := ulid.ParseStrict(id); err != nil {
		return Agent{}, ErrNotFound
	}
	return service.store.GetAgent(ctx, id)
}

// ListAgents returns one page of Agents, newest first.
func (service *Service) ListAgents(ctx context.Context, cursor string, limit int) ([]Agent, string, error) {
	after, err := pageArgs(cursor, limit)
	if err != nil {
		return nil, "", err
	}
	items, err := service.store.ListAgents(ctx, after, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("list Agents: %w", err)
	}
	if len(items) <= limit {
		return items, "", nil
	}
	items = items[:limit]
	last := items[limit-1]
	return items, encodeCursor(Cursor{CreatedAt: last.CreatedAt, ID: last.ID}), nil
}

// CreateVersion stores a new Agent Version after checking the secret reference.
func (service *Service) CreateVersion(ctx context.Context, agentID, key string, input VersionInput) (Version, error) {
	if _, err := service.GetAgent(ctx, agentID); err != nil {
		return Version{}, err
	}
	if err := input.Validate(); err != nil {
		return Version{}, err
	}
	exists, err := service.secrets.Exists(ctx, input.TargetBearerTokenRef)
	if err != nil {
		return Version{}, fmt.Errorf("check bearer token secret: %w", err)
	}
	if !exists {
		return Version{}, ErrSecretNotFound
	}
	digest, err := digestOf(struct {
		AgentID string `json:"agent_id"`
		VersionInput
	}{agentID, input})
	if err != nil {
		return Version{}, err
	}
	record := VersionRecord{
		Version: Version{
			ID: ulid.Make().String(), AgentID: agentID, Version: input.Version, ChatEndpoint: input.ChatEndpoint,
			TargetModel: input.TargetModel, ContractVersion: input.ContractVersion, ToolEvaluationMode: input.ToolEvaluationMode,
			ToolCatalog: input.ToolCatalog, TargetBearerTokenRef: input.TargetBearerTokenRef,
		},
		IdempotencyKey: key, ConfigSHA256: digest,
	}
	stored, err := service.store.InsertVersion(ctx, record)
	if err == nil {
		return stored, nil
	}
	if !errors.Is(err, ErrDuplicate) {
		return Version{}, err
	}
	replay, findErr := service.store.FindVersionByIdempotencyKey(ctx, key)
	if findErr == nil {
		if replay.ConfigSHA256 != digest {
			return Version{}, ErrIdempotencyConflict
		}
		return replay.Version, nil
	}
	if !errors.Is(findErr, ErrNotFound) {
		return Version{}, findErr
	}
	return Version{}, ErrConflict
}

// ListVersions returns one page of an Agent's Versions, newest first.
func (service *Service) ListVersions(ctx context.Context, agentID, cursor string, limit int) ([]Version, string, error) {
	if _, err := service.GetAgent(ctx, agentID); err != nil {
		return nil, "", err
	}
	after, err := pageArgs(cursor, limit)
	if err != nil {
		return nil, "", err
	}
	items, err := service.store.ListVersions(ctx, agentID, after, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("list Agent Versions: %w", err)
	}
	if len(items) <= limit {
		return items, "", nil
	}
	items = items[:limit]
	last := items[limit-1]
	return items, encodeCursor(Cursor{CreatedAt: last.CreatedAt, ID: last.ID}), nil
}

func pageArgs(cursor string, limit int) (*Cursor, error) {
	if limit < 1 || limit > MaxPageSize {
		return nil, ErrInvalidLimit
	}
	if cursor == "" {
		return nil, nil
	}
	decoded, err := decodeCursor(cursor)
	if err != nil {
		return nil, ErrInvalidCursor
	}
	return &decoded, nil
}

func digestOf(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode request: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func encodeCursor(cursor Cursor) string {
	return base64.RawURLEncoding.EncodeToString([]byte(cursor.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + cursor.ID))
}

func decodeCursor(encoded string) (Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return Cursor{}, ErrInvalidCursor
	}
	createdAt, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	if _, err := ulid.ParseStrict(parts[1]); err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	return Cursor{CreatedAt: createdAt, ID: parts[1]}, nil
}
