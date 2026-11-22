package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	validationdb "agent-test-platform/internal/validationdb"
)

const targetRequestSchema = "schemas/validation-mysql-request.schema.json"

// TargetService registers and lists validation MySQL instances.
type TargetService interface {
	Register(context.Context, string, validationdb.Input) (validationdb.Target, error)
	List(context.Context, string, int) ([]validationdb.Target, string, error)
}

// createTarget registers a validation MySQL instance (docs/architecture.md section 16.5). The response holds
// the endpoint, the admin secret's name, the TLS mode, and the public CA certificate; it never holds the
// admin password.
func (server *Server) createTarget(writer http.ResponseWriter, request *http.Request) {
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
	if err := server.validator.ValidateJSON(targetRequestSchema, body); err != nil {
		writeError(writer, http.StatusBadRequest, "SCHEMA_INVALID", err.Error(), false, nil)
		return
	}
	var input validationdb.Input
	if err := json.Unmarshal(body, &input); err != nil {
		writeError(writer, http.StatusBadRequest, "SCHEMA_INVALID", "request does not match the Target schema", false, nil)
		return
	}
	registered, err := server.targetService.Register(request.Context(), idempotencyKey, input)
	if err != nil {
		server.writeTargetError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, registered)
}

// listTargets applies the API list rules: cursor pagination, default 50, maximum 200.
func (server *Server) listTargets(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	limit := 50
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > validationdb.MaxPageSize {
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
	items, next, err := server.targetService.List(request.Context(), cursor, limit)
	if errors.Is(err, validationdb.ErrInvalidCursor) {
		writeError(writer, http.StatusBadRequest, "CURSOR_INVALID", "cursor was not issued by this API", false, nil)
		return
	}
	if err != nil {
		server.writeTargetError(writer, err)
		return
	}
	if items == nil {
		items = []validationdb.Target{}
	}
	var nextCursor any
	if next != "" {
		nextCursor = next
	}
	writeJSON(writer, http.StatusOK, map[string]any{"items": items, "next_cursor": nextCursor})
}

// writeTargetError maps registration errors to documented error codes. Validation messages name fields
// and rules only, so they are safe to return.
func (server *Server) writeTargetError(writer http.ResponseWriter, err error) {
	switch {
	case validationdb.IsInvalidInput(err):
		writeError(writer, http.StatusBadRequest, "SCHEMA_INVALID", err.Error(), false, nil)
	case errors.Is(err, validationdb.ErrSecretNotFound):
		writeError(writer, http.StatusBadRequest, "SECRET_NOT_FOUND", err.Error(), false, nil)
	case errors.Is(err, validationdb.ErrSameAsPlatform):
		writeError(writer, http.StatusBadRequest, "VALIDATION_DB_SAME_AS_PLATFORM", err.Error(), false, nil)
	case errors.Is(err, validationdb.ErrCACertificateInvalid):
		writeError(writer, http.StatusBadRequest, "CA_CERTIFICATE_INVALID", err.Error(), false, nil)
	case errors.Is(err, validationdb.ErrConflict), errors.Is(err, validationdb.ErrIdempotencyConflict):
		writeError(writer, http.StatusConflict, "CONFLICT", err.Error(), false, nil)
	case errors.Is(err, validationdb.ErrInvalidLimit):
		writeError(writer, http.StatusBadRequest, "SCHEMA_INVALID", err.Error(), false, nil)
	default:
		writeError(writer, http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error", true, nil)
	}
}
