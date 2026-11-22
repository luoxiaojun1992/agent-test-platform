// Package modelcfg manages the registered Generation and Judge models and which one is active per role.
package modelcfg

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	RoleGeneration = "generation"
	RoleJudge      = "judge"
)

var (
	ErrNotFound     = errors.New("model alias is not registered")
	ErrInvalidInput = errors.New("model request is invalid")
)

// Model is one registered model alias at its latest revision.
type Model struct {
	Alias     string    `json:"alias"`
	Role      string    `json:"role"`
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
}

// Store persists model registrations. Activation enables exactly one model of a role.
type Store interface {
	List(ctx context.Context) ([]Model, error)
	Activate(ctx context.Context, role, alias string) error
}

// Service lists models and selects the active one per role.
type Service struct{ store Store }

func NewService(store Store) (*Service, error) {
	if store == nil {
		return nil, errors.New("model store is required")
	}
	return &Service{store: store}, nil
}

func (service *Service) List(ctx context.Context) ([]Model, error) {
	return service.store.List(ctx)
}

// Activate makes alias the only active model of role.
func (service *Service) Activate(ctx context.Context, role, alias string) error {
	if role != RoleGeneration && role != RoleJudge {
		return fmt.Errorf("%w: role must be generation or judge", ErrInvalidInput)
	}
	if alias == "" {
		return fmt.Errorf("%w: alias is required", ErrInvalidInput)
	}
	return service.store.Activate(ctx, role, alias)
}
