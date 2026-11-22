package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agent-test-platform/internal/agentregistry"
)

type fakeDiscovery struct{ err error }

func (f fakeDiscovery) DiscoverTools(context.Context, agentregistry.DiscoveryInput) ([]agentregistry.ToolDefinition, error) {
	return []agentregistry.ToolDefinition{{Name: "t"}}, f.err
}

func TestToolDiscoveryRoute(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want int
	}{"ok": {nil, 200}, "failed": {agentregistry.ErrDiscoveryFailed, 502}, "secret": {agentregistry.ErrSecretNotFound, 400}} {
		server := agentTestServer(t, &fakeAgentService{})
		server.SetToolDiscovery(fakeDiscovery{tc.err})
		request := httptest.NewRequest(http.MethodPost, "/api/v1/agent-tool-discovery", strings.NewReader(`{"chat_endpoint":"https://a/chat","target_bearer_token_ref":"t"}`))
		request.Header.Set("Authorization", "Bearer api-secret")
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		if recorder.Code != tc.want {
			t.Fatalf("%s: %d %s", name, recorder.Code, recorder.Body)
		}
	}
}
