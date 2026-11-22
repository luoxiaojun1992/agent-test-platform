package analytics

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeStore struct {
	runs    map[string]DailyRuns
	assets  map[string]DailyAssets
	buckets map[int]int
}

func (fakeStore) AssetCounts(context.Context) ([]AssetCount, error)  { return nil, nil }
func (fakeStore) AgentCounts(context.Context) (AgentCount, error)    { return AgentCount{}, nil }
func (fakeStore) RunStatuses(context.Context) ([]StatusCount, error) { return nil, nil }
func (fakeStore) JobCounts(context.Context, time.Time) ([]JobCount, error) {
	return nil, nil
}
func (s fakeStore) DailyRuns(context.Context, time.Time) (map[string]DailyRuns, error) {
	return s.runs, nil
}
func (s fakeStore) DailyAssets(context.Context, time.Time) (map[string]DailyAssets, error) {
	return s.assets, nil
}
func (s fakeStore) ScoreBuckets(context.Context, time.Time) (map[int]int, error) {
	return s.buckets, nil
}
func (fakeStore) ByAgent(context.Context, time.Time) ([]AgentStat, error) {
	return nil, nil
}

func newService(t *testing.T, store Store) *Service {
	t.Helper()
	service, err := NewService(store)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return time.Date(2026, 11, 20, 15, 30, 0, 0, time.UTC) }
	return service
}

func TestOverviewFillsEveryDayOfTheWindowOldestFirst(t *testing.T) {
	rate := 50.0
	service := newService(t, fakeStore{
		runs:    map[string]DailyRuns{"2026-11-19": {Date: "2026-11-19", Total: 4, Evaluated: 2, Passed: 1, PassRate: &rate}},
		assets:  map[string]DailyAssets{"2026-11-20": {Date: "2026-11-20", TestCases: 3}},
		buckets: map[int]int{4: 7},
	})
	overview, err := service.Overview(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(overview.DailyRuns) != 3 || overview.DailyRuns[0].Date != "2026-11-18" || overview.DailyRuns[2].Date != "2026-11-20" {
		t.Fatalf("window = %+v", overview.DailyRuns)
	}
	if overview.DailyRuns[0].Total != 0 || overview.DailyRuns[0].PassRate != nil {
		t.Fatalf("an empty day must be zero with no rate: %+v", overview.DailyRuns[0])
	}
	if overview.DailyRuns[1].Total != 4 || overview.DailyRuns[1].PassRate == nil || *overview.DailyRuns[1].PassRate != 50 {
		t.Fatalf("day with runs lost its data: %+v", overview.DailyRuns[1])
	}
	if overview.DailyAssets[2].TestCases != 3 || overview.DailyAssets[0].TestCases != 0 {
		t.Fatalf("assets = %+v", overview.DailyAssets)
	}
	if len(overview.ScoreBuckets) != 5 || overview.ScoreBuckets[4].Count != 7 || overview.ScoreBuckets[4].From != 80 || overview.ScoreBuckets[0].Count != 0 {
		t.Fatalf("buckets = %+v", overview.ScoreBuckets)
	}
}

func TestOverviewValidatesTheWindow(t *testing.T) {
	service := newService(t, fakeStore{})
	for _, days := range []int{-1, 91} {
		if _, err := service.Overview(context.Background(), days); !errors.Is(err, ErrInvalidDays) {
			t.Fatalf("days=%d: %v", days, err)
		}
	}
	overview, err := service.Overview(context.Background(), 0)
	if err != nil || overview.Days != DefaultDays || len(overview.DailyRuns) != DefaultDays {
		t.Fatalf("default window: %v %d", err, overview.Days)
	}
}
