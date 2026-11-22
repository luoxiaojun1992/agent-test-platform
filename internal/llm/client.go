package llm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const maxCompletionResponseBytes = 2 * 1024 * 1024

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type Adapter struct {
	provider   Provider
	baseURL    *url.URL
	apiKey     string
	httpClient HTTPDoer
}

type HTTPError struct {
	Provider   Provider
	StatusCode int
	Body       string
}

func (err *HTTPError) Error() string {
	return fmt.Sprintf("%s HTTP status %d: %s", err.Provider, err.StatusCode, err.Body)
}

func NewOpenAIAdapter(baseURL, apiKey string, httpClient HTTPDoer) (*Adapter, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("OpenAI API key is required")
	}
	return newAdapter(ProviderOpenAI, baseURL, apiKey, httpClient)
}

func NewOllamaAdapter(baseURL string, httpClient HTTPDoer) (*Adapter, error) {
	return newAdapter(ProviderOllama, baseURL, "", httpClient)
}

func NewOllamaAdapterWithToken(baseURL, apiToken string, httpClient HTTPDoer) (*Adapter, error) {
	if strings.TrimSpace(apiToken) == "" {
		return nil, fmt.Errorf("Ollama API token is required")
	}
	return newAdapter(ProviderOllama, baseURL, apiToken, httpClient)
}

func newAdapter(provider Provider, baseURL, apiKey string, httpClient HTTPDoer) (*Adapter, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid %s base URL %q", provider, baseURL)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("LLM base URL cannot contain query or fragment")
	}
	if httpClient == nil {
		return nil, fmt.Errorf("HTTP client is required")
	}
	return &Adapter{provider: provider, baseURL: parsed, apiKey: apiKey, httpClient: httpClient}, nil
}

func (adapter *Adapter) Complete(ctx context.Context, request CompletionRequest) (CompletionResponse, error) {
	wireRequest, err := adapter.makeRequest(request)
	if err != nil {
		return CompletionResponse{}, err
	}
	body, err := json.Marshal(wireRequest)
	if err != nil {
		return CompletionResponse{}, fmt.Errorf("encode completion request: %w", err)
	}

	endpoint := *adapter.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/chat/completions"
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return CompletionResponse{}, fmt.Errorf("create completion request: %w", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	if request.RequestID != "" {
		httpRequest.Header.Set("X-Client-Request-Id", request.RequestID)
	}
	if adapter.apiKey != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+adapter.apiKey)
	}

	httpResponse, err := adapter.httpClient.Do(httpRequest)
	if err != nil {
		return CompletionResponse{}, fmt.Errorf("call %s: %w", adapter.provider, err)
	}
	defer httpResponse.Body.Close()
	responseBody, err := readResponse(httpResponse.Body)
	if err != nil {
		return CompletionResponse{}, err
	}
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		message := strings.TrimSpace(string(responseBody))
		if len(message) > 2_000 {
			message = message[:2_000]
		}
		return CompletionResponse{}, &HTTPError{Provider: adapter.provider, StatusCode: httpResponse.StatusCode, Body: message}
	}
	return adapter.parseResponse(request.Model, responseBody)
}

type wireRequest struct {
	Model               string          `json:"model"`
	Messages            []wireMessage   `json:"messages"`
	Tools               []wireTool      `json:"tools,omitempty"`
	ToolChoice          any             `json:"tool_choice,omitempty"`
	ResponseFormat      *responseFormat `json:"response_format,omitempty"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	MaxTokens           int             `json:"max_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	Seed                *int64          `json:"seed,omitempty"`
	Stream              bool            `json:"stream"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
}

type wireTool struct {
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type responseFormat struct {
	Type       string         `json:"type"`
	JSONSchema wireJSONSchema `json:"json_schema"`
}

type wireJSONSchema struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

func (adapter *Adapter) makeRequest(request CompletionRequest) (wireRequest, error) {
	if request.Model.Provider != adapter.provider || strings.TrimSpace(request.Model.Model) == "" {
		return wireRequest{}, fmt.Errorf("model provider/name does not match %s adapter", adapter.provider)
	}
	if len(request.Messages) == 0 || request.MaxOutputTokens < 1 {
		return wireRequest{}, fmt.Errorf("messages and positive max output tokens are required")
	}
	wire := wireRequest{Model: request.Model.Model, Temperature: request.Temperature, Seed: request.Seed, Stream: false}
	if adapter.provider == ProviderOpenAI {
		wire.MaxCompletionTokens = request.MaxOutputTokens
	} else {
		wire.MaxTokens = request.MaxOutputTokens
	}
	for _, message := range request.Messages {
		converted, err := convertMessage(message)
		if err != nil {
			return wireRequest{}, err
		}
		wire.Messages = append(wire.Messages, converted)
	}
	seenTools := make(map[string]struct{}, len(request.Tools))
	for _, tool := range request.Tools {
		if strings.TrimSpace(tool.Name) == "" || !isJSONObject(tool.Parameters) {
			return wireRequest{}, fmt.Errorf("tool name and object parameter schema are required")
		}
		if _, exists := seenTools[tool.Name]; exists {
			return wireRequest{}, fmt.Errorf("duplicate tool %q", tool.Name)
		}
		seenTools[tool.Name] = struct{}{}
		wire.Tools = append(wire.Tools, wireTool{Type: "function", Function: wireFunction(tool)})
	}
	choice, err := convertToolChoice(request.ToolChoice, seenTools)
	if err != nil {
		return wireRequest{}, err
	}
	wire.ToolChoice = choice
	if len(request.ResponseSchema) > 0 {
		if !isJSONObject(request.ResponseSchema) {
			return wireRequest{}, fmt.Errorf("response schema must be a JSON object")
		}
		wire.ResponseFormat = &responseFormat{Type: "json_schema", JSONSchema: wireJSONSchema{Name: "structured_response", Strict: true, Schema: request.ResponseSchema}}
	}
	return wire, nil
}

func convertMessage(message Message) (wireMessage, error) {
	if message.Role != RoleSystem && message.Role != RoleUser && message.Role != RoleAssistant && message.Role != RoleTool {
		return wireMessage{}, fmt.Errorf("invalid message role %q", message.Role)
	}
	if message.Role == RoleTool && (message.ToolCallID == "" || message.Content == nil) {
		return wireMessage{}, fmt.Errorf("tool message requires tool call ID and content")
	}
	if (message.Role == RoleSystem || message.Role == RoleUser) && (message.Content == nil || strings.TrimSpace(*message.Content) == "") {
		return wireMessage{}, fmt.Errorf("%s message requires content", message.Role)
	}
	if message.Role == RoleAssistant && message.Content == nil && len(message.ToolCalls) == 0 {
		return wireMessage{}, fmt.Errorf("assistant message requires content or tool calls")
	}
	if message.Role != RoleAssistant && len(message.ToolCalls) > 0 {
		return wireMessage{}, fmt.Errorf("only assistant messages may contain tool calls")
	}
	wire := wireMessage{Role: string(message.Role), Content: message.Content, ToolCallID: message.ToolCallID}
	for _, call := range message.ToolCalls {
		if call.ID == "" || call.Name == "" || !isJSONObject(call.Arguments) {
			return wireMessage{}, fmt.Errorf("assistant tool call requires ID, name, and object arguments")
		}
		converted := wireToolCall{ID: call.ID, Type: "function"}
		converted.Function.Name = call.Name
		converted.Function.Arguments = string(call.Arguments)
		wire.ToolCalls = append(wire.ToolCalls, converted)
	}
	return wire, nil
}

func convertToolChoice(choice ToolChoice, tools map[string]struct{}) (any, error) {
	switch choice.Mode {
	case "", ToolChoiceNone:
		return "none", nil
	case ToolChoiceAuto:
		return "auto", nil
	case ToolChoiceRequired:
		if len(tools) == 0 {
			return nil, fmt.Errorf("required tool choice needs at least one tool")
		}
		return "required", nil
	case ToolChoiceNamed:
		if _, ok := tools[choice.Name]; !ok {
			return nil, fmt.Errorf("named tool %q is not defined", choice.Name)
		}
		return map[string]any{"type": "function", "function": map[string]any{"name": choice.Name}}, nil
	default:
		return nil, fmt.Errorf("invalid tool choice %q", choice.Mode)
	}
}

type wireResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message      wireMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
	} `json:"usage"`
}

func (adapter *Adapter) parseResponse(requested ModelRef, body []byte) (CompletionResponse, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var wire wireResponse
	if err := decoder.Decode(&wire); err != nil {
		return CompletionResponse{}, fmt.Errorf("decode %s response: %w", adapter.provider, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return CompletionResponse{}, fmt.Errorf("decode %s response: trailing data", adapter.provider)
	}
	if len(wire.Choices) != 1 {
		return CompletionResponse{}, fmt.Errorf("%s returned %d choices, want 1", adapter.provider, len(wire.Choices))
	}
	choice := wire.Choices[0]
	response := CompletionResponse{ProviderRequestID: wire.ID, Model: requested, StopReason: choice.FinishReason}
	if wire.Model != "" {
		response.Model.Model = wire.Model
	}
	if choice.Message.Content != nil {
		response.Content = *choice.Message.Content
	}
	seenCalls := make(map[string]struct{}, len(choice.Message.ToolCalls))
	for _, call := range choice.Message.ToolCalls {
		arguments := json.RawMessage(call.Function.Arguments)
		if call.ID == "" || call.Function.Name == "" || !isJSONObject(arguments) {
			return CompletionResponse{}, fmt.Errorf("%s returned malformed tool call", adapter.provider)
		}
		if _, exists := seenCalls[call.ID]; exists {
			return CompletionResponse{}, fmt.Errorf("%s returned duplicate tool call ID %q", adapter.provider, call.ID)
		}
		seenCalls[call.ID] = struct{}{}
		response.ToolCalls = append(response.ToolCalls, ModelToolCall{ID: call.ID, Name: call.Function.Name, Arguments: arguments})
	}
	if choice.FinishReason == "tool_calls" && len(response.ToolCalls) == 0 {
		return CompletionResponse{}, fmt.Errorf("%s finish_reason=tool_calls without calls", adapter.provider)
	}
	if wire.Usage != nil {
		response.Usage = TokenUsage{InputTokens: wire.Usage.PromptTokens, OutputTokens: wire.Usage.CompletionTokens, TotalTokens: wire.Usage.TotalTokens, Available: true}
	}
	hash := sha256.Sum256(body)
	response.RawDigest = hex.EncodeToString(hash[:])
	return response, nil
}

func isJSONObject(data []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil {
		return false
	}
	var trailing any
	return decoder.Decode(&trailing) == io.EOF
}

func readResponse(reader io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxCompletionResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read completion response: %w", err)
	}
	if len(data) > maxCompletionResponseBytes {
		return nil, fmt.Errorf("completion response exceeds %d bytes", maxCompletionResponseBytes)
	}
	return data, nil
}
