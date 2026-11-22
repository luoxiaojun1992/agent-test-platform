package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agent-test-platform/internal/modelcfg"
)

type fakeModelService struct{ role, alias string }

func (*fakeModelService) List(context.Context) ([]modelcfg.Model, error) {
	return []modelcfg.Model{{Alias: "j", Role: "judge", Active: true}}, nil
}
func (f *fakeModelService) Activate(_ context.Context, role, alias string) error {
	if role != "judge" && role != "generation" {
		return modelcfg.ErrInvalidInput
	}
	if alias == "missing" {
		return modelcfg.ErrNotFound
	}
	f.role, f.alias = role, alias
	return nil
}

func TestModelRoutes(t *testing.T) {
	server, err := NewServer(&fakeGenerationService{}, &fakeSecretService{}, &fakeTargetService{}, &fakeAgentService{}, &fakeCatalogService{}, &fakeRunService{}, newTestValidator(t), "api-secret")
	if err != nil {
		t.Fatal(err)
	}
	models := &fakeModelService{}
	server.SetModelService(models)
	do := func(method, path, body string) int {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer api-secret")
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		return recorder.Code
	}
	if got := do(http.MethodGet, "/api/v1/models", ""); got != 200 {
		t.Fatal(got)
	}
	if got := do(http.MethodPut, "/api/v1/models/judge/active", `{"alias":"j2"}`); got != 200 || models.alias != "j2" {
		t.Fatal(got, models)
	}
	if got := do(http.MethodPut, "/api/v1/models/bad/active", `{"alias":"x"}`); got != 400 {
		t.Fatal(got)
	}
	if got := do(http.MethodPut, "/api/v1/models/judge/active", `{"alias":"missing"}`); got != 404 {
		t.Fatal(got)
	}
}
