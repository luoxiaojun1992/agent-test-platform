package mysqlstore

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"agent-test-platform/contracts"
	"agent-test-platform/internal/evaluation"
	"agent-test-platform/internal/jobs/evalworker"
	"agent-test-platform/internal/jobs/mysqlqueue"
	"agent-test-platform/internal/llm"
	"agent-test-platform/internal/mockllm"
	"agent-test-platform/migrations"

	"github.com/go-sql-driver/mysql"
)

// TestEvaluationPipelineWithMockJudgeOnMySQL84 runs the real evaluation worker, queue and store against MySQL 8.4,
// with the Judge served by the Mock LLM over HTTP: re-evaluation Run -> PENDING attempt -> RUNNING -> COMPLETED.
func TestEvaluationPipelineWithMockJudgeOnMySQL84(t *testing.T) {
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
	name := "atp_e2e_" + randomPublishHex(t, 8)
	if _, err := adminDB.ExecContext(ctx, "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatal(err)
	}
	config := *adminConfig
	config.DBName = name
	config.ParseTime = true
	config.Loc = time.UTC
	database := openPublishMySQL(t, &config)
	defer database.Close()
	if err := migrations.Apply(ctx, database); err != nil {
		t.Fatal(err)
	}
	config.Params = map[string]string{"foreign_key_checks": "0"}
	workerDB := openPublishMySQL(t, &config)
	defer workerDB.Close()

	const source = "01HZZZZZZZZZZZZZZZZZZZZZZ1"
	caseJSON := `{"expected_output":{"type":"EXACT","value":"ok"},
		"evaluation_criteria":[{"name":"correct","description":"answer is right","weight":1}],"generation_assumptions":[]}`
	if _, err := workerDB.ExecContext(ctx, `INSERT INTO test_case_revision (id, test_case_id, generation_job_id, scenario_revision_id, dataset_revision_id, revision_no, schema_version,
		content_json, content_sha256, lifecycle_status) VALUES ('T', 'TC', 'G', 'S', 'D', 1, '1.0', ?, REPEAT('a', 64), 'ACTIVE')`, caseJSON); err != nil {
		t.Fatal(err)
	}
	if _, err := workerDB.ExecContext(ctx, `INSERT INTO test_run (id, idempotency_key, test_case_revision_id, dataset_revision_id, agent_id,
		status, execution_outcome, input_text, final_output) VALUES (?, 'k-source', 'T', 'D', 'A', 'COMPLETED', 'COMPLETED', 'hello', 'ok')`, source); err != nil {
		t.Fatal(err)
	}

	mock, err := mockllm.NewServer("llm-secret")
	if err != nil {
		t.Fatal(err)
	}
	llmServer := httptest.NewServer(mock)
	defer llmServer.Close()
	adapter, err := llm.NewOpenAIAdapter(llmServer.URL+"/v1", "llm-secret", &http.Client{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := llm.NewGateway([]llm.ModelConfig{{Alias: "judge-a", Model: llm.ModelRef{Provider: llm.ProviderOpenAI, Model: "mock"},
		Capabilities: llm.ModelCapabilities{ToolCalling: true, NamedToolChoice: true, JSONSchemaOutput: true, Usage: true, ContextWindow: 32768, MaxOutputTokens: 4096, TokenizerID: "conservative-v1"},
		Adapter:      adapter, Timeout: 10 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	judge, err := evaluation.NewJudge(gateway, validator, "judge-a", 2048)
	if err != nil {
		t.Fatal(err)
	}
	evaluator, err := evalworker.NewJudgeEvaluator(judge, evalworker.JudgeModel{Alias: "judge-a", Provider: "openai", Model: "mock", BaseURL: llmServer.URL, MaxTokens: 2048}, fakeSecretResolver{}, &http.Client{}, validator)
	if err != nil {
		t.Fatal(err)
	}
	router, err := evalworker.NewRouter("judge-a", map[string]evalworker.Evaluator{"judge-a": evaluator})
	if err != nil {
		t.Fatal(err)
	}
	store, _ := NewRunStore(workerDB)
	queue, err := mysqlqueue.New(workerDB)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := evalworker.New(queue, store, router, evalworker.Config{Owner: "e2e", LeaseDuration: time.Minute, HeartbeatInterval: 20 * time.Second, PollInterval: time.Second, StageTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}

	const fresh = "01HZZZZZZZZZZZZZZZZZZZZZZ2"
	if err := store.Reevaluate(ctx, source, fresh, "key-1", "digest", "judge-a"); err != nil {
		t.Fatal(err)
	}
	if processed, err := worker.ProcessNext(ctx); !processed || err != nil {
		t.Fatalf("processed=%v err=%v", processed, err)
	}
	var runStatus, attemptStatus string
	var provider sql.NullString
	var errJSON sql.NullString
	var pass bool
	var final float64
	if err := workerDB.QueryRowContext(ctx, `SELECT r.status, a.status, a.judge_provider, IFNULL(a.pass, FALSE), IFNULL(a.final_score, 0), CAST(a.error_json AS CHAR) FROM test_run r JOIN evaluation_attempt a ON a.test_run_id = r.id WHERE r.id = ?`, fresh).
		Scan(&runStatus, &attemptStatus, &provider, &pass, &final, &errJSON); err != nil {
		t.Fatal(err)
	}
	if runStatus != "COMPLETED" || attemptStatus != "COMPLETED" || provider.String != "openai" || !pass || final < 70 {
		t.Fatalf("run=%s attempt=%s provider=%s pass=%v final=%v err=%s", runStatus, attemptStatus, provider.String, pass, final, errJSON.String)
	}
	var itemStatus string
	_ = workerDB.QueryRowContext(ctx, `SELECT status FROM work_item WHERE aggregate_id = ?`, fresh).Scan(&itemStatus)
	if itemStatus != "DONE" {
		t.Fatalf("work item %s", itemStatus)
	}
}

type fakeSecretResolver struct{}

func (fakeSecretResolver) Resolve(context.Context, string) (string, error) { return "secret", nil }
