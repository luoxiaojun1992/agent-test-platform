package agentregistry

import (
	"context"
	"errors"
	"testing"
)

type fakeResolver struct{ err error }

func (f fakeResolver) Resolve(context.Context, string) (string, error) { return "tok", f.err }

type fakeFetcher struct {
	tools []ToolDefinition
	err   error
}

func (f fakeFetcher) FetchTools(context.Context, string, string) ([]ToolDefinition, error) {
	return f.tools, f.err
}

func TestDiscoverTools(t *testing.T) {
	in := DiscoveryInput{ChatEndpoint: "https://a/chat", TargetBearerTokenRef: "tok"}
	good := []ToolDefinition{{Name: "run_query", Description: "d", InputSchema: []byte(`{"type":"object"}`)}}
	d, _ := NewDiscovery(fakeResolver{}, fakeFetcher{tools: good})
	if tools, err := d.DiscoverTools(context.Background(), in); err != nil || len(tools) != 1 {
		t.Fatal(tools, err)
	}
	d, _ = NewDiscovery(fakeResolver{err: errors.New("x")}, fakeFetcher{})
	if _, err := d.DiscoverTools(context.Background(), in); !errors.Is(err, ErrSecretNotFound) {
		t.Fatal(err)
	}
	d, _ = NewDiscovery(fakeResolver{}, fakeFetcher{err: errors.New("down")})
	if _, err := d.DiscoverTools(context.Background(), in); !errors.Is(err, ErrDiscoveryFailed) {
		t.Fatal(err)
	}
	d, _ = NewDiscovery(fakeResolver{}, fakeFetcher{})
	if _, err := d.DiscoverTools(context.Background(), in); !errors.Is(err, ErrDiscoveryFailed) {
		t.Fatal("empty catalog must fail", err)
	}
	if _, err := d.DiscoverTools(context.Background(), DiscoveryInput{}); !IsInvalidInput(err) {
		t.Fatal(err)
	}
}
