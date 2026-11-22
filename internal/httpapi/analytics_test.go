package httpapi

import (
	"context"
	"net/http"
	"testing"

	"agent-test-platform/internal/analytics"
)

type fakeAnalytics struct{ days int }

func (f *fakeAnalytics) Overview(_ context.Context, days int) (analytics.Overview, error) {
	f.days = days
	if days > analytics.MaxDays {
		return analytics.Overview{}, analytics.ErrInvalidDays
	}
	return analytics.Overview{Days: days}, nil
}

func TestAnalyticsOverviewRoute(t *testing.T) {
	server := agentTestServer(t, &fakeAgentService{})
	fake := &fakeAnalytics{}
	server.SetAnalyticsService(fake)
	cases := []struct {
		path string
		want int
	}{
		{"/api/v1/analytics/overview", http.StatusOK},
		{"/api/v1/analytics/overview?days=30", http.StatusOK},
		{"/api/v1/analytics/overview?days=abc", http.StatusBadRequest},
		{"/api/v1/analytics/overview?days=500", http.StatusBadRequest},
	}
	for _, c := range cases {
		if r := doSuiteRequest(server, http.MethodGet, c.path, "", nil); r.Code != c.want {
			t.Errorf("%s = %d %s, want %d", c.path, r.Code, r.Body, c.want)
		}
	}
	if fake.days != 500 {
		t.Errorf("days were not passed through: %d", fake.days)
	}
}
