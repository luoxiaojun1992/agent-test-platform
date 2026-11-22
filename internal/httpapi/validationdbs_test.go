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
	validationdb "agent-test-platform/internal/validationdb"
)

// newTestValidator compiles the embedded contracts once per test.
func newTestValidator(t *testing.T) *contracts.Validator {
	t.Helper()
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	return validator
}

type fakeTargetService struct {
	input       validationdb.Input
	key         string
	registerErr error
	registered  validationdb.Target
	listCursor  string
	listLimit   int
	items       []validationdb.Target
	nextCursor  string
	listErr     error
}

func (service *fakeTargetService) Register(_ context.Context, key string, input validationdb.Input) (validationdb.Target, error) {
	service.key, service.input = key, input
	return service.registered, service.registerErr
}

func (service *fakeTargetService) List(_ context.Context, cursor string, limit int) ([]validationdb.Target, string, error) {
	service.listCursor, service.listLimit = cursor, limit
	return service.items, service.nextCursor, service.listErr
}

var targetTestTime = time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)

const validTargetBody = `{"name":"target-a","host":"target-a.internal","port":3306,"admin_username":"atp_admin",` +
	`"admin_password_secret_name":"target-a-admin-password","tls_mode":"DISABLED_LOCAL_ONLY"}`

func newTargetTestServer(t *testing.T, service TargetService) *Server {
	t.Helper()
	server, err := NewServer(&fakeGenerationService{}, &fakeSecretService{}, service, &fakeAgentService{}, &fakeCatalogService{}, &fakeRunService{}, newTestValidator(t), "api-secret")
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func TestCreateTargetStoresRegistrationAndReturnsNoPassword(t *testing.T) {
	t.Parallel()

	fake := &fakeTargetService{registered: validationdb.Target{ID: "0b6d6f3e-2c4a-4f8e-9a1b-7c5d3e2f1a90", Input: validationdb.Input{
		Name: "target-a", Host: "target-a.internal", Port: 3306, AdminUsername: "atp_admin",
		AdminPasswordSecretName: "target-a-admin-password", TLSMode: validationdb.TLSDisabledLocalOnly,
	}, CreatedAt: targetTestTime}}
	server := newTargetTestServer(t, fake)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/validation-mysql", strings.NewReader(validTargetBody))
	request.Header.Set("Idempotency-Key", "register-target-a")
	authorize(request)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	if response.Code != http.StatusCreated || fake.key != "register-target-a" || fake.input.AdminPasswordSecretName != "target-a-admin-password" {
		t.Fatalf("status = %d key = %q input = %#v body = %s", response.Code, fake.key, fake.input, response.Body)
	}
	if strings.Contains(response.Body.String(), `"admin_password"`) || strings.Contains(response.Body.String(), "secret-value") {
		t.Fatalf("response carries a password field or value: %s", response.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["admin_password_secret_name"] != "target-a-admin-password" || body["id"] == nil || body["created_at"] == nil {
		t.Fatalf("body = %v", body)
	}
}

func TestCreateTargetRejectsInvalidRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  string
		body string
		code string
	}{
		{name: "missing idempotency key", body: validTargetBody, code: "IDEMPOTENCY_KEY_INVALID"},
		{name: "extra field carries a password", key: "register-extra", body: strings.Replace(validTargetBody, `"tls_mode"`, `"admin_password":"s3cret","tls_mode"`, 1), code: "SCHEMA_INVALID"},
		{name: "port out of range", key: "register-port", body: strings.Replace(validTargetBody, `"port":3306`, `"port":70000`, 1), code: "SCHEMA_INVALID"},
		{name: "unknown tls mode", key: "register-tls", body: strings.Replace(validTargetBody, "DISABLED_LOCAL_ONLY", "PREFERRED", 1), code: "SCHEMA_INVALID"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := newTargetTestServer(t, &fakeTargetService{})
			request := httptest.NewRequest(http.MethodPost, "/api/v1/validation-mysql", strings.NewReader(test.body))
			if test.key != "" {
				request.Header.Set("Idempotency-Key", test.key)
			}
			authorize(request)
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"`+test.code+`"`) {
				t.Fatalf("status = %d body = %s", response.Code, response.Body)
			}
			if strings.Contains(response.Body.String(), "s3cret") {
				t.Fatalf("rejected request echoed a value: %s", response.Body)
			}
		})
	}
}

func TestCreateTargetMapsServiceErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		err    error
		status int
		code   string
	}{
		{err: validationdb.ErrSecretNotFound, status: http.StatusBadRequest, code: "SECRET_NOT_FOUND"},
		{err: validationdb.ErrSameAsPlatform, status: http.StatusBadRequest, code: "VALIDATION_DB_SAME_AS_PLATFORM"},
		{err: validationdb.ErrCACertificateInvalid, status: http.StatusBadRequest, code: "CA_CERTIFICATE_INVALID"},
		{err: validationdb.ErrConflict, status: http.StatusConflict, code: "CONFLICT"},
		{err: validationdb.ErrIdempotencyConflict, status: http.StatusConflict, code: "CONFLICT"},
		{err: validationdb.ErrInvalidPort, status: http.StatusBadRequest, code: "SCHEMA_INVALID"},
	}
	for _, test := range tests {
		server := newTargetTestServer(t, &fakeTargetService{registerErr: test.err})
		request := httptest.NewRequest(http.MethodPost, "/api/v1/validation-mysql", strings.NewReader(validTargetBody))
		request.Header.Set("Idempotency-Key", "register-error")
		authorize(request)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != test.status || !strings.Contains(response.Body.String(), `"code":"`+test.code+`"`) {
			t.Fatalf("%v: status = %d body = %s", test.err, response.Code, response.Body)
		}
	}
}

func TestCreateTargetRequiresAPIToken(t *testing.T) {
	t.Parallel()

	server := newTargetTestServer(t, &fakeTargetService{})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/validation-mysql", strings.NewReader(validTargetBody))
	request.Header.Set("Idempotency-Key", "register-anon")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestListTargetsPaginatesAndValidatesParameters(t *testing.T) {
	t.Parallel()

	fake := &fakeTargetService{
		items:      []validationdb.Target{{ID: "0b6d6f3e-2c4a-4f8e-9a1b-7c5d3e2f1a90", Input: validationdb.Input{Name: "target-a"}, CreatedAt: targetTestTime}},
		nextCursor: "Y3Vyc29y",
	}
	server := newTargetTestServer(t, fake)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/validation-mysql?cursor=abc&limit=25", nil)
	authorize(request)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || fake.listCursor != "abc" || fake.listLimit != 25 ||
		!strings.Contains(response.Body.String(), `"next_cursor":"Y3Vyc29y"`) {
		t.Fatalf("status = %d cursor = %q limit = %d body = %s", response.Code, fake.listCursor, fake.listLimit, response.Body)
	}

	empty := newTargetTestServer(t, &fakeTargetService{})
	defaults := httptest.NewRequest(http.MethodGet, "/api/v1/validation-mysql", nil)
	authorize(defaults)
	defaultResponse := httptest.NewRecorder()
	empty.ServeHTTP(defaultResponse, defaults)
	if defaultResponse.Code != http.StatusOK || !strings.Contains(defaultResponse.Body.String(), `"items":[]`) {
		t.Fatalf("empty page = %s", defaultResponse.Body)
	}

	for _, limit := range []string{"0", "201", "many"} {
		invalid := httptest.NewRequest(http.MethodGet, "/api/v1/validation-mysql?limit="+limit, nil)
		authorize(invalid)
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, invalid)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("limit %s: status = %d", limit, recorder.Code)
		}
	}
	fake.listErr = validationdb.ErrInvalidCursor
	foreign := httptest.NewRequest(http.MethodGet, "/api/v1/validation-mysql?cursor=foreign", nil)
	authorize(foreign)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, foreign)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `"code":"CURSOR_INVALID"`) {
		t.Fatalf("foreign cursor: status = %d body = %s", recorder.Code, recorder.Body)
	}
}
