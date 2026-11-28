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
	"agent-test-platform/internal/pipeline"
	"agent-test-platform/internal/run"
)

const (
	runLockLeaseSQL = `SELECT aggregate_type, aggregate_id FROM work_item WHERE id = ? AND status = 'LEASED' AND lease_owner = ? AND fencing_token = ? FOR UPDATE`
	runCompleteSQL  = `UPDATE work_item SET status='DONE', lease_owner=NULL, lease_until=NULL, updated_at=UTC_TIMESTAMP(6) WHERE id=? AND status='LEASED' AND lease_owner=? AND fencing_token=?`
	runRetrySQL     = `UPDATE work_item SET status='READY', lease_owner=NULL, lease_until=NULL, available_at=TIMESTAMPADD(MICROSECOND, ?, UTC_TIMESTAMP(6)), last_error_json=?, updated_at=UTC_TIMESTAMP(6) WHERE id=? AND status='LEASED' AND lease_owner=? AND fencing_token=?`
	runDeadSQL      = `UPDATE work_item SET status='DEAD', lease_owner=NULL, lease_until=NULL, last_error_json=?, updated_at=UTC_TIMESTAMP(6) WHERE id=? AND status='LEASED' AND lease_owner=? AND fencing_token=?`
	runEnqueueSQL   = `INSERT INTO work_item (id, queue_name, aggregate_type, aggregate_id, stage_key, status, priority, available_at, fencing_token, attempt_count, max_attempts, payload_json) VALUES (?, ?, ?, ?, ?, 'READY', 0, UTC_TIMESTAMP(6), 0, 0, 3, ?)`
)

// maxResultErrorBytes caps a node result's error field so the pipeline record stays small
// (technical/10 §3.3).
const maxResultErrorBytes = 512

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

// loadPipeline reads and parses the Run's pipeline inside tx, holding the row lock.
func (store *RunStore) loadPipeline(ctx context.Context, tx *sql.Tx, runID string) (pipeline.Pipeline, error) {
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT CAST(pipeline_json AS CHAR) FROM test_run WHERE id = ? FOR UPDATE`, runID).Scan(&raw); err != nil {
		return pipeline.Pipeline{}, err
	}
	return pipeline.Parse(store.validator, raw)
}

// savePipeline re-validates and persists the Run's pipeline inside tx.
func (store *RunStore) savePipeline(ctx context.Context, tx *sql.Tx, runID string, p pipeline.Pipeline) error {
	data, err := pipeline.Marshal(store.validator, p)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE test_run SET pipeline_json = ? WHERE id = ?`, data, runID); err != nil {
		return err
	}
	return nil
}

func truncateError(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if len(message) > maxResultErrorBytes {
		return message[:maxResultErrorBytes]
	}
	return message
}

// BeginExecution loads the inputs for the node a work item executes and moves the Run to PREPARING
// when it is the first node of iteration 1. A Run that is no longer executable completes the work
// item and returns run.ErrConflict.
func (store *RunStore) BeginExecution(ctx context.Context, runID string, lease run.Lease, node run.NodeRef) (run.Job, error) {
	var job run.Job
	conflict := false
	err := store.inLeaseTx(ctx, runID, lease, func(tx *sql.Tx) error {
		var (
			status, caseJSON, schemaSQL, seedSQL, endpoint, model string
			datasetID                                             string
			pipelineJSON                                          []byte
			turnsTotal                                            int
			tokenRef                                              sql.NullString
			cancelAt                                              sql.NullTime
			sessionID                                             sql.NullString
			dbName, dbHost, dbUser, dbSecret, dbTLS, dbCA         sql.NullString
			dbPort                                                sql.NullInt64
			clientID, clientSecretRef, tokenURL                   sql.NullString
		)
		err := tx.QueryRowContext(ctx, `SELECT r.status, r.cancel_requested_at, r.dataset_revision_id, r.session_id, r.turns_total,
			CAST(r.pipeline_json AS CHAR), CAST(tc.content_json AS CHAR), d.schema_sql, d.seed_sql, a.chat_endpoint, a.target_bearer_token_ref, a.target_model,
			a.client_id, a.client_secret_ref, a.token_url,
			p.host, p.port, p.database_name, p.username, p.password_secret_ref, p.tls_mode, p.ca_certificate_pem
			FROM test_run r
			JOIN test_case_revision tc ON tc.id = r.test_case_revision_id
			JOIN dataset_revision d ON d.id = r.dataset_revision_id
			JOIN agent a ON a.id = r.agent_id
			LEFT JOIN database_profile p ON p.id = a.database_profile_id
			WHERE r.id = ? FOR UPDATE`, runID).Scan(&status, &cancelAt, &datasetID, &sessionID, &turnsTotal,
			&pipelineJSON, &caseJSON, &schemaSQL, &seedSQL, &endpoint, &tokenRef, &model, &clientID, &clientSecretRef, &tokenURL,
			&dbHost, &dbPort, &dbName, &dbUser, &dbSecret, &dbTLS, &dbCA)
		if errors.Is(err, sql.ErrNoRows) {
			return run.ErrNotFound
		}
		if err != nil {
			return err
		}
		switch status {
		case "QUEUED":
			// Only the init node of iteration 1 may start a queued Run.
			if node.NodeID != pipeline.InitNodeID {
				conflict = true
				return completeRunLease(ctx, tx, lease)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE test_run SET status='PREPARING', started_at=COALESCE(started_at, UTC_TIMESTAMP(6)) WHERE id = ?`, runID); err != nil {
				return err
			}
		case run.StatusPreparing, run.StatusRunning:
			// Continue the pipeline.
		default:
			conflict = true
			return completeRunLease(ctx, tx, lease)
		}
		job = run.Job{RunID: runID, NodeID: node.NodeID, NodeType: run.NodeTypeOf(node.NodeID), Iteration: node.Iteration, TurnNo: node.TurnNo,
			Status: run.StatusPreparing, CancelRequested: cancelAt.Valid,
			CaseJSON: json.RawMessage(caseJSON), PipelineJSON: json.RawMessage(pipelineJSON),
			SessionID: sessionID.String, DatasetID: datasetID, SchemaSQL: schemaSQL, SeedSQL: seedSQL,
			Agent: run.AgentSpec{Endpoint: endpoint, TokenRef: tokenRef.String, ClientID: clientID.String, ClientSecretRef: clientSecretRef.String, TokenURL: tokenURL.String, Model: model}}
		if dbHost.Valid {
			job.AgentDatabase = &run.AgentDatabase{Host: dbHost.String, Port: int(dbPort.Int64), Database: dbName.String, Username: dbUser.String,
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

// MarkRunning moves PREPARING (or an already-RUNNING Run, idempotently) to RUNNING under the lease.
func (store *RunStore) MarkRunning(ctx context.Context, runID string, lease run.Lease) error {
	conflict := false
	err := store.inLeaseTx(ctx, runID, lease, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE test_run SET status='RUNNING' WHERE id = ? AND status IN ('PREPARING','RUNNING')`, runID)
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

// Settle persists a node's result and advances the pipeline according to the node type.
func (store *RunStore) Settle(ctx context.Context, runID string, lease run.Lease, settlement run.Settlement) error {
	switch settlement.NodeType {
	case pipeline.NodeTypeInit:
		return store.settleInit(ctx, runID, lease, settlement)
	case pipeline.NodeTypeTurn:
		return store.settleTurn(ctx, runID, lease, settlement)
	case pipeline.NodeTypeFinish:
		return store.settleFinish(ctx, runID, lease, settlement)
	default:
		return fmt.Errorf("invalid settlement node type %q", settlement.NodeType)
	}
}

// settleInit records the teardown handles the init node produced and enqueues turn-1. An init that
// reported INVALID_TEST_DATA marks the round failed, skips the remaining nodes, and routes to
// finish (teardown still runs) instead of terminating the Run outright.
func (store *RunStore) settleInit(ctx context.Context, runID string, lease run.Lease, settlement run.Settlement) error {
	return store.inLeaseTx(ctx, runID, lease, func(tx *sql.Tx) error {
		p, err := store.loadPipeline(ctx, tx, runID)
		if err != nil {
			return err
		}
		if settlement.Status == run.StatusInvalidData {
			p.Context.RoundPassed = false
			record := pipeline.NewNodeResult(p.Context.Iteration, pipeline.InitNodeID, pipeline.NodeTypeInit, "ERROR",
				timeOrZero(settlement.StartedAt), timeOrZero(settlement.CleanedAt))
			if len(settlement.Error) > 0 {
				record.Error = truncateError(errors.New(string(settlement.Error)))
			}
			p.Result = append(p.Result, record)
			for _, node := range p.NodesAfter(pipeline.InitNodeID) {
				p.Result = append(p.Result, pipeline.SkipResult(p.Context.Iteration, node))
			}
			if err := store.savePipeline(ctx, tx, runID, p); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE test_run SET status = 'RUNNING', execution_outcome = 'INVALID_TEST_DATA', error_json = ? WHERE id = ?`,
				nullableJSON(settlement.Error), runID); err != nil {
				return err
			}
			if err := enqueueNode(ctx, tx, runID, settlement.Iteration, pipeline.FinishNodeID); err != nil {
				return err
			}
			return completeRunLease(ctx, tx, lease)
		}
		status := "COMPLETED"
		p.Context.KnowledgeDocumentIDs = settlement.KnowledgeDocumentIDs
		p.Context.DatabaseTables = settlement.DatabaseTables
		if p.Context.KnowledgeDocumentIDs == nil {
			p.Context.KnowledgeDocumentIDs = []string{}
		}
		if p.Context.DatabaseTables == nil {
			p.Context.DatabaseTables = []string{}
		}
		p.Result = append(p.Result, pipeline.NewNodeResult(p.Context.Iteration, pipeline.InitNodeID, pipeline.NodeTypeInit, status,
			timeOrZero(settlement.StartedAt), timeOrZero(settlement.CleanedAt)))
		if err := store.savePipeline(ctx, tx, runID, p); err != nil {
			return err
		}
		if err := enqueueNode(ctx, tx, runID, p.Context.Iteration, pipeline.TurnNodeID(1)); err != nil {
			return err
		}
		return completeRunLease(ctx, tx, lease)
	})
}

// settleTurn records the turn's output and either enqueues its judge (completed) or routes to
// finish (failed turn: the pipeline stops).
func (store *RunStore) settleTurn(ctx context.Context, runID string, lease run.Lease, settlement run.Settlement) error {
	turnCompleted := settlement.Outcome == run.OutcomeCompleted
	return store.inLeaseTx(ctx, runID, lease, func(tx *sql.Tx) error {
		p, err := store.loadPipeline(ctx, tx, runID)
		if err != nil {
			return err
		}
		if turnCompleted {
			if _, err := tx.ExecContext(ctx, `UPDATE test_run_turn SET status = 'JUDGING', final_output = ?, output_sha256 = ?, context_watermark = ?, started_at = COALESCE(started_at, ?), finished_at = UTC_TIMESTAMP(6) WHERE test_run_id = ? AND iteration = ? AND turn_no = ?`,
				settlement.FinalOutput, settlement.OutputSHA256, settlement.ContextWatermark, nullableTime(settlement.StartedAt), runID, settlement.Iteration, settlement.TurnNo); err != nil {
				return err
			}
			p.Result = append(p.Result, pipeline.NewNodeResult(p.Context.Iteration, pipeline.TurnNodeID(settlement.TurnNo), pipeline.NodeTypeTurn, run.OutcomeCompleted,
				timeOrZero(settlement.StartedAt), timeOrZero(settlement.AgentFinishedAt)))
			if err := store.savePipeline(ctx, tx, runID, p); err != nil {
				return err
			}
			if err := insertPendingAttempt(ctx, tx, runID, settlement.Iteration, settlement.TurnNo); err != nil {
				return err
			}
			if err := enqueueJudge(ctx, tx, runID, settlement.Iteration, settlement.TurnNo); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE test_run SET status = 'EVALUATING', session_id = ?, final_output = ?, actual_output_sha256 = ?, agent_finished_at = ? WHERE id = ?`,
				settlement.SessionID, settlement.FinalOutput, settlement.OutputSHA256, nullableTime(settlement.AgentFinishedAt), runID); err != nil {
				return err
			}
		} else {
			// A failed turn stops the pipeline: mark the round failed, skip the remaining nodes, and
			// route to finish for teardown.
			p.Context.RoundPassed = false
			if _, err := tx.ExecContext(ctx, `UPDATE test_run_turn SET status = ?, error_json = ?, finished_at = UTC_TIMESTAMP(6) WHERE test_run_id = ? AND iteration = ? AND turn_no = ?`,
				run.TurnError, nullableJSON(settlement.Error), runID, settlement.Iteration, settlement.TurnNo); err != nil {
				return err
			}
			record := pipeline.NewNodeResult(p.Context.Iteration, pipeline.TurnNodeID(settlement.TurnNo), pipeline.NodeTypeTurn, settlement.Outcome,
				timeOrZero(settlement.StartedAt), timeOrZero(settlement.AgentFinishedAt))
			if len(settlement.Error) > 0 {
				record.Error = truncateError(errors.New(string(settlement.Error)))
			}
			p.Result = append(p.Result, record)
			for _, node := range p.NodesAfter(pipeline.TurnNodeID(settlement.TurnNo)) {
				p.Result = append(p.Result, pipeline.SkipResult(p.Context.Iteration, node))
			}
			if err := store.savePipeline(ctx, tx, runID, p); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE test_run SET status = 'RUNNING', execution_outcome = ?, error_json = ? WHERE id = ?`,
				settlement.Outcome, nullableJSON(settlement.Error), runID); err != nil {
				return err
			}
			if err := enqueueNode(ctx, tx, runID, settlement.Iteration, pipeline.FinishNodeID); err != nil {
				return err
			}
		}
		return completeRunLease(ctx, tx, lease)
	})
}

// settleFinish tears down the iteration and drives the repetition loop: continue to the next
// iteration's init, or settle the Run terminal with the averaged final score.
func (store *RunStore) settleFinish(ctx context.Context, runID string, lease run.Lease, settlement run.Settlement) error {
	return store.inLeaseTx(ctx, runID, lease, func(tx *sql.Tx) error {
		p, err := store.loadPipeline(ctx, tx, runID)
		if err != nil {
			return err
		}
		finished := timeOrZero(settlement.CleanedAt)
		cleanupOK := len(settlement.Error) == 0
		status := "COMPLETED"
		if !cleanupOK {
			status = "ERROR"
		}
		record := pipeline.NewNodeResult(p.Context.Iteration, pipeline.FinishNodeID, pipeline.NodeTypeFinish, status,
			timeOrZero(settlement.StartedAt), finished)
		if !cleanupOK {
			record.Error = truncateError(errors.New(string(settlement.Error)))
		}
		p.Result = append(p.Result, record)
		// Clear the teardown handles only on a successful teardown; a failed teardown keeps them so
		// the next iteration's init can locate and clean the residual data (technical/10 §5).
		if cleanupOK {
			p.Context.KnowledgeDocumentIDs = []string{}
			p.Context.DatabaseTables = []string{}
		}

		continueNext := p.Context.RoundPassed && p.Context.Iteration < p.Config.Repetitions
		if continueNext {
			p.Context.Iteration++
			p.Context.RoundPassed = true
			if err := store.savePipeline(ctx, tx, runID, p); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE test_run SET status = 'RUNNING', environment_cleaned_at = ? WHERE id = ?`, nullableTime(settlement.CleanedAt), runID); err != nil {
				return err
			}
			if err := store.seedTurnRows(ctx, tx, runID, p.Context.Iteration); err != nil {
				return err
			}
			if err := enqueueNode(ctx, tx, runID, p.Context.Iteration, pipeline.InitNodeID); err != nil {
				return err
			}
		} else {
			// Terminal: final score = the mean of every iteration's score over the fixed repetition
			// count. An iteration that never reached its last judge scores 0, and interrupted
			// (never-run) iterations also count as 0.
			sum := 0.0
			for _, score := range p.Context.IterationScores {
				sum += score
			}
			finalScore := pipeline.Round2(sum / float64(p.Config.Repetitions))
			pass := p.Context.RoundPassed && len(p.Context.IterationScores) == p.Config.Repetitions
			if err := store.savePipeline(ctx, tx, runID, p); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE test_run SET status = 'COMPLETED', finished_at = UTC_TIMESTAMP(6), environment_cleaned_at = ?, final_score = ?, pass = ? WHERE id = ?`,
				nullableTime(settlement.CleanedAt), nullableFloat64(&finalScore), pass, runID); err != nil {
				return err
			}
		}
		return completeRunLease(ctx, tx, lease)
	})
}

// seedTurnRows inserts the turn rows for a new iteration so each turn has a persistent record.
func (store *RunStore) seedTurnRows(ctx context.Context, tx *sql.Tx, runID string, iteration int) error {
	var turnsTotal int
	if err := tx.QueryRowContext(ctx, `SELECT turns_total FROM test_run WHERE id = ?`, runID).Scan(&turnsTotal); err != nil {
		return err
	}
	for turn := 1; turn <= turnsTotal; turn++ {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO test_run_turn (id, test_run_id, iteration, turn_no, prompt, status) VALUES (?, ?, ?, ?, '', 'PENDING')`,
			ulid.Make().String(), runID, iteration, turn); err != nil {
			return fmt.Errorf("insert Test Run turn: %w", err)
		}
	}
	return nil
}

func enqueueNode(ctx context.Context, tx *sql.Tx, runID string, iteration int, nodeID string) error {
	payload, err := json.Marshal(map[string]any{"test_run_id": runID, "iteration": iteration, "node_id": nodeID})
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, runEnqueueSQL, ulid.Make().String(), run.QueueName, run.AggregateType, runID, run.StageKey(iteration, nodeID), string(payload)); err != nil && !isDuplicate(err) {
		return err
	}
	return nil
}

func enqueueJudge(ctx context.Context, tx *sql.Tx, runID string, iteration, turnNo int) error {
	payload, err := json.Marshal(map[string]any{"test_run_id": runID, "iteration": iteration, "node_id": pipeline.JudgeNodeID(turnNo), "turn_no": turnNo})
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, runEnqueueSQL, ulid.Make().String(), run.QueueEvaluation, run.AggregateType, runID, run.StageKey(iteration, pipeline.JudgeNodeID(turnNo)), string(payload)); err != nil && !isDuplicate(err) {
		return err
	}
	return nil
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
