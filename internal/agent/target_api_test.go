package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agent-test-platform/contracts"
)

func TestTargetModelAndKnowledgeAPIs(t *testing.T) {
	t.Parallel()

	text := "Paid but unshipped orders may be cancelled."
	digestBytes := sha256.Sum256([]byte(text))
	digest := hex.EncodeToString(digestBytes[:])
	deadline := time.Now().UTC().Add(2 * time.Second)
	expiresAt := deadline.Add(time.Hour)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer target-secret" || request.Header.Get("X-ATP-Contract-Version") != "1.0" {
			t.Errorf("Target headers are missing")
		}
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/v1/models":
			writeTargetJSON(writer, http.StatusOK, map[string]any{
				"schema_version": "1.0", "models": []any{map[string]any{
					"id": "mock-agent-v1", "display_name": "Mock Agent v1", "capabilities": []string{"CHAT", "SSE", "KNOWLEDGE"},
				}},
			})
		case request.Method == http.MethodPost && request.URL.Path == "/v1/knowledge/documents":
			if request.Header.Get("Idempotency-Key") != sseRunID {
				t.Errorf("idempotency key = %q", request.Header.Get("Idempotency-Key"))
			}
			writeTargetJSON(writer, http.StatusAccepted, map[string]any{
				"schema_version": "1.0", "document_id": "doc-1", "status": "QUEUED", "content_sha256": digest,
				"status_url": "/v1/knowledge/documents/doc-1", "accepted_at": time.Now().UTC(), "indexing_deadline": deadline,
			})
		case request.Method == http.MethodGet && request.URL.Path == "/v1/knowledge/documents/doc-1":
			writeTargetJSON(writer, http.StatusOK, map[string]any{
				"schema_version": "1.0", "document_id": "doc-1", "status": "READY", "content_sha256": digest,
				"indexing_deadline": deadline, "indexed_at": time.Now().UTC(), "error": nil,
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client := newTargetClientWithTimeouts(t, server.URL, TargetTimeouts{
		MetadataRequest: time.Second, UploadRequest: time.Second, IndexWait: 5 * time.Second, PollInterval: time.Millisecond,
	})

	models, err := client.ListModels(context.Background())
	if err != nil || len(models.Models) != 1 || models.Models[0].ID != "mock-agent-v1" {
		t.Fatalf("models=%#v error=%v", models, err)
	}
	accepted, err := client.UploadKnowledge(context.Background(), KnowledgeUploadRequest{
		SchemaVersion: "1.0", RequestID: sseRunID, Name: "policy.txt", Text: text,
		ContentSHA256: digest, IndexingDeadline: deadline, ExpiresAt: expiresAt,
		Metadata: map[string]string{"source": "test"},
	})
	if err != nil || accepted.DocumentID != "doc-1" {
		t.Fatalf("accepted=%#v error=%v", accepted, err)
	}
	status, err := client.WaitKnowledgeReady(context.Background(), accepted.DocumentID, digest)
	if err != nil || status.Status != "READY" {
		t.Fatalf("status=%#v error=%v", status, err)
	}
}

func TestKnowledgeUploadRequestTimeout(t *testing.T) {
	t.Parallel()

	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewSSEClientWithTimeouts("https://target.example", "target-secret", targetDoerFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	}), validator, TargetTimeouts{
		MetadataRequest: time.Second, UploadRequest: 10 * time.Millisecond, IndexWait: time.Second, PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := "knowledge"
	digest := sha256.Sum256([]byte(text))
	_, err = client.UploadKnowledge(context.Background(), KnowledgeUploadRequest{
		SchemaVersion: "1.0", RequestID: sseRunID, Name: "knowledge.txt", Text: text,
		ContentSHA256: hex.EncodeToString(digest[:]), IndexingDeadline: time.Now().Add(500 * time.Millisecond),
		ExpiresAt: time.Now().Add(time.Hour), Metadata: map[string]string{},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
}

func TestKnowledgeIndexWaitTimeout(t *testing.T) {
	t.Parallel()

	digest := strings.Repeat("a", 64)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeTargetJSON(writer, http.StatusOK, map[string]any{
			"schema_version": "1.0", "document_id": "doc-1", "status": "INDEXING", "content_sha256": digest,
			"indexing_deadline": time.Now().Add(time.Hour).UTC(), "indexed_at": nil, "error": nil,
		})
	}))
	defer server.Close()
	client := newTargetClientWithTimeouts(t, server.URL, TargetTimeouts{
		MetadataRequest: time.Second, UploadRequest: time.Second, IndexWait: 15 * time.Millisecond, PollInterval: time.Millisecond,
	})
	_, err := client.WaitKnowledgeReady(context.Background(), "doc-1", digest)
	if !errors.Is(err, ErrKnowledgeIndexTimeout) {
		t.Fatalf("error = %v", err)
	}
}

func TestKnowledgeUploadRejectsDigestMismatch(t *testing.T) {
	t.Parallel()

	client := newTargetClientWithTimeouts(t, "https://target.example", DefaultTargetTimeouts())
	_, err := client.UploadKnowledge(context.Background(), KnowledgeUploadRequest{
		SchemaVersion: "1.0", RequestID: sseRunID, Name: "knowledge.txt", Text: "knowledge",
		ContentSHA256: strings.Repeat("a", 64), IndexingDeadline: time.Now().Add(time.Minute),
		ExpiresAt: time.Now().Add(time.Hour), Metadata: map[string]string{},
	})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error = %v", err)
	}
}

func newTargetClientWithTimeouts(t *testing.T, endpoint string, timeouts TargetTimeouts) *SSEClient {
	t.Helper()
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewSSEClientWithTimeouts(endpoint, "target-secret", http.DefaultClient, validator, timeouts)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func writeTargetJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

type targetDoerFunc func(*http.Request) (*http.Response, error)

func (function targetDoerFunc) Do(request *http.Request) (*http.Response, error) {
	return function(request)
}
