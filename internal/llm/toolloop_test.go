package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type loopCompleter struct {
	responses []CompletionResponse
	requests  []CompletionRequest
}

func (completer *loopCompleter) Complete(_ context.Context, _ string, request CompletionRequest) (CompletionResponse, error) {
	completer.requests = append(completer.requests, request)
	index := len(completer.requests) - 1
	if index >= len(completer.responses) {
		return CompletionResponse{}, errors.New("no scripted response")
	}
	return completer.responses[index], nil
}

type loopExecutor struct {
	contents  map[string]string
	summaries map[string]json.RawMessage
	calls     []string
	terminal  string
}

func (executor *loopExecutor) Execute(_ context.Context, call ModelToolCall) (ToolExecution, error) {
	executor.calls = append(executor.calls, call.ID)
	content := executor.contents[call.ID]
	return ToolExecution{
		Status: "SUCCESS", Content: content, ResultDigest: digestBytes([]byte(content)),
		OriginalBytes: len([]byte(content)), CompactSummary: executor.summaries[call.ID],
		Terminal: call.ID == executor.terminal,
	}, nil
}

type reportRecorder struct {
	reports []CompactionReport
}

func (recorder *reportRecorder) RecordCompaction(_ context.Context, report CompactionReport) error {
	recorder.reports = append(recorder.reports, report)
	return nil
}

func TestToolLoopExecutesSequentialRounds(t *testing.T) {
	t.Parallel()

	completer := &loopCompleter{responses: []CompletionResponse{
		{StopReason: "tool_calls", ToolCalls: []ModelToolCall{{ID: "call-1", Name: "tool", Arguments: json.RawMessage(`{}`)}}},
		{StopReason: "tool_calls", ToolCalls: []ModelToolCall{{ID: "call-2", Name: "tool", Arguments: json.RawMessage(`{}`)}}},
		{StopReason: "stop", Content: "complete"},
	}}
	executor := &loopExecutor{contents: map[string]string{"call-1": `{"ok":1}`, "call-2": `{"ok":2}`}, summaries: map[string]json.RawMessage{}}
	loop := newTestToolLoop(t, completer, executor, nil, testCompactionPolicy())
	request := sessionBaseRequest()
	request.ToolChoice = ToolChoice{Mode: ToolChoiceAuto}

	result, err := loop.Run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.ModelCalls != 3 || result.ToolCalls != 2 || result.Response.Content != "complete" {
		t.Fatalf("result = %#v", result)
	}
	if len(completer.requests[1].Messages) != 4 || len(completer.requests[2].Messages) != 6 {
		t.Fatalf("message counts = %d, %d", len(completer.requests[1].Messages), len(completer.requests[2].Messages))
	}
}

func TestToolLoopCompactsBeforeNextModelCall(t *testing.T) {
	t.Parallel()

	completer := &loopCompleter{responses: []CompletionResponse{
		{StopReason: "tool_calls", ToolCalls: []ModelToolCall{{ID: "call-1", Name: "tool", Arguments: json.RawMessage(`{}`)}}},
		{StopReason: "tool_calls", ToolCalls: []ModelToolCall{{ID: "call-2", Name: "tool", Arguments: json.RawMessage(`{}`)}}},
		{StopReason: "stop", Content: "complete"},
	}}
	executor := &loopExecutor{
		contents:  map[string]string{"call-1": strings.Repeat("x", 20_000), "call-2": `{"ok":2}`},
		summaries: map[string]json.RawMessage{"call-1": json.RawMessage(`{"accepted":true}`)},
	}
	recorder := &reportRecorder{}
	loop := newTestToolLoop(t, completer, executor, recorder, testCompactionPolicy())
	request := sessionBaseRequest()
	request.ToolChoice = ToolChoice{Mode: ToolChoiceAuto}

	result, err := loop.Run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.CompactionReports) != 1 || len(recorder.reports) != 1 {
		t.Fatalf("reports = %#v recorded=%#v", result.CompactionReports, recorder.reports)
	}
	lastRequest := completer.requests[len(completer.requests)-1]
	for _, message := range lastRequest.Messages {
		if message.Content != nil && strings.Contains(*message.Content, strings.Repeat("x", 100)) {
			t.Fatal("oversized result reached the next model call")
		}
	}
}

func TestToolLoopEnforcesLimitsAndRequiredTool(t *testing.T) {
	t.Parallel()

	request := sessionBaseRequest()
	completer := &loopCompleter{responses: []CompletionResponse{{StopReason: "stop", Content: "done"}}}
	loop := newTestToolLoop(t, completer, &loopExecutor{}, nil, testCompactionPolicy())
	if _, err := loop.Run(context.Background(), request); !errors.Is(err, ErrRequiredToolMissing) {
		t.Fatalf("required Tool error = %v", err)
	}

	completer = &loopCompleter{responses: []CompletionResponse{{
		StopReason: "tool_calls", ToolCalls: []ModelToolCall{
			{ID: "call-1", Name: "tool", Arguments: json.RawMessage(`{}`)},
			{ID: "call-2", Name: "tool", Arguments: json.RawMessage(`{}`)},
		},
	}}}
	loop, err := NewToolLoop(completer, &loopExecutor{}, nil, ToolLoopConfig{
		ModelAlias: "generation", MaxModelCalls: 2, MaxToolCalls: 1, CompactionPolicy: testCompactionPolicy(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.Run(context.Background(), request); !errors.Is(err, ErrToolCallLimit) {
		t.Fatalf("Tool limit error = %v", err)
	}
}

func TestToolLoopRejectsOutputTruncation(t *testing.T) {
	t.Parallel()

	completer := &loopCompleter{responses: []CompletionResponse{{StopReason: "length", Content: "partial"}}}
	loop := newTestToolLoop(t, completer, &loopExecutor{}, nil, testCompactionPolicy())
	if _, err := loop.Run(context.Background(), sessionBaseRequest()); !errors.Is(err, ErrOutputTokenLimit) {
		t.Fatalf("output limit error = %v", err)
	}
}

func newTestToolLoop(t *testing.T, completer AliasCompleter, executor ToolExecutor, recorder CompactionRecorder, policy CompactionPolicy) *ToolLoop {
	t.Helper()
	loop, err := NewToolLoop(completer, executor, recorder, ToolLoopConfig{
		ModelAlias: "generation", MaxModelCalls: 4, MaxToolCalls: 4, CompactionPolicy: policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	return loop
}
