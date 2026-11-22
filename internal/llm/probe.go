package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

type ProbeResult struct {
	Passed    bool
	Latency   time.Duration
	Error     string
	UsageSeen bool
}

type ProbeReport struct {
	Text       ProbeResult
	ToolCall   ProbeResult
	JSONSchema ProbeResult
}

// ProbeCapabilities exercises one concrete model without persisting any production artifact.
func ProbeCapabilities(ctx context.Context, completer Completer, model ModelRef, timeout time.Duration) ProbeReport {
	if completer == nil || model.Provider == "" || model.Model == "" || timeout <= 0 {
		failure := ProbeResult{Error: "invalid probe configuration"}
		return ProbeReport{Text: failure, ToolCall: failure, JSONSchema: failure}
	}
	report := ProbeReport{}
	report.Text = runProbe(ctx, timeout, func(probeCtx context.Context) (CompletionResponse, error) {
		prompt := "Reply with probe-ok."
		return completer.Complete(probeCtx, CompletionRequest{Model: model, Messages: []Message{{Role: RoleUser, Content: &prompt}}, ToolChoice: ToolChoice{Mode: ToolChoiceNone}, MaxOutputTokens: 32})
	}, func(response CompletionResponse) error {
		if response.Content == "" {
			return fmt.Errorf("empty text response")
		}
		return nil
	})

	toolSchema := json.RawMessage(`{"type":"object","additionalProperties":false,"required":["value"],"properties":{"value":{"type":"string"}}}`)
	report.ToolCall = runProbe(ctx, timeout, func(probeCtx context.Context) (CompletionResponse, error) {
		prompt := "Call capability_probe with value probe-ok."
		return completer.Complete(probeCtx, CompletionRequest{
			Model: model, Messages: []Message{{Role: RoleUser, Content: &prompt}},
			Tools:      []ToolDefinition{{Name: "capability_probe", Parameters: toolSchema}},
			ToolChoice: ToolChoice{Mode: ToolChoiceNamed, Name: "capability_probe"}, MaxOutputTokens: 64,
		})
	}, func(response CompletionResponse) error {
		if len(response.ToolCalls) != 1 || response.ToolCalls[0].Name != "capability_probe" || !isJSONObject(response.ToolCalls[0].Arguments) {
			return fmt.Errorf("required tool call was not returned")
		}
		var arguments struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(response.ToolCalls[0].Arguments, &arguments); err != nil || arguments.Value == "" {
			return fmt.Errorf("tool call arguments do not satisfy probe schema")
		}
		return nil
	})

	responseSchema := json.RawMessage(`{"type":"object","additionalProperties":false,"required":["status"],"properties":{"status":{"const":"ok"}}}`)
	report.JSONSchema = runProbe(ctx, timeout, func(probeCtx context.Context) (CompletionResponse, error) {
		prompt := "Return status ok."
		return completer.Complete(probeCtx, CompletionRequest{Model: model, Messages: []Message{{Role: RoleUser, Content: &prompt}}, ToolChoice: ToolChoice{Mode: ToolChoiceNone}, ResponseSchema: responseSchema, MaxOutputTokens: 64})
	}, func(response CompletionResponse) error {
		compiler := jsonschema.NewCompiler()
		resource := "urn:agent-test-platform:probe-schema"
		var schema any
		if err := json.Unmarshal(responseSchema, &schema); err != nil {
			return err
		}
		if err := compiler.AddResource(resource, schema); err != nil {
			return err
		}
		compiled, err := compiler.Compile(resource)
		if err != nil {
			return err
		}
		decoder := json.NewDecoder(bytes.NewBufferString(response.Content))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return fmt.Errorf("structured response is not JSON: %w", err)
		}
		if err := compiled.Validate(value); err != nil {
			return fmt.Errorf("structured response does not match schema: %w", err)
		}
		return nil
	})
	return report
}

func (report ProbeReport) Capabilities(base ModelCapabilities) ModelCapabilities {
	base.ToolCalling = report.ToolCall.Passed
	base.NamedToolChoice = report.ToolCall.Passed
	base.JSONSchemaOutput = report.JSONSchema.Passed
	base.Usage = report.Text.UsageSeen || report.ToolCall.UsageSeen || report.JSONSchema.UsageSeen
	return base
}

func runProbe(ctx context.Context, timeout time.Duration, call func(context.Context) (CompletionResponse, error), validate func(CompletionResponse) error) ProbeResult {
	started := time.Now()
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	response, err := call(probeCtx)
	cancel()
	result := ProbeResult{Latency: time.Since(started), UsageSeen: response.Usage.Available}
	if err == nil {
		err = validate(response)
	}
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.Passed = true
	return result
}
