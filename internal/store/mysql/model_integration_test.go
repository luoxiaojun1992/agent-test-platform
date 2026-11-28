package mysqlstore

import (
	"context"
	"os"
	"testing"
	"time"

	"agent-test-platform/contracts"
	"agent-test-platform/internal/modelcfg"
	"agent-test-platform/internal/run"
	"agent-test-platform/migrations"

	"github.com/go-sql-driver/mysql"
)

// TestModelSelectionReevaluationAndCancelOnMySQL84 exercises the model-selection, re-evaluation and
// EVALUATING-cancel SQL on a real MySQL 8.4 instance.
func TestModelSelectionReevaluationAndCancelOnMySQL84(t *testing.T) {
	dsn := os.Getenv("ATP_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set ATP_TEST_MYSQL_DSN to run the MySQL 8.4 integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	adminConfig, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	adminConfig.DBName = ""
	adminDB := openPublishMySQL(t, adminConfig)
	t.Cleanup(func() { _ = adminDB.Close() })
	name := "atp_eval_" + randomPublishHex(t, 8)
	if _, err := adminDB.ExecContext(ctx, "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatal(err)
	}
	config := *adminConfig
	config.DBName = name
	config.ParseTime = true
	config.Loc = time.UTC
	database := openPublishMySQL(t, &config)
	defer database.Close()
	database.SetMaxOpenConns(1)
	if err := migrations.Apply(ctx, database); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS = 0"); err != nil {
		t.Fatal(err)
	}

	models, _ := NewModelStore(database)
	for _, m := range [][2]string{{modelcfg.RoleJudge, "judge-a"}, {modelcfg.RoleJudge, "judge-b"}, {modelcfg.RoleGeneration, "gen-a"}} {
		if err := models.Register(ctx, m[0], m[1], "openai", "m", "https://x"); err != nil {
			t.Fatal(err)
		}
	}
	active := func() map[string]bool {
		items, err := models.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, item := range items {
			if item.Active {
				out[item.Role+"/"+item.Alias] = true
			}
		}
		return out
	}
	if got := active(); len(got) != 2 || !got["judge/judge-a"] || !got["generation/gen-a"] {
		t.Fatalf("first registered must be active: %v", got)
	}
	if err := models.Activate(ctx, modelcfg.RoleJudge, "judge-b"); err != nil {
		t.Fatal(err)
	}
	if got := active(); len(got) != 2 || !got["judge/judge-b"] {
		t.Fatalf("activation must switch the judge: %v", got)
	}
	if err := models.Activate(ctx, modelcfg.RoleJudge, "nope"); err == nil {
		t.Fatal("unknown alias must fail")
	}

	validator, _ := contracts.NewValidator()
	store, _ := NewRunStore(database, validator)
	if ok, _ := store.JudgeAliasExists(ctx, "judge-a"); !ok {
		t.Fatal("judge-a must exist")
	}
	if ok, _ := store.JudgeAliasExists(ctx, "gen-a"); ok {
		t.Fatal("generation alias is not a judge")
	}

	const source = "01HZZZZZZZZZZZZZZZZZZZZZZ1"
	if _, err := database.ExecContext(ctx, `INSERT INTO test_run (id, idempotency_key, test_case_revision_id, dataset_revision_id, agent_id,
		status, execution_outcome, input_text, final_output) VALUES (?, 'k-source', 'T', 'D', 'A', 'COMPLETED', 'EXECUTED', 'hello', 'out')`, source); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO tool_call_record (id, test_run_id, sequence_no, call_id, tool_name, arguments_json, arguments_sha256, status, trace_source, truncated, duration_ms, started_at)
		VALUES ('01HZZZZZZZZZZZZZZZZZZZZZC1', ?, 1, 'c1', 'run_query', JSON_OBJECT(), REPEAT('a', 64), 'SUCCEEDED', 'AGENT_REPORTED', FALSE, 5, UTC_TIMESTAMP(6))`, source); err != nil {
		t.Fatal(err)
	}
	const fresh = "01HZZZZZZZZZZZZZZZZZZZZZZ2"
	if err := store.Reevaluate(ctx, source, fresh, "key-1", "digest", "judge-b"); err != nil {
		t.Fatal(err)
	}
	var status, alias string
	var calls, items int
	if err := database.QueryRowContext(ctx, `SELECT r.status, a.judge_model_alias, (SELECT COUNT(*) FROM tool_call_record WHERE test_run_id = r.id),
		(SELECT COUNT(*) FROM work_item WHERE aggregate_id = r.id) FROM test_run r JOIN evaluation_attempt a ON a.test_run_id = r.id WHERE r.id = ?`, fresh).
		Scan(&status, &alias, &calls, &items); err != nil {
		t.Fatal(err)
	}
	if status != "EVALUATING" || alias != "judge-b" || calls != 1 || items != 1 {
		t.Fatalf("re-evaluation Run: %s %s %d %d", status, alias, calls, items)
	}
	var sourceStatus string
	_ = database.QueryRowContext(ctx, `SELECT status FROM test_run WHERE id = ?`, source).Scan(&sourceStatus)
	if sourceStatus != "COMPLETED" {
		t.Fatalf("source Run changed: %s", sourceStatus)
	}
	if creation, err := store.FindCreation(ctx, "key-1"); err != nil || len(creation.RunIDs) != 1 || creation.RunIDs[0] != fresh {
		t.Fatalf("creation lookup: %v %v", creation, err)
	}
	if err := store.Reevaluate(ctx, fresh, "01HZZZZZZZZZZZZZZZZZZZZZZ3", "key-2", "d", "judge-a"); err == nil {
		t.Fatal("an EVALUATING Run must not be re-evaluated")
	}

	cancelled, err := store.Cancel(ctx, fresh)
	if err != nil || cancelled.Status != run.StatusCancelled {
		t.Fatalf("cancel: %+v %v", cancelled, err)
	}
	var attemptStatus, itemStatus string
	_ = database.QueryRowContext(ctx, `SELECT status FROM evaluation_attempt WHERE test_run_id = ?`, fresh).Scan(&attemptStatus)
	_ = database.QueryRowContext(ctx, `SELECT status FROM work_item WHERE aggregate_id = ?`, fresh).Scan(&itemStatus)
	if attemptStatus != "CANCELLED" || itemStatus != "DONE" {
		t.Fatalf("attempt=%s item=%s", attemptStatus, itemStatus)
	}
}
