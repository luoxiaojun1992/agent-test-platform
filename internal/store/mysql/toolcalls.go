package mysqlstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"agent-test-platform/internal/evidence"

	"github.com/oklog/ulid/v2"
)

var ErrEvidenceConflict = errors.New("tool evidence conflicts with persisted records")

const (
	lockRunSQL       = `SELECT id FROM test_run WHERE id = ? FOR UPDATE`
	loadToolCallsSQL = `SELECT sequence_no, call_id, parent_call_id, tool_name,
arguments_json, arguments_sha256, result_json, result_sha256, error_json, error_sha256,
status, trace_source, truncated, original_bytes, duration_ms, started_at
FROM tool_call_record WHERE test_run_id = ? ORDER BY sequence_no`
	insertToolCallSQL = `INSERT INTO tool_call_record (
id, test_run_id, sequence_no, call_id, parent_call_id, tool_name,
arguments_json, arguments_sha256, result_json, result_sha256, error_json, error_sha256,
status, trace_source, truncated, original_bytes, duration_ms, started_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
)

type IDGenerator func() (string, error)

type ToolCallStore struct {
	database *sql.DB
	newID    IDGenerator
}

func NewToolCallStore(database *sql.DB) (*ToolCallStore, error) {
	return newToolCallStore(database, func() (string, error) {
		id, err := ulid.New(ulid.Timestamp(time.Now().UTC()), rand.Reader)
		if err != nil {
			return "", err
		}
		return id.String(), nil
	})
}

func newToolCallStore(database *sql.DB, generator IDGenerator) (*ToolCallStore, error) {
	if database == nil || generator == nil {
		return nil, fmt.Errorf("database and ID generator are required")
	}
	return &ToolCallStore{database: database, newID: generator}, nil
}

// SaveToolCalls atomically stores one complete ordered call list.
func (store *ToolCallStore) SaveToolCalls(ctx context.Context, runID string, records []evidence.Record) error {
	if _, err := ulid.ParseStrict(strings.ToUpper(strings.TrimSpace(runID))); err != nil {
		return fmt.Errorf("invalid run ID: %w", err)
	}
	if err := validateRecordSet(records); err != nil {
		return err
	}

	transaction, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tool evidence transaction: %w", err)
	}
	defer transaction.Rollback()

	var lockedRunID string
	if err := transaction.QueryRowContext(ctx, lockRunSQL, runID).Scan(&lockedRunID); err != nil {
		return fmt.Errorf("lock test run %s: %w", runID, err)
	}
	existing, err := loadToolCalls(ctx, transaction, runID)
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		if !sameRecordSet(existing, records) {
			return ErrEvidenceConflict
		}
		if err := transaction.Commit(); err != nil {
			return fmt.Errorf("commit idempotent tool evidence: %w", err)
		}
		return nil
	}

	for _, record := range records {
		id, err := store.newID()
		if err != nil {
			return fmt.Errorf("generate tool call ID: %w", err)
		}
		if _, err := transaction.ExecContext(ctx, insertToolCallSQL,
			id, runID, record.Sequence, record.CallID, nullableString(record.ParentCallID), record.ToolName,
			[]byte(record.Arguments), record.ArgumentDigest, nullableJSON(record.Result), nullableStringValue(record.ResultDigest),
			nullableJSON(record.Error), nullableStringValue(record.ErrorDigest), string(record.Status), string(record.Source),
			record.Truncated, nullableInt64(record.OriginalBytes), record.DurationMS, nullableTime(record.StartedAt),
		); err != nil {
			return fmt.Errorf("insert tool call %s: %w", record.CallID, err)
		}
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit tool evidence: %w", err)
	}
	return nil
}

// ListToolCalls returns the normalized persisted calls in sequence order.
func (store *ToolCallStore) ListToolCalls(ctx context.Context, runID string) ([]evidence.Record, error) {
	if _, err := ulid.ParseStrict(strings.ToUpper(strings.TrimSpace(runID))); err != nil {
		return nil, fmt.Errorf("invalid run ID: %w", err)
	}
	return loadToolCalls(ctx, store.database, runID)
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func loadToolCalls(ctx context.Context, queryer queryer, runID string) ([]evidence.Record, error) {
	rows, err := queryer.QueryContext(ctx, loadToolCallsSQL, runID)
	if err != nil {
		return nil, fmt.Errorf("query tool calls: %w", err)
	}
	defer rows.Close()

	var records []evidence.Record
	for rows.Next() {
		var record evidence.Record
		var parent, resultDigest, errorDigest sql.NullString
		var resultJSON, errorJSON []byte
		var status, source string
		var originalBytes sql.NullInt64
		var startedAt sql.NullTime
		if err := rows.Scan(
			&record.Sequence, &record.CallID, &parent, &record.ToolName,
			&record.Arguments, &record.ArgumentDigest, &resultJSON, &resultDigest, &errorJSON, &errorDigest,
			&status, &source, &record.Truncated, &originalBytes, &record.DurationMS, &startedAt,
		); err != nil {
			return nil, fmt.Errorf("scan tool call: %w", err)
		}
		if parent.Valid {
			record.ParentCallID = &parent.String
		}
		if len(resultJSON) > 0 {
			record.Result = append([]byte(nil), resultJSON...)
		}
		if len(errorJSON) > 0 {
			record.Error = append([]byte(nil), errorJSON...)
		}
		record.ResultDigest = resultDigest.String
		record.ErrorDigest = errorDigest.String
		record.Status = evidence.Status(status)
		record.Source = evidence.Source(source)
		if originalBytes.Valid {
			value := originalBytes.Int64
			record.OriginalBytes = &value
		}
		if startedAt.Valid {
			value := startedAt.Time.UTC()
			record.StartedAt = &value
		}
		if err := record.Validate(); err != nil {
			return nil, fmt.Errorf("persisted tool call %s is invalid: %w", record.CallID, err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tool calls: %w", err)
	}
	if err := validateRecordSet(records); err != nil {
		return nil, fmt.Errorf("persisted tool call set: %w", err)
	}
	return records, nil
}

func validateRecordSet(records []evidence.Record) error {
	seen := make(map[string]struct{}, len(records))
	for index, record := range records {
		if err := record.Validate(); err != nil {
			return fmt.Errorf("tool call %d: %w", index+1, err)
		}
		if record.Sequence != index+1 {
			return fmt.Errorf("tool call sequence %d at index %d, want %d", record.Sequence, index, index+1)
		}
		if _, exists := seen[record.CallID]; exists {
			return fmt.Errorf("duplicate call ID %q", record.CallID)
		}
		seen[record.CallID] = struct{}{}
	}
	return nil
}

func sameRecordSet(left, right []evidence.Record) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		a, b := left[index], right[index]
		if a.Sequence != b.Sequence || a.CallID != b.CallID || stringPointer(a.ParentCallID) != stringPointer(b.ParentCallID) ||
			!strings.EqualFold(a.ToolName, b.ToolName) || a.ArgumentDigest != b.ArgumentDigest ||
			a.ResultDigest != b.ResultDigest || a.ErrorDigest != b.ErrorDigest || a.Status != b.Status ||
			a.Source != b.Source || a.Truncated != b.Truncated || int64PointerValue(a.OriginalBytes) != int64PointerValue(b.OriginalBytes) {
			return false
		}
	}
	return true
}

func nullableString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableStringValue(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableJSON(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

func nullableInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC()
}

func nullableFloat64(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

func timeOrZero(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return value.UTC()
}

func stringPointer(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func int64PointerValue(value *int64) int64 {
	if value == nil {
		return -1
	}
	return *value
}
