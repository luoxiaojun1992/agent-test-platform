package mysqlqueue

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestEnqueue(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	queue, _ := New(database)
	available := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	mock.ExpectExec(regexp.QuoteMeta(enqueueSQL)).WithArgs("work-1", "generation", "generation_job", "job-1", "VALIDATING_INPUT:0", 5, available, 3, []byte(`{"job_id":"job-1"}`)).WillReturnResult(sqlmock.NewResult(1, 1))
	if err := queue.Enqueue(context.Background(), EnqueueRequest{ID: "work-1", QueueName: "generation", AggregateType: "generation_job", AggregateID: "job-1", StageKey: "VALIDATING_INPUT:0", Priority: 5, AvailableAt: available, MaxAttempts: 3, Payload: json.RawMessage(`{"job_id":"job-1"}`)}); err != nil {
		t.Fatal(err)
	}
}

func TestClaim(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	queue, _ := New(database)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(claimSQL)).WithArgs("generation").WillReturnRows(sqlmock.NewRows([]string{"id", "aggregate_type", "aggregate_id", "stage_key", "payload_json", "fencing_token", "attempt_count", "max_attempts"}).AddRow("work-1", "generation_job", "job-1", "stage:0", []byte(`{"job_id":"job-1"}`), uint64(4), 1, 3))
	mock.ExpectExec(regexp.QuoteMeta(leaseSQL)).WithArgs("worker-1", int64(60), "work-1", uint64(4)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	item, err := queue.Claim(context.Background(), "generation", "worker-1", 60*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if item.FencingToken != 5 || item.AttemptCount != 2 || string(item.Payload) != `{"job_id":"job-1"}` {
		t.Fatalf("item = %#v", item)
	}
}

func TestClaimNoWork(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	queue, _ := New(database)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(claimSQL)).WithArgs("generation").WillReturnRows(sqlmock.NewRows([]string{"id", "aggregate_type", "aggregate_id", "stage_key", "payload_json", "fencing_token", "attempt_count", "max_attempts"}))
	mock.ExpectRollback()
	if _, err := queue.Claim(context.Background(), "generation", "worker", time.Minute); !errors.Is(err, ErrNoWork) {
		t.Fatalf("error = %v", err)
	}
}

func TestClaimReturnsExpiredExhaustedLeaseForAggregateCleanup(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	queue, _ := New(database)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(claimSQL)).WithArgs("generation").WillReturnRows(sqlmock.NewRows([]string{"id", "aggregate_type", "aggregate_id", "stage_key", "payload_json", "fencing_token", "attempt_count", "max_attempts"}).AddRow("work-1", "generation_job", "job-1", "stage:0", []byte(`{"job_id":"job-1"}`), uint64(4), 3, 3))
	mock.ExpectExec(regexp.QuoteMeta(leaseSQL)).WithArgs("worker-1", int64(60), "work-1", uint64(4)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	item, err := queue.Claim(context.Background(), "generation", "worker-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !item.Exhausted() || item.AttemptCount != 4 {
		t.Fatalf("item = %#v", item)
	}
}

func TestHeartbeatCompleteAndStaleLease(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	queue, _ := New(database)
	item := Item{ID: "work-1", LeaseOwner: "worker", FencingToken: 2}
	mock.ExpectExec(regexp.QuoteMeta(heartbeatSQL)).WithArgs(int64(61), "work-1", "worker", uint64(2)).WillReturnResult(sqlmock.NewResult(0, 1))
	if err := queue.Heartbeat(context.Background(), item, 60*time.Second+time.Millisecond); err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec(regexp.QuoteMeta(completeSQL)).WithArgs("work-1", "worker", uint64(2)).WillReturnResult(sqlmock.NewResult(0, 0))
	if err := queue.Complete(context.Background(), item); !errors.Is(err, ErrStaleLease) {
		t.Fatalf("error = %v", err)
	}
}

func TestFailRetriesOrDeadLetters(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	queue, _ := New(database)
	failure := json.RawMessage(`{"code":"TEMPORARY"}`)
	item := Item{ID: "work-1", LeaseOwner: "worker", FencingToken: 3, AttemptCount: 1, MaxAttempts: 3}
	mock.ExpectExec(regexp.QuoteMeta(retrySQL)).WithArgs(int64(time.Second.Microseconds()), []byte(failure), "work-1", "worker", uint64(3)).WillReturnResult(sqlmock.NewResult(0, 1))
	if err := queue.Fail(context.Background(), item, true, time.Second, failure); err != nil {
		t.Fatal(err)
	}
	item.AttemptCount = 3
	mock.ExpectExec(regexp.QuoteMeta(deadSQL)).WithArgs([]byte(failure), "work-1", "worker", uint64(3)).WillReturnResult(sqlmock.NewResult(0, 1))
	if err := queue.Fail(context.Background(), item, true, 0, failure); err != nil {
		t.Fatal(err)
	}
}
