package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"agent-test-platform/contracts"
)

func TestSSEClientStreamsAndNormalizesSession(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat" || request.Method != http.MethodPost {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer target-secret" || request.Header.Get("Idempotency-Key") != sseRunID {
			t.Errorf("authentication/idempotency headers are missing")
		}
		if request.Header.Get("Accept") != "text/event-stream" || request.Header.Get("X-ATP-Contract-Version") != "1.0" {
			t.Errorf("SSE contract headers are missing")
		}
		writer.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		flusher, ok := writer.(http.Flusher)
		if !ok {
			t.Fatal("response writer cannot flush")
		}
		for _, event := range validSSEEvents() {
			_, _ = fmt.Fprintf(writer, "event: %s\nid: %s\ndata: %s\n\n", event["type"], event["event_id"], mustEncode(event))
			flusher.Flush()
		}
	}))
	defer server.Close()

	client := newTestSSEClient(t, server.URL, "target-secret")
	var streamed []SessionEvent
	result, err := client.Chat(context.Background(), validChatRequest(), func(event SessionEvent) error {
		streamed = append(streamed, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "COMPLETED" || result.FinalOutput == nil || *result.FinalOutput != "Order cancelled." {
		t.Fatalf("result = %#v", result)
	}
	if len(streamed) != 6 || len(result.Events) != 6 || len(result.ToolCalls) != 1 || len(result.Evidence) != 1 {
		t.Fatalf("events=%d calls=%d evidence=%d", len(streamed), len(result.ToolCalls), len(result.Evidence))
	}
	if result.ToolCalls[0].ToolName != "mysql.execute" || result.ToolCalls[0].Result == nil || result.ToolCalls[0].Result.AffectedRows == nil || *result.ToolCalls[0].Result.AffectedRows != 1 {
		t.Fatalf("Tool call = %#v", result.ToolCalls[0])
	}
}

func TestSSEClientRejectsProtocolViolations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		events []map[string]any
		want   error
	}{
		{name: "sequence gap", events: mutateEvents(func(events []map[string]any) { events[1]["sequence"] = 3 }), want: ErrSSEContract},
		{name: "result without call", events: mutateEvents(func(events []map[string]any) { events = events[:0] }), want: ErrSSEContract},
		{name: "delta mismatch", events: mutateEvents(func(events []map[string]any) { events[5]["final_output"] = "different" }), want: ErrSSEContract},
		{name: "missing terminal", events: validSSEEvents()[:5], want: ErrSSETruncated},
	}
	// Replace the Tool call event while preserving a schema-valid stream.
	tests[1].events = []map[string]any{validSSEEvents()[0], validSSEEvents()[1], validSSEEvents()[3]}
	tests[1].events[2]["sequence"] = 3

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := sseServer(test.events)
			defer server.Close()
			client := newTestSSEClient(t, server.URL, "target-secret")
			_, err := client.Chat(context.Background(), validChatRequest(), nil)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestSSEClientRequiresBearerAndEventStream(t *testing.T) {
	t.Parallel()

	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSSEClient("https://target.example", "", http.DefaultClient, validator); err == nil {
		t.Fatal("empty Target Bearer token was accepted")
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{}`))
	}))
	defer server.Close()
	client := newTestSSEClient(t, server.URL, "target-secret")
	if _, err := client.Chat(context.Background(), validChatRequest(), nil); !errors.Is(err, ErrSSEContract) {
		t.Fatalf("content type error = %v", err)
	}
}

func validChatRequest() ChatRequest {
	return ChatRequest{
		SchemaVersion: "1.0", RunID: sseRunID, Model: "mock-agent-v1",
		Message:              ChatMessage{ID: "msg-" + sseRunID, Role: "user", Content: "Cancel order O10001."},
		Deadline:             time.Now().UTC().Add(time.Minute),
		KnowledgeDocumentIDs: []string{},
	}
}

func validSSEEvents() []map[string]any {
	createdAt := "2026-09-15T07:00:00Z"
	return []map[string]any{
		{"schema_version": "1.0", "event_id": "evt-1", "run_id": sseRunID, "sequence": 1, "type": "response.created", "created_at": createdAt},
		{"schema_version": "1.0", "event_id": "evt-2", "run_id": sseRunID, "sequence": 2, "type": "response.output_text.delta", "created_at": createdAt, "delta": "Order "},
		{"schema_version": "1.0", "event_id": "evt-3", "run_id": sseRunID, "sequence": 3, "type": "response.tool_call", "created_at": createdAt, "tool_sequence": 1, "call_id": "call-1", "parent_call_id": nil, "tool_name": "mysql.execute", "arguments": map[string]any{"sql": "UPDATE orders SET status = 'CANCELLED' WHERE id = 'O10001'"}},
		{"schema_version": "1.0", "event_id": "evt-4", "run_id": sseRunID, "sequence": 4, "type": "response.tool_result", "created_at": createdAt, "tool_sequence": 1, "call_id": "call-1", "duration_ms": 8, "result": map[string]any{"status": "SUCCESS", "output": map[string]any{"affected_rows": 1}, "affected_rows": 1, "truncated": false}},
		{"schema_version": "1.0", "event_id": "evt-5", "run_id": sseRunID, "sequence": 5, "type": "response.output_text.delta", "created_at": createdAt, "delta": "cancelled."},
		{"schema_version": "1.0", "event_id": "evt-6", "run_id": sseRunID, "sequence": 6, "type": "response.completed", "created_at": createdAt, "status": "COMPLETED", "session_id": "sess-1", "final_output": "Order cancelled.", "usage": map[string]any{"input_tokens": 10, "output_tokens": 4}},
	}
}

func mutateEvents(mutate func([]map[string]any)) []map[string]any {
	original := validSSEEvents()
	cloned := make([]map[string]any, len(original))
	for index, event := range original {
		encoded, _ := json.Marshal(event)
		_ = json.Unmarshal(encoded, &cloned[index])
	}
	mutate(cloned)
	return cloned
}

func sseServer(events []map[string]any) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		for _, event := range events {
			_, _ = fmt.Fprintf(writer, "event: %s\nid: %s\ndata: %s\n\n", event["type"], event["event_id"], mustEncode(event))
		}
	}))
}

func newTestSSEClient(t *testing.T, endpoint, token string) *SSEClient {
	t.Helper()
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewSSEClient(endpoint, token, http.DefaultClient, validator)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func mustEncode(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

const sseRunID = "01K4Z6Y7X8W9V0T1S2R3Q4P5N6"
