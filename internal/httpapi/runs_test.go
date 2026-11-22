package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agent-test-platform/internal/run"
)

type fakeRunService struct{}

func (*fakeRunService) Create(_ context.Context, key string, _ run.Request) ([]string, error) {
	switch key {
	case "missing-key-1234":
		return nil, run.ErrNotFound
	case "unbound-key-1234":
		return nil, run.ErrAgentDatabaseMissing
	}
	return []string{"01ARZ3NDEKTSV4RRFFQ69G5FAV"}, nil
}
func (*fakeRunService) Get(context.Context, string) (run.Run, error) {
	return run.Run{}, run.ErrNotFound
}
func (*fakeRunService) List(context.Context, run.Filter, string, int) ([]run.Run, string, error) {
	return nil, "", nil
}
func (*fakeRunService) ToolCalls(context.Context, string) ([]run.ToolCall, error) { return nil, nil }
func (*fakeRunService) Cancel(context.Context, string) (run.Run, error) {
	return run.Run{}, run.ErrConflict
}
func (*fakeRunService) Evaluations(context.Context, string) ([]run.Evaluation, error) {
	return nil, nil
}
func (*fakeRunService) AllEvaluations(context.Context, run.EvalFilter, string, int) ([]run.Evaluation, string, error) {
	return nil, "", nil
}
func (*fakeRunService) Benchmark(context.Context, []string) ([]run.BenchmarkEntry, error) {
	return nil, nil
}

func TestRunRoutes(t *testing.T) {
	server := agentTestServer(t, &fakeAgentService{})
	agent := `"agent_version_id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","test_case_revision_ids":["01ARZ3NDEKTSV4RRFFQ69G5FAV"]`
	idem := map[string]string{"Idempotency-Key": "key-12345678"}
	cases := []struct {
		method, path, body string
		idem               bool
		want               int
	}{
		{http.MethodPost, "/api/v1/test-runs", `{` + agent + `}`, true, http.StatusAccepted},
		{http.MethodPost, "/api/v1/test-runs", `{` + agent + `}`, false, http.StatusBadRequest},
		{http.MethodPost, "/api/v1/test-runs", `{"agent_version_id":"01ARZ3NDEKTSV4RRFFQ69G5FAV"}`, true, http.StatusBadRequest},
		{http.MethodPost, "/api/v1/test-runs", `{` + agent + `,"repetitions":11}`, true, http.StatusBadRequest},
		{http.MethodGet, "/api/v1/benchmark?test_case_revision_id=01ARZ3NDEKTSV4RRFFQ69G5FAV", "", false, http.StatusOK},
		{http.MethodGet, "/api/v1/test-runs", "", false, http.StatusOK},
		{http.MethodGet, "/api/v1/test-runs/x", "", false, http.StatusNotFound},
		{http.MethodGet, "/api/v1/test-runs/x/tool-calls", "", false, http.StatusOK},
		{http.MethodPost, "/api/v1/test-runs/x/cancel", "", true, http.StatusConflict},
		{http.MethodGet, "/api/v1/test-runs/x/evaluations", "", false, http.StatusOK},
		{http.MethodGet, "/api/v1/evaluations?pass=true", "", false, http.StatusOK},
		{http.MethodGet, "/api/v1/evaluations?pass=maybe", "", false, http.StatusBadRequest},
	}
	for _, c := range cases {
		var headers map[string]string
		if c.idem {
			headers = idem
		}
		if r := doSuiteRequest(server, c.method, c.path, c.body, headers); r.Code != c.want {
			t.Errorf("%s %s %s = %d %s, want %d", c.method, c.path, c.body, r.Code, r.Body, c.want)
		}
	}
}

func (*fakeRunService) Reevaluate(_ context.Context, _, _, _ string) (string, error) {
	return "01HZZZZZZZZZZZZZZZZZZZZZZZ", nil
}

func doSuiteRequest(server *Server, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	authorize(request)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	return recorder
}

func TestRunCreateErrors(t *testing.T) {
	server := agentTestServer(t, &fakeAgentService{})
	body := `{"agent_version_id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","test_case_revision_ids":["01ARZ3NDEKTSV4RRFFQ69G5FAV"]}`
	for key, want := range map[string]int{"missing-key-1234": http.StatusNotFound, "unbound-key-1234": http.StatusConflict} {
		if r := doSuiteRequest(server, http.MethodPost, "/api/v1/test-runs", body, map[string]string{"Idempotency-Key": key}); r.Code != want {
			t.Errorf("%s = %d %s, want %d", key, r.Code, r.Body, want)
		}
	}
}
