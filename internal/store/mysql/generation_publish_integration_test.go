package mysqlstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"agent-test-platform/contracts"
	"agent-test-platform/internal/generation"
	"agent-test-platform/migrations"

	"github.com/go-sql-driver/mysql"
	"github.com/oklog/ulid/v2"
)

// TestPublishGenerationFullPipeline exercises the Atomic Publish Transaction against a real
// MySQL 8.4 instance for all three job_kind values, chained exactly like a Generation Pipeline
// (docs/architecture.md 6.3): SCENARIO publishes first, DATASET references its published
// Scenario Revision, TEST_CASES references both. It also verifies that retrying PUBLISHING for
// a Job already stuck there (simulating a crash right before commit) does not create duplicate
// logical rows, per PublishGeneration's idempotency contract.
func TestPublishGenerationFullPipeline(t *testing.T) {
	dsn := os.Getenv("ATP_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set ATP_TEST_MYSQL_DSN to run the MySQL 8.4 publish integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	adminConfig, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse ATP_TEST_MYSQL_DSN: %v", err)
	}
	adminConfig.DBName = ""
	adminDB := openPublishMySQL(t, adminConfig)
	t.Cleanup(func() { _ = adminDB.Close() })
	if err := adminDB.PingContext(ctx); err != nil {
		t.Fatalf("ping MySQL: %v", err)
	}

	databaseName := "atp_publish_" + randomPublishHex(t, 8)
	quoted := "`" + databaseName + "`"
	if _, err := adminDB.ExecContext(ctx, "CREATE DATABASE "+quoted+" CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatalf("create test database: %v", err)
	}

	databaseConfig := *adminConfig
	databaseConfig.DBName = databaseName
	databaseConfig.ParseTime = true
	databaseConfig.Loc = time.UTC
	database := openPublishMySQL(t, &databaseConfig)
	defer database.Close()
	if _, err := database.ExecContext(ctx, "SET time_zone = '+00:00'"); err != nil {
		t.Fatalf("set UTC session timezone: %v", err)
	}
	if err := migrations.Apply(ctx, database); err != nil {
		t.Fatal(err)
	}

	store, err := NewGenerationStore(database)
	if err != nil {
		t.Fatal(err)
	}
	// Two Targets, so the Dataset binding order and both binding rows are exercised.
	seedValidationDB(t, ctx, database, seededValidationDBID, "target-a", "target-a.internal")
	secondValidationDBID := "5e8c1b2a-7f3d-4c6e-8a9b-0d1e2f3a4b5c"
	seedValidationDB(t, ctx, database, secondValidationDBID, "target-b", "target-b.internal")

	// Phase 1: SCENARIO, with one GENERATED_ASSUMPTION business rule so quality_level is
	// expected to be SYNTHETIC.
	scenarioJobID := seedPublishJob(t, ctx, database, seedPublishRequest(t, ctx, database, true), "SCENARIO", 0, nil, nil)
	seedPublishDraft(t, ctx, database, scenarioJobID, "SCENARIO", "scenario", []byte(`{
		"business_rules": [
			{"id": "r1", "source": "SOURCE_REQUIREMENT"},
			{"id": "r2", "source": "GENERATED_ASSUMPTION"}
		]
	}`))
	seedPublishDraft(t, ctx, database, scenarioJobID, "CRITIC_REVIEW", "scenario-round-0", criticReviewFixture(t))
	scenarioLease := seedPublishWork(t, ctx, database, scenarioJobID, "PUBLISHING:repair-0")
	if _, err := store.PublishGeneration(ctx, scenarioJobID, scenarioLease); err != nil {
		t.Fatalf("publish scenario: %v", err)
	}
	assertPublishJobCompleted(t, ctx, database, scenarioJobID)

	var scenarioRevisionID, qualityLevel string
	if err := database.QueryRowContext(ctx,
		"SELECT id, quality_level FROM scenario_revision WHERE generation_job_id = ?", scenarioJobID,
	).Scan(&scenarioRevisionID, &qualityLevel); err != nil {
		t.Fatalf("load published scenario revision: %v", err)
	}
	if qualityLevel != "SYNTHETIC" {
		t.Fatalf("quality_level = %q, want SYNTHETIC", qualityLevel)
	}

	// The Job's provenance records the producer and the accepted Critic review, so the manifest
	// can name both models (docs/architecture.md section 6.3).
	var producerModel, criticModel, criticPrompt string
	if err := database.QueryRowContext(ctx, `SELECT
		JSON_UNQUOTE(JSON_EXTRACT(provenance_json, '$.producer.model')),
		JSON_UNQUOTE(JSON_EXTRACT(provenance_json, '$.critic.model')),
		JSON_UNQUOTE(JSON_EXTRACT(provenance_json, '$.critic.prompt_version'))
		FROM generation_job WHERE id = ?`, scenarioJobID,
	).Scan(&producerModel, &criticModel, &criticPrompt); err != nil {
		t.Fatalf("load scenario provenance: %v", err)
	}
	if producerModel != "gpt-4.1" || criticModel != "gpt-4.1-mini" || criticPrompt != "semantic-critic/v1" {
		t.Fatalf("provenance producer=%q critic=%q critic prompt=%q", producerModel, criticModel, criticPrompt)
	}

	// Retrying PUBLISHING for the same Job (simulating a crash right before commit) must not
	// create a second Scenario or Scenario Revision.
	resetPublishJobToPublishing(t, ctx, database, scenarioJobID)
	retryLease := seedPublishWork(t, ctx, database, scenarioJobID, "PUBLISHING:repair-0-retry")
	if _, err := store.PublishGeneration(ctx, scenarioJobID, retryLease); err != nil {
		t.Fatalf("retry publish scenario: %v", err)
	}
	assertPublishRowCount(t, ctx, database, "scenario", 1)
	assertPublishRowCount(t, ctx, database, "scenario_revision", 1)
	var retryRevisionID string
	if err := database.QueryRowContext(ctx,
		"SELECT id FROM scenario_revision WHERE generation_job_id = ?", scenarioJobID,
	).Scan(&retryRevisionID); err != nil {
		t.Fatalf("load scenario revision after retry: %v", err)
	}
	if retryRevisionID != scenarioRevisionID {
		t.Fatalf("scenario_revision id changed on retry: %s != %s", retryRevisionID, scenarioRevisionID)
	}

	// Phase 2: DATASET, referencing the published Scenario Revision.
	datasetJobID := seedPublishJob(t, ctx, database, seedPublishRequest(t, ctx, database, true), "DATASET", 0, &scenarioRevisionID, nil)
	seedPublishDraft(t, ctx, database, datasetJobID, "DATASET", "dataset", []byte(`{
		"schema_sql": "CREATE TABLE orders (id VARCHAR(32) PRIMARY KEY)",
		"seed_sql": "INSERT INTO orders (id) VALUES ('O1')",
		"constraints": {"version": "1.0", "checks": []},
		"metadata": {"schema_version": "1.0", "tables": [{"name": "orders", "columns": [{"name": "id"}]}]}
	}`))
	executionRevisionID := newPublishULID(t)
	seedDatasetExecution(t, ctx, database, datasetJobID, executionRevisionID, []string{seededValidationDBID, secondValidationDBID})
	datasetLease := seedPublishWork(t, ctx, database, datasetJobID, "PUBLISHING:repair-0")
	if _, err := store.PublishGeneration(ctx, datasetJobID, datasetLease); err != nil {
		t.Fatalf("publish dataset: %v", err)
	}
	assertPublishJobCompleted(t, ctx, database, datasetJobID)

	var datasetRevisionID, datasetScenarioRevisionID, executionDatabase string
	if err := database.QueryRowContext(ctx,
		"SELECT id, scenario_revision_id, execution_database FROM dataset_revision WHERE generation_job_id = ?", datasetJobID,
	).Scan(&datasetRevisionID, &datasetScenarioRevisionID, &executionDatabase); err != nil {
		t.Fatalf("load published dataset revision: %v", err)
	}
	if datasetScenarioRevisionID != scenarioRevisionID {
		t.Fatalf("dataset_revision.scenario_revision_id = %s, want %s", datasetScenarioRevisionID, scenarioRevisionID)
	}
	if datasetRevisionID != executionRevisionID {
		t.Fatalf("dataset_revision.id = %s, want the execution record's %s", datasetRevisionID, executionRevisionID)
	}
	if executionDatabase != "dataset_"+strings.ToLower(executionRevisionID) {
		t.Fatalf("dataset_revision.execution_database = %q", executionDatabase)
	}
	assertDatasetRevisionTargets(t, ctx, database, datasetRevisionID, []string{seededValidationDBID, secondValidationDBID})

	// Phase 3: TEST_CASES, referencing both published Revisions.
	testCasesJobID := seedPublishJob(t, ctx, database, seedPublishRequest(t, ctx, database, true), "TEST_CASES", 2, &scenarioRevisionID, &datasetRevisionID)
	seedPublishDraft(t, ctx, database, testCasesJobID, "TEST_CASE_MANIFEST", "round-0", []byte(`{
		"accepted": ["case-1", "case-2"], "rejected": []
	}`))
	for _, clientCaseID := range []string{"case-1", "case-2"} {
		content, err := json.Marshal(map[string]any{
			"schema_version": "1.0", "job_id": testCasesJobID, "client_case_id": clientCaseID,
			"scenario_revision_id": scenarioRevisionID, "dataset_revision_id": datasetRevisionID,
			"agent_mode": "TASK", "tool_evaluation_mode": "SESSION_LLM_ONLY",
			"prompt": "Please cancel order O1.",
		})
		if err != nil {
			t.Fatal(err)
		}
		seedPublishDraft(t, ctx, database, testCasesJobID, "TEST_CASE", clientCaseID, content)
	}
	testCasesLease := seedPublishWork(t, ctx, database, testCasesJobID, "PUBLISHING:repair-0")
	if _, err := store.PublishGeneration(ctx, testCasesJobID, testCasesLease); err != nil {
		t.Fatalf("publish test cases: %v", err)
	}
	assertPublishJobCompleted(t, ctx, database, testCasesJobID)
	assertPublishRowCount(t, ctx, database, "test_case", 2)
	assertPublishRowCount(t, ctx, database, "test_case_revision", 2)
	// Standalone Jobs (no pipeline_id) skip the Generation Package step entirely
	// (docs/technical/01-storage-and-workflows.md section 6, step 5).
	assertPublishRowCount(t, ctx, database, "generation_package", 0)

	rows, err := database.QueryContext(ctx,
		"SELECT lifecycle_status, scenario_revision_id, dataset_revision_id, content_json FROM test_case_revision WHERE generation_job_id = ?",
		testCasesJobID,
	)
	if err != nil {
		t.Fatalf("load published test case revisions: %v", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		var lifecycleStatus, revScenarioID, revDatasetID string
		var contentJSON []byte
		if err := rows.Scan(&lifecycleStatus, &revScenarioID, &revDatasetID, &contentJSON); err != nil {
			t.Fatalf("scan test case revision: %v", err)
		}
		if lifecycleStatus != "ACTIVE" {
			t.Fatalf("lifecycle_status = %q, want ACTIVE (auto_activate was true)", lifecycleStatus)
		}
		if revScenarioID != scenarioRevisionID || revDatasetID != datasetRevisionID {
			t.Fatalf("test_case_revision references = (%s, %s)", revScenarioID, revDatasetID)
		}
		if strings.Contains(string(contentJSON), "job_id") || strings.Contains(string(contentJSON), "client_case_id") {
			t.Fatalf("published content still carries transport fields: %s", contentJSON)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("test_case_revision count = %d, want 2", count)
	}
}

// TestGenerationPipelineAutoChain exercises the "one description in, full package out"
// convenience flow (docs/architecture.md 6.1) against a real MySQL 8.4 instance:
// CreateGenerationJob with no explicit job_kind creates a Generation Pipeline and its first
// (SCENARIO) Job; publishing that Job auto-creates the DATASET Job referencing its published
// Scenario Revision; publishing that Job auto-creates the TEST_CASES Job (carrying the
// Pipeline's requested_test_case_count) referencing both published Revisions; publishing that
// Job marks the Pipeline COMPLETED. GetGenerationJob against the original (SCENARIO) Job ID
// transparently resolves to whichever stage is furthest along at each point, so a client that
// only ever polls the ID it originally received needs no pipeline-aware logic of its own.
func TestGenerationPipelineAutoChain(t *testing.T) {
	dsn := os.Getenv("ATP_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set ATP_TEST_MYSQL_DSN to run the MySQL 8.4 publish integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	adminConfig, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse ATP_TEST_MYSQL_DSN: %v", err)
	}
	adminConfig.DBName = ""
	adminDB := openPublishMySQL(t, adminConfig)
	t.Cleanup(func() { _ = adminDB.Close() })
	if err := adminDB.PingContext(ctx); err != nil {
		t.Fatalf("ping MySQL: %v", err)
	}

	databaseName := "atp_pipeline_" + randomPublishHex(t, 8)
	quoted := "`" + databaseName + "`"
	if _, err := adminDB.ExecContext(ctx, "CREATE DATABASE "+quoted+" CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatalf("create test database: %v", err)
	}

	databaseConfig := *adminConfig
	databaseConfig.DBName = databaseName
	databaseConfig.ParseTime = true
	databaseConfig.Loc = time.UTC
	database := openPublishMySQL(t, &databaseConfig)
	defer database.Close()
	if _, err := database.ExecContext(ctx, "SET time_zone = '+00:00'"); err != nil {
		t.Fatalf("set UTC session timezone: %v", err)
	}
	if err := migrations.Apply(ctx, database); err != nil {
		t.Fatal(err)
	}

	store, err := NewGenerationStore(database)
	if err != nil {
		t.Fatal(err)
	}

	seedValidationDB(t, ctx, database, seededValidationDBID, "target-a", "target-a.internal")
	requestDocument, err := json.Marshal(map[string]any{
		"description": "Generate cancellation tests.", "auto_activate": true,
		"validation_db_ids": []string{seededValidationDBID},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, requestDigest, err := generation.CanonicalJSON(requestDocument)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateGenerationJob(ctx, generation.CreateJobInput{
		IdempotencyKey: "pipeline-autochain-" + randomPublishHex(t, 8),
		Request:        requestDocument, RequestSHA256: requestDigest, DescriptionTokens: 8,
		RequestedTestCaseCount: 2, GeneratorConfigSHA256: strings.Repeat("a", 64), GenerateDataset: true,
		ValidationDBIDs: []string{seededValidationDBID},
	})
	if err != nil {
		t.Fatalf("create generation pipeline: %v", err)
	}
	if created.Kind != generation.KindScenario {
		t.Fatalf("created.Kind = %q, want SCENARIO", created.Kind)
	}
	if created.PipelineID == nil {
		t.Fatal("created.PipelineID is nil, want set")
	}
	pipelineID := *created.PipelineID
	scenarioJobID := created.ID

	// Before the SCENARIO job publishes, GetGenerationJob must resolve to itself: the Pipeline
	// has no further stage yet.
	if resolved, err := store.GetGenerationJob(ctx, scenarioJobID); err != nil {
		t.Fatal(err)
	} else if resolved.ID != scenarioJobID {
		t.Fatalf("resolved.ID = %s, want %s (SCENARIO, no further stage yet)", resolved.ID, scenarioJobID)
	}

	// Drive the SCENARIO job straight to PUBLISHING, skipping the generation/validation stages
	// that other tests already cover, to focus on the Pipeline auto-chain in PublishGeneration.
	driveJobToPublishing(t, ctx, database, scenarioJobID)
	seedPublishDraft(t, ctx, database, scenarioJobID, "SCENARIO", "scenario", []byte(`{
		"business_rules": [{"id": "r1", "source": "SOURCE_REQUIREMENT"}]
	}`))
	scenarioLease := seedPublishWork(t, ctx, database, scenarioJobID, "PUBLISHING:repair-0")
	if _, err := store.PublishGeneration(ctx, scenarioJobID, scenarioLease); err != nil {
		t.Fatalf("publish scenario: %v", err)
	}
	assertPublishJobCompleted(t, ctx, database, scenarioJobID)

	var scenarioRevisionID string
	if err := database.QueryRowContext(ctx,
		"SELECT id FROM scenario_revision WHERE generation_job_id = ?", scenarioJobID,
	).Scan(&scenarioRevisionID); err != nil {
		t.Fatalf("load published scenario revision: %v", err)
	}

	var datasetJobID string
	if err := database.QueryRowContext(ctx,
		"SELECT dataset_job_id FROM generation_pipeline WHERE id = ?", pipelineID,
	).Scan(&datasetJobID); err != nil {
		t.Fatalf("load pipeline dataset_job_id: %v", err)
	}
	if datasetJobID == "" {
		t.Fatal("generation_pipeline.dataset_job_id was not set after SCENARIO published")
	}
	assertGenerationJobKindAndInputs(t, ctx, database, datasetJobID, "DATASET", &scenarioRevisionID, nil)

	// GetGenerationJob against the ORIGINAL scenario Job ID must now resolve to the DATASET Job.
	if resolved, err := store.GetGenerationJob(ctx, scenarioJobID); err != nil {
		t.Fatal(err)
	} else if resolved.ID != datasetJobID || resolved.Kind != generation.KindDataset {
		t.Fatalf("resolved = %#v, want DATASET job %s", resolved, datasetJobID)
	}

	// Drive the DATASET job to PUBLISHING and publish it.
	driveJobToPublishing(t, ctx, database, datasetJobID)
	seedPublishDraft(t, ctx, database, datasetJobID, "DATASET", "dataset", []byte(`{
		"schema_sql": "CREATE TABLE orders (id VARCHAR(32) PRIMARY KEY)",
		"seed_sql": "INSERT INTO orders (id) VALUES ('O1')",
		"constraints": {"version": "1.0", "checks": []},
		"metadata": {"schema_version": "1.0", "tables": [{"name": "orders", "columns": [{"name": "id"}]}]}
	}`))
	executionRevisionID := newPublishULID(t)
	seedDatasetExecution(t, ctx, database, datasetJobID, executionRevisionID, []string{seededValidationDBID})
	datasetLease := seedPublishWork(t, ctx, database, datasetJobID, "PUBLISHING:repair-0")
	if _, err := store.PublishGeneration(ctx, datasetJobID, datasetLease); err != nil {
		t.Fatalf("publish dataset: %v", err)
	}
	assertPublishJobCompleted(t, ctx, database, datasetJobID)
	assertDatasetRevisionTargets(t, ctx, database, executionRevisionID, []string{seededValidationDBID})

	var datasetRevisionID string
	if err := database.QueryRowContext(ctx,
		"SELECT id FROM dataset_revision WHERE generation_job_id = ?", datasetJobID,
	).Scan(&datasetRevisionID); err != nil {
		t.Fatalf("load published dataset revision: %v", err)
	}

	var testCasesJobID string
	if err := database.QueryRowContext(ctx,
		"SELECT test_cases_job_id FROM generation_pipeline WHERE id = ?", pipelineID,
	).Scan(&testCasesJobID); err != nil {
		t.Fatalf("load pipeline test_cases_job_id: %v", err)
	}
	if testCasesJobID == "" {
		t.Fatal("generation_pipeline.test_cases_job_id was not set after DATASET published")
	}
	assertGenerationJobKindAndInputs(t, ctx, database, testCasesJobID, "TEST_CASES", &scenarioRevisionID, &datasetRevisionID)

	var testCasesRequestedCount int
	if err := database.QueryRowContext(ctx,
		"SELECT requested_test_case_count FROM generation_job WHERE id = ?", testCasesJobID,
	).Scan(&testCasesRequestedCount); err != nil {
		t.Fatalf("load TEST_CASES requested count: %v", err)
	}
	if testCasesRequestedCount != 2 {
		t.Fatalf("TEST_CASES requested_test_case_count = %d, want 2 (carried from the Pipeline)", testCasesRequestedCount)
	}

	// GetGenerationJob against the ORIGINAL scenario Job ID must now resolve to the TEST_CASES Job.
	if resolved, err := store.GetGenerationJob(ctx, scenarioJobID); err != nil {
		t.Fatal(err)
	} else if resolved.ID != testCasesJobID || resolved.Kind != generation.KindTestCases {
		t.Fatalf("resolved = %#v, want TEST_CASES job %s", resolved, testCasesJobID)
	}

	// Drive the TEST_CASES job to PUBLISHING and publish it; the Pipeline must then complete.
	driveJobToPublishing(t, ctx, database, testCasesJobID)
	seedPublishDraft(t, ctx, database, testCasesJobID, "TEST_CASE_MANIFEST", "round-0", []byte(`{
		"accepted": ["case-1", "case-2"], "rejected": []
	}`))
	for _, clientCaseID := range []string{"case-1", "case-2"} {
		content, err := json.Marshal(map[string]any{
			"schema_version": "1.0", "job_id": testCasesJobID, "client_case_id": clientCaseID,
			"scenario_revision_id": scenarioRevisionID, "dataset_revision_id": datasetRevisionID,
			"agent_mode": "TASK", "tool_evaluation_mode": "SESSION_LLM_ONLY",
			"prompt": "Please cancel order O1.",
		})
		if err != nil {
			t.Fatal(err)
		}
		seedPublishDraft(t, ctx, database, testCasesJobID, "TEST_CASE", clientCaseID, content)
	}
	testCasesLease := seedPublishWork(t, ctx, database, testCasesJobID, "PUBLISHING:repair-0")
	if _, err := store.PublishGeneration(ctx, testCasesJobID, testCasesLease); err != nil {
		t.Fatalf("publish test cases: %v", err)
	}
	assertPublishJobCompleted(t, ctx, database, testCasesJobID)

	var pipelineStatus string
	if err := database.QueryRowContext(ctx,
		"SELECT status FROM generation_pipeline WHERE id = ?", pipelineID,
	).Scan(&pipelineStatus); err != nil {
		t.Fatalf("load pipeline status: %v", err)
	}
	if pipelineStatus != "COMPLETED" {
		t.Fatalf("generation_pipeline.status = %q, want COMPLETED", pipelineStatus)
	}

	resolved, err := store.GetGenerationJob(ctx, scenarioJobID)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ID != testCasesJobID || resolved.Status != generation.StatusCompleted {
		t.Fatalf("resolved = %#v, want COMPLETED TEST_CASES job %s", resolved, testCasesJobID)
	}
	assertPipelinePackage(t, ctx, database, store, pipelineID)
}

// assertPipelinePackage checks what a completed Pipeline publishes: one Generation Package links
// the three Jobs' outputs, the TEST_CASES Job points at it, every Job resolves to the same package
// through the read path, and its manifest is ordered and digest-consistent
// (docs/technical/01-storage-and-workflows.md section 6, step 5 and 7).
func assertPipelinePackage(t *testing.T, ctx context.Context, database *sql.DB, store *GenerationStore, pipelineID string) {
	t.Helper()
	var scenarioJobID, datasetJobID, testCasesJobID string
	if err := database.QueryRowContext(ctx,
		"SELECT scenario_job_id, dataset_job_id, test_cases_job_id FROM generation_pipeline WHERE id = ?", pipelineID,
	).Scan(&scenarioJobID, &datasetJobID, &testCasesJobID); err != nil {
		t.Fatalf("load pipeline job IDs: %v", err)
	}

	var packageID, scenarioRevisionID, datasetRevisionID, manifestDigest string
	if err := database.QueryRowContext(ctx, `SELECT id, scenario_revision_id, dataset_revision_id, manifest_sha256
		FROM generation_package WHERE pipeline_id = ?`, pipelineID,
	).Scan(&packageID, &scenarioRevisionID, &datasetRevisionID, &manifestDigest); err != nil {
		t.Fatalf("load generation package: %v", err)
	}
	var linked sql.NullString
	if err := database.QueryRowContext(ctx, "SELECT published_package_id FROM generation_job WHERE id = ?", testCasesJobID).Scan(&linked); err != nil {
		t.Fatalf("load published_package_id: %v", err)
	}
	if linked.String != packageID {
		t.Fatalf("TEST_CASES job published_package_id = %q, want %s", linked.String, packageID)
	}
	var missingProvenance int
	if err := database.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM generation_job WHERE pipeline_id = ? AND provenance_json IS NULL", pipelineID,
	).Scan(&missingProvenance); err != nil {
		t.Fatalf("count jobs without provenance: %v", err)
	}
	if missingProvenance != 0 {
		t.Fatalf("%d pipeline jobs have no provenance", missingProvenance)
	}
	var lifecycles int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(DISTINCT lifecycle_status) FROM (
		SELECT lifecycle_status FROM scenario_revision UNION ALL
		SELECT lifecycle_status FROM dataset_revision UNION ALL
		SELECT lifecycle_status FROM test_case_revision) AS revisions`,
	).Scan(&lifecycles); err != nil {
		t.Fatalf("count revision lifecycles: %v", err)
	}
	if lifecycles != 1 {
		t.Fatalf("published revisions carry %d distinct lifecycle statuses, want one (auto_activate applies to all)", lifecycles)
	}

	var packageTestCases, revisionTestCases int
	if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM generation_package_test_case WHERE generation_package_id = ?", packageID).Scan(&packageTestCases); err != nil {
		t.Fatalf("count package test cases: %v", err)
	}
	if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM test_case_revision").Scan(&revisionTestCases); err != nil {
		t.Fatalf("count test case revisions: %v", err)
	}
	if packageTestCases == 0 || packageTestCases != revisionTestCases {
		t.Fatalf("package holds %d test cases, published %d", packageTestCases, revisionTestCases)
	}

	for _, jobID := range []string{scenarioJobID, datasetJobID, testCasesJobID} {
		pkg, err := store.GetGenerationPackage(ctx, jobID)
		if err != nil {
			t.Fatalf("get generation package through job %s: %v", jobID, err)
		}
		if pkg.JobID != testCasesJobID || pkg.ScenarioRevisionID != scenarioRevisionID || pkg.DatasetRevisionID != datasetRevisionID ||
			len(pkg.TestCaseRevisionIDs) != packageTestCases || pkg.ManifestSHA256 != manifestDigest {
			t.Fatalf("package through job %s = %#v", jobID, pkg)
		}
	}

	pkg, err := store.GetGenerationPackage(ctx, scenarioJobID)
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		PipelineID string `json:"pipeline_id"`
		Stages     []struct {
			JobKind string `json:"job_kind"`
			JobID   string `json:"job_id"`
		} `json:"stages"`
		Artifacts struct {
			Scenario struct {
				RevisionID string `json:"revision_id"`
			} `json:"scenario"`
			TestCases []struct {
				Ordinal    int    `json:"ordinal"`
				RevisionID string `json:"revision_id"`
			} `json:"test_cases"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(pkg.Manifest, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	wantStages := []struct{ kind, jobID string }{{"SCENARIO", scenarioJobID}, {"DATASET", datasetJobID}, {"TEST_CASES", testCasesJobID}}
	if manifest.PipelineID != pipelineID || len(manifest.Stages) != 3 || manifest.Artifacts.Scenario.RevisionID != scenarioRevisionID {
		t.Fatalf("manifest header = %#v", manifest)
	}
	for index, want := range wantStages {
		if manifest.Stages[index].JobKind != want.kind || manifest.Stages[index].JobID != want.jobID {
			t.Fatalf("manifest stage %d = %#v, want %s %s", index, manifest.Stages[index], want.kind, want.jobID)
		}
	}
	for index, testCase := range manifest.Artifacts.TestCases {
		if testCase.Ordinal != index+1 || testCase.RevisionID != pkg.TestCaseRevisionIDs[index] {
			t.Fatalf("manifest test case %d = %#v, package order %v", index, testCase, pkg.TestCaseRevisionIDs)
		}
	}
}

// driveJobToPublishing fast-forwards a Job straight to PUBLISHING, also setting
// accepted_test_case_count = requested_test_case_count so the TEST_CASES-kind invariant
// JobState.Validate enforces for PUBLISHING (requiresExactTestCaseCount) already holds, exactly
// as it would by the time a real Job reaches PUBLISHING through VALIDATING_TEST_CASES.
func driveJobToPublishing(t *testing.T, ctx context.Context, database *sql.DB, jobID string) {
	t.Helper()
	if _, err := database.ExecContext(ctx, `UPDATE generation_job
		SET status = 'PUBLISHING', accepted_test_case_count = requested_test_case_count,
		    started_at = COALESCE(started_at, UTC_TIMESTAMP(6))
		WHERE id = ?`, jobID,
	); err != nil {
		t.Fatalf("drive job %s to PUBLISHING: %v", jobID, err)
	}
}

// assertGenerationJobKindAndInputs verifies a Pipeline auto-chain created the expected next
// stage Job: still PENDING (not yet picked up by a worker), with the given job_kind and input
// revision references.
func assertGenerationJobKindAndInputs(
	t *testing.T, ctx context.Context, database *sql.DB, jobID, wantKind string, wantScenarioRevisionID, wantDatasetRevisionID *string,
) {
	t.Helper()
	var kind, status string
	var scenarioRevisionID, datasetRevisionID sql.NullString
	if err := database.QueryRowContext(ctx,
		"SELECT job_kind, status, input_scenario_revision_id, input_dataset_revision_id FROM generation_job WHERE id = ?", jobID,
	).Scan(&kind, &status, &scenarioRevisionID, &datasetRevisionID); err != nil {
		t.Fatalf("load chained generation job %s: %v", jobID, err)
	}
	if kind != wantKind {
		t.Fatalf("job %s kind = %q, want %q", jobID, kind, wantKind)
	}
	if status != "PENDING" {
		t.Fatalf("job %s status = %q, want PENDING", jobID, status)
	}
	if wantScenarioRevisionID != nil && (!scenarioRevisionID.Valid || scenarioRevisionID.String != *wantScenarioRevisionID) {
		t.Fatalf("job %s input_scenario_revision_id = %#v, want %s", jobID, scenarioRevisionID, *wantScenarioRevisionID)
	}
	if wantDatasetRevisionID != nil && (!datasetRevisionID.Valid || datasetRevisionID.String != *wantDatasetRevisionID) {
		t.Fatalf("job %s input_dataset_revision_id = %#v, want %s", jobID, datasetRevisionID, *wantDatasetRevisionID)
	}
}

func seedPublishRequest(t *testing.T, ctx context.Context, database *sql.DB, autoActivate bool) string {
	t.Helper()
	id := newPublishULID(t)
	document, err := json.Marshal(map[string]any{
		"description": "Generate cancellation tests.", "auto_activate": autoActivate,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, digest, err := generation.CanonicalJSON(document)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO generation_request (
		id, idempotency_key, request_json, request_sha256, description_tokens, generate_dataset
	) VALUES (?, ?, ?, ?, ?, TRUE)`, id, id, document, digest, 8); err != nil {
		t.Fatalf("seed generation_request: %v", err)
	}
	return id
}

func seedPublishJob(t *testing.T, ctx context.Context, database *sql.DB, requestID, jobKind string, requestedCount int, inputScenarioRevisionID, inputDatasetRevisionID *string) string {
	t.Helper()
	id := newPublishULID(t)
	// A real Job only ever reaches PUBLISHING after VALIDATING_TEST_CASES already set
	// accepted_test_case_count == requested_test_case_count (workflow.go Transition); seed the
	// same invariant directly since this test seeds PUBLISHING state without that transition.
	if _, err := database.ExecContext(ctx, `INSERT INTO generation_job (
		id, request_id, job_kind, status, requested_test_case_count, accepted_test_case_count,
		generator_config_sha256, input_scenario_revision_id, input_dataset_revision_id
	) VALUES (?, ?, ?, 'PUBLISHING', ?, ?, ?, ?, ?)`,
		id, requestID, jobKind, requestedCount, requestedCount, strings.Repeat("a", 64), inputScenarioRevisionID, inputDatasetRevisionID,
	); err != nil {
		t.Fatalf("seed generation_job: %v", err)
	}
	return id
}

func resetPublishJobToPublishing(t *testing.T, ctx context.Context, database *sql.DB, jobID string) {
	t.Helper()
	if _, err := database.ExecContext(ctx, "UPDATE generation_job SET status = 'PUBLISHING', finished_at = NULL WHERE id = ?", jobID); err != nil {
		t.Fatalf("reset job to PUBLISHING: %v", err)
	}
}

func seedPublishDraft(t *testing.T, ctx context.Context, database *sql.DB, jobID, kind, key string, content []byte) {
	t.Helper()
	if kind == "TEST_CASE_MANIFEST" {
		content = withPublishGenerator(t, content)
	}
	canonical, digest, err := generation.CanonicalJSON(content)
	if err != nil {
		t.Fatalf("canonicalize draft %s/%s: %v", kind, key, err)
	}
	validation := defaultPublishValidation(kind)
	if _, err := database.ExecContext(ctx, `INSERT INTO generation_artifact_draft (
		id, generation_job_id, artifact_kind, artifact_key, content_json, content_sha256, validation_json, expires_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, TIMESTAMPADD(HOUR, 24, UTC_TIMESTAMP(6)))`,
		newPublishULID(t), jobID, kind, key, canonical, digest, validation,
	); err != nil {
		t.Fatalf("seed draft %s/%s: %v", kind, key, err)
	}
}

// publishTestDigest returns a well-formed SHA-256 hex string for fixture records.
// seedDatasetExecution stores the DATASET_EXECUTION record the Dataset stage writes once the Dataset loaded on
// every bound Target. Each Target holds the execution schema dataset_<lowercase revision ID>.
func seedDatasetExecution(t *testing.T, ctx context.Context, database *sql.DB, jobID, revisionID string, targetIDs []string) {
	t.Helper()
	databaseName := "dataset_" + strings.ToLower(revisionID)
	targets := make([]map[string]string, 0, len(targetIDs))
	for _, targetID := range targetIDs {
		targets = append(targets, map[string]string{"validation_db_id": targetID, "database": databaseName})
	}
	content, err := json.Marshal(map[string]any{"dataset_revision_id": revisionID, "targets": targets})
	if err != nil {
		t.Fatal(err)
	}
	seedPublishDraft(t, ctx, database, jobID, "DATASET_EXECUTION", "dataset", content)
}

// assertDatasetRevisionTargets checks the Targets a Dataset Revision is bound to, in ordinal order.
func assertDatasetRevisionTargets(t *testing.T, ctx context.Context, database *sql.DB, revisionID string, want []string) {
	t.Helper()
	rows, err := database.QueryContext(ctx,
		"SELECT validation_db_id FROM dataset_revision_validation_db WHERE dataset_revision_id = ? ORDER BY ordinal", revisionID,
	)
	if err != nil {
		t.Fatalf("load Dataset Revision Targets: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var targetID string
		if err := rows.Scan(&targetID); err != nil {
			t.Fatal(err)
		}
		got = append(got, targetID)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("dataset_revision_validation_db for %s = %v, want %v", revisionID, got, want)
	}
}

func publishTestDigest(seed string) string {
	sum := strings.Repeat(seed, 64)
	return sum[:64]
}

// criticReviewFixture is the accepted Critic result the Critic artifact stores (the shared
// contract example, so the fixture always matches critic-result.schema.json).
func criticReviewFixture(t *testing.T) []byte {
	t.Helper()
	content, err := contracts.Files.ReadFile("examples/critic-result.json")
	if err != nil {
		t.Fatalf("read Critic result example: %v", err)
	}
	return content
}

// defaultPublishValidation is the producer record a model-generated Scenario or Dataset draft
// carries (see the validation records written by internal/generation/pipeline). Other kinds
// carry no validation record, so the value is nil and stored as SQL NULL.
func defaultPublishValidation(kind string) []byte {
	switch kind {
	case "CRITIC_REVIEW":
		return []byte(`{"source":"LLM","provider":"openai","model":"gpt-4.1-mini","prompt_version":"semantic-critic/v1",` +
			`"temperature":0,"max_output_tokens":2048,"seed":null,"provider_request_id":"req_critic",` +
			`"raw_response_sha256":"` + publishTestDigest("6") + `","input_tokens":900,"output_tokens":120,` +
			`"result_sha256":"` + publishTestDigest("7") + `"}`)
	case "SCENARIO":
		return []byte(`{"schema":"schemas/scenario.schema.json","prompt_version":"scenario-generator/v1","source":"LLM",` +
			`"temperature":0.2,"max_output_tokens":4096,"seed":null,"source_sha256":"` + publishTestDigest("1") + `",` +
			`"provider":"openai","model":"gpt-4.1","provider_request_id":"req_scenario","raw_response_sha256":"` +
			publishTestDigest("2") + `","input_tokens":100,"output_tokens":200,"valid":true}`)
	case "DATASET":
		return []byte(`{"schema":"schemas/dataset-metadata.schema.json","prompt_version":"dataset-generator/v1","source":"LLM",` +
			`"temperature":0.2,"max_output_tokens":8192,"seed":null,"source_sha256":"` + publishTestDigest("1") + `",` +
			`"scenario_sha256":"` + publishTestDigest("3") + `","provider":"openai","model":"gpt-4.1",` +
			`"provider_request_id":"req_dataset","raw_response_sha256":"` + publishTestDigest("4") +
			`","input_tokens":300,"output_tokens":400,"valid":true}`)
	default:
		return nil
	}
}

// withPublishGenerator adds the generator record a Test Case round's manifest carries when the
// fixture does not already provide one.
func withPublishGenerator(t *testing.T, content []byte) []byte {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(content, &fields); err != nil {
		t.Fatalf("decode Test Case manifest fixture: %v", err)
	}
	if _, present := fields["generator"]; !present {
		fields["generator"] = json.RawMessage(`{"source":"LLM","provider":"openai","model":"gpt-4.1",` +
			`"prompt_version":"test-case-generator/v1","temperature":null,"max_output_tokens":2048,"seed":null,` +
			`"tool_schema_version":"1.0","model_calls":2,"provider_request_id":"req_testcase",` +
			`"raw_response_sha256":"` + publishTestDigest("5") + `"}`)
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("encode Test Case manifest fixture: %v", err)
	}
	return encoded
}

func seedPublishWork(t *testing.T, ctx context.Context, database *sql.DB, jobID, stageKey string) generation.WorkLease {
	t.Helper()
	id := newPublishULID(t)
	owner := "publish-test-worker"
	if _, err := database.ExecContext(ctx, `INSERT INTO work_item (
		id, queue_name, aggregate_type, aggregate_id, stage_key, status, priority,
		lease_owner, lease_until, fencing_token, attempt_count, max_attempts, payload_json
	) VALUES (?, 'generation', 'generation_job', ?, ?, 'LEASED', 0, ?, TIMESTAMPADD(MINUTE, 5, UTC_TIMESTAMP(6)), 1, 1, 3, '{}')`,
		id, jobID, stageKey, owner,
	); err != nil {
		t.Fatalf("seed work_item: %v", err)
	}
	return generation.WorkLease{WorkItemID: id, Owner: owner, FencingToken: 1, AttemptCount: 1, MaxAttempts: 3}
}

func assertPublishJobCompleted(t *testing.T, ctx context.Context, database *sql.DB, jobID string) {
	t.Helper()
	var status string
	if err := database.QueryRowContext(ctx, "SELECT status FROM generation_job WHERE id = ?", jobID).Scan(&status); err != nil {
		t.Fatalf("load job status: %v", err)
	}
	if status != "COMPLETED" {
		t.Fatalf("job status = %q, want COMPLETED", status)
	}
}

func assertPublishRowCount(t *testing.T, ctx context.Context, database *sql.DB, table string, want int) {
	t.Helper()
	var count int
	if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if count != want {
		t.Fatalf("%s row count = %d, want %d", table, count, want)
	}
}

func newPublishULID(t *testing.T) string {
	t.Helper()
	return ulid.Make().String()
}

func openPublishMySQL(t *testing.T, config *mysql.Config) *sql.DB {
	t.Helper()
	connector, err := mysql.NewConnector(config)
	if err != nil {
		t.Fatalf("create MySQL connector: %v", err)
	}
	database := sql.OpenDB(connector)
	database.SetMaxOpenConns(2)
	database.SetMaxIdleConns(2)
	database.SetConnMaxLifetime(time.Minute)
	return database
}

func randomPublishHex(t *testing.T, n int) string {
	t.Helper()
	buffer := make([]byte, (n+1)/2)
	if _, err := rand.Read(buffer); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(buffer)[:n]
}
