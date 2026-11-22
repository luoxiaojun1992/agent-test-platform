package run

import (
	"context"
	"errors"
	"testing"
)

const (
	c1     = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	c2     = "01ARZ3NDEKTSV4RRFFQ69G5FAX"
	agentV = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
)

var both = []string{c1, c2}

type fakeStore struct {
	unknownJudge bool
	agentDB      bool
	reevalErr    error
	reeval       []string
	inserted     []NewRun
	key          string
	digest       string
	cancel       error
}

func (s *fakeStore) TestCases(context.Context, []string) ([]Case, error) {
	return []Case{{RevisionID: c1, DatasetRevisionID: "d1", Prompt: "p"}, {RevisionID: c2, DatasetRevisionID: "d1", Prompt: "q"}}, nil
}
func (s *fakeStore) AgentVersionInfo(context.Context, string) (bool, bool, error) {
	return true, s.agentDB, nil
}
func (s *fakeStore) FindCreation(_ context.Context, key string) (Creation, error) {
	if key != s.key || s.key == "" {
		return Creation{}, ErrNotFound
	}
	ids := []string{}
	for _, r := range s.inserted {
		ids = append(ids, r.ID)
	}
	return Creation{RunIDs: ids, RequestSHA256: s.digest}, nil
}
func (s *fakeStore) InsertRuns(_ context.Context, _, key, digest string, runs []NewRun) error {
	s.inserted, s.key, s.digest = runs, key, digest
	return nil
}
func (s *fakeStore) GetRun(context.Context, string) (Run, error)                   { return Run{}, nil }
func (s *fakeStore) ListRuns(context.Context, Filter, *Cursor, int) ([]Run, error) { return nil, nil }
func (s *fakeStore) ListToolCalls(context.Context, string) ([]ToolCall, error)     { return nil, nil }
func (s *fakeStore) Cancel(context.Context, string) (Run, error)                   { return Run{}, s.cancel }
func (s *fakeStore) ListEvaluations(context.Context, string) ([]Evaluation, error) { return nil, nil }
func (s *fakeStore) ListAllEvaluations(context.Context, EvalFilter, *Cursor, int) ([]Evaluation, error) {
	return nil, nil
}
func (s *fakeStore) Benchmark(context.Context, []string) ([]BenchmarkEntry, error) { return nil, nil }

func newSvc(t *testing.T, s *fakeStore) *Service {
	t.Helper()
	svc, err := NewService(s)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestCreateQueuesOneRunPerCaseAndRepetition(t *testing.T) {
	s := &fakeStore{agentDB: true}
	ids, err := newSvc(t, s).Create(context.Background(), "key", Request{AgentVersionID: agentV, TestCaseRevisionIDs: both, Repetitions: 2})
	if err != nil || len(ids) != 4 {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
	if s.inserted[1].Repetition != 2 {
		t.Fatalf("unexpected rows %+v", s.inserted)
	}
}

func TestCreateRequiresAgentDatabase(t *testing.T) {
	s := &fakeStore{}
	if _, err := newSvc(t, s).Create(context.Background(), "k", Request{AgentVersionID: agentV, TestCaseRevisionIDs: both}); !errors.Is(err, ErrAgentDatabaseMissing) {
		t.Fatalf("no database: %v", err)
	}
	if len(s.inserted) != 0 {
		t.Fatal("nothing may be queued")
	}
}

func TestCreateReplayAndConflict(t *testing.T) {
	s := &fakeStore{agentDB: true}
	svc := newSvc(t, s)
	ctx := context.Background()
	first, err := svc.Create(ctx, "key", Request{AgentVersionID: agentV, TestCaseRevisionIDs: both})
	if err != nil {
		t.Fatal(err)
	}
	again, err := svc.Create(ctx, "key", Request{AgentVersionID: agentV, TestCaseRevisionIDs: both, Repetitions: 1})
	if err != nil || len(again) != len(first) || again[0] != first[0] {
		t.Fatalf("replay %v %v", again, err)
	}
	if _, err := svc.Create(ctx, "key", Request{AgentVersionID: agentV, TestCaseRevisionIDs: both, Repetitions: 3}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflict: %v", err)
	}
}

func TestPagingValidation(t *testing.T) {
	svc := newSvc(t, &fakeStore{})
	if _, _, err := svc.List(context.Background(), Filter{}, "", 0); !errors.Is(err, ErrInvalidLimit) {
		t.Fatal(err)
	}
	if _, _, err := svc.List(context.Background(), Filter{}, "!!", 10); !errors.Is(err, ErrInvalidCursor) {
		t.Fatal(err)
	}
	if _, _, err := svc.List(context.Background(), Filter{Status: "BOGUS"}, "", 10); !IsInvalidInput(err) {
		t.Fatal(err)
	}
}

func TestCancelPropagatesConflict(t *testing.T) {
	svc := newSvc(t, &fakeStore{cancel: ErrConflict})
	if _, err := svc.Cancel(context.Background(), c1); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
}

func (s *fakeStore) Reevaluate(_ context.Context, source, id, key, digest, alias string) error {
	if s.reevalErr != nil {
		return s.reevalErr
	}
	s.reeval = []string{source, id, alias}
	s.inserted, s.key, s.digest = []NewRun{{ID: id}}, key, digest
	return nil
}

func TestReevaluateCreatesNewRunAndReplays(t *testing.T) {
	s := &fakeStore{}
	service, _ := NewService(s)
	source := "01HZZZZZZZZZZZZZZZZZZZZZZZ"
	id, err := service.Reevaluate(context.Background(), source, "k1", "judge-b")
	if err != nil || id == "" || id == source || s.reeval[2] != "judge-b" {
		t.Fatalf("id=%q err=%v reeval=%v", id, err, s.reeval)
	}
	again, err := service.Reevaluate(context.Background(), source, "k1", "judge-b")
	if err != nil || again != id {
		t.Fatalf("replay=%q err=%v", again, err)
	}
	if _, err := service.Reevaluate(context.Background(), source, "k1", "judge-c"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("err=%v", err)
	}
}

func TestReevaluateValidatesInput(t *testing.T) {
	service, _ := NewService(&fakeStore{reevalErr: ErrConflict})
	if _, err := service.Reevaluate(context.Background(), "bad", "k", "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v", err)
	}
	if _, err := service.Reevaluate(context.Background(), "01HZZZZZZZZZZZZZZZZZZZZZZZ", "k", ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("err=%v", err)
	}
	if _, err := service.Reevaluate(context.Background(), "01HZZZZZZZZZZZZZZZZZZZZZZZ", "k", "a"); !errors.Is(err, ErrConflict) {
		t.Fatalf("err=%v", err)
	}
}

func (s *fakeStore) JudgeAliasExists(context.Context, string) (bool, error) {
	return !s.unknownJudge, nil
}

func TestReevaluateRejectsUnknownJudge(t *testing.T) {
	service, _ := NewService(&fakeStore{unknownJudge: true})
	if _, err := service.Reevaluate(context.Background(), "01HZZZZZZZZZZZZZZZZZZZZZZZ", "k", "nope"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("err=%v", err)
	}
}

func TestCreateRejectsBadCaseList(t *testing.T) {
	svc := newSvc(t, &fakeStore{})
	for _, ids := range [][]string{nil, {c1, c1}, {"x"}} {
		if _, err := svc.Create(context.Background(), "k", Request{AgentVersionID: agentV, TestCaseRevisionIDs: ids}); !IsInvalidInput(err) {
			t.Fatalf("%v: %v", ids, err)
		}
	}
}
