// Package catalog serves the read-only view of published Scenarios, Datasets and Test Cases and their immutable
// revisions. Revisions are written only by the generation publish transaction.
package catalog

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// Kind selects one of the three logical asset types.
type Kind string

const (
	Scenario Kind = "scenario"
	Dataset  Kind = "dataset"
	TestCase Kind = "test_case"

	MaxPageSize = 200
)

var (
	ErrNotFound      = errors.New("resource is not found")
	ErrInvalidCursor = errors.New("invalid list cursor")
	ErrInvalidLimit  = errors.New("limit must be from 1 to 200")
)

// Row is one list item. Body holds the API representation; ID and CreatedAt drive paging.
type Row struct {
	ID        string
	CreatedAt time.Time
	Body      map[string]any
}

// Cursor is an opaque list position.
type Cursor struct {
	CreatedAt time.Time
	ID        string
}

// Store reads assets. The MySQL implementation lives in the storage layer.
type Store interface {
	List(ctx context.Context, kind Kind, after *Cursor, limit int) ([]Row, error)
	ListRevisions(ctx context.Context, kind Kind, parentID string, after *Cursor, limit int) ([]Row, error)
	GetRevision(ctx context.Context, kind Kind, id string) (Row, error)
	// SearchRevisions lists revisions of every asset of a kind, newest first, optionally filtered by a text query.
	SearchRevisions(ctx context.Context, kind Kind, query string, after *Cursor, limit int) ([]Row, error)
	Exists(ctx context.Context, kind Kind, id string) (bool, error)
}

// Service pages and fetches assets.
type Service struct{ store Store }

// NewService returns a Service.
func NewService(store Store) (*Service, error) {
	if store == nil {
		return nil, errors.New("catalog store is required")
	}
	return &Service{store: store}, nil
}

// List returns one page of logical assets, newest first.
func (service *Service) List(ctx context.Context, kind Kind, cursor string, limit int) ([]map[string]any, string, error) {
	after, err := pageArgs(cursor, limit)
	if err != nil {
		return nil, "", err
	}
	rows, err := service.store.List(ctx, kind, after, limit+1)
	return page(rows, limit, err)
}

// ListRevisions returns one page of an asset's revisions, newest first.
func (service *Service) ListRevisions(ctx context.Context, kind Kind, parentID, cursor string, limit int) ([]map[string]any, string, error) {
	after, err := pageArgs(cursor, limit)
	if err != nil {
		return nil, "", err
	}
	if _, err := ulid.ParseStrict(parentID); err != nil {
		return nil, "", ErrNotFound
	}
	exists, err := service.store.Exists(ctx, kind, parentID)
	if err != nil {
		return nil, "", fmt.Errorf("check %s: %w", kind, err)
	}
	if !exists {
		return nil, "", ErrNotFound
	}
	rows, err := service.store.ListRevisions(ctx, kind, parentID, after, limit+1)
	return page(rows, limit, err)
}

// MaxSearchLength bounds the text query of SearchRevisions.
const MaxSearchLength = 128

// SearchRevisions returns one page of revisions across all assets of a kind, newest first, whose id, name or content matches query.
func (service *Service) SearchRevisions(ctx context.Context, kind Kind, query, cursor string, limit int) ([]map[string]any, string, error) {
	after, err := pageArgs(cursor, limit)
	if err != nil {
		return nil, "", err
	}
	query = strings.TrimSpace(query)
	if len([]rune(query)) > MaxSearchLength {
		return nil, "", ErrInvalidLimit
	}
	rows, err := service.store.SearchRevisions(ctx, kind, query, after, limit+1)
	return page(rows, limit, err)
}

// GetRevision returns one immutable revision.
func (service *Service) GetRevision(ctx context.Context, kind Kind, id string) (map[string]any, error) {
	if _, err := ulid.ParseStrict(id); err != nil {
		return nil, ErrNotFound
	}
	row, err := service.store.GetRevision(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	return row.Body, nil
}

func page(rows []Row, limit int, err error) ([]map[string]any, string, error) {
	if err != nil {
		return nil, "", fmt.Errorf("list: %w", err)
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		next = encodeCursor(Cursor{CreatedAt: rows[limit-1].CreatedAt, ID: rows[limit-1].ID})
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, row.Body)
	}
	return items, next, nil
}

func pageArgs(cursor string, limit int) (*Cursor, error) {
	if limit < 1 || limit > MaxPageSize {
		return nil, ErrInvalidLimit
	}
	if cursor == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, ErrInvalidCursor
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return nil, ErrInvalidCursor
	}
	createdAt, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return nil, ErrInvalidCursor
	}
	if _, err := ulid.ParseStrict(parts[1]); err != nil {
		return nil, ErrInvalidCursor
	}
	return &Cursor{CreatedAt: createdAt, ID: parts[1]}, nil
}

func encodeCursor(cursor Cursor) string {
	return base64.RawURLEncoding.EncodeToString([]byte(cursor.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + cursor.ID))
}
