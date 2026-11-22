package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"agent-test-platform/internal/generation"
	"agent-test-platform/internal/llm"
	"agent-test-platform/internal/sandbox"
	"agent-test-platform/internal/snapshot"

	"github.com/go-sql-driver/mysql"
)

type staticSecret string

func (secret staticSecret) Resolve(_ context.Context, _ string) (string, error) {
	return string(secret), nil
}

// snapshotBusinessReply returns the business fields the model would give for the reference schema
// used below. checkQuery and expectation decide whether the imported rows satisfy the constraint.
func snapshotBusinessReply(checkQuery string, expected int) string {
	return fmt.Sprintf(`{
  "constraints": {"version": "1.0", "checks": [{
    "id": "orders-reference-customers", "phases": ["AFTER_SEED"], "query": %q,
    "expectation": {"type": "ROW_COUNT", "equals": %d}, "critical": true, "rule_id": "orders-customer"
  }]},
  "difficulty_profile": {"data_complexity": 2, "reasoning_complexity": 2, "tool_complexity": 2, "ambiguity": 1, "multi_step": 1, "state_change_complexity": 1, "adversarial_complexity": 1},
  "coverage_tags": ["NORMAL"]
}`, checkQuery, expected)
}

// openDatabaseNamed opens a single-connection handle to one database on the test MySQL instance.
func openDatabaseNamed(t *testing.T, base *mysql.Config, name string) *sql.DB {
	t.Helper()
	config := base.Clone()
	config.DBName = name
	connector, err := mysql.NewConnector(config)
	if err != nil {
		t.Fatal(err)
	}
	database := sql.OpenDB(connector)
	database.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = database.Close() })
	return database
}

// TestSnapshotDatasetPassesTargetValidation runs the snapshot path end to end on MySQL 8.4: the
// reference schema and rows are read by the snapshot adapter, the stage builds the Dataset draft,
// and VALIDATING_DATASET loads that draft into a fresh execution schema on validation MySQL, where the
// model's constraints are checked against the imported rows.
func TestSnapshotDatasetPassesTargetValidation(t *testing.T) {
	dsn := os.Getenv("ATP_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set ATP_TEST_MYSQL_DSN to run the snapshot Dataset validation test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	base, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	base.DBName = ""
	admin := openDatabaseNamed(t, base, "")

	reference := fmt.Sprintf("atp_e2e_ref_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+reference+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatal(err)
	}
	referenceDB := openDatabaseNamed(t, base, reference)
	for _, statement := range []string{
		`CREATE TABLE customers (id VARCHAR(32) NOT NULL PRIMARY KEY, name VARCHAR(128) NOT NULL)`,
		`CREATE TABLE orders (id VARCHAR(32) NOT NULL PRIMARY KEY, customer_id VARCHAR(32) NOT NULL, status VARCHAR(32) NOT NULL,
			CONSTRAINT fk_orders_customer FOREIGN KEY (customer_id) REFERENCES customers (id))`,
		`INSERT INTO customers (id, name) VALUES ('C1', 'Ada')`,
		`INSERT INTO orders (id, customer_id, status) VALUES ('O1', 'C1', 'PAID'), ('O2', 'C1', 'SHIPPED')`,
	} {
		if _, err := referenceDB.ExecContext(ctx, statement); err != nil {
			t.Fatalf("prepare reference schema: %v", err)
		}
	}

	host, portText, err := net.SplitHostPort(base.Addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	profile := importedProfile()
	profile.Host, profile.Port, profile.Database, profile.Username = host, port, reference, base.User
	profile.TLSMode = "DISABLED_LOCAL_ONLY"

	// The orphan check passes on the reference rows (the foreign key guarantees it), so the Dataset is accepted.
	runSnapshotDatasetThroughValidation(ctx, t, base, profile,
		snapshotBusinessReply(`SELECT o.id FROM orders o LEFT JOIN customers c ON c.id = o.customer_id WHERE c.id IS NULL`, 0),
		base.Passwd, generation.EventValidationAccepted)

	// A check the reference rows violate is critical, so the same flow rejects the Dataset.
	runSnapshotDatasetThroughValidation(ctx, t, base, profile,
		snapshotBusinessReply(`SELECT o.id FROM orders o WHERE o.status = 'PAID'`, 0),
		base.Passwd, generation.EventValidationRejected)
}

func runSnapshotDatasetThroughValidation(
	ctx context.Context, t *testing.T, base *mysql.Config, profile generation.DatabaseProfileConnection,
	businessReply, secret string, wantEvent generation.JobEvent,
) {
	t.Helper()
	source, err := snapshot.NewMySQLSource(staticSecret(secret))
	if err != nil {
		t.Fatal(err)
	}
	store := importStore(t)
	store.profile = profile
	gateway := &fakeDatasetGateway{response: llm.CompletionResponse{
		Content: businessReply, StopReason: "stop", Model: llm.ModelRef{Provider: llm.ProviderOpenAI, Model: "gpt-test"},
	}}
	processor := newDatasetProcessor(t, store, gateway, scenarioPolicy())
	processor.snapshotSource = source
	stage := generation.StageRequest{JobID: pipelineJobID, Stage: generation.StatusGeneratingDataset, Attempt: 1, Lease: validLease()}

	result, err := processor.Process(ctx, stage)
	if err != nil || result.Failure != nil || result.Transition.Event != generation.EventDatasetStored {
		t.Fatalf("generate stage result=%#v error=%v", result, err)
	}
	draft := *store.saved
	store.artifactsByKey[generation.ArtifactKindDataset+"/"+datasetDraftKey(0)] = generation.ArtifactSnapshot{
		Kind: generation.ArtifactKindDataset, Key: datasetDraftKey(0), ContentJSON: draft.ContentJSON, SHA256: draft.SHA256,
	}

	processor.targets = singleTarget{sandbox.AdminConnection{Admin: openDatabaseNamed(t, base, ""), Base: base}}
	stage.Stage = generation.StatusValidatingDataset
	result, err = processor.Process(ctx, stage)
	if err != nil || result.Failure != nil || result.Transition.Event != wantEvent {
		t.Fatalf("validate stage result=%#v error=%v, want %s", result, err, wantEvent)
	}
	if wantEvent != generation.EventValidationAccepted {
		return
	}
	if store.saved == nil || store.saved.Kind != generation.ArtifactKindDatasetExecution {
		t.Fatal("an accepted Dataset did not record its execution schema")
	}
	var execution generation.DatasetExecutionRecord
	if err := json.Unmarshal(store.saved.ContentJSON, &execution); err != nil || len(execution.Targets) == 0 || execution.Targets[0].Database == "" {
		t.Fatalf("execution record = %s error=%v", store.saved.ContentJSON, err)
	}
	var orders int
	if err := openDatabaseNamed(t, base, execution.Targets[0].Database).QueryRowContext(ctx, "SELECT COUNT(*) FROM orders").Scan(&orders); err != nil || orders != 2 {
		t.Fatalf("execution schema holds %d orders, error=%v; want the 2 reference rows", orders, err)
	}
}
