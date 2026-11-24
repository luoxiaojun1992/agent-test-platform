// Package agentregistry stores Agents under test (docs/architecture.md section 16).
// An Agent records the SSE chat endpoint, the name of the bearer-token secret, the model, and the optional Tool Catalog.
// The token itself is never part of a stored record.
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
	MaxPageSize     = 200
	ContractVersion = "1.0"
)

var (
	ErrInvalidInput        = errors.New("agent request is invalid")
	ErrSecretNotFound      = errors.New("target bearer token secret is not found; enter it on the Credentials page first")
	ErrDatabaseCheckFailed = errors.New("the Agent database check failed")
	ErrConflict            = errors.New("an Agent with this name already exists")
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
	DatabaseProfileID *string `json:"database_profile_id,omitempty"`
	// ChatEndpoint is the base URL the platform calls for chat and tool discovery.
	ChatEndpoint string `json:"chat_endpoint"`
	// TargetBearerTokenRef names a stored secret; the token itself is never part of a request body.
	TargetBearerTokenRef string           `json:"target_bearer_token_ref"`
	TargetModel          string           `json:"target_model"`
	ContractVersion      string           `json:"contract_version"`
	ToolCatalog          []ToolDefinition `json:"tool_catalog"`
	// ClientID, ClientSecretRef and TokenURL configure OAuth 2.0 client credentials. When TokenURL
	// is set, the platform obtains a short-lived access token instead of the static bearer (08).
	ClientID        *string `json:"client_id,omitempty"`
	ClientSecretRef *string `json:"client_secret_ref,omitempty"`
	TokenURL        *string `json:"token_url,omitempty"`
}

// Agent is a stored Agent under test. It omits the token reference.
type Agent struct {
	ID                string           `json:"id"`
	Name              string           `json:"name"`
	Description       string           `json:"description,omitempty"`
	DatabaseProfileID *string          `json:"database_profile_id,omitempty"`
	ChatEndpoint      string           `json:"chat_endpoint"`
	TargetModel       string           `json:"target_model"`
	ContractVersion   string           `json:"contract_version"`
	ToolCatalog       []ToolDefinition `json:"tool_catalog"`
	CreatedAt         time.Time        `json:"created_at"`
	// TargetBearerTokenRef is stored but never serialized back to clients.
	TargetBearerTokenRef string `json:"-"`
	ClientID             string `json:"client_id,omitempty"`
	TokenURL             string `json:"token_url,omitempty"`
	// ClientSecretRef is stored but never serialized back to clients.
	ClientSecretRef string `json:"-"`
}

// ToolDefinition describes one Tool the Agent may call.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
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
	parsed, err := url.Parse(input.ChatEndpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || len(input.ChatEndpoint) > 2048 {
		return invalid("chat_endpoint must be an http or https URL of at most 2048 characters")
	}
	if err := secrets.ValidateName(input.TargetBearerTokenRef); err != nil {
		return fmt.Errorf("target_bearer_token_ref: %w", err)
	}
	if input.TokenURL != nil && strings.TrimSpace(*input.TokenURL) != "" {
		oauthURL, err := url.Parse(*input.TokenURL)
		if err != nil || (oauthURL.Scheme != "http" && oauthURL.Scheme != "https") || oauthURL.Host == "" || len(*input.TokenURL) > 2048 {
			return invalid("token_url must be an http or https URL of at most 2048 characters")
		}
		if input.ClientID == nil || strings.TrimSpace(*input.ClientID) == "" {
			return invalid("client_id is required when token_url is set")
		}
		if input.ClientSecretRef == nil || strings.TrimSpace(*input.ClientSecretRef) == "" {
			return invalid("client_secret_ref is required when token_url is set")
		}
		if err := secrets.ValidateName(*input.ClientSecretRef); err != nil {
			return fmt.Errorf("client_secret_ref: %w", err)
		}
	} else if input.ClientID != nil || input.ClientSecretRef != nil {
		return invalid("client_id and client_secret_ref require token_url")
	}
	if n := len([]rune(input.TargetModel)); n < 1 || n > 255 {
		return invalid("target_model must contain 1-255 characters")
	}
	if input.ContractVersion != ContractVersion {
		return invalid("contract_version must be %s", ContractVersion)
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

// Cursor is an opaque list position.
type Cursor struct {
	CreatedAt time.Time
	ID        string
}

// Store persists Agents. The MySQL implementation lives in the storage layer.
type Store interface {
	InsertAgent(ctx context.Context, record AgentRecord) (Agent, error)
	FindAgentByIdempotencyKey(ctx context.Context, key string) (AgentRecord, error)
	GetAgent(ctx context.Context, id string) (Agent, error)
	ListAgents(ctx context.Context, after *Cursor, limit int) ([]Agent, error)
	// DatabaseProfileExists supports reference checks.
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

// Service manages Agents under test.
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

// CreateAgent stores a new Agent under test. Replaying a key with the same request returns the stored Agent.
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
	exists, err := service.secrets.Exists(ctx, input.TargetBearerTokenRef)
	if err != nil {
		return Agent{}, fmt.Errorf("check bearer token secret: %w", err)
	}
	if !exists {
		return Agent{}, ErrSecretNotFound
	}
	if input.ClientSecretRef != nil && strings.TrimSpace(*input.ClientSecretRef) != "" {
		clientSecretExists, err := service.secrets.Exists(ctx, *input.ClientSecretRef)
		if err != nil {
			return Agent{}, fmt.Errorf("check client secret: %w", err)
		}
		if !clientSecretExists {
			return Agent{}, ErrSecretNotFound
		}
	}
	digest, err := digestOf(input)
	if err != nil {
		return Agent{}, err
	}
	record := AgentRecord{Agent: Agent{
		ID: ulid.Make().String(), Name: input.Name, Description: input.Description, DatabaseProfileID: input.DatabaseProfileID,
		ChatEndpoint: input.ChatEndpoint, TargetModel: input.TargetModel, ContractVersion: input.ContractVersion,
		ToolCatalog: input.ToolCatalog, TargetBearerTokenRef: input.TargetBearerTokenRef,
		ClientID: derefString(input.ClientID), ClientSecretRef: derefString(input.ClientSecretRef), TokenURL: derefString(input.TokenURL),
	}, IdempotencyKey: key, ConfigSHA256: digest}
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

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
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
