package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"agent-test-platform/contracts"
	"agent-test-platform/internal/generation"
	"agent-test-platform/internal/jobs/mysqlqueue"

	"github.com/oklog/ulid/v2"
)

const (
	upsertGenerationRequestSQL = `INSERT INTO generation_request (
id, idempotency_key, request_json, request_sha256, description_tokens,
target_agent_version_id, generate_dataset, database_profile_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE id = id`
	lockGenerationRequestSQL = `SELECT id, request_sha256
FROM generation_request WHERE idempotency_key = ? FOR UPDATE`
	insertGenerationJobSQL = `INSERT INTO generation_job (
id, request_id, job_kind, input_scenario_revision_id, input_dataset_revision_id,
status, current_stage, requested_test_case_count, generator_config_sha256, require_manual_review
) VALUES (?, ?, ?, ?, ?, 'PENDING', 'PENDING', ?, ?, ?)`
	selectGenerationJobSQL = `SELECT id, job_kind, pipeline_id, require_manual_review, status, requested_test_case_count,
accepted_test_case_count, repair_round, error_json, created_at, updated_at
FROM generation_job WHERE id = ?`
	selectGenerationJobByRequestSQL = `SELECT id, job_kind, pipeline_id, require_manual_review, status, requested_test_case_count,
accepted_test_case_count, repair_round, error_json, created_at, updated_at
FROM generation_job WHERE request_id = ?`
	lockGenerationJobDetailsSQL = `SELECT id, job_kind, pipeline_id, require_manual_review, status, requested_test_case_count,
accepted_test_case_count, repair_round, error_json, created_at, updated_at
FROM generation_job WHERE id = ? FOR UPDATE`
	cancelGenerationJobSQL = `UPDATE generation_job
SET status = 'CANCELLED', current_stage = NULL, finished_at = UTC_TIMESTAMP(6),
    updated_at = UTC_TIMESTAMP(6)
WHERE id = ?`
	cancelGenerationWorkSQL = `UPDATE work_item
SET status = 'DEAD', lease_owner = NULL, lease_until = NULL,
    last_error_json = JSON_OBJECT('code', 'GENERATION_CANCELLED', 'message', 'Generation job was cancelled.', 'retryable', FALSE),
    updated_at = UTC_TIMESTAMP(6)
WHERE aggregate_type = 'generation_job' AND aggregate_id = ? AND status IN ('READY', 'LEASED')`
	lockGenerationLeaseSQL = `SELECT aggregate_type, aggregate_id
FROM work_item
WHERE id = ? AND status = 'LEASED' AND lease_owner = ? AND fencing_token = ?
FOR UPDATE`
	lockGenerationJobSQL = `SELECT job_kind, status, requested_test_case_count, accepted_test_case_count, repair_round,
input_scenario_revision_id, input_dataset_revision_id, require_manual_review
FROM generation_job WHERE id = ? FOR UPDATE`
	updateGenerationJobSQL = `UPDATE generation_job
SET status = ?, current_stage = ?, accepted_test_case_count = ?, repair_round = ?,
    started_at = COALESCE(started_at, UTC_TIMESTAMP(6)),
    deadline_at = COALESCE(deadline_at, TIMESTAMPADD(MICROSECOND, ?, UTC_TIMESTAMP(6))),
    finished_at = CASE WHEN ? THEN UTC_TIMESTAMP(6) ELSE finished_at END,
    updated_at = UTC_TIMESTAMP(6)
WHERE id = ?`
	completeGenerationWorkSQL = `UPDATE work_item
SET status = 'DONE', lease_owner = NULL, lease_until = NULL, updated_at = UTC_TIMESTAMP(6)
WHERE id = ? AND status = 'LEASED' AND lease_owner = ? AND fencing_token = ?`
	retryGenerationWorkSQL = `UPDATE work_item
SET status = 'READY', lease_owner = NULL, lease_until = NULL,
	available_at = TIMESTAMPADD(MICROSECOND, ?, UTC_TIMESTAMP(6)),
	last_error_json = ?, updated_at = UTC_TIMESTAMP(6)
WHERE id = ? AND status = 'LEASED' AND lease_owner = ? AND fencing_token = ?`
	deadGenerationWorkSQL = `UPDATE work_item
SET status = 'DEAD', lease_owner = NULL, lease_until = NULL,
	last_error_json = ?, updated_at = UTC_TIMESTAMP(6)
WHERE id = ? AND status = 'LEASED' AND lease_owner = ? AND fencing_token = ?`
	failGenerationJobSQL = `UPDATE generation_job
SET status = 'FAILED', current_stage = NULL, error_json = ?,
	started_at = COALESCE(started_at, UTC_TIMESTAMP(6)), finished_at = UTC_TIMESTAMP(6),
	updated_at = UTC_TIMESTAMP(6)
WHERE id = ?`
	insertGenerationWorkSQL = `INSERT INTO work_item (
id, queue_name, aggregate_type, aggregate_id, stage_key, status, priority,
available_at, fencing_token, attempt_count, max_attempts, payload_json
) VALUES (?, 'generation', 'generation_job', ?, ?, 'READY', ?, UTC_TIMESTAMP(6), 0, 0, ?, ?)`
	insertGenerationPipelineSQL = `INSERT INTO generation_pipeline (
id, status, requested_test_case_count, scenario_job_id
) VALUES (?, 'RUNNING', ?, ?)`
	setGenerationJobPipelineSQL     = `UPDATE generation_job SET pipeline_id = ?, updated_at = UTC_TIMESTAMP(6) WHERE id = ?`
	selectGenerationPipelineJobsSQL = `SELECT status, scenario_job_id, dataset_job_id, test_cases_job_id
FROM generation_pipeline WHERE id = ?`
	markGenerationReviewPendingSQL = `UPDATE generation_job SET review_status = 'PENDING', updated_at = UTC_TIMESTAMP(6) WHERE id = ?`
	// A Job's failure or cancellation ends its Generation Pipeline (docs/architecture.md 6.3);
	// already-published upstream output is never rolled back. No-op for Jobs outside a Pipeline.
	settleGenerationPipelineSQL = `UPDATE generation_pipeline AS pipeline
JOIN generation_job AS job ON job.pipeline_id = pipeline.id
SET pipeline.status = ?, pipeline.error_json = job.error_json, pipeline.updated_at = UTC_TIMESTAMP(6)
WHERE job.id = ? AND pipeline.status = 'RUNNING'`
)

type GenerationLease = generation.WorkLease

type GenerationStore struct {
	database  *sql.DB
	newID     IDGenerator
	validator *contracts.Validator
}

var _ generation.JobStore = (*GenerationStore)(nil)

func NewGenerationStore(database *sql.DB) (*GenerationStore, error) {
	return newGenerationStore(database, func() (string, error) {
		return ulid.Make().String(), nil
	})
}

func newGenerationStore(database *sql.DB, generator IDGenerator) (*GenerationStore, error) {
	if database == nil || generator == nil {
		return nil, fmt.Errorf("database and ID generator are required")
	}
	validator, err := contracts.NewValidator()
	if err != nil {
		return nil, fmt.Errorf("compile contract schemas: %w", err)
	}
	return &GenerationStore{database: database, newID: generator, validator: validator}, nil
}

func (store *GenerationStore) CreateGenerationJob(ctx context.Context, input generation.CreateJobInput) (generation.Job, error) {
	// An unspecified job_kind means the caller wants the convenience "one description in,
	// full package out" flow (docs/architecture.md 6.1): create a Generation Pipeline that
	// starts a SCENARIO job and auto-chains DATASET then TEST_CASES as each stage publishes
	// (see pipelineAfterPublish in generation_publish.go), rather than defaulting to a single
	// TEST_CASES job with no inputs (which the strict validateJobKindInputs precondition
	// would always reject).
	if input.Kind == "" {
		return store.createGenerationPipeline(ctx, input)
	}
	if err := validateCreateGenerationJob(input); err != nil {
		return generation.Job{}, err
	}
	if err := requireRegisteredTargets(ctx, store.database, input.ValidationDBIDs); err != nil {
		return generation.Job{}, err
	}
	ids, err := store.newGenerationIDs(3)
	if err != nil {
		return generation.Job{}, err
	}
	requestID, jobID, workID := ids[0], ids[1], ids[2]
	payload, err := json.Marshal(generationWorkPayload{
		JobID: jobID, JobKind: string(input.Kind),
		InputScenarioRevisionID: input.InputScenarioRevisionID, InputDatasetRevisionID: input.InputDatasetRevisionID,
	})
	if err != nil {
		return generation.Job{}, fmt.Errorf("encode initial generation work: %w", err)
	}

	transaction, err := store.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return generation.Job{}, fmt.Errorf("begin generation create: %w", err)
	}
	defer transaction.Rollback()
	if _, err := transaction.ExecContext(ctx, upsertGenerationRequestSQL,
		requestID, input.IdempotencyKey, []byte(input.Request), input.RequestSHA256, input.DescriptionTokens,
		input.TargetAgentVersionID, input.GenerateDataset, input.DatabaseProfileID,
	); err != nil {
		return generation.Job{}, fmt.Errorf("upsert generation request: %w", err)
	}

	var storedRequestID, storedRequestDigest string
	if err := transaction.QueryRowContext(ctx, lockGenerationRequestSQL, input.IdempotencyKey).Scan(&storedRequestID, &storedRequestDigest); err != nil {
		return generation.Job{}, fmt.Errorf("lock generation request: %w", err)
	}
	if storedRequestDigest != input.RequestSHA256 {
		return generation.Job{}, generation.ErrIdempotencyConflict
	}
	if storedRequestID == requestID {
		if _, err := transaction.ExecContext(ctx, insertGenerationJobSQL,
			jobID, requestID, string(input.Kind), input.InputScenarioRevisionID, input.InputDatasetRevisionID,
			input.RequestedTestCaseCount, input.GeneratorConfigSHA256, input.RequireManualReview,
		); err != nil {
			return generation.Job{}, fmt.Errorf("insert generation job: %w", err)
		}
		if _, err := transaction.ExecContext(ctx, insertGenerationWorkSQL,
			workID, jobID, "PENDING:repair-0", 0, 3, payload,
		); err != nil {
			return generation.Job{}, fmt.Errorf("enqueue initial generation work: %w", err)
		}
	}

	job, err := scanGenerationJob(transaction.QueryRowContext(ctx, selectGenerationJobByRequestSQL, storedRequestID))
	if errors.Is(err, sql.ErrNoRows) {
		return generation.Job{}, fmt.Errorf("generation request %s has no job", storedRequestID)
	}
	if err != nil {
		return generation.Job{}, fmt.Errorf("load generation job: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return generation.Job{}, fmt.Errorf("commit generation create: %w", err)
	}
	return job, nil
}

// createGenerationPipeline implements the "one description in, full package out" convenience
// flow (docs/architecture.md 6.1) used when CreateGenerationJob is called without an explicit
// job_kind: it creates a Generation Pipeline together with its first (SCENARIO) Job, carrying
// the caller's desired test case count forward on the Pipeline row so that the auto-chain in
// generation_publish.go can create the DATASET and TEST_CASES Jobs once their preconditions
// (the prior stage's published Revision) exist.
func (store *GenerationStore) createGenerationPipeline(ctx context.Context, input generation.CreateJobInput) (generation.Job, error) {
	if err := validateCreateGenerationPipeline(input); err != nil {
		return generation.Job{}, err
	}
	ids, err := store.newGenerationIDs(4)
	if err != nil {
		return generation.Job{}, err
	}
	requestID, jobID, workID, pipelineID := ids[0], ids[1], ids[2], ids[3]
	if err := requireRegisteredTargets(ctx, store.database, input.ValidationDBIDs); err != nil {
		return generation.Job{}, err
	}
	payload, err := json.Marshal(generationWorkPayload{JobID: jobID, JobKind: string(generation.KindScenario)})
	if err != nil {
		return generation.Job{}, fmt.Errorf("encode initial generation work: %w", err)
	}

	transaction, err := store.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return generation.Job{}, fmt.Errorf("begin generation pipeline create: %w", err)
	}
	defer transaction.Rollback()
	if _, err := transaction.ExecContext(ctx, upsertGenerationRequestSQL,
		requestID, input.IdempotencyKey, []byte(input.Request), input.RequestSHA256, input.DescriptionTokens,
		input.TargetAgentVersionID, input.GenerateDataset, input.DatabaseProfileID,
	); err != nil {
		return generation.Job{}, fmt.Errorf("upsert generation request: %w", err)
	}

	var storedRequestID, storedRequestDigest string
	if err := transaction.QueryRowContext(ctx, lockGenerationRequestSQL, input.IdempotencyKey).Scan(&storedRequestID, &storedRequestDigest); err != nil {
		return generation.Job{}, fmt.Errorf("lock generation request: %w", err)
	}
	if storedRequestDigest != input.RequestSHA256 {
		return generation.Job{}, generation.ErrIdempotencyConflict
	}
	if storedRequestID == requestID {
		if _, err := transaction.ExecContext(ctx, insertGenerationJobSQL,
			jobID, requestID, string(generation.KindScenario), nil, nil, 0, input.GeneratorConfigSHA256, input.RequireManualReview,
		); err != nil {
			return generation.Job{}, fmt.Errorf("insert generation pipeline scenario job: %w", err)
		}
		if _, err := transaction.ExecContext(ctx, insertGenerationPipelineSQL,
			pipelineID, input.RequestedTestCaseCount, jobID,
		); err != nil {
			return generation.Job{}, fmt.Errorf("insert generation pipeline: %w", err)
		}
		if _, err := transaction.ExecContext(ctx, setGenerationJobPipelineSQL, pipelineID, jobID); err != nil {
			return generation.Job{}, fmt.Errorf("link generation job to pipeline: %w", err)
		}
		if _, err := transaction.ExecContext(ctx, insertGenerationWorkSQL,
			workID, jobID, "PENDING:repair-0", 0, 3, payload,
		); err != nil {
			return generation.Job{}, fmt.Errorf("enqueue initial generation pipeline work: %w", err)
		}
	}

	job, err := scanGenerationJob(transaction.QueryRowContext(ctx, selectGenerationJobByRequestSQL, storedRequestID))
	if errors.Is(err, sql.ErrNoRows) {
		return generation.Job{}, fmt.Errorf("generation request %s has no job", storedRequestID)
	}
	if err != nil {
		return generation.Job{}, fmt.Errorf("load generation pipeline job: %w", err)
	}
	job, err = resolvePipelineJob(ctx, transaction, job)
	if err != nil {
		return generation.Job{}, err
	}
	if err := transaction.Commit(); err != nil {
		return generation.Job{}, fmt.Errorf("commit generation pipeline create: %w", err)
	}
	return job, nil
}

// sqlQuerier is satisfied by both *sql.DB and *sql.Tx, letting helpers such as
// resolvePipelineJob run against either an ad-hoc read (GetGenerationJob) or a Job's own
// write transaction (createGenerationPipeline, CancelGenerationJob).
type sqlQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// resolvePipelineJob follows job.PipelineID (if set) to the furthest-advanced Job in that
// Generation Pipeline (TEST_CASES if started, else DATASET if started, else the SCENARIO job
// itself), so that polling the ID returned at Pipeline creation time transparently "follows"
// the pipeline through all of its stages without the caller needing any pipeline-aware logic.
func resolvePipelineJob(ctx context.Context, querier sqlQuerier, job generation.Job) (generation.Job, error) {
	if job.PipelineID == nil {
		return job, nil
	}
	latestID, err := resolveLatestPipelineJobID(ctx, querier, *job.PipelineID, job.ID)
	if err != nil {
		return generation.Job{}, err
	}
	if latestID == job.ID {
		return job, nil
	}
	latest, err := scanGenerationJob(querier.QueryRowContext(ctx, selectGenerationJobSQL, latestID))
	if err != nil {
		return generation.Job{}, fmt.Errorf("load latest generation pipeline job: %w", err)
	}
	return latest, nil
}

// resolveLatestPipelineJobID returns the furthest-advanced Job ID in the Generation Pipeline
// identified by pipelineID, falling back to fallbackID if the Pipeline row cannot be found
// (which should not normally happen, since a Job's pipeline_id is only ever set in the same
// transaction that first inserts the Pipeline row it points to).
func resolveLatestPipelineJobID(ctx context.Context, querier sqlQuerier, pipelineID, fallbackID string) (string, error) {
	var status string
	var scenarioJobID, datasetJobID, testCasesJobID sql.NullString
	err := querier.QueryRowContext(ctx, selectGenerationPipelineJobsSQL, pipelineID).
		Scan(&status, &scenarioJobID, &datasetJobID, &testCasesJobID)
	if errors.Is(err, sql.ErrNoRows) {
		return fallbackID, nil
	}
	if err != nil {
		return "", fmt.Errorf("load generation pipeline: %w", err)
	}
	switch {
	case testCasesJobID.Valid:
		return testCasesJobID.String, nil
	case datasetJobID.Valid:
		return datasetJobID.String, nil
	case scenarioJobID.Valid:
		return scenarioJobID.String, nil
	default:
		return fallbackID, nil
	}
}

func (store *GenerationStore) GetGenerationJob(ctx context.Context, id string) (generation.Job, error) {
	if _, err := ulid.ParseStrict(id); err != nil {
		return generation.Job{}, generation.ErrNotFound
	}
	job, err := scanGenerationJob(store.database.QueryRowContext(ctx, selectGenerationJobSQL, id))
	if errors.Is(err, sql.ErrNoRows) {
		return generation.Job{}, generation.ErrNotFound
	}
	if err != nil {
		return generation.Job{}, fmt.Errorf("load generation job: %w", err)
	}
	return resolvePipelineJob(ctx, store.database, job)
}

func (store *GenerationStore) CancelGenerationJob(ctx context.Context, id, idempotencyKey string) (generation.Job, error) {
	if _, err := ulid.ParseStrict(id); err != nil {
		return generation.Job{}, generation.ErrNotFound
	}
	if strings.TrimSpace(idempotencyKey) == "" || len(idempotencyKey) > 128 {
		return generation.Job{}, fmt.Errorf("valid idempotency key is required")
	}
	transaction, err := store.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return generation.Job{}, fmt.Errorf("begin generation cancellation: %w", err)
	}
	defer transaction.Rollback()

	current, err := scanGenerationJob(transaction.QueryRowContext(ctx, lockGenerationJobDetailsSQL, id))
	if errors.Is(err, sql.ErrNoRows) {
		return generation.Job{}, generation.ErrNotFound
	}
	if err != nil {
		return generation.Job{}, fmt.Errorf("lock generation job: %w", err)
	}
	// A cancellation request against the entry-point Job of a Generation Pipeline should
	// cancel whichever stage is currently running, not the (possibly already-COMPLETED)
	// SCENARIO/DATASET Job the caller originally polled.
	if current.PipelineID != nil {
		latestID, err := resolveLatestPipelineJobID(ctx, transaction, *current.PipelineID, current.ID)
		if err != nil {
			return generation.Job{}, err
		}
		if latestID != current.ID {
			current, err = scanGenerationJob(transaction.QueryRowContext(ctx, lockGenerationJobDetailsSQL, latestID))
			if errors.Is(err, sql.ErrNoRows) {
				return generation.Job{}, generation.ErrNotFound
			}
			if err != nil {
				return generation.Job{}, fmt.Errorf("lock generation job: %w", err)
			}
		}
	}
	id = current.ID
	if current.Status == generation.StatusCancelled {
		if err := transaction.Commit(); err != nil {
			return generation.Job{}, fmt.Errorf("commit generation cancellation replay: %w", err)
		}
		return current, nil
	}
	state := generation.JobState{
		Kind: current.Kind, Status: current.Status, RequestedTestCaseCount: current.RequestedTestCaseCount,
		AcceptedTestCaseCount: current.AcceptedTestCaseCount, RepairRound: current.RepairRound,
	}
	if _, err := generation.Transition(state, generation.TransitionInput{Event: generation.EventCancel}); err != nil {
		return generation.Job{}, fmt.Errorf("%w: %v", generation.ErrConflict, err)
	}
	if _, err := transaction.ExecContext(ctx, cancelGenerationJobSQL, id); err != nil {
		return generation.Job{}, fmt.Errorf("cancel generation job: %w", err)
	}
	if _, err := transaction.ExecContext(ctx, cancelGenerationWorkSQL, id); err != nil {
		return generation.Job{}, fmt.Errorf("cancel generation work: %w", err)
	}
	if _, err := transaction.ExecContext(ctx, settleGenerationPipelineSQL, "CANCELLED", id); err != nil {
		return generation.Job{}, fmt.Errorf("cancel generation pipeline: %w", err)
	}
	job, err := scanGenerationJob(transaction.QueryRowContext(ctx, selectGenerationJobSQL, id))
	if err != nil {
		return generation.Job{}, fmt.Errorf("load cancelled generation job: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return generation.Job{}, fmt.Errorf("commit generation cancellation: %w", err)
	}
	return job, nil
}

func validateCreateGenerationJob(input generation.CreateJobInput) error {
	if err := validateCreateGenerationCommon(input); err != nil {
		return err
	}
	if !input.Kind.Valid() {
		return fmt.Errorf("job kind is invalid")
	}
	if input.Kind == generation.KindTestCases {
		if input.RequestedTestCaseCount < 1 || input.RequestedTestCaseCount > 20 {
			return fmt.Errorf("requested test case count outside [1,20]")
		}
	} else if input.RequestedTestCaseCount != 0 {
		return fmt.Errorf("requested test case count only applies to TEST_CASES jobs")
	}
	if err := validateJobKindInputs(input.Kind, input.InputScenarioRevisionID, input.InputDatasetRevisionID); err != nil {
		return err
	}
	return nil
}

// validateCreateGenerationPipeline validates the request for the "one description in, full
// package out" convenience flow (docs/architecture.md 6.1): a Generation Pipeline always
// starts from a SCENARIO job with no input references, and carries the caller's desired test
// case count forward for the TEST_CASES job it will create once the SCENARIO and DATASET jobs
// publish successfully (see pipelineAfterPublish in generation_publish.go).
func validateCreateGenerationPipeline(input generation.CreateJobInput) error {
	if err := validateCreateGenerationCommon(input); err != nil {
		return err
	}
	if input.RequestedTestCaseCount < 1 || input.RequestedTestCaseCount > 20 {
		return fmt.Errorf("requested test case count outside [1,20]")
	}
	if input.InputScenarioRevisionID != nil || input.InputDatasetRevisionID != nil {
		return fmt.Errorf("a Generation Pipeline does not accept input revision references")
	}
	return nil
}

// validateCreateGenerationCommon validates the fields shared by both a single explicit-kind
// Generation Job (validateCreateGenerationJob) and a full Generation Pipeline
// (validateCreateGenerationPipeline).
func validateCreateGenerationCommon(input generation.CreateJobInput) error {
	if strings.TrimSpace(input.IdempotencyKey) == "" || len(input.IdempotencyKey) > 128 {
		return fmt.Errorf("valid idempotency key is required")
	}
	if len(input.Request) == 0 || !json.Valid(input.Request) {
		return fmt.Errorf("generation request must be valid JSON")
	}
	if !validSHA256(input.RequestSHA256) || !validSHA256(input.GeneratorConfigSHA256) {
		return fmt.Errorf("request and generator config SHA-256 values are required")
	}
	if input.DescriptionTokens < 1 || input.DescriptionTokens > generation.MaxDescriptionTokens {
		return fmt.Errorf("description token count outside [1,%d]", generation.MaxDescriptionTokens)
	}
	minimumTargets := 1
	if input.Kind == generation.KindScenario || input.Kind == generation.KindTestCases {
		minimumTargets = 0
	}
	if len(input.ValidationDBIDs) < minimumTargets || len(input.ValidationDBIDs) > generation.MaxRequestTargets {
		return fmt.Errorf("validation_db_ids must name 1 to %d registered validation MySQL instances", generation.MaxRequestTargets)
	}
	if input.TargetAgentVersionID != nil {
		if _, err := ulid.ParseStrict(*input.TargetAgentVersionID); err != nil {
			return fmt.Errorf("target Agent Version ID is invalid")
		}
	}
	if input.GenerateDataset && input.DatabaseProfileID != nil {
		return fmt.Errorf("Database Profile is not allowed when Dataset generation is enabled")
	}
	if !input.GenerateDataset {
		if input.DatabaseProfileID == nil {
			return fmt.Errorf("Database Profile is required when Dataset generation is disabled")
		}
		if _, err := ulid.ParseStrict(*input.DatabaseProfileID); err != nil {
			return fmt.Errorf("Database Profile ID is invalid")
		}
	}
	return nil
}

// validateJobKindInputs enforces the precondition in docs/architecture.md 6.2 at the
// application layer: SCENARIO carries neither input reference, DATASET carries only
// input_scenario_revision_id, TEST_CASES carries both. The database only enforces the
// narrower "SCENARIO carries no input reference" direction (see migrations/0003).
func validateJobKindInputs(kind generation.JobKind, scenarioRevisionID, datasetRevisionID *string) error {
	validID := func(id *string) error {
		if id == nil {
			return nil
		}
		if _, err := ulid.ParseStrict(*id); err != nil {
			return fmt.Errorf("input revision ID is invalid")
		}
		return nil
	}
	if err := validID(scenarioRevisionID); err != nil {
		return err
	}
	if err := validID(datasetRevisionID); err != nil {
		return err
	}
	switch kind {
	case generation.KindScenario:
		if scenarioRevisionID != nil || datasetRevisionID != nil {
			return fmt.Errorf("SCENARIO jobs must not carry an input revision reference")
		}
	case generation.KindDataset:
		if scenarioRevisionID == nil {
			return fmt.Errorf("DATASET jobs require input_scenario_revision_id")
		}
		if datasetRevisionID != nil {
			return fmt.Errorf("DATASET jobs must not carry input_dataset_revision_id")
		}
	case generation.KindTestCases:
		if scenarioRevisionID == nil || datasetRevisionID == nil {
			return fmt.Errorf("TEST_CASES jobs require input_scenario_revision_id and input_dataset_revision_id")
		}
	}
	return nil
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func (store *GenerationStore) newGenerationIDs(count int) ([]string, error) {
	ids := make([]string, count)
	for index := range ids {
		id, err := store.newID()
		if err != nil {
			return nil, fmt.Errorf("generate generation ID: %w", err)
		}
		if _, err := ulid.ParseStrict(id); err != nil {
			return nil, fmt.Errorf("generate generation ID: %w", err)
		}
		ids[index] = id
	}
	return ids, nil
}

type rowScanner interface {
	Scan(...any) error
}

func scanGenerationJob(scanner rowScanner) (generation.Job, error) {
	var job generation.Job
	var pipelineID sql.NullString
	var failureJSON []byte
	if err := scanner.Scan(
		&job.ID, &job.Kind, &pipelineID, &job.RequireManualReview, &job.Status, &job.RequestedTestCaseCount, &job.AcceptedTestCaseCount,
		&job.RepairRound, &failureJSON, &job.CreatedAt, &job.UpdatedAt,
	); err != nil {
		return generation.Job{}, err
	}
	if pipelineID.Valid {
		job.PipelineID = &pipelineID.String
	}
	state := generation.JobState{
		Kind: job.Kind, Status: job.Status, RequestedTestCaseCount: job.RequestedTestCaseCount,
		AcceptedTestCaseCount: job.AcceptedTestCaseCount, RepairRound: job.RepairRound,
	}
	if err := state.Validate(); err != nil {
		return generation.Job{}, fmt.Errorf("invalid stored generation job %s: %w", job.ID, err)
	}
	if len(failureJSON) > 0 {
		var failure generation.JobFailure
		if err := json.Unmarshal(failureJSON, &failure); err != nil {
			return generation.Job{}, fmt.Errorf("invalid stored generation error: %w", err)
		}
		job.Error = &failure
	}
	job.CreatedAt = job.CreatedAt.UTC()
	job.UpdatedAt = job.UpdatedAt.UTC()
	return job, nil
}

// FailGeneration atomically retries a leased stage or terminally fails both its Job and work item.
func (store *GenerationStore) FailGeneration(ctx context.Context, jobID string, lease GenerationLease, failure generation.JobFailure, backoff time.Duration) (bool, error) {
	if _, err := ulid.ParseStrict(jobID); err != nil {
		return false, fmt.Errorf("invalid generation job ID: %w", err)
	}
	if lease.WorkItemID == "" || lease.Owner == "" || lease.FencingToken == 0 || lease.AttemptCount < 1 || lease.MaxAttempts < 1 {
		return false, fmt.Errorf("complete generation lease is required")
	}
	if err := failure.Validate(); err != nil {
		return false, err
	}
	if backoff < 0 || backoff > 24*time.Hour {
		return false, fmt.Errorf("retry backoff outside [0,24h]")
	}
	failureJSON, err := json.Marshal(failure)
	if err != nil {
		return false, fmt.Errorf("encode generation failure: %w", err)
	}

	transaction, err := store.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return false, fmt.Errorf("begin generation failure: %w", err)
	}
	defer transaction.Rollback()
	current, _, _, err := lockGenerationState(ctx, transaction, jobID)
	if err != nil {
		return false, err
	}
	if current.Terminal() {
		return false, fmt.Errorf("%w: job status %s is terminal", generation.ErrConflict, current.Status)
	}
	if err := lockGenerationLease(ctx, transaction, jobID, lease); err != nil {
		return false, err
	}

	terminal := !failure.Retryable || lease.AttemptCount >= lease.MaxAttempts
	if !terminal {
		result, err := transaction.ExecContext(ctx, retryGenerationWorkSQL,
			backoff.Microseconds(), failureJSON, lease.WorkItemID, lease.Owner, lease.FencingToken,
		)
		if err != nil {
			return false, fmt.Errorf("retry generation work: %w", err)
		}
		if err := requireSingleRow(result); err != nil {
			return false, mysqlqueue.ErrStaleLease
		}
	} else {
		if _, err := generation.Transition(current, generation.TransitionInput{Event: generation.EventFatalFailure}); err != nil {
			return false, fmt.Errorf("fail generation job: %w", err)
		}
		result, err := transaction.ExecContext(ctx, failGenerationJobSQL, failureJSON, jobID)
		if err != nil {
			return false, fmt.Errorf("fail generation job: %w", err)
		}
		if err := requireSingleRow(result); err != nil {
			return false, fmt.Errorf("fail generation job: %w", err)
		}
		if _, err := transaction.ExecContext(ctx, settleGenerationPipelineSQL, "FAILED", jobID); err != nil {
			return false, fmt.Errorf("fail generation pipeline: %w", err)
		}
		result, err = transaction.ExecContext(ctx, deadGenerationWorkSQL,
			failureJSON, lease.WorkItemID, lease.Owner, lease.FencingToken,
		)
		if err != nil {
			return false, fmt.Errorf("dead-letter generation work: %w", err)
		}
		if err := requireSingleRow(result); err != nil {
			return false, mysqlqueue.ErrStaleLease
		}
	}
	if err := transaction.Commit(); err != nil {
		return false, fmt.Errorf("commit generation failure: %w", err)
	}
	return terminal, nil
}

// generationWorkPayload is work_item.payload_json's shape: the aggregate ID every stage
// needs, plus the job_kind/input revision references DATASET and TEST_CASES kind stage
// handlers need to resolve their cross-Job context (see generation.StageRequest).
type generationWorkPayload struct {
	JobID                   string  `json:"job_id"`
	JobKind                 string  `json:"job_kind,omitempty"`
	InputScenarioRevisionID *string `json:"input_scenario_revision_id,omitempty"`
	InputDatasetRevisionID  *string `json:"input_dataset_revision_id,omitempty"`
}

func lockGenerationState(ctx context.Context, transaction *sql.Tx, jobID string) (generation.JobState, *string, *string, error) {
	state := generation.JobState{}
	var inputScenarioRevisionID, inputDatasetRevisionID sql.NullString
	if err := transaction.QueryRowContext(ctx, lockGenerationJobSQL, jobID).Scan(
		&state.Kind, &state.Status, &state.RequestedTestCaseCount, &state.AcceptedTestCaseCount, &state.RepairRound,
		&inputScenarioRevisionID, &inputDatasetRevisionID, &state.RequireManualReview,
	); err != nil {
		return generation.JobState{}, nil, nil, fmt.Errorf("lock generation job: %w", err)
	}
	if err := state.Validate(); err != nil {
		return generation.JobState{}, nil, nil, fmt.Errorf("invalid stored generation job %s: %w", jobID, err)
	}
	var scenarioID, datasetID *string
	if inputScenarioRevisionID.Valid {
		scenarioID = &inputScenarioRevisionID.String
	}
	if inputDatasetRevisionID.Valid {
		datasetID = &inputDatasetRevisionID.String
	}
	return state, scenarioID, datasetID, nil
}

func lockGenerationLease(ctx context.Context, transaction *sql.Tx, jobID string, lease GenerationLease) error {
	var aggregateType, aggregateID string
	if err := transaction.QueryRowContext(ctx, lockGenerationLeaseSQL, lease.WorkItemID, lease.Owner, lease.FencingToken).Scan(&aggregateType, &aggregateID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return mysqlqueue.ErrStaleLease
		}
		return fmt.Errorf("lock generation lease: %w", err)
	}
	if aggregateType != "generation_job" || aggregateID != jobID {
		return mysqlqueue.ErrStaleLease
	}
	return nil
}

// AdvanceGeneration atomically consumes one lease, transitions the Job, and enqueues the next stage.
func (store *GenerationStore) AdvanceGeneration(ctx context.Context, jobID string, lease GenerationLease, input generation.TransitionInput) (generation.JobState, error) {
	if _, err := ulid.ParseStrict(jobID); err != nil {
		return generation.JobState{}, fmt.Errorf("invalid generation job ID: %w", err)
	}
	if lease.WorkItemID == "" || lease.Owner == "" || lease.FencingToken == 0 {
		return generation.JobState{}, fmt.Errorf("complete generation lease is required")
	}
	transaction, err := store.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return generation.JobState{}, fmt.Errorf("begin generation transition: %w", err)
	}
	defer transaction.Rollback()

	current, inputScenarioRevisionID, inputDatasetRevisionID, err := lockGenerationState(ctx, transaction, jobID)
	if err != nil {
		return generation.JobState{}, err
	}
	if err := lockGenerationLease(ctx, transaction, jobID, lease); err != nil {
		return generation.JobState{}, err
	}

	next, err := generation.Transition(current, input)
	if err != nil {
		return current, err
	}
	terminal := next.Terminal()
	currentStage := string(next.Status)
	if terminal {
		currentStage = ""
	}
	result, err := transaction.ExecContext(ctx, updateGenerationJobSQL,
		string(next.Status), currentStage, next.AcceptedTestCaseCount, next.RepairRound,
		generation.GenerationJobBudget.Microseconds(), terminal, jobID,
	)
	if err != nil {
		return current, fmt.Errorf("update generation job: %w", err)
	}
	if err := requireSingleRow(result); err != nil {
		return current, fmt.Errorf("update generation job: %w", err)
	}
	result, err = transaction.ExecContext(ctx, completeGenerationWorkSQL, lease.WorkItemID, lease.Owner, lease.FencingToken)
	if err != nil {
		return current, fmt.Errorf("complete generation work: %w", err)
	}
	if err := requireSingleRow(result); err != nil {
		return current, mysqlqueue.ErrStaleLease
	}
	switch {
	case next.Status == generation.StatusAwaitingReview:
		// The Job now waits for a human decision, so nothing is enqueued until
		// ReviewGenerationJob approves it (docs/technical/01-storage-and-workflows.md section 5).
		if _, err := transaction.ExecContext(ctx, markGenerationReviewPendingSQL, jobID); err != nil {
			return current, fmt.Errorf("mark generation review pending: %w", err)
		}
	case !terminal:
		workID, err := store.newID()
		if err != nil {
			return current, fmt.Errorf("generate next work ID: %w", err)
		}
		payload, _ := json.Marshal(generationWorkPayload{
			JobID: jobID, JobKind: string(current.Kind),
			InputScenarioRevisionID: inputScenarioRevisionID, InputDatasetRevisionID: inputDatasetRevisionID,
		})
		stageKey := fmt.Sprintf("%s:repair-%d", next.Status, next.RepairRound)
		if _, err := transaction.ExecContext(ctx, insertGenerationWorkSQL,
			workID, jobID, stageKey, 0, 3, payload,
		); err != nil {
			return current, fmt.Errorf("enqueue next generation stage: %w", err)
		}
	case next.Status == generation.StatusFailed:
		if _, err := transaction.ExecContext(ctx, settleGenerationPipelineSQL, "FAILED", jobID); err != nil {
			return current, fmt.Errorf("fail generation pipeline: %w", err)
		}
	}
	if err := transaction.Commit(); err != nil {
		return current, fmt.Errorf("commit generation transition: %w", err)
	}
	return next, nil
}

func requireSingleRow(result sql.Result) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("affected rows %d, want 1", rows)
	}
	return nil
}
