package agentregistry

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// SecretResolver returns the plaintext of a stored secret.
type SecretResolver interface {
	Resolve(ctx context.Context, name string) (string, error)
}

// ToolFetcher reads the complete Tool list from a Target Agent's GET /v1/tools.
type ToolFetcher interface {
	FetchTools(ctx context.Context, endpoint, token string) ([]ToolDefinition, error)
}

// DiscoveryInput identifies the Target Agent to query.
type DiscoveryInput struct {
	ChatEndpoint         string `json:"chat_endpoint"`
	TargetBearerTokenRef string `json:"target_bearer_token_ref"`
}

// Discovery lets testers pull the Tool list from an Agent before registering a Version.
type Discovery struct {
	secrets SecretResolver
	fetcher ToolFetcher
}

func NewDiscovery(secrets SecretResolver, fetcher ToolFetcher) (*Discovery, error) {
	if secrets == nil || fetcher == nil {
		return nil, errors.New("secret resolver and tool fetcher are required")
	}
	return &Discovery{secrets: secrets, fetcher: fetcher}, nil
}

// DiscoverTools returns the Agent's own Tool definitions, validated like a submitted tool_catalog.
func (discovery *Discovery) DiscoverTools(ctx context.Context, input DiscoveryInput) ([]ToolDefinition, error) {
	if strings.TrimSpace(input.ChatEndpoint) == "" || strings.TrimSpace(input.TargetBearerTokenRef) == "" {
		return nil, invalid("chat_endpoint and target_bearer_token_ref are required")
	}
	token, err := discovery.secrets.Resolve(ctx, input.TargetBearerTokenRef)
	if err != nil {
		return nil, ErrSecretNotFound
	}
	tools, err := discovery.fetcher.FetchTools(ctx, input.ChatEndpoint, token)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDiscoveryFailed, err)
	}
	check := VersionInput{Version: "v", ChatEndpoint: input.ChatEndpoint, TargetBearerTokenRef: input.TargetBearerTokenRef,
		TargetModel: "m", ContractVersion: ContractVersion, ToolEvaluationMode: ModeDeterministicChain, ToolCatalog: tools}
	if err := check.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDiscoveryFailed, err)
	}
	return tools, nil
}

// ErrDiscoveryFailed reports that the Target Agent's tool list could not be read or is invalid.
var ErrDiscoveryFailed = errors.New("target Agent tool list is unavailable or invalid")
