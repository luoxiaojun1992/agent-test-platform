package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"agent-test-platform/internal/catalog"
)

type fakeCatalogService struct{}

func (*fakeCatalogService) List(context.Context, catalog.Kind, string, int) ([]map[string]any, string, error) {
	return []map[string]any{{"id": "x"}}, "next", nil
}
func (*fakeCatalogService) ListRevisions(context.Context, catalog.Kind, string, string, int) ([]map[string]any, string, error) {
	return nil, "", catalog.ErrNotFound
}
func (*fakeCatalogService) SearchRevisions(context.Context, catalog.Kind, string, string, int) ([]map[string]any, string, error) {
	return []map[string]any{{"id": "r"}}, "", nil
}
func (*fakeCatalogService) GetRevision(context.Context, catalog.Kind, string) (map[string]any, error) {
	return map[string]any{"id": "r"}, nil
}

func TestCatalogRoutes(t *testing.T) {
	server := agentTestServer(t, &fakeAgentService{})
	for path, want := range map[string]int{
		"/api/v1/scenarios":                http.StatusOK,
		"/api/v1/datasets?limit=5":         http.StatusOK,
		"/api/v1/test-cases?limit=0":       http.StatusBadRequest,
		"/api/v1/test-cases/abc/revisions": http.StatusNotFound,
		"/api/v1/test-case-revisions/abc":  http.StatusOK,
		"/api/v1/dataset-revisions/abc":    http.StatusOK,
		"/api/v1/test-case-revisions?q=x":  http.StatusOK,
		"/api/v1/scenario-revisions":       http.StatusOK,
		"/api/v1/scenario-revisions/abc":   http.StatusOK,
	} {
		if r := doAgentRequest(server, http.MethodGet, path, ""); r.Code != want {
			t.Errorf("%s = %d %s, want %d", path, r.Code, r.Body, want)
		}
	}
	if r := doAgentRequest(server, http.MethodGet, "/api/v1/scenarios", ""); !strings.Contains(r.Body.String(), `"next_cursor":"next"`) {
		t.Fatal(r.Body)
	}
}
