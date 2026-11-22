package evalworker

import (
	"context"
	"fmt"

	"agent-test-platform/internal/run"
)

// Router dispatches an evaluation job to the Judge registered for its attempt's model alias.
type Router struct {
	judges   map[string]Evaluator
	fallback string
}

// NewRouter registers Judges by alias. Jobs without an alias use fallback.
func NewRouter(fallback string, judges map[string]Evaluator) (*Router, error) {
	if judges[fallback] == nil {
		return nil, fmt.Errorf("fallback judge %q is not registered", fallback)
	}
	return &Router{judges: judges, fallback: fallback}, nil
}

func (router *Router) Evaluate(ctx context.Context, job run.EvaluationJob) (run.EvaluationResult, error) {
	alias := job.JudgeAlias
	if alias == "" || alias == run.DefaultJudgeAlias {
		alias = router.fallback
	}
	evaluator := router.judges[alias]
	if evaluator == nil {
		return run.EvaluationResult{}, fmt.Errorf("judge model alias %q is not configured on this worker", alias)
	}
	return evaluator.Evaluate(ctx, job)
}
