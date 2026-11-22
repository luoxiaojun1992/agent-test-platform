package llm

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

type probeCompleter struct {
	calls int
	fail  int
}

func (probe *probeCompleter) Complete(_ context.Context, request CompletionRequest) (CompletionResponse, error) {
	probe.calls++
	if probe.calls == probe.fail {
		return CompletionResponse{}, context.DeadlineExceeded
	}
	response := CompletionResponse{Model: request.Model, Usage: TokenUsage{Available: true}}
	switch probe.calls {
	case 1:
		response.Content = "probe-ok"
	case 2:
		response.ToolCalls = []ModelToolCall{{ID: "call-1", Name: "capability_probe", Arguments: json.RawMessage(`{"value":"probe-ok"}`)}}
	case 3:
		response.Content = `{"status":"ok"}`
	}
	return response, nil
}

func TestProbeCapabilities(t *testing.T) {
	t.Parallel()

	completer := &probeCompleter{}
	report := ProbeCapabilities(context.Background(), completer, ModelRef{Provider: ProviderOllama, Model: "local"}, time.Second)
	if !report.Text.Passed || !report.ToolCall.Passed || !report.JSONSchema.Passed || completer.calls != 3 {
		t.Fatalf("report = %#v calls=%d", report, completer.calls)
	}
	capabilities := report.Capabilities(ModelCapabilities{ContextWindow: 8192, MaxOutputTokens: 1024})
	if !capabilities.ToolCalling || !capabilities.NamedToolChoice || !capabilities.JSONSchemaOutput || !capabilities.Usage {
		t.Fatalf("capabilities = %#v", capabilities)
	}
}

func TestProbeCapabilitiesRecordsIndividualFailure(t *testing.T) {
	t.Parallel()

	completer := &probeCompleter{fail: 2}
	report := ProbeCapabilities(context.Background(), completer, ModelRef{Provider: ProviderOpenAI, Model: "model"}, time.Second)
	if !report.Text.Passed || report.ToolCall.Passed || !report.JSONSchema.Passed || report.ToolCall.Error == "" {
		t.Fatalf("report = %#v", report)
	}
}
