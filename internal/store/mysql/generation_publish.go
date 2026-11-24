package mysqlstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"agent-test-platform/internal/generation"
	"agent-test-platform/internal/jobs/mysqlqueue"

	"github.com/oklog/ulid/v2"
)

const (
	lockGenerationJobForPublishSQL = `SELECT job_kind, pipeline_id, status, repair_round, requested_test_case_count,
accepted_test_case_count, request_id, input_scenario_revision_id, input_dataset_revision_id, generator_config_sha256,
require_manual_review
FROM generation_job WHERE id = ? FOR UPDATE`
	selectGenerationRequestForPublishSQL = `SELECT request_json, request_sha256, description_tokens,
target_agent_id, generate_dataset, database_profile_id
FROM generation_request WHERE id = ?`
	selectGenerationArtifactForPublishSQL = `SELECT content_json, content_sha256
FROM generation_artifact_draft
WHERE generation_job_id = ? AND artifact_kind = ? AND artifact_key = ?`
	insertPublishedIDMarkerSQL = `INSERT INTO generation_artifact_draft (
id, generation_job_id, artifact_kind, artifact_key, content_json, content_sha256, expires_at
) VALUES (?, ?, 'PUBLISHED_ID', ?, ?, ?, TIMESTAMPADD(HOUR, 24, UTC_TIMESTAMP(6)))
ON DUPLICATE KEY UPDATE id = id`
	selectPublishedIDMarkerSQL = `SELECT content_json FROM generation_artifact_draft
WHERE generation_job_id = ? AND artifact_kind = 'PUBLISHED_ID' AND artifact_key = ?`
	insertScenarioSQL         = `INSERT INTO scenario (id) VALUES (?) ON DUPLICATE KEY UPDATE id = id`
	insertScenarioRevisionSQL = `INSERT INTO scenario_revision (
id, scenario_id, generation_job_id, revision_no, quality_level, lifecycle_status, content_json, content_sha256
) VALUES (?, ?, ?, 1, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE id = id`
	selectScenarioIDForRevisionSQL = `SELECT scenario_id FROM scenario_revision WHERE id = ?`
	selectDatasetIDForRevisionSQL  = `SELECT dataset_id FROM dataset_revision WHERE id = ?`
	insertDatasetSQL               = `INSERT INTO dataset (id, scenario_id) VALUES (?, ?) ON DUPLICATE KEY UPDATE id = id`
	insertDatasetRevisionSQL       = `INSERT INTO dataset_revision (
id, dataset_id, generation_job_id, scenario_revision_id, revision_no,
schema_sql, seed_sql, constraints_json, metadata_json, content_sha256,
execution_status, execution_database, execution_ready_at, lifecycle_status
) VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?, ?, 'READY', ?, UTC_TIMESTAMP(6), ?) ON DUPLICATE KEY UPDATE id = id`
	insertTestCaseSQL         = `INSERT INTO test_case (id, scenario_id, dataset_id) VALUES (?, ?, ?) ON DUPLICATE KEY UPDATE id = id`
	insertTestCaseRevisionSQL = `INSERT INTO test_case_revision (
id, test_case_id, generation_job_id, scenario_revision_id, dataset_revision_id, revision_no,
schema_version, content_json, content_sha256, lifecycle_status
) VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE id = id`
	completeGenerationPublishSQL = `UPDATE generation_job
SET status = 'COMPLETED', current_stage = NULL, finished_at = UTC_TIMESTAMP(6), updated_at = UTC_TIMESTAMP(6)
WHERE id = ?`
	selectGenerationJobIDByRequestSQL   = `SELECT id FROM generation_job WHERE request_id = ?`
	lockGenerationPipelineForPublishSQL = `SELECT status, requested_test_case_count, scenario_job_id, dataset_job_id, test_cases_job_id
FROM generation_pipeline WHERE id = ? FOR UPDATE`
	setGenerationPipelineDatasetJobSQL   = `UPDATE generation_pipeline SET dataset_job_id = ?, updated_at = UTC_TIMESTAMP(6) WHERE id = ?`
	setGenerationPipelineTestCasesJobSQL = `UPDATE generation_pipeline SET test_cases_job_id = ?, updated_at = UTC_TIMESTAMP(6) WHERE id = ?`
	completeGenerationPipelineSQL        = `UPDATE generation_pipeline SET status = 'COMPLETED', updated_at = UTC_TIMESTAMP(6) WHERE id = ?`
)

// PublishGeneration performs the Atomic Publish Transaction (docs/technical/01-storage-and-
// workflows.md section 6) for one Job: it inserts the logical+revision row(s) that Job's
// job_kind owns, marks the Job COMPLETED, and completes its work item, all in one Platform
// MySQL transaction. Every insert targets a deterministically pre-allocated ID (see
// allocatePublishedID), and every insert statement is an upsert no-op on retry, so calling
// this twice for the same Job (for example after a crash right before commit) is safe.
func (store *GenerationStore) PublishGeneration(ctx context.Context, jobID string, lease GenerationLease) (generation.Job, error) {
	if _, err := ulid.ParseStrict(jobID); err != nil {
		return generation.Job{}, fmt.Errorf("invalid generation job ID: %w", err)
	}
	if lease.WorkItemID == "" || lease.Owner == "" || lease.FencingToken == 0 {
		return generation.Job{}, fmt.Errorf("complete generation lease is required")
	}

	transaction, err := store.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return generation.Job{}, fmt.Errorf("begin generation publish: %w", err)
	}
	defer transaction.Rollback()

	var kind, status string
	var pipelineID sql.NullString
	var repairRound, requestedCount, acceptedCount int
	var requestID, generatorConfigSHA256 string
	var requireManualReview bool
	var inputScenarioRevisionID, inputDatasetRevisionID sql.NullString
	if err := transaction.QueryRowContext(ctx, lockGenerationJobForPublishSQL, jobID).Scan(
		&kind, &pipelineID, &status, &repairRound, &requestedCount, &acceptedCount, &requestID,
		&inputScenarioRevisionID, &inputDatasetRevisionID, &generatorConfigSHA256, &requireManualReview,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return generation.Job{}, generation.ErrNotFound
		}
		return generation.Job{}, fmt.Errorf("lock generation job for publish: %w", err)
	}
	if status != string(generation.StatusPublishing) {
		return generation.Job{}, fmt.Errorf("%w: job status %s is not PUBLISHING", generation.ErrConflict, status)
	}
	if err := lockGenerationLease(ctx, transaction, jobID, lease); err != nil {
		return generation.Job{}, err
	}

	var requestDocument []byte
	var requestSHA256 string
	var descriptionTokens int
	var targetAgentID, databaseProfileID sql.NullString
	var generateDataset bool
	if err := transaction.QueryRowContext(ctx, selectGenerationRequestForPublishSQL, requestID).Scan(
		&requestDocument, &requestSHA256, &descriptionTokens, &targetAgentID, &generateDataset, &databaseProfileID,
	); err != nil {
		return generation.Job{}, fmt.Errorf("load generation request for publish: %w", err)
	}
	var request struct {
		AutoActivate bool `json:"auto_activate"`
	}
	if err := json.Unmarshal(requestDocument, &request); err != nil {
		return generation.Job{}, fmt.Errorf("decode generation request for publish: %w", err)
	}

	var newScenarioRevisionID, newDatasetRevisionID string
	switch generation.JobKind(kind) {
	case generation.KindScenario:
		newScenarioRevisionID, err = store.publishScenario(ctx, transaction, jobID, repairRound, revisionLifecycleStatus(request.AutoActivate))
		if err != nil {
			return generation.Job{}, err
		}
	case generation.KindDataset:
		if !inputScenarioRevisionID.Valid {
			return generation.Job{}, fmt.Errorf("DATASET job %s is missing input_scenario_revision_id", jobID)
		}
		newDatasetRevisionID, err = store.publishDataset(ctx, transaction, jobID, repairRound, inputScenarioRevisionID.String, revisionLifecycleStatus(request.AutoActivate))
		if err != nil {
			return generation.Job{}, err
		}
	case generation.KindTestCases:
		if !inputScenarioRevisionID.Valid || !inputDatasetRevisionID.Valid {
			return generation.Job{}, fmt.Errorf("TEST_CASES job %s is missing input revision references", jobID)
		}
		if err := store.publishTestCases(
			ctx, transaction, jobID, repairRound,
			inputScenarioRevisionID.String, inputDatasetRevisionID.String, request.AutoActivate,
		); err != nil {
			return generation.Job{}, err
		}
	default:
		return generation.Job{}, fmt.Errorf("unknown job kind %q", kind)
	}

	provenance, err := store.buildJobProvenance(ctx, transaction, jobID, generation.JobKind(kind), repairRound)
	if err != nil {
		return generation.Job{}, err
	}
	if _, err := transaction.ExecContext(ctx, setGenerationJobProvenanceSQL, []byte(provenance), jobID); err != nil {
		return generation.Job{}, fmt.Errorf("store job provenance: %w", err)
	}
	if pipelineID.Valid && generation.JobKind(kind) == generation.KindTestCases {
		if err := store.publishGenerationPackage(ctx, transaction, packagePublish{
			jobID: jobID, pipelineID: pipelineID.String, repairRound: repairRound,
			requestDocument: requestDocument, requestSHA256: requestSHA256, descriptionTokens: descriptionTokens,
			generatorConfigSHA256: generatorConfigSHA256,
			scenarioRevisionID:    inputScenarioRevisionID.String, datasetRevisionID: inputDatasetRevisionID.String,
			provenance: provenance,
		}); err != nil {
			return generation.Job{}, err
		}
	}

	if pipelineID.Valid {
		chain := pipelineChainContext{
			requestDocument: requestDocument, requestSHA256: requestSHA256, descriptionTokens: descriptionTokens,
			targetAgentID: nullStringPtr(targetAgentID), generateDataset: generateDataset,
			databaseProfileID: nullStringPtr(databaseProfileID), generatorConfigSHA256: generatorConfigSHA256,
			requireManualReview: requireManualReview,
		}
		if err := store.pipelineAfterPublish(
			ctx, transaction, generation.JobKind(kind), pipelineID.String,
			firstNonEmpty(newScenarioRevisionID, inputScenarioRevisionID.String),
			firstNonEmpty(newDatasetRevisionID, inputDatasetRevisionID.String),
			chain,
		); err != nil {
			return generation.Job{}, err
		}
	}

	result, err := transaction.ExecContext(ctx, completeGenerationPublishSQL, jobID)
	if err != nil {
		return generation.Job{}, fmt.Errorf("complete generation job: %w", err)
	}
	if err := requireSingleRow(result); err != nil {
		return generation.Job{}, fmt.Errorf("complete generation job: %w", err)
	}
	result, err = transaction.ExecContext(ctx, completeGenerationWorkSQL, lease.WorkItemID, lease.Owner, lease.FencingToken)
	if err != nil {
		return generation.Job{}, fmt.Errorf("complete generation work: %w", err)
	}
	if err := requireSingleRow(result); err != nil {
		return generation.Job{}, mysqlqueue.ErrStaleLease
	}

	job, err := scanGenerationJob(transaction.QueryRowContext(ctx, selectGenerationJobSQL, jobID))
	if err != nil {
		return generation.Job{}, fmt.Errorf("load published generation job: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return generation.Job{}, fmt.Errorf("commit generation publish: %w", err)
	}
	return job, nil
}

// nullStringPtr converts a sql.NullString read from a nullable column into the *string shape
// CreateJobInput and generationWorkPayload use.
func nullStringPtr(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

// firstNonEmpty returns candidate if it is non-empty, else fallback. Used to pick between a
// Revision ID this publish just created (candidate) and one that was already an input to this
// Job (fallback), whichever applies for the Job's own kind.
func firstNonEmpty(candidate, fallback string) string {
	if candidate != "" {
		return candidate
	}
	return fallback
}

// revisionLifecycleStatus is the status every Revision a publish creates starts in: ACTIVE when
// the original request asked for auto_activate, VALIDATED otherwise (docs/technical/
// 01-storage-and-workflows.md section 6, step 6). Applies to Scenario, Dataset, and Test Case
// Revisions alike (docs/architecture.md section 15.2).
func revisionLifecycleStatus(autoActivate bool) string {
	if autoActivate {
		return "ACTIVE"
	}
	return "VALIDATED"
}

// pipelineChainContext carries the fields a chained Job's own generation_request row must
// copy from the Generation Pipeline's originating request, so every Job in the pipeline is
// generated against the same description, target Agent, and Dataset source.
type pipelineChainContext struct {
	requestDocument       []byte
	requestSHA256         string
	descriptionTokens     int
	targetAgentID         *string
	generateDataset       bool
	databaseProfileID     *string
	generatorConfigSHA256 string
	requireManualReview   bool
}

// pipelineAfterPublish advances the Generation Pipeline the just-published Job belongs to: a
// published SCENARIO job creates the pipeline's DATASET job, a published DATASET job creates
// its TEST_CASES job (carrying the pipeline's requested_test_case_count), and a published
// TEST_CASES job marks the Pipeline COMPLETED. It runs inside PublishGeneration's own
// transaction, so a Job's publish and the creation of its successor Job are atomic together
// (docs/architecture.md section 6.1).
func (store *GenerationStore) pipelineAfterPublish(
	ctx context.Context, tx *sql.Tx, kind generation.JobKind, pipelineID string,
	publishedScenarioRevisionID, publishedDatasetRevisionID string, chain pipelineChainContext,
) error {
	var pipelineStatus string
	var requestedTestCaseCount int
	var scenarioJobID, datasetJobID, testCasesJobID sql.NullString
	if err := tx.QueryRowContext(ctx, lockGenerationPipelineForPublishSQL, pipelineID).Scan(
		&pipelineStatus, &requestedTestCaseCount, &scenarioJobID, &datasetJobID, &testCasesJobID,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: generation pipeline %s was not found", generation.ErrConflict, pipelineID)
		}
		return fmt.Errorf("lock generation pipeline: %w", err)
	}

	switch kind {
	case generation.KindScenario:
		if datasetJobID.Valid {
			return nil
		}
		jobID, err := store.createPipelineStageJob(
			ctx, tx, pipelineID, "pipeline:"+pipelineID+":DATASET", generation.KindDataset, 0,
			&publishedScenarioRevisionID, nil, chain,
		)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, setGenerationPipelineDatasetJobSQL, jobID, pipelineID); err != nil {
			return fmt.Errorf("link dataset job to pipeline: %w", err)
		}
	case generation.KindDataset:
		if testCasesJobID.Valid {
			return nil
		}
		jobID, err := store.createPipelineStageJob(
			ctx, tx, pipelineID, "pipeline:"+pipelineID+":TEST_CASES", generation.KindTestCases, requestedTestCaseCount,
			&publishedScenarioRevisionID, &publishedDatasetRevisionID, chain,
		)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, setGenerationPipelineTestCasesJobSQL, jobID, pipelineID); err != nil {
			return fmt.Errorf("link test cases job to pipeline: %w", err)
		}
	case generation.KindTestCases:
		if _, err := tx.ExecContext(ctx, completeGenerationPipelineSQL, pipelineID); err != nil {
			return fmt.Errorf("complete generation pipeline: %w", err)
		}
	}
	return nil
}

// createPipelineStageJob creates (or, on safe retry, loads) the next stage Job in a Generation
// Pipeline. It mirrors createGenerationPipeline's idempotent request/job/work insert sequence
// scoped to one explicit job_kind, running inside the caller's own Publish transaction so the
// new Job is created atomically with the predecessor Job's publish.
func (store *GenerationStore) createPipelineStageJob(
	ctx context.Context, tx *sql.Tx, pipelineID, idempotencyKey string, kind generation.JobKind, requestedTestCaseCount int,
	inputScenarioRevisionID, inputDatasetRevisionID *string, chain pipelineChainContext,
) (string, error) {
	ids, err := store.newGenerationIDs(3)
	if err != nil {
		return "", err
	}
	requestID, jobID, workID := ids[0], ids[1], ids[2]
	payload, err := json.Marshal(generationWorkPayload{
		JobID: jobID, JobKind: string(kind),
		InputScenarioRevisionID: inputScenarioRevisionID, InputDatasetRevisionID: inputDatasetRevisionID,
	})
	if err != nil {
		return "", fmt.Errorf("encode chained generation work: %w", err)
	}
	if _, err := tx.ExecContext(ctx, upsertGenerationRequestSQL,
		requestID, idempotencyKey, chain.requestDocument, chain.requestSHA256, chain.descriptionTokens,
		chain.targetAgentID, chain.generateDataset, chain.databaseProfileID,
	); err != nil {
		return "", fmt.Errorf("upsert chained generation request: %w", err)
	}
	var storedRequestID, storedRequestDigest string
	if err := tx.QueryRowContext(ctx, lockGenerationRequestSQL, idempotencyKey).Scan(&storedRequestID, &storedRequestDigest); err != nil {
		return "", fmt.Errorf("lock chained generation request: %w", err)
	}
	if storedRequestDigest != chain.requestSHA256 {
		return "", fmt.Errorf("%w: chained generation request digest mismatch", generation.ErrConflict)
	}
	if storedRequestID == requestID {
		if _, err := tx.ExecContext(ctx, insertGenerationJobSQL,
			jobID, requestID, string(kind), inputScenarioRevisionID, inputDatasetRevisionID,
			requestedTestCaseCount, chain.generatorConfigSHA256, chain.requireManualReview,
		); err != nil {
			return "", fmt.Errorf("insert chained generation job: %w", err)
		}
		if _, err := tx.ExecContext(ctx, setGenerationJobPipelineSQL, pipelineID, jobID); err != nil {
			return "", fmt.Errorf("link chained generation job to pipeline: %w", err)
		}
		if _, err := tx.ExecContext(ctx, insertGenerationWorkSQL,
			workID, jobID, "PENDING:repair-0", 0, 3, payload,
		); err != nil {
			return "", fmt.Errorf("enqueue chained generation work: %w", err)
		}
	}
	var existingJobID string
	if err := tx.QueryRowContext(ctx, selectGenerationJobIDByRequestSQL, storedRequestID).Scan(&existingJobID); err != nil {
		return "", fmt.Errorf("load chained generation job: %w", err)
	}
	return existingJobID, nil
}

// publishDraftKey mirrors pipeline.scenarioDraftKey/datasetDraftKey's convention (round 0 is
// the plain base key, later rounds are suffixed) - MUST stay in sync with those functions.
func publishDraftKey(base string, round int) string {
	if round == 0 {
		return base
	}
	return fmt.Sprintf("%s-round-%d", base, round)
}

// publishManifestKey mirrors pipeline.testCaseManifestKey - MUST stay in sync with it.
func publishManifestKey(round int) string {
	return fmt.Sprintf("round-%d", round)
}

func (store *GenerationStore) loadPublishArtifact(ctx context.Context, tx *sql.Tx, jobID, kind, key string) ([]byte, string, error) {
	var content []byte
	var digest string
	if err := tx.QueryRowContext(ctx, selectGenerationArtifactForPublishSQL, jobID, kind, key).Scan(&content, &digest); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", generation.ErrNotFound
		}
		return nil, "", fmt.Errorf("load draft %s/%s: %w", kind, key, err)
	}
	canonical, computed, err := generation.CanonicalJSON(content)
	if err != nil || computed != digest {
		return nil, "", fmt.Errorf("stored draft %s/%s failed its integrity check", kind, key)
	}
	return canonical, digest, nil
}

// loadAcceptedManifest returns the client_case_ids the final VALIDATING_TEST_CASES round
// accepted, in acceptance order.
func (store *GenerationStore) loadAcceptedManifest(ctx context.Context, tx *sql.Tx, jobID string, repairRound int) ([]string, error) {
	content, _, err := store.loadPublishArtifact(ctx, tx, jobID, generation.ArtifactKindTestCaseManifest, publishManifestKey(repairRound))
	if err != nil {
		return nil, err
	}
	var manifest struct {
		Accepted []string `json:"accepted"`
	}
	if err := json.Unmarshal(content, &manifest); err != nil {
		return nil, fmt.Errorf("decode Test Case manifest: %w", err)
	}
	return manifest.Accepted, nil
}

// allocatePublishedID deterministically and idempotently assigns a ULID to one logical row
// this publish needs to create, so retrying the whole transaction after a crash reuses the
// same ID instead of creating a duplicate row under a new ID.
func (store *GenerationStore) allocatePublishedID(ctx context.Context, tx *sql.Tx, jobID, key string) (string, error) {
	var content []byte
	err := tx.QueryRowContext(ctx, selectPublishedIDMarkerSQL, jobID, key).Scan(&content)
	if err == nil {
		var marker struct {
			ID string `json:"id"`
		}
		if jsonErr := json.Unmarshal(content, &marker); jsonErr != nil {
			return "", fmt.Errorf("decode published ID marker %s: %w", key, jsonErr)
		}
		return marker.ID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("load published ID marker %s: %w", key, err)
	}
	id, err := store.newID()
	if err != nil {
		return "", err
	}
	artifactID, err := store.newID()
	if err != nil {
		return "", err
	}
	content, err = json.Marshal(map[string]string{"id": id})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(content)
	if _, err := tx.ExecContext(ctx, insertPublishedIDMarkerSQL, artifactID, jobID, key, content, hex.EncodeToString(digest[:])); err != nil {
		return "", fmt.Errorf("save published ID marker %s: %w", key, err)
	}
	return id, nil
}

// publishScenario inserts the one Scenario and Scenario Revision a SCENARIO kind Job creates,
// and returns the published Scenario Revision's ID. quality_level is SYNTHETIC when any
// business rule is a GENERATED_ASSUMPTION, SOURCE_GROUNDED otherwise
// (docs/technical/03-generation-and-validation.md section 2).
func (store *GenerationStore) publishScenario(ctx context.Context, tx *sql.Tx, jobID string, repairRound int, lifecycleStatus string) (string, error) {
	content, digest, err := store.loadPublishArtifact(ctx, tx, jobID, generation.ArtifactKindScenario, publishDraftKey("scenario", repairRound))
	if err != nil {
		return "", err
	}
	var scenario struct {
		BusinessRules []struct {
			Source string `json:"source"`
		} `json:"business_rules"`
	}
	if err := json.Unmarshal(content, &scenario); err != nil {
		return "", fmt.Errorf("decode Scenario draft for publish: %w", err)
	}
	qualityLevel := "SOURCE_GROUNDED"
	for _, rule := range scenario.BusinessRules {
		if rule.Source == "GENERATED_ASSUMPTION" {
			qualityLevel = "SYNTHETIC"
			break
		}
	}

	scenarioID, err := store.allocatePublishedID(ctx, tx, jobID, "scenario_id")
	if err != nil {
		return "", err
	}
	revisionID, err := store.allocatePublishedID(ctx, tx, jobID, "scenario_revision_id")
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, insertScenarioSQL, scenarioID); err != nil {
		return "", fmt.Errorf("insert scenario: %w", err)
	}
	if _, err := tx.ExecContext(ctx, insertScenarioRevisionSQL, revisionID, scenarioID, jobID, qualityLevel, lifecycleStatus, content, digest); err != nil {
		return "", fmt.Errorf("insert scenario revision: %w", err)
	}
	return revisionID, nil
}

// publishDataset inserts the one Dataset and Dataset Revision a DATASET kind Job creates,
// under the logical Scenario its input_scenario_revision_id belongs to, and returns the
// published Dataset Revision's ID.
func (store *GenerationStore) publishDataset(ctx context.Context, tx *sql.Tx, jobID string, repairRound int, inputScenarioRevisionID, lifecycleStatus string) (string, error) {
	datasetContent, datasetDigest, err := store.loadPublishArtifact(ctx, tx, jobID, generation.ArtifactKindDataset, publishDraftKey("dataset", repairRound))
	if err != nil {
		return "", err
	}
	var dataset struct {
		SchemaSQL   string          `json:"schema_sql"`
		SeedSQL     string          `json:"seed_sql"`
		Constraints json.RawMessage `json:"constraints"`
		Metadata    json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal(datasetContent, &dataset); err != nil {
		return "", fmt.Errorf("decode Dataset draft for publish: %w", err)
	}
	executionContent, _, err := store.loadPublishArtifact(ctx, tx, jobID, generation.ArtifactKindDatasetExecution, publishDraftKey("dataset", repairRound))
	if err != nil {
		return "", err
	}
	var execution generation.DatasetExecutionRecord
	execution, err = generation.DecodeDatasetExecutionRecord(executionContent)
	if err != nil {
		return "", fmt.Errorf("decode Dataset execution record for publish: %w", err)
	}

	var scenarioID string
	if err := tx.QueryRowContext(ctx, selectScenarioIDForRevisionSQL, inputScenarioRevisionID).Scan(&scenarioID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("%w: input Scenario Revision was not found", generation.ErrConflict)
		}
		return "", fmt.Errorf("load scenario for dataset publish: %w", err)
	}

	datasetID, err := store.allocatePublishedID(ctx, tx, jobID, "dataset_id")
	if err != nil {
		return "", err
	}
	// The revision ID is the one the execution schemas were named from, so every Target's schema name
	// dataset_<revision ULID> identifies this revision.
	revisionID := execution.DatasetRevisionID
	if _, err := tx.ExecContext(ctx, insertDatasetSQL, datasetID, scenarioID); err != nil {
		return "", fmt.Errorf("insert dataset: %w", err)
	}
	if _, err := tx.ExecContext(ctx, insertDatasetRevisionSQL,
		revisionID, datasetID, jobID, inputScenarioRevisionID,
		dataset.SchemaSQL, dataset.SeedSQL, []byte(dataset.Constraints), []byte(dataset.Metadata), datasetDigest,
		execution.Targets[0].Database, lifecycleStatus,
	); err != nil {
		return "", fmt.Errorf("insert dataset revision: %w", err)
	}
	for ordinal, target := range execution.Targets {
		if _, err := tx.ExecContext(ctx, insertDatasetRevisionTargetSQL, revisionID, target.ValidationDBID, ordinal); err != nil {
			return "", fmt.Errorf("bind dataset revision to validation MySQL: %w", err)
		}
	}
	return revisionID, nil
}

// publishTestCases inserts one Test Case and Test Case Revision per candidate the final round
// of VALIDATING_TEST_CASES accepted, under the logical Scenario and Dataset its input revision
// IDs belong to. lifecycle_status is ACTIVE when the original request asked for auto_activate,
// VALIDATED otherwise (docs/technical/01-storage-and-workflows.md section 6 step 6). Published
// content strips the create_test_case transport fields (job_id, client_case_id) that
// test-case.schema.json does not allow; scenario_revision_id/dataset_revision_id already equal
// the real published IDs because Test Case generation validated candidates against them
// directly (see internal/generation/pipeline/testcase.go loadTestCaseContext).
func (store *GenerationStore) publishTestCases(
	ctx context.Context, tx *sql.Tx, jobID string, repairRound int,
	inputScenarioRevisionID, inputDatasetRevisionID string, autoActivate bool,
) error {
	accepted, err := store.loadAcceptedManifest(ctx, tx, jobID, repairRound)
	if err != nil {
		return err
	}

	var scenarioID string
	if err := tx.QueryRowContext(ctx, selectScenarioIDForRevisionSQL, inputScenarioRevisionID).Scan(&scenarioID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: input Scenario Revision was not found", generation.ErrConflict)
		}
		return fmt.Errorf("load scenario for test case publish: %w", err)
	}
	var datasetID string
	if err := tx.QueryRowContext(ctx, selectDatasetIDForRevisionSQL, inputDatasetRevisionID).Scan(&datasetID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: input Dataset Revision was not found", generation.ErrConflict)
		}
		return fmt.Errorf("load dataset for test case publish: %w", err)
	}

	lifecycleStatus := revisionLifecycleStatus(autoActivate)

	for _, clientCaseID := range accepted {
		content, _, err := store.loadPublishArtifact(ctx, tx, jobID, generation.ArtifactKindTestCase, clientCaseID)
		if err != nil {
			return fmt.Errorf("load accepted candidate %s: %w", clientCaseID, err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(content, &fields); err != nil {
			return fmt.Errorf("decode accepted candidate %s: %w", clientCaseID, err)
		}
		delete(fields, "job_id")
		delete(fields, "client_case_id")
		published, err := json.Marshal(fields)
		if err != nil {
			return fmt.Errorf("encode published candidate %s: %w", clientCaseID, err)
		}
		canonicalPublished, publishedDigest, err := generation.CanonicalJSON(published)
		if err != nil {
			return fmt.Errorf("canonicalize published candidate %s: %w", clientCaseID, err)
		}
		var candidate struct {
			SchemaVersion string `json:"schema_version"`
		}
		if err := json.Unmarshal(canonicalPublished, &candidate); err != nil {
			return fmt.Errorf("decode published candidate %s: %w", clientCaseID, err)
		}

		testCaseID, err := store.allocatePublishedID(ctx, tx, jobID, "test_case_id:"+clientCaseID)
		if err != nil {
			return err
		}
		revisionID, err := store.allocatePublishedID(ctx, tx, jobID, "test_case_revision_id:"+clientCaseID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, insertTestCaseSQL, testCaseID, scenarioID, datasetID); err != nil {
			return fmt.Errorf("insert test case %s: %w", clientCaseID, err)
		}
		if _, err := tx.ExecContext(ctx, insertTestCaseRevisionSQL,
			revisionID, testCaseID, jobID, inputScenarioRevisionID, inputDatasetRevisionID,
			candidate.SchemaVersion,
			canonicalPublished, publishedDigest, lifecycleStatus,
		); err != nil {
			return fmt.Errorf("insert test case revision %s: %w", clientCaseID, err)
		}
	}
	return nil
}
