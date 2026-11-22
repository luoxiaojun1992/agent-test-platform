// Package target registers validation MySQL instances, the test databases that validate Datasets and host Run
// databases (docs/architecture.md section 16.5). A registration stores the endpoint, the name of the
// administrator password secret, the TLS mode, and an optional public CA certificate in Platform MySQL.
// The administrator password itself is never part of the record.
package validationdb

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"agent-test-platform/internal/pki"
	"agent-test-platform/internal/sandbox"
	"agent-test-platform/internal/secrets"
)

const (
	// TLSDisabledLocalOnly allows plaintext only on isolated local networks (docs/technical/06 section 4).
	TLSDisabledLocalOnly = sandbox.TLSDisabledLocalOnly
	// TLSVerifyIdentity verifies the server certificate against the Target's CA, or the system roots.
	TLSVerifyIdentity = sandbox.TLSVerifyIdentity
	// MaxPageSize is the largest page List returns (docs/technical/05-api-and-implementation-plan.md section 2).
	MaxPageSize = 200

	defaultMySQLPort = 3306
)

var (
	// ErrInvalidName reports a Target name outside ^[a-z0-9][a-z0-9-]{0,62}$.
	ErrInvalidName = errors.New("target name must match ^[a-z0-9][a-z0-9-]{0,62}$")
	// ErrInvalidHost reports a host that is neither a hostname nor an IP address.
	ErrInvalidHost = errors.New("target host must be a host name or IP address of at most 255 characters")
	// ErrInvalidPort reports a port outside 1 to 65535.
	ErrInvalidPort = errors.New("target port must be from 1 to 65535")
	// ErrInvalidUsername reports an administrator name outside 1 to 32 letters, digits, or underscores.
	ErrInvalidUsername = errors.New("target admin username must be 1-32 letters, digits, or underscores")
	// ErrInvalidTLSMode reports a TLS mode other than the two documented ones.
	ErrInvalidTLSMode = errors.New("tls_mode must be DISABLED_LOCAL_ONLY or VERIFY_IDENTITY")
	// ErrCACertificateInvalid reports CA text that is not X.509 PEM, is too large, or is set without VERIFY_IDENTITY.
	ErrCACertificateInvalid = errors.New("ca_certificate_pem must hold X.509 certificates of at most 16384 bytes and applies only to VERIFY_IDENTITY")
	// ErrSecretNotFound reports an administrator password secret that has not been entered yet.
	ErrSecretNotFound = errors.New("admin password secret is not found; enter it on the Credentials page first")
	// ErrSameAsPlatform rejects a Target that points at the Platform MySQL endpoint.
	ErrSameAsPlatform = errors.New("target endpoint must differ from the Platform MySQL endpoint")
	// ErrConflict reports a Target name or endpoint that is already registered.
	ErrConflict = errors.New("a Target with this name or endpoint already exists")
	// ErrIdempotencyConflict reports an Idempotency-Key reused for a different registration.
	ErrIdempotencyConflict = errors.New("Idempotency-Key was already used for a different Target")
	// ErrDuplicate is returned by a Store when a unique key rejects an insert.
	ErrDuplicate = errors.New("target violates a unique key")
	// ErrNotFound reports that a Store has no row for the key.
	ErrNotFound = errors.New("target is not found")
	// ErrInvalidCursor reports a list cursor that this package did not issue.
	ErrInvalidCursor = errors.New("invalid list cursor")
	// ErrInvalidLimit reports a page size outside 1 to MaxPageSize.
	ErrInvalidLimit = errors.New("limit must be from 1 to 200")
)

var (
	namePattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	hostPattern     = regexp.MustCompile(`^[A-Za-z0-9.-]{1,255}$`)
	usernamePattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,32}$`)
	// targetIDPattern is the canonical lowercase form of an RFC 4122 UUID, the identifier of a validation MySQL.
	targetIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// newTargetID returns a random version 4 UUID in canonical lowercase form.
func newTargetID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate Target ID: %w", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(raw[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}

// Input is a registration request. Its JSON form is the request body, and its digest identifies a replay.
type Input struct {
	Name                    string `json:"name"`
	Host                    string `json:"host"`
	Port                    int    `json:"port"`
	AdminUsername           string `json:"admin_username"`
	AdminPasswordSecretName string `json:"admin_password_secret_name"`
	TLSMode                 string `json:"tls_mode"`
	CACertificatePEM        string `json:"ca_certificate_pem,omitempty"`
}

// Target is a registered validation MySQL instance.
type Target struct {
	ID string `json:"id"`
	Input
	CreatedAt time.Time `json:"created_at"`
}

// Endpoint is a TCP host and port.
type Endpoint struct {
	Host string
	Port int
}

// EndpointFromAddr parses a host:port address as the MySQL driver reports it. A missing port means 3306.
func EndpointFromAddr(addr string) (Endpoint, error) {
	host, rawPort, err := net.SplitHostPort(addr)
	if err != nil {
		host, rawPort = addr, strconv.Itoa(defaultMySQLPort)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port < 1 || port > 65535 || host == "" {
		return Endpoint{}, errors.New("platform MySQL address is not a host and port")
	}
	return Endpoint{Host: host, Port: port}, nil
}

// IsInvalidInput reports whether err is a request validation failure, which the API reports as 400.
func IsInvalidInput(err error) bool {
	for _, sentinel := range []error{ErrInvalidName, ErrInvalidHost, ErrInvalidPort, ErrInvalidUsername, ErrInvalidTLSMode} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return errors.Is(err, secrets.ErrInvalidName)
}

// Validate checks the request without touching the database. The admin secret's existence and the endpoint
// comparison are checked by the Service.
func (input Input) Validate() error {
	if !namePattern.MatchString(input.Name) {
		return ErrInvalidName
	}
	if !validHost(input.Host) {
		return ErrInvalidHost
	}
	if input.Port < 1 || input.Port > 65535 {
		return ErrInvalidPort
	}
	if !usernamePattern.MatchString(input.AdminUsername) {
		return ErrInvalidUsername
	}
	if err := secrets.ValidateName(input.AdminPasswordSecretName); err != nil {
		return fmt.Errorf("admin_password_secret_name: %w", err)
	}
	switch input.TLSMode {
	case TLSDisabledLocalOnly, TLSVerifyIdentity:
	default:
		return ErrInvalidTLSMode
	}
	if input.CACertificatePEM != "" {
		if input.TLSMode != TLSVerifyIdentity || pki.ValidateCAPEM(input.CACertificatePEM) != nil {
			return ErrCACertificateInvalid
		}
	}
	return nil
}

func validHost(host string) bool {
	if net.ParseIP(host) != nil {
		return len(host) <= 255
	}
	return hostPattern.MatchString(host)
}

// Record is a registration as stored, with the data an Idempotency-Key replay is compared against.
type Record struct {
	Target         Target
	IdempotencyKey string
	ConfigSHA256   string
}

// Store persists registrations. The MySQL implementation lives in the storage layer.
type Store interface {
	// Insert stores the record and returns the stored Target. ErrDuplicate reports a unique violation.
	Insert(ctx context.Context, record Record) (Target, error)
	// FindByIdempotencyKey returns the record stored with key, or ErrNotFound.
	FindByIdempotencyKey(ctx context.Context, key string) (Record, error)
	// ListPage returns up to limit Targets, newest first. A non-nil after resumes after that position.
	ListPage(ctx context.Context, after *Cursor, limit int) ([]Target, error)
}

// SecretChecker reports whether a secret exists. The secrets package satisfies it.
type SecretChecker interface {
	Exists(ctx context.Context, name string) (bool, error)
}

// Cursor is an opaque list position: the creation time and identifier of the last item returned.
type Cursor struct {
	CreatedAt time.Time
	ID        string
}

// Service registers and lists validation MySQL instances.
type Service struct {
	store    Store
	secrets  SecretChecker
	platform Endpoint
}

// NewService returns a Service. The platform endpoint is compared with every registration.
func NewService(store Store, secrets SecretChecker, platform Endpoint) (*Service, error) {
	if store == nil || secrets == nil {
		return nil, errors.New("target store and secret checker are required")
	}
	if platform.Host == "" || platform.Port < 1 {
		return nil, errors.New("platform endpoint is required")
	}
	return &Service{store: store, secrets: secrets, platform: platform}, nil
}

// Register stores a new Target. Replaying the same Idempotency-Key with the same request returns the stored
// Target; the same key with a different request is ErrIdempotencyConflict; a name or endpoint that is
// already registered is ErrConflict.
func (service *Service) Register(ctx context.Context, idempotencyKey string, input Input) (Target, error) {
	if err := input.Validate(); err != nil {
		return Target{}, err
	}
	if strings.EqualFold(input.Host, service.platform.Host) && input.Port == service.platform.Port {
		return Target{}, ErrSameAsPlatform
	}
	exists, err := service.secrets.Exists(ctx, input.AdminPasswordSecretName)
	if err != nil {
		return Target{}, fmt.Errorf("check admin password secret: %w", err)
	}
	if !exists {
		return Target{}, ErrSecretNotFound
	}
	digest, err := digestOf(input)
	if err != nil {
		return Target{}, err
	}
	targetID, err := newTargetID()
	if err != nil {
		return Target{}, err
	}
	record := Record{
		Target:         Target{ID: targetID, Input: input},
		IdempotencyKey: idempotencyKey,
		ConfigSHA256:   digest,
	}
	stored, err := service.store.Insert(ctx, record)
	if err == nil {
		return stored, nil
	}
	if !errors.Is(err, ErrDuplicate) {
		return Target{}, err
	}
	replay, findErr := service.store.FindByIdempotencyKey(ctx, idempotencyKey)
	if findErr == nil {
		if replay.ConfigSHA256 != digest {
			return Target{}, ErrIdempotencyConflict
		}
		return replay.Target, nil
	}
	if !errors.Is(findErr, ErrNotFound) {
		return Target{}, findErr
	}
	return Target{}, ErrConflict
}

// List returns one page of Targets, newest first. The returned cursor is empty when no items remain.
func (service *Service) List(ctx context.Context, cursor string, limit int) ([]Target, string, error) {
	if limit < 1 || limit > MaxPageSize {
		return nil, "", ErrInvalidLimit
	}
	var after *Cursor
	if cursor != "" {
		decoded, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", ErrInvalidCursor
		}
		after = &decoded
	}
	items, err := service.store.ListPage(ctx, after, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("list Targets: %w", err)
	}
	if len(items) <= limit {
		return items, "", nil
	}
	items = items[:limit]
	last := items[limit-1]
	return items, encodeCursor(Cursor{CreatedAt: last.CreatedAt, ID: last.ID}), nil
}

func digestOf(input Input) (string, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("encode Target request: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func encodeCursor(cursor Cursor) string {
	return base64.RawURLEncoding.EncodeToString([]byte(cursor.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + cursor.ID))
}

func decodeCursor(encoded string) (Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return Cursor{}, ErrInvalidCursor
	}
	createdAt, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	if !targetIDPattern.MatchString(parts[1]) {
		return Cursor{}, ErrInvalidCursor
	}
	return Cursor{CreatedAt: createdAt, ID: parts[1]}, nil
}
