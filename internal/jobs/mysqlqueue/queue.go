package mysqlqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	ErrNoWork     = errors.New("no work available")
	ErrStaleLease = errors.New("work lease is stale")
)

const (
	claimSQL = `SELECT id, aggregate_type, aggregate_id, stage_key, payload_json,
fencing_token, attempt_count, max_attempts
FROM work_item
WHERE queue_name = ?
  AND available_at <= UTC_TIMESTAMP(6)
  AND (status = 'READY' OR (status = 'LEASED' AND lease_until < UTC_TIMESTAMP(6)))
ORDER BY priority DESC, created_at
LIMIT 1
FOR UPDATE SKIP LOCKED`
	leaseSQL = `UPDATE work_item
SET status = 'LEASED', lease_owner = ?,
    lease_until = TIMESTAMPADD(SECOND, ?, UTC_TIMESTAMP(6)),
    fencing_token = fencing_token + 1,
    attempt_count = attempt_count + 1,
    updated_at = UTC_TIMESTAMP(6)
WHERE id = ? AND fencing_token = ?`
	heartbeatSQL = `UPDATE work_item
SET lease_until = TIMESTAMPADD(SECOND, ?, UTC_TIMESTAMP(6)), updated_at = UTC_TIMESTAMP(6)
WHERE id = ? AND status = 'LEASED' AND lease_owner = ? AND fencing_token = ?`
	completeSQL = `UPDATE work_item
SET status = 'DONE', lease_owner = NULL, lease_until = NULL, updated_at = UTC_TIMESTAMP(6)
WHERE id = ? AND status = 'LEASED' AND lease_owner = ? AND fencing_token = ?`
	retrySQL = `UPDATE work_item
SET status = 'READY', lease_owner = NULL, lease_until = NULL,
    available_at = TIMESTAMPADD(MICROSECOND, ?, UTC_TIMESTAMP(6)),
    last_error_json = ?, updated_at = UTC_TIMESTAMP(6)
WHERE id = ? AND status = 'LEASED' AND lease_owner = ? AND fencing_token = ?`
	deadSQL = `UPDATE work_item
SET status = 'DEAD', lease_owner = NULL, lease_until = NULL,
    last_error_json = ?, updated_at = UTC_TIMESTAMP(6)
WHERE id = ? AND status = 'LEASED' AND lease_owner = ? AND fencing_token = ?`
	enqueueSQL = `INSERT INTO work_item (
id, queue_name, aggregate_type, aggregate_id, stage_key, status, priority,
available_at, fencing_token, attempt_count, max_attempts, payload_json
) VALUES (?, ?, ?, ?, ?, 'READY', ?, ?, 0, 0, ?, ?)`
)

type Queue struct {
	database *sql.DB
}

type EnqueueRequest struct {
	ID            string
	QueueName     string
	AggregateType string
	AggregateID   string
	StageKey      string
	Priority      int
	AvailableAt   time.Time
	MaxAttempts   int
	Payload       json.RawMessage
}

type Item struct {
	ID            string
	QueueName     string
	AggregateType string
	AggregateID   string
	StageKey      string
	Payload       json.RawMessage
	FencingToken  uint64
	AttemptCount  int
	MaxAttempts   int
	LeaseOwner    string
}

// Exhausted reports whether this administrative reclaim is beyond the final processing attempt.
func (item Item) Exhausted() bool {
	return item.MaxAttempts > 0 && item.AttemptCount > item.MaxAttempts
}

func New(database *sql.DB) (*Queue, error) {
	if database == nil {
		return nil, fmt.Errorf("database is required")
	}
	return &Queue{database: database}, nil
}

func (queue *Queue) Enqueue(ctx context.Context, request EnqueueRequest) error {
	if request.ID == "" || request.QueueName == "" || request.AggregateType == "" || request.AggregateID == "" || request.StageKey == "" {
		return fmt.Errorf("work identity fields are required")
	}
	if request.MaxAttempts < 1 || request.MaxAttempts > 100 || len(request.Payload) == 0 || !json.Valid(request.Payload) {
		return fmt.Errorf("invalid max attempts or payload")
	}
	if request.AvailableAt.IsZero() {
		request.AvailableAt = time.Now().UTC()
	}
	_, err := queue.database.ExecContext(ctx, enqueueSQL,
		request.ID, request.QueueName, request.AggregateType, request.AggregateID,
		request.StageKey, request.Priority, request.AvailableAt.UTC(), request.MaxAttempts, []byte(request.Payload),
	)
	if err != nil {
		return fmt.Errorf("enqueue work item: %w", err)
	}
	return nil
}

func (queue *Queue) Claim(ctx context.Context, queueName, owner string, lease time.Duration) (Item, error) {
	if queueName == "" || owner == "" || lease <= 0 || lease > 10*time.Minute {
		return Item{}, fmt.Errorf("invalid queue, owner, or lease")
	}
	transaction, err := queue.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return Item{}, fmt.Errorf("begin claim transaction: %w", err)
	}
	defer transaction.Rollback()

	item := Item{QueueName: queueName, LeaseOwner: owner}
	var payload []byte
	var oldToken uint64
	if err := transaction.QueryRowContext(ctx, claimSQL, queueName).Scan(
		&item.ID, &item.AggregateType, &item.AggregateID, &item.StageKey, &payload,
		&oldToken, &item.AttemptCount, &item.MaxAttempts,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Item{}, ErrNoWork
		}
		return Item{}, fmt.Errorf("select work item: %w", err)
	}
	if !json.Valid(payload) {
		return Item{}, fmt.Errorf("work item %s has invalid payload JSON", item.ID)
	}
	result, err := transaction.ExecContext(ctx, leaseSQL, owner, durationSeconds(lease), item.ID, oldToken)
	if err != nil {
		return Item{}, fmt.Errorf("lease work item %s: %w", item.ID, err)
	}
	if err := requireOneRow(result); err != nil {
		return Item{}, fmt.Errorf("lease work item %s: %w", item.ID, err)
	}
	if err := transaction.Commit(); err != nil {
		return Item{}, fmt.Errorf("commit work lease: %w", err)
	}
	item.Payload = append([]byte(nil), payload...)
	item.FencingToken = oldToken + 1
	item.AttemptCount++
	return item, nil
}

func (queue *Queue) Heartbeat(ctx context.Context, item Item, lease time.Duration) error {
	if lease <= 0 || lease > 10*time.Minute {
		return fmt.Errorf("invalid lease duration")
	}
	result, err := queue.database.ExecContext(ctx, heartbeatSQL, durationSeconds(lease), item.ID, item.LeaseOwner, item.FencingToken)
	if err != nil {
		return fmt.Errorf("heartbeat work item %s: %w", item.ID, err)
	}
	return checkLeaseResult(result)
}

func (queue *Queue) Complete(ctx context.Context, item Item) error {
	result, err := queue.database.ExecContext(ctx, completeSQL, item.ID, item.LeaseOwner, item.FencingToken)
	if err != nil {
		return fmt.Errorf("complete work item %s: %w", item.ID, err)
	}
	return checkLeaseResult(result)
}

func (queue *Queue) Fail(ctx context.Context, item Item, retryable bool, backoff time.Duration, failure json.RawMessage) error {
	if len(failure) == 0 || !json.Valid(failure) {
		return fmt.Errorf("failure must be valid JSON")
	}
	dead := !retryable || item.AttemptCount >= item.MaxAttempts
	var result sql.Result
	var err error
	if dead {
		result, err = queue.database.ExecContext(ctx, deadSQL, []byte(failure), item.ID, item.LeaseOwner, item.FencingToken)
	} else {
		if backoff < 0 || backoff > 24*time.Hour {
			return fmt.Errorf("invalid retry backoff")
		}
		result, err = queue.database.ExecContext(ctx, retrySQL, backoff.Microseconds(), []byte(failure), item.ID, item.LeaseOwner, item.FencingToken)
	}
	if err != nil {
		return fmt.Errorf("fail work item %s: %w", item.ID, err)
	}
	return checkLeaseResult(result)
}

func checkLeaseResult(result sql.Result) error {
	if err := requireOneRow(result); err != nil {
		return ErrStaleLease
	}
	return nil
}

func requireOneRow(result sql.Result) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("affected rows %d, want 1", rows)
	}
	return nil
}

func durationSeconds(duration time.Duration) int64 {
	seconds := duration / time.Second
	if duration%time.Second != 0 {
		seconds++
	}
	return int64(seconds)
}
