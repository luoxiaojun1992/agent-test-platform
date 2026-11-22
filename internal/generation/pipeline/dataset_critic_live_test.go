package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-test-platform/internal/llm"

	"github.com/go-sql-driver/mysql"
)

// TestDatasetCriticReadsLiveExecutionSchema runs the paged reader on a real execution schema in
// MySQL 8.4: the catalog queries, the typed projection (CAST for text, HEX for binary), LIMIT and
// OFFSET paging with NULLs, and a full Critic review that reads every page through the Tool loop.
func TestDatasetCriticReadsLiveExecutionSchema(t *testing.T) {
	dsn := os.Getenv("ATP_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set ATP_TEST_MYSQL_DSN to run the live Dataset Critic read test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	base, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	adminConfig := base.Clone()
	adminConfig.DBName = ""
	admin, err := sql.Open("mysql", adminConfig.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	name := "atp_critic_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatalf("create execution schema: %v", err)
	}
	database := openDatabaseNamed(t, base, name)

	if _, err := database.ExecContext(ctx, "CREATE TABLE orders (id VARCHAR(32) NOT NULL PRIMARY KEY, status VARCHAR(32) NULL, amount DECIMAL(10,2) NOT NULL, payload BLOB NULL)"); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 120; index++ {
		var status any = "PAID"
		if index%5 == 0 {
			status = nil
		}
		var payload any = []byte{byte(index), 0xff}
		if index%7 == 0 {
			payload = nil
		}
		if _, err := database.ExecContext(ctx, "INSERT INTO orders (id, status, amount, payload) VALUES (?, ?, 1.50, ?)",
			fmt.Sprintf("O%05d", index), status, payload); err != nil {
			t.Fatal(err)
		}
	}

	reader, err := loadDatasetRows(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	table, ok := reader.lookup("orders")
	if !ok || table.rowCount != 120 || len(table.primaryKey) != 1 || table.primaryKey[0] != "id" || len(table.columns) != 4 {
		t.Fatalf("catalog = %#v", table)
	}
	first, err := reader.page(ctx, table, 0, datasetReadDefaultRows)
	if err != nil {
		t.Fatal(err)
	}
	if first.ReturnedRows != 50 || first.NextOffset == nil || *first.NextOffset != 50 || first.Rows[0][0] == nil || *first.Rows[0][0] != "O00000" {
		t.Fatalf("first page = %#v", first)
	}
	if first.Rows[0][1] != nil || first.Rows[0][3] != nil {
		t.Fatalf("row 0 should carry NULL status and payload: %#v", first.Rows[0])
	}
	if first.Rows[1][3] == nil || *first.Rows[1][3] != "01FF" {
		t.Fatalf("payload of row 1 = %v, want hex 01FF", first.Rows[1][3])
	}
	last, err := reader.page(ctx, table, 100, datasetReadDefaultRows)
	if err != nil {
		t.Fatal(err)
	}
	if last.ReturnedRows != 20 || last.NextOffset != nil || last.Rows[19][0] == nil || *last.Rows[19][0] != "O00119" {
		t.Fatalf("last page = %#v", last)
	}

	critic := &scriptedCritic{responses: []llm.CompletionResponse{
		criticToolCall("read-0", datasetReadToolName, `{"table":"orders","offset":0,"limit":50}`),
		criticToolCall("read-50", datasetReadToolName, `{"table":"orders","offset":50,"limit":50}`),
		criticToolCall("read-100", datasetReadToolName, `{"table":"orders","offset":100,"limit":50}`),
		criticToolCall("submit-1", datasetSubmitToolName, acceptedDatasetReview),
	}}
	processor, _, _ := newCriticHarness(t, critic)
	review, err := processor.runDatasetCritic(ctx, criticStage(),
		json.RawMessage(criticTestScenario), json.RawMessage(criticTestDataset), database, nil, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !review.Outcome.accepted() {
		t.Fatalf("review outcome = %#v", review.Outcome)
	}
	if !strings.Contains(toolResultText(critic.requests[1]), `"01FF"`) {
		t.Fatal("the Critic's first page did not carry the binary value as hex")
	}
	var coverage datasetCoverage
	if err := json.Unmarshal(review.Provenance.ReadCoverage, &coverage); err != nil {
		t.Fatal(err)
	}
	if got := coverage.Tables["orders"]; got.TotalRows != 120 || len(got.Ranges) != 1 || got.Ranges[0] != [2]int{0, 120} {
		t.Fatalf("coverage = %#v", got)
	}
}
