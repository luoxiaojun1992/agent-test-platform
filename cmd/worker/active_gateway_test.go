package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"agent-test-platform/internal/llm"
)

type recordingCompleter struct{ aliases []string }

func (r *recordingCompleter) Complete(_ context.Context, alias string, _ llm.CompletionRequest) (llm.CompletionResponse, error) {
	r.aliases = append(r.aliases, alias)
	return llm.CompletionResponse{}, nil
}

type fakeSource struct {
	alias string
	err   error
	calls int
}

func (f *fakeSource) ActiveAlias(context.Context, string) (string, error) {
	f.calls++
	return f.alias, f.err
}

func TestActiveModelGatewayFollowsSelection(t *testing.T) {
	inner, source := &recordingCompleter{}, &fakeSource{alias: "gen-b"}
	now := time.Unix(0, 0)
	gateway := newActiveModelGateway(inner, source, "generation", "gen-a", []string{"gen-a", "gen-b"})
	gateway.now = func() time.Time { return now }
	call := func() { _, _ = gateway.Complete(context.Background(), "ignored", llm.CompletionRequest{}) }
	call()
	call()
	if source.calls != 1 || inner.aliases[1] != "gen-b" {
		t.Fatalf("selection must be cached: calls=%d aliases=%v", source.calls, inner.aliases)
	}
	source.alias, now = "gen-a", now.Add(10*time.Second)
	call()
	if inner.aliases[2] != "gen-a" {
		t.Fatalf("selection change must apply after the TTL: %v", inner.aliases)
	}
}

func TestActiveModelGatewayFallsBack(t *testing.T) {
	for name, source := range map[string]*fakeSource{"error": {err: errors.New("db down")}, "unknown": {alias: "not-on-this-worker"}} {
		inner := &recordingCompleter{}
		gateway := newActiveModelGateway(inner, source, "generation", "gen-a", []string{"gen-a"})
		_, _ = gateway.Complete(context.Background(), "x", llm.CompletionRequest{})
		if inner.aliases[0] != "gen-a" {
			t.Fatalf("%s: %v", name, inner.aliases)
		}
	}
}
