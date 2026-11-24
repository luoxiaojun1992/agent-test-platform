package agentregistry

import (
	"context"
	"errors"
	"testing"
)

type fakeStore struct {
	Store
	agents map[string]AgentRecord
}

func (s *fakeStore) InsertAgent(_ context.Context, r AgentRecord) (Agent, error) {
	if _, ok := s.agents[r.IdempotencyKey]; ok {
		return Agent{}, ErrDuplicate
	}
	s.agents[r.IdempotencyKey] = r
	return r.Agent, nil
}
func (s *fakeStore) FindAgentByIdempotencyKey(_ context.Context, k string) (AgentRecord, error) {
	return s.agents[k], nil
}

func (s *fakeStore) DatabaseProfileExists(context.Context, string) (bool, error) { return true, nil }

type failingChecker struct{ err error }

func (c failingChecker) CheckDatabase(context.Context, string) error { return c.err }

type noSecrets struct{}

func (noSecrets) Exists(context.Context, string) (bool, error) { return true, nil }

func validInput(name string) AgentInput {
	return AgentInput{
		Name: name, ChatEndpoint: "https://a/chat", TargetBearerTokenRef: "tok",
		TargetModel: "m", ContractVersion: ContractVersion,
	}
}

func TestCreateAgentRejectsAnUnusableDatabase(t *testing.T) {
	store := &fakeStore{agents: map[string]AgentRecord{}}
	service, _ := NewService(store, noSecrets{})
	profile := "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	service.SetDatabaseChecker(failingChecker{err: errors.New("the account cannot create databases")})
	input := validInput("a")
	input.DatabaseProfileID = &profile
	if _, err := service.CreateAgent(context.Background(), "key-12345", input); !errors.Is(err, ErrDatabaseCheckFailed) {
		t.Fatalf("err = %v", err)
	}
	if len(store.agents) != 0 {
		t.Fatal("an Agent with an unusable database must not be stored")
	}
	service.SetDatabaseChecker(failingChecker{})
	if _, err := service.CreateAgent(context.Background(), "key-12345", input); err != nil {
		t.Fatalf("a usable database must be accepted: %v", err)
	}
	if _, err := service.CreateAgent(context.Background(), "key-67890", validInput("b")); err != nil {
		t.Fatalf("an Agent without a database needs no check: %v", err)
	}
}

func TestCreateAgentReplayAndConflict(t *testing.T) {
	service, _ := NewService(&fakeStore{agents: map[string]AgentRecord{}}, noSecrets{})
	first, err := service.CreateAgent(context.Background(), "key-12345", validInput("a"))
	if err != nil {
		t.Fatal(err)
	}
	replay, err := service.CreateAgent(context.Background(), "key-12345", validInput("a"))
	if err != nil || replay.ID != first.ID {
		t.Fatalf("replay = %v %v", replay, err)
	}
	if _, err := service.CreateAgent(context.Background(), "key-12345", validInput("b")); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("err = %v", err)
	}
}

func TestAgentValidation(t *testing.T) {
	valid := validInput("a")
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	valid.TargetModel = ""
	if err := valid.Validate(); !IsInvalidInput(err) {
		t.Fatalf("err = %v", err)
	}
}
