package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-test-platform/internal/llm"
	"agent-test-platform/internal/mockllm"
	"agent-test-platform/internal/modelcfg"
	mysqlstore "agent-test-platform/internal/store/mysql"
	"agent-test-platform/migrations"

	"github.com/go-sql-driver/mysql"
)

// TestGenerationFollowsActiveModelOnMySQL84 proves, against real MySQL 8.4 and the Mock LLM over HTTP, that switching the
// active generation model through the model store changes which model the next Generation call is sent to.
func TestGenerationFollowsActiveModelOnMySQL84(t *testing.T) {
	dsn := os.Getenv("ATP_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set ATP_TEST_MYSQL_DSN to run the MySQL 8.4 integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	config, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ParseTime, config.Loc, config.DBName = true, time.UTC, ""
	admin := openDB(t, config)
	defer admin.Close()
	name := strings.NewReplacer(".", "_").Replace("atp_genswitch_" + time.Now().Format("150405.000000"))
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatal(err)
	}
	config.DBName = name
	database := openDB(t, config)
	defer database.Close()
	if err := migrations.Apply(ctx, database); err != nil {
		t.Fatal(err)
	}

	store, err := mysqlstore.NewModelStore(database)
	if err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{"gen-a", "gen-b"} {
		if err := store.Register(ctx, modelcfg.RoleGeneration, alias, "openai", "model-"+alias, "http://x"); err != nil {
			t.Fatal(err)
		}
	}

	mock, err := mockllm.NewServer("llm-secret")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &parsed)
		mu.Lock()
		seen = append(seen, parsed.Model)
		mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		mock.ServeHTTP(w, r)
	}))
	defer server.Close()

	var models []llm.ModelConfig
	for _, alias := range []string{"gen-a", "gen-b"} {
		adapter, err := llm.NewOpenAIAdapter(server.URL+"/v1", "llm-secret", &http.Client{Timeout: 10 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		models = append(models, llm.ModelConfig{Alias: alias, Model: llm.ModelRef{Provider: llm.ProviderOpenAI, Model: "model-" + alias},
			Capabilities: llm.ModelCapabilities{ToolCalling: true, NamedToolChoice: true, JSONSchemaOutput: true, Usage: true, ContextWindow: 32768, MaxOutputTokens: 4096, TokenizerID: "conservative-v1"},
			Adapter:      adapter, Timeout: 10 * time.Second})
	}
	gateway, err := llm.NewGateway(models)
	if err != nil {
		t.Fatal(err)
	}
	active := newActiveModelGateway(gateway, store, modelcfg.RoleGeneration, "gen-a", []string{"gen-a", "gen-b"})
	active.ttl = 0

	prompt := "Generate."
	request := llm.CompletionRequest{
		Messages:        []llm.Message{{Role: llm.RoleUser, Content: &prompt}},
		ResponseSchema:  json.RawMessage(`{"title":"Business Scenario Revision Content","type":"object"}`),
		MaxOutputTokens: 512,
	}
	call := func() {
		t.Helper()
		if _, err := active.Complete(ctx, "ignored", request); err != nil {
			t.Fatal(err)
		}
	}
	call()
	if err := store.Activate(ctx, modelcfg.RoleGeneration, "gen-b"); err != nil {
		t.Fatal(err)
	}
	call()
	if err := store.Activate(ctx, modelcfg.RoleGeneration, "gen-a"); err != nil {
		t.Fatal(err)
	}
	call()
	mu.Lock()
	defer mu.Unlock()
	want := []string{"model-gen-a", "model-gen-b", "model-gen-a"}
	if len(seen) != 3 || seen[0] != want[0] || seen[1] != want[1] || seen[2] != want[2] {
		t.Fatalf("the Mock LLM received models %v, want %v", seen, want)
	}
}

func openDB(t *testing.T, config *mysql.Config) *sql.DB {
	t.Helper()
	connector, err := mysql.NewConnector(config)
	if err != nil {
		t.Fatal(err)
	}
	return sql.OpenDB(connector)
}
