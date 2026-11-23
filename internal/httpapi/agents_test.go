package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agent-test-platform/internal/agentregistry"
)

type fakeAgentService struct{}

func (f *fakeAgentService) CreateAgent(_ context.Context, _ string, in agentregistry.AgentInput) (agentregistry.Agent, error) {
	return agentregistry.Agent{
		ID: "01JZ0000000000000000000000", Name: in.Name, Description: in.Description, DatabaseProfileID: in.DatabaseProfileID,
		ChatEndpoint: in.ChatEndpoint, TargetModel: in.TargetModel, ContractVersion: in.ContractVersion,
		ToolEvaluationMode: in.ToolEvaluationMode, ToolCatalog: in.ToolCatalog, CreatedAt: time.Unix(0, 0).UTC(),
	}, nil
}
func (f *fakeAgentService) GetAgent(context.Context, string) (agentregistry.Agent, error) {
	return agentregistry.Agent{}, agentregistry.ErrNotFound
}
func (f *fakeAgentService) ListAgents(context.Context, string, int) ([]agentregistry.Agent, string, error) {
	return nil, "", nil
}

func agentTestServer(t *testing.T, agents AgentService) *Server {
	t.Helper()
	server, err := NewServer(&fakeGenerationService{}, &fakeSecretService{}, &fakeTargetService{}, agents, &fakeCatalogService{}, &fakeRunService{}, newTestValidator(t), "api-secret")
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func doAgentRequest(server *Server, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Idempotency-Key", "idem-key-12345")
	request.Header.Set("Content-Type", "application/json")
	authorize(request)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	return recorder
}

func TestCreateAgentAndList(t *testing.T) {
	server := agentTestServer(t, &fakeAgentService{})
	base := `{"chat_endpoint":"https://agent.example/chat","target_bearer_token_ref":"tok","target_model":"m","contract_version":"1.0","tool_evaluation_mode":"SESSION_LLM_ONLY",`
	if r := doAgentRequest(server, http.MethodPost, "/api/v1/agents", base+`"name":"a"}`); r.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", r.Code, r.Body)
	}
	if r := doAgentRequest(server, http.MethodPost, "/api/v1/agents", base+`"name":""}`); r.Code != http.StatusBadRequest {
		t.Fatalf("empty name = %d", r.Code)
	}
	r := doAgentRequest(server, http.MethodGet, "/api/v1/agents", "")
	if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `"items":[]`) || !strings.Contains(r.Body.String(), `"next_cursor":null`) {
		t.Fatalf("list = %d %s", r.Code, r.Body)
	}
	if r := doAgentRequest(server, http.MethodGet, "/api/v1/agents/x", ""); r.Code != http.StatusNotFound {
		t.Fatalf("get = %d", r.Code)
	}
}

func TestCreateAgentSchemaRules(t *testing.T) {
	server := agentTestServer(t, &fakeAgentService{})
	base := `{"name":"a","chat_endpoint":"https://agent.example/chat","target_bearer_token_ref":"tok","target_model":"m","contract_version":"1.0",`
	path := "/api/v1/agents"
	tool := `[{"name":"search","description":"d","input_schema":{}}]`
	cases := []struct {
		body string
		code int
	}{
		{base + `"tool_evaluation_mode":"DETERMINISTIC_CHAIN","tool_catalog":` + tool + `}`, http.StatusCreated},
		{base + `"tool_evaluation_mode":"DETERMINISTIC_CHAIN"}`, http.StatusBadRequest},
		{base + `"tool_evaluation_mode":"SESSION_LLM_ONLY"}`, http.StatusCreated},
		{base + `"tool_evaluation_mode":"SESSION_LLM_ONLY","tool_catalog":` + tool + `}`, http.StatusBadRequest},
		{strings.Replace(base, "https://agent.example/chat", "ftp://x", 1) + `"tool_evaluation_mode":"SESSION_LLM_ONLY"}`, http.StatusBadRequest},
	}
	for i, c := range cases {
		if r := doAgentRequest(server, http.MethodPost, path, c.body); r.Code != c.code {
			t.Errorf("case %d = %d %s, want %d", i, r.Code, r.Body, c.code)
		}
	}
}

func TestAgentDatabaseCheckFailureIsABadRequest(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeAgentError(recorder, fmt.Errorf("%w: the account cannot create databases", agentregistry.ErrDatabaseCheckFailed))
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "AGENT_DATABASE_CHECK_FAILED") {
		t.Fatalf("%d %s", recorder.Code, recorder.Body.String())
	}
}
