package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"agent-test-platform/internal/generation"

	"github.com/oklog/ulid/v2"
)

const (
	selectArtifactValidationForPublishSQL = `SELECT validation_json FROM generation_artifact_draft
WHERE generation_job_id = ? AND artifact_kind = ? AND artifact_key = ?`
	setGenerationJobProvenanceSQL = `UPDATE generation_job
SET provenance_json = ?, updated_at = UTC_TIMESTAMP(6) WHERE id = ?`
	selectCompletedJobProvenanceSQL      = `SELECT provenance_json FROM generation_job WHERE id = ? AND status = 'COMPLETED'`
	selectScenarioRevisionDigestSQL      = `SELECT content_sha256 FROM scenario_revision WHERE id = ?`
	selectDatasetRevisionDigestSQL       = `SELECT content_sha256 FROM dataset_revision WHERE id = ?`
	selectTestCaseRevisionDigestSQL      = `SELECT content_sha256 FROM test_case_revision WHERE id = ?`
	insertGenerationPackageSQL           = `INSERT INTO generation_package (id, pipeline_id, scenario_revision_id, dataset_revision_id, manifest_json, manifest_sha256) VALUES (?, ?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE id = id`
	insertGenerationPackageTestCaseSQL   = `INSERT INTO generation_package_test_case (generation_package_id, test_case_revision_id, ordinal) VALUES (?, ?, ?) ON DUPLICATE KEY UPDATE ordinal = ordinal`
	setGenerationJobPublishedPackageSQL  = `UPDATE generation_job SET published_package_id = ?, updated_at = UTC_TIMESTAMP(6) WHERE id = ?`
	selectGenerationPackageByPipelineSQL = `SELECT id, scenario_revision_id, dataset_revision_id, manifest_json, manifest_sha256
FROM generation_package WHERE pipeline_id = ?`
	selectGenerationPackageTestCasesSQL = `SELECT test_case_revision_id FROM generation_package_test_case
WHERE generation_package_id = ? ORDER BY ordinal`
	selectPackageJobIDSQL = `SELECT id FROM generation_job WHERE published_package_id = ?`
)

// packagePublish is what a TEST_CASES Job that belongs to a Generation Pipeline writes its
// Generation Package from. Every value is either this publish's own output or durable row state.
type packagePublish struct {
	jobID                 string
	pipelineID            string
	repairRound           int
	requestDocument       []byte
	requestSHA256         string
	descriptionTokens     int
	generatorConfigSHA256 string
	scenarioRevisionID    string
	datasetRevisionID     string
	provenance            json.RawMessage
}

// buildJobProvenance records how a Job's published output was produced and accepted: the
// producer (the Scenario or Dataset validation record, or the final Test Case round's generator
// record) and the accepted Critic review when a Critic was configured. It is stored on the Job at
// publish time, so the Generation Manifest can be assembled after the Job's drafts have expired.
func (store *GenerationStore) buildJobProvenance(ctx context.Context, tx *sql.Tx, jobID string, kind generation.JobKind, repairRound int) (json.RawMessage, error) {
	var producer json.RawMessage
	var err error
	var criticScope string
	switch kind {
	case generation.KindScenario:
		criticScope = generation.CriticScopeScenario
		producer, err = loadArtifactValidation(ctx, tx, jobID, generation.ArtifactKindScenario, publishDraftKey("scenario", repairRound))
	case generation.KindDataset:
		criticScope = generation.CriticScopeDataset
		producer, err = loadArtifactValidation(ctx, tx, jobID, generation.ArtifactKindDataset, publishDraftKey("dataset", repairRound))
	case generation.KindTestCases:
		criticScope = generation.CriticScopeTestCases
		producer, err = store.loadTestCaseGenerator(ctx, tx, jobID, repairRound)
	default:
		return nil, fmt.Errorf("unknown job kind %q", kind)
	}
	if err != nil {
		return nil, fmt.Errorf("load %s producer provenance: %w", kind, err)
	}
	critic, err := loadArtifactValidation(ctx, tx, jobID, generation.ArtifactKindCriticReview, generation.CriticReviewKey(criticScope, repairRound))
	if errors.Is(err, generation.ErrNotFound) {
		critic = nil
	} else if err != nil {
		return nil, fmt.Errorf("load %s Critic provenance: %w", kind, err)
	}
	encoded, err := json.Marshal(map[string]any{
		"schema_version": "1.0", "job_kind": kind, "producer": producer, "critic": critic,
	})
	if err != nil {
		return nil, fmt.Errorf("encode %s provenance: %w", kind, err)
	}
	canonical, _, err := generation.CanonicalJSON(encoded)
	if err != nil {
		return nil, fmt.Errorf("canonicalize %s provenance: %w", kind, err)
	}
	return canonical, nil
}

// loadTestCaseGenerator returns the generator record of one Test Case round's manifest.
func (store *GenerationStore) loadTestCaseGenerator(ctx context.Context, tx *sql.Tx, jobID string, repairRound int) (json.RawMessage, error) {
	content, _, err := store.loadPublishArtifact(ctx, tx, jobID, generation.ArtifactKindTestCaseManifest, publishManifestKey(repairRound))
	if err != nil {
		return nil, err
	}
	var manifest struct {
		Generator json.RawMessage `json:"generator"`
	}
	if err := json.Unmarshal(content, &manifest); err != nil {
		return nil, fmt.Errorf("decode Test Case manifest: %w", err)
	}
	if len(manifest.Generator) == 0 || string(manifest.Generator) == "null" {
		return nil, fmt.Errorf("Test Case manifest round %d has no generator record", repairRound)
	}
	return manifest.Generator, nil
}

// loadArtifactValidation returns one draft artifact's canonical validation record. Publishing
// reads only its own Job's drafts, inside the publish transaction, before they can expire.
func loadArtifactValidation(ctx context.Context, tx *sql.Tx, jobID, kind, key string) (json.RawMessage, error) {
	var validation []byte
	if err := tx.QueryRowContext(ctx, selectArtifactValidationForPublishSQL, jobID, kind, key).Scan(&validation); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, generation.ErrNotFound
		}
		return nil, fmt.Errorf("load validation record %s/%s: %w", kind, key, err)
	}
	if len(validation) == 0 {
		return nil, fmt.Errorf("%s/%s has no validation record", kind, key)
	}
	canonical, _, err := generation.CanonicalJSON(validation)
	if err != nil {
		return nil, fmt.Errorf("canonicalize validation record %s/%s: %w", kind, key, err)
	}
	return canonical, nil
}

// loadCompletedJobProvenance returns the provenance a completed pipeline Job stored at its publish.
func loadCompletedJobProvenance(ctx context.Context, tx *sql.Tx, jobID string) (json.RawMessage, error) {
	var provenance []byte
	if err := tx.QueryRowContext(ctx, selectCompletedJobProvenanceSQL, jobID).Scan(&provenance); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: pipeline job %s is not COMPLETED", generation.ErrConflict, jobID)
		}
		return nil, fmt.Errorf("load provenance of job %s: %w", jobID, err)
	}
	if len(provenance) == 0 {
		return nil, fmt.Errorf("completed job %s has no provenance", jobID)
	}
	canonical, _, err := generation.CanonicalJSON(provenance)
	if err != nil {
		return nil, fmt.Errorf("canonicalize provenance of job %s: %w", jobID, err)
	}
	return canonical, nil
}

// loadPublishedTestCases lists the Test Case Revisions this round's publish created, in accepted
// order. It reads the deterministic published-ID markers publishTestCases wrote, so a retried
// publish sees the same IDs.
func (store *GenerationStore) loadPublishedTestCases(ctx context.Context, tx *sql.Tx, jobID string, repairRound int) ([]generation.ManifestTestCase, error) {
	accepted, err := store.loadAcceptedManifest(ctx, tx, jobID, repairRound)
	if err != nil {
		return nil, err
	}
	testCases := make([]generation.ManifestTestCase, 0, len(accepted))
	for index, clientCaseID := range accepted {
		var content []byte
		if err := tx.QueryRowContext(ctx, selectPublishedIDMarkerSQL, jobID, "test_case_revision_id:"+clientCaseID).Scan(&content); err != nil {
			return nil, fmt.Errorf("load published ID for test case %s: %w", clientCaseID, err)
		}
		var marker struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(content, &marker); err != nil {
			return nil, fmt.Errorf("decode published ID for test case %s: %w", clientCaseID, err)
		}
		digest, err := queryDigest(ctx, tx, selectTestCaseRevisionDigestSQL, marker.ID)
		if err != nil {
			return nil, fmt.Errorf("load digest of test case %s: %w", clientCaseID, err)
		}
		testCases = append(testCases, generation.ManifestTestCase{Ordinal: index + 1, RevisionID: marker.ID, SHA256: digest})
	}
	return testCases, nil
}

func queryDigest(ctx context.Context, tx *sql.Tx, query, revisionID string) (string, error) {
	var digest string
	if err := tx.QueryRowContext(ctx, query, revisionID).Scan(&digest); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", generation.ErrNotFound
		}
		return "", err
	}
	return digest, nil
}

// publishGenerationPackage writes the Generation Manifest and the package rows covering all three
// chained Jobs of a Pipeline (docs/technical/01-storage-and-workflows.md section 6, step 5), and
// links the package from the publishing Job (step 7). It runs in the TEST_CASES Job's publish
// transaction, so the package exists exactly when the Pipeline's output is complete.
func (store *GenerationStore) publishGenerationPackage(ctx context.Context, tx *sql.Tx, publish packagePublish) error {
	var pipelineStatus string
	var requestedTestCaseCount int
	var scenarioJobID, datasetJobID, testCasesJobID sql.NullString
	if err := tx.QueryRowContext(ctx, lockGenerationPipelineForPublishSQL, publish.pipelineID).Scan(
		&pipelineStatus, &requestedTestCaseCount, &scenarioJobID, &datasetJobID, &testCasesJobID,
	); err != nil {
		return fmt.Errorf("lock generation pipeline for package: %w", err)
	}
	if !scenarioJobID.Valid || !datasetJobID.Valid {
		return fmt.Errorf("%w: generation pipeline %s is missing its Scenario or Dataset job", generation.ErrConflict, publish.pipelineID)
	}
	scenarioProvenance, err := loadCompletedJobProvenance(ctx, tx, scenarioJobID.String)
	if err != nil {
		return err
	}
	datasetProvenance, err := loadCompletedJobProvenance(ctx, tx, datasetJobID.String)
	if err != nil {
		return err
	}
	scenarioDigest, err := queryDigest(ctx, tx, selectScenarioRevisionDigestSQL, publish.scenarioRevisionID)
	if err != nil {
		return fmt.Errorf("load Scenario Revision digest: %w", err)
	}
	datasetDigest, err := queryDigest(ctx, tx, selectDatasetRevisionDigestSQL, publish.datasetRevisionID)
	if err != nil {
		return fmt.Errorf("load Dataset Revision digest: %w", err)
	}
	testCases, err := store.loadPublishedTestCases(ctx, tx, publish.jobID, publish.repairRound)
	if err != nil {
		return err
	}
	normalized, err := generation.NormalizeRequest(publish.requestDocument)
	if err != nil {
		return fmt.Errorf("normalize request for manifest: %w", err)
	}
	config, err := json.Marshal(normalized)
	if err != nil {
		return fmt.Errorf("encode request config for manifest: %w", err)
	}

	manifest, manifestDigest, err := generation.BuildGenerationManifest(generation.ManifestInput{
		PipelineID:        publish.pipelineID,
		RequestSHA256:     publish.requestSHA256,
		DescriptionTokens: publish.descriptionTokens,
		RequestConfig:     config,
		// Generator config is the Job's own (every Job of a Pipeline shares one request and config).
		GeneratorConfigSHA256: publish.generatorConfigSHA256,
		Stages: []generation.ManifestStage{
			{JobID: scenarioJobID.String, JobKind: generation.KindScenario, Provenance: scenarioProvenance},
			{JobID: datasetJobID.String, JobKind: generation.KindDataset, Provenance: datasetProvenance},
			{JobID: publish.jobID, JobKind: generation.KindTestCases, Provenance: publish.provenance},
		},
		Scenario:  generation.ManifestRevision{RevisionID: publish.scenarioRevisionID, SHA256: scenarioDigest},
		Dataset:   generation.ManifestRevision{RevisionID: publish.datasetRevisionID, SHA256: datasetDigest},
		TestCases: testCases,
	})
	if err != nil {
		return fmt.Errorf("build generation manifest: %w", err)
	}
	if err := store.validator.ValidateJSON(generation.ManifestSchema, manifest); err != nil {
		return fmt.Errorf("generation manifest does not match its contract: %w", err)
	}

	packageID, err := store.allocatePublishedID(ctx, tx, publish.jobID, "generation_package_id")
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, insertGenerationPackageSQL,
		packageID, publish.pipelineID, publish.scenarioRevisionID, publish.datasetRevisionID, []byte(manifest), manifestDigest,
	); err != nil {
		return fmt.Errorf("insert generation package: %w", err)
	}
	for _, testCase := range testCases {
		if _, err := tx.ExecContext(ctx, insertGenerationPackageTestCaseSQL, packageID, testCase.RevisionID, testCase.Ordinal); err != nil {
			return fmt.Errorf("insert generation package test case %d: %w", testCase.Ordinal, err)
		}
	}
	if _, err := tx.ExecContext(ctx, setGenerationJobPublishedPackageSQL, packageID, publish.jobID); err != nil {
		return fmt.Errorf("link generation package to job: %w", err)
	}
	return nil
}

// GetGenerationPackage returns the immutable Generation Package of the Pipeline a Job belongs to
// (contracts/openapi.yaml getGenerationPackage). Any Job of a Pipeline resolves to the same
// Pipeline's package. A Job outside a Pipeline, or a Pipeline whose TEST_CASES Job has not
// published yet, has no package and returns ErrNotFound.
func (store *GenerationStore) GetGenerationPackage(ctx context.Context, jobID string) (generation.Package, error) {
	if _, err := ulid.ParseStrict(jobID); err != nil {
		return generation.Package{}, generation.ErrNotFound
	}
	job, err := scanGenerationJob(store.database.QueryRowContext(ctx, selectGenerationJobSQL, jobID))
	if errors.Is(err, sql.ErrNoRows) {
		return generation.Package{}, generation.ErrNotFound
	}
	if err != nil {
		return generation.Package{}, fmt.Errorf("load generation job: %w", err)
	}
	if job.PipelineID == nil {
		return generation.Package{}, generation.ErrNotFound
	}

	var packageID string
	var pkg generation.Package
	var manifest []byte
	if err := store.database.QueryRowContext(ctx, selectGenerationPackageByPipelineSQL, *job.PipelineID).Scan(
		&packageID, &pkg.ScenarioRevisionID, &pkg.DatasetRevisionID, &manifest, &pkg.ManifestSHA256,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return generation.Package{}, generation.ErrNotFound
		}
		return generation.Package{}, fmt.Errorf("load generation package: %w", err)
	}
	canonical, digest, err := generation.CanonicalJSON(manifest)
	if err != nil || digest != pkg.ManifestSHA256 {
		return generation.Package{}, fmt.Errorf("stored generation manifest failed its integrity check")
	}
	pkg.Manifest = canonical

	if err := store.database.QueryRowContext(ctx, selectPackageJobIDSQL, packageID).Scan(&pkg.JobID); err != nil {
		return generation.Package{}, fmt.Errorf("load package job: %w", err)
	}
	rows, err := store.database.QueryContext(ctx, selectGenerationPackageTestCasesSQL, packageID)
	if err != nil {
		return generation.Package{}, fmt.Errorf("load package test cases: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var revisionID string
		if err := rows.Scan(&revisionID); err != nil {
			return generation.Package{}, fmt.Errorf("scan package test case: %w", err)
		}
		pkg.TestCaseRevisionIDs = append(pkg.TestCaseRevisionIDs, revisionID)
	}
	if err := rows.Err(); err != nil {
		return generation.Package{}, fmt.Errorf("read package test cases: %w", err)
	}
	return pkg, nil
}
