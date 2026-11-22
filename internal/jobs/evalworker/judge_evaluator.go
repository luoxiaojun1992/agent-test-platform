package evalworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"agent-test-platform/internal/evaluation"
	"agent-test-platform/internal/run"
)

// PromptVersion identifies the Judge prompt and request contract in use.
const PromptVersion = "judge-v1"

const (
	judgeThreshold = 70
	finalThreshold = 70
)

// JudgeModel describes the configured Judge model for audit records.
type JudgeModel struct {
	Alias     string
	Provider  string
	Model     string
	BaseURL   string
	MaxTokens int
}

// JudgeEvaluator builds the Judge request from Run evidence and aggregates the verdict.
type JudgeEvaluator struct {
	judge *evaluation.Judge
	model JudgeModel
}

func NewJudgeEvaluator(judge *evaluation.Judge, model JudgeModel) (*JudgeEvaluator, error) {
	if judge == nil || model.Alias == "" || model.Provider == "" || model.Model == "" || model.BaseURL == "" {
		return nil, errors.New("judge and model identity are required")
	}
	return &JudgeEvaluator{judge: judge, model: model}, nil
}

func (evaluator *JudgeEvaluator) Evaluate(ctx context.Context, job run.EvaluationJob) (run.EvaluationResult, error) {
	prepared, err := evaluation.BuildRequest(evaluation.RequestInput{Outcome: job.Outcome, Prompt: job.Prompt, CaseJSON: job.CaseJSON,
		FinalOutput: job.FinalOutput, Deterministic: job.Deterministic, ToolCalls: job.ToolCalls})
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
	verdict, err := evaluation.Aggregate(prepared.Checks, prepared.Criteria, result, judgeThreshold, finalThreshold)
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
