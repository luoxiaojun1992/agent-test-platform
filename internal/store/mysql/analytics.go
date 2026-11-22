package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"agent-test-platform/internal/analytics"
)

// AnalyticsStore aggregates dashboard figures in Platform MySQL. A Run's verdict is its latest COMPLETED Evaluation Attempt.
type AnalyticsStore struct{ database *sql.DB }

func NewAnalyticsStore(database *sql.DB) (*AnalyticsStore, error) {
	if database == nil {
		return nil, errors.New("Platform MySQL database is required")
	}
	return &AnalyticsStore{database: database}, nil
}

// verdict joins each Run with its latest completed Evaluation Attempt.
const verdictFrom = `test_run r
LEFT JOIN evaluation_attempt e ON e.test_run_id = r.id AND e.status = 'COMPLETED'
	AND e.attempt_no = (SELECT MAX(x.attempt_no) FROM evaluation_attempt x WHERE x.test_run_id = r.id AND x.status = 'COMPLETED')`

func (store *AnalyticsStore) AssetCounts(ctx context.Context) ([]analytics.AssetCount, error) {
	kinds := []struct{ kind, logical, revision, usable string }{
		{"scenarios", "scenario", "scenario_revision", "lifecycle_status IN ('ACTIVE','VALIDATED')"},
		{"datasets", "dataset", "dataset_revision", "lifecycle_status IN ('ACTIVE','VALIDATED')"},
		{"test_cases", "test_case", "test_case_revision", "lifecycle_status IN ('ACTIVE','VALIDATED')"},
	}
	out := make([]analytics.AssetCount, 0, len(kinds))
	for _, kind := range kinds {
		item := analytics.AssetCount{Kind: kind.kind}
		if err := store.database.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM `+kind.logical+`), COUNT(*), COALESCE(SUM(`+kind.usable+`), 0) FROM `+kind.revision).
			Scan(&item.Resources, &item.Revisions, &item.Usable); err != nil {
			return nil, fmt.Errorf("count %s: %w", kind.kind, err)
		}
		out = append(out, item)
	}
	return out, nil
}

func (store *AnalyticsStore) AgentCounts(ctx context.Context) (analytics.AgentCount, error) {
	var out analytics.AgentCount
	if err := store.database.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM agent), (SELECT COUNT(*) FROM agent_version)`).Scan(&out.Agents, &out.Versions); err != nil {
		return out, fmt.Errorf("count agents: %w", err)
	}
	return out, nil
}

func (store *AnalyticsStore) RunStatuses(ctx context.Context) ([]analytics.StatusCount, error) {
	rows, err := store.database.QueryContext(ctx, `SELECT status, COUNT(*) FROM test_run GROUP BY status ORDER BY status`)
	if err != nil {
		return nil, fmt.Errorf("count run statuses: %w", err)
	}
	defer rows.Close()
	out := []analytics.StatusCount{}
	for rows.Next() {
		var item analytics.StatusCount
		if err := rows.Scan(&item.Status, &item.Count); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (store *AnalyticsStore) JobCounts(ctx context.Context, since time.Time) ([]analytics.JobCount, error) {
	rows, err := store.database.QueryContext(ctx, `SELECT job_kind, status, COUNT(*) FROM generation_job WHERE created_at >= ? GROUP BY job_kind, status ORDER BY job_kind, status`, since)
	if err != nil {
		return nil, fmt.Errorf("count generation jobs: %w", err)
	}
	defer rows.Close()
	out := []analytics.JobCount{}
	for rows.Next() {
		var item analytics.JobCount
		if err := rows.Scan(&item.Kind, &item.Status, &item.Count); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (store *AnalyticsStore) DailyRuns(ctx context.Context, since time.Time) (map[string]analytics.DailyRuns, error) {
	rows, err := store.database.QueryContext(ctx, `SELECT DATE_FORMAT(r.created_at, '%Y-%m-%d'), COUNT(*), COUNT(e.id), COALESCE(SUM(e.pass), 0), AVG(e.final_score)
FROM `+verdictFrom+` WHERE r.created_at >= ? GROUP BY 1`, since)
	if err != nil {
		return nil, fmt.Errorf("aggregate daily runs: %w", err)
	}
	defer rows.Close()
	out := map[string]analytics.DailyRuns{}
	for rows.Next() {
		var item analytics.DailyRuns
		var avg sql.NullFloat64
		if err := rows.Scan(&item.Date, &item.Total, &item.Evaluated, &item.Passed, &avg); err != nil {
			return nil, err
		}
		if item.Evaluated > 0 {
			rate := 100 * float64(item.Passed) / float64(item.Evaluated)
			item.PassRate = &rate
		}
		if avg.Valid {
			item.AvgScore = &avg.Float64
		}
		out[item.Date] = item
	}
	return out, rows.Err()
}

func (store *AnalyticsStore) DailyAssets(ctx context.Context, since time.Time) (map[string]analytics.DailyAssets, error) {
	out := map[string]analytics.DailyAssets{}
	for _, kind := range []struct {
		table string
		set   func(*analytics.DailyAssets, int)
	}{
		{"scenario_revision", func(d *analytics.DailyAssets, n int) { d.Scenarios = n }},
		{"dataset_revision", func(d *analytics.DailyAssets, n int) { d.Datasets = n }},
		{"test_case_revision", func(d *analytics.DailyAssets, n int) { d.TestCases = n }},
	} {
		rows, err := store.database.QueryContext(ctx, `SELECT DATE_FORMAT(created_at, '%Y-%m-%d'), COUNT(*) FROM `+kind.table+` WHERE created_at >= ? GROUP BY 1`, since)
		if err != nil {
			return nil, fmt.Errorf("aggregate daily %s: %w", kind.table, err)
		}
		for rows.Next() {
			var date string
			var count int
			if err := rows.Scan(&date, &count); err != nil {
				rows.Close()
				return nil, err
			}
			item := out[date]
			item.Date = date
			kind.set(&item, count)
			out[date] = item
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

func (store *AnalyticsStore) ScoreBuckets(ctx context.Context, since time.Time) (map[int]int, error) {
	rows, err := store.database.QueryContext(ctx, `SELECT LEAST(FLOOR(e.final_score / 20), 4), COUNT(*) FROM `+verdictFrom+` WHERE r.created_at >= ? AND e.final_score IS NOT NULL GROUP BY 1`, since)
	if err != nil {
		return nil, fmt.Errorf("aggregate score distribution: %w", err)
	}
	defer rows.Close()
	out := map[int]int{}
	for rows.Next() {
		var bucket, count int
		if err := rows.Scan(&bucket, &count); err != nil {
			return nil, err
		}
		out[bucket] = count
	}
	return out, rows.Err()
}

func (store *AnalyticsStore) ByAgentVersion(ctx context.Context, since time.Time) ([]analytics.AgentVersionStat, error) {
	rows, err := store.database.QueryContext(ctx, `SELECT a.name, v.version, v.id, COUNT(r.id), COUNT(e.id), COALESCE(SUM(e.pass), 0), AVG(e.final_score)
FROM test_run r JOIN agent_version v ON v.id = r.agent_version_id JOIN agent a ON a.id = v.agent_id
LEFT JOIN evaluation_attempt e ON e.test_run_id = r.id AND e.status = 'COMPLETED'
	AND e.attempt_no = (SELECT MAX(x.attempt_no) FROM evaluation_attempt x WHERE x.test_run_id = r.id AND x.status = 'COMPLETED')
WHERE r.created_at >= ? GROUP BY v.id, a.name, v.version ORDER BY COUNT(r.id) DESC, a.name, v.version LIMIT 12`, since)
	if err != nil {
		return nil, fmt.Errorf("aggregate by agent version: %w", err)
	}
	defer rows.Close()
	out := []analytics.AgentVersionStat{}
	for rows.Next() {
		var item analytics.AgentVersionStat
		var avg sql.NullFloat64
		if err := rows.Scan(&item.AgentName, &item.Version, &item.AgentVersionID, &item.Runs, &item.Evaluated, &item.Passed, &avg); err != nil {
			return nil, err
		}
		if item.Evaluated > 0 {
			rate := 100 * float64(item.Passed) / float64(item.Evaluated)
			item.PassRate = &rate
		}
		if avg.Valid {
			item.AvgScore = &avg.Float64
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
