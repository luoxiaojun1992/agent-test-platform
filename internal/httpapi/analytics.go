package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"agent-test-platform/internal/analytics"
)

// AnalyticsService aggregates results and asset counts for the dashboard.
type AnalyticsService interface {
	Overview(ctx context.Context, days int) (analytics.Overview, error)
}

// SetAnalyticsService enables GET /api/v1/analytics/overview.
func (server *Server) SetAnalyticsService(service AnalyticsService) {
	server.analyticsService = service
	server.handler.(*http.ServeMux).Handle("GET /api/v1/analytics/overview", server.authenticate(http.HandlerFunc(server.analyticsOverview)))
}

func (server *Server) analyticsOverview(writer http.ResponseWriter, request *http.Request) {
	days := 0
	if text := request.URL.Query().Get("days"); text != "" {
		parsed, err := strconv.Atoi(text)
		if err != nil {
			writeError(writer, http.StatusBadRequest, "SCHEMA_INVALID", "days must be an integer from 1 to 90", false, nil)
			return
		}
		days = parsed
	}
	overview, err := server.analyticsService.Overview(request.Context(), days)
	switch {
	case err == nil:
		writeJSON(writer, http.StatusOK, overview)
	case errors.Is(err, analytics.ErrInvalidDays):
		writeError(writer, http.StatusBadRequest, "SCHEMA_INVALID", err.Error(), false, nil)
	default:
		writeError(writer, http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error", true, nil)
	}
}
