package httpapi

import (
	"context"
	"errors"
	"net/http"

	"agent-test-platform/internal/catalog"
)

// CatalogService serves published Scenarios, Datasets, Test Cases and their revisions.
type CatalogService interface {
	List(context.Context, catalog.Kind, string, int) ([]map[string]any, string, error)
	ListRevisions(context.Context, catalog.Kind, string, string, int) ([]map[string]any, string, error)
	GetRevision(context.Context, catalog.Kind, string) (map[string]any, error)
	SearchRevisions(ctx context.Context, kind catalog.Kind, query, cursor string, limit int) ([]map[string]any, string, error)
}

func (server *Server) searchCatalogRevisions(kind catalog.Kind) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		cursor, limit, ok := pageParams(writer, request)
		if !ok {
			return
		}
		items, next, err := server.catalogService.SearchRevisions(request.Context(), kind, request.URL.Query().Get("q"), cursor, limit)
		if err != nil {
			writeCatalogError(writer, err)
			return
		}
		writePage(writer, items, next)
	}
}

func (server *Server) listCatalog(kind catalog.Kind) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		cursor, limit, ok := pageParams(writer, request)
		if !ok {
			return
		}
		items, next, err := server.catalogService.List(request.Context(), kind, cursor, limit)
		if err != nil {
			writeCatalogError(writer, err)
			return
		}
		writePage(writer, items, next)
	}
}

func (server *Server) listCatalogRevisions(kind catalog.Kind, param string) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		cursor, limit, ok := pageParams(writer, request)
		if !ok {
			return
		}
		items, next, err := server.catalogService.ListRevisions(request.Context(), kind, request.PathValue(param), cursor, limit)
		if err != nil {
			writeCatalogError(writer, err)
			return
		}
		writePage(writer, items, next)
	}
}

func (server *Server) getCatalogRevision(kind catalog.Kind) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		body, err := server.catalogService.GetRevision(request.Context(), kind, request.PathValue("revisionId"))
		if err != nil {
			writeCatalogError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, body)
	}
}

func (server *Server) registerCatalogRoutes(mux *http.ServeMux) {
	for _, spec := range []struct {
		kind   catalog.Kind
		plural string
		param  string
		rev    string
	}{
		{catalog.Scenario, "scenarios", "scenarioId", "scenario-revisions"},
		{catalog.Dataset, "datasets", "datasetId", "dataset-revisions"},
		{catalog.TestCase, "test-cases", "testCaseId", "test-case-revisions"},
	} {
		base := "/api/v1/" + spec.plural
		mux.Handle("GET "+base, server.authenticate(server.listCatalog(spec.kind)))
		mux.Handle("GET "+base+"/{"+spec.param+"}/revisions", server.authenticate(server.listCatalogRevisions(spec.kind, spec.param)))
		mux.Handle("GET /api/v1/"+spec.rev, server.authenticate(server.searchCatalogRevisions(spec.kind)))
		mux.Handle("GET /api/v1/"+spec.rev+"/{revisionId}", server.authenticate(server.getCatalogRevision(spec.kind)))
	}
}

func writeCatalogError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, catalog.ErrInvalidLimit):
		writeError(writer, http.StatusBadRequest, "SCHEMA_INVALID", err.Error(), false, nil)
	case errors.Is(err, catalog.ErrInvalidCursor):
		writeError(writer, http.StatusBadRequest, "CURSOR_INVALID", "cursor was not issued by this API", false, nil)
	case errors.Is(err, catalog.ErrNotFound):
		writeError(writer, http.StatusNotFound, "NOT_FOUND", "resource is not found", false, nil)
	default:
		writeError(writer, http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error", true, nil)
	}
}
