package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"agent-test-platform/contracts"
	"agent-test-platform/internal/evidence"
	"agent-test-platform/internal/toolmatch"
)

const (
	targetChatRequestSchema = "schemas/target-chat-request.schema.json"
	targetChatEventSchema   = "schemas/target-chat-event.schema.json"
	maxSSEEventBytes        = 256 * 1024
	maxSSEResponseBytes     = 2 * 1024 * 1024
	maxOutputCharacters     = 16_000
)

var (
	ErrSSEContract  = errors.New("Target SSE contract violation")
	ErrSSETruncated = errors.New("Target SSE stream ended before a terminal event")
)

type SSEClient struct {
	endpoint   *url.URL
	token      string
	httpClient HTTPDoer
	validator  *contracts.Validator
	timeouts   TargetTimeouts
}

type TargetTimeouts struct {
	MetadataRequest time.Duration
	UploadRequest   time.Duration
	IndexWait       time.Duration
	PollInterval    time.Duration
}

func DefaultTargetTimeouts() TargetTimeouts {
	return TargetTimeouts{
		MetadataRequest: 10 * time.Second,
		UploadRequest:   30 * time.Second,
		IndexWait:       10 * time.Minute,
		PollInterval:    2 * time.Second,
	}
}

type ChatRequest struct {
	SchemaVersion        string        `json:"schema_version"`
	RunID                string        `json:"run_id"`
	Model                string        `json:"model"`
	Mode                 string        `json:"mode"`
	Message              ChatMessage   `json:"message"`
	Deadline             time.Time     `json:"deadline"`
	Tools                *[]TargetTool `json:"tools"`
	KnowledgeDocumentIDs []string      `json:"knowledge_document_ids"`
}

type ChatMessage struct {
	ID      string `json:"id"`
	Role    string `json:"role"`
	Content string `json:"content"`
}

type TargetTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type SessionEvent struct {
	SchemaVersion string          `json:"schema_version"`
	EventID       string          `json:"event_id"`
	RunID         string          `json:"run_id"`
	Sequence      int             `json:"sequence"`
	Type          string          `json:"type"`
	CreatedAt     time.Time       `json:"created_at"`
	Raw           json.RawMessage `json:"-"`
}

type ChatResult struct {
	RunID       string
	Status      string
	FinalOutput *string
	ToolCalls   []ToolCall
	Usage       *Usage
	Error       *AgentError
	Events      []SessionEvent
	Normalized  []toolmatch.Call
	Evidence    []evidence.Record
}

type EventHandler func(SessionEvent) error

func NewSSEClient(endpoint, targetBearerToken string, httpClient HTTPDoer, validator *contracts.Validator) (*SSEClient, error) {
	return NewSSEClientWithTimeouts(endpoint, targetBearerToken, httpClient, validator, DefaultTargetTimeouts())
}

func NewSSEClientWithTimeouts(endpoint, targetBearerToken string, httpClient HTTPDoer, validator *contracts.Validator, timeouts TargetTimeouts) (*SSEClient, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid Target chat endpoint %q", endpoint)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("Target chat endpoint cannot contain query or fragment")
	}
	if strings.TrimSpace(targetBearerToken) == "" || httpClient == nil || validator == nil {
		return nil, fmt.Errorf("Target Bearer token, HTTP client, and contract validator are required")
	}
	if err := timeouts.validate(); err != nil {
		return nil, err
	}
	return &SSEClient{endpoint: parsed, token: targetBearerToken, httpClient: httpClient, validator: validator, timeouts: timeouts}, nil
}

func (client *SSEClient) Chat(ctx context.Context, request ChatRequest, onEvent EventHandler) (ChatResult, error) {
	if request.KnowledgeDocumentIDs == nil {
		request.KnowledgeDocumentIDs = []string{}
	}
	body, err := json.Marshal(request)
	if err != nil {
		return ChatResult{}, fmt.Errorf("encode Target chat request: %w", err)
	}
	if len(body) > maxRequestBytes {
		return ChatResult{}, fmt.Errorf("Target chat request exceeds %d bytes", maxRequestBytes)
	}
	if err := client.validator.ValidateJSON(targetChatRequestSchema, body); err != nil {
		return ChatResult{}, fmt.Errorf("Target chat request contract: %w", err)
	}

	chatURL := *client.endpoint
	chatURL.Path = strings.TrimRight(chatURL.Path, "/") + "/v1/chat"
	chatContext, cancelChat := context.WithDeadline(ctx, request.Deadline)
	defer cancelChat()
	httpRequest, err := http.NewRequestWithContext(chatContext, http.MethodPost, chatURL.String(), bytes.NewReader(body))
	if err != nil {
		return ChatResult{}, fmt.Errorf("create Target chat request: %w", err)
	}
	httpRequest.Header.Set("Authorization", "Bearer "+client.token)
	httpRequest.Header.Set("Idempotency-Key", request.RunID)
	httpRequest.Header.Set("X-ATP-Contract-Version", "1.0")
	httpRequest.Header.Set("Content-Type", "application/json; charset=utf-8")
	httpRequest.Header.Set("Accept", "text/event-stream")
	httpRequest.Header.Set("Cache-Control", "no-cache")

	httpResponse, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return ChatResult{}, fmt.Errorf("execute Target chat request: %w", err)
	}
	defer httpResponse.Body.Close()
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		responseBody, readErr := readLimited(httpResponse.Body, 2_000)
		if readErr != nil {
			return ChatResult{}, readErr
		}
		return ChatResult{}, &HTTPError{StatusCode: httpResponse.StatusCode, Body: strings.TrimSpace(string(responseBody))}
	}
	mediaType, _, err := mime.ParseMediaType(httpResponse.Header.Get("Content-Type"))
	if err != nil || mediaType != "text/event-stream" {
		return ChatResult{}, fmt.Errorf("%w: Content-Type must be text/event-stream", ErrSSEContract)
	}

	state := newSSEState(request.RunID, onEvent, client.validator)
	limited := &countingReader{reader: httpResponse.Body, remaining: maxSSEResponseBytes}
	if err := parseSSE(limited, state.accept); err != nil {
		return ChatResult{}, err
	}
	if !state.terminal {
		return ChatResult{}, ErrSSETruncated
	}
	if state.pendingCalls() != 0 {
		return ChatResult{}, fmt.Errorf("%w: terminal event has unresolved Tool calls", ErrSSEContract)
	}
	normalized, records, err := normalizeToolCalls(state.result.ToolCalls)
	if err != nil {
		return ChatResult{}, fmt.Errorf("normalize Target Tool calls: %w", err)
	}
	state.result.Normalized = normalized
	state.result.Evidence = records
	return state.result, nil
}

func (timeouts TargetTimeouts) validate() error {
	if timeouts.MetadataRequest <= 0 || timeouts.MetadataRequest > time.Minute ||
		timeouts.UploadRequest <= 0 || timeouts.UploadRequest > 5*time.Minute ||
		timeouts.IndexWait <= 0 || timeouts.IndexWait > time.Hour ||
		timeouts.PollInterval <= 0 || timeouts.PollInterval > time.Minute {
		return fmt.Errorf("invalid Target API timeout configuration")
	}
	return nil
}

type sseFrame struct {
	event string
	id    string
	data  strings.Builder
}

func parseSSE(reader io.Reader, dispatch func(sseFrame) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4*1024), maxSSEEventBytes+1024)
	frame := sseFrame{}
	flush := func() error {
		if frame.event == "" && frame.id == "" && frame.data.Len() == 0 {
			return nil
		}
		if frame.event == "" || frame.id == "" || frame.data.Len() == 0 {
			return fmt.Errorf("%w: event, id, and data fields are required", ErrSSEContract)
		}
		if frame.data.Len() > maxSSEEventBytes {
			return fmt.Errorf("%w: event data exceeds %d bytes", ErrSSEContract, maxSSEEventBytes)
		}
		if err := dispatch(frame); err != nil {
			return err
		}
		frame = sseFrame{}
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			value = ""
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			frame.event = value
		case "id":
			frame.id = value
		case "data":
			if frame.data.Len() > 0 {
				frame.data.WriteByte('\n')
			}
			frame.data.WriteString(value)
		case "retry":
		default:
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read Target SSE stream: %w", err)
	}
	return flush()
}

type sseState struct {
	runID            string
	validator        *contracts.Validator
	onEvent          EventHandler
	result           ChatResult
	created          bool
	terminal         bool
	nextSequence     int
	nextToolSequence int
	eventIDs         map[string]struct{}
	callIndexes      map[string]int
	resolved         map[string]struct{}
	output           strings.Builder
}

func newSSEState(runID string, onEvent EventHandler, validator *contracts.Validator) *sseState {
	return &sseState{
		runID: runID, validator: validator, onEvent: onEvent,
		result: ChatResult{RunID: runID}, nextSequence: 1, nextToolSequence: 1,
		eventIDs: make(map[string]struct{}), callIndexes: make(map[string]int), resolved: make(map[string]struct{}),
	}
}

func (state *sseState) accept(frame sseFrame) error {
	if state.terminal {
		return fmt.Errorf("%w: event after terminal event", ErrSSEContract)
	}
	raw := json.RawMessage(frame.data.String())
	if err := state.validator.ValidateJSON(targetChatEventSchema, raw); err != nil {
		return fmt.Errorf("%w: %v", ErrSSEContract, err)
	}
	var envelope SessionEvent
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("%w: decode event: %v", ErrSSEContract, err)
	}
	if envelope.Type != frame.event || envelope.EventID != frame.id || envelope.RunID != state.runID {
		return fmt.Errorf("%w: SSE frame identity mismatch", ErrSSEContract)
	}
	if envelope.Sequence != state.nextSequence {
		return fmt.Errorf("%w: event sequence %d, want %d", ErrSSEContract, envelope.Sequence, state.nextSequence)
	}
	if _, exists := state.eventIDs[envelope.EventID]; exists {
		return fmt.Errorf("%w: duplicate event ID %q", ErrSSEContract, envelope.EventID)
	}
	if !state.created && envelope.Type != "response.created" {
		return fmt.Errorf("%w: first event must be response.created", ErrSSEContract)
	}
	if state.created && envelope.Type == "response.created" {
		return fmt.Errorf("%w: duplicate response.created", ErrSSEContract)
	}
	envelope.Raw = append([]byte(nil), raw...)
	state.eventIDs[envelope.EventID] = struct{}{}
	state.nextSequence++

	switch envelope.Type {
	case "response.created":
		state.created = true
	case "response.output_text.delta":
		var event struct {
			Delta string `json:"delta"`
		}
		_ = json.Unmarshal(raw, &event)
		state.output.WriteString(event.Delta)
		if len([]rune(state.output.String())) > maxOutputCharacters {
			return fmt.Errorf("%w: output exceeds %d characters", ErrSSEContract, maxOutputCharacters)
		}
	case "response.tool_call":
		if err := state.acceptToolCall(raw); err != nil {
			return err
		}
	case "response.tool_result":
		if err := state.acceptToolResult(raw); err != nil {
			return err
		}
	case "response.completed":
		if err := state.acceptCompleted(raw); err != nil {
			return err
		}
	case "response.error":
		if err := state.acceptError(raw); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: unknown event type %q", ErrSSEContract, envelope.Type)
	}
	state.result.Events = append(state.result.Events, envelope)
	if state.onEvent != nil {
		if err := state.onEvent(envelope); err != nil {
			return fmt.Errorf("handle Target SSE event: %w", err)
		}
	}
	return nil
}

func (state *sseState) acceptToolCall(raw json.RawMessage) error {
	var event struct {
		ToolSequence int            `json:"tool_sequence"`
		CallID       string         `json:"call_id"`
		ParentCallID *string        `json:"parent_call_id"`
		ToolName     string         `json:"tool_name"`
		Arguments    map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		return err
	}
	if event.ToolSequence != state.nextToolSequence {
		return fmt.Errorf("%w: Tool sequence %d, want %d", ErrSSEContract, event.ToolSequence, state.nextToolSequence)
	}
	if _, exists := state.callIndexes[event.CallID]; exists {
		return fmt.Errorf("%w: duplicate Tool call ID %q", ErrSSEContract, event.CallID)
	}
	state.callIndexes[event.CallID] = len(state.result.ToolCalls)
	state.result.ToolCalls = append(state.result.ToolCalls, ToolCall{
		Sequence: event.ToolSequence, CallID: event.CallID, ParentCallID: event.ParentCallID,
		ToolName: event.ToolName, Arguments: event.Arguments,
	})
	state.nextToolSequence++
	return nil
}

func (state *sseState) acceptToolResult(raw json.RawMessage) error {
	var event struct {
		ToolSequence int         `json:"tool_sequence"`
		CallID       string      `json:"call_id"`
		DurationMS   int64       `json:"duration_ms"`
		Result       *ToolResult `json:"result"`
		Error        *AgentError `json:"error"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		return err
	}
	index, exists := state.callIndexes[event.CallID]
	if !exists || state.result.ToolCalls[index].Sequence != event.ToolSequence {
		return fmt.Errorf("%w: Tool result has no matching call", ErrSSEContract)
	}
	if _, exists := state.resolved[event.CallID]; exists {
		return fmt.Errorf("%w: duplicate Tool result for %q", ErrSSEContract, event.CallID)
	}
	state.result.ToolCalls[index].Result = event.Result
	state.result.ToolCalls[index].Error = event.Error
	state.result.ToolCalls[index].DurationMS = event.DurationMS
	state.resolved[event.CallID] = struct{}{}
	return nil
}

func (state *sseState) acceptCompleted(raw json.RawMessage) error {
	var event struct {
		Status      string `json:"status"`
		FinalOutput string `json:"final_output"`
		Usage       *Usage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		return err
	}
	if state.pendingCalls() != 0 {
		return fmt.Errorf("%w: completion has unresolved Tool calls", ErrSSEContract)
	}
	if state.output.Len() > 0 && state.output.String() != event.FinalOutput {
		return fmt.Errorf("%w: output deltas do not match final_output", ErrSSEContract)
	}
	output := event.FinalOutput
	state.result.Status = event.Status
	state.result.FinalOutput = &output
	state.result.Usage = event.Usage
	state.terminal = true
	return nil
}

func (state *sseState) acceptError(raw json.RawMessage) error {
	var event struct {
		Status string      `json:"status"`
		Error  *AgentError `json:"error"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		return err
	}
	if state.pendingCalls() != 0 {
		return fmt.Errorf("%w: terminal error has unresolved Tool calls", ErrSSEContract)
	}
	state.result.Status = event.Status
	state.result.Error = event.Error
	state.terminal = true
	return nil
}

func (state *sseState) pendingCalls() int {
	return len(state.callIndexes) - len(state.resolved)
}

type countingReader struct {
	reader    io.Reader
	remaining int64
}

func (reader *countingReader) Read(buffer []byte) (int, error) {
	if reader.remaining <= 0 {
		return 0, fmt.Errorf("Target SSE response exceeds %d bytes", maxSSEResponseBytes)
	}
	if int64(len(buffer)) > reader.remaining+1 {
		buffer = buffer[:reader.remaining+1]
	}
	count, err := reader.reader.Read(buffer)
	reader.remaining -= int64(count)
	if reader.remaining < 0 {
		return count, fmt.Errorf("Target SSE response exceeds %d bytes", maxSSEResponseBytes)
	}
	return count, err
}
