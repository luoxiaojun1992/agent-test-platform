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
	"agent-test-platform/internal/run"
)

// BeginEvaluation loads the evidence of an EVALUATING Run. Any other status completes the lease and returns run.ErrConflict.
func (store *RunStore) BeginEvaluation(ctx context.Context, runID string, lease run.Lease) (run.EvaluationJob, error) {
	var job run.EvaluationJob
	conflict := false
	err := store.inLeaseTx(ctx, runID, lease, func(tx *sql.Tx) error {
		var (
			status, input, caseJSON string
			outcome, finalOutput    sql.NullString
			deterministic           []byte
		)
		err := tx.QueryRowContext(ctx, `SELECT r.status, r.execution_outcome, r.input_text, r.final_output, CAST(r.deterministic_json AS CHAR), CAST(tc.content_json AS CHAR)
			FROM test_run r JOIN test_case_revision tc ON tc.id = r.test_case_revision_id WHERE r.id = ? FOR UPDATE`, runID).
			Scan(&status, &outcome, &input, &finalOutput, &deterministic, &caseJSON)
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
		calls, err := loadToolCalls(ctx, tx, runID)
		if err != nil {
			return err
		}
		attemptNo, alias, err := startAttempt(ctx, tx, runID)
		if err != nil {
			return err
		}
		job = run.EvaluationJob{RunID: runID, Outcome: outcome.String, Prompt: input, CaseJSON: json.RawMessage(caseJSON),
			Deterministic: json.RawMessage(deterministic), ToolCalls: calls, AttemptNo: attemptNo, JudgeAlias: alias}
		if finalOutput.Valid {
			text := finalOutput.String
			job.FinalOutput = &text
		}
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

// SettleEvaluation records the Judge attempt, completes the Run and finishes the work item.
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
		if _, err := tx.ExecContext(ctx, `UPDATE test_run SET status='COMPLETED', finished_at=UTC_TIMESTAMP(6) WHERE id = ?`, runID); err != nil {
			return err
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
func insertPendingAttempt(ctx context.Context, tx *sql.Tx, runID string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO evaluation_attempt (id, test_run_id, attempt_no, status, judge_model_alias)
		SELECT ?, ?, COALESCE((SELECT MAX(attempt_no) FROM evaluation_attempt WHERE test_run_id = ?), 0) + 1, 'PENDING',
		COALESCE((SELECT alias_name FROM model_config WHERE model_role = 'judge' AND enabled = TRUE ORDER BY revision_no DESC LIMIT 1), ?)`,
		ulid.Make().String(), runID, runID, run.DefaultJudgeAlias)
	return err
}

// startAttempt moves the open attempt to RUNNING, creating it when the Run predates attempt tracking.
func startAttempt(ctx context.Context, tx *sql.Tx, runID string) (int, string, error) {
	find := func() (int, string, error) {
		var attemptNo int
		var alias string
		err := tx.QueryRowContext(ctx, `SELECT attempt_no, judge_model_alias FROM evaluation_attempt WHERE test_run_id = ? AND status IN ('PENDING','RUNNING') ORDER BY attempt_no DESC LIMIT 1 FOR UPDATE`, runID).Scan(&attemptNo, &alias)
		return attemptNo, alias, err
	}
	attemptNo, alias, err := find()
	if errors.Is(err, sql.ErrNoRows) {
		if err := insertPendingAttempt(ctx, tx, runID); err != nil {
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

// Reevaluate copies the evidence of a finished Run into a new EVALUATING Run and queues its evaluation.
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
			agent_version_id, repetition_no, status, execution_outcome, input_text, final_output, precondition_json, postcondition_json, state_diff_json,
			deterministic_json, deterministic_score, actual_output_sha256, started_at, agent_finished_at, environment_cleaned_at)
		SELECT ?, ?, ?, ?, test_case_revision_id, dataset_revision_id, agent_version_id,
			(SELECT COALESCE(MAX(x.repetition_no), 0) + 1 FROM test_run x WHERE x.agent_version_id = s.agent_version_id AND x.test_case_revision_id = s.test_case_revision_id),
			'EVALUATING', execution_outcome, input_text, final_output, precondition_json, postcondition_json, state_diff_json,
			deterministic_json, deterministic_score, actual_output_sha256, started_at, agent_finished_at, environment_cleaned_at
		FROM test_run s WHERE s.id = ?`, newID, hex.EncodeToString(rowSum[:]), requestKey, requestSHA256, sourceID); err != nil {
		if isDuplicate(err) {
			return run.ErrDuplicate
		}
		return fmt.Errorf("insert re-evaluation Run: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM tool_call_record WHERE test_run_id = ? ORDER BY sequence_no`, sourceID)
	if err != nil {
		return fmt.Errorf("list tool calls: %w", err)
	}
	var callIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		callIDs = append(callIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, callID := range callIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO tool_call_record (id, test_run_id, sequence_no, call_id, parent_call_id, tool_name, arguments_json, arguments_sha256,
				result_json, result_sha256, error_json, error_sha256, status, trace_source, truncated, original_bytes, duration_ms, started_at)
			SELECT ?, ?, sequence_no, call_id, parent_call_id, tool_name, arguments_json, arguments_sha256,
				result_json, result_sha256, error_json, error_sha256, status, trace_source, truncated, original_bytes, duration_ms, started_at
			FROM tool_call_record WHERE id = ?`, ulid.Make().String(), newID, callID); err != nil {
			return fmt.Errorf("copy tool call: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO evaluation_attempt (id, test_run_id, attempt_no, status, judge_model_alias) VALUES (?, ?, 1, 'PENDING', ?)`,
		ulid.Make().String(), newID, judgeAlias); err != nil {
		return fmt.Errorf("insert evaluation attempt: %w", err)
	}
	payload, _ := json.Marshal(map[string]string{"test_run_id": newID})
	if _, err := tx.ExecContext(ctx, runEnqueueSQL, ulid.Make().String(), run.QueueEvaluation, run.AggregateType, newID, run.StageEvaluate, string(payload)); err != nil {
		return fmt.Errorf("enqueue evaluation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit re-evaluation: %w", err)
	}
	return nil
}
