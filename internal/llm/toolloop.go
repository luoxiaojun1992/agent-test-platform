package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrModelCallLimit      = errors.New("model call limit exceeded")
	ErrToolCallLimit       = errors.New("tool call limit exceeded")
	ErrOutputTokenLimit    = errors.New("model output token limit reached")
	ErrRequiredToolMissing = errors.New("required tool call missing")
)

type AliasCompleter interface {
	Complete(context.Context, string, CompletionRequest) (CompletionResponse, error)
}

type ToolExecution struct {
	Status         string
	Content        string
	ResultDigest   string
	OriginalBytes  int
	CompactSummary json.RawMessage
	Terminal       bool
}

func (execution ToolExecution) validate() error {
	if strings.TrimSpace(execution.Status) == "" || execution.Content == "" || !validSHA256(execution.ResultDigest) {
		return fmt.Errorf("Tool execution status, content, and digest are required")
	}
	if execution.OriginalBytes < len([]byte(execution.Content)) {
		return fmt.Errorf("Tool execution original byte count is invalid")
	}
	if len(execution.CompactSummary) > 0 && !isJSONObject(execution.CompactSummary) {
		return fmt.Errorf("Tool execution compact summary must be a JSON object")
	}
	return nil
}

type ToolExecutor interface {
	Execute(context.Context, ModelToolCall) (ToolExecution, error)
}

type CompactionRecorder interface {
	RecordCompaction(context.Context, CompactionReport) error
}

type ToolLoopConfig struct {
	ModelAlias       string
	MaxModelCalls    int
	MaxToolCalls     int
	CompactionPolicy CompactionPolicy
	StateFold        StateFold
}

type ToolLoopResult struct {
	Response          CompletionResponse
	ModelCalls        int
	ToolCalls         int
	TerminalTool      bool
	CompactionReports []CompactionReport
	Usage             TokenUsage
}

type ToolLoop struct {
	completer AliasCompleter
	executor  ToolExecutor
	recorder  CompactionRecorder
	config    ToolLoopConfig
}

func NewToolLoop(completer AliasCompleter, executor ToolExecutor, recorder CompactionRecorder, config ToolLoopConfig) (*ToolLoop, error) {
	if completer == nil || executor == nil || strings.TrimSpace(config.ModelAlias) == "" {
		return nil, fmt.Errorf("completer, Tool executor, and model alias are required")
	}
	if config.MaxModelCalls < 1 || config.MaxModelCalls > 100 || config.MaxToolCalls < 1 || config.MaxToolCalls > 1000 {
		return nil, fmt.Errorf("invalid model or Tool call limit")
	}
	if _, err := config.CompactionPolicy.inputTokenLimit(1); err != nil {
		return nil, fmt.Errorf("invalid compaction policy: %w", err)
	}
	return &ToolLoop{completer: completer, executor: executor, recorder: recorder, config: config}, nil
}

func (loop *ToolLoop) Run(ctx context.Context, request CompletionRequest) (ToolLoopResult, error) {
	if len(request.Messages) == 0 {
		return ToolLoopResult{}, fmt.Errorf("base request requires messages")
	}
	session := newSession(loop.config.StateFold)
	result := ToolLoopResult{}
	for result.ModelCalls < loop.config.MaxModelCalls {
		prepared, report, err := session.Prepare(request, loop.config.CompactionPolicy)
		if err != nil {
			return result, err
		}
		if report.CompactedGroups > 0 {
			if loop.recorder != nil {
				if err := loop.recorder.RecordCompaction(ctx, report); err != nil {
					return result, fmt.Errorf("record LLM session compaction: %w", err)
				}
			}
			result.CompactionReports = append(result.CompactionReports, report)
		}

		response, err := loop.completer.Complete(ctx, loop.config.ModelAlias, prepared)
		result.ModelCalls++
		if err != nil {
			return result, err
		}
		result.Response = response
		result.Usage = addUsage(result.Usage, response.Usage)
		if response.StopReason == "length" {
			return result, ErrOutputTokenLimit
		}
		if len(response.ToolCalls) == 0 {
			if request.ToolChoice.Mode == ToolChoiceRequired || request.ToolChoice.Mode == ToolChoiceNamed {
				return result, ErrRequiredToolMissing
			}
			if strings.TrimSpace(response.Content) == "" {
				return result, fmt.Errorf("model returned neither content nor Tool calls")
			}
			return result, nil
		}
		if response.StopReason != "tool_calls" {
			return result, fmt.Errorf("model returned Tool calls with stop reason %q", response.StopReason)
		}
		if result.ToolCalls+len(response.ToolCalls) > loop.config.MaxToolCalls {
			return result, ErrToolCallLimit
		}
		if err := session.AppendAssistantToolCalls(response, nil); err != nil {
			return result, fmt.Errorf("append assistant Tool calls: %w", err)
		}

		terminal := false
		for _, call := range response.ToolCalls {
			execution, err := loop.executor.Execute(ctx, call)
			if err != nil {
				return result, fmt.Errorf("execute Tool %s: %w", call.Name, err)
			}
			if err := execution.validate(); err != nil {
				return result, fmt.Errorf("execute Tool %s: %w", call.Name, err)
			}
			if err := session.AppendToolResult(SessionToolResult{
				CallID: call.ID, Status: execution.Status, Content: execution.Content,
				ResultDigest: execution.ResultDigest, OriginalBytes: execution.OriginalBytes,
				CompactSummary: execution.CompactSummary,
			}); err != nil {
				return result, fmt.Errorf("append Tool result %s: %w", call.ID, err)
			}
			result.ToolCalls++
			terminal = terminal || execution.Terminal
		}
		if terminal {
			result.TerminalTool = true
			return result, nil
		}
	}
	return result, ErrModelCallLimit
}

func addUsage(total, usage TokenUsage) TokenUsage {
	total.InputTokens += usage.InputTokens
	total.OutputTokens += usage.OutputTokens
	total.TotalTokens += usage.TotalTokens
	total.Available = total.Available || usage.Available
	return total
}
