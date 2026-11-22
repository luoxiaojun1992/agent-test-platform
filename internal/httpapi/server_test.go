package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agent-test-platform/contracts"
	"agent-test-platform/internal/generation"
)

type fakeGenerationService struct {
	createInput CreateGenerationJobInput
	createJob   GenerationJob
	createErr   error
	getID       string
	getJob      GenerationJob
	getErr      error
	cancelID    string
	cancelKey   string
	cancelJob   GenerationJob
	cancelErr   error
	reviewID    string
	reviewKey   string
	reviewInput generation.ReviewInput
	reviewJob   GenerationJob
	reviewErr   error
	reviewCalls int
	draftID     string
	draft       generation.DraftView
	draftErr    error
	packageID   string
	pkg         generation.Package
	packageErr  error
	profileKey  string
	profileBody []byte
	profile     generation.DatabaseProfile
	profileErr  error
	listCursor  string
	listLimit   int
	profiles    []generation.DatabaseProfile
	nextCursor  string
	listErr     error
}

func (service *fakeGenerationService) CreateGenerationJob(_ context.Context, input CreateGenerationJobInput) (GenerationJob, error) {
	service.createInput = input
	return service.createJob, service.createErr
}

func (service *fakeGenerationService) GetGenerationJob(_ context.Context, id string) (GenerationJob, error) {
	service.getID = id
	return service.getJob, service.getErr
}

func (service *fakeGenerationService) CancelGenerationJob(_ context.Context, id, key string) (GenerationJob, error) {
	service.cancelID, service.cancelKey = id, key
	return service.cancelJob, service.cancelErr
}

func (service *fakeGenerationService) ReviewGenerationJob(_ context.Context, id, key string, input generation.ReviewInput) (GenerationJob, error) {
	service.reviewCalls++
	service.reviewID, service.reviewKey, service.reviewInput = id, key, input
	return service.reviewJob, service.reviewErr
}

func (service *fakeGenerationService) GetGenerationJobDraft(_ context.Context, id string) (generation.DraftView, error) {
	service.draftID = id
	return service.draft, service.draftErr
}

func (service *fakeGenerationService) GetGenerationPackage(_ context.Context, id string) (generation.Package, error) {
	service.packageID = id
	return service.pkg, service.packageErr
}

func (service *fakeGenerationService) CreateDatabaseProfile(_ context.Context, key string, body json.RawMessage) (generation.DatabaseProfile, error) {
	service.profileKey, service.profileBody = key, append([]byte(nil), body...)
	return service.profile, service.profileErr
}

func (service *fakeGenerationService) ListDatabaseProfiles(_ context.Context, cursor string, limit int) ([]generation.DatabaseProfile, string, error) {
	service.listCursor, service.listLimit = cursor, limit
	return service.profiles, service.nextCursor, service.listErr
}

func TestGetGenerationJobDraft(t *testing.T) {
	t.Parallel()

	service := &fakeGenerationService{draft: generation.DraftView{
		JobID: apiJobID, Kind: generation.KindScenario,
		Scenario: json.RawMessage(`{"domain":"e-commerce"}`),
	}}
	server := newTestServer(t, service)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/generation-jobs/"+apiJobID+"/draft", nil)
	authorize(request)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	if response.Code != http.StatusOK || service.draftID != apiJobID {
		t.Fatalf("status = %d id=%q body=%s", response.Code, service.draftID, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	scenario, _ := body["scenario"].(map[string]any)
	if body["job_id"] != apiJobID || body["job_kind"] != "SCENARIO" || scenario["domain"] != "e-commerce" {
		t.Fatalf("body = %#v", body)
	}
	if _, present := body["dataset"]; present {
		t.Fatalf("unrelated draft sections must be omitted: %#v", body)
	}
}

func TestGetGenerationJobDraftMapsErrorsAndRequiresAuth(t *testing.T) {
	t.Parallel()

	for err, status := range map[error]int{ErrNotFound: http.StatusNotFound, ErrConflict: http.StatusConflict} {
		server := newTestServer(t, &fakeGenerationService{draftErr: err})
		request := httptest.NewRequest(http.MethodGet, "/api/v1/generation-jobs/"+apiJobID+"/draft", nil)
		authorize(request)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != status {
			t.Errorf("error %v status = %d, want %d", err, response.Code, status)
		}
	}

	server := newTestServer(t, &fakeGenerationService{})
	unauthorized := httptest.NewRecorder()
	server.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/v1/generation-jobs/"+apiJobID+"/draft", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}
}

func TestGetGenerationPackage(t *testing.T) {
	t.Parallel()

	manifest := json.RawMessage(`{"schema_version":"1.0"}`)
	service := &fakeGenerationService{pkg: generation.Package{
		JobID: apiJobID, ScenarioRevisionID: apiRevisionID, DatasetRevisionID: apiRevisionID,
		TestCaseRevisionIDs: []string{apiRevisionID}, Manifest: manifest, ManifestSHA256: apiDigest,
	}}
	server := newTestServer(t, service)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/generation-jobs/"+apiJobID+"/package", nil)
	authorize(request)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	if response.Code != http.StatusOK || service.packageID != apiJobID {
		t.Fatalf("status = %d id=%q body=%s", response.Code, service.packageID, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["job_id"] != apiJobID || body["manifest_sha256"] != apiDigest || len(body["test_case_revision_ids"].([]any)) != 1 {
		t.Fatalf("body = %#v", body)
	}
	if manifestBody, _ := body["manifest"].(map[string]any); manifestBody["schema_version"] != "1.0" {
		t.Fatalf("manifest = %#v", body["manifest"])
	}
}

func TestGetGenerationPackageMapsNotFoundAndRequiresAuth(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, &fakeGenerationService{packageErr: ErrNotFound})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/generation-jobs/"+apiJobID+"/package", nil)
	authorize(request)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing package status = %d, want 404", response.Code)
	}

	unauthorized := httptest.NewRecorder()
	server.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/v1/generation-jobs/"+apiJobID+"/package", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}
}

func TestReviewGenerationJob(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	service := &fakeGenerationService{reviewJob: GenerationJob{ID: apiJobID, Status: "PUBLISHING", CreatedAt: now, UpdatedAt: now}}
	server := newTestServer(t, service)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/generation-jobs/"+apiJobID+"/review", strings.NewReader(`{"decision":"APPROVE","note":"data looks right"}`))
	authorize(request)
	request.Header.Set("Idempotency-Key", "review-key-1")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if service.reviewID != apiJobID || service.reviewKey != "review-key-1" ||
		service.reviewInput.Decision != generation.ReviewApprove || service.reviewInput.Note != "data looks right" {
		t.Fatalf("review = %q %q %#v", service.reviewID, service.reviewKey, service.reviewInput)
	}
}

func TestReviewGenerationJobRejectsInvalidInputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  string
		body string
		code string
	}{
		{name: "missing idempotency", body: `{"decision":"APPROVE"}`, code: "IDEMPOTENCY_KEY_INVALID"},
		{name: "empty body", key: "review-key-1", body: ``, code: "REQUEST_BODY_INVALID"},
		{name: "unknown decision", key: "review-key-1", body: `{"decision":"MAYBE"}`, code: "SCHEMA_INVALID"},
		{name: "missing decision", key: "review-key-1", body: `{"note":"hi"}`, code: "SCHEMA_INVALID"},
		{name: "unknown field", key: "review-key-1", body: `{"decision":"APPROVE","reviewer":"someone"}`, code: "SCHEMA_INVALID"},
		{name: "trailing data", key: "review-key-1", body: `{"decision":"APPROVE"} {}`, code: "SCHEMA_INVALID"},
		{name: "note too long", key: "review-key-1", body: `{"decision":"REJECT","note":"` + strings.Repeat("x", generation.MaxReviewNoteBytes+1) + `"}`, code: "SCHEMA_INVALID"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			service := &fakeGenerationService{}
			server := newTestServer(t, service)
			request := httptest.NewRequest(http.MethodPost, "/api/v1/generation-jobs/"+apiJobID+"/review", strings.NewReader(test.body))
			authorize(request)
			if test.key != "" {
				request.Header.Set("Idempotency-Key", test.key)
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
			}
			var failure ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil || failure.Code != test.code {
				t.Fatalf("failure = %#v err=%v", failure, err)
			}
			if service.reviewCalls != 0 {
				t.Fatalf("invalid review reached the service")
			}
		})
	}
}

func TestReviewGenerationJobMapsServiceErrorsAndRequiresAuth(t *testing.T) {
	t.Parallel()

	for err, status := range map[error]int{ErrNotFound: http.StatusNotFound, ErrConflict: http.StatusConflict} {
		server := newTestServer(t, &fakeGenerationService{reviewErr: err})
		request := httptest.NewRequest(http.MethodPost, "/api/v1/generation-jobs/"+apiJobID+"/review", strings.NewReader(`{"decision":"REJECT"}`))
		authorize(request)
		request.Header.Set("Idempotency-Key", "review-key-1")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != status {
			t.Errorf("error %v status = %d, want %d", err, response.Code, status)
		}
	}

	server := newTestServer(t, &fakeGenerationService{})
	unauthorized := httptest.NewRecorder()
	server.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/api/v1/generation-jobs/"+apiJobID+"/review", strings.NewReader(`{"decision":"APPROVE"}`)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}
}

func TestCreateGenerationJob(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	service := &fakeGenerationService{createJob: GenerationJob{ID: apiJobID, Status: "PENDING", RequestedTestCaseCount: 10, CreatedAt: now, UpdatedAt: now}}
	server := newTestServer(t, service)
	body := []byte(`{"validation_db_ids":["0b6d6f3e-2c4a-4f8e-9a1b-7c5d3e2f1a90"],"description":"Generate order cancellation tests."}`)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/generation-jobs", bytes.NewReader(body))
	authorize(request)
	request.Header.Set("Idempotency-Key", "request-key-1")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if service.createInput.IdempotencyKey != "request-key-1" || service.createInput.RequestedTestCaseCount != 10 || service.createInput.DescriptionTokens != EstimateDescriptionTokens("Generate order cancellation tests.") {
		t.Fatalf("input = %#v", service.createInput)
	}
}

func TestGenerationAPIAuthenticationAndHealth(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, &fakeGenerationService{})
	health := httptest.NewRecorder()
	server.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d", health.Code)
	}

	unauthorized := httptest.NewRecorder()
	server.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/v1/generation-jobs/"+apiJobID, nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}
}

func TestCreateGenerationJobRejectsInvalidInputs(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, &fakeGenerationService{})
	tests := []struct {
		name string
		key  string
		body string
		code string
	}{
		{name: "missing idempotency", body: `{"description":"valid"}`, code: "IDEMPOTENCY_KEY_INVALID"},
		{name: "schema", key: "request-key-1", body: `{"description":""}`, code: "SCHEMA_INVALID"},
		{name: "token limit", key: "request-key-1", body: `{"validation_db_ids":["0b6d6f3e-2c4a-4f8e-9a1b-7c5d3e2f1a90"],"description":"` + strings.Repeat("界", 1001) + `"}`, code: "INPUT_TOKEN_LIMIT_EXCEEDED"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := httptest.NewRequest(http.MethodPost, "/api/v1/generation-jobs", strings.NewReader(test.body))
			authorize(request)
			if test.key != "" {
				request.Header.Set("Idempotency-Key", test.key)
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
			}
			var failure ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil || failure.Code != test.code {
				t.Fatalf("failure = %#v err=%v", failure, err)
			}
		})
	}
}

func TestGetAndCancelGenerationJob(t *testing.T) {
	t.Parallel()

	job := GenerationJob{ID: apiJobID, Status: "CANCELLED", RequestedTestCaseCount: 3, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	service := &fakeGenerationService{getJob: job, cancelJob: job}
	server := newTestServer(t, service)

	getRequest := httptest.NewRequest(http.MethodGet, "/api/v1/generation-jobs/"+apiJobID, nil)
	authorize(getRequest)
	getResponse := httptest.NewRecorder()
	server.ServeHTTP(getResponse, getRequest)
	if getResponse.Code != http.StatusOK || service.getID != apiJobID {
		t.Fatalf("GET status=%d id=%s", getResponse.Code, service.getID)
	}

	cancelRequest := httptest.NewRequest(http.MethodPost, "/api/v1/generation-jobs/"+apiJobID+"/cancel", nil)
	authorize(cancelRequest)
	cancelRequest.Header.Set("Idempotency-Key", "cancel-key-1")
	cancelResponse := httptest.NewRecorder()
	server.ServeHTTP(cancelResponse, cancelRequest)
	if cancelResponse.Code != http.StatusAccepted || service.cancelID != apiJobID || service.cancelKey != "cancel-key-1" {
		t.Fatalf("cancel status=%d id=%s key=%s", cancelResponse.Code, service.cancelID, service.cancelKey)
	}
}

func TestGenerationServiceErrorMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		err    error
		status int
	}{
		{err: ErrNotFound, status: http.StatusNotFound},
		{err: ErrIdempotencyConflict, status: http.StatusConflict},
		{err: errors.New("database unavailable"), status: http.StatusInternalServerError},
	}
	for _, test := range tests {
		service := &fakeGenerationService{getErr: test.err}
		server := newTestServer(t, service)
		request := httptest.NewRequest(http.MethodGet, "/api/v1/generation-jobs/"+apiJobID, nil)
		authorize(request)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != test.status {
			t.Errorf("error %v status=%d want=%d", test.err, response.Code, test.status)
		}
	}
}

const apiJobID = "01K4Z6Y7X8W9V0T1S2R3Q4P5N6"
const apiRevisionID = "01K4Z6Y7X8W9V0T1S2R3Q4P5NB"
const apiDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func newTestServer(t *testing.T, service GenerationService) *Server {
	t.Helper()
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(service, &fakeSecretService{}, &fakeTargetService{}, &fakeAgentService{}, &fakeCatalogService{}, &fakeRunService{}, validator, "api-secret")
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func authorize(request *http.Request) {
	request.Header.Set("Authorization", "Bearer api-secret")
}

const validDatabaseProfileBody = `{"name":"reference-sales","engine":"MYSQL","host":"target-mysql","port":3306,` +
	`"database":"reference_sales","username":"atp_reader","password_secret_ref":"sales-reader-password",` +
	`"tls_mode":"VERIFY_IDENTITY","snapshot_mode":"SCHEMA_AND_DATA","snapshot_timeout_seconds":120}`

func TestCreateDatabaseProfile(t *testing.T) {
	t.Parallel()

	service := &fakeGenerationService{profile: generation.DatabaseProfile{ID: apiJobID, Name: "reference-sales"}}
	server := newTestServer(t, service)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/database-profiles", strings.NewReader(validDatabaseProfileBody))
	authorize(request)
	request.Header.Set("Idempotency-Key", "profile-create-1")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	if response.Code != http.StatusCreated || service.profileKey != "profile-create-1" || !strings.Contains(string(service.profileBody), `"password_secret_ref"`) {
		t.Fatalf("status = %d key=%q body=%s", response.Code, service.profileKey, response.Body.String())
	}
}

func TestCreateDatabaseProfileRejectsInvalidRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  string
		body string
		code string
	}{
		{name: "missing idempotency key", body: validDatabaseProfileBody, code: "IDEMPOTENCY_KEY_INVALID"},
		{name: "unknown field", key: "profile-create-2", body: strings.Replace(validDatabaseProfileBody, `"engine"`, `"password":"plain","engine"`, 1), code: "SCHEMA_INVALID"},
		{name: "plaintext password is not a secret reference", key: "profile-create-3", body: strings.Replace(validDatabaseProfileBody, `"tls_mode"`, `"tls_mode_extra":1,"tls_mode"`, 1), code: "SCHEMA_INVALID"},
		{name: "unknown snapshot mode", key: "profile-create-4", body: strings.Replace(validDatabaseProfileBody, "SCHEMA_AND_DATA", "EVERYTHING", 1), code: "SCHEMA_INVALID"},
		{name: "timeout above limit", key: "profile-create-5", body: strings.Replace(validDatabaseProfileBody, `"snapshot_timeout_seconds":120`, `"snapshot_timeout_seconds":301`, 1), code: "SCHEMA_INVALID"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			service := &fakeGenerationService{}
			server := newTestServer(t, service)
			request := httptest.NewRequest(http.MethodPost, "/api/v1/database-profiles", strings.NewReader(test.body))
			authorize(request)
			if test.key != "" {
				request.Header.Set("Idempotency-Key", test.key)
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || service.profileKey != "" {
				t.Fatalf("status = %d key=%q body=%s", response.Code, service.profileKey, response.Body.String())
			}
			var body ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Code != test.code {
				t.Fatalf("error body = %#v, want code %s", body, test.code)
			}
		})
	}
}

func TestCreateDatabaseProfileMapsConflicts(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, &fakeGenerationService{profileErr: ErrConflict})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/database-profiles", strings.NewReader(validDatabaseProfileBody))
	authorize(request)
	request.Header.Set("Idempotency-Key", "profile-create-6")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d, want 409", response.Code)
	}

	unauthorized := httptest.NewRecorder()
	server.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/api/v1/database-profiles", strings.NewReader(validDatabaseProfileBody)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}
}

func TestListDatabaseProfilesPaginates(t *testing.T) {
	t.Parallel()

	service := &fakeGenerationService{nextCursor: "next-page"}
	server := newTestServer(t, service)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/database-profiles?cursor=abc&limit=25", nil)
	authorize(request)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	if response.Code != http.StatusOK || service.listCursor != "abc" || service.listLimit != 25 {
		t.Fatalf("status = %d cursor=%q limit=%d", response.Code, service.listCursor, service.listLimit)
	}
	var body struct {
		Items      []map[string]any `json:"items"`
		NextCursor *string          `json:"next_cursor"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Items == nil || body.NextCursor == nil || *body.NextCursor != "next-page" {
		t.Fatalf("body = %s", response.Body.String())
	}
}

func TestListDatabaseProfilesDefaultsAndValidatesLimit(t *testing.T) {
	t.Parallel()

	service := &fakeGenerationService{}
	server := newTestServer(t, service)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/database-profiles", nil)
	authorize(request)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || service.listLimit != 50 {
		t.Fatalf("default limit = %d status = %d", service.listLimit, response.Code)
	}
	if !strings.Contains(response.Body.String(), `"items":[]`) || !strings.Contains(response.Body.String(), `"next_cursor":null`) {
		t.Fatalf("empty page body = %s", response.Body.String())
	}

	for _, limit := range []string{"0", "201", "many"} {
		invalid := httptest.NewRequest(http.MethodGet, "/api/v1/database-profiles?limit="+limit, nil)
		authorize(invalid)
		invalidResponse := httptest.NewRecorder()
		server.ServeHTTP(invalidResponse, invalid)
		if invalidResponse.Code != http.StatusBadRequest {
			t.Fatalf("limit %s status = %d, want 400", limit, invalidResponse.Code)
		}
	}

	service.listErr = generation.ErrInvalidCursor
	badCursor := httptest.NewRequest(http.MethodGet, "/api/v1/database-profiles?cursor=%%%", nil)
	authorize(badCursor)
	badCursorResponse := httptest.NewRecorder()
	server.ServeHTTP(badCursorResponse, badCursor)
	if badCursorResponse.Code != http.StatusBadRequest {
		t.Fatalf("invalid cursor status = %d, want 400", badCursorResponse.Code)
	}
}

func TestCreateGenerationJobPassesKindAndInputRevisions(t *testing.T) {
	t.Parallel()

	const scenario = "01HZZZZZZZZZZZZZZZZZZZZZZ1"
	const dataset = "01HZZZZZZZZZZZZZZZZZZZZZZ2"
	tests := map[string]struct {
		body      string
		status    int
		kind      generation.JobKind
		wantCount int
	}{
		"scenario":        {`{"description":"d","job_kind":"SCENARIO"}`, http.StatusAccepted, generation.KindScenario, 0},
		"dataset":         {`{"description":"d","job_kind":"DATASET","input_scenario_revision_id":"` + scenario + `","validation_db_ids":["0b6d6f3e-2c4a-4f8e-9a1b-7c5d3e2f1a90"]}`, http.StatusAccepted, generation.KindDataset, 0},
		"test cases":      {`{"description":"d","job_kind":"TEST_CASES","test_case_count":4,"input_scenario_revision_id":"` + scenario + `","input_dataset_revision_id":"` + dataset + `"}`, http.StatusAccepted, generation.KindTestCases, 4},
		"dataset missing": {`{"description":"d","job_kind":"TEST_CASES","input_scenario_revision_id":"` + scenario + `"}`, http.StatusBadRequest, "", 0},
		"full package":    {`{"description":"d","validation_db_ids":["0b6d6f3e-2c4a-4f8e-9a1b-7c5d3e2f1a90"]}`, http.StatusAccepted, "", 10},
	}
	for name, test := range tests {
		service := &fakeGenerationService{createJob: GenerationJob{ID: apiJobID, Status: "PENDING"}}
		server := newTestServer(t, service)
		request := httptest.NewRequest(http.MethodPost, "/api/v1/generation-jobs", bytes.NewReader([]byte(test.body)))
		authorize(request)
		request.Header.Set("Idempotency-Key", "request-key-1")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != test.status {
			t.Errorf("%s: status = %d body=%s", name, response.Code, response.Body.String())
			continue
		}
		if test.status != http.StatusAccepted {
			continue
		}
		if service.createInput.Kind != test.kind || service.createInput.RequestedTestCaseCount != test.wantCount {
			t.Errorf("%s: input = %#v", name, service.createInput)
		}
		if test.kind == generation.KindTestCases && (service.createInput.InputScenarioRevisionID == nil || *service.createInput.InputDatasetRevisionID != dataset) {
			t.Errorf("%s: inputs = %#v", name, service.createInput)
		}
	}
}
