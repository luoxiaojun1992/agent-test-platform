package runexec

import (
	"math"
	"strings"
	"testing"
	"time"

	"agent-test-platform/internal/evaluation"
	"agent-test-platform/internal/generation"
	"agent-test-platform/internal/run"
	"agent-test-platform/internal/sandbox"
)

func TestCellType(t *testing.T) {
	cases := map[string]sandbox.CellType{
		"BIGINT": sandbox.CellInteger, "tinyint(1)": sandbox.CellBoolean, "DECIMAL(10,2)": sandbox.CellDecimal,
		"VARCHAR(20)": sandbox.CellString, "DATETIME": sandbox.CellDateTime, "DATE": sandbox.CellDate,
		"JSON": sandbox.CellJSON, "VARBINARY(8)": sandbox.CellBinary,
	}
	for input, want := range cases {
		if got := cellType(input); got != want {
			t.Errorf("cellType(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestCapOutput(t *testing.T) {
	short, digest := capOutput("hello")
	if short != "hello" || len(digest) != 64 {
		t.Fatalf("unexpected %q %q", short, digest)
	}
	long := strings.Repeat("好", run.MaxFinalOutputLength+5)
	capped, longDigest := capOutput(long)
	if len([]rune(capped)) != run.MaxFinalOutputLength || longDigest == digest {
		t.Fatal("output not capped on rune boundary")
	}
}

func TestBuildRequestPromptNamesDatabase(t *testing.T) {
	job := run.Job{RunID: "run1", Prompt: "do it", Agent: run.AgentSpec{Model: "m", ToolsJSON: []byte(`[{"name":"q","description":"d","input_schema":{"type":"object"}}]`)}}
	request, err := buildRequest(job, generation.Candidate{AgentMode: "TASK"}, "atp_run_x", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(request.Message.Content, "`atp_run_x`") || request.Tools == nil || len(*request.Tools) != 1 {
		t.Fatalf("unexpected request %+v", request)
	}
	if _, err := buildRequest(job, generation.Candidate{AgentMode: "X"}, "d", time.Now()); err == nil {
		t.Fatal("expected mode error")
	}
}

func TestBuildChecksWeightsSumToOne(t *testing.T) {
	post := []evaluation.AssertionResult{{ID: "a", Pass: true}, {ID: "b", Pass: false}}
	checks, err := buildChecks(generation.Candidate{AgentMode: "TASK"}, run.OutcomeCompleted, post, sandbox.MutationReport{Pass: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sum := 0.0
	for _, check := range checks {
		sum += check.Weight
	}
	if math.Abs(sum-1) > 1e-9 || len(checks) != 3 {
		t.Fatalf("sum=%v checks=%d", sum, len(checks))
	}
}

func TestBuildChecksFallbackOutcome(t *testing.T) {
	checks, err := buildChecks(generation.Candidate{AgentMode: "ANSWER"}, run.OutcomeTimeout, nil, sandbox.MutationReport{}, nil)
	if err != nil || len(checks) != 1 || checks[0].Pass || checks[0].Weight != 1 {
		t.Fatalf("unexpected %+v %v", checks, err)
	}
}
