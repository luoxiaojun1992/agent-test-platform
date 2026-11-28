package mysqlstore_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-test-platform/contracts"
	"agent-test-platform/internal/evaluation"
	"agent-test-platform/internal/httpapi"
	"agent-test-platform/internal/jobs/evalworker"
	"agent-test-platform/internal/jobs/mysqlqueue"
	"agent-test-platform/internal/jobs/runworker"
	"agent-test-platform/internal/llm"
	"agent-test-platform/internal/mockllm"
	"agent-test-platform/internal/mocktarget"
	"agent-test-platform/internal/modelcfg"
	"agent-test-platform/internal/run"
	"agent-test-platform/internal/runexec"
	mysqlstore "agent-test-platform/internal/store/mysql"
	"agent-test-platform/migrations"

	"github.com/go-sql-driver/mysql"
)

// fixedSecrets resolves the Agent token and the Agent database password.
type fixedSecrets struct{ databasePassword string }

func (f fixedSecrets) Resolve(_ context.Context, name string) (string, error) {
	if name == "agent-db-password" {
		return f.databasePassword, nil
	}
	return "mock-target-token", nil
}

// TestRunLifecycleWithMockTargetAndJudgeEndToEnd covers the whole service layer: HTTP API creates Runs, the run
// worker provisions an isolated database on real MySQL and calls the Mock Target Agent over SSE, the evaluation
// worker judges the result through the Mock LLM, and the API reports the finished Run and its evaluation.
func TestRunLifecycleWithMockTargetAndJudgeEndToEnd(t *testing.T) {
	dsn := os.Getenv("ATP_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set ATP_TEST_MYSQL_DSN to run the end-to-end test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	adminConfig, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	adminConfig.DBName = ""
	adminConfig.ParseTime, adminConfig.Loc = true, time.UTC
	admin := open(t, adminConfig)
	defer admin.Close()
	name := strings.NewReplacer(".", "_").Replace("atp_run_e2e_" + time.Now().Format("150405.000000"))
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatal(err)
	}
	config := *adminConfig
	config.DBName = name
	setup := open(t, &config)
	defer setup.Close()
	if err := migrations.Apply(ctx, setup); err != nil {
		t.Fatal(err)
	}
	config.Params = map[string]string{"foreign_key_checks": "0"}
	database := open(t, &config)
	defer database.Close()

	const (
		agentID = "01HZZZZZZZZZZZZZZZZZZZZZZ2"
	)
	caseJSON := `{"schema_version":"1.0","prompt":"How many rows are in t?","agent_mode":"ANSWER","preconditions":[],"postconditions":[],
		"allowed_state_changes":[],"knowledge_documents":[],"tool_evaluation_mode":"SESSION_LLM_ONLY","expected_tool_call_chain":null,
		"expected_output":{"type":"SEMANTIC","value":"one row"},
		"evaluation_criteria":[{"name":"correct","description":"states the row count","weight":1}],"generation_assumptions":[]}`
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	targetAgent, err := mocktarget.NewServer(mocktarget.Config{BearerToken: "mock-target-token", ModelID: "mock-agent-v1", IndexDelay: 0}, validator)
	if err != nil {
		t.Fatal(err)
	}
	targetServer := httptest.NewServer(targetAgent)
	defer targetServer.Close()

	exec := func(query string, args ...any) {
		t.Helper()
		query = strings.ReplaceAll(query, "@@ENDPOINT@@", targetServer.URL)
		if _, err := database.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	exec(`INSERT INTO test_case_revision (id, test_case_id, generation_job_id, scenario_revision_id, dataset_revision_id, revision_no, schema_version, agent_mode, tool_evaluation_mode, content_json, content_sha256, lifecycle_status)
		VALUES ('01HZZZZZZZZZZZZZZZZZZZZZT1', 'TC', 'G', 'S', '01HZZZZZZZZZZZZZZZZZZZZZD1', 1, '1.0', 'ANSWER', 'SESSION_LLM_ONLY', ?, REPEAT('a', 64), 'ACTIVE')`, caseJSON)
	exec(`INSERT INTO dataset_revision (id, dataset_id, generation_job_id, scenario_revision_id, revision_no, schema_sql, seed_sql, constraints_json, metadata_json, content_sha256, lifecycle_status, execution_database)
		VALUES ('01HZZZZZZZZZZZZZZZZZZZZZD1', 'DS', 'G', 'S', 1, 'CREATE TABLE t (id INT PRIMARY KEY, v VARCHAR(10));', 'INSERT INTO t (id, v) VALUES (1, ''a'');', JSON_OBJECT(), JSON_OBJECT(), REPEAT('b', 64), 'ACTIVE', 'unused')`)
	host, portText, _ := net.SplitHostPort(adminConfig.Addr)
	port, _ := strconv.Atoi(portText)
	exec(`INSERT INTO database_profile (id, name, engine, host, port, database_name, username, password_secret_ref, tls_mode, snapshot_mode, config_json, config_sha256, idempotency_key)
		VALUES ('01HZZZZZZZZZZZZZZZZZZZZZP1', 'agent-db', 'MYSQL', ?, ?, 'agent_db', ?, 'agent-db-password', 'DISABLED_LOCAL_ONLY', 'SCHEMA_ONLY', JSON_OBJECT(), REPEAT('f', 64), 'profile-key')`, host, port, adminConfig.User)
	exec(`INSERT INTO agent (id, name, database_profile_id, chat_endpoint, target_bearer_token_ref, target_model, contract_version, tool_evaluation_mode, config_json, config_sha256, idempotency_key)
		VALUES ('` + agentID + `', 'mock-agent', '01HZZZZZZZZZZZZZZZZZZZZZP1', '@@ENDPOINT@@', 'tok', 'mock-agent-v1', '1.0', 'SESSION_LLM_ONLY', JSON_OBJECT(), REPEAT('d', 64), 'agent-key')`)

	runStore, _ := mysqlstore.NewRunStore(database, validator)
	runService, _ := run.NewService(runStore, validator)
	modelStore, _ := mysqlstore.NewModelStore(database)
	if err := modelStore.Register(ctx, modelcfg.RoleJudge, "judge-a", "openai", "mock", "http://x"); err != nil {
		t.Fatal(err)
	}
	api, err := httpapi.NewServer(genStub{}, secretStub{}, validationDBStub{}, agentStub{}, catalogStub{}, runService, validator, "api-secret")
	if err != nil {
		t.Fatal(err)
	}
	apiServer := httptest.NewServer(api)
	defer apiServer.Close()
	call := func(method, path, key, body string) (int, map[string]any) {
		t.Helper()
		request, _ := http.NewRequest(method, apiServer.URL+path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer api-secret")
		request.Header.Set("Content-Type", "application/json")
		if key != "" {
			request.Header.Set("Idempotency-Key", key)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, _ := io.ReadAll(response.Body)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return response.StatusCode, out
	}

	exec(`INSERT INTO agent (id, name, chat_endpoint, target_bearer_token_ref, target_model, contract_version, tool_evaluation_mode, config_json, config_sha256, idempotency_key)
		VALUES ('01HZZZZZZZZZZZZZZZZZZZZZZ3', 'no-db-agent', '@@ENDPOINT@@', 'tok', 'mock-agent-v1', '1.0', 'SESSION_LLM_ONLY', JSON_OBJECT(), REPEAT('2', 64), 'agent-key-2')`)
	if code, body := call("POST", "/api/v1/test-runs", "create-runs-nodb", `{"agent_id":"01HZZZZZZZZZZZZZZZZZZZZZZ3","test_case_revision_ids":["01HZZZZZZZZZZZZZZZZZZZZZT1"]}`); code != 409 || body["code"] != "AGENT_DATABASE_NOT_CONFIGURED" {
		t.Fatal("an Agent without a database must be refused", code, body)
	}

	code, body := call("POST", "/api/v1/test-runs", "create-runs-1", `{"agent_id":"`+agentID+`","test_case_revision_ids":["01HZZZZZZZZZZZZZZZZZZZZZT1"]}`)
	ids, _ := body["run_ids"].([]any)
	if code != 202 || len(ids) != 1 {
		t.Fatal("create runs", code, body)
	}
	runID := ids[0].(string)
	if _, body := call("GET", "/api/v1/test-runs/"+runID, "", ""); body["status"] != "QUEUED" {
		t.Fatal("expected QUEUED", body)
	}

	queue, _ := mysqlqueue.New(database)
	toolCalls, _ := mysqlstore.NewToolCallStore(database)
	executor, err := runexec.New(runexec.Deps{
		Secrets: fixedSecrets{databasePassword: adminConfig.Passwd}, ToolCalls: toolCalls, Cancel: runStore, HTTP: &http.Client{}, Validator: validator})
	if err != nil {
		t.Fatal(err)
	}
	runWorker, err := runworker.New(queue, runStore, executor, runworker.Config{Owner: "e2e", LeaseDuration: time.Minute, HeartbeatInterval: 20 * time.Second, PollInterval: time.Second, StageTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if processed, err := runWorker.ProcessNext(ctx); !processed || err != nil {
		t.Fatal("run worker", processed, err)
	}
	_, body = call("GET", "/api/v1/test-runs/"+runID, "", "")
	if body["status"] != "EVALUATING" || body["execution_outcome"] != "COMPLETED" {
		t.Fatal("after execution", body)
	}
	if out, _ := body["final_output"].(string); !strings.Contains(out, "Mock response from mock-agent-v1") || !strings.Contains(out, "`") {
		t.Fatal("final output must come from the Mock Target and carry the database-name prompt", body["final_output"])
	}
	_, body = call("GET", "/api/v1/test-runs/"+runID+"/evaluations", "", "")
	if body["items"].([]any)[0].(map[string]any)["status"] != "PENDING" {
		t.Fatal("attempt must be PENDING before evaluation", body)
	}

	mock, _ := mockllm.NewServer("llm-secret")
	llmServer := httptest.NewServer(mock)
	defer llmServer.Close()
	adapter, _ := llm.NewOpenAIAdapter(llmServer.URL+"/v1", "llm-secret", &http.Client{Timeout: 10 * time.Second})
	gateway, err := llm.NewGateway([]llm.ModelConfig{{Alias: "judge-a", Model: llm.ModelRef{Provider: llm.ProviderOpenAI, Model: "mock"},
		Capabilities: llm.ModelCapabilities{ToolCalling: true, NamedToolChoice: true, JSONSchemaOutput: true, Usage: true, ContextWindow: 32768, MaxOutputTokens: 4096, TokenizerID: "conservative-v1"},
		Adapter:      adapter, Timeout: 10 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	judge, _ := evaluation.NewJudge(gateway, validator, "judge-a", 2048)
	evaluator, _ := evalworker.NewJudgeEvaluator(judge, evalworker.JudgeModel{Alias: "judge-a", Provider: "openai", Model: "mock", BaseURL: llmServer.URL, MaxTokens: 2048}, fakeSecretResolver{}, &http.Client{}, validator)
	router, _ := evalworker.NewRouter("judge-a", map[string]evalworker.Evaluator{"judge-a": evaluator})
	evalWorker, err := evalworker.New(queue, runStore, router, evalworker.Config{Owner: "e2e", LeaseDuration: time.Minute, HeartbeatInterval: 20 * time.Second, PollInterval: time.Second, StageTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if processed, err := evalWorker.ProcessNext(ctx); !processed || err != nil {
		t.Fatal("evaluation worker", processed, err)
	}
	_, body = call("GET", "/api/v1/test-runs/"+runID, "", "")
	if body["status"] != "COMPLETED" {
		t.Fatal("final status", body)
	}
	_, body = call("GET", "/api/v1/test-runs/"+runID+"/evaluations", "", "")
	attempt := body["items"].([]any)[0].(map[string]any)
	if attempt["status"] != "COMPLETED" || attempt["pass"] != true {
		t.Fatal("attempt", attempt)
	}

	var leftover int
	_ = admin.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name = CONCAT('run_', LOWER(?))`, runID).Scan(&leftover)
	if leftover != 0 {
		t.Fatalf("the Run database must be dropped after execution, found %d", leftover)
	}
}
