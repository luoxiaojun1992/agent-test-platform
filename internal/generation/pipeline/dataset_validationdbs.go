package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"agent-test-platform/internal/generation"
	"agent-test-platform/internal/sandbox"
)

// TargetResolver returns the administrator connection of a registered validation MySQL by ID
// (docs/architecture.md section 16.5). The worker builds it from the Target registry; tests substitute fakes.
type TargetResolver interface {
	ValidationDB(ctx context.Context, targetID string) (ValidationDB, error)
}

// WithValidationDB configures the validation MySQL instances the Dataset and Test Case stages validate on. A Dataset
// is validated on every Target its request named, in request order.
func WithValidationDB(resolver TargetResolver) Option {
	return func(processor *Processor) error {
		if resolver == nil {
			return fmt.Errorf("validation MySQL resolver is required")
		}
		processor.targets = resolver
		return nil
	}
}

// openPrimaryDataset opens the execution schema of a Dataset Revision on its first Target. Every bound Target
// holds identical content, so the Dataset Critic and the Test Case stage read from the first one.
func (processor *Processor) openPrimaryDataset(ctx context.Context, targetID, database string) (*sql.DB, error) {
	connection, err := processor.targets.ValidationDB(ctx, targetID)
	if err != nil {
		return nil, fmt.Errorf("resolve validation MySQL %s: %w", targetID, err)
	}
	return connection.OpenDatabase(ctx, database)
}

// requestTargetIDs returns the validation MySQL IDs the stage's Generation request names, in request order. The
// normalized request is the stage's own snapshot, so its digest is checked like every other stage input.
func (processor *Processor) requestTargetIDs(ctx context.Context, stage generation.StageRequest) ([]string, *generation.StageResult, error) {
	snapshot, err := processor.store.LoadGenerationArtifact(ctx, stage.JobID, generation.ArtifactKindNormalizedRequest, normalizedRequestKey)
	if errors.Is(err, generation.ErrNotFound) {
		result := failure("REFERENCE_NOT_FOUND", "Normalized Generation request was not found.", false, nil)
		return nil, &result, nil
	}
	if err != nil {
		return nil, nil, err
	}
	canonical, digest, err := generation.CanonicalJSON(snapshot.ContentJSON)
	if err != nil || digest != snapshot.SHA256 {
		result := failure("ARTIFACT_INTEGRITY_ERROR", "Normalized request failed its integrity check.", false, nil)
		return nil, &result, nil
	}
	var normalized generation.NormalizedRequest
	if err := json.Unmarshal(canonical, &normalized); err != nil {
		return nil, nil, fmt.Errorf("decode normalized request: %w", err)
	}
	if len(normalized.ValidationDBIDs) == 0 {
		result := failure("VALIDATION_DB_NOT_BOUND", "The Generation request names no validation MySQL.", false, nil)
		return nil, &result, nil
	}
	return normalized.ValidationDBIDs, nil, nil
}

// storedDatasetExecution decodes a stored execution record and checks it against the request: it must list the
// request's Targets in order, and every database must be the execution schema name derived from its revision ID.
func storedDatasetExecution(content json.RawMessage, requestTargetIDs []string) (generation.DatasetExecutionRecord, error) {
	record, err := generation.DecodeDatasetExecutionRecord(content)
	if err != nil {
		return generation.DatasetExecutionRecord{}, err
	}
	identifiers, err := sandbox.DeriveDatasetIdentifiers(record.DatasetRevisionID)
	if err != nil {
		return generation.DatasetExecutionRecord{}, fmt.Errorf("derive Dataset execution identifiers: %w", err)
	}
	if len(record.Targets) != len(requestTargetIDs) {
		return generation.DatasetExecutionRecord{}, fmt.Errorf("%w: the record lists %d Targets, the request names %d",
			generation.ErrInvalidExecutionRecord, len(record.Targets), len(requestTargetIDs))
	}
	for index, target := range record.Targets {
		if target.ValidationDBID != requestTargetIDs[index] {
			return generation.DatasetExecutionRecord{}, fmt.Errorf("%w: Target %d is %s, the request names %s",
				generation.ErrInvalidExecutionRecord, index, target.ValidationDBID, requestTargetIDs[index])
		}
		if target.Database != identifiers.Database {
			return generation.DatasetExecutionRecord{}, fmt.Errorf("%w: database %q is not %q",
				generation.ErrInvalidExecutionRecord, target.Database, identifiers.Database)
		}
	}
	return record, nil
}

// loadDatasetOnTarget creates the execution schema of one Dataset Revision on one Target, loads schema.sql and
// seed.sql there, and evaluates the AFTER_SEED constraints. It returns the Target's entry in the execution record.
// The stage result is non-nil when the attempt ends here: a rejection when the content fails on this Target, or a
// Target failure. An error means the stage was interrupted or infrastructure failed. A schema loaded on an earlier
// Target, or a failed one, is kept for manual deletion (docs/technical/06 section 6).
func (processor *Processor) loadDatasetOnTarget(ctx context.Context, stage generation.StageRequest, targetID, revisionID string, schema, seed []string, checks []datasetConstraintCheck) (generation.DatasetExecutionTarget, *generation.StageResult, error) {
	connection, err := processor.targets.ValidationDB(ctx, targetID)
	if err != nil {
		if ctx.Err() != nil {
			return generation.DatasetExecutionTarget{}, nil, err
		}
		result := failure("VALIDATION_DB_CONFIGURATION_INVALID", "A registered validation MySQL could not be opened.", false,
			map[string]any{"validation_db_id": targetID, "reason": err.Error()})
		return generation.DatasetExecutionTarget{}, &result, nil
	}
	identifiers, err := sandbox.ProvisionDataset(ctx, connection, revisionID)
	if err != nil {
		if ctx.Err() != nil {
			return generation.DatasetExecutionTarget{}, nil, err
		}
		result := failure("VALIDATION_DB_UNAVAILABLE", "Could not provision the Dataset execution schema.", false,
			map[string]any{"validation_db_id": targetID, "reason": err.Error()})
		return generation.DatasetExecutionTarget{}, &result, nil
	}

	database, err := connection.OpenDatabase(ctx, identifiers.Database)
	if err != nil {
		return generation.DatasetExecutionTarget{}, nil, fmt.Errorf("open Dataset execution connection: %w", err)
	}
	violations, executeErr := loadDatasetFixture(ctx, database, schema, seed, checks)
	database.Close()
	if executeErr != nil && ctx.Err() != nil {
		// The stage was cut short while the schema was loading. That is an interruption, not a validation
		// rejection, so it must not spend a repair round.
		return generation.DatasetExecutionTarget{}, nil, executeErr
	}
	if executeErr != nil {
		result, err := processor.rejectDataset(ctx, stage, "DATASET_EXECUTION_FAILED: "+executeErr.Error())
		return generation.DatasetExecutionTarget{}, result, err
	}
	if len(violations) > 0 {
		result, err := processor.rejectDataset(ctx, stage, "DATASET_INVARIANT_VIOLATION: "+strings.Join(violations, "; "))
		return generation.DatasetExecutionTarget{}, result, err
	}
	return generation.DatasetExecutionTarget{ValidationDBID: targetID, Database: identifiers.Database}, nil, nil
}

// rejectDataset records one retryable rejection of the Dataset draft and returns the stage result that spends a
// repair round on it.
func (processor *Processor) rejectDataset(ctx context.Context, stage generation.StageRequest, reason string) (*generation.StageResult, error) {
	if err := processor.saveDatasetRejection(ctx, stage, stage.RepairRound, []string{reason}); err != nil {
		return nil, err
	}
	result := generation.StageResult{Transition: generation.TransitionInput{Event: generation.EventValidationRejected, AllErrorsRetryable: true}}
	return &result, nil
}
