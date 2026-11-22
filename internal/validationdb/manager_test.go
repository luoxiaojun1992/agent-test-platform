package validationdb

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeLookup struct {
	targets map[string]Target
	calls   int
}

func (lookup *fakeLookup) FindByID(_ context.Context, id string) (Target, error) {
	lookup.calls++
	record, found := lookup.targets[id]
	if !found {
		return Target{}, ErrNotFound
	}
	return record, nil
}

// fakeCredentials serves one password and write time per secret name, and can fail.
type fakeCredentials struct {
	mu        sync.Mutex
	passwords map[string]string
	versions  map[string]time.Time
	err       error
}

func (credentials *fakeCredentials) ResolveVersioned(_ context.Context, name string) (string, time.Time, error) {
	credentials.mu.Lock()
	defer credentials.mu.Unlock()
	if credentials.err != nil {
		return "", time.Time{}, credentials.err
	}
	password, found := credentials.passwords[name]
	if !found {
		return "", time.Time{}, errors.New("secret is not found")
	}
	return password, credentials.versions[name], nil
}

func (credentials *fakeCredentials) replace(name, password string, version time.Time) {
	credentials.mu.Lock()
	defer credentials.mu.Unlock()
	credentials.passwords[name] = password
	credentials.versions[name] = version
}

const (
	managerTargetA = "7b1e2d3c-4f50-4a61-9b72-83c4d5e6f708"
	managerTargetB = "8c2f3e4d-5061-4b72-8c83-94d5e6f70819"
)

var managerVersion = time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)

func newTestManager(t *testing.T) (*Manager, *fakeLookup, *fakeCredentials) {
	t.Helper()
	lookup := &fakeLookup{targets: map[string]Target{
		managerTargetA: {ID: managerTargetA, Input: Input{
			Name: "target-a", Host: "target-a.internal", Port: 3306, AdminUsername: "atp_admin",
			AdminPasswordSecretName: "target-a-admin-password", TLSMode: TLSDisabledLocalOnly,
		}},
		managerTargetB: {ID: managerTargetB, Input: Input{
			Name: "target-b", Host: "target-b.internal", Port: 3307, AdminUsername: "atp_admin",
			AdminPasswordSecretName: "target-b-admin-password", TLSMode: TLSDisabledLocalOnly,
		}},
	}}
	credentials := &fakeCredentials{
		passwords: map[string]string{"target-a-admin-password": "pw-a", "target-b-admin-password": "pw-b"},
		versions:  map[string]time.Time{"target-a-admin-password": managerVersion, "target-b-admin-password": managerVersion},
	}
	manager, err := NewManager(lookup, credentials, Endpoint{Host: "platform-mysql", Port: 3306})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager, lookup, credentials
}

func TestManagerBuildsOneConnectionPerTargetFromItsRecord(t *testing.T) {
	t.Parallel()

	manager, _, _ := newTestManager(t)
	a, err := manager.Connection(context.Background(), managerTargetA)
	if err != nil {
		t.Fatal(err)
	}
	b, err := manager.Connection(context.Background(), managerTargetB)
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "target-a" || a.Admin.Base.Addr != "target-a.internal:3306" || a.Admin.Base.User != "atp_admin" || a.Admin.Base.Passwd != "pw-a" {
		t.Fatalf("connection A = %#v", a.Admin.Base)
	}
	if b.Name != "target-b" || b.Admin.Base.Addr != "target-b.internal:3307" || b.Admin.Base.Passwd != "pw-b" {
		t.Fatalf("connection B = %#v", b.Admin.Base)
	}
	if a.Admin.Admin == b.Admin.Admin {
		t.Fatal("two Targets share one connection pool")
	}
}

func TestManagerReusesTheConnectionUntilThePasswordSecretIsReplaced(t *testing.T) {
	t.Parallel()

	manager, _, credentials := newTestManager(t)
	first, err := manager.Connection(context.Background(), managerTargetA)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Connection(context.Background(), managerTargetA)
	if err != nil || second.Admin.Admin != first.Admin.Admin {
		t.Fatalf("unchanged secret rebuilt the pool: %v", err)
	}

	credentials.replace("target-a-admin-password", "pw-a-rotated", managerVersion.Add(time.Hour))
	rebuilt, err := manager.Connection(context.Background(), managerTargetA)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.Admin.Admin == first.Admin.Admin || rebuilt.Admin.Base.Passwd != "pw-a-rotated" {
		t.Fatalf("replaced secret did not rebuild the pool: password %q", rebuilt.Admin.Base.Passwd)
	}
	if err := first.Admin.Admin.PingContext(context.Background()); err != nil && strings.Contains(err.Error(), "closed") {
		t.Fatal("the replaced pool was closed while a stage could still be using it")
	}
}

func TestManagerRejectsUnknownTargetPlatformEndpointAndSecretFailures(t *testing.T) {
	t.Parallel()

	manager, lookup, credentials := newTestManager(t)
	if _, err := manager.Connection(context.Background(), "9d304f5e-6172-4c83-9d94-a5e6f7081920"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown Target = %v", err)
	}

	platformTarget := lookup.targets[managerTargetA]
	platformTarget.Host, platformTarget.Port = "PLATFORM-MYSQL", 3306
	lookup.targets[managerTargetA] = platformTarget
	if _, err := manager.Connection(context.Background(), managerTargetA); !errors.Is(err, ErrSameAsPlatform) {
		t.Fatalf("platform endpoint = %v", err)
	}

	credentials.err = errors.New("cannot open secret")
	_, err := manager.Connection(context.Background(), managerTargetB)
	if err == nil || !strings.Contains(err.Error(), "target-b") || strings.Contains(err.Error(), "pw-b") {
		t.Fatalf("secret failure = %v", err)
	}
}

func TestManagerRejectsInvalidTLSSettings(t *testing.T) {
	t.Parallel()

	manager, lookup, _ := newTestManager(t)
	broken := lookup.targets[managerTargetB]
	broken.TLSMode = TLSVerifyIdentity
	broken.CACertificatePEM = "not a certificate"
	lookup.targets[managerTargetB] = broken
	if _, err := manager.Connection(context.Background(), managerTargetB); err == nil || !strings.Contains(err.Error(), "target-b") {
		t.Fatalf("invalid CA = %v", err)
	}
}

func TestNewManagerRequiresItsDependencies(t *testing.T) {
	t.Parallel()

	if _, err := NewManager(nil, &fakeCredentials{}, Endpoint{Host: "platform", Port: 3306}); err == nil {
		t.Fatal("nil lookup accepted")
	}
	if _, err := NewManager(&fakeLookup{}, &fakeCredentials{}, Endpoint{}); err == nil {
		t.Fatal("empty platform endpoint accepted")
	}
}
