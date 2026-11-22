package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"agent-test-platform/internal/agentregistry"
)

const (
	agentRequestSchema        = "schemas/agent-request.schema.json"
	agentVersionRequestSchema = "schemas/agent-version-request.schema.json"
)

// AgentService manages Agents and Agent Versions.
type AgentService interface {
	CreateAgent(context.Context, string, agentregistry.AgentInput) (agentregistry.Agent, error)
	GetAgent(context.Context, string) (agentregistry.Agent, error)
	ListAgents(context.Context, string, int) ([]agentregistry.Agent, string, error)
	CreateVersion(context.Context, string, string, agentregistry.VersionInput) (agentregistry.Version, error)
	ListVersions(context.Context, string, string, int) ([]agentregistry.Version, string, error)
}

func idempotencyKeyOf(writer http.ResponseWriter, request *http.Request) (string, bool) {
	key := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if len(key) < 8 || len(key) > 128 {
		writeError(writer, http.StatusBadRequest, "IDEMPOTENCY_KEY_INVALID", "Idempotency-Key must contain 8-128 characters", false, nil)
		return "", false
	}
	return key, true
}

func (server *Server) readValidated(writer http.ResponseWriter, request *http.Request, schema string, out any) bool {
	body, err := readRequestBody(writer, request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "REQUEST_BODY_INVALID", err.Error(), false, nil)
		return false
	}
	if err := server.validator.ValidateJSON(schema, body); err != nil {
		writeError(writer, http.StatusBadRequest, "SCHEMA_INVALID", err.Error(), false, nil)
		return false
	}
	if err := json.Unmarshal(body, out); err != nil {
		writeError(writer, http.StatusBadRequest, "SCHEMA_INVALID", "request does not match the schema", false, nil)
		return false
	}
	return true
}

func (server *Server) createAgent(writer http.ResponseWriter, request *http.Request) {
	key, ok := idempotencyKeyOf(writer, request)
	if !ok {
		return
	}
	var input agentregistry.AgentInput
	if !server.readValidated(writer, request, agentRequestSchema, &input) {
		return
	}
	agent, err := server.agentService.CreateAgent(request.Context(), key, input)
	if err != nil {
		writeAgentError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, agent)
}

func (server *Server) getAgent(writer http.ResponseWriter, request *http.Request) {
	agent, err := server.agentService.GetAgent(request.Context(), request.PathValue("agentId"))
	if err != nil {
		writeAgentError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, agent)
}

func (server *Server) listAgents(writer http.ResponseWriter, request *http.Request) {
	cursor, limit, ok := pageParams(writer, request)
	if !ok {
		return
	}
	items, next, err := server.agentService.ListAgents(request.Context(), cursor, limit)
	if err != nil {
		writeAgentError(writer, err)
		return
	}
	if items == nil {
		items = []agentregistry.Agent{}
	}
	writePage(writer, items, next)
}

func (server *Server) createAgentVersion(writer http.ResponseWriter, request *http.Request) {
	key, ok := idempotencyKeyOf(writer, request)
	if !ok {
		return
	}
	var input agentregistry.VersionInput
	if !server.readValidated(writer, request, agentVersionRequestSchema, &input) {
		return
	}
	version, err := server.agentService.CreateVersion(request.Context(), request.PathValue("agentId"), key, input)
	if err != nil {
		writeAgentError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, version)
}

func (server *Server) listAgentVersions(writer http.ResponseWriter, request *http.Request) {
	cursor, limit, ok := pageParams(writer, request)
	if !ok {
		return
	}
	items, next, err := server.agentService.ListVersions(request.Context(), request.PathValue("agentId"), cursor, limit)
	if err != nil {
		writeAgentError(writer, err)
		return
	}
	if items == nil {
		items = []agentregistry.Version{}
	}
	writePage(writer, items, next)
}

// pageParams applies the API list rules: default 50, maximum 200, cursor of at most 512 characters.
func pageParams(writer http.ResponseWriter, request *http.Request) (string, int, bool) {
	query := request.URL.Query()
	limit := 50
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > agentregistry.MaxPageSize {
			writeError(writer, http.StatusBadRequest, "SCHEMA_INVALID", "limit must be an integer from 1 to 200", false, nil)
			return "", 0, false
		}
		limit = parsed
	}
	cursor := query.Get("cursor")
	if len(cursor) > 512 {
		writeError(writer, http.StatusBadRequest, "CURSOR_INVALID", "cursor must not exceed 512 characters", false, nil)
		return "", 0, false
	}
	return cursor, limit, true
}

func writePage(writer http.ResponseWriter, items any, next string) {
	var nextCursor any
	if next != "" {
		nextCursor = next
	}
	writeJSON(writer, http.StatusOK, map[string]any{"items": items, "next_cursor": nextCursor})
}

func writeAgentError(writer http.ResponseWriter, err error) {
	switch {
	case agentregistry.IsInvalidInput(err), errors.Is(err, agentregistry.ErrInvalidLimit):
		writeError(writer, http.StatusBadRequest, "SCHEMA_INVALID", err.Error(), false, nil)
	case errors.Is(err, agentregistry.ErrInvalidCursor):
		writeError(writer, http.StatusBadRequest, "CURSOR_INVALID", "cursor was not issued by this API", false, nil)
	case errors.Is(err, agentregistry.ErrDatabaseCheckFailed):
		writeError(writer, http.StatusBadRequest, "AGENT_DATABASE_CHECK_FAILED", err.Error(), false, nil)
	case errors.Is(err, agentregistry.ErrSecretNotFound):
		writeError(writer, http.StatusBadRequest, "SECRET_NOT_FOUND", err.Error(), false, nil)
	case errors.Is(err, agentregistry.ErrNotFound):
		writeError(writer, http.StatusNotFound, "NOT_FOUND", "agent is not found", false, nil)
	case errors.Is(err, agentregistry.ErrConflict), errors.Is(err, agentregistry.ErrIdempotencyConflict):
		writeError(writer, http.StatusConflict, "CONFLICT", err.Error(), false, nil)
	default:
		writeError(writer, http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error", true, nil)
	}
}
