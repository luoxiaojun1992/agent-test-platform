package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"agent-test-platform/internal/modelcfg"
)

// ModelService lists registered models and selects the active one per role.
type ModelService interface {
	List(context.Context) ([]modelcfg.Model, error)
	Activate(ctx context.Context, role, alias string) error
}

// SetModelService enables the model-selection routes.
func (server *Server) SetModelService(service ModelService) {
	server.modelService = service
	server.handler.(*http.ServeMux).Handle("GET /api/v1/models", server.authenticate(http.HandlerFunc(server.listModels)))
	server.handler.(*http.ServeMux).Handle("PUT /api/v1/models/{role}/active", server.authenticate(http.HandlerFunc(server.activateModel)))
}

func (server *Server) listModels(writer http.ResponseWriter, request *http.Request) {
	items, err := server.modelService.List(request.Context())
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error", true, nil)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"items": items})
}

func (server *Server) activateModel(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Alias string `json:"alias"`
	}
	body, err := readRequestBody(writer, request)
	if err != nil || json.Unmarshal(body, &input) != nil {
		writeError(writer, http.StatusBadRequest, "REQUEST_BODY_INVALID", "body must be {\"alias\": \"...\"}", false, nil)
		return
	}
	switch err := server.modelService.Activate(request.Context(), request.PathValue("role"), input.Alias); {
	case err == nil:
		writeJSON(writer, http.StatusOK, map[string]string{"role": request.PathValue("role"), "alias": input.Alias})
	case errors.Is(err, modelcfg.ErrInvalidInput):
		writeError(writer, http.StatusBadRequest, "SCHEMA_INVALID", err.Error(), false, nil)
	case errors.Is(err, modelcfg.ErrNotFound):
		writeError(writer, http.StatusNotFound, "NOT_FOUND", "model alias is not registered", false, nil)
	default:
		writeError(writer, http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error", true, nil)
	}
}
