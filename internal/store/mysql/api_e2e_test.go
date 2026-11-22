package mysqlstore_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"agent-test-platform/contracts"
	"agent-test-platform/internal/evaluation"
	"agent-test-platform/internal/httpapi"
	"agent-test-platform/internal/jobs/evalworker"
	"agent-test-platform/internal/jobs/mysqlqueue"
	"agent-test-platform/internal/llm"
	"agent-test-platform/internal/mockllm"
	"agent-test-platform/internal/modelcfg"
	"agent-test-platform/internal/run"
	mysqlstore "agent-test-platform/internal/store/mysql"
	"agent-test-platform/migrations"

	"github.com/go-sql-driver/mysql"

	"database/sql"
)

type (
	genStub          struct{ httpapi.GenerationService }
	secretStub       struct{ httpapi.SecretService }
	validationDBStub struct{ httpapi.TargetService }
	agentStub        struct{ httpapi.AgentService }
	catalogStub      struct{ httpapi.CatalogService }
)

// TestHTTPAPIToMockJudgeEndToEnd drives the public HTTP API (models, re-evaluation, evaluations, cancel)
// over real services, MySQL 8.4, the evaluation worker and the Mock LLM Judge.
func TestHTTPAPIToMockJudgeEndToEnd(t *testing.T) {
	dsn := os.Getenv("ATP_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set ATP_TEST_MYSQL_DSN to run the end-to-end test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	adminConfig, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	adminConfig.DBName = ""
	admin := open(t, adminConfig)
	defer admin.Close()
	name := "atp_api_e2e_" + time.Now().Format("150405.000000")
	name = strings.NewReplacer(".", "_").Replace(name)
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatal(err)
	}
	config := *adminConfig
	config.DBName, config.ParseTime, config.Loc = name, true, time.UTC
	setup := open(t, &config)
	defer setup.Close()
	if err := migrations.Apply(ctx, setup); err != nil {
		t.Fatal(err)
	}
	config.Params = map[string]string{"foreign_key_checks": "0"}
	database := open(t, &config)
	defer database.Close()

	caseJSON := `{"expected_output":{"type":"EXACT","value":"ok"},"tool_evaluation_mode":"SESSION_LLM_ONLY","expected_tool_call_chain":null,
		"evaluation_criteria":[{"name":"correct","description":"answer is right","weight":1}],"generation_assumptions":[]}`
	must := func(_ sql.Result, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(database.ExecContext(ctx, `INSERT INTO test_case_revision (id, test_case_id, generation_job_id, scenario_revision_id, dataset_revision_id, revision_no, schema_version,
		agent_mode, tool_evaluation_mode, content_json, content_sha256, lifecycle_status) VALUES ('T', 'TC', 'G', 'S', 'D', 1, '1.0', 'SINGLE_TURN', 'SESSION_LLM_ONLY', ?, REPEAT('a', 64), 'ACTIVE')`, caseJSON))
	const source = "01HZZZZZZZZZZZZZZZZZZZZZZ1"
	must(database.ExecContext(ctx, `INSERT INTO test_run (id, idempotency_key, test_case_revision_id, dataset_revision_id, agent_id,
		status, execution_outcome, input_text, final_output, deterministic_json) VALUES (?, 'k-source', 'T', 'D', 'A', 'COMPLETED', 'COMPLETED', 'hello', 'ok',
		JSON_ARRAY(JSON_OBJECT('id','state_diff','category','STATE','critical',TRUE,'weight',1,'pass',TRUE,'expected',JSON_OBJECT(),'actual',JSON_OBJECT())))`, source))

	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	runStore, _ := mysqlstore.NewRunStore(database)
	runService, _ := run.NewService(runStore)
	modelStore, _ := mysqlstore.NewModelStore(database)
	for _, alias := range []string{"judge-a", "judge-b"} {
		if err := modelStore.Register(ctx, modelcfg.RoleJudge, alias, "openai", "mock", "http://x"); err != nil {
			t.Fatal(err)
		}
	}
	modelService, _ := modelcfg.NewService(modelStore)
	api, err := httpapi.NewServer(genStub{}, secretStub{}, validationDBStub{}, agentStub{}, catalogStub{}, runService, validator, "api-secret")
	if err != nil {
		t.Fatal(err)
	}
	api.SetModelService(modelService)
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

	if code, _ := call("PUT", "/api/v1/models/judge/active", "", `{"alias":"judge-b"}`); code != 200 {
		t.Fatal("activate", code)
	}
	if code, body := call("POST", "/api/v1/test-runs/"+source+"/evaluations", "reeval-key-1", `{"judge_model_alias":"unknown"}`); code != 400 {
		t.Fatal("unknown judge", code, body)
	}
	code, body := call("POST", "/api/v1/test-runs/"+source+"/evaluations", "reeval-key-1", `{"judge_model_alias":"judge-b"}`)
	ids, _ := body["run_ids"].([]any)
	if code != 202 || len(ids) != 1 {
		t.Fatal("re-evaluate", code, body)
	}
	fresh := ids[0].(string)
	if code, body := call("POST", "/api/v1/test-runs/"+source+"/evaluations", "reeval-key-1", `{"judge_model_alias":"judge-b"}`); code != 202 || body["run_ids"].([]any)[0] != fresh {
		t.Fatal("replay must return the same Run", code, body)
	}
	if code, _ := call("POST", "/api/v1/test-runs/"+source+"/evaluations", "reeval-key-1", `{"judge_model_alias":"judge-a"}`); code != 409 {
		t.Fatal("key reuse with different body", code)
	}
	if _, body := call("GET", "/api/v1/test-runs/"+fresh+"/evaluations", "", ""); body["items"].([]any)[0].(map[string]any)["status"] != "PENDING" {
		t.Fatal("attempt must be PENDING", body)
	}

	mock, _ := mockllm.NewServer("llm-secret")
	llmServer := httptest.NewServer(mock)
	defer llmServer.Close()
	adapter, _ := llm.NewOpenAIAdapter(llmServer.URL+"/v1", "llm-secret", &http.Client{Timeout: 10 * time.Second})
	gateway, err := llm.NewGateway([]llm.ModelConfig{{Alias: "judge-b", Model: llm.ModelRef{Provider: llm.ProviderOpenAI, Model: "mock"},
		Capabilities: llm.ModelCapabilities{ToolCalling: true, NamedToolChoice: true, JSONSchemaOutput: true, Usage: true, ContextWindow: 32768, MaxOutputTokens: 4096, TokenizerID: "conservative-v1"},
		Adapter:      adapter, Timeout: 10 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	judge, _ := evaluation.NewJudge(gateway, validator, "judge-b", 2048)
	evaluator, _ := evalworker.NewJudgeEvaluator(judge, evalworker.JudgeModel{Alias: "judge-b", Provider: "openai", Model: "mock", BaseURL: llmServer.URL, MaxTokens: 2048})
	router, _ := evalworker.NewRouter("judge-b", map[string]evalworker.Evaluator{"judge-b": evaluator})
	queue, _ := mysqlqueue.New(database)
	worker, err := evalworker.New(queue, runStore, router, evalworker.Config{Owner: "e2e", LeaseDuration: time.Minute, HeartbeatInterval: 20 * time.Second, PollInterval: time.Second, StageTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if processed, err := worker.ProcessNext(ctx); !processed || err != nil {
		t.Fatal(processed, err)
	}
	_, body = call("GET", "/api/v1/test-runs/"+fresh, "", "")
	if body["status"] != "COMPLETED" {
		t.Fatal("run", body)
	}
	_, body = call("GET", "/api/v1/test-runs/"+fresh+"/evaluations", "", "")
	attempt := body["items"].([]any)[0].(map[string]any)
	if attempt["status"] != "COMPLETED" || attempt["judge_model_alias"] != "judge-b" || attempt["pass"] != true {
		t.Fatal("attempt", attempt)
	}
	if _, body := call("GET", "/api/v1/test-runs/"+source, "", ""); body["status"] != "COMPLETED" {
		t.Fatal("source Run must be untouched", body)
	}
	if code, _ := call("POST", "/api/v1/test-runs/"+fresh+"/cancel", "cancel-key-1", ""); code != 409 {
		t.Fatal("a completed Run cannot be cancelled", code)
	}
}

func open(t *testing.T, config *mysql.Config) *sql.DB {
	t.Helper()
	connector, err := mysql.NewConnector(config)
	if err != nil {
		t.Fatal(err)
	}
	return sql.OpenDB(connector)
}
