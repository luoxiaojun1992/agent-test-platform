package modelcfg

import (
	"context"
	"errors"
	"testing"
)

type fakeStore struct{ role, alias string }

func (*fakeStore) List(context.Context) ([]Model, error) { return nil, nil }
func (s *fakeStore) Activate(_ context.Context, role, alias string) error {
	s.role, s.alias = role, alias
	return nil
}

func TestActivateValidates(t *testing.T) {
	store := &fakeStore{}
	service, _ := NewService(store)
	if err := service.Activate(context.Background(), "other", "a"); !errors.Is(err, ErrInvalidInput) {
		t.Fatal(err)
	}
	if err := service.Activate(context.Background(), RoleJudge, ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatal(err)
	}
	if err := service.Activate(context.Background(), RoleJudge, "j"); err != nil || store.role != RoleJudge || store.alias != "j" {
		t.Fatal(err, store)
	}
}
