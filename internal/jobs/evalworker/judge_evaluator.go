package evalworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"agent-test-platform/contracts"
	"agent-test-platform/internal/agent"
	"agent-test-platform/internal/evaluation"
	"agent-test-platform/internal/run"
)

// PromptVersion identifies the Judge prompt and request contract in use.
const PromptVersion = "judge-v1"

const (
	judgeThreshold = 70
)

// JudgeModel describes the configured Judge model for audit records.
type JudgeModel struct {
	Alias     string
	Provider  string
	Model     string
	BaseURL   string
	MaxTokens int
}

// SecretResolver resolves a stored secret (the Target bearer token or the OAuth client secret).
type SecretResolver interface {
	Resolve(ctx context.Context, name string) (string, error)
}

// JudgeEvaluator pulls the Target session context and aggregates the Judge verdict.
type JudgeEvaluator struct {
	judge     *evaluation.Judge
	model     JudgeModel
	secrets   SecretResolver
	http      agent.HTTPDoer
	validator *contracts.Validator
}

func NewJudgeEvaluator(judge *evaluation.Judge, model JudgeModel, secrets SecretResolver, http agent.HTTPDoer, validator *contracts.Validator) (*JudgeEvaluator, error) {
	if judge == nil || model.Alias == "" || model.Provider == "" || model.Model == "" || model.BaseURL == "" {
		return nil, errors.New("judge and model identity are required")
	}
	if secrets == nil || http == nil || validator == nil {
		return nil, errors.New("secret resolver, HTTP client, and validator are required")
	}
	return &JudgeEvaluator{judge: judge, model: model, secrets: secrets, http: http, validator: validator}, nil
}

func (evaluator *JudgeEvaluator) Evaluate(ctx context.Context, job run.EvaluationJob) (run.EvaluationResult, error) {
	token, err := evaluator.resolveToken(ctx, job.Agent)
	if err != nil {
		return run.EvaluationResult{}, RetryableError{Err: err}
	}
	client, err := agent.NewSSEClient(job.Agent.Endpoint, token, evaluator.http, evaluator.validator)
	if err != nil {
		return run.EvaluationResult{}, err
	}
	items, err := client.ListAllSessionContext(ctx, job.SessionID, 0)
	if err != nil {
		return run.EvaluationResult{}, RetryableError{Err: err}
	}
	contextItems := make([]json.RawMessage, len(items))
	for index, item := range items {
		encoded, err := json.Marshal(item)
		if err != nil {
			return run.EvaluationResult{}, err
		}
		contextItems[index] = encoded
	}
	prepared, err := evaluation.BuildRequest(evaluation.RequestInput{Outcome: job.Outcome, Prompt: job.Prompt, CaseJSON: job.CaseJSON,
		FinalOutput: job.FinalOutput, SessionContext: contextItems})
	if err != nil {
		return run.EvaluationResult{}, err
	}
	result, response, err := evaluator.judge.Evaluate(ctx, prepared.RequestJSON, prepared.Criteria)
	if err != nil {
		if strings.HasPrefix(err.Error(), "Judge request contract") {
			return run.EvaluationResult{}, err
		}
		return run.EvaluationResult{}, RetryableError{Err: err}
	}
	verdict, err := evaluation.Aggregate(prepared.Criteria, result, judgeThreshold)
	if err != nil {
		return run.EvaluationResult{}, err
	}
	config, _ := json.Marshal(map[string]any{"alias": evaluator.model.Alias, "provider": evaluator.model.Provider, "model": evaluator.model.Model,
		"base_url": evaluator.model.BaseURL, "max_output_tokens": evaluator.model.MaxTokens, "prompt_version": PromptVersion})
	sum := sha256.Sum256(config)
	responseJSON, err := json.Marshal(result)
	if err != nil {
		return run.EvaluationResult{}, err
	}
	criteria, err := json.Marshal(result.Criteria)
	if err != nil {
		return run.EvaluationResult{}, err
	}
	categories := result.FailureCategories
	if categories == nil {
		categories = []string{}
	}
	categoriesJSON, err := json.Marshal(categories)
	if err != nil {
		return run.EvaluationResult{}, err
	}
	out := run.EvaluationResult{Provider: evaluator.model.Provider, Model: evaluator.model.Model, Alias: evaluator.model.Alias,
		PromptVersion: PromptVersion, ConfigSHA256: hex.EncodeToString(sum[:]), BaseURL: evaluator.model.BaseURL, ConfigJSON: config,
		RequestJSON: prepared.RequestJSON, ResponseJSON: responseJSON, CriteriaJSON: criteria, FailureCategories: categoriesJSON,
		LLMScore: verdict.LLMScore, FinalScore: verdict.FinalScore, Pass: verdict.Pass, ProviderRequestID: response.ProviderRequestID}
	if response.Usage.Available {
		input, output := response.Usage.InputTokens, response.Usage.OutputTokens
		out.InputTokens, out.OutputTokens = &input, &output
	}
	return out, nil
}

func (evaluator *JudgeEvaluator) resolveToken(ctx context.Context, spec run.AgentSpec) (string, error) {
	if spec.TokenURL != "" {
		secret, err := evaluator.secrets.Resolve(ctx, spec.ClientSecretRef)
		if err != nil {
			return "", err
		}
		return agent.FetchClientCredentialsToken(ctx, evaluator.http, spec.TokenURL, spec.ClientID, secret)
	}
	return evaluator.secrets.Resolve(ctx, spec.TokenRef)
}
