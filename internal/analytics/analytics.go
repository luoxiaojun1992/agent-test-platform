// Package analytics aggregates Test Run results and asset counts for the dashboard.
package analytics

import (
	"context"
	"errors"
	"time"
)

const (
	DefaultDays = 14
	MaxDays     = 90
)

var ErrInvalidDays = errors.New("days must be from 1 to 90")

// Overview is everything the dashboard shows. Dates are UTC calendar days and every day of the window is present.
type Overview struct {
	Days         int           `json:"days"`
	Assets       []AssetCount  `json:"assets"`
	Agents       AgentCount    `json:"agents"`
	RunStatuses  []StatusCount `json:"run_statuses"`
	Jobs         []JobCount    `json:"generation_jobs"`
	DailyRuns    []DailyRuns   `json:"daily_runs"`
	DailyAssets  []DailyAssets `json:"daily_assets"`
	ScoreBuckets []ScoreBucket `json:"score_distribution"`
	ByAgent      []AgentStat   `json:"by_agent"`
}

// AssetCount counts one kind of asset: logical resources, revisions, and the revisions that can still be used.
type AssetCount struct {
	Kind      string `json:"kind"`
	Resources int    `json:"resources"`
	Revisions int    `json:"revisions"`
	Usable    int    `json:"usable_revisions"`
}

type AgentCount struct {
	Agents int `json:"agents"`
}

type StatusCount struct {
	Status string `json:"status"`
	Count  int    `json:"count"`
}

type JobCount struct {
	Kind   string `json:"kind"`
	Status string `json:"status"`
	Count  int    `json:"count"`
}

// DailyRuns is one day of Test Runs. Evaluated counts Runs with a completed Evaluation Attempt; PassRate and AvgScore are
// derived from those only and are null when the day has none.
type DailyRuns struct {
	Date      string   `json:"date"`
	Total     int      `json:"total"`
	Evaluated int      `json:"evaluated"`
	Passed    int      `json:"passed"`
	PassRate  *float64 `json:"pass_rate"`
	AvgScore  *float64 `json:"avg_score"`
}

type DailyAssets struct {
	Date      string `json:"date"`
	Scenarios int    `json:"scenarios"`
	Datasets  int    `json:"datasets"`
	TestCases int    `json:"test_cases"`
}

// ScoreBucket is a 20-point range of final scores.
type ScoreBucket struct {
	From  int `json:"from"`
	To    int `json:"to"`
	Count int `json:"count"`
}

type AgentStat struct {
	AgentName string   `json:"agent_name"`
	AgentID   string   `json:"agent_id"`
	Runs      int      `json:"runs"`
	Evaluated int      `json:"evaluated"`
	Passed    int      `json:"passed"`
	PassRate  *float64 `json:"pass_rate"`
	AvgScore  *float64 `json:"avg_score"`
}

// Store reads the aggregates. The MySQL implementation lives in the storage layer.
type Store interface {
	AssetCounts(ctx context.Context) ([]AssetCount, error)
	AgentCounts(ctx context.Context) (AgentCount, error)
	RunStatuses(ctx context.Context) ([]StatusCount, error)
	JobCounts(ctx context.Context, since time.Time) ([]JobCount, error)
	DailyRuns(ctx context.Context, since time.Time) (map[string]DailyRuns, error)
	DailyAssets(ctx context.Context, since time.Time) (map[string]DailyAssets, error)
	ScoreBuckets(ctx context.Context, since time.Time) (map[int]int, error)
	ByAgent(ctx context.Context, since time.Time) ([]AgentStat, error)
}

type Service struct {
	store Store
	now   func() time.Time
}

func NewService(store Store) (*Service, error) {
	if store == nil {
		return nil, errors.New("analytics store is required")
	}
	return &Service{store: store, now: time.Now}, nil
}

// Overview aggregates the last days UTC calendar days, today included.
func (service *Service) Overview(ctx context.Context, days int) (Overview, error) {
	if days == 0 {
		days = DefaultDays
	}
	if days < 1 || days > MaxDays {
		return Overview{}, ErrInvalidDays
	}
	today := service.now().UTC().Truncate(24 * time.Hour)
	since := today.AddDate(0, 0, -(days - 1))
	out := Overview{Days: days}
	var err error
	if out.Assets, err = service.store.AssetCounts(ctx); err != nil {
		return Overview{}, err
	}
	if out.Agents, err = service.store.AgentCounts(ctx); err != nil {
		return Overview{}, err
	}
	if out.RunStatuses, err = service.store.RunStatuses(ctx); err != nil {
		return Overview{}, err
	}
	if out.Jobs, err = service.store.JobCounts(ctx, since); err != nil {
		return Overview{}, err
	}
	runs, err := service.store.DailyRuns(ctx, since)
	if err != nil {
		return Overview{}, err
	}
	assets, err := service.store.DailyAssets(ctx, since)
	if err != nil {
		return Overview{}, err
	}
	for day := 0; day < days; day++ {
		date := since.AddDate(0, 0, day).Format("2006-01-02")
		entry, ok := runs[date]
		if !ok {
			entry = DailyRuns{Date: date}
		}
		out.DailyRuns = append(out.DailyRuns, entry)
		created, ok := assets[date]
		if !ok {
			created = DailyAssets{Date: date}
		}
		out.DailyAssets = append(out.DailyAssets, created)
	}
	buckets, err := service.store.ScoreBuckets(ctx, since)
	if err != nil {
		return Overview{}, err
	}
	for index := 0; index < 5; index++ {
		out.ScoreBuckets = append(out.ScoreBuckets, ScoreBucket{From: index * 20, To: index*20 + 20, Count: buckets[index]})
	}
	if out.ByAgent, err = service.store.ByAgent(ctx, since); err != nil {
		return Overview{}, err
	}
	return out, nil
}
