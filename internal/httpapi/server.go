package httpapi

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"agent-test-platform/contracts"
	"agent-test-platform/internal/generation"
	"agent-test-platform/internal/pki"
	"agent-test-platform/internal/secrets"
)

const (
	generationRequestSchema      = "schemas/generation-request.schema.json"
	databaseProfileRequestSchema = "schemas/database-profile-request.schema.json"
	secretWriteRequestSchema     = "schemas/secret-write-request.schema.json"
	maxRequestBodyBytes          = 256 * 1024
)

var (
	ErrNotFound            = generation.ErrNotFound
	ErrConflict            = generation.ErrConflict
	ErrIdempotencyConflict = generation.ErrIdempotencyConflict
)

type GenerationService interface {
	CreateGenerationJob(context.Context, generation.CreateJobInput) (generation.Job, error)
	GetGenerationJob(context.Context, string) (generation.Job, error)
	CancelGenerationJob(context.Context, string, string) (generation.Job, error)
	ReviewGenerationJob(context.Context, string, string, generation.ReviewInput) (generation.Job, error)
	GetGenerationJobDraft(context.Context, string) (generation.DraftView, error)
	GetGenerationPackage(context.Context, string) (generation.Package, error)
	CreateDatabaseProfile(context.Context, string, json.RawMessage) (generation.DatabaseProfile, error)
	ListDatabaseProfiles(context.Context, string, int) ([]generation.DatabaseProfile, string, error)
}

// SecretService stores and lists test-environment credentials. It has no method that returns a
// plaintext value, so no handler can echo one.
type SecretService interface {
	Put(context.Context, string, string) (secrets.Metadata, error)
	List(context.Context, string, int) ([]secrets.Metadata, string, error)
}

type CreateGenerationJobInput = generation.CreateJobInput
type GenerationJob = generation.Job

type ErrorResponse struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Retryable bool           `json:"retryable"`
	Details   map[string]any `json:"details,omitempty"`
}

type generationRequest struct {
	Description             string  `json:"description"`
	TestCaseCount           int     `json:"test_case_count"`
	JobKind                 string  `json:"job_kind"`
	InputScenarioRevisionID *string `json:"input_scenario_revision_id"`
	InputDatasetRevisionID  *string `json:"input_dataset_revision_id"`
}

type reviewRequest struct {
	Decision generation.ReviewDecision `json:"decision"`
	Note     string                    `json:"note"`
}

type Server struct {
	handler          http.Handler
	service          GenerationService
	secretService    SecretService
	targetService    TargetService
	agentService     AgentService
	catalogService   CatalogService
	runService       RunService
	modelService     ModelService
	analyticsService AnalyticsService
	toolDiscovery    ToolDiscoveryService
	validator        *contracts.Validator
	apiToken         string
}

func NewServer(service GenerationService, secretService SecretService, targetService TargetService, agentService AgentService, catalogService CatalogService, runService RunService, validator *contracts.Validator, apiToken string) (*Server, error) {
	if service == nil || secretService == nil || targetService == nil || agentService == nil || catalogService == nil || runService == nil || validator == nil || strings.TrimSpace(apiToken) == "" {
		return nil, fmt.Errorf("generation service, secret service, target service, Agent service, contract validator, and API token are required")
	}
	server := &Server{service: service, secretService: secretService, targetService: targetService, agentService: agentService, catalogService: catalogService, runService: runService, validator: validator, apiToken: apiToken}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", server.health)
	mux.Handle("POST /api/v1/generation-jobs", server.authenticate(http.HandlerFunc(server.createGenerationJob)))
	mux.Handle("GET /api/v1/generation-jobs/{jobId}", server.authenticate(http.HandlerFunc(server.getGenerationJob)))
	mux.Handle("POST /api/v1/generation-jobs/{jobId}/cancel", server.authenticate(http.HandlerFunc(server.cancelGenerationJob)))
	mux.Handle("POST /api/v1/generation-jobs/{jobId}/review", server.authenticate(http.HandlerFunc(server.reviewGenerationJob)))
	mux.Handle("GET /api/v1/generation-jobs/{jobId}/draft", server.authenticate(http.HandlerFunc(server.getGenerationJobDraft)))
	mux.Handle("GET /api/v1/generation-jobs/{jobId}/package", server.authenticate(http.HandlerFunc(server.getGenerationPackage)))
	mux.Handle("POST /api/v1/database-profiles", server.authenticate(http.HandlerFunc(server.createDatabaseProfile)))
	mux.Handle("GET /api/v1/database-profiles", server.authenticate(http.HandlerFunc(server.listDatabaseProfiles)))
	mux.Handle("PUT /api/v1/secrets/{name}", server.authenticate(http.HandlerFunc(server.putSecret)))
	mux.Handle("GET /api/v1/secrets", server.authenticate(http.HandlerFunc(server.listSecrets)))
	mux.Handle("POST /api/v1/validation-mysql", server.authenticate(http.HandlerFunc(server.createTarget)))
	mux.Handle("GET /api/v1/validation-mysql", server.authenticate(http.HandlerFunc(server.listTargets)))
	mux.Handle("POST /api/v1/agents", server.authenticate(http.HandlerFunc(server.createAgent)))
	mux.Handle("GET /api/v1/agents", server.authenticate(http.HandlerFunc(server.listAgents)))
	mux.Handle("GET /api/v1/agents/{agentId}", server.authenticate(http.HandlerFunc(server.getAgent)))
	server.registerCatalogRoutes(mux)
	server.registerRunRoutes(mux)
	server.handler = mux
	return server, nil
}

func (server *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	server.handler.ServeHTTP(writer, request)
}

func (server *Server) health(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
}

func (server *Server) createGenerationJob(writer http.ResponseWriter, request *http.Request) {
	idempotencyKey := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if len(idempotencyKey) < 8 || len(idempotencyKey) > 128 {
		writeError(writer, http.StatusBadRequest, "IDEMPOTENCY_KEY_INVALID", "Idempotency-Key must contain 8-128 characters", false, nil)
		return
	}
	body, err := readRequestBody(writer, request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "REQUEST_BODY_INVALID", err.Error(), false, nil)
		return
	}
	if err := server.validator.ValidateJSON(generationRequestSchema, body); err != nil {
		writeError(writer, http.StatusBadRequest, "SCHEMA_INVALID", err.Error(), false, nil)
		return
	}
	var parsed generationRequest
	if err := json.Unmarshal(body, &parsed); err != nil {
		writeError(writer, http.StatusBadRequest, "SCHEMA_INVALID", err.Error(), false, nil)
		return
	}
	tokens := EstimateDescriptionTokens(parsed.Description)
	if tokens > generation.MaxDescriptionTokens {
		writeError(writer, http.StatusBadRequest, "INPUT_TOKEN_LIMIT_EXCEEDED", "description exceeds conservative token limit", false, map[string]any{"actual": tokens, "limit": generation.MaxDescriptionTokens})
		return
	}
	if parsed.TestCaseCount == 0 {
		parsed.TestCaseCount = 10
	}
	kind := generation.JobKind(parsed.JobKind)
	if kind != "" && kind != generation.KindTestCases {
		parsed.TestCaseCount = 0
	}
	job, err := server.service.CreateGenerationJob(request.Context(), generation.CreateJobInput{
		IdempotencyKey: idempotencyKey, Request: append([]byte(nil), body...), Kind: kind,
		InputScenarioRevisionID: parsed.InputScenarioRevisionID, InputDatasetRevisionID: parsed.InputDatasetRevisionID,
		DescriptionTokens: tokens, RequestedTestCaseCount: parsed.TestCaseCount,
	})
	if err != nil {
		server.writeServiceError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, job)
}

func (server *Server) getGenerationJob(writer http.ResponseWriter, request *http.Request) {
	job, err := server.service.GetGenerationJob(request.Context(), request.PathValue("jobId"))
	if err != nil {
		server.writeServiceError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, job)
}

func (server *Server) cancelGenerationJob(writer http.ResponseWriter, request *http.Request) {
	idempotencyKey := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if len(idempotencyKey) < 8 || len(idempotencyKey) > 128 {
		writeError(writer, http.StatusBadRequest, "IDEMPOTENCY_KEY_INVALID", "Idempotency-Key must contain 8-128 characters", false, nil)
		return
	}
	job, err := server.service.CancelGenerationJob(request.Context(), request.PathValue("jobId"), idempotencyKey)
	if err != nil {
		server.writeServiceError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, job)
}

func (server *Server) reviewGenerationJob(writer http.ResponseWriter, request *http.Request) {
	idempotencyKey := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if len(idempotencyKey) < 8 || len(idempotencyKey) > 128 {
		writeError(writer, http.StatusBadRequest, "IDEMPOTENCY_KEY_INVALID", "Idempotency-Key must contain 8-128 characters", false, nil)
		return
	}
	body, err := readRequestBody(writer, request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "REQUEST_BODY_INVALID", err.Error(), false, nil)
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var parsed reviewRequest
	if err := decoder.Decode(&parsed); err != nil {
		writeError(writer, http.StatusBadRequest, "SCHEMA_INVALID", err.Error(), false, nil)
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		writeError(writer, http.StatusBadRequest, "SCHEMA_INVALID", "request body has trailing JSON data", false, nil)
		return
	}
	input := generation.ReviewInput{Decision: parsed.Decision, Note: parsed.Note}
	if err := input.Validate(); err != nil {
		writeError(writer, http.StatusBadRequest, "SCHEMA_INVALID", err.Error(), false, nil)
		return
	}
	job, err := server.service.ReviewGenerationJob(request.Context(), request.PathValue("jobId"), idempotencyKey, input)
	if err != nil {
		server.writeServiceError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, job)
}

func (server *Server) getGenerationJobDraft(writer http.ResponseWriter, request *http.Request) {
	draft, err := server.service.GetGenerationJobDraft(request.Context(), request.PathValue("jobId"))
	if err != nil {
		server.writeServiceError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, draft)
}

func (server *Server) getGenerationPackage(writer http.ResponseWriter, request *http.Request) {
	pkg, err := server.service.GetGenerationPackage(request.Context(), request.PathValue("jobId"))
	if err != nil {
		server.writeServiceError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, pkg)
}

func (server *Server) createDatabaseProfile(writer http.ResponseWriter, request *http.Request) {
	idempotencyKey := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if len(idempotencyKey) < 8 || len(idempotencyKey) > 128 {
		writeError(writer, http.StatusBadRequest, "IDEMPOTENCY_KEY_INVALID", "Idempotency-Key must contain 8-128 characters", false, nil)
		return
	}
	body, err := readRequestBody(writer, request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "REQUEST_BODY_INVALID", err.Error(), false, nil)
		return
	}
	if err := server.validator.ValidateJSON(databaseProfileRequestSchema, body); err != nil {
		writeError(writer, http.StatusBadRequest, "SCHEMA_INVALID", err.Error(), false, nil)
		return
	}
	profile, err := server.service.CreateDatabaseProfile(request.Context(), idempotencyKey, body)
	if err != nil {
		server.writeServiceError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, profile)
}

// listDatabaseProfiles applies the API list rules (docs/technical/05-api-and-implementation-plan.md
// section 2): cursor pagination, default 50, maximum 200.
func (server *Server) listDatabaseProfiles(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	limit := 50
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 200 {
			writeError(writer, http.StatusBadRequest, "SCHEMA_INVALID", "limit must be an integer from 1 to 200", false, nil)
			return
		}
		limit = parsed
	}
	cursor := query.Get("cursor")
	if len(cursor) > 512 {
		writeError(writer, http.StatusBadRequest, "CURSOR_INVALID", "cursor must not exceed 512 characters", false, nil)
		return
	}
	profiles, next, err := server.service.ListDatabaseProfiles(request.Context(), cursor, limit)
	if errors.Is(err, generation.ErrInvalidCursor) {
		writeError(writer, http.StatusBadRequest, "CURSOR_INVALID", "cursor was not issued by this API", false, nil)
		return
	}
	if err != nil {
		server.writeServiceError(writer, err)
		return
	}
	if profiles == nil {
		profiles = []generation.DatabaseProfile{}
	}
	var nextCursor any
	if next != "" {
		nextCursor = next
	}
	writeJSON(writer, http.StatusOK, map[string]any{"items": profiles, "next_cursor": nextCursor})
}

// putSecret stores or replaces a test-environment credential (docs/technical/06-sandbox-and-security.md
// section 8.1). The response carries metadata only, and a rejected request never echoes its value.
func (server *Server) putSecret(writer http.ResponseWriter, request *http.Request) {
	body, err := readRequestBody(writer, request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "REQUEST_BODY_INVALID", err.Error(), false, nil)
		return
	}
	const shapeMessage = "request must be a JSON object whose only field is a string value"
	if err := server.validator.ValidateJSON(secretWriteRequestSchema, body); err != nil {
		writeError(writer, http.StatusBadRequest, "SCHEMA_INVALID", shapeMessage, false, nil)
		return
	}
	var parsed struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		writeError(writer, http.StatusBadRequest, "SCHEMA_INVALID", shapeMessage, false, nil)
		return
	}
	metadata, err := server.secretService.Put(request.Context(), request.PathValue("name"), parsed.Value)
	if errors.Is(err, secrets.ErrInvalidName) || errors.Is(err, secrets.ErrInvalidValue) {
		writeError(writer, http.StatusBadRequest, "SCHEMA_INVALID", err.Error(), false, nil)
		return
	}
	if err != nil {
		server.writeServiceError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, metadata)
}

// listSecrets returns one page of credential metadata. It applies the API list rules: cursor
// pagination, default 50, maximum 200. Values are never part of the response.
func (server *Server) listSecrets(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	limit := 50
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > secrets.MaxPageSize {
			writeError(writer, http.StatusBadRequest, "SCHEMA_INVALID", "limit must be an integer from 1 to 200", false, nil)
			return
		}
		limit = parsed
	}
	cursor := query.Get("cursor")
	if len(cursor) > 512 {
		writeError(writer, http.StatusBadRequest, "CURSOR_INVALID", "cursor must not exceed 512 characters", false, nil)
		return
	}
	items, next, err := server.secretService.List(request.Context(), cursor, limit)
	if errors.Is(err, secrets.ErrInvalidCursor) {
		writeError(writer, http.StatusBadRequest, "CURSOR_INVALID", "cursor was not issued by this API", false, nil)
		return
	}
	if err != nil {
		server.writeServiceError(writer, err)
		return
	}
	if items == nil {
		items = []secrets.Metadata{}
	}
	var nextCursor any
	if next != "" {
		nextCursor = next
	}
	writeJSON(writer, http.StatusOK, map[string]any{"items": items, "next_cursor": nextCursor})
}

func (server *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		provided := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		if len(provided) != len(server.apiToken) || subtle.ConstantTimeCompare([]byte(provided), []byte(server.apiToken)) != 1 {
			writeError(writer, http.StatusUnauthorized, "UNAUTHORIZED", "valid Bearer token required", false, nil)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func (server *Server) writeServiceError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(writer, http.StatusNotFound, "NOT_FOUND", err.Error(), false, nil)
	case errors.Is(err, ErrConflict), errors.Is(err, ErrIdempotencyConflict):
		writeError(writer, http.StatusConflict, "CONFLICT", err.Error(), false, nil)
	case errors.Is(err, generation.ErrInvalidRequest):
		writeError(writer, http.StatusBadRequest, "SCHEMA_INVALID", err.Error(), false, nil)
	case errors.Is(err, generation.ErrTargetNotFound):
		writeError(writer, http.StatusBadRequest, "VALIDATION_DB_NOT_FOUND", err.Error(), false, nil)
	case errors.Is(err, pki.ErrInvalidCA):
		writeError(writer, http.StatusBadRequest, "CA_CERTIFICATE_INVALID", err.Error(), false, nil)
	default:
		writeError(writer, http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error", true, nil)
	}
}

func readRequestBody(writer http.ResponseWriter, request *http.Request) ([]byte, error) {
	reader := http.MaxBytesReader(writer, request.Body, maxRequestBodyBytes)
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("request body is empty")
	}
	return data, nil
}

// EstimateDescriptionTokens is the approved conservative fallback for unknown tokenizers.
func EstimateDescriptionTokens(description string) int {
	return (len([]byte(description))+1)/2 + 16
}

func writeError(writer http.ResponseWriter, status int, code, message string, retryable bool, details map[string]any) {
	writeJSON(writer, status, ErrorResponse{Code: code, Message: message, Retryable: retryable, Details: details})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
