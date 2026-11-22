package main

import (
	"context"
	"sync"
	"time"

	"agent-test-platform/internal/llm"
)

// activeAliasSource reports the alias selected for a model role.
type activeAliasSource interface {
	ActiveAlias(ctx context.Context, role string) (string, error)
}

type completer interface {
	Complete(context.Context, string, llm.CompletionRequest) (llm.CompletionResponse, error)
}

// activeModelGateway sends every Generation call to the model currently selected through the models API,
// falling back to the configured alias while nothing is selected or the selection is unavailable.
type activeModelGateway struct {
	inner    completer
	source   activeAliasSource
	role     string
	fallback string
	known    map[string]bool
	ttl      time.Duration
	now      func() time.Time

	mu      sync.Mutex
	alias   string
	fetched time.Time
}

func newActiveModelGateway(inner completer, source activeAliasSource, role, fallback string, known []string) *activeModelGateway {
	set := map[string]bool{}
	for _, alias := range known {
		set[alias] = true
	}
	return &activeModelGateway{inner: inner, source: source, role: role, fallback: fallback, known: set, ttl: 5 * time.Second, now: time.Now}
}

func (gateway *activeModelGateway) resolve(ctx context.Context) string {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	if gateway.alias != "" && gateway.now().Sub(gateway.fetched) < gateway.ttl {
		return gateway.alias
	}
	alias, err := gateway.source.ActiveAlias(ctx, gateway.role)
	if err != nil || !gateway.known[alias] {
		alias = gateway.fallback
	}
	gateway.alias, gateway.fetched = alias, gateway.now()
	return alias
}

func (gateway *activeModelGateway) Complete(ctx context.Context, _ string, request llm.CompletionRequest) (llm.CompletionResponse, error) {
	return gateway.inner.Complete(ctx, gateway.resolve(ctx), request)
}
