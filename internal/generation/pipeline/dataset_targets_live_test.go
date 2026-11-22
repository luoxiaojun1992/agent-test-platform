package pipeline

import (
	"context"
	"os"
	"testing"
	"time"

	"agent-test-platform/internal/generation"
	"agent-test-platform/internal/sandbox"

	"github.com/go-sql-driver/mysql"
)

// TestDatasetValidatesOnTwoLiveTargets runs VALIDATING_DATASET for one Dataset bound to two separate MySQL 8.4
// instances. Each instance must hold the execution schema with the seeded row, and the execution record must name
// both. Nothing is dropped: the schemas stay on both instances for manual deletion, and each run uses a new
// revision ID, so reruns never collide.
func TestDatasetValidatesOnTwoLiveTargets(t *testing.T) {
	firstDSN := os.Getenv("ATP_TEST_MYSQL_DSN")
	secondDSN := os.Getenv("ATP_TEST_MYSQL_DSN_B")
	if firstDSN == "" || secondDSN == "" {
		t.Skip("set ATP_TEST_MYSQL_DSN and ATP_TEST_MYSQL_DSN_B to run the two-Target Dataset validation test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	connections := map[string]sandbox.AdminConnection{
		testTargetID:       liveAdminConnection(t, firstDSN),
		secondTestTargetID: liveAdminConnection(t, secondDSN),
	}
	store := datasetValidationFakeStore(t)
	store.artifactsByKind[generation.ArtifactKindNormalizedRequest] = normalizedArtifactForTargets(t, testTargetID, secondTestTargetID)
	processor := newProcessorWithTargets(t, store, targetsByID{
		testTargetID:       connections[testTargetID],
		secondTestTargetID: connections[secondTestTargetID],
	})

	result, err := processor.Process(ctx, validateDatasetStage())
	if err != nil {
		t.Fatal(err)
	}
	if result.Failure != nil || result.Transition.Event != generation.EventValidationAccepted {
		t.Fatalf("result = %#v", result)
	}
	if store.saved == nil || store.saved.Kind != generation.ArtifactKindDatasetExecution {
		t.Fatalf("execution artifact = %#v", store.saved)
	}
	record, err := generation.DecodeDatasetExecutionRecord(store.saved.ContentJSON)
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Targets) != 2 || record.Targets[0].ValidationDBID != testTargetID || record.Targets[1].ValidationDBID != secondTestTargetID {
		t.Fatalf("execution Targets = %#v, want %s then %s", record.Targets, testTargetID, secondTestTargetID)
	}
	for _, bound := range record.Targets {
		assertLiveExecutionSchema(ctx, t, connections[bound.ValidationDBID], bound)
	}
}

// liveAdminConnection opens the administrator connection of one MySQL 8.4 instance, the way the Worker opens a
// registered Target.
func liveAdminConnection(t *testing.T, dsn string) sandbox.AdminConnection {
	t.Helper()
	base, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	base.DBName = ""
	return sandbox.AdminConnection{Admin: openDatabaseNamed(t, base, ""), Base: base}
}

// assertLiveExecutionSchema checks that a Target holds the execution schema named in the record, with the seeded row.
func assertLiveExecutionSchema(ctx context.Context, t *testing.T, connection sandbox.AdminConnection, bound generation.DatasetExecutionTarget) {
	t.Helper()
	var schemas int
	if err := connection.Admin.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", bound.Database,
	).Scan(&schemas); err != nil {
		t.Fatal(err)
	}
	if schemas != 1 {
		t.Fatalf("Target %s: execution schema %s was not created", bound.ValidationDBID, bound.Database)
	}
	database, err := connection.OpenDatabase(ctx, bound.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var orders int
	if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM orders").Scan(&orders); err != nil {
		t.Fatal(err)
	}
	if orders != 1 {
		t.Fatalf("Target %s: %s.orders holds %d rows, want the seeded row", bound.ValidationDBID, bound.Database, orders)
	}
}
