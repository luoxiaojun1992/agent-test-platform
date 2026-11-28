package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	targetModelListSchema         = "schemas/target-model-list-response.schema.json"
	targetToolListSchema          = "schemas/target-tool-list-response.schema.json"
	maxToolListPages              = 20
	targetKnowledgeUploadSchema   = "schemas/target-knowledge-upload-request.schema.json"
	targetKnowledgeAcceptedSchema = "schemas/target-knowledge-upload-response.schema.json"
	targetKnowledgeStatusSchema   = "schemas/target-knowledge-status-response.schema.json"
	targetSessionContextSchema    = "schemas/target-session-context-response.schema.json"
	maxKnowledgeUploadBytes       = 2 * 1024 * 1024
	maxSessionContextItems        = 100_000
)

var (
	ErrKnowledgeIndexFailed  = errors.New("Target knowledge indexing failed")
	ErrKnowledgeIndexTimeout = errors.New("Target knowledge indexing timed out")
)

type TargetModel struct {
	ID              string   `json:"id"`
	DisplayName     string   `json:"display_name"`
	Capabilities    []string `json:"capabilities"`
	ContextWindow   int      `json:"context_window,omitempty"`
	MaxOutputTokens int      `json:"max_output_tokens,omitempty"`
}

type ModelList struct {
	SchemaVersion string        `json:"schema_version"`
	Models        []TargetModel `json:"models"`
}

type KnowledgeUploadRequest struct {
	SchemaVersion    string            `json:"schema_version"`
	RequestID        string            `json:"request_id"`
	Name             string            `json:"name"`
	Text             string            `json:"text"`
	ContentSHA256    string            `json:"content_sha256"`
	IndexingDeadline time.Time         `json:"indexing_deadline"`
	ExpiresAt        time.Time         `json:"expires_at"`
	Metadata         map[string]string `json:"metadata"`
}

type KnowledgeAccepted struct {
	SchemaVersion    string    `json:"schema_version"`
	DocumentID       string    `json:"document_id"`
	Status           string    `json:"status"`
	ContentSHA256    string    `json:"content_sha256"`
	StatusURL        string    `json:"status_url"`
	AcceptedAt       time.Time `json:"accepted_at"`
	IndexingDeadline time.Time `json:"indexing_deadline"`
}

type KnowledgeStatus struct {
	SchemaVersion    string      `json:"schema_version"`
	DocumentID       string      `json:"document_id"`
	Status           string      `json:"status"`
	ContentSHA256    string      `json:"content_sha256"`
	IndexingDeadline time.Time   `json:"indexing_deadline"`
	IndexedAt        *time.Time  `json:"indexed_at"`
	Error            *AgentError `json:"error"`
}

func (client *SSEClient) ListModels(ctx context.Context) (ModelList, error) {
	requestContext, cancel := context.WithTimeout(ctx, client.timeouts.MetadataRequest)
	defer cancel()
	body, _, err := client.doJSON(requestContext, http.MethodGet, "/v1/models", "", nil, http.StatusOK, targetModelListSchema)
	if err != nil {
		return ModelList{}, err
	}
	var result ModelList
	if err := json.Unmarshal(body, &result); err != nil {
		return ModelList{}, fmt.Errorf("decode Target model list: %w", err)
	}
	seen := make(map[string]struct{}, len(result.Models))
	for _, model := range result.Models {
		if _, exists := seen[model.ID]; exists {
			return ModelList{}, fmt.Errorf("Target model list contains duplicate ID %q", model.ID)
		}
		seen[model.ID] = struct{}{}
	}
	return result, nil
}

func (client *SSEClient) UploadKnowledge(ctx context.Context, request KnowledgeUploadRequest) (KnowledgeAccepted, error) {
	if request.SchemaVersion == "" {
		request.SchemaVersion = "1.0"
	}
	if request.Metadata == nil {
		request.Metadata = map[string]string{}
	}
	digest := sha256.Sum256([]byte(request.Text))
	actualDigest := hex.EncodeToString(digest[:])
	if request.ContentSHA256 != actualDigest {
		return KnowledgeAccepted{}, fmt.Errorf("knowledge content SHA-256 does not match text")
	}
	now := time.Now()
	if !request.IndexingDeadline.After(now) || request.IndexingDeadline.After(now.Add(client.timeouts.IndexWait)) {
		return KnowledgeAccepted{}, fmt.Errorf("knowledge indexing deadline must be in the future and within the configured wait timeout")
	}
	if !request.ExpiresAt.After(request.IndexingDeadline) {
		return KnowledgeAccepted{}, fmt.Errorf("knowledge expiry must be after indexing deadline")
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return KnowledgeAccepted{}, fmt.Errorf("encode knowledge upload: %w", err)
	}
	if len(encoded) > maxKnowledgeUploadBytes {
		return KnowledgeAccepted{}, fmt.Errorf("knowledge upload exceeds %d bytes", maxKnowledgeUploadBytes)
	}
	if err := client.validator.ValidateJSON(targetKnowledgeUploadSchema, encoded); err != nil {
		return KnowledgeAccepted{}, fmt.Errorf("Target knowledge upload contract: %w", err)
	}
	requestContext, cancel := context.WithTimeout(ctx, client.timeouts.UploadRequest)
	defer cancel()
	body, _, err := client.doJSON(requestContext, http.MethodPost, "/v1/knowledge/documents", request.RequestID, encoded, http.StatusAccepted, targetKnowledgeAcceptedSchema)
	if err != nil {
		return KnowledgeAccepted{}, err
	}
	var accepted KnowledgeAccepted
	if err := json.Unmarshal(body, &accepted); err != nil {
		return KnowledgeAccepted{}, fmt.Errorf("decode knowledge upload response: %w", err)
	}
	if accepted.ContentSHA256 != request.ContentSHA256 || !accepted.IndexingDeadline.Equal(request.IndexingDeadline) {
		return KnowledgeAccepted{}, fmt.Errorf("Target knowledge upload response does not match request")
	}
	return accepted, nil
}

func (client *SSEClient) GetKnowledgeStatus(ctx context.Context, documentID string) (KnowledgeStatus, time.Duration, error) {
	if strings.TrimSpace(documentID) == "" {
		return KnowledgeStatus{}, 0, fmt.Errorf("knowledge document ID is required")
	}
	requestContext, cancel := context.WithTimeout(ctx, client.timeouts.MetadataRequest)
	defer cancel()
	path := "/v1/knowledge/documents/" + url.PathEscape(documentID)
	body, header, err := client.doJSON(requestContext, http.MethodGet, path, "", nil, http.StatusOK, targetKnowledgeStatusSchema)
	if err != nil {
		return KnowledgeStatus{}, 0, err
	}
	var status KnowledgeStatus
	if err := json.Unmarshal(body, &status); err != nil {
		return KnowledgeStatus{}, 0, fmt.Errorf("decode knowledge status: %w", err)
	}
	if status.DocumentID != documentID {
		return KnowledgeStatus{}, 0, fmt.Errorf("Target knowledge status document ID mismatch")
	}
	return status, retryAfter(header, client.timeouts.PollInterval), nil
}

// DeleteKnowledge removes one uploaded knowledge document from the Target.
func (client *SSEClient) DeleteKnowledge(ctx context.Context, documentID string) error {
	if strings.TrimSpace(documentID) == "" {
		return fmt.Errorf("knowledge document ID is required")
	}
	requestContext, cancel := context.WithTimeout(ctx, client.timeouts.MetadataRequest)
	defer cancel()
	path := "/v1/knowledge/documents/" + url.PathEscape(documentID)
	if _, _, err := client.doJSON(requestContext, http.MethodDelete, path, "", nil, http.StatusNoContent, ""); err != nil {
		return err
	}
	return nil
}

func (client *SSEClient) WaitKnowledgeReady(ctx context.Context, documentID, expectedDigest string) (KnowledgeStatus, error) {
	waitContext, cancel := context.WithTimeout(ctx, client.timeouts.IndexWait)
	defer cancel()
	for {
		status, delay, err := client.GetKnowledgeStatus(waitContext, documentID)
		if err != nil {
			if errors.Is(waitContext.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				return KnowledgeStatus{}, ErrKnowledgeIndexTimeout
			}
			return KnowledgeStatus{}, err
		}
		if status.ContentSHA256 != expectedDigest {
			return KnowledgeStatus{}, fmt.Errorf("Target knowledge status digest mismatch")
		}
		switch status.Status {
		case "READY":
			return status, nil
		case "FAILED":
			if status.Error != nil {
				return KnowledgeStatus{}, fmt.Errorf("%w: %s: %s", ErrKnowledgeIndexFailed, status.Error.Code, status.Error.Message)
			}
			return KnowledgeStatus{}, ErrKnowledgeIndexFailed
		case "QUEUED", "INDEXING":
		default:
			return KnowledgeStatus{}, fmt.Errorf("unknown Target knowledge status %q", status.Status)
		}
		if !status.IndexingDeadline.IsZero() && time.Now().After(status.IndexingDeadline) {
			return KnowledgeStatus{}, ErrKnowledgeIndexTimeout
		}
		timer := time.NewTimer(delay)
		select {
		case <-waitContext.Done():
			timer.Stop()
			if errors.Is(waitContext.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				return KnowledgeStatus{}, ErrKnowledgeIndexTimeout
			}
			return KnowledgeStatus{}, waitContext.Err()
		case <-timer.C:
		}
	}
}

func (client *SSEClient) doJSON(ctx context.Context, method, path, idempotencyKey string, body []byte, expectedStatus int, responseSchema string) ([]byte, http.Header, error) {
	endpoint := *client.endpoint
	if route, query, found := strings.Cut(path, "?"); found {
		path = route
		endpoint.RawQuery = query
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + path
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), reader)
	if err != nil {
		return nil, nil, fmt.Errorf("create Target API request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+client.token)
	request.Header.Set("X-ATP-Contract-Version", "1.0")
	request.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return nil, nil, fmt.Errorf("call Target API: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := readLimited(response.Body, maxKnowledgeUploadBytes)
	if err != nil {
		return nil, nil, err
	}
	if response.StatusCode != expectedStatus {
		message := strings.TrimSpace(string(responseBody))
		if len(message) > 2_000 {
			message = message[:2_000]
		}
		return nil, nil, &HTTPError{StatusCode: response.StatusCode, Body: message}
	}
	if responseSchema != "" {
		if err := client.validator.ValidateJSON(responseSchema, responseBody); err != nil {
			return nil, nil, fmt.Errorf("Target API response contract: %w", err)
		}
	}
	return responseBody, response.Header.Clone(), nil
}

func retryAfter(header http.Header, fallback time.Duration) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(header.Get("Retry-After")))
	if err != nil || seconds < 1 || seconds > 60 {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}

type ToolList struct {
	SchemaVersion string       `json:"schema_version"`
	Tools         []TargetTool `json:"tools"`
	NextCursor    *string      `json:"next_cursor"`
}

// ListTools fetches one page of the Tool definitions exposed by the Target Agent.
func (client *SSEClient) ListTools(ctx context.Context, cursor string, limit int) (ToolList, error) {
	query := url.Values{}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/tools"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	requestContext, cancel := context.WithTimeout(ctx, client.timeouts.MetadataRequest)
	defer cancel()
	body, _, err := client.doJSON(requestContext, http.MethodGet, path, "", nil, http.StatusOK, targetToolListSchema)
	if err != nil {
		return ToolList{}, err
	}
	var result ToolList
	if err := json.Unmarshal(body, &result); err != nil {
		return ToolList{}, fmt.Errorf("decode Target tool list: %w", err)
	}
	return result, nil
}

// ListAllTools follows next_cursor until the catalog is complete and rejects duplicate names.
func (client *SSEClient) ListAllTools(ctx context.Context) ([]TargetTool, error) {
	var all []TargetTool
	seen := map[string]struct{}{}
	cursor := ""
	for page := 0; page < maxToolListPages; page++ {
		result, err := client.ListTools(ctx, cursor, 100)
		if err != nil {
			return nil, err
		}
		for _, tool := range result.Tools {
			if _, exists := seen[tool.Name]; exists {
				return nil, fmt.Errorf("Target tool list contains duplicate name %q", tool.Name)
			}
			seen[tool.Name] = struct{}{}
			all = append(all, tool)
		}
		if result.NextCursor == nil {
			return all, nil
		}
		cursor = *result.NextCursor
	}
	return nil, fmt.Errorf("Target tool list exceeds %d pages", maxToolListPages)
}

// SessionContextItem is one item of a Target session's LLM context: a message, a Tool call, or a
// Tool result. Fields are populated by item type.
type SessionContextItem struct {
	Sequence  int             `json:"sequence"`
	Type      string          `json:"type"`
	Role      string          `json:"role,omitempty"`
	Content   string          `json:"content,omitempty"`
	ToolName  string          `json:"tool_name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Status    string          `json:"status,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     json.RawMessage `json:"error,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// SessionContextPage is one page of a Target session's LLM context.
type SessionContextPage struct {
	SchemaVersion string               `json:"schema_version"`
	SessionID     string               `json:"session_id"`
	Items         []SessionContextItem `json:"items"`
	NextCursor    *string              `json:"next_cursor"`
}

// GetSessionContext fetches one page of a Target session's LLM context, optionally bounded by
// untilSequence (only items with sequence <= untilSequence are returned).
func (client *SSEClient) GetSessionContext(ctx context.Context, sessionID, cursor string, limit, untilSequence int) (SessionContextPage, error) {
	if strings.TrimSpace(sessionID) == "" {
		return SessionContextPage{}, fmt.Errorf("session ID is required")
	}
	query := url.Values{}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	if untilSequence > 0 {
		query.Set("until_sequence", strconv.Itoa(untilSequence))
	}
	path := "/v1/sessions/" + url.PathEscape(sessionID) + "/context"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	requestContext, cancel := context.WithTimeout(ctx, client.timeouts.UploadRequest)
	defer cancel()
	body, _, err := client.doJSON(requestContext, http.MethodGet, path, "", nil, http.StatusOK, targetSessionContextSchema)
	if err != nil {
		return SessionContextPage{}, err
	}
	var page SessionContextPage
	if err := json.Unmarshal(body, &page); err != nil {
		return SessionContextPage{}, fmt.Errorf("decode session context: %w", err)
	}
	if page.SessionID != sessionID {
		return SessionContextPage{}, fmt.Errorf("Target session context ID mismatch")
	}
	return page, nil
}

// ListAllSessionContext pages through a session's context up to untilSequence (0 means the whole
// session) until the cursor is exhausted, bounded by maxItems.
func (client *SSEClient) ListAllSessionContext(ctx context.Context, sessionID string, maxItems, untilSequence int) ([]SessionContextItem, error) {
	if maxItems <= 0 {
		maxItems = maxSessionContextItems
	}
	var all []SessionContextItem
	lastSequence := 0
	cursor := ""
	for {
		page, err := client.GetSessionContext(ctx, sessionID, cursor, 100, untilSequence)
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			if item.Sequence <= lastSequence {
				return nil, fmt.Errorf("Target session context sequence not strictly increasing")
			}
			lastSequence = item.Sequence
			all = append(all, item)
			if len(all) > maxItems {
				return nil, fmt.Errorf("Target session context exceeds %d items", maxItems)
			}
		}
		if page.NextCursor == nil {
			return all, nil
		}
		cursor = *page.NextCursor
	}
}
