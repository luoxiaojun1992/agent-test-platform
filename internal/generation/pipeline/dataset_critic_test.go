package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"agent-test-platform/contracts"
	"agent-test-platform/internal/generation"
	"agent-test-platform/internal/llm"

	"github.com/DATA-DOG/go-sqlmock"
)

const acceptedDatasetReview = `{"schema_version":"1.0","decision":"ACCEPT","coverage":{"requested":["NORMAL"],"covered":["NORMAL"]},"case_findings":[],"package_findings":[],"confidence":0.9}`

const criticTestScenario = `{"scenario_id":"cancel-paid-order"}`

const criticTestDataset = `{"schema_sql":"CREATE TABLE orders (id VARCHAR(32) PRIMARY KEY, status VARCHAR(32))","constraints":{"version":"1.0","checks":[]}}`

// scriptedCritic answers each Critic model call with the next scripted response and records every
// request, so tests can inspect exactly what the model was shown.
type scriptedCritic struct {
	responses []llm.CompletionResponse
	requests  []llm.CompletionRequest
}

func (critic *scriptedCritic) Complete(_ context.Context, _ string, request llm.CompletionRequest) (llm.CompletionResponse, error) {
	critic.requests = append(critic.requests, request)
	if len(critic.requests) > len(critic.responses) {
		return llm.CompletionResponse{}, errors.New("no scripted Critic response")
	}
	return critic.responses[len(critic.requests)-1], nil
}

type countingRecorder struct {
	reports int
}

func (recorder *countingRecorder) RecordCompaction(context.Context, llm.CompactionReport) error {
	recorder.reports++
	return nil
}

func criticToolCall(id, name, arguments string) llm.CompletionResponse {
	return llm.CompletionResponse{
		StopReason: "tool_calls", Model: llm.ModelRef{Provider: llm.ProviderOpenAI, Model: "critic-test"},
		ToolCalls: []llm.ModelToolCall{{ID: id, Name: name, Arguments: json.RawMessage(arguments)}},
	}
}

// criticTestPolicy is the configured policy a worker would pass: a 16K window, conservative budget.
func criticTestPolicy() llm.CompactionPolicy {
	return llm.CompactionPolicy{
		ContextWindow: 16_000, ReservedOutputTokens: 2_048, SafetyMarginTokens: 1_024,
		KeepRecentGroups: 1, MaxSummaryBytes: 1_024, MaxCompactions: 4,
		MaxRetainedRecords: 4, MaxRollupBytes: 4_096, Conservative: true,
	}
}

// newCriticHarness builds a processor with a real contract validator and the scripted Critic. The
// returned sqlmock backs the execution schema, which tests program with expectOrdersCatalog and
// further queries.
func newCriticHarness(t *testing.T, critic CriticGateway) (*Processor, sqlmock.Sqlmock, *sql.DB) {
	t.Helper()
	database, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	processor, err := New(datasetFakeStore(t), validator, WithCriticReview(critic, CriticConfig{
		ModelAlias: "critic-primary", MaxOutputTokens: 2_048, CompactionPolicy: criticTestPolicy(),
	}))
	if err != nil {
		t.Fatal(err)
	}
	return processor, mock, database
}

// expectOrdersCatalog programs the catalog queries for one table, orders, with columns id (primary
// key) and status, and the given row count.
func expectOrdersCatalog(mock sqlmock.Sqlmock, rowCount int64) {
	mock.ExpectQuery(regexp.QuoteMeta("information_schema.TABLES")).
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME"}).AddRow("orders"))
	mock.ExpectQuery(regexp.QuoteMeta("information_schema.COLUMNS")).
		WillReturnRows(sqlmock.NewRows([]string{"t", "c", "d"}).AddRow("orders", "id", "varchar").AddRow("orders", "status", "varchar"))
	mock.ExpectQuery(regexp.QuoteMeta("information_schema.KEY_COLUMN_USAGE")).
		WillReturnRows(sqlmock.NewRows([]string{"t", "c"}).AddRow("orders", "id"))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM `orders`")).
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(rowCount))
}

// catalogTarget is a validation MySQL fake whose execution schema matches expectOrdersCatalog.
func catalogTarget(t *testing.T) *fakeValidationDB {
	t.Helper()
	database, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	expectOrdersCatalog(mock, 3)
	return &fakeValidationDB{loader: database}
}

func criticStage() generation.StageRequest {
	return generation.StageRequest{JobID: pipelineJobID, Stage: generation.StatusValidatingDataset, Attempt: 1, Lease: validLease()}
}

func requestText(request llm.CompletionRequest) string {
	var builder strings.Builder
	for _, message := range request.Messages {
		if message.Content != nil {
			builder.WriteString(*message.Content)
			builder.WriteString("\n")
		}
	}
	return builder.String()
}

func toolResultText(request llm.CompletionRequest) string {
	var builder strings.Builder
	for _, message := range request.Messages {
		if message.Role == llm.RoleTool && message.Content != nil {
			builder.WriteString(*message.Content)
		}
	}
	return builder.String()
}

func TestDatasetCriticReadsRowsOnlyThroughTheTool(t *testing.T) {
	t.Parallel()

	critic := &scriptedCritic{responses: []llm.CompletionResponse{
		criticToolCall("read-1", datasetReadToolName, `{"table":"orders"}`),
		criticToolCall("submit-1", datasetSubmitToolName, acceptedDatasetReview),
	}}
	processor, mock, database := newCriticHarness(t, critic)
	expectOrdersCatalog(mock, 3)
	mock.ExpectQuery(regexp.QuoteMeta("FROM `orders` ORDER BY `id` LIMIT ? OFFSET ?")).
		WithArgs(int64(datasetReadDefaultRows), int64(0)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).
			AddRow("O1", "SNAPSHOT-ROW-VALUE-77").AddRow("O2", nil).AddRow("O3", "PAID"))

	review, err := processor.runDatasetCritic(context.Background(), criticStage(),
		json.RawMessage(criticTestScenario), json.RawMessage(criticTestDataset), database, nil, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !review.Outcome.accepted() {
		t.Fatalf("review outcome = %#v", review.Outcome)
	}
	if strings.Contains(requestText(critic.requests[0]), "SNAPSHOT-ROW-VALUE-77") {
		t.Fatal("row values reached the Critic before any read")
	}
	if !strings.Contains(toolResultText(critic.requests[1]), "SNAPSHOT-ROW-VALUE-77") {
		t.Fatal("the read result did not deliver the row")
	}
	var coverage datasetCoverage
	if err := json.Unmarshal(review.Provenance.ReadCoverage, &coverage); err != nil {
		t.Fatal(err)
	}
	if got := coverage.Tables["orders"]; got.TotalRows != 3 || len(got.Ranges) != 1 || got.Ranges[0] != [2]int{0, 3} {
		t.Fatalf("coverage = %#v", got)
	}
	if review.Provenance.ToolCalls != 2 || review.Provenance.ModelCalls != 2 || review.Provenance.PromptVersion != datasetCriticPromptVersion {
		t.Fatalf("provenance = %#v", review.Provenance)
	}
}

func TestDatasetCriticRequiresASchemaValidReviewBeforeAccepting(t *testing.T) {
	t.Parallel()

	critic := &scriptedCritic{responses: []llm.CompletionResponse{
		criticToolCall("submit-1", datasetSubmitToolName, `{"decision":"ACCEPT"}`),
		criticToolCall("submit-2", datasetSubmitToolName, acceptedDatasetReview),
	}}
	processor, mock, database := newCriticHarness(t, critic)
	expectOrdersCatalog(mock, 0)

	review, err := processor.runDatasetCritic(context.Background(), criticStage(),
		json.RawMessage(criticTestScenario), json.RawMessage(criticTestDataset), database, nil, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !review.Outcome.accepted() {
		t.Fatalf("review outcome = %#v", review.Outcome)
	}
	if !strings.Contains(toolResultText(critic.requests[1]), "REVIEW_INVALID") {
		t.Fatalf("the invalid review was not reported back: %s", toolResultText(critic.requests[1]))
	}
}

// Six pages of about 3 KB each cannot all fit the 16K window, so older pages compact. The newest page
// stays verbatim, the coverage ledger keeps every range, and the read budget then stops reading.
func TestDatasetCriticPagesStayWithinBudgetAcrossCompaction(t *testing.T) {
	t.Parallel()

	const pages = 6
	responses := make([]llm.CompletionResponse, 0, pages+2)
	for page := 0; page < pages; page++ {
		responses = append(responses, criticToolCall(fmt.Sprintf("read-%d", page), datasetReadToolName,
			fmt.Sprintf(`{"table":"orders","offset":%d,"limit":50}`, page*50)))
	}
	responses = append(responses,
		criticToolCall("read-over", datasetReadToolName, `{"table":"orders"}`),
		criticToolCall("submit-1", datasetSubmitToolName, acceptedDatasetReview),
	)
	critic := &scriptedCritic{responses: responses}
	processor, mock, database := newCriticHarness(t, critic)
	expectOrdersCatalog(mock, 1200)
	for page := 0; page < pages; page++ {
		rows := sqlmock.NewRows([]string{"id", "status"})
		for row := 0; row < 50; row++ {
			rows.AddRow(fmt.Sprintf("row-%06d-%s", page*50+row, strings.Repeat("x", 30)), "PAID")
		}
		mock.ExpectQuery(regexp.QuoteMeta("FROM `orders` ORDER BY `id` LIMIT ? OFFSET ?")).
			WithArgs(int64(50), int64(page*50)).WillReturnRows(rows)
	}
	recorder := &countingRecorder{}

	review, err := processor.runDatasetCritic(context.Background(), criticStage(),
		json.RawMessage(criticTestScenario), json.RawMessage(criticTestDataset), database, recorder, pages)
	if err != nil {
		t.Fatal(err)
	}
	if !review.Outcome.accepted() {
		t.Fatalf("review outcome = %#v", review.Outcome)
	}
	if recorder.reports == 0 {
		t.Fatal("no compaction happened, so the test did not exercise bounded context")
	}
	last := critic.requests[len(critic.requests)-1]
	if !strings.Contains(toolResultText(critic.requests[pages+1]), "READ_BUDGET_EXHAUSTED") {
		t.Fatal("the read beyond the budget was not refused")
	}
	if strings.Contains(requestText(last), "row-000000-") {
		t.Fatal("the first page is still verbatim after compaction")
	}
	if !strings.Contains(requestText(last), `"state":{"tables"`) {
		t.Fatal("the coverage ledger is missing from the compacted context")
	}
	var coverage datasetCoverage
	if err := json.Unmarshal(review.Provenance.ReadCoverage, &coverage); err != nil {
		t.Fatal(err)
	}
	if got := coverage.Tables["orders"]; len(got.Ranges) != 1 || got.Ranges[0] != [2]int{0, pages * 50} {
		t.Fatalf("coverage = %#v", got)
	}
}

func TestDatasetCriticChecksToolArgumentsAndNames(t *testing.T) {
	t.Parallel()

	_, mock, database := newCriticHarness(t, &scriptedCritic{})
	expectOrdersCatalog(mock, 3)
	rows, err := loadDatasetRows(context.Background(), database)
	if err != nil {
		t.Fatal(err)
	}
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	critic := &datasetCritic{validator: validator, rows: rows, readBudget: 8}
	cases := map[string]struct {
		name      string
		arguments string
		code      string
	}{
		"unknown table":     {datasetReadToolName, `{"table":"missing"}`, "UNKNOWN_TABLE"},
		"negative offset":   {datasetReadToolName, `{"table":"orders","offset":-1}`, "INVALID_ARGUMENTS"},
		"oversized limit":   {datasetReadToolName, `{"table":"orders","limit":500}`, "INVALID_ARGUMENTS"},
		"unexpected field":  {datasetReadToolName, `{"table":"orders","where":"1=1"}`, "INVALID_ARGUMENTS"},
		"unknown tool name": {"drop_table", `{}`, "UNKNOWN_TOOL"},
	}
	for name, test := range cases {
		execution, err := critic.Execute(context.Background(), llm.ModelToolCall{ID: "call", Name: test.name, Arguments: json.RawMessage(test.arguments)})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if execution.Status != "ERROR" || !strings.Contains(execution.Content, test.code) || execution.Terminal {
			t.Fatalf("%s: execution = %#v", name, execution)
		}
	}
}

func TestMapDatasetCriticErrorsToJobFailures(t *testing.T) {
	t.Parallel()

	cases := []struct {
		err  error
		code string
	}{
		{fmt.Errorf("wrapped: %w", llm.ErrContextBudgetExceeded), "CONTEXT_BUDGET_EXCEEDED"},
		{llm.ErrOutputTokenLimit, "TOKEN_LIMIT_EXCEEDED"},
		{llm.ErrModelCallLimit, "RESOURCE_LIMIT_EXCEEDED"},
		{llm.ErrToolCallLimit, "RESOURCE_LIMIT_EXCEEDED"},
	}
	for _, test := range cases {
		result, handled := mapDatasetCriticError(test.err)
		if !handled || result.Failure == nil || result.Failure.Code != test.code {
			t.Fatalf("%v mapped to %#v (handled=%v), want %s", test.err, result.Failure, handled, test.code)
		}
	}
	if _, handled := mapDatasetCriticError(llm.ErrRequiredToolMissing); handled {
		t.Fatal("a missing review call must stay a stage error for the worker to retry")
	}
}

func TestFoldDatasetReadMergesCoverageAndIgnoresOtherTools(t *testing.T) {
	t.Parallel()

	group := func(callID, toolName, offset string) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"source_digest":"d","tool_calls":[{"call_id":%q,"tool_name":%q,"arguments_digest":"a"}],`+
			`"tool_results":[{"call_id":%q,"status":"SUCCESS","result_digest":"r","original_bytes":10,"summary":%s}]}`,
			callID, toolName, callID, offset))
	}
	first, err := foldDatasetRead(nil, group("read-1", datasetReadToolName,
		`{"table":"orders","offset":0,"returned_rows":50,"total_rows":120}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := foldDatasetRead(first, group("read-2", datasetReadToolName,
		`{"table":"orders","offset":50,"returned_rows":50,"total_rows":120}`))
	if err != nil {
		t.Fatal(err)
	}
	third, err := foldDatasetRead(second, group("submit-1", datasetSubmitToolName,
		`{"table":"orders","offset":100,"returned_rows":20,"total_rows":120}`))
	if err != nil {
		t.Fatal(err)
	}
	var coverage datasetCoverage
	if err := json.Unmarshal(third, &coverage); err != nil {
		t.Fatal(err)
	}
	if got := coverage.Tables["orders"]; got.TotalRows != 120 || len(got.Ranges) != 1 || got.Ranges[0] != [2]int{0, 100} {
		t.Fatalf("merged coverage = %#v", got)
	}
}

// A model call that outlives the stage deadline must return context.DeadlineExceeded, which the
// worker records as DATASET_VALIDATION_TIMEOUT.
func TestDatasetCriticStopsAtStageDeadline(t *testing.T) {
	t.Parallel()

	processor, mock, database := newCriticHarness(t, stallingCritic{})
	expectOrdersCatalog(mock, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := processor.runDatasetCritic(ctx, criticStage(),
		json.RawMessage(criticTestScenario), json.RawMessage(criticTestDataset), database, nil, datasetCriticReadBudget)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
}

// stallingCritic never answers before its context ends.
type stallingCritic struct{}

func (stallingCritic) Complete(ctx context.Context, _ string, _ llm.CompletionRequest) (llm.CompletionResponse, error) {
	<-ctx.Done()
	return llm.CompletionResponse{}, ctx.Err()
}

func TestDatasetWithoutRowsDropsOnlyTheSeed(t *testing.T) {
	t.Parallel()

	withoutRows, err := datasetWithoutRows([]byte(`{"schema_sql":"CREATE TABLE t (id INT PRIMARY KEY)","seed_sql":"INSERT INTO t (id) VALUES (1)"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(withoutRows), "seed_sql") || strings.Contains(string(withoutRows), "VALUES (1)") ||
		!strings.Contains(string(withoutRows), "schema_sql") {
		t.Fatalf("Critic document = %s", withoutRows)
	}
}
