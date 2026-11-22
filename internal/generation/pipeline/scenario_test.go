package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"agent-test-platform/contracts"
	"agent-test-platform/internal/generation"
	"agent-test-platform/internal/llm"
)

type fakeScenarioGateway struct {
	response llm.CompletionResponse
	err      error
	alias    string
	request  llm.CompletionRequest
}

func (gateway *fakeScenarioGateway) Complete(_ context.Context, alias string, request llm.CompletionRequest) (llm.CompletionResponse, error) {
	gateway.alias, gateway.request = alias, request
	return gateway.response, gateway.err
}

func TestScenarioStageGeneratesValidatedDraft(t *testing.T) {
	t.Parallel()

	store := &fakeStore{artifact: normalizedArtifact(t)}
	scenario, err := contracts.Files.ReadFile("examples/scenario.json")
	if err != nil {
		t.Fatal(err)
	}
	gateway := &fakeScenarioGateway{response: llm.CompletionResponse{
		ProviderRequestID: "provider-request", Model: llm.ModelRef{Provider: llm.ProviderOpenAI, Model: "gpt-test"},
		StopReason: "stop", Content: string(scenario), RawDigest: strings.Repeat("d", 64),
		Usage: llm.TokenUsage{InputTokens: 100, OutputTokens: 200, Available: true},
	}}
	processor := newScenarioProcessor(t, store, gateway, scenarioPolicy())
	result, err := processor.Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusGeneratingScenario, Attempt: 1, Lease: validLease(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Transition.Event != generation.EventScenarioStored || result.Failure != nil {
		t.Fatalf("result = %#v", result)
	}
	if store.saved == nil || store.saved.Kind != generation.ArtifactKindScenario || store.saved.Key != scenarioArtifactKey || len(store.saved.SHA256) != 64 {
		t.Fatalf("draft = %#v", store.saved)
	}
	if gateway.alias != "generation-primary" || len(gateway.request.ResponseSchema) == 0 || len(gateway.request.Tools) != 0 {
		t.Fatalf("alias=%s request=%#v", gateway.alias, gateway.request)
	}
}

func TestScenarioStageRejectsConstraintMismatch(t *testing.T) {
	t.Parallel()

	store := &fakeStore{artifact: normalizedArtifact(t)}
	scenario, err := contracts.Files.ReadFile("examples/scenario.json")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(scenario, &document); err != nil {
		t.Fatal(err)
	}
	document["domain"] = "banking"
	invalid, _ := json.Marshal(document)
	gateway := &fakeScenarioGateway{response: llm.CompletionResponse{StopReason: "stop", Content: string(invalid)}}
	result, err := newScenarioProcessor(t, store, gateway, scenarioPolicy()).Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusGeneratingScenario, Attempt: 1, Lease: validLease(),
	})
	if err != nil || result.Failure == nil || result.Failure.Code != "SCENARIO_CONSTRAINT_MISMATCH" || store.saved != nil {
		t.Fatalf("result=%#v error=%v", result, err)
	}
}

func TestScenarioStageRejectsInvalidOutputAndBudget(t *testing.T) {
	t.Parallel()

	store := &fakeStore{artifact: normalizedArtifact(t)}
	gateway := &fakeScenarioGateway{response: llm.CompletionResponse{StopReason: "stop", Content: `{}`}}
	result, err := newScenarioProcessor(t, store, gateway, scenarioPolicy()).Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusGeneratingScenario, Attempt: 1, Lease: validLease(),
	})
	if err != nil || result.Failure == nil || result.Failure.Code != "SCENARIO_SCHEMA_INVALID" {
		t.Fatalf("result=%#v error=%v", result, err)
	}

	policy := scenarioPolicy()
	policy.ContextWindow = 1_500
	result, err = newScenarioProcessor(t, &fakeStore{artifact: normalizedArtifact(t)}, gateway, policy).Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusGeneratingScenario, Attempt: 1, Lease: validLease(),
	})
	if err != nil || result.Failure == nil || result.Failure.Code != "CONTEXT_BUDGET_EXCEEDED" {
		t.Fatalf("budget result=%#v error=%v", result, err)
	}
}

func TestScenarioStagePropagatesGatewayFailure(t *testing.T) {
	t.Parallel()

	gatewayError := errors.New("provider unavailable")
	processor := newScenarioProcessor(t, &fakeStore{artifact: normalizedArtifact(t)}, &fakeScenarioGateway{err: gatewayError}, scenarioPolicy())
	_, err := processor.Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusGeneratingScenario, Attempt: 1, Lease: validLease(),
	})
	if !errors.Is(err, gatewayError) {
		t.Fatalf("error = %v", err)
	}
}

func normalizedArtifact(t *testing.T) generation.ArtifactSnapshot {
	t.Helper()
	document := json.RawMessage(`{
		"schema_version":"1.1",
		"validation_db_ids":["0b6d6f3e-2c4a-4f8e-9a1b-7c5d3e2f1a90"],
		"description":"Generate order cancellation tests.",
		"target_agent_version_id":null,
		"generate_dataset":true,
		"database_profile_id":null,
		"domain":"e-commerce",
		"agent_mode":"TASK",
		"capabilities":["MYSQL_EXECUTE","MYSQL_QUERY"],
		"difficulty":{"min":"MEDIUM","max":"HARD"},
		"test_case_count":3,
		"coverage":["AMBIGUOUS","EDGE_CASE","NORMAL"],
		"auto_activate":true
	}`)
	canonical, digest, err := generation.CanonicalJSON(document)
	if err != nil {
		t.Fatal(err)
	}
	return generation.ArtifactSnapshot{
		Kind: generation.ArtifactKindNormalizedRequest, Key: normalizedRequestKey,
		ContentJSON: canonical, SHA256: digest,
	}
}

func newScenarioProcessor(t *testing.T, store Store, gateway ScenarioGateway, policy llm.CompactionPolicy) *Processor {
	t.Helper()
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	processor, err := New(store, validator, WithScenarioGeneration(gateway, ScenarioConfig{
		ModelAlias: "generation-primary", MaxOutputTokens: 2_048, CompactionPolicy: policy,
	}))
	if err != nil {
		t.Fatal(err)
	}
	return processor
}

func scenarioPolicy() llm.CompactionPolicy {
	return llm.CompactionPolicy{
		ContextWindow: 16_000, ReservedOutputTokens: 2_048, SafetyMarginTokens: 1_024,
		KeepRecentGroups: 1, MaxSummaryBytes: 1_024, MaxCompactions: 4,
		MaxRetainedRecords: 4, MaxRollupBytes: 4_096,
	}
}
