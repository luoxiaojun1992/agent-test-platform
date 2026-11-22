package mysqlstore

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"agent-test-platform/internal/evidence"

	"github.com/DATA-DOG/go-sqlmock"
)

const (
	storeRunID = "01K4Z6Y7X8W9V0T1S2R3Q4P5N6"
	recordID   = "01K4Z6Y7X8W9V0T1S2R3Q4P5N7"
)

func TestSaveToolCalls(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := newToolCallStore(database, func() (string, error) { return recordID, nil })
	if err != nil {
		t.Fatal(err)
	}
	records := validRecords()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(lockRunSQL)).WithArgs(storeRunID).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(storeRunID))
	mock.ExpectQuery(regexp.QuoteMeta(loadToolCallsSQL)).WithArgs(storeRunID).WillReturnRows(toolCallRows())
	mock.ExpectExec(regexp.QuoteMeta(insertToolCallSQL)).WithArgs(
		recordID, storeRunID, 1, "call-1", nil, "mysql.query",
		[]byte(records[0].Arguments), records[0].ArgumentDigest, []byte(records[0].Result), records[0].ResultDigest,
		nil, nil, "SUCCESS", "AGENT_REPORTED", false, nil, int64(12), nil,
	).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if err := store.SaveToolCalls(context.Background(), storeRunID, records); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSaveToolCallsIsIdempotent(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := newToolCallStore(database, func() (string, error) { return recordID, nil })
	records := validRecords()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(lockRunSQL)).WithArgs(storeRunID).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(storeRunID))
	mock.ExpectQuery(regexp.QuoteMeta(loadToolCallsSQL)).WithArgs(storeRunID).WillReturnRows(toolCallRowsFrom(records))
	mock.ExpectCommit()
	if err := store.SaveToolCalls(context.Background(), storeRunID, records); err != nil {
		t.Fatal(err)
	}
}

func TestSaveToolCallsRejectsConflict(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := newToolCallStore(database, func() (string, error) { return recordID, nil })
	records := validRecords()
	existing := validRecords()
	existing[0].Arguments = []byte(`{"sql":"SELECT 2"}`)
	existing[0].ArgumentDigest, _ = evidence.CanonicalJSONDigest(existing[0].Arguments)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(lockRunSQL)).WithArgs(storeRunID).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(storeRunID))
	mock.ExpectQuery(regexp.QuoteMeta(loadToolCallsSQL)).WithArgs(storeRunID).WillReturnRows(toolCallRowsFrom(existing))
	mock.ExpectRollback()
	err = store.SaveToolCalls(context.Background(), storeRunID, records)
	if !errors.Is(err, ErrEvidenceConflict) {
		t.Fatalf("error = %v", err)
	}
}

func TestListToolCalls(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := newToolCallStore(database, func() (string, error) { return recordID, nil })
	records := validRecords()
	mock.ExpectQuery(regexp.QuoteMeta(loadToolCallsSQL)).WithArgs(storeRunID).WillReturnRows(toolCallRowsFrom(records))
	got, err := store.ListToolCalls(context.Background(), storeRunID)
	if err != nil {
		t.Fatal(err)
	}
	if !sameRecordSet(got, records) {
		t.Fatalf("records differ: got %#v want %#v", got, records)
	}
}

func validRecords() []evidence.Record {
	arguments := []byte(`{"sql":"SELECT 1"}`)
	argumentDigest, _ := evidence.CanonicalJSONDigest(arguments)
	result := []byte(`{"status":"SUCCESS","output":null,"truncated":false}`)
	resultDigest, _ := evidence.CanonicalJSONDigest([]byte(`null`))
	return []evidence.Record{{
		Sequence: 1, CallID: "call-1", ToolName: "mysql.query",
		Arguments: arguments, ArgumentDigest: argumentDigest,
		Result: result, ResultDigest: resultDigest,
		Status: evidence.StatusSuccess, Source: evidence.SourceAgentReported, DurationMS: 12,
	}}
}

func toolCallRows() *sqlmock.Rows {
	return sqlmock.NewRows(toolCallColumns())
}

func toolCallRowsFrom(records []evidence.Record) *sqlmock.Rows {
	rows := toolCallRows()
	for _, record := range records {
		rows.AddRow(
			record.Sequence, record.CallID, nil, record.ToolName,
			[]byte(record.Arguments), record.ArgumentDigest, []byte(record.Result), record.ResultDigest,
			nil, nil, string(record.Status), string(record.Source), record.Truncated, nil, record.DurationMS, nil,
		)
	}
	return rows
}

func toolCallColumns() []string {
	return []string{"sequence_no", "call_id", "parent_call_id", "tool_name", "arguments_json", "arguments_sha256", "result_json", "result_sha256", "error_json", "error_sha256", "status", "trace_source", "truncated", "original_bytes", "duration_ms", "started_at"}
}
