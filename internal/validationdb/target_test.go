package validationdb

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"sort"
	"strings"
	"testing"
	"time"

	"agent-test-platform/internal/pki"
)

// fakeSecrets reports a fixed set of secret names as existing.
type fakeSecrets map[string]bool

func (secrets fakeSecrets) Exists(_ context.Context, name string) (bool, error) {
	return secrets[name], nil
}

// memoryStore is an in-memory Store with the same uniqueness rules as the MySQL table.
type memoryStore struct {
	records []Record
	clock   time.Time
}

func (store *memoryStore) Insert(_ context.Context, record Record) (Target, error) {
	for _, existing := range store.records {
		if existing.IdempotencyKey == record.IdempotencyKey || existing.Target.Name == record.Target.Name ||
			(strings.EqualFold(existing.Target.Host, record.Target.Host) && existing.Target.Port == record.Target.Port) {
			return Target{}, ErrDuplicate
		}
	}
	store.clock = store.clock.Add(time.Second)
	record.Target.CreatedAt = store.clock
	store.records = append(store.records, record)
	return record.Target, nil
}

func (store *memoryStore) FindByIdempotencyKey(_ context.Context, key string) (Record, error) {
	for _, record := range store.records {
		if record.IdempotencyKey == key {
			return record, nil
		}
	}
	return Record{}, ErrNotFound
}

func (store *memoryStore) ListPage(_ context.Context, after *Cursor, limit int) ([]Target, error) {
	items := make([]Target, 0, len(store.records))
	for _, record := range store.records {
		items = append(items, record.Target)
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.After(items[j].CreatedAt)
		}
		return items[i].ID > items[j].ID
	})
	var page []Target
	for _, item := range items {
		if after != nil && !(item.CreatedAt.Before(after.CreatedAt) || (item.CreatedAt.Equal(after.CreatedAt) && item.ID < after.ID)) {
			continue
		}
		page = append(page, item)
		if len(page) == limit {
			break
		}
	}
	return page, nil
}

func validInput() Input {
	return Input{
		Name:                    "target-a",
		Host:                    "target-a.internal",
		Port:                    3306,
		AdminUsername:           "atp_admin",
		AdminPasswordSecretName: "target-a-admin-password",
		TLSMode:                 TLSDisabledLocalOnly,
	}
}

func newTestService(t *testing.T) (*Service, *memoryStore) {
	t.Helper()
	store := &memoryStore{clock: time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)}
	service, err := NewService(store, fakeSecrets{"target-a-admin-password": true, "target-b-admin-password": true},
		Endpoint{Host: "platform-mysql", Port: 3306})
	if err != nil {
		t.Fatal(err)
	}
	return service, store
}

// testCAPEM returns one self-signed X.509 certificate in PEM form.
func testCAPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test target CA"},
		NotBefore:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:     time.Date(2036, 1, 1, 0, 0, 0, 0, time.UTC),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestInputValidationRejectsMalformedFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		change func(*Input)
		want   error
	}{
		{name: "name", change: func(i *Input) { i.Name = "Target_A" }, want: ErrInvalidName},
		{name: "host", change: func(i *Input) { i.Host = "bad host" }, want: ErrInvalidHost},
		{name: "port zero", change: func(i *Input) { i.Port = 0 }, want: ErrInvalidPort},
		{name: "port too large", change: func(i *Input) { i.Port = 65536 }, want: ErrInvalidPort},
		{name: "username", change: func(i *Input) { i.AdminUsername = "root;drop" }, want: ErrInvalidUsername},
		{name: "tls mode", change: func(i *Input) { i.TLSMode = "PREFERRED" }, want: ErrInvalidTLSMode},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			input := validInput()
			test.change(&input)
			if err := input.Validate(); !errors.Is(err, test.want) {
				t.Fatalf("Validate = %v, want %v", err, test.want)
			}
		})
	}
	ipv6 := validInput()
	ipv6.Host = "::1"
	if err := ipv6.Validate(); err != nil {
		t.Fatalf("IPv6 host rejected: %v", err)
	}
}

func TestCACertificateIsOptionalPublicAndVerifyIdentityOnly(t *testing.T) {
	t.Parallel()

	pemText := testCAPEM(t)
	verify := validInput()
	verify.TLSMode = TLSVerifyIdentity
	verify.CACertificatePEM = pemText
	if err := verify.Validate(); err != nil {
		t.Fatalf("valid CA rejected: %v", err)
	}

	disabled := validInput()
	disabled.CACertificatePEM = pemText
	if err := disabled.Validate(); !errors.Is(err, ErrCACertificateInvalid) {
		t.Fatalf("CA with DISABLED_LOCAL_ONLY = %v", err)
	}

	garbage := verify
	garbage.CACertificatePEM = "not a certificate"
	if err := garbage.Validate(); !errors.Is(err, ErrCACertificateInvalid) {
		t.Fatalf("non-PEM CA = %v", err)
	}

	keyBlock := verify
	keyBlock.CACertificatePEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("x")}))
	if err := keyBlock.Validate(); !errors.Is(err, ErrCACertificateInvalid) {
		t.Fatalf("private key block accepted as CA: %v", err)
	}

	trailing := verify
	trailing.CACertificatePEM = pemText + "stray text"
	if err := trailing.Validate(); !errors.Is(err, ErrCACertificateInvalid) {
		t.Fatalf("text after the certificate accepted: %v", err)
	}

	oversized := verify
	oversized.CACertificatePEM = pemText + strings.Repeat("\n", pki.MaxCAPEMBytes)
	if err := oversized.Validate(); !errors.Is(err, ErrCACertificateInvalid) {
		t.Fatalf("oversized CA accepted: %v", err)
	}
}

func TestRegisterRejectsPlatformEndpointAndMissingSecret(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t)
	sameEndpoint := validInput()
	sameEndpoint.Host = "PLATFORM-MYSQL"
	if _, err := service.Register(context.Background(), "key-same-endpoint", sameEndpoint); !errors.Is(err, ErrSameAsPlatform) {
		t.Fatalf("platform endpoint = %v", err)
	}
	missing := validInput()
	missing.AdminPasswordSecretName = "not-entered-yet"
	if _, err := service.Register(context.Background(), "key-missing-secret", missing); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("missing secret = %v", err)
	}
}

func TestRegisterIsIdempotentAndDetectsConflicts(t *testing.T) {
	t.Parallel()

	service, store := newTestService(t)
	first, err := service.Register(context.Background(), "key-target-a", validInput())
	if err != nil {
		t.Fatal(err)
	}
	replay, err := service.Register(context.Background(), "key-target-a", validInput())
	if err != nil || replay.ID != first.ID || len(store.records) != 1 {
		t.Fatalf("replay = %#v, %v (records %d)", replay, err, len(store.records))
	}
	different := validInput()
	different.Port = 3307
	if _, err := service.Register(context.Background(), "key-target-a", different); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("same key, different request = %v", err)
	}
	sameName := validInput()
	sameName.Host = "other.internal"
	if _, err := service.Register(context.Background(), "key-other", sameName); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate name = %v", err)
	}
	sameEndpoint := validInput()
	sameEndpoint.Name = "target-b"
	sameEndpoint.AdminPasswordSecretName = "target-b-admin-password"
	if _, err := service.Register(context.Background(), "key-third", sameEndpoint); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate endpoint = %v", err)
	}
}

func TestListPagesNewestFirstWithOpaqueCursors(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t)
	names := []string{"target-a", "target-b"}
	hosts := []string{"target-a.internal", "target-b.internal"}
	for index := range names {
		input := validInput()
		input.Name = names[index]
		input.Host = hosts[index]
		input.AdminPasswordSecretName = names[index] + "-admin-password"
		if _, err := service.Register(context.Background(), "key-"+names[index], input); err != nil {
			t.Fatal(err)
		}
	}
	first, next, err := service.List(context.Background(), "", 1)
	if err != nil || len(first) != 1 || first[0].Name != "target-b" || next == "" {
		t.Fatalf("first page = %#v next = %q err = %v", first, next, err)
	}
	second, next, err := service.List(context.Background(), next, 1)
	if err != nil || len(second) != 1 || second[0].Name != "target-a" || next != "" {
		t.Fatalf("second page = %#v next = %q err = %v", second, next, err)
	}
	if _, _, err := service.List(context.Background(), "%%%", 1); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("invalid cursor = %v", err)
	}
	if _, _, err := service.List(context.Background(), encodeCursor(Cursor{CreatedAt: time.Now(), ID: "bad"}), 1); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("cursor with bad id = %v", err)
	}
	if _, _, err := service.List(context.Background(), "", MaxPageSize+1); !errors.Is(err, ErrInvalidLimit) {
		t.Fatalf("limit = %v", err)
	}
}

func TestEndpointFromAddrDefaultsPort(t *testing.T) {
	t.Parallel()

	endpoint, err := EndpointFromAddr("platform-mysql:3307")
	if err != nil || endpoint != (Endpoint{Host: "platform-mysql", Port: 3307}) {
		t.Fatalf("endpoint = %#v, %v", endpoint, err)
	}
	defaulted, err := EndpointFromAddr("platform-mysql")
	if err != nil || defaulted.Port != 3306 {
		t.Fatalf("defaulted = %#v, %v", defaulted, err)
	}
	if _, err := EndpointFromAddr(":3306"); err == nil {
		t.Fatal("empty host accepted")
	}
}
