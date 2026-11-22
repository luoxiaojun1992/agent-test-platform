package httpapi

import (
	"context"
	"errors"
	"net/http"

	"agent-test-platform/internal/run"
)

const (
	createRunsSchema = "schemas/create-runs-request.schema.json"
	reevaluateSchema = "schemas/reevaluate-request.schema.json"
)

// RunService creates and reads Test Runs.
type RunService interface {
	Create(ctx context.Context, key string, request run.Request) ([]string, error)
	Get(context.Context, string) (run.Run, error)
	List(ctx context.Context, filter run.Filter, cursor string, limit int) ([]run.Run, string, error)
	ToolCalls(context.Context, string) ([]run.ToolCall, error)
	Cancel(context.Context, string) (run.Run, error)
	Reevaluate(ctx context.Context, sourceID, key, judgeAlias string) (string, error)
	Evaluations(context.Context, string) ([]run.Evaluation, error)
	AllEvaluations(ctx context.Context, filter run.EvalFilter, cursor string, limit int) ([]run.Evaluation, string, error)
	Benchmark(context.Context, []string) ([]run.BenchmarkEntry, error)
}

func (server *Server) registerRunRoutes(mux *http.ServeMux) {
	handle := func(pattern string, handler http.HandlerFunc) {
		mux.Handle(pattern, server.authenticate(handler))
	}
	handle("POST /api/v1/test-runs", server.createRuns)
	handle("GET /api/v1/benchmark", server.getBenchmark)
	handle("GET /api/v1/test-runs", server.listRuns)
	handle("GET /api/v1/test-runs/{runId}", server.getRun)
	handle("GET /api/v1/test-runs/{runId}/tool-calls", server.listRunToolCalls)
	handle("POST /api/v1/test-runs/{runId}/cancel", server.cancelRun)
	handle("POST /api/v1/test-runs/{runId}/evaluations", server.reevaluateRun)
	handle("GET /api/v1/test-runs/{runId}/evaluations", server.listRunEvaluations)
	handle("GET /api/v1/evaluations", server.listEvaluations)
}

func (server *Server) createRuns(writer http.ResponseWriter, request *http.Request) {
	key, ok := idempotencyKeyOf(writer, request)
	if !ok {
		return
	}
	var input run.Request
	if !server.readValidated(writer, request, createRunsSchema, &input) {
		return
	}
	ids, err := server.runService.Create(request.Context(), key, input)
	if err != nil {
		writeRunError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, map[string]any{"run_ids": ids})
}

func (server *Server) getBenchmark(writer http.ResponseWriter, request *http.Request) {
	items, err := server.runService.Benchmark(request.Context(), request.URL.Query()["test_case_revision_id"])
	if err != nil {
		writeRunError(writer, err)
		return
	}
	if items == nil {
		items = []run.BenchmarkEntry{}
	}
	writeJSON(writer, http.StatusOK, map[string]any{"items": items})
}

func (server *Server) listRuns(writer http.ResponseWriter, request *http.Request) {
	cursor, limit, ok := pageParams(writer, request)
	if !ok {
		return
	}
	query := request.URL.Query()
	filter := run.Filter{AgentID: query.Get("agent_id"), TestCaseRevisionID: query.Get("test_case_revision_id"), Status: query.Get("status")}
	items, next, err := server.runService.List(request.Context(), filter, cursor, limit)
	if err != nil {
		writeRunError(writer, err)
		return
	}
	if items == nil {
		items = []run.Run{}
	}
	writePage(writer, items, next)
}

func (server *Server) getRun(writer http.ResponseWriter, request *http.Request) {
	found, err := server.runService.Get(request.Context(), request.PathValue("runId"))
	if err != nil {
		writeRunError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, found)
}

func (server *Server) listRunToolCalls(writer http.ResponseWriter, request *http.Request) {
	items, err := server.runService.ToolCalls(request.Context(), request.PathValue("runId"))
	if err != nil {
		writeRunError(writer, err)
		return
	}
	if items == nil {
		items = []run.ToolCall{}
	}
	writeJSON(writer, http.StatusOK, map[string]any{"items": items})
}

func (server *Server) cancelRun(writer http.ResponseWriter, request *http.Request) {
	if _, ok := idempotencyKeyOf(writer, request); !ok {
		return
	}
	cancelled, err := server.runService.Cancel(request.Context(), request.PathValue("runId"))
	if err != nil {
		writeRunError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, cancelled)
}

func (server *Server) listRunEvaluations(writer http.ResponseWriter, request *http.Request) {
	items, err := server.runService.Evaluations(request.Context(), request.PathValue("runId"))
	if err != nil {
		writeRunError(writer, err)
		return
	}
	if items == nil {
		items = []run.Evaluation{}
	}
	writeJSON(writer, http.StatusOK, map[string]any{"items": items})
}

func (server *Server) listEvaluations(writer http.ResponseWriter, request *http.Request) {
	cursor, limit, ok := pageParams(writer, request)
	if !ok {
		return
	}
	query := request.URL.Query()
	filter := run.EvalFilter{Status: query.Get("status")}
	switch query.Get("pass") {
	case "":
	case "true", "false":
		pass := query.Get("pass") == "true"
		filter.Pass = &pass
	default:
		writeError(writer, http.StatusBadRequest, "SCHEMA_INVALID", "pass must be true or false", false, nil)
		return
	}
	items, next, err := server.runService.AllEvaluations(request.Context(), filter, cursor, limit)
	if err != nil {
		writeRunError(writer, err)
		return
	}
	if items == nil {
		items = []run.Evaluation{}
	}
	writePage(writer, items, next)
}

func writeRunError(writer http.ResponseWriter, err error) {
	switch {
	case run.IsInvalidInput(err), errors.Is(err, run.ErrInvalidLimit):
		writeError(writer, http.StatusBadRequest, "SCHEMA_INVALID", err.Error(), false, nil)
	case errors.Is(err, run.ErrInvalidCursor):
		writeError(writer, http.StatusBadRequest, "CURSOR_INVALID", "cursor was not issued by this API", false, nil)
	case errors.Is(err, run.ErrNotFound):
		writeError(writer, http.StatusNotFound, "NOT_FOUND", "resource is not found", false, nil)
	case errors.Is(err, run.ErrAgentDatabaseMissing):
		writeError(writer, http.StatusConflict, "AGENT_DATABASE_NOT_CONFIGURED", err.Error(), false, nil)
	case errors.Is(err, run.ErrConflict), errors.Is(err, run.ErrIdempotencyConflict):
		writeError(writer, http.StatusConflict, "CONFLICT", err.Error(), false, nil)
	default:
		writeError(writer, http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error", true, nil)
	}
}

func (server *Server) reevaluateRun(writer http.ResponseWriter, request *http.Request) {
	key, ok := idempotencyKeyOf(writer, request)
	if !ok {
		return
	}
	var input struct {
		JudgeModelAlias string `json:"judge_model_alias"`
	}
	if !server.readValidated(writer, request, reevaluateSchema, &input) {
		return
	}
	id, err := server.runService.Reevaluate(request.Context(), request.PathValue("runId"), key, input.JudgeModelAlias)
	if err != nil {
		writeRunError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, map[string]any{"run_ids": []string{id}})
}
