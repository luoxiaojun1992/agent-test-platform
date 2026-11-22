package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	"agent-test-platform/internal/jobs/mysqlqueue"
	"agent-test-platform/internal/run"
)

const (
	runLockLeaseSQL = `SELECT aggregate_type, aggregate_id FROM work_item WHERE id = ? AND status = 'LEASED' AND lease_owner = ? AND fencing_token = ? FOR UPDATE`
	runCompleteSQL  = `UPDATE work_item SET status='DONE', lease_owner=NULL, lease_until=NULL, updated_at=UTC_TIMESTAMP(6) WHERE id=? AND status='LEASED' AND lease_owner=? AND fencing_token=?`
	runRetrySQL     = `UPDATE work_item SET status='READY', lease_owner=NULL, lease_until=NULL, available_at=TIMESTAMPADD(MICROSECOND, ?, UTC_TIMESTAMP(6)), last_error_json=?, updated_at=UTC_TIMESTAMP(6) WHERE id=? AND status='LEASED' AND lease_owner=? AND fencing_token=?`
	runDeadSQL      = `UPDATE work_item SET status='DEAD', lease_owner=NULL, lease_until=NULL, last_error_json=?, updated_at=UTC_TIMESTAMP(6) WHERE id=? AND status='LEASED' AND lease_owner=? AND fencing_token=?`
	runEnqueueSQL   = `INSERT INTO work_item (id, queue_name, aggregate_type, aggregate_id, stage_key, status, priority, available_at, fencing_token, attempt_count, max_attempts, payload_json) VALUES (?, ?, ?, ?, ?, 'READY', 0, UTC_TIMESTAMP(6), 0, 0, 3, ?)`
)

// lockRunLease verifies the fenced lease and that it belongs to runID, within tx.
func lockRunLease(ctx context.Context, tx *sql.Tx, runID string, lease run.Lease) error {
	if lease.WorkItemID == "" || lease.Owner == "" || lease.FencingToken == 0 {
		return mysqlqueue.ErrStaleLease
	}
	var aggregateType, aggregateID string
	err := tx.QueryRowContext(ctx, runLockLeaseSQL, lease.WorkItemID, lease.Owner, lease.FencingToken).Scan(&aggregateType, &aggregateID)
	if errors.Is(err, sql.ErrNoRows) {
		return mysqlqueue.ErrStaleLease
	}
	if err != nil {
		return err
	}
	if aggregateType != run.AggregateType || aggregateID != runID {
		return mysqlqueue.ErrStaleLease
	}
	return nil
}

func completeRunLease(ctx context.Context, tx *sql.Tx, lease run.Lease) error {
	result, err := tx.ExecContext(ctx, runCompleteSQL, lease.WorkItemID, lease.Owner, lease.FencingToken)
	if err != nil {
		return err
	}
	if requireSingleRow(result) != nil {
		return mysqlqueue.ErrStaleLease
	}
	return nil
}

func (store *RunStore) inLeaseTx(ctx context.Context, runID string, lease run.Lease, body func(tx *sql.Tx) error) error {
	tx, err := store.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockRunLease(ctx, tx, runID, lease); err != nil {
		return err
	}
	if err := body(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// BeginExecution moves a QUEUED or PREPARING Run to PREPARING and returns its execution inputs.
// A Run that is no longer executable completes the work item and returns run.ErrConflict.
func (store *RunStore) BeginExecution(ctx context.Context, runID string, lease run.Lease) (run.Job, error) {
	var job run.Job
	conflict := false
	err := store.inLeaseTx(ctx, runID, lease, func(tx *sql.Tx) error {
		var (
			status, input, caseJSON, schemaSQL, seedSQL, endpoint, model, toolMode string
			datasetID                                                              string
			tokenRef                                                               sql.NullString
			cancelAt                                                               sql.NullTime
			tools                                                                  []byte
			dbHost, dbUser, dbSecret, dbTLS, dbCA                                  sql.NullString
			dbPort                                                                 sql.NullInt64
		)
		err := tx.QueryRowContext(ctx, `SELECT r.status, r.input_text, r.cancel_requested_at, r.dataset_revision_id,
			CAST(tc.content_json AS CHAR), d.schema_sql, d.seed_sql, a.chat_endpoint, a.target_bearer_token_ref, a.target_model, a.tool_evaluation_mode, a.tool_catalog_json,
			p.host, p.port, p.username, p.password_secret_ref, p.tls_mode, p.ca_certificate_pem
			FROM test_run r
			JOIN test_case_revision tc ON tc.id = r.test_case_revision_id
			JOIN dataset_revision d ON d.id = r.dataset_revision_id
			JOIN agent_version a ON a.id = r.agent_version_id
			JOIN agent ag ON ag.id = a.agent_id
			LEFT JOIN database_profile p ON p.id = ag.database_profile_id
			WHERE r.id = ? FOR UPDATE`, runID).Scan(&status, &input, &cancelAt, &datasetID, &caseJSON, &schemaSQL, &seedSQL,
			&endpoint, &tokenRef, &model, &toolMode, &tools, &dbHost, &dbPort, &dbUser, &dbSecret, &dbTLS, &dbCA)
		if errors.Is(err, sql.ErrNoRows) {
			return run.ErrNotFound
		}
		if err != nil {
			return err
		}
		if status != "QUEUED" && status != run.StatusPreparing {
			conflict = true
			return completeRunLease(ctx, tx, lease)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE test_run SET status='PREPARING', started_at=COALESCE(started_at, UTC_TIMESTAMP(6)) WHERE id = ?`, runID); err != nil {
			return err
		}
		job = run.Job{RunID: runID, Status: run.StatusPreparing, Prompt: input, CancelRequested: cancelAt.Valid,
			CaseJSON: json.RawMessage(caseJSON), DatasetID: datasetID, SchemaSQL: schemaSQL, SeedSQL: seedSQL,
			Agent: run.AgentSpec{Endpoint: endpoint, TokenRef: tokenRef.String, Model: model, ToolMode: toolMode, ToolsJSON: tools}}
		if dbHost.Valid {
			job.AgentDatabase = &run.AgentDatabase{Host: dbHost.String, Port: int(dbPort.Int64), Username: dbUser.String,
				PasswordSecretRef: dbSecret.String, TLSMode: dbTLS.String, CACertificatePEM: dbCA.String}
		}
		return nil
	})
	if err != nil {
		return run.Job{}, err
	}
	if conflict {
		return run.Job{}, run.ErrConflict
	}
	return job, nil
}

// MarkRunning moves PREPARING to RUNNING under the lease.
func (store *RunStore) MarkRunning(ctx context.Context, runID string, lease run.Lease) error {
	conflict := false
	err := store.inLeaseTx(ctx, runID, lease, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE test_run SET status='RUNNING' WHERE id = ? AND status = 'PREPARING'`, runID)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n != 1 {
			conflict = true
			return completeRunLease(ctx, tx, lease)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if conflict {
		return run.ErrConflict
	}
	return nil
}

// CancelRequested reports whether a cancellation was requested for the Run.
func (store *RunStore) CancelRequested(ctx context.Context, runID string) (bool, error) {
	var at sql.NullTime
	if err := store.database.QueryRowContext(ctx, `SELECT cancel_requested_at FROM test_run WHERE id = ?`, runID).Scan(&at); err != nil {
		return false, err
	}
	return at.Valid, nil
}

// Settle persists the execution result, enqueues evaluation when required, and completes the work item.
func (store *RunStore) Settle(ctx context.Context, runID string, lease run.Lease, settlement run.Settlement) error {
	switch settlement.Status {
	case run.StatusEvaluating, run.StatusCompleted, run.StatusInvalidData, run.StatusCancelled:
	default:
		return fmt.Errorf("invalid settlement status %q", settlement.Status)
	}
	return store.inLeaseTx(ctx, runID, lease, func(tx *sql.Tx) error {
		finished := settlement.Status != run.StatusEvaluating
		result, err := tx.ExecContext(ctx, `UPDATE test_run SET status=?, execution_outcome=?, final_output=?, actual_output_sha256=?,
			precondition_json=?, postcondition_json=?, state_diff_json=?, deterministic_json=?, deterministic_score=?, error_json=?,
			started_at=COALESCE(started_at, ?), agent_finished_at=?, environment_cleaned_at=?,
			finished_at=IF(?, UTC_TIMESTAMP(6), finished_at)
			WHERE id = ? AND status IN ('PREPARING','RUNNING')`,
			settlement.Status, nullableStringValue(settlement.Outcome), settlement.FinalOutput, settlement.OutputSHA256,
			nullableJSON(settlement.Precondition), nullableJSON(settlement.Postcondition), nullableJSON(settlement.StateDiff),
			nullableJSON(settlement.Deterministic), settlement.Score, nullableJSON(settlement.Error),
			nullableTime(settlement.StartedAt), nullableTime(settlement.AgentFinishedAt), nullableTime(settlement.CleanedAt), finished, runID)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n != 1 {
			return run.ErrConflict
		}
		if settlement.Status == run.StatusEvaluating {
			payload, _ := json.Marshal(map[string]string{"test_run_id": runID})
			if err := insertPendingAttempt(ctx, tx, runID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, runEnqueueSQL, ulid.Make().String(), run.QueueEvaluation, run.AggregateType, runID, run.StageEvaluate, string(payload)); err != nil && !isDuplicate(err) {
				return err
			}
		}
		return completeRunLease(ctx, tx, lease)
	})
}

// FailExecution schedules a retry or, when terminal, settles the Run as COMPLETED/INFRA_ERROR.
func (store *RunStore) FailExecution(ctx context.Context, runID string, lease run.Lease, failure run.Failure, backoff time.Duration) (bool, error) {
	if backoff < 0 || backoff > 24*time.Hour {
		return false, fmt.Errorf("invalid backoff %s", backoff)
	}
	failureJSON, err := json.Marshal(failure)
	if err != nil {
		return false, err
	}
	terminal := !failure.Retryable || lease.AttemptCount >= lease.MaxAttempts
	err = store.inLeaseTx(ctx, runID, lease, func(tx *sql.Tx) error {
		if !terminal {
			result, err := tx.ExecContext(ctx, runRetrySQL, backoff.Microseconds(), string(failureJSON), lease.WorkItemID, lease.Owner, lease.FencingToken)
			if err != nil {
				return err
			}
			if requireSingleRow(result) != nil {
				return mysqlqueue.ErrStaleLease
			}
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE test_run SET status='COMPLETED', execution_outcome='INFRA_ERROR', error_json=?, finished_at=UTC_TIMESTAMP(6)
			WHERE id = ? AND status IN ('QUEUED','PREPARING','RUNNING')`, string(failureJSON), runID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, runDeadSQL, string(failureJSON), lease.WorkItemID, lease.Owner, lease.FencingToken)
		if err != nil {
			return err
		}
		if requireSingleRow(result) != nil {
			return mysqlqueue.ErrStaleLease
		}
		return nil
	})
	return terminal, err
}
