package runworker

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

func (q *fakeQueue) Claim(context.Context, string, string, time.Duration) (mysqlqueue.Item, error) {
	return q.item, q.claimErr
}
func (q *fakeQueue) Heartbeat(context.Context, mysqlqueue.Item, time.Duration) error { return nil }
func (q *fakeQueue) Fail(_ context.Context, _ mysqlqueue.Item, retryable bool, _ time.Duration, failure json.RawMessage) error {
	q.failed, q.retryable, q.failure = true, retryable, failure
	return nil
}

type fakeStore struct {
	beginErr  error
	running   bool
	settled   *run.Settlement
	failures  []run.Failure
	settleErr error
}

func (s *fakeStore) BeginExecution(context.Context, string, run.Lease, run.NodeRef) (run.Job, error) {
	return run.Job{}, s.beginErr
}
func (s *fakeStore) MarkRunning(context.Context, string, run.Lease) error {
	s.running = true
	return nil
}
func (s *fakeStore) Settle(_ context.Context, _ string, _ run.Lease, settlement run.Settlement) error {
	s.settled = &settlement
	return s.settleErr
}
func (s *fakeStore) FailExecution(_ context.Context, _ string, _ run.Lease, failure run.Failure, _ time.Duration) (bool, error) {
	s.failures = append(s.failures, failure)
	return failure.Retryable, nil
}

type fakeExecutor struct {
	err         error
	callRunning bool
}

func (e fakeExecutor) Execute(ctx context.Context, _ run.Job, markRunning func(context.Context) error) (run.Settlement, error) {
	if e.callRunning {
		if err := markRunning(ctx); err != nil {
			return run.Settlement{}, err
		}
	}
	return run.Settlement{Status: run.StatusEvaluating, Outcome: "EXECUTED"}, e.err
}

func validItem() mysqlqueue.Item {
	return mysqlqueue.Item{ID: "w1", QueueName: run.QueueName, AggregateType: run.AggregateType, AggregateID: "run-1", StageKey: run.StageKey(1, "turn-1"),
		Payload: json.RawMessage(`{"test_run_id":"run-1","iteration":1,"node_id":"turn-1"}`), FencingToken: 1, AttemptCount: 1, MaxAttempts: 3, LeaseOwner: "owner"}
}

func newWorker(t *testing.T, q *fakeQueue, s *fakeStore, e Executor) *Worker {
	t.Helper()
	w, err := New(q, s, e, Config{Owner: "owner", LeaseDuration: time.Minute, HeartbeatInterval: 20 * time.Second, PollInterval: time.Second, StageTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestNewValidatesConfig(t *testing.T) {
	good := Config{Owner: "o", LeaseDuration: time.Minute, HeartbeatInterval: time.Second, PollInterval: time.Second}
	if _, err := New(&fakeQueue{}, &fakeStore{}, fakeExecutor{}, good); err != nil {
		t.Fatal(err)
	}
	if _, err := New(nil, &fakeStore{}, fakeExecutor{}, good); err == nil {
		t.Fatal("nil queue")
	}
	bad := good
	bad.HeartbeatInterval = good.LeaseDuration
	if _, err := New(&fakeQueue{}, &fakeStore{}, fakeExecutor{}, bad); err == nil {
		t.Fatal("heartbeat must be below lease")
	}
	bad = good
	bad.Owner = ""
	if _, err := New(&fakeQueue{}, &fakeStore{}, fakeExecutor{}, bad); err == nil {
		t.Fatal("owner required")
	}
}

func TestProcessNextNoWork(t *testing.T) {
	w := newWorker(t, &fakeQueue{claimErr: mysqlqueue.ErrNoWork}, &fakeStore{}, fakeExecutor{})
	if processed, err := w.ProcessNext(context.Background()); processed || err != nil {
		t.Fatal(processed, err)
	}
}

func TestProcessNextSettlesRun(t *testing.T) {
	store := &fakeStore{}
	w := newWorker(t, &fakeQueue{item: validItem()}, store, fakeExecutor{callRunning: true})
	if processed, err := w.ProcessNext(context.Background()); !processed || err != nil {
		t.Fatal(processed, err)
	}
	if !store.running || store.settled == nil || store.settled.Status != run.StatusEvaluating {
		t.Fatalf("running=%v settled=%+v", store.running, store.settled)
	}
}

func TestProcessNextRetryableAndTerminalFailures(t *testing.T) {
	store := &fakeStore{}
	w := newWorker(t, &fakeQueue{item: validItem()}, store, fakeExecutor{err: RetryableError{Err: errors.New("agent down")}})
	if _, err := w.ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.failures) != 1 || !store.failures[0].Retryable || store.failures[0].Code != "RUN_EXECUTION_FAILED" {
		t.Fatalf("%+v", store.failures)
	}
	store = &fakeStore{}
	w = newWorker(t, &fakeQueue{item: validItem()}, store, fakeExecutor{err: errors.New("bad")})
	_, _ = w.ProcessNext(context.Background())
	if len(store.failures) != 1 || store.failures[0].Retryable {
		t.Fatalf("%+v", store.failures)
	}
}

func TestProcessNextIgnoresStaleAndConflict(t *testing.T) {
	for _, begin := range []error{mysqlqueue.ErrStaleLease, run.ErrConflict, run.ErrNotFound} {
		store := &fakeStore{beginErr: begin}
		w := newWorker(t, &fakeQueue{item: validItem()}, store, fakeExecutor{})
		if processed, err := w.ProcessNext(context.Background()); !processed || err != nil {
			t.Fatalf("%v: %v %v", begin, processed, err)
		}
		if len(store.failures) != 0 {
			t.Fatal("must not record failure")
		}
	}
}

func TestInvalidWorkItemFailsTerminally(t *testing.T) {
	item := validItem()
	item.Payload = json.RawMessage(`{"test_run_id":"other"}`)
	queue := &fakeQueue{item: item}
	w := newWorker(t, queue, &fakeStore{}, fakeExecutor{})
	if _, err := w.ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !queue.failed || queue.retryable {
		t.Fatal("expected terminal queue failure")
	}
	item = validItem()
	item.Payload = json.RawMessage(`{"test_run_id":"run-1","iteration":1,"node_id":"bogus"}`)
	queue = &fakeQueue{item: item}
	_, _ = newWorker(t, queue, &fakeStore{}, fakeExecutor{}).ProcessNext(context.Background())
	if !queue.failed {
		t.Fatal("unknown node_id must fail")
	}
}

func TestExhaustedItemFails(t *testing.T) {
	item := validItem()
	item.AttemptCount, item.MaxAttempts = 4, 3
	queue := &fakeQueue{item: item}
	_, _ = newWorker(t, queue, &fakeStore{}, fakeExecutor{}).ProcessNext(context.Background())
	if !queue.failed || queue.retryable {
		t.Fatal("exhausted item must fail terminally")
	}
}

func TestRetryBackoffGrowsAndIsBounded(t *testing.T) {
	first := retryBackoff(run.Lease{WorkItemID: "w", AttemptCount: 1})
	second := retryBackoff(run.Lease{WorkItemID: "w", AttemptCount: 2})
	capped := retryBackoff(run.Lease{WorkItemID: "w", AttemptCount: 50})
	if first < time.Second || second < 2*time.Second || capped >= 90*time.Second || retryBackoff(run.Lease{AttemptCount: 0}) < time.Second {
		t.Fatal(first, second, capped)
	}
}
