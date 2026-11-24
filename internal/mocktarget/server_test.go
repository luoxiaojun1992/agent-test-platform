package mocktarget

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agent-test-platform/contracts"
	"agent-test-platform/internal/agent"
)

func TestMockTargetEndToEnd(t *testing.T) {
	t.Parallel()

	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewServer(Config{BearerToken: "target-secret", ModelID: "mock-agent-v1", IndexDelay: 0}, validator)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	client, err := agent.NewSSEClientWithTimeouts(server.URL, "target-secret", http.DefaultClient, validator, agent.TargetTimeouts{
		MetadataRequest: time.Second, UploadRequest: time.Second, IndexWait: 5 * time.Second, PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	models, err := client.ListModels(context.Background())
	if err != nil || len(models.Models) != 1 || models.Models[0].ID != "mock-agent-v1" {
		t.Fatalf("models=%#v error=%v", models, err)
	}
	text := "Paid but unshipped orders may be cancelled."
	digestBytes := sha256.Sum256([]byte(text))
	digest := hex.EncodeToString(digestBytes[:])
	deadline := time.Now().UTC().Add(2 * time.Second)
	accepted, err := client.UploadKnowledge(context.Background(), agent.KnowledgeUploadRequest{
		SchemaVersion: "1.0", RequestID: mockRunID, Name: "policy.txt", Text: text,
		ContentSHA256: digest, IndexingDeadline: deadline, ExpiresAt: deadline.Add(time.Hour),
		Metadata: map[string]string{"source": "mock-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, err := client.WaitKnowledgeReady(context.Background(), accepted.DocumentID, digest)
	if err != nil || status.Status != "READY" {
		t.Fatalf("status=%#v error=%v", status, err)
	}
	catalog, err := client.ListAllTools(context.Background())
	if err != nil || len(catalog) != 2 || catalog[1].Name != "mysql.execute" {
		t.Fatalf("catalog=%#v error=%v", catalog, err)
	}
	result, err := client.Chat(context.Background(), agent.ChatRequest{
		SchemaVersion: "1.0", RunID: mockRunID, Model: "mock-agent-v1",
		Message:              agent.ChatMessage{ID: "msg-" + mockRunID, Role: "user", Content: "Cancel order O10001."},
		Deadline:             time.Now().Add(time.Minute),
		KnowledgeDocumentIDs: []string{accepted.DocumentID},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "COMPLETED" || result.FinalOutput == nil || !strings.Contains(*result.FinalOutput, "mock-agent-v1") || len(result.ToolCalls) != 1 {
		t.Fatalf("result = %#v", result)
	}
}

func TestMockTargetRejectsWrongModelAndUnreadyKnowledge(t *testing.T) {
	t.Parallel()

	validator, _ := contracts.NewValidator()
	handler, _ := NewServer(Config{BearerToken: "target-secret", ModelID: "mock-agent-v1", IndexDelay: time.Second}, validator)
	requestBody := map[string]any{
		"schema_version": "1.0", "run_id": mockRunID, "model": "unknown",
		"message":                map[string]any{"id": "msg-1", "role": "user", "content": "Hello"},
		"deadline":               time.Now().Add(time.Minute),
		"knowledge_document_ids": []string{},
	}
	encoded, _ := json.Marshal(requestBody)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat", strings.NewReader(string(encoded)))
	request.Header.Set("Authorization", "Bearer target-secret")
	request.Header.Set("X-ATP-Contract-Version", "1.0")
	request.Header.Set("Idempotency-Key", mockRunID)
	request.Header.Set("Accept", "text/event-stream")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "MODEL_NOT_FOUND") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

const mockRunID = "01K4Z6Y7X8W9V0T1S2R3Q4P5N6"
