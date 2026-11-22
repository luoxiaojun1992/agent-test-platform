package mocktarget

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"agent-test-platform/contracts"
	"agent-test-platform/internal/agent"
)

const maxRequestBytes = 2 * 1024 * 1024

type Config struct {
	BearerToken string
	ModelID     string
	IndexDelay  time.Duration
}

type Server struct {
	config    Config
	validator *contracts.Validator
	mux       *http.ServeMux
	mu        sync.RWMutex
	documents map[string]document
	requests  map[string]string
}

type document struct {
	ID               string
	Name             string
	Text             string
	Digest           string
	AcceptedAt       time.Time
	IndexingDeadline time.Time
	ExpiresAt        time.Time
}

func NewServer(config Config, validator *contracts.Validator) (*Server, error) {
	if strings.TrimSpace(config.BearerToken) == "" || strings.TrimSpace(config.ModelID) == "" || config.IndexDelay < 0 || config.IndexDelay > time.Minute || validator == nil {
		return nil, fmt.Errorf("Mock Target token, model, index delay, and validator are required")
	}
	server := &Server{config: config, validator: validator, mux: http.NewServeMux(), documents: make(map[string]document), requests: make(map[string]string)}
	server.mux.HandleFunc("GET /healthz", server.health)
	server.mux.Handle("GET /v1/models", server.authenticate(http.HandlerFunc(server.listModels)))
	server.mux.Handle("GET /v1/tools", server.authenticate(http.HandlerFunc(server.listTools)))
	server.mux.Handle("POST /v1/knowledge/documents", server.authenticate(http.HandlerFunc(server.uploadKnowledge)))
	server.mux.Handle("GET /v1/knowledge/documents/{documentId}", server.authenticate(http.HandlerFunc(server.getKnowledgeStatus)))
	server.mux.Handle("POST /v1/chat", server.authenticate(http.HandlerFunc(server.chat)))
	return server, nil
}

func (server *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	server.mux.ServeHTTP(writer, request)
}

func (server *Server) health(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
}

func (server *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		provided := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		if len(provided) != len(server.config.BearerToken) || subtle.ConstantTimeCompare([]byte(provided), []byte(server.config.BearerToken)) != 1 {
			writeError(writer, http.StatusUnauthorized, "UNAUTHORIZED", "valid Target Bearer token required", false)
			return
		}
		if request.Header.Get("X-ATP-Contract-Version") != "1.0" {
			writeError(writer, http.StatusBadRequest, "CONTRACT_VERSION_INVALID", "X-ATP-Contract-Version must be 1.0", false)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func (server *Server) listModels(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{
		"schema_version": "1.0",
		"models": []any{map[string]any{
			"id": server.config.ModelID, "display_name": "Mock Target Agent",
			"capabilities":   []string{"CHAT", "SSE", "TOOLS", "KNOWLEDGE"},
			"context_window": 32768, "max_output_tokens": 4096,
		}},
	})
}

// listTools serves a two-page catalog so clients exercise cursor pagination.
func (server *Server) listTools(writer http.ResponseWriter, request *http.Request) {
	tools := []map[string]any{
		{"name": "mysql.query", "description": "Query MySQL.", "input_schema": map[string]any{"type": "object"}},
		{"name": "mysql.execute", "description": "Execute a MySQL statement.", "input_schema": map[string]any{"type": "object"}},
	}
	page, next := tools[:1], any(nil)
	if request.URL.Query().Get("cursor") == "" {
		next = "2"
	} else {
		page = tools[1:]
	}
	writeJSON(writer, http.StatusOK, map[string]any{"schema_version": "1.0", "tools": page, "next_cursor": next})
}

func (server *Server) uploadKnowledge(writer http.ResponseWriter, request *http.Request) {
	if strings.TrimSpace(request.Header.Get("Idempotency-Key")) == "" {
		writeError(writer, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key is required", false)
		return
	}
	body, err := readJSONBody(writer, request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "REQUEST_INVALID", err.Error(), false)
		return
	}
	if err := server.validator.ValidateJSON("schemas/target-knowledge-upload-request.schema.json", body); err != nil {
		writeError(writer, http.StatusBadRequest, "REQUEST_SCHEMA_INVALID", err.Error(), false)
		return
	}
	var input agent.KnowledgeUploadRequest
	if err := json.Unmarshal(body, &input); err != nil {
		writeError(writer, http.StatusBadRequest, "REQUEST_INVALID", err.Error(), false)
		return
	}
	digest := sha256.Sum256([]byte(input.Text))
	if hex.EncodeToString(digest[:]) != input.ContentSHA256 {
		writeError(writer, http.StatusBadRequest, "CONTENT_DIGEST_MISMATCH", "content_sha256 does not match text", false)
		return
	}
	now := time.Now().UTC()
	if !input.IndexingDeadline.After(now) || input.IndexingDeadline.After(now.Add(10*time.Minute)) || !input.ExpiresAt.After(input.IndexingDeadline) {
		writeError(writer, http.StatusBadRequest, "DEADLINE_INVALID", "indexing_deadline must be within 10 minutes and before expires_at", false)
		return
	}

	server.mu.Lock()
	documentID, exists := server.requests[input.RequestID]
	if !exists {
		documentID = "kb-" + strings.ToLower(input.RequestID)
		server.requests[input.RequestID] = documentID
		server.documents[documentID] = document{
			ID: documentID, Name: input.Name, Text: input.Text, Digest: input.ContentSHA256,
			AcceptedAt: time.Now().UTC(), IndexingDeadline: input.IndexingDeadline, ExpiresAt: input.ExpiresAt,
		}
	}
	stored := server.documents[documentID]
	server.mu.Unlock()
	if stored.Digest != input.ContentSHA256 {
		writeError(writer, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "request_id was reused with different text", false)
		return
	}
	writer.Header().Set("Retry-After", "1")
	writeJSON(writer, http.StatusAccepted, map[string]any{
		"schema_version": "1.0", "document_id": stored.ID, "status": server.documentStatus(stored, time.Now().UTC()),
		"content_sha256": stored.Digest, "status_url": "/v1/knowledge/documents/" + stored.ID,
		"accepted_at": stored.AcceptedAt, "indexing_deadline": stored.IndexingDeadline,
	})
}

func (server *Server) getKnowledgeStatus(writer http.ResponseWriter, request *http.Request) {
	id := request.PathValue("documentId")
	server.mu.RLock()
	stored, exists := server.documents[id]
	server.mu.RUnlock()
	if !exists {
		writeError(writer, http.StatusNotFound, "KNOWLEDGE_NOT_FOUND", "knowledge document was not found", false)
		return
	}
	now := time.Now().UTC()
	status := server.documentStatus(stored, now)
	var indexedAt *time.Time
	var statusError any
	if status == "READY" {
		value := stored.AcceptedAt.Add(server.config.IndexDelay)
		indexedAt = &value
	} else if status == "FAILED" {
		statusError = map[string]any{"code": "KNOWLEDGE_INDEX_TIMEOUT", "message": "Mock knowledge indexing deadline elapsed.", "retryable": false}
	}
	if status != "READY" {
		writer.Header().Set("Retry-After", "1")
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"schema_version": "1.0", "document_id": stored.ID, "status": status,
		"content_sha256": stored.Digest, "indexing_deadline": stored.IndexingDeadline,
		"indexed_at": indexedAt, "error": statusError,
	})
}

func (server *Server) chat(writer http.ResponseWriter, request *http.Request) {
	if strings.TrimSpace(request.Header.Get("Idempotency-Key")) == "" || request.Header.Get("Accept") != "text/event-stream" {
		writeError(writer, http.StatusBadRequest, "CHAT_HEADERS_INVALID", "Idempotency-Key and Accept: text/event-stream are required", false)
		return
	}
	body, err := readJSONBody(writer, request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "REQUEST_INVALID", err.Error(), false)
		return
	}
	if err := server.validator.ValidateJSON("schemas/target-chat-request.schema.json", body); err != nil {
		writeError(writer, http.StatusBadRequest, "REQUEST_SCHEMA_INVALID", err.Error(), false)
		return
	}
	var input agent.ChatRequest
	if err := json.Unmarshal(body, &input); err != nil {
		writeError(writer, http.StatusBadRequest, "REQUEST_INVALID", err.Error(), false)
		return
	}
	if input.Model != server.config.ModelID {
		writeError(writer, http.StatusBadRequest, "MODEL_NOT_FOUND", "requested model is not available", false)
		return
	}
	if time.Now().After(input.Deadline) {
		writeError(writer, http.StatusBadRequest, "DEADLINE_EXCEEDED", "chat deadline has elapsed", false)
		return
	}
	for _, documentID := range input.KnowledgeDocumentIDs {
		server.mu.RLock()
		stored, exists := server.documents[documentID]
		server.mu.RUnlock()
		if !exists || server.documentStatus(stored, time.Now().UTC()) != "READY" {
			writeError(writer, http.StatusConflict, "KNOWLEDGE_NOT_READY", "all knowledge documents must be READY", true)
			return
		}
	}

	writer.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeError(writer, http.StatusInternalServerError, "STREAM_UNAVAILABLE", "response writer does not support streaming", true)
		return
	}
	sequence := 1
	server.writeSSE(writer, flusher, input.RunID, sequence, "response.created", nil)
	sequence++
	if input.Tools != nil && len(*input.Tools) > 0 {
		tool := (*input.Tools)[0]
		server.writeSSE(writer, flusher, input.RunID, sequence, "response.tool_call", map[string]any{
			"tool_sequence": 1, "call_id": "mock-target-call-1", "parent_call_id": nil,
			"tool_name": tool.Name, "arguments": map[string]any{"input": input.Message.Content},
		})
		sequence++
		server.writeSSE(writer, flusher, input.RunID, sequence, "response.tool_result", map[string]any{
			"tool_sequence": 1, "call_id": "mock-target-call-1", "duration_ms": 1,
			"result": map[string]any{"status": "SUCCESS", "output": map[string]any{"mock": true}, "truncated": false},
		})
		sequence++
	}
	finalOutput := "Mock response from " + input.Model + ": " + input.Message.Content
	server.writeSSE(writer, flusher, input.RunID, sequence, "response.output_text.delta", map[string]any{"delta": finalOutput})
	sequence++
	server.writeSSE(writer, flusher, input.RunID, sequence, "response.completed", map[string]any{
		"status": "COMPLETED", "final_output": finalOutput,
		"usage": map[string]int{"input_tokens": 32, "output_tokens": 16},
	})
}

func (server *Server) writeSSE(writer http.ResponseWriter, flusher http.Flusher, runID string, sequence int, eventType string, fields map[string]any) {
	eventID := fmt.Sprintf("evt-%04d", sequence)
	payload := map[string]any{
		"schema_version": "1.0", "event_id": eventID, "run_id": runID,
		"sequence": sequence, "type": eventType, "created_at": time.Now().UTC(),
	}
	for key, value := range fields {
		payload[key] = value
	}
	encoded, _ := json.Marshal(payload)
	_, _ = fmt.Fprintf(writer, "event: %s\nid: %s\ndata: %s\n\n", eventType, eventID, encoded)
	flusher.Flush()
}

func (server *Server) documentStatus(stored document, now time.Time) string {
	if !now.Before(stored.ExpiresAt) {
		return "FAILED"
	}
	if !now.Before(stored.IndexingDeadline) {
		return "FAILED"
	}
	if now.Sub(stored.AcceptedAt) >= server.config.IndexDelay {
		return "READY"
	}
	if now.Sub(stored.AcceptedAt) >= server.config.IndexDelay/2 {
		return "INDEXING"
	}
	return "QUEUED"
}

func readJSONBody(writer http.ResponseWriter, request *http.Request) ([]byte, error) {
	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBytes)
	defer request.Body.Close()
	var document json.RawMessage
	decoder := json.NewDecoder(request.Body)
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("body contains trailing JSON data")
	}
	if !json.Valid(document) {
		return nil, fmt.Errorf("body must be one JSON document")
	}
	return document, nil
}

func writeError(writer http.ResponseWriter, status int, code, message string, retryable bool) {
	writeJSON(writer, status, map[string]any{"code": code, "message": message, "retryable": retryable})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
