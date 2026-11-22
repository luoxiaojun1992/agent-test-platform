package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"
)

func testKeys(ids ...string) map[string][]byte {
	keys := make(map[string][]byte, len(ids))
	for index, id := range ids {
		keys[id] = bytes.Repeat([]byte{byte(index + 1)}, KeyBytes)
	}
	return keys
}

func testKeyring(t *testing.T, current string, ids ...string) *Keyring {
	t.Helper()
	ring, err := NewKeyring(current, testKeys(ids...))
	if err != nil {
		t.Fatal(err)
	}
	return ring
}

func TestKeyringRoundTripsAndBindsName(t *testing.T) {
	t.Parallel()

	ring := testKeyring(t, "k1", "k1")
	keyID, nonce, ciphertext, err := ring.Seal("sales-reader-password", "s3cret-value")
	if err != nil {
		t.Fatal(err)
	}
	if keyID != "k1" || bytes.Contains(ciphertext, []byte("s3cret-value")) {
		t.Fatalf("Seal returned key %q, ciphertext contains plaintext = %v", keyID, bytes.Contains(ciphertext, []byte("s3cret-value")))
	}
	value, err := ring.Open(keyID, "sales-reader-password", nonce, ciphertext)
	if err != nil || value != "s3cret-value" {
		t.Fatalf("Open = %q, %v", value, err)
	}
	if _, err := ring.Open(keyID, "other-name", nonce, ciphertext); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("ciphertext opened under another name: %v", err)
	}
}

func TestKeyringOpensWithOlderKeyAndReportsMissingKeyID(t *testing.T) {
	t.Parallel()

	old := testKeyring(t, "k1", "k1")
	keyID, nonce, ciphertext, err := old.Seal("reader", "value")
	if err != nil {
		t.Fatal(err)
	}
	rotated := testKeyring(t, "k2", "k1", "k2")
	if value, err := rotated.Open(keyID, "reader", nonce, ciphertext); err != nil || value != "value" {
		t.Fatalf("value sealed under k1 unreadable after rotation: %q, %v", value, err)
	}
	onlyNew := testKeyring(t, "k2", "k2")
	_, err = onlyNew.Open(keyID, "reader", nonce, ciphertext)
	if !errors.Is(err, ErrKeyUnavailable) || !strings.Contains(err.Error(), "k1") || strings.Contains(err.Error(), "value") {
		t.Fatalf("missing key id error = %v", err)
	}
}

func TestKeyringRejectsWrongKeyTamperingAndShortNonce(t *testing.T) {
	t.Parallel()

	ring := testKeyring(t, "k1", "k1")
	keyID, nonce, ciphertext, err := ring.Seal("reader", "value")
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewKeyring("k1", map[string][]byte{"k1": bytes.Repeat([]byte{9}, KeyBytes)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Open(keyID, "reader", nonce, ciphertext); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("wrong key: %v", err)
	}
	tampered := append([]byte(nil), ciphertext...)
	tampered[0] ^= 0x01
	if _, err := ring.Open(keyID, "reader", nonce, tampered); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("tampered ciphertext: %v", err)
	}
	if _, err := ring.Open(keyID, "reader", nonce[:4], ciphertext); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("short nonce: %v", err)
	}
}

func TestKeyringUsesFreshNoncePerWrite(t *testing.T) {
	t.Parallel()

	ring := testKeyring(t, "k1", "k1")
	_, firstNonce, firstCiphertext, err := ring.Seal("reader", "same")
	if err != nil {
		t.Fatal(err)
	}
	_, secondNonce, secondCiphertext, err := ring.Seal("reader", "same")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(firstNonce, secondNonce) || bytes.Equal(firstCiphertext, secondCiphertext) {
		t.Fatal("two writes of one value reused a nonce or ciphertext")
	}
}

func TestNewKeyringValidatesKeysAndCurrentID(t *testing.T) {
	t.Parallel()

	if _, err := NewKeyring("k1", map[string][]byte{"k1": make([]byte, 16)}); err == nil {
		t.Fatal("16-byte key was accepted")
	}
	if _, err := NewKeyring("k2", testKeys("k1")); err == nil {
		t.Fatal("current id outside the keyring was accepted")
	}
	if _, err := NewKeyring("Bad_Id", map[string][]byte{"Bad_Id": make([]byte, KeyBytes)}); err == nil {
		t.Fatal("invalid key id was accepted")
	}
	if _, err := NewKeyring("k1", map[string][]byte{}); err == nil {
		t.Fatal("empty keyring was accepted")
	}
}

func TestLoadKeyringReadsListedKeysAndNamesProblemsWithoutValues(t *testing.T) {
	t.Parallel()

	valid := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, KeyBytes))
	short := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 16))
	values := map[string]string{
		"SECRET_ENCRYPTION_KEY_ID":  "k2",
		"SECRET_ENCRYPTION_KEY_IDS": "k1, k2",
		"ATP_ENCRYPTION_KEY_K1":     valid,
		"ATP_ENCRYPTION_KEY_K2":     valid,
	}
	lookup := func(name string) (string, bool) {
		value, found := values[name]
		return value, found
	}
	ring, err := LoadKeyring(lookup)
	if err != nil || ring.CurrentID() != "k2" {
		t.Fatalf("LoadKeyring = %v, %v", ring, err)
	}

	tests := []struct {
		name   string
		change func(map[string]string)
		want   string
	}{
		{name: "missing current id", change: func(v map[string]string) { delete(v, "SECRET_ENCRYPTION_KEY_ID") }, want: "SECRET_ENCRYPTION_KEY_ID is required"},
		{name: "invalid current id", change: func(v map[string]string) { v["SECRET_ENCRYPTION_KEY_ID"] = "K2" }, want: "SECRET_ENCRYPTION_KEY_ID must be"},
		{name: "invalid listed id", change: func(v map[string]string) { v["SECRET_ENCRYPTION_KEY_IDS"] = "k1,bad_id" }, want: "invalid key id"},
		{name: "repeated listed id", change: func(v map[string]string) { v["SECRET_ENCRYPTION_KEY_IDS"] = "k1,k1" }, want: "repeats key id k1"},
		{name: "unset key variable", change: func(v map[string]string) { delete(v, "ATP_ENCRYPTION_KEY_K1") }, want: "ATP_ENCRYPTION_KEY_K1 is not set"},
		{name: "wrong key length", change: func(v map[string]string) { v["ATP_ENCRYPTION_KEY_K1"] = short }, want: "must be base64 of 32 bytes"},
		{name: "current not listed", change: func(v map[string]string) { v["SECRET_ENCRYPTION_KEY_IDS"] = "k1" }, want: "current key k2 is not in the keyring"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			changed := make(map[string]string, len(values))
			for name, value := range values {
				changed[name] = value
			}
			test.change(changed)
			_, err := LoadKeyring(func(name string) (string, bool) {
				value, found := changed[name]
				return value, found
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if strings.Contains(err.Error(), valid) || strings.Contains(err.Error(), short) {
				t.Fatalf("error exposes key material: %v", err)
			}
		})
	}
}

func TestValidateNameAndValue(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"reader", "sales-reader-password", "a", strings.Repeat("a", 128)} {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v", name, err)
		}
	}
	for _, name := range []string{"", "Reader", "reader_password", "-reader", strings.Repeat("a", 129), "env://x"} {
		if !errors.Is(ValidateName(name), ErrInvalidName) {
			t.Errorf("ValidateName(%q) accepted an invalid name", name)
		}
	}
	for _, value := range []string{"s3cret", " padded value ", strings.Repeat("x", MaxValueBytes)} {
		if err := ValidateValue(value); err != nil {
			t.Errorf("ValidateValue(%d bytes) = %v", len(value), err)
		}
	}
	for _, value := range []string{"", "   \t", strings.Repeat("x", MaxValueBytes+1)} {
		if !errors.Is(ValidateValue(value), ErrInvalidValue) {
			t.Errorf("ValidateValue(%d bytes) accepted an invalid value", len(value))
		}
	}
}

// memoryStore is an in-memory Store used to test Service without MySQL.
type memoryStore struct {
	records map[string]Record
	puts    int
}

func newMemoryStore() *memoryStore {
	return &memoryStore{records: make(map[string]Record)}
}

func (store *memoryStore) Put(_ context.Context, name, keyID string, nonce, ciphertext []byte) (Metadata, error) {
	store.puts++
	now := time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC).Add(time.Duration(store.puts) * time.Second)
	created := now
	if existing, found := store.records[name]; found {
		created = existing.CreatedAt
	}
	store.records[name] = Record{
		Metadata:   Metadata{Name: name, CreatedAt: created, UpdatedAt: now},
		KeyID:      keyID,
		Nonce:      nonce,
		Ciphertext: ciphertext,
	}
	return store.records[name].Metadata, nil
}

func (store *memoryStore) Load(_ context.Context, name string) (Record, error) {
	record, found := store.records[name]
	if !found {
		return Record{}, ErrNotFound
	}
	return record, nil
}

func (store *memoryStore) names(keep func(Record) bool, after string) []string {
	names := make([]string, 0, len(store.records))
	for name, record := range store.records {
		if name > after && keep(record) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func (store *memoryStore) List(_ context.Context, after string, limit int) ([]Metadata, error) {
	names := store.names(func(Record) bool { return true }, after)
	if len(names) > limit {
		names = names[:limit]
	}
	items := make([]Metadata, 0, len(names))
	for _, name := range names {
		items = append(items, store.records[name].Metadata)
	}
	return items, nil
}

func (store *memoryStore) ListNotUnder(_ context.Context, keyID string, limit int) ([]Record, error) {
	names := store.names(func(record Record) bool { return record.KeyID != keyID }, "")
	if len(names) > limit {
		names = names[:limit]
	}
	records := make([]Record, 0, len(names))
	for _, name := range names {
		records = append(records, store.records[name])
	}
	return records, nil
}

func (store *memoryStore) Replace(_ context.Context, stale Record, keyID string, nonce, ciphertext []byte) (bool, error) {
	current, found := store.records[stale.Name]
	if !found || current.KeyID != stale.KeyID || !bytes.Equal(current.Nonce, stale.Nonce) || !bytes.Equal(current.Ciphertext, stale.Ciphertext) {
		return false, nil
	}
	current.KeyID, current.Nonce, current.Ciphertext = keyID, nonce, ciphertext
	store.records[stale.Name] = current
	return true, nil
}

func (store *memoryStore) CountByKey(_ context.Context) (map[string]int, error) {
	counts := make(map[string]int)
	for _, record := range store.records {
		counts[record.KeyID]++
	}
	return counts, nil
}

func newTestService(t *testing.T, ring *Keyring) (*Service, *memoryStore) {
	t.Helper()
	store := newMemoryStore()
	service, err := NewService(store, ring)
	if err != nil {
		t.Fatal(err)
	}
	return service, store
}

func TestServicePutStoresOnlyCiphertextAndKeepsCreatedAt(t *testing.T) {
	t.Parallel()

	service, store := newTestService(t, testKeyring(t, "k1", "k1"))
	created, err := service.Put(context.Background(), "sales-reader-password", "s3cret-value")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(store.records["sales-reader-password"].Ciphertext, []byte("s3cret-value")) {
		t.Fatal("stored record contains the plaintext")
	}
	if value, err := service.Resolve(context.Background(), "sales-reader-password"); err != nil || value != "s3cret-value" {
		t.Fatalf("Resolve = %q, %v", value, err)
	}
	replaced, err := service.Put(context.Background(), "sales-reader-password", "rotated-value")
	if err != nil {
		t.Fatal(err)
	}
	if !replaced.CreatedAt.Equal(created.CreatedAt) || !replaced.UpdatedAt.After(created.UpdatedAt) {
		t.Fatalf("replace metadata = %#v, created = %#v", replaced, created)
	}
	if value, err := service.Resolve(context.Background(), "sales-reader-password"); err != nil || value != "rotated-value" {
		t.Fatalf("Resolve after replace = %q, %v", value, err)
	}
}

func TestServiceRejectsInvalidInputAndReportsExistence(t *testing.T) {
	t.Parallel()

	service, store := newTestService(t, testKeyring(t, "k1", "k1"))
	if _, err := service.Put(context.Background(), "Bad_Name", "value"); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("invalid name: %v", err)
	}
	if _, err := service.Put(context.Background(), "reader", "  "); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("blank value: %v", err)
	}
	if store.puts != 0 {
		t.Fatalf("invalid input reached the store %d times", store.puts)
	}
	if _, err := service.Resolve(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve(missing) = %v", err)
	}
	if exists, err := service.Exists(context.Background(), "missing"); err != nil || exists {
		t.Fatalf("Exists(missing) = %v, %v", exists, err)
	}
	if _, err := service.Put(context.Background(), "reader", "value"); err != nil {
		t.Fatal(err)
	}
	if exists, err := service.Exists(context.Background(), "reader"); err != nil || !exists {
		t.Fatalf("Exists(reader) = %v, %v", exists, err)
	}
}

func TestServiceListPagesInNameOrderWithOpaqueCursors(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t, testKeyring(t, "k1", "k1"))
	for _, name := range []string{"charlie", "alpha", "bravo"} {
		if _, err := service.Put(context.Background(), name, "value-"+name); err != nil {
			t.Fatal(err)
		}
	}
	first, next, err := service.List(context.Background(), "", 2)
	if err != nil || len(first) != 2 || first[0].Name != "alpha" || first[1].Name != "bravo" || next == "" {
		t.Fatalf("first page = %#v next = %q err = %v", first, next, err)
	}
	second, next, err := service.List(context.Background(), next, 2)
	if err != nil || len(second) != 1 || second[0].Name != "charlie" || next != "" {
		t.Fatalf("second page = %#v next = %q err = %v", second, next, err)
	}
	for _, cursor := range []string{"!!!", base64.RawURLEncoding.EncodeToString([]byte("Bad_Name"))} {
		if _, _, err := service.List(context.Background(), cursor, 2); !errors.Is(err, ErrInvalidCursor) {
			t.Fatalf("cursor %q: %v", cursor, err)
		}
	}
	for _, limit := range []int{0, MaxPageSize + 1} {
		if _, _, err := service.List(context.Background(), "", limit); !errors.Is(err, ErrInvalidLimit) {
			t.Fatalf("limit %d: %v", limit, err)
		}
	}
}

func TestResolveVersionedReturnsTheWriteTimeAndChangesOnReplace(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t, testKeyring(t, "k1", "k1"))
	first, err := service.Put(context.Background(), "reader", "first-value")
	if err != nil {
		t.Fatal(err)
	}
	value, version, err := service.ResolveVersioned(context.Background(), "reader")
	if err != nil || value != "first-value" || !version.Equal(first.UpdatedAt) {
		t.Fatalf("ResolveVersioned = %q, %v, %v (written at %v)", value, version, err, first.UpdatedAt)
	}
	second, err := service.Put(context.Background(), "reader", "second-value")
	if err != nil {
		t.Fatal(err)
	}
	_, replacedVersion, err := service.ResolveVersioned(context.Background(), "reader")
	if err != nil || !replacedVersion.Equal(second.UpdatedAt) || !replacedVersion.After(version) {
		t.Fatalf("version after replace = %v, want after %v", replacedVersion, version)
	}
	if _, _, err := service.ResolveVersioned(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing secret = %v", err)
	}
}

func TestRewrapMovesEverySecretToTheCurrentKey(t *testing.T) {
	t.Parallel()

	oldService, store := newTestService(t, testKeyring(t, "k1", "k1"))
	for _, name := range []string{"alpha", "bravo", "charlie"} {
		if _, err := oldService.Put(context.Background(), name, "value-"+name); err != nil {
			t.Fatal(err)
		}
	}
	rotated, err := NewService(store, testKeyring(t, "k2", "k1", "k2"))
	if err != nil {
		t.Fatal(err)
	}
	report, err := rotated.Rewrap(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if report.Rewrapped != 3 || report.RemainingByKey["k1"] != 0 || report.RemainingByKey["k2"] != 3 {
		t.Fatalf("report = %#v", report)
	}
	for _, name := range []string{"alpha", "bravo", "charlie"} {
		if value, err := rotated.Resolve(context.Background(), name); err != nil || value != "value-"+name {
			t.Fatalf("Resolve(%s) after rewrap = %q, %v", name, value, err)
		}
	}
	again, err := rotated.Rewrap(context.Background(), 2)
	if err != nil || again.Rewrapped != 0 {
		t.Fatalf("second rewrap = %#v, %v", again, err)
	}
}

func TestRewrapStopsOnUnreadableSecretWithoutRevealingIt(t *testing.T) {
	t.Parallel()

	service, store := newTestService(t, testKeyring(t, "k1", "k1"))
	if _, err := service.Put(context.Background(), "alpha", "plain-value-alpha"); err != nil {
		t.Fatal(err)
	}
	record := store.records["alpha"]
	record.KeyID = "retired"
	store.records["alpha"] = record

	rotated, err := NewService(store, testKeyring(t, "k2", "k2"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = rotated.Rewrap(context.Background(), 10)
	if !errors.Is(err, ErrKeyUnavailable) || !strings.Contains(err.Error(), "alpha") || !strings.Contains(err.Error(), "retired") {
		t.Fatalf("Rewrap error = %v", err)
	}
	if strings.Contains(err.Error(), "plain-value-alpha") {
		t.Fatalf("error reveals the value: %v", err)
	}
}
