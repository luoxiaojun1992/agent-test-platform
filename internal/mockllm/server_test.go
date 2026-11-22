package mockllm

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestServerReturnsStructuredScenario(t *testing.T) {
	t.Parallel()

	server, err := NewServer("llm-secret")
	if err != nil {
		t.Fatal(err)
	}
	schema := json.RawMessage(`{"title":"Business Scenario Revision Content","type":"object"}`)
	body := map[string]any{
		"model": "mock", "messages": []any{map[string]any{"role": "user", "content": "Generate."}},
		"response_format":       map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "structured_response", "strict": true, "schema": schema}},
		"max_completion_tokens": 2048, "stream": false,
	}
	response := performRequest(t, server, body, "llm-secret")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	var scenario map[string]any
	if len(result.Choices) != 1 || json.Unmarshal([]byte(result.Choices[0].Message.Content), &scenario) != nil || scenario["schema_version"] != "1.0" {
		t.Fatalf("result = %#v", result)
	}
}

func TestServerReturnsRequiredToolCall(t *testing.T) {
	t.Parallel()

	server, _ := NewServer("llm-secret")
	body := map[string]any{
		"model": "mock", "messages": []any{map[string]any{"role": "user", "content": "Call the Tool."}},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "probe", "parameters": map[string]any{"type": "object", "required": []string{"value"}, "properties": map[string]any{"value": map[string]any{"type": "string"}}},
		}}},
		"tool_choice": "required", "max_tokens": 64, "stream": false,
	}
	response := performRequest(t, server, body, "llm-secret")
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`"finish_reason":"tool_calls"`)) || !bytes.Contains(response.Body.Bytes(), []byte(`\"value\":\"mock-value\"`)) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestServerRejectsWrongBearer(t *testing.T) {
	t.Parallel()

	server, _ := NewServer("llm-secret")
	response := performRequest(t, server, map[string]any{"model": "mock", "messages": []any{}}, "wrong")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", response.Code)
	}
}

func performRequest(t *testing.T, handler http.Handler, body any, token string) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(encoded))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
