package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"agent-test-platform/internal/agentregistry"
)

// ToolDiscoveryService reads an Agent's Tool list through its GET /v1/tools interface.
type ToolDiscoveryService interface {
	DiscoverTools(context.Context, agentregistry.DiscoveryInput) ([]agentregistry.ToolDefinition, error)
}

// SetToolDiscovery enables POST /api/v1/agent-tool-discovery.
func (server *Server) SetToolDiscovery(service ToolDiscoveryService) {
	server.toolDiscovery = service
	server.handler.(*http.ServeMux).Handle("POST /api/v1/agent-tool-discovery", server.authenticate(http.HandlerFunc(server.discoverTools)))
}

func (server *Server) discoverTools(writer http.ResponseWriter, request *http.Request) {
	var input agentregistry.DiscoveryInput
	body, err := readRequestBody(writer, request)
	if err != nil || json.Unmarshal(body, &input) != nil {
		writeError(writer, http.StatusBadRequest, "REQUEST_BODY_INVALID", "body must contain chat_endpoint and target_bearer_token_ref", false, nil)
		return
	}
	tools, err := server.toolDiscovery.DiscoverTools(request.Context(), input)
	if errors.Is(err, agentregistry.ErrDiscoveryFailed) {
		writeError(writer, http.StatusBadGateway, "TARGET_TOOL_LIST_FAILED", err.Error(), true, nil)
		return
	}
	if err != nil {
		writeAgentError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"tools": tools})
}
