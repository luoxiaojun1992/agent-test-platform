package pipeline

import (
	"encoding/json"
	"testing"

	"agent-test-platform/contracts"
)

func newValidator(t *testing.T) *contracts.Validator {
	t.Helper()
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	return validator
}

func TestDeriveBuildsInitTurnJudgeFinishPipeline(t *testing.T) {
	t.Parallel()

	p, err := Derive(2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if p.Config.Repetitions != 3 {
		t.Fatalf("repetitions = %d, want 3", p.Config.Repetitions)
	}
	// init, turn-1, judge-1, turn-2, judge-2, finish = 6 nodes.
	if len(p.Nodes) != 6 || len(p.Edges) != 6 {
		t.Fatalf("nodes=%d edges=%d, want 6 and 6", len(p.Nodes), len(p.Edges))
	}
	wantNodes := []string{"init", "turn-1", "judge-1", "turn-2", "judge-2", "finish"}
	for index, node := range p.Nodes {
		if node.ID != wantNodes[index] {
			t.Fatalf("node[%d] id = %q, want %q", index, node.ID, wantNodes[index])
		}
	}
	// init → turn-1 (static), turn-1 → judge-1 (static), judge-1 → turn-2 (conditional),
	// judge-1 → finish (conditional fail), turn-2 → judge-2 (static), judge-2 → finish (static).
	if p.Edges[0].When != nil || p.Edges[1].When != nil || p.Edges[2].When == nil || p.Edges[3].When == nil || p.Edges[4].When != nil || p.Edges[5].When != nil {
		t.Fatalf("edge conditions = %#v", p.Edges)
	}
	if p.Edges[2].When.Field != "pass" || p.Edges[2].When.Equals != true {
		t.Fatalf("gate condition = %#v", p.Edges[2].When)
	}
	if p.Edges[3].When.Field != "pass" || p.Edges[3].When.Equals != false {
		t.Fatalf("fail condition = %#v", p.Edges[3].When)
	}
}

func TestDeriveSingleTurnMatchesSingleTurnBehavior(t *testing.T) {
	t.Parallel()

	p, err := Derive(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	// init, turn-1, judge-1, finish = 4 nodes; init→turn-1, turn-1→judge-1, judge-1→finish = 3 edges.
	if len(p.Nodes) != 4 || len(p.Edges) != 3 {
		t.Fatalf("nodes=%d edges=%d, want 4 and 3", len(p.Nodes), len(p.Edges))
	}
	if p.Nodes[0].ID != "init" || p.Nodes[1].ID != "turn-1" || p.Nodes[2].ID != "judge-1" || p.Nodes[3].ID != "finish" {
		t.Fatalf("single-turn pipeline = %#v", p.Nodes)
	}
	if p.Edges[2].When != nil || p.Edges[2].From != "judge-1" || p.Edges[2].To != "finish" {
		t.Fatalf("last judge edge = %#v, want static judge-1→finish", p.Edges[2])
	}
}

func TestDeriveRejectsInvalidTurnCount(t *testing.T) {
	t.Parallel()

	for _, count := range []int{0, 17, -1} {
		if _, err := Derive(count, 1); err == nil {
			t.Fatalf("Derive(%d) accepted", count)
		}
	}
}

func TestDeriveRejectsInvalidRepetitions(t *testing.T) {
	t.Parallel()

	for _, reps := range []int{0, 11, -1} {
		if _, err := Derive(1, reps); err == nil {
			t.Fatalf("Derive(1, %d) accepted", reps)
		}
	}
}

func TestDeriveInitializesContext(t *testing.T) {
	t.Parallel()

	p, err := Derive(2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if p.Context.Iteration != 1 || !p.Context.RoundPassed {
		t.Fatalf("initial context = %#v", p.Context)
	}
	if p.Context.KnowledgeDocumentIDs == nil || p.Context.DatabaseTables == nil || p.Context.IterationScores == nil {
		t.Fatalf("context slices must be non-nil: %#v", p.Context)
	}
	if len(p.Result) != 0 {
		t.Fatalf("result must start empty: %#v", p.Result)
	}
}

func TestMarshalParseRoundtrip(t *testing.T) {
	t.Parallel()

	validator := newValidator(t)
	p, err := Derive(3, 2)
	if err != nil {
		t.Fatal(err)
	}
	data, err := Marshal(validator, p)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(validator, data)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Nodes) != len(p.Nodes) || len(parsed.Edges) != len(p.Edges) {
		t.Fatalf("roundtrip changed pipeline: %#v", parsed)
	}
	if parsed.Config.Repetitions != 2 || parsed.Context.Iteration != 1 {
		t.Fatalf("roundtrip changed config/context: %#v", parsed)
	}
}

func TestValidateRejectsUnknownReference(t *testing.T) {
	t.Parallel()

	p, err := Derive(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	p.Edges = append(p.Edges, Edge{From: "turn-1", To: "missing"})
	if err := Validate(p); err == nil {
		t.Fatal("pipeline with unknown edge target passed")
	}
}

func TestValidateRejectsMissingPair(t *testing.T) {
	t.Parallel()

	p, err := Derive(2, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Remove judge-2 to break the turn/judge pairing.
	filtered := p.Nodes[:0]
	for _, node := range p.Nodes {
		if node.ID == "judge-2" {
			continue
		}
		filtered = append(filtered, node)
	}
	p.Nodes = filtered
	if err := Validate(p); err == nil {
		t.Fatal("pipeline with missing judge node passed")
	}
}

func TestNextNodesGatesOnJudgeVerdict(t *testing.T) {
	t.Parallel()

	p, err := Derive(2, 1)
	if err != nil {
		t.Fatal(err)
	}
	passed := true
	failed := false

	// judge-1 pass → turn-2 becomes eligible.
	next := p.NextNodes("judge-1", NodeOutput{Pass: &passed, FinalScore: 80})
	if len(next) != 1 || next[0] != "turn-2" {
		t.Fatalf("pass next = %v, want [turn-2]", next)
	}
	// judge-1 fail → finish (fail edge).
	next = p.NextNodes("judge-1", NodeOutput{Pass: &failed, FinalScore: 40})
	if len(next) != 1 || next[0] != "finish" {
		t.Fatalf("fail next = %v, want [finish]", next)
	}
	// turn-1 always enables judge-1 (static edge).
	next = p.NextNodes("turn-1", NodeOutput{Status: "COMPLETED"})
	if len(next) != 1 || next[0] != "judge-1" {
		t.Fatalf("turn-1 next = %v, want [judge-1]", next)
	}
	// init always enables turn-1.
	next = p.NextNodes("init", NodeOutput{Status: "COMPLETED"})
	if len(next) != 1 || next[0] != "turn-1" {
		t.Fatalf("init next = %v, want [turn-1]", next)
	}
	// last judge always reaches finish, whether it passes or fails.
	next = p.NextNodes("judge-2", NodeOutput{Pass: &passed, FinalScore: 90})
	if len(next) != 1 || next[0] != "finish" {
		t.Fatalf("last judge pass next = %v, want [finish]", next)
	}
	next = p.NextNodes("judge-2", NodeOutput{Pass: &failed, FinalScore: 10})
	if len(next) != 1 || next[0] != "finish" {
		t.Fatalf("last judge fail next = %v, want [finish]", next)
	}
}

func TestNodesAfterExcludesFinish(t *testing.T) {
	t.Parallel()

	p, err := Derive(2, 1)
	if err != nil {
		t.Fatal(err)
	}
	// After turn-1: judge-1, turn-2, judge-2 (finish excluded).
	after := p.NodesAfter("turn-1")
	got := make([]string, len(after))
	for index, node := range after {
		got[index] = node.ID
	}
	want := []string{"judge-1", "turn-2", "judge-2"}
	if len(got) != len(want) {
		t.Fatalf("NodesAfter(turn-1) = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("NodesAfter(turn-1) = %v, want %v", got, want)
		}
	}
	// After judge-2 (last judge): nothing but finish, which is excluded.
	if after := p.NodesAfter("judge-2"); len(after) != 0 {
		t.Fatalf("NodesAfter(judge-2) = %v, want none", after)
	}
	// After init: everything but finish.
	if after := p.NodesAfter("init"); len(after) != 4 {
		t.Fatalf("NodesAfter(init) = %v, want 4 nodes", after)
	}
}

func TestSkipResultIsMinimal(t *testing.T) {
	t.Parallel()

	node := Node{ID: "turn-2", Type: NodeTypeTurn, Params: json.RawMessage(`{"turn":2}`)}
	record := SkipResult(3, node)
	if record.Iteration != 3 || record.NodeID != "turn-2" || record.Type != "turn" || record.Status != StatusSkip {
		t.Fatalf("SkipResult = %#v", record)
	}
	if record.Pass != nil || record.FinalScore != nil || record.StartedAt != "" || record.FinishedAt != "" || record.Error != "" {
		t.Fatalf("SkipResult must be minimal: %#v", record)
	}
}

func TestRound2(t *testing.T) {
	t.Parallel()

	if Round2(85.0) != 85.0 || Round2(85.1234) != 85.12 || Round2(85.999) != 86.0 {
		t.Fatalf("Round2 misbehaved")
	}
}
