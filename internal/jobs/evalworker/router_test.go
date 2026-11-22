package evalworker

import (
	"context"
	"testing"

	"agent-test-platform/internal/run"
)

type aliasEvaluator struct{ name string }

func (e aliasEvaluator) Evaluate(context.Context, run.EvaluationJob) (run.EvaluationResult, error) {
	return run.EvaluationResult{Alias: e.name}, nil
}

func TestRouterSelectsJudgeByAlias(t *testing.T) {
	router, err := NewRouter("a", map[string]Evaluator{"a": aliasEvaluator{"a"}, "b": aliasEvaluator{"b"}})
	if err != nil {
		t.Fatal(err)
	}
	for alias, want := range map[string]string{"": "a", run.DefaultJudgeAlias: "a", "b": "b"} {
		got, err := router.Evaluate(context.Background(), run.EvaluationJob{JudgeAlias: alias})
		if err != nil || got.Alias != want {
			t.Fatalf("%q: %v %v", alias, got.Alias, err)
		}
	}
	if _, err := router.Evaluate(context.Background(), run.EvaluationJob{JudgeAlias: "zzz"}); err == nil {
		t.Fatal("expected unknown alias error")
	}
}
