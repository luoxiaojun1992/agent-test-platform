package llm

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

type scriptedCompleter struct {
	responses []CompletionResponse
	errors    []error
	calls     []CompletionRequest
}

func (script *scriptedCompleter) Complete(_ context.Context, request CompletionRequest) (CompletionResponse, error) {
	script.calls = append(script.calls, request)
	index := len(script.calls) - 1
	if index < len(script.errors) && script.errors[index] != nil {
		return CompletionResponse{}, script.errors[index]
	}
	if index < len(script.responses) {
		return script.responses[index], nil
	}
	return CompletionResponse{Model: request.Model, Content: "ok"}, nil
}

func TestGatewayResolvesAliasAndRetriesTransientError(t *testing.T) {
	t.Parallel()

	adapter := &scriptedCompleter{
		errors:    []error{&HTTPError{Provider: ProviderOpenAI, StatusCode: http.StatusTooManyRequests}, nil},
		responses: []CompletionResponse{{}, {Content: "ok"}},
	}
	var sleeps []time.Duration
	gateway := testGateway(t, adapter, func(_ context.Context, duration time.Duration) error {
		sleeps = append(sleeps, duration)
		return nil
	})
	response, err := gateway.Complete(context.Background(), "primary", validGatewayRequest())
	if err != nil {
		t.Fatal(err)
	}
	if response.Content != "ok" || len(adapter.calls) != 2 || len(sleeps) != 1 {
		t.Fatalf("response=%#v calls=%d sleeps=%v", response, len(adapter.calls), sleeps)
	}
	for _, request := range adapter.calls {
		if request.Model.Provider != ProviderOpenAI || request.Model.Model != "gpt-test" {
			t.Fatalf("alias did not resolve: %#v", request.Model)
		}
	}
}

func TestGatewayDoesNotRetryNonTransientOrContextErrors(t *testing.T) {
	t.Parallel()

	for _, failure := range []error{
		&HTTPError{Provider: ProviderOpenAI, StatusCode: http.StatusBadRequest},
		context.Canceled,
	} {
		adapter := &scriptedCompleter{errors: []error{failure}}
		gateway := testGateway(t, adapter, func(context.Context, time.Duration) error { return nil })
		if _, err := gateway.Complete(context.Background(), "primary", validGatewayRequest()); err == nil {
			t.Fatal("completion succeeded")
		}
		if len(adapter.calls) != 1 {
			t.Fatalf("non-retryable error was retried %d times", len(adapter.calls))
		}
	}
}

func TestGatewayRetriesInternalTimeoutButNotCallerCancellation(t *testing.T) {
	t.Parallel()

	adapter := &scriptedCompleter{errors: []error{context.DeadlineExceeded, nil}, responses: []CompletionResponse{{}, {Content: "ok"}}}
	gateway := testGateway(t, adapter, func(context.Context, time.Duration) error { return nil })
	if _, err := gateway.Complete(context.Background(), "primary", validGatewayRequest()); err != nil {
		t.Fatal(err)
	}
	if len(adapter.calls) != 2 {
		t.Fatalf("internal timeout calls = %d", len(adapter.calls))
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	adapter = &scriptedCompleter{errors: []error{context.Canceled}}
	gateway = testGateway(t, adapter, func(context.Context, time.Duration) error { return nil })
	if _, err := gateway.Complete(cancelled, "primary", validGatewayRequest()); err == nil {
		t.Fatal("cancelled caller succeeded")
	}
	if len(adapter.calls) != 1 {
		t.Fatalf("cancelled caller was retried %d times", len(adapter.calls))
	}
}

func TestGatewayCapabilityAndTokenLimits(t *testing.T) {
	t.Parallel()

	adapter := &scriptedCompleter{}
	gateway, err := newGateway([]ModelConfig{{
		Alias: "limited", Model: ModelRef{Provider: ProviderOllama, Model: "local"}, Adapter: adapter,
		Timeout:      time.Second,
		Capabilities: ModelCapabilities{ContextWindow: 2048, MaxOutputTokens: 100},
	}}, time.Now, func(context.Context, time.Duration) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	request := validGatewayRequest()
	request.MaxOutputTokens = 101
	if _, err := gateway.Complete(context.Background(), "limited", request); err == nil {
		t.Fatal("unsupported capabilities or token limit passed")
	}
	if len(adapter.calls) != 0 {
		t.Fatal("adapter called before capability validation")
	}
}

func TestGatewayCircuitBreaker(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	adapter := &scriptedCompleter{}
	for range 10 {
		adapter.errors = append(adapter.errors, &net.DNSError{IsTimeout: true})
	}
	gateway, err := newGateway([]ModelConfig{{
		Alias: "primary", Model: ModelRef{Provider: ProviderOpenAI, Model: "gpt-test"}, Adapter: adapter,
		Timeout: time.Second, MaxRetries: 0,
		Capabilities: ModelCapabilities{ToolCalling: true, NamedToolChoice: true, JSONSchemaOutput: true, ContextWindow: 8192, MaxOutputTokens: 1024},
	}}, func() time.Time { return now }, func(context.Context, time.Duration) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 5; attempt++ {
		_, _ = gateway.Complete(context.Background(), "primary", validGatewayRequest())
	}
	if _, err := gateway.Complete(context.Background(), "primary", validGatewayRequest()); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("circuit error = %v", err)
	}
	if len(adapter.calls) != 5 {
		t.Fatalf("open circuit called adapter: %d", len(adapter.calls))
	}
	now = now.Add(31 * time.Second)
	_, _ = gateway.Complete(context.Background(), "primary", validGatewayRequest())
	if len(adapter.calls) != 6 {
		t.Fatalf("expired circuit did not call adapter: %d", len(adapter.calls))
	}
}

func testGateway(t *testing.T, adapter Completer, sleep Sleeper) *Gateway {
	t.Helper()
	gateway, err := newGateway([]ModelConfig{{
		Alias: "primary", Model: ModelRef{Provider: ProviderOpenAI, Model: "gpt-test"}, Adapter: adapter,
		Timeout: time.Second, MaxRetries: 1, Backoff: []time.Duration{10 * time.Millisecond},
		Capabilities: ModelCapabilities{ToolCalling: true, NamedToolChoice: true, JSONSchemaOutput: true, ContextWindow: 8192, MaxOutputTokens: 1024},
	}}, time.Now, sleep)
	if err != nil {
		t.Fatal(err)
	}
	return gateway
}

func validGatewayRequest() CompletionRequest {
	return CompletionRequest{
		Messages:       []Message{{Role: RoleUser, Content: stringPointer("test")}},
		Tools:          []ToolDefinition{{Name: "tool", Parameters: []byte(`{"type":"object"}`)}},
		ToolChoice:     ToolChoice{Mode: ToolChoiceNamed, Name: "tool"},
		ResponseSchema: []byte(`{"type":"object"}`), MaxOutputTokens: 256,
	}
}
