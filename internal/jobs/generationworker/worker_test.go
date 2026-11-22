package generationworker

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"agent-test-platform/internal/generation"
	"agent-test-platform/internal/jobs/mysqlqueue"
)

const workerJobID = "01K4Z6Y7X8W9V0T1S2R3Q4P5N6"

type fakeQueue struct {
	item           mysqlqueue.Item
	claimErr       error
	heartbeatErr   error
	heartbeatCount int
	failed         bool
	mu             sync.Mutex
}

func (queue *fakeQueue) Claim(context.Context, string, string, time.Duration) (mysqlqueue.Item, error) {
	return queue.item, queue.claimErr
}
func (queue *fakeQueue) Heartbeat(context.Context, mysqlqueue.Item, time.Duration) error {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	queue.heartbeatCount++
	return queue.heartbeatErr
}
func (queue *fakeQueue) Fail(context.Context, mysqlqueue.Item, bool, time.Duration, json.RawMessage) error {
	queue.failed = true
	return nil
}

type fakeStore struct {
	advanced     bool
	failed       bool
	jobID        string
	lease        generation.WorkLease
	transition   generation.TransitionInput
	failure      generation.JobFailure
	backoff      time.Duration
	published    bool
	publishJobID string
	publishLease generation.WorkLease
	publishErr   error
	budget       time.Duration
	bounded      bool
}

func (store *fakeStore) LoadGenerationBudget(context.Context, string) (time.Duration, bool, error) {
	return store.budget, store.bounded, nil
}

func (store *fakeStore) AdvanceGeneration(_ context.Context, jobID string, lease generation.WorkLease, input generation.TransitionInput) (generation.JobState, error) {
	store.advanced, store.jobID, store.lease, store.transition = true, jobID, lease, input
	return generation.JobState{Status: generation.StatusValidatingInput, RequestedTestCaseCount: 1}, nil
}
func (store *fakeStore) FailGeneration(_ context.Context, jobID string, lease generation.WorkLease, failure generation.JobFailure, backoff time.Duration) (bool, error) {
	store.failed, store.jobID, store.lease, store.failure, store.backoff = true, jobID, lease, failure, backoff
	return !failure.Retryable || lease.AttemptCount >= lease.MaxAttempts, nil
}
func (store *fakeStore) PublishGeneration(_ context.Context, jobID string, lease generation.WorkLease) (generation.Job, error) {
	store.published, store.publishJobID, store.publishLease = true, jobID, lease
	if store.publishErr != nil {
		return generation.Job{}, store.publishErr
	}
	return generation.Job{ID: jobID, Kind: generation.KindScenario, Status: generation.StatusCompleted}, nil
}

type processorFunc func(context.Context, StageRequest) (StageResult, error)

func (function processorFunc) Process(ctx context.Context, request StageRequest) (StageResult, error) {
	return function(ctx, request)
}

func TestProcessNextAdvancesSuccessfulStage(t *testing.T) {
	t.Parallel()

	queue := &fakeQueue{item: validItem()}
	store := &fakeStore{}
	processor := processorFunc(func(_ context.Context, request StageRequest) (StageResult, error) {
		if request.Stage != generation.StatusPending || request.JobID != workerJobID || request.Attempt != 1 {
			t.Fatalf("request = %#v", request)
		}
		return StageResult{Transition: generation.TransitionInput{Event: generation.EventInputAccepted}}, nil
	})
	worker := newTestWorker(t, queue, store, processor, time.Second)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("processed=%v error=%v", processed, err)
	}
	if !store.advanced || store.failed || store.transition.Event != generation.EventInputAccepted {
		t.Fatalf("store = %#v", store)
	}
}

func TestProcessNextPublishesDirectlyWithoutCallingProcessor(t *testing.T) {
	t.Parallel()

	item := validItem()
	item.StageKey = "PUBLISHING:repair-0"
	queue := &fakeQueue{item: item}
	store := &fakeStore{}
	calledProcessor := false
	processor := processorFunc(func(context.Context, StageRequest) (StageResult, error) {
		calledProcessor = true
		return StageResult{}, nil
	})
	worker := newTestWorker(t, queue, store, processor, time.Second)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("processed=%v error=%v", processed, err)
	}
	if calledProcessor {
		t.Fatal("PUBLISHING should not go through the generic Processor")
	}
	if !store.published || store.publishJobID != workerJobID || store.advanced || store.failed {
		t.Fatalf("store = %#v", store)
	}
}

func TestProcessNextPublishFailureRetriesStage(t *testing.T) {
	t.Parallel()

	item := validItem()
	item.StageKey = "PUBLISHING:repair-0"
	queue := &fakeQueue{item: item}
	store := &fakeStore{publishErr: errors.New("draft artifact missing")}
	worker := newTestWorker(t, queue, store, processorFunc(func(context.Context, StageRequest) (StageResult, error) {
		t.Fatal("Process should not be called for PUBLISHING")
		return StageResult{}, nil
	}), time.Second)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("processed=%v error=%v", processed, err)
	}
	if !store.published || !store.failed || store.failure.Code != "PUBLISH_FAILED" || !store.failure.Retryable {
		t.Fatalf("store = %#v", store)
	}
}

func TestProcessNextPublishStaleLeaseIsHandledGracefully(t *testing.T) {
	t.Parallel()

	item := validItem()
	item.StageKey = "PUBLISHING:repair-0"
	queue := &fakeQueue{item: item}
	store := &fakeStore{publishErr: mysqlqueue.ErrStaleLease}
	worker := newTestWorker(t, queue, store, processorFunc(func(context.Context, StageRequest) (StageResult, error) {
		t.Fatal("Process should not be called for PUBLISHING")
		return StageResult{}, nil
	}), time.Second)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("processed=%v error=%v", processed, err)
	}
	if store.failed {
		t.Fatal("a stale lease should not be recorded as a stage failure")
	}
}

func TestProcessNextPersistsStageFailure(t *testing.T) {
	t.Parallel()

	queue := &fakeQueue{item: validItem()}
	store := &fakeStore{}
	failure := generation.JobFailure{Code: "PROVIDER_UNAVAILABLE", Message: "Provider is temporarily unavailable.", Retryable: true}
	processor := processorFunc(func(context.Context, StageRequest) (StageResult, error) {
		return StageResult{Failure: &failure}, nil
	})
	worker := newTestWorker(t, queue, store, processor, time.Second)

	if processed, err := worker.ProcessNext(context.Background()); err != nil || !processed {
		t.Fatalf("processed=%v error=%v", processed, err)
	}
	if !store.failed || store.advanced || store.failure.Code != failure.Code || store.backoff < time.Second {
		t.Fatalf("store = %#v", store)
	}
}

func TestProcessNextRejectsConflictingStageResult(t *testing.T) {
	t.Parallel()

	queue := &fakeQueue{item: validItem()}
	store := &fakeStore{}
	failure := generation.JobFailure{Code: "PROVIDER_UNAVAILABLE", Message: "Provider is temporarily unavailable.", Retryable: true}
	processor := processorFunc(func(context.Context, StageRequest) (StageResult, error) {
		return StageResult{
			Transition: generation.TransitionInput{Event: generation.EventInputAccepted},
			Failure:    &failure,
		}, nil
	})
	worker := newTestWorker(t, queue, store, processor, time.Second)

	if _, err := worker.ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !store.failed || store.failure.Code != "STAGE_RESULT_INVALID" || store.failure.Retryable {
		t.Fatalf("store = %#v", store)
	}
}

func TestProcessNextDeadLettersInvalidAggregate(t *testing.T) {
	t.Parallel()

	item := validItem()
	item.AggregateID = "not-a-ulid"
	queue := &fakeQueue{item: item}
	store := &fakeStore{}
	worker := newTestWorker(t, queue, store, processorFunc(func(context.Context, StageRequest) (StageResult, error) {
		t.Fatal("processor called for invalid aggregate")
		return StageResult{}, nil
	}), time.Second)

	if _, err := worker.ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !queue.failed || store.failed || store.advanced {
		t.Fatalf("queue failed=%v store=%#v", queue.failed, store)
	}
}

func TestProcessNextFailsExhaustedLeaseWithoutCallingProcessor(t *testing.T) {
	t.Parallel()

	item := validItem()
	item.AttemptCount = 4
	item.MaxAttempts = 3
	queue := &fakeQueue{item: item}
	store := &fakeStore{}
	processor := processorFunc(func(context.Context, StageRequest) (StageResult, error) {
		t.Fatal("processor called for exhausted lease")
		return StageResult{}, nil
	})
	worker := newTestWorker(t, queue, store, processor, time.Second)

	if _, err := worker.ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !store.failed || store.failure.Code != "STAGE_ATTEMPTS_EXHAUSTED" || store.failure.Retryable {
		t.Fatalf("store = %#v", store)
	}
}

func TestProcessNextStopsOnStaleHeartbeat(t *testing.T) {
	t.Parallel()

	queue := &fakeQueue{item: validItem(), heartbeatErr: mysqlqueue.ErrStaleLease}
	store := &fakeStore{}
	processorCancelled := make(chan struct{})
	processor := processorFunc(func(ctx context.Context, _ StageRequest) (StageResult, error) {
		<-ctx.Done()
		close(processorCancelled)
		return StageResult{}, ctx.Err()
	})
	worker := newTestWorker(t, queue, store, processor, time.Millisecond)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("processed=%v error=%v", processed, err)
	}
	select {
	case <-processorCancelled:
	default:
		t.Fatal("processor was not cancelled")
	}
	if store.advanced || store.failed || queue.heartbeatCount == 0 {
		t.Fatalf("store=%#v heartbeats=%d", store, queue.heartbeatCount)
	}
}

func TestProcessNextReturnsNoWork(t *testing.T) {
	t.Parallel()

	queue := &fakeQueue{claimErr: mysqlqueue.ErrNoWork}
	worker := newTestWorker(t, queue, &fakeStore{}, processorFunc(func(context.Context, StageRequest) (StageResult, error) {
		return StageResult{}, errors.New("should not run")
	}), time.Second)
	processed, err := worker.ProcessNext(context.Background())
	if err != nil || processed {
		t.Fatalf("processed=%v error=%v", processed, err)
	}
}

func TestProcessNextEnforcesDatasetStageTimeout(t *testing.T) {
	t.Parallel()

	item := validItem()
	item.StageKey = "GENERATING_DATASET:repair-0"
	queue := &fakeQueue{item: item}
	store := &fakeStore{}
	processor := processorFunc(func(ctx context.Context, _ StageRequest) (StageResult, error) {
		<-ctx.Done()
		return StageResult{}, ctx.Err()
	})
	worker, err := New(queue, store, processor, Config{
		Owner: "worker-1", LeaseDuration: time.Minute, HeartbeatInterval: time.Second,
		PollInterval: time.Millisecond, StageTimeouts: map[generation.JobStatus]time.Duration{
			generation.StatusGeneratingDataset: time.Millisecond,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if processed, err := worker.ProcessNext(context.Background()); err != nil || !processed {
		t.Fatalf("processed=%v error=%v", processed, err)
	}
	if !store.failed || store.failure.Code != "DATASET_CREATION_TIMEOUT" || !store.failure.Retryable {
		t.Fatalf("store = %#v", store)
	}
}

func TestProcessNextEnforcesDatasetValidationTimeout(t *testing.T) {
	t.Parallel()

	item := validItem()
	item.StageKey = "VALIDATING_DATASET:repair-0"
	queue := &fakeQueue{item: item}
	store := &fakeStore{}
	processor := processorFunc(func(ctx context.Context, _ StageRequest) (StageResult, error) {
		<-ctx.Done()
		return StageResult{}, ctx.Err()
	})
	worker, err := New(queue, store, processor, Config{
		Owner: "worker-1", LeaseDuration: time.Minute, HeartbeatInterval: time.Second,
		PollInterval: time.Millisecond, StageTimeouts: map[generation.JobStatus]time.Duration{
			generation.StatusValidatingDataset: time.Millisecond,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if processed, err := worker.ProcessNext(context.Background()); err != nil || !processed {
		t.Fatalf("processed=%v error=%v", processed, err)
	}
	if !store.failed || store.failure.Code != "DATASET_VALIDATION_TIMEOUT" || !store.failure.Retryable {
		t.Fatalf("store = %#v", store)
	}
}

// One Dataset attempt is a generation stage plus a validation stage. With a minute to publish, it
// must fit the 10-minute Generation Job deadline (docs/technical/01-storage-and-workflows.md, section 3).
func TestDefaultDatasetStagesFitTheGenerationJobDeadline(t *testing.T) {
	t.Parallel()

	const generationJobDeadline, publishReserve = 10 * time.Minute, time.Minute
	timeouts := DefaultStageTimeouts()
	attempt := timeouts[generation.StatusGeneratingDataset] + timeouts[generation.StatusValidatingDataset]
	if attempt+publishReserve > generationJobDeadline {
		t.Fatalf("one Dataset attempt takes %s, which leaves no room to publish within %s", attempt, generationJobDeadline)
	}
}

func TestProcessNextFailsJobWhoseBudgetIsSpentWithoutRunningStage(t *testing.T) {
	t.Parallel()

	item := validItem()
	item.StageKey = "GENERATING_DATASET:repair-0"
	queue := &fakeQueue{item: item}
	store := &fakeStore{bounded: true, budget: 0}
	ran := false
	processor := processorFunc(func(context.Context, StageRequest) (StageResult, error) {
		ran = true
		return StageResult{}, nil
	})
	worker := newTestWorker(t, queue, store, processor, time.Second)
	if processed, err := worker.ProcessNext(context.Background()); err != nil || !processed {
		t.Fatalf("processed=%v error=%v", processed, err)
	}
	if ran {
		t.Fatal("stage ran after its Job budget was spent")
	}
	if !store.failed || store.failure.Code != "GENERATION_JOB_DEADLINE_EXCEEDED" || store.failure.Retryable {
		t.Fatalf("store = %#v", store)
	}
}

// The Stage timeout is 1 minute, but the Job has 20 milliseconds left, so the Job budget is the
// limit that fires. It is not a retryable stage timeout.
func TestProcessNextBoundsStageByRemainingJobBudget(t *testing.T) {
	t.Parallel()

	item := validItem()
	item.StageKey = "GENERATING_DATASET:repair-0"
	queue := &fakeQueue{item: item}
	store := &fakeStore{bounded: true, budget: 20 * time.Millisecond}
	processor := processorFunc(func(ctx context.Context, _ StageRequest) (StageResult, error) {
		<-ctx.Done()
		return StageResult{}, ctx.Err()
	})
	worker, err := New(queue, store, processor, Config{
		Owner: "worker-1", LeaseDuration: time.Minute, HeartbeatInterval: time.Second, PollInterval: time.Millisecond,
		StageTimeouts: map[generation.JobStatus]time.Duration{generation.StatusGeneratingDataset: time.Minute},
	})
	if err != nil {
		t.Fatal(err)
	}
	if processed, err := worker.ProcessNext(context.Background()); err != nil || !processed {
		t.Fatalf("processed=%v error=%v", processed, err)
	}
	if !store.failed || store.failure.Code != "GENERATION_JOB_DEADLINE_EXCEEDED" || store.failure.Retryable {
		t.Fatalf("store = %#v", store)
	}
}

// A stage that fails must still release its context, or every stage leaves a child behind on the
// worker's long-lived root context.
func TestProcessNextReleasesStageContext(t *testing.T) {
	t.Parallel()

	queue := &fakeQueue{item: validItem()}
	store := &fakeStore{}
	var stageCtx context.Context
	processor := processorFunc(func(ctx context.Context, _ StageRequest) (StageResult, error) {
		stageCtx = ctx
		return StageResult{}, errors.New("stage failed")
	})
	worker := newTestWorker(t, queue, store, processor, time.Second)
	if _, err := worker.ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stageCtx == nil || stageCtx.Err() == nil {
		t.Fatal("stage context was not released after the stage returned")
	}
}

func TestProcessNextReportsJobBudgetWhenStageReturnsTimeoutFailure(t *testing.T) {
	t.Parallel()

	item := validItem()
	item.StageKey = "GENERATING_DATASET:repair-0"
	queue := &fakeQueue{item: item}
	store := &fakeStore{bounded: true, budget: 20 * time.Millisecond}
	processor := processorFunc(func(ctx context.Context, _ StageRequest) (StageResult, error) {
		<-ctx.Done()
		return StageResult{Failure: &generation.JobFailure{
			Code: "DATASET_CREATION_TIMEOUT", Message: "Dataset creation exceeded its Stage deadline.", Retryable: true,
		}}, nil
	})
	worker := newTestWorker(t, queue, store, processor, time.Second)
	if processed, err := worker.ProcessNext(context.Background()); err != nil || !processed {
		t.Fatalf("processed=%v error=%v", processed, err)
	}
	if !store.failed || store.failure.Code != "GENERATION_JOB_DEADLINE_EXCEEDED" || store.failure.Retryable {
		t.Fatalf("store = %#v, want the Job budget reported instead of the stage timeout", store)
	}
}

// Approval after the Job's budget ran out leaves the publish work queued. The Worker must fail the
// Job instead of publishing it.
func TestProcessNextFailsPublishingJobWhoseBudgetIsSpent(t *testing.T) {
	t.Parallel()

	item := validItem()
	item.StageKey = "PUBLISHING:repair-0"
	queue := &fakeQueue{item: item}
	store := &fakeStore{bounded: true, budget: 0}
	ran := false
	processor := processorFunc(func(context.Context, StageRequest) (StageResult, error) {
		ran = true
		return StageResult{}, nil
	})
	worker := newTestWorker(t, queue, store, processor, time.Second)
	if processed, err := worker.ProcessNext(context.Background()); err != nil || !processed {
		t.Fatalf("processed=%v error=%v", processed, err)
	}
	if ran || store.published {
		t.Fatalf("a Job whose budget was spent was published (ran=%v published=%v)", ran, store.published)
	}
	if !store.failed || store.failure.Code != "GENERATION_JOB_DEADLINE_EXCEEDED" {
		t.Fatalf("store = %#v", store)
	}
}

func validItem() mysqlqueue.Item {
	return mysqlqueue.Item{
		ID: "work-1", QueueName: QueueName, AggregateType: aggregateType, AggregateID: workerJobID,
		StageKey: "PENDING:repair-0", Payload: json.RawMessage(`{"job_id":"` + workerJobID + `"}`),
		FencingToken: 1, AttemptCount: 1, MaxAttempts: 3, LeaseOwner: "worker-1",
	}
}

func newTestWorker(t *testing.T, queue Queue, store Store, processor StageProcessor, heartbeat time.Duration) *Worker {
	t.Helper()
	worker, err := New(queue, store, processor, Config{
		Owner: "worker-1", LeaseDuration: time.Minute, HeartbeatInterval: heartbeat, PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return worker
}
