package generationworker

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"agent-test-platform/internal/generation"
	"agent-test-platform/internal/jobs/mysqlqueue"

	"github.com/oklog/ulid/v2"
)

const (
	QueueName     = "generation"
	aggregateType = "generation_job"
)

type Queue interface {
	Claim(context.Context, string, string, time.Duration) (mysqlqueue.Item, error)
	Heartbeat(context.Context, mysqlqueue.Item, time.Duration) error
	Fail(context.Context, mysqlqueue.Item, bool, time.Duration, json.RawMessage) error
}

type Store interface {
	AdvanceGeneration(context.Context, string, generation.WorkLease, generation.TransitionInput) (generation.JobState, error)
	FailGeneration(context.Context, string, generation.WorkLease, generation.JobFailure, time.Duration) (bool, error)
	PublishGeneration(context.Context, string, generation.WorkLease) (generation.Job, error)
	LoadGenerationBudget(context.Context, string) (time.Duration, bool, error)
}

type StageRequest = generation.StageRequest
type StageResult = generation.StageResult
type StageProcessor = generation.StageProcessor

type Config struct {
	Owner             string
	LeaseDuration     time.Duration
	HeartbeatInterval time.Duration
	PollInterval      time.Duration
	StageTimeouts     map[generation.JobStatus]time.Duration
}

// The validation budget covers a Semantic Critic review of at most 24 model calls. With the
// 3-minute generation stage, the 10-minute Generation Job deadline keeps a minute to publish.
func DefaultStageTimeouts() map[generation.JobStatus]time.Duration {
	return map[generation.JobStatus]time.Duration{
		generation.StatusGeneratingDataset: 3 * time.Minute,
		generation.StatusValidatingDataset: 6 * time.Minute,
	}
}

type Worker struct {
	queue     Queue
	store     Store
	processor StageProcessor
	config    Config
}

func New(queue Queue, store Store, processor StageProcessor, config Config) (*Worker, error) {
	if queue == nil || store == nil || processor == nil {
		return nil, fmt.Errorf("queue, store, and stage processor are required")
	}
	if strings.TrimSpace(config.Owner) == "" || len(config.Owner) > 128 {
		return nil, fmt.Errorf("worker owner is required and must not exceed 128 bytes")
	}
	if config.LeaseDuration <= 0 || config.LeaseDuration > 10*time.Minute {
		return nil, fmt.Errorf("lease duration outside (0,10m]")
	}
	if config.HeartbeatInterval <= 0 || config.HeartbeatInterval >= config.LeaseDuration {
		return nil, fmt.Errorf("heartbeat interval must be positive and shorter than the lease")
	}
	if config.PollInterval <= 0 || config.PollInterval > time.Minute {
		return nil, fmt.Errorf("poll interval outside (0,1m]")
	}
	for stage, timeout := range config.StageTimeouts {
		if !stage.Valid() || timeout <= 0 || timeout > 10*time.Minute {
			return nil, fmt.Errorf("invalid timeout for Generation stage %s", stage)
		}
	}
	return &Worker{queue: queue, store: store, processor: processor, config: config}, nil
}

func (worker *Worker) Run(ctx context.Context) error {
	for {
		processed, err := worker.ProcessNext(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if processed {
			continue
		}
		if err := sleep(ctx, worker.config.PollInterval); err != nil {
			return nil
		}
	}
}

// ProcessNext claims and settles at most one Generation work item.
func (worker *Worker) ProcessNext(ctx context.Context) (bool, error) {
	item, err := worker.queue.Claim(ctx, QueueName, worker.config.Owner, worker.config.LeaseDuration)
	if errors.Is(err, mysqlqueue.ErrNoWork) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim generation work: %w", err)
	}
	if err := worker.processClaimed(ctx, item); err != nil {
		if errors.Is(err, mysqlqueue.ErrStaleLease) || errors.Is(err, generation.ErrConflict) {
			return true, nil
		}
		return true, err
	}
	return true, nil
}

func (worker *Worker) processClaimed(ctx context.Context, item mysqlqueue.Item) error {
	request, err := stageRequest(item)
	if err != nil {
		return worker.rejectInvalidItem(ctx, item, err)
	}
	if item.Exhausted() {
		failure := generation.JobFailure{
			Code: "STAGE_ATTEMPTS_EXHAUSTED", Message: "Generation stage attempts were exhausted.", Retryable: false,
		}
		return worker.failGeneration(ctx, request, failure)
	}
	budget, bounded, err := worker.store.LoadGenerationBudget(ctx, request.JobID)
	if err != nil {
		return fmt.Errorf("load budget of generation job %s: %w", request.JobID, err)
	}
	if bounded && budget <= 0 {
		return worker.failGeneration(ctx, request, jobBudgetFailure())
	}
	// PUBLISHING inserts logical+revision rows and completes the Job in one Platform MySQL
	// transaction (docs/technical/01-storage-and-workflows.md section 6); it does not go
	// through the generic Process()-then-AdvanceGeneration() path every other stage uses.
	if request.Stage == generation.StatusPublishing {
		return worker.publish(ctx, request)
	}

	timeout, budgetBound := stageTimeout(worker.config.StageTimeouts[request.Stage], budget, bounded)
	result, processErr, heartbeatErr, expired := worker.processWithHeartbeat(ctx, item, request, timeout)
	if heartbeatErr != nil {
		return heartbeatErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// The Job's budget ran out while the stage was running. Whatever timeout code the stage reported,
	// the Job failed for lack of time. A stage that succeeded just before the budget ended keeps its
	// result, and the next claim sees the spent budget.
	if budgetBound && expired && (processErr != nil || result.Failure != nil) {
		return worker.failGeneration(ctx, request, jobBudgetFailure())
	}
	if processErr != nil {
		slog.Error("Generation stage failed", "job_id", request.JobID, "stage", request.Stage, "error", processErr)
		failure := processFailure(request.Stage, processErr)
		return worker.failGeneration(ctx, request, failure)
	}
	if result.Failure != nil && result.Transition.Event != "" {
		return worker.failGeneration(ctx, request, generation.JobFailure{
			Code: "STAGE_RESULT_INVALID", Message: "Generation stage returned conflicting outcomes.", Retryable: false,
		})
	}
	if result.Failure != nil {
		if err := result.Failure.Validate(); err != nil {
			return worker.failGeneration(ctx, request, generation.JobFailure{
				Code: "STAGE_RESULT_INVALID", Message: "Generation stage returned an invalid failure.", Retryable: false,
			})
		}
		return worker.failGeneration(ctx, request, *result.Failure)
	}
	if result.Transition.Event == "" {
		return worker.failGeneration(ctx, request, generation.JobFailure{
			Code: "STAGE_RESULT_INVALID", Message: "Generation stage returned no transition.", Retryable: false,
		})
	}
	_, err = worker.store.AdvanceGeneration(ctx, request.JobID, request.Lease, result.Transition)
	if err != nil {
		return fmt.Errorf("advance generation job %s: %w", request.JobID, err)
	}
	return nil
}

// stageTimeout returns the deadline a stage runs under: its configured Stage timeout, cut short by
// the Job's remaining budget. budgetBound reports whether the Job budget is the binding limit.
func stageTimeout(configured, remaining time.Duration, bounded bool) (time.Duration, bool) {
	if !bounded {
		return configured, false
	}
	if configured <= 0 || remaining < configured {
		return remaining, true
	}
	return configured, false
}

func jobBudgetFailure() generation.JobFailure {
	return generation.JobFailure{
		Code: "GENERATION_JOB_DEADLINE_EXCEEDED", Message: "Generation job exceeded its automated processing budget.", Retryable: false,
	}
}

func processFailure(stage generation.JobStatus, processErr error) generation.JobFailure {
	if errors.Is(processErr, context.DeadlineExceeded) {
		switch stage {
		case generation.StatusGeneratingDataset:
			return generation.JobFailure{Code: "DATASET_CREATION_TIMEOUT", Message: "Dataset creation exceeded its Stage deadline.", Retryable: true}
		case generation.StatusValidatingDataset:
			return generation.JobFailure{Code: "DATASET_VALIDATION_TIMEOUT", Message: "Dataset validation exceeded its Stage deadline.", Retryable: true}
		default:
			return generation.JobFailure{Code: "GENERATION_STAGE_TIMEOUT", Message: "Generation stage exceeded its deadline.", Retryable: true}
		}
	}
	return generation.JobFailure{Code: "GENERATION_STAGE_ERROR", Message: "Generation stage failed unexpectedly.", Retryable: true}
}

func (worker *Worker) processWithHeartbeat(ctx context.Context, item mysqlqueue.Item, request StageRequest, timeout time.Duration) (StageResult, error, error, bool) {
	processContext, cancelProcess := stageContext(ctx, timeout)
	heartbeatDone := make(chan error, 1)
	go worker.maintainLease(processContext, cancelProcess, item, heartbeatDone)

	result, processErr := worker.processor.Process(processContext, request)
	expired := errors.Is(processContext.Err(), context.DeadlineExceeded)
	cancelProcess()
	heartbeatErr := <-heartbeatDone
	return result, processErr, heartbeatErr, expired
}

// stageContext returns the context one stage runs under. It holds a single cancel function, so the
// caller releases it once the stage returns.
func stageContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(ctx, timeout)
	}
	return context.WithCancel(ctx)
}

func (worker *Worker) maintainLease(ctx context.Context, cancel context.CancelFunc, item mysqlqueue.Item, done chan<- error) {
	ticker := time.NewTicker(worker.config.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			done <- nil
			return
		case <-ticker.C:
			if err := worker.queue.Heartbeat(ctx, item, worker.config.LeaseDuration); err != nil {
				cancel()
				done <- fmt.Errorf("heartbeat generation work %s: %w", item.ID, err)
				return
			}
		}
	}
}

// publish performs the Atomic Publish Transaction for a Job in PUBLISHING (see
// PublishGeneration); a stale lease or job-state conflict is returned as-is so ProcessNext's
// existing handling applies, any other failure goes through the normal retry/exhaust path.
func (worker *Worker) publish(ctx context.Context, request StageRequest) error {
	_, err := worker.store.PublishGeneration(ctx, request.JobID, request.Lease)
	if err == nil {
		return nil
	}
	if errors.Is(err, mysqlqueue.ErrStaleLease) || errors.Is(err, generation.ErrConflict) {
		return err
	}
	return worker.failGeneration(ctx, request, generation.JobFailure{
		Code: "PUBLISH_FAILED", Message: "Publishing the Generation package failed.", Retryable: true,
	})
}

func (worker *Worker) failGeneration(ctx context.Context, request StageRequest, failure generation.JobFailure) error {
	_, err := worker.store.FailGeneration(ctx, request.JobID, request.Lease, failure, retryBackoff(request.Lease))
	if err != nil {
		return fmt.Errorf("settle failed generation stage %s: %w", request.Stage, err)
	}
	return nil
}

func (worker *Worker) rejectInvalidItem(ctx context.Context, item mysqlqueue.Item, validationErr error) error {
	failure := generation.JobFailure{
		Code: "WORK_ITEM_INVALID", Message: "Generation work item is invalid.", Retryable: false,
		Details: map[string]any{"reason": validationErr.Error()},
	}
	_, idErr := ulid.ParseStrict(item.AggregateID)
	if item.AggregateType == aggregateType && idErr == nil && item.FencingToken > 0 && strings.TrimSpace(item.LeaseOwner) != "" {
		request := StageRequest{JobID: item.AggregateID, Stage: generation.StatusPending, Lease: workLease(item)}
		return worker.failGeneration(ctx, request, failure)
	}
	failureJSON, err := json.Marshal(failure)
	if err != nil {
		return fmt.Errorf("encode invalid work item failure: %w", err)
	}
	if err := worker.queue.Fail(ctx, item, false, 0, failureJSON); err != nil {
		return fmt.Errorf("dead-letter invalid generation work: %w", err)
	}
	return nil
}

func stageRequest(item mysqlqueue.Item) (StageRequest, error) {
	if item.QueueName != QueueName || item.AggregateType != aggregateType || strings.TrimSpace(item.AggregateID) == "" {
		return StageRequest{}, fmt.Errorf("unexpected queue or aggregate identity")
	}
	if item.AttemptCount < 1 || item.MaxAttempts < 1 || item.FencingToken == 0 || strings.TrimSpace(item.LeaseOwner) == "" {
		return StageRequest{}, fmt.Errorf("incomplete work lease")
	}
	stageText, roundText, found := strings.Cut(item.StageKey, ":repair-")
	if !found || strings.Contains(roundText, ":") {
		return StageRequest{}, fmt.Errorf("invalid stage key")
	}
	stage := generation.JobStatus(stageText)
	if !stage.Valid() || stage == generation.StatusCompleted || stage == generation.StatusFailed || stage == generation.StatusCancelled {
		return StageRequest{}, fmt.Errorf("invalid generation stage %q", stageText)
	}
	repairRound, err := strconv.Atoi(roundText)
	if err != nil || repairRound < 0 || repairRound > generation.MaxRepairRounds {
		return StageRequest{}, fmt.Errorf("invalid repair round")
	}

	decoder := json.NewDecoder(strings.NewReader(string(item.Payload)))
	decoder.DisallowUnknownFields()
	var payload struct {
		JobID                   string  `json:"job_id"`
		JobKind                 string  `json:"job_kind"`
		InputScenarioRevisionID *string `json:"input_scenario_revision_id"`
		InputDatasetRevisionID  *string `json:"input_dataset_revision_id"`
	}
	if err := decoder.Decode(&payload); err != nil {
		return StageRequest{}, fmt.Errorf("invalid generation payload: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return StageRequest{}, fmt.Errorf("generation payload has trailing data")
	}
	if payload.JobID != item.AggregateID {
		return StageRequest{}, fmt.Errorf("payload Job ID does not match aggregate")
	}

	return StageRequest{
		JobID: item.AggregateID, Stage: stage, RepairRound: repairRound,
		InputScenarioRevisionID: payload.InputScenarioRevisionID, InputDatasetRevisionID: payload.InputDatasetRevisionID,
		Attempt: item.AttemptCount, Payload: append([]byte(nil), item.Payload...), Lease: workLease(item),
	}, nil
}

func workLease(item mysqlqueue.Item) generation.WorkLease {
	return generation.WorkLease{
		WorkItemID: item.ID, Owner: item.LeaseOwner, FencingToken: item.FencingToken,
		AttemptCount: item.AttemptCount, MaxAttempts: item.MaxAttempts,
	}
}

func retryBackoff(lease generation.WorkLease) time.Duration {
	attempt := lease.AttemptCount
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 7 {
		attempt = 7
	}
	base := time.Second << (attempt - 1)
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", lease.WorkItemID, lease.AttemptCount)))
	jitterWindow := uint64(base / 4)
	if jitterWindow == 0 {
		return base
	}
	jitter := time.Duration(binary.BigEndian.Uint64(digest[:8]) % jitterWindow)
	return base + jitter
}

func sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
