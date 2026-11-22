package mockllm

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"agent-test-platform/contracts"
)

const maxRequestBytes = 2 * 1024 * 1024

type Server struct {
	token string
	mux   *http.ServeMux
}

type completionRequest struct {
	Model          string            `json:"model"`
	Messages       []json.RawMessage `json:"messages"`
	Tools          []wireTool        `json:"tools"`
	ToolChoice     any               `json:"tool_choice"`
	ResponseFormat *struct {
		Type       string `json:"type"`
		JSONSchema struct {
			Name   string          `json:"name"`
			Strict bool            `json:"strict"`
			Schema json.RawMessage `json:"schema"`
		} `json:"json_schema"`
	} `json:"response_format"`
	MaxCompletionTokens int      `json:"max_completion_tokens"`
	MaxTokens           int      `json:"max_tokens"`
	Temperature         *float64 `json:"temperature"`
	Seed                *int64   `json:"seed"`
	Stream              bool     `json:"stream"`
}

type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

func NewServer(bearerToken string) (*Server, error) {
	if strings.TrimSpace(bearerToken) == "" {
		return nil, fmt.Errorf("Mock LLM Bearer token is required")
	}
	server := &Server{token: bearerToken, mux: http.NewServeMux()}
	server.mux.HandleFunc("GET /healthz", server.health)
	server.mux.HandleFunc("POST /v1/chat/completions", server.complete)
	return server, nil
}

func (server *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	server.mux.ServeHTTP(writer, request)
}

func (server *Server) health(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
}

func (server *Server) complete(writer http.ResponseWriter, request *http.Request) {
	provided := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
	if len(provided) != len(server.token) || subtle.ConstantTimeCompare([]byte(provided), []byte(server.token)) != 1 {
		writeJSON(writer, http.StatusUnauthorized, map[string]any{"error": map[string]string{"message": "invalid Mock LLM token", "type": "authentication_error"}})
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var input completionRequest
	if err := decoder.Decode(&input); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"error": map[string]string{"message": err.Error(), "type": "invalid_request_error"}})
		return
	}
	if input.Model == "" || len(input.Messages) == 0 || input.Stream {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"error": map[string]string{"message": "model, messages, and stream=false are required", "type": "invalid_request_error"}})
		return
	}
	if requestContains(input.Messages, "[mock:http-500]") {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"error": map[string]string{"message": "injected Mock LLM failure", "type": "server_error"}})
		return
	}

	message := map[string]any{"role": "assistant", "content": "Mock completion."}
	finishReason := "stop"
	if name, arguments, ok, err := scriptedToolCall(input); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"error": map[string]string{"message": err.Error(), "type": "invalid_request_error"}})
		return
	} else if ok {
		encoded, _ := json.Marshal(arguments)
		message = map[string]any{
			"role": "assistant", "content": nil,
			"tool_calls": []any{map[string]any{
				"id": "mock-call-" + fmt.Sprint(countToolResults(decodeMessages(input.Messages))+1), "type": "function",
				"function": map[string]any{"name": name, "arguments": string(encoded)},
			}},
		}
		finishReason = "tool_calls"
	} else if toolName, parameters, ok := selectedTool(input); ok {
		arguments, err := mockValue(parameters)
		if err != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"error": map[string]string{"message": err.Error(), "type": "invalid_tool_schema"}})
			return
		}
		encoded, _ := json.Marshal(arguments)
		message = map[string]any{
			"role": "assistant", "content": nil,
			"tool_calls": []any{map[string]any{
				"id": "mock-call-1", "type": "function",
				"function": map[string]any{"name": toolName, "arguments": string(encoded)},
			}},
		}
		finishReason = "tool_calls"
	} else if input.ResponseFormat != nil && len(input.ResponseFormat.JSONSchema.Schema) > 0 {
		content, err := structuredContent(input.ResponseFormat.JSONSchema.Schema, input.Messages)
		if err != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"error": map[string]string{"message": err.Error(), "type": "invalid_response_schema"}})
			return
		}
		message["content"] = string(content)
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"id": "mock-response-1", "object": "chat.completion", "model": input.Model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finishReason}},
		"usage":   map[string]int{"prompt_tokens": 64, "completion_tokens": 32, "total_tokens": 96},
	})
}

func selectedTool(request completionRequest) (string, json.RawMessage, bool) {
	if len(request.Tools) == 0 {
		return "", nil, false
	}
	switch choice := request.ToolChoice.(type) {
	case string:
		if choice != "required" {
			return "", nil, false
		}
		return request.Tools[0].Function.Name, request.Tools[0].Function.Parameters, true
	case map[string]any:
		function, _ := choice["function"].(map[string]any)
		name, _ := function["name"].(string)
		for _, tool := range request.Tools {
			if tool.Function.Name == name {
				return name, tool.Function.Parameters, true
			}
		}
	}
	return "", nil, false
}

func structuredContent(schema json.RawMessage, messages []json.RawMessage) (json.RawMessage, error) {
	var document map[string]any
	if err := json.Unmarshal(schema, &document); err != nil {
		return nil, err
	}
	title, _ := document["title"].(string)
	if isDatasetSchema(document) {
		return scriptedDataset()
	}
	switch title {
	case "Business Scenario Revision Content":
		return contracts.Files.ReadFile("examples/scenario.json")
	case "Generation Semantic Critic Result":
		return contracts.Files.ReadFile("examples/critic-result.json")
	case "LLM Judge Result":
		return mockJudgeResult(messages)
	default:
		value, err := mockValue(schema)
		if err != nil {
			return nil, err
		}
		return json.Marshal(value)
	}
}

func mockJudgeResult(messages []json.RawMessage) (json.RawMessage, error) {
	joined := ""
	for _, message := range messages {
		joined += string(message)
	}
	matches := regexp.MustCompile(`\\?"name\\?"\s*:\s*\\?"([a-z][a-z0-9_]*)\\?"`).FindAllStringSubmatch(joined, -1)
	seen := make(map[string]struct{})
	criteria := make([]map[string]any, 0, len(matches))
	for _, match := range matches {
		if _, exists := seen[match[1]]; exists {
			continue
		}
		seen[match[1]] = struct{}{}
		criteria = append(criteria, map[string]any{"criterion_id": match[1], "score": 100, "reason": "Mock Judge accepted the observable evidence."})
	}
	if len(criteria) == 0 {
		criteria = append(criteria, map[string]any{"criterion_id": "outcome_correctness", "score": 100, "reason": "Mock Judge accepted the observable evidence."})
	}
	return json.Marshal(map[string]any{
		"schema_version": "1.0", "criteria": criteria, "tool_findings": []any{},
		"failure_categories": []any{}, "confidence": 1, "overall_reason": "Deterministic Mock Judge result.",
	})
}

func mockValue(schema json.RawMessage) (any, error) {
	var document map[string]any
	if err := json.Unmarshal(schema, &document); err != nil {
		return nil, err
	}
	return mockSchemaNode(document), nil
}

func mockSchemaNode(schema map[string]any) any {
	if value, exists := schema["const"]; exists {
		return value
	}
	if values, ok := schema["enum"].([]any); ok && len(values) > 0 {
		return values[0]
	}
	typeName, _ := schema["type"].(string)
	switch typeName {
	case "object", "":
		result := make(map[string]any)
		properties, _ := schema["properties"].(map[string]any)
		required, _ := schema["required"].([]any)
		for _, nameValue := range required {
			name, _ := nameValue.(string)
			property, _ := properties[name].(map[string]any)
			result[name] = mockSchemaNode(property)
		}
		return result
	case "array":
		return []any{}
	case "integer", "number":
		return float64(1)
	case "boolean":
		return true
	case "null":
		return nil
	default:
		return "mock-value"
	}
}

func requestContains(messages []json.RawMessage, needle string) bool {
	for _, message := range messages {
		if strings.Contains(string(message), needle) {
			return true
		}
	}
	return false
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
