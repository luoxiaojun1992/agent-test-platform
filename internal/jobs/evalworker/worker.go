// Package evalworker claims Test Run evaluation work items and runs the LLM Judge stage.
package evalworker

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"agent-test-platform/internal/jobs/mysqlqueue"
	"agent-test-platform/internal/run"
)

// Queue is the durable queue subset used by the worker.
type Queue interface {
	Claim(ctx context.Context, queueName, owner string, lease time.Duration) (mysqlqueue.Item, error)
	Heartbeat(ctx context.Context, item mysqlqueue.Item, lease time.Duration) error
	Fail(ctx context.Context, item mysqlqueue.Item, retryable bool, backoff time.Duration, failure json.RawMessage) error
}

// Store persists fenced evaluation state transitions.
type Store interface {
	BeginEvaluation(ctx context.Context, runID string, lease run.Lease, node run.NodeRef) (run.EvaluationJob, error)
	SettleEvaluation(ctx context.Context, runID string, lease run.Lease, attemptNo int, result run.EvaluationResult, startedAt time.Time) error
	FailEvaluation(ctx context.Context, runID string, lease run.Lease, failure run.Failure, backoff time.Duration) (bool, error)
}

// Evaluator runs the Judge and aggregates the verdict for one Run.
type Evaluator interface {
	Evaluate(ctx context.Context, job run.EvaluationJob) (run.EvaluationResult, error)
}

// RetryableError marks an Evaluator failure that may succeed on a later attempt.
type RetryableError struct{ Err error }

func (err RetryableError) Error() string   { return err.Err.Error() }
func (err RetryableError) Unwrap() error   { return err.Err }
func (err RetryableError) Retryable() bool { return true }

type Config struct {
	Owner             string
	LeaseDuration     time.Duration
	HeartbeatInterval time.Duration
	PollInterval      time.Duration
	StageTimeout      time.Duration
}

type Worker struct {
	queue     Queue
	store     Store
	evaluator Evaluator
	config    Config
	sleep     func(context.Context, time.Duration) error
}

func New(queue Queue, store Store, evaluator Evaluator, config Config) (*Worker, error) {
	if queue == nil || store == nil || evaluator == nil {
		return nil, errors.New("evaluation worker dependencies are required")
	}
	if strings.TrimSpace(config.Owner) == "" || len(config.Owner) > 128 {
		return nil, errors.New("worker owner must be 1 to 128 bytes")
	}
	if config.LeaseDuration <= 0 || config.LeaseDuration > 10*time.Minute {
		return nil, errors.New("lease duration must be in (0, 10m]")
	}
	if config.HeartbeatInterval <= 0 || config.HeartbeatInterval >= config.LeaseDuration {
		return nil, errors.New("heartbeat interval must be in (0, lease duration)")
	}
	if config.PollInterval <= 0 || config.PollInterval > time.Minute {
		return nil, errors.New("poll interval must be in (0, 1m]")
	}
	if config.StageTimeout < 0 {
		return nil, errors.New("stage timeout must not be negative")
	}
	return &Worker{queue: queue, store: store, evaluator: evaluator, config: config, sleep: sleepContext}, nil
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Run processes work until ctx is cancelled.
func (worker *Worker) Run(ctx context.Context) error {
	for {
		processed, err := worker.ProcessNext(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if !processed {
			if err := worker.sleep(ctx, worker.config.PollInterval); err != nil {
				return nil
			}
		}
	}
}

// ProcessNext claims and processes at most one item.
func (worker *Worker) ProcessNext(ctx context.Context) (bool, error) {
	item, err := worker.queue.Claim(ctx, run.QueueEvaluation, worker.config.Owner, worker.config.LeaseDuration)
	if errors.Is(err, mysqlqueue.ErrNoWork) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim evaluation work: %w", err)
	}
	err = worker.processClaimed(ctx, item)
	if errors.Is(err, mysqlqueue.ErrStaleLease) || errors.Is(err, run.ErrConflict) || errors.Is(err, run.ErrNotFound) {
		return true, nil
	}
	return true, err
}

type payload struct {
	TestRunID string `json:"test_run_id"`
	Iteration int    `json:"iteration"`
	NodeID    string `json:"node_id"`
	TurnNo    int    `json:"turn_no"`
}

func parseItem(item mysqlqueue.Item) (string, run.NodeRef, error) {
	if item.QueueName != run.QueueEvaluation || item.AggregateType != run.AggregateType {
		return "", run.NodeRef{}, errors.New("work item is not a run judge item")
	}
	if item.AttemptCount < 1 || item.MaxAttempts < 1 || item.FencingToken == 0 || strings.TrimSpace(item.LeaseOwner) == "" {
		return "", run.NodeRef{}, errors.New("work item lease is incomplete")
	}
	decoder := json.NewDecoder(strings.NewReader(string(item.Payload)))
	decoder.DisallowUnknownFields()
	var body payload
	if err := decoder.Decode(&body); err != nil {
		return "", run.NodeRef{}, fmt.Errorf("decode payload: %w", err)
	}
	var trailing json.RawMessage
	if decoder.Decode(&trailing) != io.EOF {
		return "", run.NodeRef{}, errors.New("payload has trailing data")
	}
	if body.TestRunID == "" || body.TestRunID != item.AggregateID {
		return "", run.NodeRef{}, errors.New("payload test_run_id does not match aggregate")
	}
	if body.Iteration < 1 || body.TurnNo < 1 {
		return "", run.NodeRef{}, errors.New("payload iteration or turn_no is invalid")
	}
	node := run.NodeRef{NodeID: body.NodeID, Iteration: body.Iteration, TurnNo: body.TurnNo}
	if node.NodeID == "" {
		node.NodeID = "judge-" + strconv.Itoa(body.TurnNo)
	}
	return body.TestRunID, node, nil
}

func leaseOf(item mysqlqueue.Item) run.Lease {
	return run.Lease{WorkItemID: item.ID, Owner: item.LeaseOwner, FencingToken: item.FencingToken, AttemptCount: item.AttemptCount, MaxAttempts: item.MaxAttempts}
}

func failureJSON(failure run.Failure) json.RawMessage {
	data, _ := json.Marshal(failure)
	return data
}

func (worker *Worker) processClaimed(ctx context.Context, item mysqlqueue.Item) error {
	runID, node, err := parseItem(item)
	if err != nil {
		return worker.queue.Fail(ctx, item, false, 0, failureJSON(run.Failure{Code: "INVALID_WORK_ITEM", Message: err.Error()}))
	}
	if item.Exhausted() {
		return worker.queue.Fail(ctx, item, false, 0, failureJSON(run.Failure{Code: "STAGE_ATTEMPTS_EXHAUSTED", Message: "evaluation attempts exhausted"}))
	}
	lease := leaseOf(item)
	processCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if worker.config.StageTimeout > 0 {
		var stop context.CancelFunc
		processCtx, stop = context.WithTimeout(processCtx, worker.config.StageTimeout)
		defer stop()
	}
	heartbeatDone := make(chan error, 1)
	go func() { heartbeatDone <- worker.maintainLease(processCtx, cancel, item) }()

	processErr := worker.process(processCtx, runID, node, lease)
	timedOut := errors.Is(processCtx.Err(), context.DeadlineExceeded)
	cancel()
	heartbeatErr := <-heartbeatDone

	if heartbeatErr != nil {
		return heartbeatErr
	}
	if processErr == nil {
		return nil
	}
	if errors.Is(processErr, mysqlqueue.ErrStaleLease) || errors.Is(processErr, run.ErrConflict) || errors.Is(processErr, run.ErrNotFound) {
		return processErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	failure := run.Failure{Code: "EVALUATION_FAILED", Message: processErr.Error()}
	var retryable interface{ Retryable() bool }
	if errors.As(processErr, &retryable) && retryable.Retryable() {
		failure.Retryable = true
	}
	if timedOut {
		failure = run.Failure{Code: "EVALUATION_STAGE_TIMEOUT", Message: "evaluation stage timed out", Retryable: true}
	}
	_, err = worker.store.FailEvaluation(ctx, runID, lease, failure, retryBackoff(lease))
	return err
}

func (worker *Worker) process(ctx context.Context, runID string, node run.NodeRef, lease run.Lease) error {
	job, err := worker.store.BeginEvaluation(ctx, runID, lease, node)
	if err != nil {
		return err
	}
	startedAt := time.Now().UTC()
	result, err := worker.evaluator.Evaluate(ctx, job)
	if err != nil {
		return err
	}
	return worker.store.SettleEvaluation(ctx, runID, lease, job.AttemptNo, result, startedAt)
}
func (worker *Worker) maintainLease(ctx context.Context, cancel context.CancelFunc, item mysqlqueue.Item) error {
	ticker := time.NewTicker(worker.config.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := worker.queue.Heartbeat(ctx, item, worker.config.LeaseDuration); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				cancel()
				return fmt.Errorf("heartbeat evaluation lease: %w", err)
			}
		}
	}
}

func retryBackoff(lease run.Lease) time.Duration {
	attempt := lease.AttemptCount
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 7 {
		attempt = 7
	}
	base := time.Second << (attempt - 1)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", lease.WorkItemID, lease.AttemptCount)))
	jitter := time.Duration(binary.BigEndian.Uint64(sum[:8]) % uint64(base/4))
	return base + jitter
}
