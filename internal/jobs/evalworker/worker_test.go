package evalworker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"agent-test-platform/internal/jobs/mysqlqueue"
	"agent-test-platform/internal/run"
)

type fakeQueue struct {
	item      mysqlqueue.Item
	claimErr  error
	failed    bool
	retryable bool
	failure   json.RawMessage
}

func (queue *fakeQueue) Claim(context.Context, string, string, time.Duration) (mysqlqueue.Item, error) {
	return queue.item, queue.claimErr
}
func (queue *fakeQueue) Heartbeat(context.Context, mysqlqueue.Item, time.Duration) error { return nil }
func (queue *fakeQueue) Fail(_ context.Context, _ mysqlqueue.Item, retryable bool, _ time.Duration, failure json.RawMessage) error {
	queue.failed, queue.retryable, queue.failure = true, retryable, failure
	return nil
}

type fakeStore struct {
	beginErr  error
	settled   bool
	failures  []run.Failure
	attemptNo int
}

func (store *fakeStore) BeginEvaluation(context.Context, string, run.Lease, run.NodeRef) (run.EvaluationJob, error) {
	return run.EvaluationJob{RunID: "run-1", AttemptNo: 2}, store.beginErr
}
func (store *fakeStore) SettleEvaluation(_ context.Context, _ string, _ run.Lease, attemptNo int, _ run.EvaluationResult, _ time.Time) error {
	store.settled, store.attemptNo = true, attemptNo
	return nil
}
func (store *fakeStore) FailEvaluation(_ context.Context, _ string, _ run.Lease, failure run.Failure, _ time.Duration) (bool, error) {
	store.failures = append(store.failures, failure)
	return failure.Retryable, nil
}

type fakeEvaluator struct{ err error }

func (evaluator fakeEvaluator) Evaluate(context.Context, run.EvaluationJob) (run.EvaluationResult, error) {
	return run.EvaluationResult{Pass: true}, evaluator.err
}

func validItem() mysqlqueue.Item {
	return mysqlqueue.Item{ID: "w1", QueueName: run.QueueEvaluation, AggregateType: run.AggregateType, AggregateID: "run-1", StageKey: run.StageKey(1, "judge-1"),
		Payload: json.RawMessage(`{"test_run_id":"run-1","iteration":1,"node_id":"judge-1","turn_no":1}`), FencingToken: 1, AttemptCount: 1, MaxAttempts: 3, LeaseOwner: "owner"}
}

func newWorker(t *testing.T, queue *fakeQueue, store *fakeStore, evaluator Evaluator) *Worker {
	t.Helper()
	worker, err := New(queue, store, evaluator, Config{Owner: "owner", LeaseDuration: time.Minute, HeartbeatInterval: 20 * time.Second, PollInterval: time.Second, StageTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

func TestProcessNextSettlesEvaluation(t *testing.T) {
	queue, store := &fakeQueue{item: validItem()}, &fakeStore{}
	processed, err := newWorker(t, queue, store, fakeEvaluator{}).ProcessNext(context.Background())
	if err != nil || !processed || !store.settled || store.attemptNo != 2 {
		t.Fatalf("processed=%v err=%v settled=%v attempt=%d", processed, err, store.settled, store.attemptNo)
	}
}

func TestProcessNextNoWork(t *testing.T) {
	queue := &fakeQueue{claimErr: mysqlqueue.ErrNoWork}
	processed, err := newWorker(t, queue, &fakeStore{}, fakeEvaluator{}).ProcessNext(context.Background())
	if err != nil || processed {
		t.Fatalf("processed=%v err=%v", processed, err)
	}
}

func TestRetryableAndTerminalFailures(t *testing.T) {
	for _, test := range []struct {
		name      string
		err       error
		retryable bool
	}{
		{"retryable", RetryableError{Err: errors.New("judge down")}, true},
		{"terminal", errors.New("contract violated"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{}
			if _, err := newWorker(t, &fakeQueue{item: validItem()}, store, fakeEvaluator{err: test.err}).ProcessNext(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(store.failures) != 1 || store.failures[0].Retryable != test.retryable || store.failures[0].Code != "EVALUATION_FAILED" || store.settled {
				t.Fatalf("unexpected failures: %+v settled=%v", store.failures, store.settled)
			}
		})
	}
}

func TestStaleStateIsNotFatal(t *testing.T) {
	for _, beginErr := range []error{run.ErrConflict, run.ErrNotFound, mysqlqueue.ErrStaleLease} {
		store := &fakeStore{beginErr: beginErr}
		processed, err := newWorker(t, &fakeQueue{item: validItem()}, store, fakeEvaluator{}).ProcessNext(context.Background())
		if err != nil || !processed || len(store.failures) != 0 {
			t.Fatalf("%v: processed=%v err=%v failures=%v", beginErr, processed, err, store.failures)
		}
	}
}

func TestInvalidItemFailsPermanently(t *testing.T) {
	item := validItem()
	item.Payload = json.RawMessage(`{"test_run_id":"other"}`)
	queue := &fakeQueue{item: item}
	if _, err := newWorker(t, queue, &fakeStore{}, fakeEvaluator{}).ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !queue.failed || queue.retryable {
		t.Fatalf("failed=%v retryable=%v", queue.failed, queue.retryable)
	}
}

func TestExhaustedItemFailsPermanently(t *testing.T) {
	item := validItem()
	item.AttemptCount = 4
	queue := &fakeQueue{item: item}
	if _, err := newWorker(t, queue, &fakeStore{}, fakeEvaluator{}).ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !queue.failed || queue.retryable {
		t.Fatalf("failed=%v retryable=%v", queue.failed, queue.retryable)
	}
}
