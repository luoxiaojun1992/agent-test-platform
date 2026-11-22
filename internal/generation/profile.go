package generation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"agent-test-platform/internal/pki"
)

// ErrInvalidCursor reports a list cursor that was not issued by this API.
var ErrInvalidCursor = errors.New("invalid list cursor")

// DatabaseProfileRequest is the body of POST /api/v1/database-profiles
// (contracts/schemas/database-profile-request.schema.json). PasswordSecretRef names a Secret
// Reference; the plaintext password is never part of the request or stored by the platform.
type DatabaseProfileRequest struct {
	Name                   string `json:"name"`
	Engine                 string `json:"engine"`
	Host                   string `json:"host"`
	Port                   int    `json:"port"`
	Database               string `json:"database"`
	Username               string `json:"username"`
	PasswordSecretRef      string `json:"password_secret_ref"`
	TLSMode                string `json:"tls_mode"`
	CACertificatePEM       string `json:"ca_certificate_pem,omitempty"`
	SnapshotMode           string `json:"snapshot_mode"`
	SnapshotTimeoutSeconds int    `json:"snapshot_timeout_seconds"`
}

// DatabaseProfile is the public representation of a Database Profile (contracts/openapi.yaml
// DatabaseProfile). It never includes the Secret Reference. The optional CA certificate is public material.
type DatabaseProfile struct {
	ID                     string    `json:"id"`
	Name                   string    `json:"name"`
	Engine                 string    `json:"engine"`
	Host                   string    `json:"host"`
	Port                   int       `json:"port"`
	Database               string    `json:"database"`
	Username               string    `json:"username"`
	TLSMode                string    `json:"tls_mode"`
	CACertificatePEM       string    `json:"ca_certificate_pem,omitempty"`
	SnapshotMode           string    `json:"snapshot_mode"`
	SnapshotTimeoutSeconds int       `json:"snapshot_timeout_seconds"`
	CreatedAt              time.Time `json:"created_at"`
}

// CreateDatabaseProfileInput is what the store persists for one create request. ConfigJSON is the
// canonical request document and ConfigSHA256 its digest; the digest is unique per profile.
type CreateDatabaseProfileInput struct {
	IdempotencyKey string
	ConfigJSON     json.RawMessage
	ConfigSHA256   string
	Request        DatabaseProfileRequest
}

// CreateDatabaseProfile canonicalizes and digests the request, then stores it. The digest
// (and therefore the name) identifies the configuration, so a replay with the same key and the
// same configuration returns the stored profile.
func (service *Service) CreateDatabaseProfile(ctx context.Context, idempotencyKey string, body json.RawMessage) (DatabaseProfile, error) {
	canonical, digest, err := CanonicalJSON(body)
	if err != nil {
		return DatabaseProfile{}, err
	}
	var request DatabaseProfileRequest
	if err := json.Unmarshal(canonical, &request); err != nil {
		return DatabaseProfile{}, err
	}
	if request.CACertificatePEM != "" {
		if request.TLSMode != "VERIFY_IDENTITY" {
			return DatabaseProfile{}, fmt.Errorf("%w: a CA certificate applies only to VERIFY_IDENTITY", pki.ErrInvalidCA)
		}
		if err := pki.ValidateCAPEM(request.CACertificatePEM); err != nil {
			return DatabaseProfile{}, err
		}
	}
	return service.store.CreateDatabaseProfile(ctx, CreateDatabaseProfileInput{
		IdempotencyKey: idempotencyKey, ConfigJSON: canonical, ConfigSHA256: digest, Request: request,
	})
}

// ListDatabaseProfiles returns one page of Database Profiles, most recently created first, and
// the cursor for the next page ("" when this page is the last).
func (service *Service) ListDatabaseProfiles(ctx context.Context, cursor string, limit int) ([]DatabaseProfile, string, error) {
	return service.store.ListDatabaseProfiles(ctx, cursor, limit)
}
