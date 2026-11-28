package mysqlstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	"agent-test-platform/internal/jobs/mysqlqueue"
	"agent-test-platform/internal/pipeline"
	"agent-test-platform/internal/run"
)

// BeginEvaluation loads the evidence of the current turn of an EVALUATING Run. Any other status
// completes the lease and returns run.ErrConflict.
func (store *RunStore) BeginEvaluation(ctx context.Context, runID string, lease run.Lease, node run.NodeRef) (run.EvaluationJob, error) {
	var job run.EvaluationJob
	conflict := false
	err := store.inLeaseTx(ctx, runID, lease, func(tx *sql.Tx) error {
		var (
			status, caseJSON    string
			outcome             sql.NullString
			sessionID           sql.NullString
			pipelineJSON        []byte
			turnsTotal          int
			watermark           int
			endpoint, tokenRef  sql.NullString
			model               sql.NullString
			clientID, secretRef sql.NullString
			tokenURL            sql.NullString
		)
		err := tx.QueryRowContext(ctx, `SELECT r.status, r.execution_outcome, r.session_id, r.turns_total, CAST(r.pipeline_json AS CHAR),
			COALESCE(t.context_watermark, 0), CAST(tc.content_json AS CHAR),
			a.chat_endpoint, a.target_bearer_token_ref, a.target_model, a.client_id, a.client_secret_ref, a.token_url
			FROM test_run r
			JOIN test_case_revision tc ON tc.id = r.test_case_revision_id
			JOIN agent a ON a.id = r.agent_id
			LEFT JOIN test_run_turn t ON t.test_run_id = r.id AND t.iteration = ? AND t.turn_no = ?
			WHERE r.id = ? FOR UPDATE`, node.Iteration, node.TurnNo, runID).
			Scan(&status, &outcome, &sessionID, &turnsTotal, &pipelineJSON, &watermark, &caseJSON,
				&endpoint, &tokenRef, &model, &clientID, &secretRef, &tokenURL)
		if errors.Is(err, sql.ErrNoRows) {
			return run.ErrNotFound
		}
		if err != nil {
			return err
		}
		if status != run.StatusEvaluating || !outcome.Valid {
			conflict = true
			return completeRunLease(ctx, tx, lease)
		}
		attemptNo, alias, err := startAttempt(ctx, tx, runID, node.Iteration, node.TurnNo)
		if err != nil {
			return err
		}
		job = run.EvaluationJob{RunID: runID, NodeID: node.NodeID, Iteration: node.Iteration, Outcome: outcome.String,
			CaseJSON: json.RawMessage(caseJSON), PipelineJSON: json.RawMessage(pipelineJSON),
			TurnNo: node.TurnNo, TotalTurns: turnsTotal, SessionID: sessionID.String, UntilSequence: watermark,
			AttemptNo: attemptNo, JudgeAlias: alias,
			Agent: run.AgentSpec{Endpoint: endpoint.String, TokenRef: tokenRef.String, Model: model.String,
				ClientID: clientID.String, ClientSecretRef: secretRef.String, TokenURL: tokenURL.String}}
		return nil
	})
	if err != nil {
		return run.EvaluationJob{}, err
	}
	if conflict {
		return run.EvaluationJob{}, run.ErrConflict
	}
	return job, nil
}

func ensureJudgeConfig(ctx context.Context, tx *sql.Tx, result run.EvaluationResult) (string, error) {
	lookup := func() (string, error) {
		var id string
		err := tx.QueryRowContext(ctx, `SELECT id FROM model_config WHERE config_sha256 = ?`, result.ConfigSHA256).Scan(&id)
		return id, err
	}
	id, err := lookup()
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	id = ulid.Make().String()
	_, err = tx.ExecContext(ctx, `INSERT INTO model_config (id, alias_name, revision_no, model_role, provider, model_name, base_url, capabilities_json, config_json, config_sha256, enabled)
		SELECT ?, ?, COALESCE(MAX(revision_no), 0) + 1, 'judge', ?, ?, ?, JSON_OBJECT(), ?, ?, FALSE FROM model_config WHERE alias_name = ?`,
		id, result.Alias, result.Provider, result.Model, result.BaseURL, string(result.ConfigJSON), result.ConfigSHA256, result.Alias)
	if isDuplicate(err) {
		return lookup()
	}
	if err != nil {
		return "", err
	}
	return id, nil
}

// SettleEvaluation records the Judge attempt for the current turn, advances the pipeline gate, and
// either enqueues the next turn, or finish (which drives the repetition loop).
func (store *RunStore) SettleEvaluation(ctx context.Context, runID string, lease run.Lease, attemptNo int, result run.EvaluationResult, startedAt time.Time) error {
	conflict := false
	err := store.inLeaseTx(ctx, runID, lease, func(tx *sql.Tx) error {
		var status string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM test_run WHERE id = ? FOR UPDATE`, runID).Scan(&status); err != nil {
			return err
		}
		if status != run.StatusEvaluating {
			conflict = true
			return completeRunLease(ctx, tx, lease)
		}
		configID, err := ensureJudgeConfig(ctx, tx, result)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE evaluation_attempt SET status = ?, judge_model_config_id = ?, judge_model_alias = ?, judge_provider = ?, judge_model = ?,
			judge_prompt_version = ?, judge_config_sha256 = ?, request_json = ?, response_json = ?, criteria_json = ?, failure_categories_json = ?, llm_score = ?, final_score = ?, pass = ?,
			provider_request_id = ?, input_tokens = ?, output_tokens = ?, started_at = ?, finished_at = UTC_TIMESTAMP(6), error_json = NULL
			WHERE test_run_id = ? AND attempt_no = ? AND status = 'RUNNING'`,
			run.EvaluationCompleted, configID, result.Alias, result.Provider, result.Model,
			result.PromptVersion, result.ConfigSHA256, string(result.RequestJSON), nullableJSON(result.ResponseJSON), nullableJSON(result.CriteriaJSON),
			nullableJSON(result.FailureCategories), result.LLMScore, result.FinalScore, result.Pass, nullableStringValue(result.ProviderRequestID),
			nullableInt64(result.InputTokens), nullableInt64(result.OutputTokens), startedAt.UTC(), runID, attemptNo)
		if err != nil {
			return err
		}
		if requireSingleRow(res) != nil {
			return run.ErrConflict
		}
		turnStatus := run.TurnPassed
		if !result.Pass {
			turnStatus = run.TurnFailed
		}
		if _, err := tx.ExecContext(ctx, `UPDATE test_run_turn SET status = ?, llm_score = ?, final_score = ?, pass = ?, finished_at = UTC_TIMESTAMP(6) WHERE test_run_id = ? AND iteration = ? AND turn_no = ?`,
			turnStatus, result.LLMScore, result.FinalScore, result.Pass, runID, result.Iteration, result.TurnNo); err != nil {
			return err
		}
		p, err := store.loadPipeline(ctx, tx, runID)
		if err != nil {
			return err
		}
		record := pipeline.NewNodeResult(p.Context.Iteration, pipeline.JudgeNodeID(result.TurnNo), pipeline.NodeTypeJudge, turnStatus, startedAt, time.Now().UTC())
		record.Pass = &result.Pass
		score := pipeline.Round2(result.FinalScore)
		record.FinalScore = &score
		p.Result = append(p.Result, record)
		if !result.Pass {
			p.Context.RoundPassed = false
		}

		isLastJudge := p.TurnCount() == result.TurnNo
		if isLastJudge {
			// Reached the last judge of the pipeline → its score is this iteration's score,
			// whether it passed or failed (technical/10 §3.4).
			p.Context.IterationScores = append(p.Context.IterationScores, score)
		}

		if isLastJudge || !result.Pass {
			// Last judge (pass or fail) or a failed intermediate judge → finish. A failed
			// intermediate judge skips the remaining turn/judge nodes, but finish still runs.
			if !isLastJudge {
				for _, node := range p.NodesAfter(pipeline.JudgeNodeID(result.TurnNo)) {
					p.Result = append(p.Result, pipeline.SkipResult(p.Context.Iteration, node))
				}
			}
			if err := store.savePipeline(ctx, tx, runID, p); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE test_run SET status = 'RUNNING' WHERE id = ?`, runID); err != nil {
				return err
			}
			if err := enqueueNode(ctx, tx, runID, result.Iteration, pipeline.FinishNodeID); err != nil {
				return err
			}
		} else {
			// A passed intermediate judge advances to the next turn.
			if err := store.savePipeline(ctx, tx, runID, p); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE test_run SET status = 'RUNNING' WHERE id = ?`, runID); err != nil {
				return err
			}
			passed := result.Pass
			next := p.NextNodes(pipeline.JudgeNodeID(result.TurnNo), pipeline.NodeOutput{Pass: &passed, LLMScore: result.LLMScore, FinalScore: result.FinalScore})
			if len(next) == 0 {
				return fmt.Errorf("pipeline judge %q has no outgoing node", pipeline.JudgeNodeID(result.TurnNo))
			}
			if err := enqueueNode(ctx, tx, runID, result.Iteration, next[0]); err != nil {
				return err
			}
		}
		return completeRunLease(ctx, tx, lease)
	})
	if err != nil {
		return err
	}
	if conflict {
		return run.ErrConflict
	}
	return nil
}

// FailEvaluation retries or, when terminal, marks the Run EVALUATION_FAILED and the work item DEAD.
func (store *RunStore) FailEvaluation(ctx context.Context, runID string, lease run.Lease, failure run.Failure, backoff time.Duration) (bool, error) {
	if backoff < 0 || backoff > 24*time.Hour {
		return false, fmt.Errorf("invalid backoff %s", backoff)
	}
	failureJSON, err := json.Marshal(failure)
	if err != nil {
		return false, err
	}
	terminal := !failure.Retryable || lease.AttemptCount >= lease.MaxAttempts
	err = store.inLeaseTx(ctx, runID, lease, func(tx *sql.Tx) error {
		nextStatus := run.EvaluationPending
		if terminal {
			nextStatus = run.EvaluationFailed
		}
		if _, err := tx.ExecContext(ctx, `UPDATE evaluation_attempt SET status = ?, error_json = ?, finished_at = IF(?, UTC_TIMESTAMP(6), NULL)
			WHERE test_run_id = ? AND status IN ('PENDING','RUNNING')`, nextStatus, string(failureJSON), terminal, runID); err != nil {
			return err
		}
		var result sql.Result
		var err error
		if !terminal {
			result, err = tx.ExecContext(ctx, runRetrySQL, backoff.Microseconds(), string(failureJSON), lease.WorkItemID, lease.Owner, lease.FencingToken)
		} else {
			if _, err = tx.ExecContext(ctx, `UPDATE test_run SET status='EVALUATION_FAILED', error_json=?, finished_at=UTC_TIMESTAMP(6) WHERE id = ? AND status = 'EVALUATING'`,
				string(failureJSON), runID); err != nil {
				return err
			}
			result, err = tx.ExecContext(ctx, runDeadSQL, string(failureJSON), lease.WorkItemID, lease.Owner, lease.FencingToken)
		}
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

// insertPendingAttempt records the next Evaluation Attempt as PENDING using the active Judge alias.
func insertPendingAttempt(ctx context.Context, tx *sql.Tx, runID string, iteration, turnNo int) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO evaluation_attempt (id, test_run_id, attempt_no, status, judge_model_alias, iteration, turn_no)
		SELECT ?, ?, COALESCE((SELECT MAX(attempt_no) FROM evaluation_attempt WHERE test_run_id = ?), 0) + 1, 'PENDING',
		COALESCE((SELECT alias_name FROM model_config WHERE model_role = 'judge' AND enabled = TRUE ORDER BY revision_no DESC LIMIT 1), ?), ?, ?`,
		ulid.Make().String(), runID, runID, run.DefaultJudgeAlias, iteration, turnNo)
	return err
}

// startAttempt moves the open attempt to RUNNING, creating it when the Run predates attempt tracking.
func startAttempt(ctx context.Context, tx *sql.Tx, runID string, iteration, turnNo int) (int, string, error) {
	find := func() (int, string, error) {
		var attemptNo int
		var alias string
		err := tx.QueryRowContext(ctx, `SELECT attempt_no, judge_model_alias FROM evaluation_attempt WHERE test_run_id = ? AND status IN ('PENDING','RUNNING') ORDER BY attempt_no DESC LIMIT 1 FOR UPDATE`, runID).Scan(&attemptNo, &alias)
		return attemptNo, alias, err
	}
	attemptNo, alias, err := find()
	if errors.Is(err, sql.ErrNoRows) {
		if err := insertPendingAttempt(ctx, tx, runID, iteration, turnNo); err != nil {
			return 0, "", err
		}
		attemptNo, alias, err = find()
	}
	if err != nil {
		return 0, "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE evaluation_attempt SET status = 'RUNNING', started_at = UTC_TIMESTAMP(6) WHERE test_run_id = ? AND attempt_no = ?`, runID, attemptNo); err != nil {
		return 0, "", err
	}
	return attemptNo, alias, nil
}

// Reevaluate copies the evidence of a finished Run into a new EVALUATING Run, resetting its turns
// to re-judge from the first turn with the chosen Judge alias.
func (store *RunStore) Reevaluate(ctx context.Context, sourceID, newID, requestKey, requestSHA256, judgeAlias string) error {
	tx, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin re-evaluation: %w", err)
	}
	defer tx.Rollback()
	var status string
	var outcome sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT status, execution_outcome FROM test_run WHERE id = ? FOR UPDATE`, sourceID).Scan(&status, &outcome); errors.Is(err, sql.ErrNoRows) {
		return run.ErrNotFound
	} else if err != nil {
		return fmt.Errorf("lock source Test Run: %w", err)
	}
	if (status != run.StatusCompleted && status != run.StatusEvaluationFailed) || !outcome.Valid {
		return run.ErrConflict
	}
	rowSum := sha256.Sum256([]byte(requestKey + "/" + newID))
	if _, err := tx.ExecContext(ctx, `INSERT INTO test_run (id, idempotency_key, request_key, request_sha256, test_case_revision_id, dataset_revision_id,
			agent_id, repetition_no, status, execution_outcome, input_text, final_output, session_id,
			pipeline_json, turns_total,
			actual_output_sha256, started_at, agent_finished_at, environment_cleaned_at)
		SELECT ?, ?, ?, ?, test_case_revision_id, dataset_revision_id, agent_id,
			(SELECT COALESCE(MAX(x.repetition_no), 0) + 1 FROM test_run x WHERE x.agent_id = s.agent_id AND x.test_case_revision_id = s.test_case_revision_id),
			'EVALUATING', execution_outcome, input_text, final_output, session_id,
			pipeline_json, turns_total,
			actual_output_sha256, started_at, agent_finished_at, environment_cleaned_at
		FROM test_run s WHERE s.id = ?`, newID, hex.EncodeToString(rowSum[:]), requestKey, requestSHA256, sourceID); err != nil {
		if isDuplicate(err) {
			return run.ErrDuplicate
		}
		return fmt.Errorf("insert re-evaluation Run: %w", err)
	}
	turnRows, err := tx.QueryContext(ctx, `SELECT iteration, turn_no, prompt, final_output, output_sha256, context_watermark FROM test_run_turn WHERE test_run_id = ? ORDER BY iteration, turn_no`, sourceID)
	if err != nil {
		return fmt.Errorf("load source turns: %w", err)
	}
	type sourceTurn struct {
		iteration        int
		turnNo           int
		prompt           string
		finalOutput      sql.NullString
		outputSHA256     sql.NullString
		contextWatermark sql.NullInt64
	}
	var turns []sourceTurn
	for turnRows.Next() {
		var item sourceTurn
		if err := turnRows.Scan(&item.iteration, &item.turnNo, &item.prompt, &item.finalOutput, &item.outputSHA256, &item.contextWatermark); err != nil {
			turnRows.Close()
			return fmt.Errorf("scan source turn: %w", err)
		}
		turns = append(turns, item)
	}
	turnRows.Close()
	if err := turnRows.Err(); err != nil {
		return err
	}
	for _, item := range turns {
		if _, err := tx.ExecContext(ctx, `INSERT INTO test_run_turn (id, test_run_id, iteration, turn_no, prompt, status, final_output, output_sha256, context_watermark) VALUES (?, ?, ?, ?, ?, 'PENDING', ?, ?, ?)`,
			ulid.Make().String(), newID, item.iteration, item.turnNo, item.prompt, item.finalOutput, item.outputSHA256, item.contextWatermark); err != nil {
			return fmt.Errorf("copy turn: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO evaluation_attempt (id, test_run_id, attempt_no, status, judge_model_alias, iteration, turn_no) VALUES (?, ?, 1, 'PENDING', ?, 1, 1)`,
		ulid.Make().String(), newID, judgeAlias); err != nil {
		return fmt.Errorf("insert evaluation attempt: %w", err)
	}
	if err := enqueueJudge(ctx, tx, newID, 1, 1); err != nil {
		return fmt.Errorf("enqueue evaluation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit re-evaluation: %w", err)
	}
	return nil
}
