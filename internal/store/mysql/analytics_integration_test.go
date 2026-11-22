package mysqlstore_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"agent-test-platform/internal/analytics"
	mysqlstore "agent-test-platform/internal/store/mysql"
	"agent-test-platform/migrations"

	"github.com/go-sql-driver/mysql"
)

// TestAnalyticsAggregatesOnMySQL84 seeds Runs, Evaluation Attempts and asset revisions and checks the dashboard aggregates: the verdict of a Run is its
// latest COMPLETED attempt, unevaluated Runs count toward totals but not toward the pass rate, and score buckets are 20 points wide.
func TestAnalyticsAggregatesOnMySQL84(t *testing.T) {
	dsn := os.Getenv("ATP_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set ATP_TEST_MYSQL_DSN to run the analytics integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	adminConfig, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	adminConfig.DBName, adminConfig.ParseTime, adminConfig.Loc = "", true, time.UTC
	admin := open(t, adminConfig)
	defer admin.Close()
	name := strings.NewReplacer(".", "_").Replace("atp_analytics_" + time.Now().Format("150405.000000"))
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatal(err)
	}
	config := *adminConfig
	config.DBName = name
	config.Params = map[string]string{"foreign_key_checks": "0"}
	database := open(t, &config)
	defer database.Close()
	if err := migrations.Apply(ctx, database); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := database.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	exec(`INSERT INTO agent (id, name, chat_endpoint, target_bearer_token_ref, target_model, contract_version, tool_evaluation_mode, config_json, config_sha256, idempotency_key)
		VALUES ('AG', 'bot', 'http://x', 't', 'm', '1.0', 'SESSION_LLM_ONLY', JSON_OBJECT(), REPEAT('a', 64), 'k')`)
	run := func(id, status string, createdAt time.Time) {
		exec(`INSERT INTO test_run (id, idempotency_key, test_case_revision_id, dataset_revision_id, agent_id, status, input_text, created_at) VALUES (?, ?, 'T', 'D', 'AG', ?, 'x', ?)`, id, "k-"+id, status, createdAt)
	}
	attempt := func(id, runID string, no int, status string, score float64, pass bool) {
		exec(`INSERT INTO evaluation_attempt (id, test_run_id, attempt_no, status, judge_model_config_id, judge_model_alias, judge_provider, judge_model, judge_prompt_version, judge_config_sha256, request_json, final_score, pass)
			VALUES (?, ?, ?, ?, 'M', 'judge', 'openai', 'm', 'p', REPEAT('c', 64), JSON_OBJECT(), ?, ?)`, id, runID, no, status, score, pass)
	}
	now := time.Now().UTC()
	yesterday := now.AddDate(0, 0, -1)
	old := now.AddDate(0, 0, -60)
	run("R1", "COMPLETED", now)
	attempt("E1", "R1", 1, "COMPLETED", 55, false)
	attempt("E2", "R1", 2, "COMPLETED", 95, true)
	attempt("E3", "R1", 3, "FAILED", 0, false)
	run("R2", "COMPLETED", now)
	attempt("E4", "R2", 1, "COMPLETED", 30, false)
	run("R3", "RUNNING", yesterday)
	run("R4", "COMPLETED", old)
	attempt("E5", "R4", 1, "COMPLETED", 100, true)
	exec(`INSERT INTO scenario (id) VALUES ('S1'), ('S2')`)
	exec(`INSERT INTO scenario_revision (id, scenario_id, generation_job_id, revision_no, quality_level, lifecycle_status, content_json, content_sha256, created_at)
		VALUES ('SR1', 'S1', 'G', 1, 'PASSED', 'ACTIVE', JSON_OBJECT(), REPEAT('d', 64), ?), ('SR2', 'S2', 'G', 1, 'PASSED', 'DEPRECATED', JSON_OBJECT(), REPEAT('e', 64), ?)`, now, old)

	store, err := mysqlstore.NewAnalyticsStore(database)
	if err != nil {
		t.Fatal(err)
	}
	service, err := analytics.NewService(store)
	if err != nil {
		t.Fatal(err)
	}
	overview, err := service.Overview(ctx, 14)
	if err != nil {
		t.Fatal(err)
	}
	today := overview.DailyRuns[len(overview.DailyRuns)-1]
	if today.Total != 2 || today.Evaluated != 2 || today.Passed != 1 || today.PassRate == nil || *today.PassRate != 50 {
		t.Fatalf("today = %+v (the latest COMPLETED attempt decides: R1 passes at 95, R2 fails at 30)", today)
	}
	if today.AvgScore == nil || *today.AvgScore != 62.5 {
		t.Fatalf("average score = %v, want 62.5", today.AvgScore)
	}
	day := overview.DailyRuns[len(overview.DailyRuns)-2]
	if day.Total != 1 || day.Evaluated != 0 || day.PassRate != nil {
		t.Fatalf("an unevaluated Run counts in the total but has no rate: %+v", day)
	}
	buckets := map[int]int{}
	for _, bucket := range overview.ScoreBuckets {
		buckets[bucket.From] = bucket.Count
	}
	if buckets[80] != 1 || buckets[20] != 1 || buckets[60] != 0 {
		t.Fatalf("score buckets = %+v", overview.ScoreBuckets)
	}
	statuses := map[string]int{}
	for _, status := range overview.RunStatuses {
		statuses[status.Status] = status.Count
	}
	if statuses["COMPLETED"] != 3 || statuses["RUNNING"] != 1 {
		t.Fatalf("statuses (all time) = %+v", statuses)
	}
	if len(overview.ByAgent) != 1 || overview.ByAgent[0].Runs != 3 || overview.ByAgent[0].Evaluated != 2 || overview.ByAgent[0].AgentName != "bot" {
		t.Fatalf("by agent (the 60-day-old Run is outside the window) = %+v", overview.ByAgent)
	}
	var scenarios analytics.AssetCount
	for _, asset := range overview.Assets {
		if asset.Kind == "scenarios" {
			scenarios = asset
		}
	}
	if scenarios.Resources != 2 || scenarios.Revisions != 2 || scenarios.Usable != 1 {
		t.Fatalf("scenarios = %+v (a DEPRECATED revision is not usable)", scenarios)
	}
	if last := overview.DailyAssets[len(overview.DailyAssets)-1]; last.Scenarios != 1 {
		t.Fatalf("a scenario created today must show on today's bar: %+v", last)
	}
	if overview.Agents.Agents != 1 {
		t.Fatalf("agents = %+v", overview.Agents)
	}
}
