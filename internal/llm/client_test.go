package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIAndOllamaAdaptersShareContract(t *testing.T) {
	t.Parallel()

	providers := []Provider{ProviderOpenAI, ProviderOllama}
	for _, provider := range providers {
		provider := provider
		t.Run(string(provider), func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/v1/chat/completions" {
					t.Errorf("path = %s", request.URL.Path)
				}
				if provider == ProviderOpenAI && request.Header.Get("Authorization") != "Bearer secret" {
					t.Errorf("OpenAI authorization = %q", request.Header.Get("Authorization"))
				}
				if provider == ProviderOllama && request.Header.Get("Authorization") != "" {
					t.Errorf("Ollama received authorization")
				}
				var body map[string]any
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if provider == ProviderOpenAI {
					if body["max_completion_tokens"] != float64(256) || body["max_tokens"] != nil {
						t.Errorf("OpenAI token fields = %#v", body)
					}
				} else if body["max_tokens"] != float64(256) || body["max_completion_tokens"] != nil {
					t.Errorf("Ollama token fields = %#v", body)
				}
				if body["response_format"] == nil || body["tools"] == nil {
					t.Errorf("structured output or tools missing: %#v", body)
				}
				writer.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(writer, `{
					"id":"response-1","model":"resolved-model",
					"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"create_test_case","arguments":"{\"client_case_id\":\"case-1\"}"}}]},"finish_reason":"tool_calls"}],
					"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}
				}`)
			}))
			defer server.Close()

			var adapter *Adapter
			var err error
			if provider == ProviderOpenAI {
				adapter, err = NewOpenAIAdapter(server.URL+"/v1", "secret", http.DefaultClient)
			} else {
				adapter, err = NewOllamaAdapter(server.URL+"/v1", http.DefaultClient)
			}
			if err != nil {
				t.Fatal(err)
			}
			response, err := adapter.Complete(context.Background(), validCompletionRequest(provider))
			if err != nil {
				t.Fatal(err)
			}
			if response.StopReason != "tool_calls" || len(response.ToolCalls) != 1 || !response.Usage.Available || len(response.RawDigest) != 64 {
				t.Fatalf("response = %#v", response)
			}
		})
	}
}

func TestAdapterRejectsMalformedToolCall(t *testing.T) {
	t.Parallel()

	server := responseServer(`{"id":"x","choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"tool","arguments":"not-json"}}]},"finish_reason":"tool_calls"}]}`, http.StatusOK)
	defer server.Close()
	adapter, _ := NewOllamaAdapter(server.URL+"/v1", http.DefaultClient)
	if _, err := adapter.Complete(context.Background(), validCompletionRequest(ProviderOllama)); err == nil || !strings.Contains(err.Error(), "malformed tool call") {
		t.Fatalf("malformed tool error = %v", err)
	}
}

func TestAuthenticatedOllamaAdapterUsesIndependentBearerToken(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer ollama-secret" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"id":"response-1","model":"local","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	adapter, err := NewOllamaAdapterWithToken(server.URL+"/v1", "ollama-secret", http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	request := validCompletionRequest(ProviderOllama)
	request.Tools = nil
	request.ToolChoice = ToolChoice{Mode: ToolChoiceNone}
	request.ResponseSchema = nil
	if _, err := adapter.Complete(context.Background(), request); err != nil {
		t.Fatal(err)
	}
}

func TestAdapterValidatesRequestBeforeNetwork(t *testing.T) {
	t.Parallel()

	adapter, err := NewOpenAIAdapter("https://api.openai.example/v1", "secret", doerFunc(func(*http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	}))
	if err != nil {
		t.Fatal(err)
	}
	request := validCompletionRequest(ProviderOpenAI)
	request.ToolChoice = ToolChoice{Mode: ToolChoiceNamed, Name: "missing"}
	if _, err := adapter.Complete(context.Background(), request); err == nil || !strings.Contains(err.Error(), "not defined") {
		t.Fatalf("invalid request error = %v", err)
	}
}

func TestAdapterReturnsHTTPErrorAndLimitsResponse(t *testing.T) {
	t.Parallel()

	server := responseServer(`{"error":"unavailable"}`, http.StatusServiceUnavailable)
	defer server.Close()
	adapter, _ := NewOllamaAdapter(server.URL+"/v1", http.DefaultClient)
	if _, err := adapter.Complete(context.Background(), validCompletionRequest(ProviderOllama)); err == nil {
		t.Fatal("HTTP error was accepted")
	}

	largeServer := responseServer(strings.Repeat("x", maxCompletionResponseBytes+1), http.StatusOK)
	defer largeServer.Close()
	largeAdapter, _ := NewOllamaAdapter(largeServer.URL+"/v1", http.DefaultClient)
	if _, err := largeAdapter.Complete(context.Background(), validCompletionRequest(ProviderOllama)); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("large response error = %v", err)
	}
}

func TestMessageValidation(t *testing.T) {
	t.Parallel()

	adapter, _ := NewOpenAIAdapter("https://api.openai.example/v1", "secret", doerFunc(func(*http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	}))
	request := validCompletionRequest(ProviderOpenAI)
	request.Messages = []Message{{Role: RoleTool, Content: stringPointer("result")}}
	if _, err := adapter.Complete(context.Background(), request); err == nil {
		t.Fatal("tool message without call ID was accepted")
	}
	request = validCompletionRequest(ProviderOpenAI)
	request.Messages = []Message{{Role: RoleUser}}
	if _, err := adapter.Complete(context.Background(), request); err == nil {
		t.Fatal("empty user message was accepted")
	}
}

func TestAdapterRejectsTrailingResponseData(t *testing.T) {
	t.Parallel()

	server := responseServer(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]} trailing`, http.StatusOK)
	defer server.Close()
	adapter, _ := NewOllamaAdapter(server.URL+"/v1", http.DefaultClient)
	if _, err := adapter.Complete(context.Background(), validCompletionRequest(ProviderOllama)); err == nil || !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("trailing response error = %v", err)
	}
}

func validCompletionRequest(provider Provider) CompletionRequest {
	return CompletionRequest{
		RequestID: "request-1", Model: ModelRef{Provider: provider, Model: "model"},
		Messages:        []Message{{Role: RoleSystem, Content: stringPointer("Generate a case.")}, {Role: RoleUser, Content: stringPointer("Cancel an order.")}},
		Tools:           []ToolDefinition{{Name: "create_test_case", Description: "Create a test case.", Parameters: json.RawMessage(`{"type":"object","properties":{"client_case_id":{"type":"string"}},"required":["client_case_id"]}`)}},
		ToolChoice:      ToolChoice{Mode: ToolChoiceRequired},
		ResponseSchema:  json.RawMessage(`{"type":"object","properties":{"status":{"type":"string"}},"required":["status"]}`),
		MaxOutputTokens: 256,
	}
}

func responseServer(body string, status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(status)
		_, _ = io.WriteString(writer, body)
	}))
}

func stringPointer(value string) *string { return &value }

type doerFunc func(*http.Request) (*http.Response, error)

func (function doerFunc) Do(request *http.Request) (*http.Response, error) { return function(request) }
