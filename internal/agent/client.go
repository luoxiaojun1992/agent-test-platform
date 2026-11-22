package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"agent-test-platform/internal/evidence"
	"agent-test-platform/internal/toolmatch"
)

const (
	maxRequestBytes       = 64 * 1024
	maxResponseBytes      = 512 * 1024
	maxArgumentBytes      = 16 * 1024
	maxResultBytes        = 32 * 1024
	maxTotalEvidenceBytes = 256 * 1024
)

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type ToolCall struct {
	Sequence     int            `json:"sequence"`
	CallID       string         `json:"call_id"`
	ParentCallID *string        `json:"parent_call_id"`
	ToolName     string         `json:"tool_name"`
	Arguments    map[string]any `json:"arguments"`
	Result       *ToolResult    `json:"result"`
	Error        *AgentError    `json:"error"`
	DurationMS   int64          `json:"duration_ms"`
}

type ToolResult struct {
	Status        string `json:"status"`
	Output        any    `json:"output"`
	RowCount      *int64 `json:"row_count"`
	AffectedRows  *int64 `json:"affected_rows"`
	Truncated     bool   `json:"truncated"`
	OriginalBytes *int64 `json:"original_bytes"`
	SHA256        string `json:"sha256"`
}

type AgentError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

type HTTPError struct {
	StatusCode int
	Body       string
}

func (err *HTTPError) Error() string {
	return fmt.Sprintf("Agent HTTP status %d: %s", err.StatusCode, err.Body)
}

func normalizeToolCalls(toolCalls []ToolCall) ([]toolmatch.Call, []evidence.Record, error) {
	calls := make([]toolmatch.Call, len(toolCalls))
	records := make([]evidence.Record, len(toolCalls))
	totalEvidenceBytes := 0
	for index, call := range toolCalls {
		argumentBytes, err := json.Marshal(call.Arguments)
		if err != nil {
			return nil, nil, fmt.Errorf("call %s arguments: %w", call.CallID, err)
		}
		if len(argumentBytes) > maxArgumentBytes {
			return nil, nil, fmt.Errorf("call %s arguments exceed %d bytes", call.CallID, maxArgumentBytes)
		}
		totalEvidenceBytes += len(argumentBytes)
		result := toolmatch.Result{Status: toolmatch.StatusError}
		record := evidence.Record{
			Sequence: call.Sequence, CallID: call.CallID, ParentCallID: call.ParentCallID,
			ToolName: call.ToolName, Arguments: argumentBytes, Status: evidence.StatusError,
			Source: evidence.SourceAgentReported, DurationMS: call.DurationMS,
		}
		record.ArgumentDigest, err = toolmatch.CanonicalJSONDigest(call.Arguments)
		if err != nil {
			return nil, nil, fmt.Errorf("call %s argument digest: %w", call.CallID, err)
		}
		if call.Result != nil {
			resultBytes, err := json.Marshal(call.Result)
			if err != nil {
				return nil, nil, fmt.Errorf("call %s result: %w", call.CallID, err)
			}
			if len(resultBytes) > maxResultBytes {
				return nil, nil, fmt.Errorf("call %s result exceeds %d bytes", call.CallID, maxResultBytes)
			}
			totalEvidenceBytes += len(resultBytes)
			result.Status = toolmatch.StatusSuccess
			result.AffectedRows = call.Result.AffectedRows
			result.Digest = call.Result.SHA256
			if call.Result.Truncated && result.Digest == "" {
				return nil, nil, fmt.Errorf("call %s truncated result requires full SHA-256", call.CallID)
			}
			outputDigest, err := toolmatch.CanonicalJSONDigest(call.Result.Output)
			if err != nil {
				return nil, nil, fmt.Errorf("call %s result digest: %w", call.CallID, err)
			}
			if result.Digest == "" {
				result.Digest = outputDigest
			} else if !call.Result.Truncated && result.Digest != outputDigest {
				return nil, nil, fmt.Errorf("call %s result SHA-256 does not match output", call.CallID)
			}
			record.Result = resultBytes
			record.ResultDigest = result.Digest
			record.Status = evidence.StatusSuccess
			record.Truncated = call.Result.Truncated
			record.OriginalBytes = call.Result.OriginalBytes
		} else {
			errorBytes, err := json.Marshal(call.Error)
			if err != nil {
				return nil, nil, fmt.Errorf("call %s error: %w", call.CallID, err)
			}
			totalEvidenceBytes += len(errorBytes)
			record.Error = errorBytes
			result.Digest, err = toolmatch.CanonicalJSONDigest(call.Error)
			if err != nil {
				return nil, nil, fmt.Errorf("call %s error digest: %w", call.CallID, err)
			}
			record.ErrorDigest = result.Digest
		}
		calls[index] = toolmatch.Call{
			Sequence: call.Sequence, CallID: call.CallID, ToolName: call.ToolName,
			Arguments: call.Arguments, Result: result,
		}
		if err := record.Validate(); err != nil {
			return nil, nil, fmt.Errorf("call %s evidence: %w", call.CallID, err)
		}
		records[index] = record
	}
	if totalEvidenceBytes > maxTotalEvidenceBytes {
		return nil, nil, fmt.Errorf("tool evidence exceeds %d bytes", maxTotalEvidenceBytes)
	}
	normalized, err := toolmatch.NormalizeAgentCalls(calls)
	return normalized, records, err
}

func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read Agent response: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("Agent response exceeds %d bytes", limit)
	}
	return data, nil
}
