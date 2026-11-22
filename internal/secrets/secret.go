// Package secrets stores test-environment credentials encrypted in Platform MySQL and resolves them by
// name. Values are write-only: Resolve is the only method that returns a plaintext value, and the
// Worker calls it at the moment a connection needs one.
package secrets

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var (
	// ErrInvalidName reports a secret name outside the documented pattern.
	ErrInvalidName = errors.New("secret name must match ^[a-z0-9][a-z0-9-]{0,127}$")
	// ErrInvalidValue reports a blank value or one longer than MaxValueBytes.
	ErrInvalidValue = errors.New("secret value must be 1-4096 bytes and not blank")
	// ErrInvalidLimit reports a page size outside 1 to MaxPageSize.
	ErrInvalidLimit = errors.New("secret page size must be from 1 to 200")
	// ErrInvalidCursor reports a list cursor that this package did not issue.
	ErrInvalidCursor = errors.New("secret cursor is invalid")
	// ErrNotFound reports a name with no stored value.
	ErrNotFound = errors.New("secret is not found")
)

// MaxValueBytes bounds one plaintext value. The stored ciphertext adds the GCM tag.
const MaxValueBytes = 4096

// MaxPageSize is the largest page List returns (docs/technical/05-api-and-implementation-plan.md section 2).
const MaxPageSize = 200

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,127}$`)

// Metadata describes a stored secret. It never includes the value.
type Metadata struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Record is a stored secret that is still sealed.
type Record struct {
	Metadata
	KeyID      string
	Nonce      []byte
	Ciphertext []byte
}

// Store persists sealed secrets. The MySQL implementation lives in the storage layer.
type Store interface {
	// Put inserts or replaces the sealed value stored under name and returns its metadata.
	Put(ctx context.Context, name, keyID string, nonce, ciphertext []byte) (Metadata, error)
	// Load returns the sealed record stored under name, or ErrNotFound.
	Load(ctx context.Context, name string) (Record, error)
	// List returns up to limit metadata rows whose names sort after after, in name order.
	List(ctx context.Context, after string, limit int) ([]Metadata, error)
	// ListNotUnder returns up to limit sealed records whose key id differs from keyID, in name order.
	ListNotUnder(ctx context.Context, keyID string, limit int) ([]Record, error)
	// Replace swaps the sealed value of stale for the given one only if stale is still stored as it was
	// read. It reports false when the row changed in the meantime.
	Replace(ctx context.Context, stale Record, keyID string, nonce, ciphertext []byte) (bool, error)
	// CountByKey returns how many stored secrets are sealed under each key id.
	CountByKey(ctx context.Context) (map[string]int, error)
}

// Service writes, lists, rewraps, and resolves secrets through a Store, sealing values with a Keyring.
type Service struct {
	store   Store
	keyring *Keyring
}

// NewService returns a Service. Both the store and the keyring are required.
func NewService(store Store, keyring *Keyring) (*Service, error) {
	if store == nil || keyring == nil {
		return nil, errors.New("secret store and keyring are required")
	}
	return &Service{store: store, keyring: keyring}, nil
}

// ValidateName reports whether name follows the secret naming rule.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return ErrInvalidName
	}
	return nil
}

// ValidateValue reports whether value may be stored. Values are kept exactly as entered; only
// blank values are rejected.
func ValidateValue(value string) error {
	if len(value) > MaxValueBytes || strings.TrimSpace(value) == "" {
		return ErrInvalidValue
	}
	return nil
}

// Put stores value under name, replacing any previous value, and returns the metadata only.
func (service *Service) Put(ctx context.Context, name, value string) (Metadata, error) {
	if err := ValidateName(name); err != nil {
		return Metadata{}, err
	}
	if err := ValidateValue(value); err != nil {
		return Metadata{}, err
	}
	keyID, nonce, ciphertext, err := service.keyring.Seal(name, value)
	if err != nil {
		return Metadata{}, err
	}
	metadata, err := service.store.Put(ctx, name, keyID, nonce, ciphertext)
	if err != nil {
		return Metadata{}, fmt.Errorf("store secret %s: %w", name, err)
	}
	return metadata, nil
}

// List returns one page of secret metadata in name order. The cursor for the next page is empty
// when no rows remain.
func (service *Service) List(ctx context.Context, cursor string, limit int) ([]Metadata, string, error) {
	if limit < 1 || limit > MaxPageSize {
		return nil, "", ErrInvalidLimit
	}
	after := ""
	if cursor != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || ValidateName(string(decoded)) != nil {
			return nil, "", ErrInvalidCursor
		}
		after = string(decoded)
	}
	items, err := service.store.List(ctx, after, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("list secrets: %w", err)
	}
	if len(items) <= limit {
		return items, "", nil
	}
	items = items[:limit]
	return items, base64.RawURLEncoding.EncodeToString([]byte(items[limit-1].Name)), nil
}

// Exists reports whether a value is stored under name. It never returns the value.
func (service *Service) Exists(ctx context.Context, name string) (bool, error) {
	if err := ValidateName(name); err != nil {
		return false, err
	}
	_, err := service.store.Load(ctx, name)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load secret %s: %w", name, err)
	}
	return true, nil
}

// Resolve returns the plaintext value stored under name. It is the only method besides ResolveVersioned
// that returns one, and the Worker's snapshot source and Target connections are its only callers.
func (service *Service) Resolve(ctx context.Context, name string) (string, error) {
	value, _, err := service.ResolveVersioned(ctx, name)
	return value, err
}

// ResolveVersioned returns the plaintext value stored under name together with the time it was last
// written. A caller that caches something built from the value (a connection pool) keys the cache by
// that time, so a replaced value takes effect without a restart. A key rotation does not change it.
func (service *Service) ResolveVersioned(ctx context.Context, name string) (string, time.Time, error) {
	if err := ValidateName(name); err != nil {
		return "", time.Time{}, err
	}
	record, err := service.store.Load(ctx, name)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("load secret %s: %w", name, err)
	}
	value, err := service.keyring.Open(record.KeyID, name, record.Nonce, record.Ciphertext)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("open secret %s: %w", name, err)
	}
	return value, record.UpdatedAt, nil
}

// RewrapReport summarizes one Rewrap run.
type RewrapReport struct {
	// Rewrapped counts secrets moved to the current key.
	Rewrapped int
	// RemainingByKey counts stored secrets by the key that still seals them, after the run.
	RemainingByKey map[string]int
}

// Rewrap re-seals every secret that is not under the current key, in batches of batchSize. Each value is
// opened with its own key and sealed with the current one. A row that changed during the run is skipped,
// because a concurrent write already uses the current key. A value that cannot be opened stops the run,
// and the error names the secret and its key id, never the value.
func (service *Service) Rewrap(ctx context.Context, batchSize int) (RewrapReport, error) {
	report := RewrapReport{}
	if batchSize < 1 {
		return report, errors.New("rewrap batch size must be positive")
	}
	current := service.keyring.CurrentID()
	for {
		stale, err := service.store.ListNotUnder(ctx, current, batchSize)
		if err != nil {
			return report, fmt.Errorf("list secrets to rewrap: %w", err)
		}
		if len(stale) == 0 {
			break
		}
		progressed := false
		for _, record := range stale {
			plaintext, err := service.keyring.Open(record.KeyID, record.Name, record.Nonce, record.Ciphertext)
			if err != nil {
				return report, fmt.Errorf("rewrap secret %s: %w", record.Name, err)
			}
			keyID, nonce, ciphertext, err := service.keyring.Seal(record.Name, plaintext)
			if err != nil {
				return report, fmt.Errorf("seal secret %s: %w", record.Name, err)
			}
			replaced, err := service.store.Replace(ctx, record, keyID, nonce, ciphertext)
			if err != nil {
				return report, fmt.Errorf("store rewrapped secret %s: %w", record.Name, err)
			}
			if replaced {
				report.Rewrapped++
				progressed = true
			}
		}
		if !progressed {
			return report, errors.New("rewrap made no progress: stored secrets changed on every attempt")
		}
	}
	counts, err := service.store.CountByKey(ctx)
	if err != nil {
		return report, fmt.Errorf("count secrets by key: %w", err)
	}
	report.RemainingByKey = counts
	return report, nil
}
