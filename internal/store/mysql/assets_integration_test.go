package mysqlstore

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"agent-test-platform/internal/catalog"
	"agent-test-platform/internal/generation"
	"agent-test-platform/migrations"

	"github.com/go-sql-driver/mysql"
)

// TestRevisionSearchAndKindSpecificJobsOnMySQL84 covers the catalog revision search (newest first, text query, lifecycle in the body)
// and the creation of a single-kind Generation Job with and without its required input revisions.
func TestRevisionSearchAndKindSpecificJobsOnMySQL84(t *testing.T) {
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
	config.DBName, config.ParseTime, config.Loc = "", true, time.UTC
	admin := openPublishMySQL(t, config)
	defer admin.Close()
	name := strings.NewReplacer(".", "_").Replace("atp_assets_" + time.Now().Format("150405.000000"))
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatal(err)
	}
	config.DBName = name
	setup := openPublishMySQL(t, config)
	defer setup.Close()
	if err := migrations.Apply(ctx, setup); err != nil {
		t.Fatal(err)
	}
	config.Params = map[string]string{"foreign_key_checks": "0"}
	database := openPublishMySQL(t, config)
	defer database.Close()

	const (
		scenarioA = "01HZZZZZZZZZZZZZZZZZZZZZA1"
		scenarioB = "01HZZZZZZZZZZZZZZZZZZZZZA2"
		revA      = "01HZZZZZZZZZZZZZZZZZZZZZR1"
		revB      = "01HZZZZZZZZZZZZZZZZZZZZZR2"
	)
	for _, statement := range []string{
		`INSERT INTO scenario (id) VALUES ('` + scenarioA + `'), ('` + scenarioB + `')`,
		`INSERT INTO scenario_revision (id, scenario_id, generation_job_id, revision_no, quality_level, lifecycle_status, content_json, content_sha256, created_at)
			VALUES ('` + revA + `', '` + scenarioA + `', 'G', 1, 'PASSED', 'ACTIVE', JSON_OBJECT('name', 'Cancel an order', 'domain', 'e-commerce'), REPEAT('a', 64), '2026-01-01 10:00:00'),
			       ('` + revB + `', '` + scenarioB + `', 'G', 1, 'PASSED', 'DEPRECATED', JSON_OBJECT('name', 'Refund a payment 100%_off', 'domain', 'billing'), REPEAT('b', 64), '2026-01-02 10:00:00')`,
	} {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}

	store, err := NewCatalogStore(database)
	if err != nil {
		t.Fatal(err)
	}
	service, _ := catalog.NewService(store)
	ids := func(query string) []string {
		t.Helper()
		items, _, err := service.SearchRevisions(ctx, catalog.Scenario, query, "", 10)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, item := range items {
			out = append(out, item["id"].(string))
		}
		return out
	}
	if got := ids(""); len(got) != 2 || got[0] != revB || got[1] != revA {
		t.Fatalf("newest first: %v", got)
	}
	if got := ids("cancel"); len(got) != 1 || got[0] != revA {
		t.Fatalf("content search: %v", got)
	}
	if got := ids("billing"); len(got) != 1 || got[0] != revB {
		t.Fatalf("domain search: %v", got)
	}
	if got := ids(revA[:10]); len(got) != 2 {
		t.Fatalf("id prefix search should match both: %v", got)
	}
	if got := ids("100%_off"); len(got) != 1 || got[0] != revB {
		t.Fatalf("LIKE wildcards must be escaped: %v", got)
	}
	if got := ids("%"); len(got) != 1 {
		t.Fatalf("a bare wildcard must match literally: %v", got)
	}
	page, next, err := service.SearchRevisions(ctx, catalog.Scenario, "", "", 1)
	if err != nil || len(page) != 1 || next == "" {
		t.Fatalf("paging: %v %q %v", page, next, err)
	}
	if page[0]["lifecycle_status"] != "DEPRECATED" {
		t.Fatalf("lifecycle must be in the body: %v", page[0])
	}
	if _, _, err := service.SearchRevisions(ctx, catalog.Scenario, strings.Repeat("x", 200), "", 10); err == nil {
		t.Fatal("an over-long query must be rejected")
	}

	generationStore, err := NewGenerationStore(database)
	if err != nil {
		t.Fatal(err)
	}
	request := func(extra string) json.RawMessage {
		return json.RawMessage(`{"description":"d"` + extra + `}`)
	}
	digest := strings.Repeat("c", 64)
	create := func(kind generation.JobKind, key string, scenario, dataset *string, count int, targets []string) error {
		_, err := generationStore.CreateGenerationJob(ctx, generation.CreateJobInput{
			IdempotencyKey: key, Kind: kind, InputScenarioRevisionID: scenario, InputDatasetRevisionID: dataset,
			Request: request(`,"k":"` + key + `"`), RequestSHA256: digest, GeneratorConfigSHA256: digest, DescriptionTokens: 20,
			RequestedTestCaseCount: count, GenerateDataset: true, ValidationDBIDs: targets,
		})
		return err
	}
	scenarioRev := revA
	if err := create(generation.KindScenario, "scenario-job-1", nil, nil, 0, nil); err != nil {
		t.Fatalf("a scenario job needs no target: %v", err)
	}
	if err := create(generation.KindDataset, "dataset-job-1", &scenarioRev, nil, 0, nil); err == nil {
		t.Fatal("a dataset job without a Target must be rejected")
	}
	if err := create(generation.KindDataset, "dataset-job-2", nil, nil, 0, []string{"0b6d6f3e-2c4a-4f8e-9a1b-7c5d3e2f1a90"}); err == nil {
		t.Fatal("a dataset job without a scenario revision must be rejected")
	}
	if err := create(generation.KindTestCases, "cases-job-1", &scenarioRev, nil, 3, nil); err == nil {
		t.Fatal("test cases without a dataset revision must be rejected")
	}
	datasetRev := "01HZZZZZZZZZZZZZZZZZZZZZD1"
	if err := create(generation.KindTestCases, "cases-job-2", &scenarioRev, &datasetRev, 3, nil); err != nil {
		t.Fatalf("test cases with both inputs and no target: %v", err)
	}
	var jobs int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM generation_job WHERE job_kind IN ('SCENARIO', 'TEST_CASES')`).Scan(&jobs); err != nil || jobs != 2 {
		t.Fatalf("stored single-kind jobs: %d %v", jobs, err)
	}
}
