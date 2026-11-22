package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agent-test-platform/contracts"
	"agent-test-platform/internal/secrets"
)

type fakeSecretService struct {
	putName    string
	putValue   string
	putResult  secrets.Metadata
	listCursor string
	listLimit  int
	items      []secrets.Metadata
	nextCursor string
	listErr    error
}

// Put applies the same validation as secrets.Service, so these tests exercise the real error mapping.
func (service *fakeSecretService) Put(_ context.Context, name, value string) (secrets.Metadata, error) {
	service.putName, service.putValue = name, value
	if err := secrets.ValidateName(name); err != nil {
		return secrets.Metadata{}, err
	}
	if err := secrets.ValidateValue(value); err != nil {
		return secrets.Metadata{}, err
	}
	return service.putResult, nil
}

func (service *fakeSecretService) List(_ context.Context, cursor string, limit int) ([]secrets.Metadata, string, error) {
	service.listCursor, service.listLimit = cursor, limit
	return service.items, service.nextCursor, service.listErr
}

var secretTestTime = time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)

func newSecretTestServer(t *testing.T, service SecretService) *Server {
	t.Helper()
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(&fakeGenerationService{}, service, &fakeTargetService{}, &fakeAgentService{}, &fakeCatalogService{}, &fakeRunService{}, validator, "api-secret")
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func TestPutSecretStoresValueAndReturnsMetadataOnly(t *testing.T) {
	t.Parallel()

	fake := &fakeSecretService{putResult: secrets.Metadata{Name: "sales-reader-password", CreatedAt: secretTestTime, UpdatedAt: secretTestTime}}
	server := newSecretTestServer(t, fake)
	request := httptest.NewRequest(http.MethodPut, "/api/v1/secrets/sales-reader-password", strings.NewReader(`{"value":"s3cret-value"}`))
	authorize(request)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	if response.Code != http.StatusOK || fake.putName != "sales-reader-password" || fake.putValue != "s3cret-value" {
		t.Fatalf("status = %d name = %q value = %q body = %s", response.Code, fake.putName, fake.putValue, response.Body)
	}
	if strings.Contains(response.Body.String(), "s3cret-value") {
		t.Fatalf("response echoed the secret value: %s", response.Body)
	}
	var metadata map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["name"] != "sales-reader-password" || metadata["created_at"] == nil || metadata["updated_at"] == nil || len(metadata) != 3 {
		t.Fatalf("metadata = %v", metadata)
	}
}

func TestPutSecretRejectsInvalidRequestsWithoutEchoingValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		body string
	}{
		{name: "invalid name", path: "/api/v1/secrets/Sales_Reader", body: `{"value":"s3cret"}`},
		{name: "missing value", path: "/api/v1/secrets/reader", body: `{}`},
		{name: "number value", path: "/api/v1/secrets/reader", body: `{"value":12345}`},
		{name: "extra field", path: "/api/v1/secrets/reader", body: `{"value":"s3cret","note":"x"}`},
		{name: "blank value", path: "/api/v1/secrets/reader", body: `{"value":"   "}`},
		{name: "oversized value", path: "/api/v1/secrets/reader", body: `{"value":"` + strings.Repeat("x", secrets.MaxValueBytes+1) + `"}`},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := newSecretTestServer(t, &fakeSecretService{})
			request := httptest.NewRequest(http.MethodPut, test.path, strings.NewReader(test.body))
			authorize(request)
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)

			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"SCHEMA_INVALID"`) {
				t.Fatalf("status = %d body = %s", response.Code, response.Body)
			}
			if strings.Contains(response.Body.String(), "s3cret") || strings.Contains(response.Body.String(), "12345") {
				t.Fatalf("rejected request echoed its value: %s", response.Body)
			}
		})
	}
}

func TestPutSecretRequiresAPIToken(t *testing.T) {
	t.Parallel()

	server := newSecretTestServer(t, &fakeSecretService{})
	request := httptest.NewRequest(http.MethodPut, "/api/v1/secrets/reader", strings.NewReader(`{"value":"s3cret"}`))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestListSecretsPaginatesMetadataOnly(t *testing.T) {
	t.Parallel()

	fake := &fakeSecretService{
		items:      []secrets.Metadata{{Name: "reader", CreatedAt: secretTestTime, UpdatedAt: secretTestTime}},
		nextCursor: "cmVhZGVy",
	}
	server := newSecretTestServer(t, fake)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/secrets?cursor=abc&limit=25", nil)
	authorize(request)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	if response.Code != http.StatusOK || fake.listCursor != "abc" || fake.listLimit != 25 {
		t.Fatalf("status = %d cursor = %q limit = %d", response.Code, fake.listCursor, fake.listLimit)
	}
	var page struct {
		Items      []map[string]any `json:"items"`
		NextCursor *string          `json:"next_cursor"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.NextCursor == nil || *page.NextCursor != "cmVhZGVy" {
		t.Fatalf("page = %s", response.Body)
	}
	for key := range page.Items[0] {
		if key != "name" && key != "created_at" && key != "updated_at" {
			t.Fatalf("list item exposes %q", key)
		}
	}
}

func TestListSecretsDefaultsAndValidatesParameters(t *testing.T) {
	t.Parallel()

	fake := &fakeSecretService{}
	server := newSecretTestServer(t, fake)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/secrets", nil)
	authorize(request)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	if response.Code != http.StatusOK || fake.listLimit != 50 ||
		!strings.Contains(response.Body.String(), `"items":[]`) || !strings.Contains(response.Body.String(), `"next_cursor":null`) {
		t.Fatalf("status = %d limit = %d body = %s", response.Code, fake.listLimit, response.Body)
	}
	for _, limit := range []string{"0", "201", "many"} {
		invalid := httptest.NewRequest(http.MethodGet, "/api/v1/secrets?limit="+limit, nil)
		authorize(invalid)
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, invalid)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("limit %s: status = %d", limit, recorder.Code)
		}
	}
	tooLong := httptest.NewRequest(http.MethodGet, "/api/v1/secrets?cursor="+strings.Repeat("c", 513), nil)
	authorize(tooLong)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, tooLong)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `"code":"CURSOR_INVALID"`) {
		t.Fatalf("long cursor: status = %d body = %s", recorder.Code, recorder.Body)
	}
	fake.listErr = secrets.ErrInvalidCursor
	foreign := httptest.NewRequest(http.MethodGet, "/api/v1/secrets?cursor=foreign", nil)
	authorize(foreign)
	recorder = httptest.NewRecorder()
	server.ServeHTTP(recorder, foreign)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `"code":"CURSOR_INVALID"`) {
		t.Fatalf("foreign cursor: status = %d body = %s", recorder.Code, recorder.Body)
	}
}
