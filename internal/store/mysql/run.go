package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/oklog/ulid/v2"

	"agent-test-platform/internal/run"
)

// RunStore persists Test Runs in Platform MySQL.
type RunStore struct{ database *sql.DB }

// NewRunStore returns a store over the Platform MySQL database.
func NewRunStore(database *sql.DB) (*RunStore, error) {
	if database == nil {
		return nil, errors.New("Platform MySQL database is required")
	}
	return &RunStore{database: database}, nil
}

const (
	latestEvaluation = `(SELECT e.%s FROM evaluation_attempt e WHERE e.test_run_id = r.id AND e.status = 'COMPLETED' ORDER BY e.attempt_no DESC LIMIT 1)`

	runColumns = `r.id, r.status, r.execution_outcome, r.test_case_revision_id, r.dataset_revision_id, r.agent_id,
r.final_output, r.precondition_json, r.postcondition_json, r.state_diff_json, r.deterministic_json, r.error_json,
r.deterministic_score, %s, %s, r.created_at`
)

var selectRun = `SELECT ` + fmt.Sprintf(runColumns, fmt.Sprintf(latestEvaluation, "final_score"), fmt.Sprintf(latestEvaluation, "pass")) + ` FROM test_run r`

func rawJSON(value []byte) json.RawMessage {
	if value == nil {
		return json.RawMessage("null")
	}
	return json.RawMessage(value)
}

func scanRun(row rowScanner) (run.Run, error) {
	var item run.Run
	var outcome, output sql.NullString
	var pre, post, diff, det, errJSON []byte
	var detScore, finalScore sql.NullFloat64
	var pass sql.NullBool
	if err := row.Scan(&item.ID, &item.Status, &outcome, &item.TestCaseRevisionID, &item.DatasetRevisionID, &item.AgentID,
		&output, &pre, &post, &diff, &det, &errJSON, &detScore, &finalScore, &pass, &item.CreatedAt); err != nil {
		return item, err
	}
	if outcome.Valid {
		item.ExecutionOutcome = &outcome.String
	}
	if output.Valid {
		item.FinalOutput = &output.String
	}
	item.PreconditionResults, item.PostconditionResults, item.StateDiff = rawJSON(pre), rawJSON(post), rawJSON(diff)
	item.DeterministicDetails, item.Error = rawJSON(det), rawJSON(errJSON)
	if detScore.Valid {
		item.DeterministicScore = &detScore.Float64
	}
	if finalScore.Valid {
		item.FinalScore = &finalScore.Float64
	}
	if pass.Valid {
		item.Pass = &pass.Bool
	}
	return item, nil
}

func (store *RunStore) TestCases(ctx context.Context, ids []string) ([]run.Case, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := store.database.QueryContext(ctx,
		`SELECT id, dataset_revision_id, lifecycle_status, JSON_UNQUOTE(JSON_EXTRACT(content_json, '$.prompt'))
FROM test_case_revision WHERE id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("load Test Case Revisions: %w", err)
	}
	defer rows.Close()
	byID := make(map[string]run.Case, len(ids))
	for rows.Next() {
		var item run.Case
		var prompt sql.NullString
		if err := rows.Scan(&item.RevisionID, &item.DatasetRevisionID, &item.Lifecycle, &prompt); err != nil {
			return nil, fmt.Errorf("scan Test Case Revision: %w", err)
		}
		item.Prompt = prompt.String
		byID[item.RevisionID] = item
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	cases := make([]run.Case, 0, len(ids))
	for _, id := range ids {
		if item, ok := byID[id]; ok {
			cases = append(cases, item)
		}
	}
	return cases, nil
}

func (store *RunStore) AgentInfo(ctx context.Context, id string) (bool, bool, error) {
	var profile sql.NullString
	err := store.database.QueryRowContext(ctx, `SELECT database_profile_id FROM agent WHERE id = ?`, id).Scan(&profile)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("load Agent: %w", err)
	}
	return true, profile.Valid, nil
}

func (store *RunStore) FindCreation(ctx context.Context, requestKey string) (run.Creation, error) {
	rows, err := store.database.QueryContext(ctx, `SELECT id, request_sha256 FROM test_run WHERE request_key = ? ORDER BY id`, requestKey)
	if err != nil {
		return run.Creation{}, fmt.Errorf("load Runs by idempotency key: %w", err)
	}
	defer rows.Close()
	var creation run.Creation
	for rows.Next() {
		var id string
		var digest sql.NullString
		if err := rows.Scan(&id, &digest); err != nil {
			return run.Creation{}, fmt.Errorf("scan Run: %w", err)
		}
		creation.RunIDs = append(creation.RunIDs, id)
		creation.RequestSHA256 = digest.String
	}
	if err := rows.Err(); err != nil {
		return run.Creation{}, err
	}
	if len(creation.RunIDs) == 0 {
		return run.Creation{}, run.ErrNotFound
	}
	return creation, nil
}

func (store *RunStore) InsertRuns(ctx context.Context, agentID, requestKey, requestSHA256 string, runs []run.NewRun) error {
	tx, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin Run insert: %w", err)
	}
	defer tx.Rollback()
	for _, item := range runs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO test_run (id, idempotency_key, request_key, request_sha256, test_case_revision_id, dataset_revision_id,
agent_id, repetition_no, status, input_text) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'QUEUED', ?)`,
			item.ID, item.RowKey, requestKey, requestSHA256, item.TestCaseRevisionID, item.DatasetRevisionID,
			agentID, item.Repetition, item.Prompt); err != nil {
			if isDuplicate(err) {
				return run.ErrDuplicate
			}
			return fmt.Errorf("insert Test Run: %w", err)
		}
		payload, err := json.Marshal(map[string]string{"test_run_id": item.ID})
		if err != nil {
			return fmt.Errorf("encode queue payload: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO work_item (id, queue_name, aggregate_type, aggregate_id, stage_key, status, priority, available_at, fencing_token, attempt_count, max_attempts, payload_json)
VALUES (?, ?, ?, ?, ?, 'READY', 0, UTC_TIMESTAMP(6), 0, 0, 3, ?)`,
			ulid.Make().String(), run.QueueName, run.AggregateType, item.ID, run.StageExecute, payload); err != nil {
			return fmt.Errorf("enqueue Test Run: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit Test Runs: %w", err)
	}
	return nil
}

func (store *RunStore) GetRun(ctx context.Context, id string) (run.Run, error) {
	item, err := scanRun(store.database.QueryRowContext(ctx, selectRun+` WHERE r.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return run.Run{}, run.ErrNotFound
	}
	if err != nil {
		return run.Run{}, fmt.Errorf("load Test Run: %w", err)
	}
	return item, nil
}

func (store *RunStore) ListRuns(ctx context.Context, filter run.Filter, after *run.Cursor, limit int) ([]run.Run, error) {
	query := selectRun + ` WHERE 1 = 1`
	var args []any
	if filter.AgentID != "" {
		query += ` AND r.agent_id = ?`
		args = append(args, filter.AgentID)
	}
	if filter.TestCaseRevisionID != "" {
		query += ` AND r.test_case_revision_id = ?`
		args = append(args, filter.TestCaseRevisionID)
	}
	if filter.Status != "" {
		query += ` AND r.status = ?`
		args = append(args, filter.Status)
	}
	if after != nil {
		query += ` AND (r.created_at < ? OR (r.created_at = ? AND r.id < ?))`
		args = append(args, after.CreatedAt, after.CreatedAt, after.ID)
	}
	query += ` ORDER BY r.created_at DESC, r.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := store.database.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list Test Runs: %w", err)
	}
	defer rows.Close()
	var items []run.Run
	for rows.Next() {
		item, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan Test Run: %w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (store *RunStore) ListToolCalls(ctx context.Context, runID string) ([]run.ToolCall, error) {
	rows, err := store.database.QueryContext(ctx,
		`SELECT sequence_no, call_id, tool_name, arguments_json, result_json, error_json, status, duration_ms, trace_source
FROM tool_call_record WHERE test_run_id = ? ORDER BY sequence_no`, runID)
	if err != nil {
		return nil, fmt.Errorf("list Tool Call Records: %w", err)
	}
	defer rows.Close()
	items := []run.ToolCall{}
	for rows.Next() {
		var item run.ToolCall
		var arguments, result, errJSON []byte
		if err := rows.Scan(&item.Sequence, &item.CallID, &item.ToolName, &arguments, &result, &errJSON, &item.Status, &item.DurationMS, &item.TraceSource); err != nil {
			return nil, fmt.Errorf("scan Tool Call Record: %w", err)
		}
		item.Arguments, item.Result, item.Error = rawJSON(arguments), rawJSON(result), rawJSON(errJSON)
		items = append(items, item)
	}
	return items, rows.Err()
}

func (store *RunStore) Cancel(ctx context.Context, id string) (run.Run, error) {
	tx, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return run.Run{}, fmt.Errorf("begin cancel: %w", err)
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM test_run WHERE id = ? FOR UPDATE`, id).Scan(&status); errors.Is(err, sql.ErrNoRows) {
		return run.Run{}, run.ErrNotFound
	} else if err != nil {
		return run.Run{}, fmt.Errorf("lock Test Run: %w", err)
	}
	switch status {
	case "QUEUED", "PREPARING":
		if _, err := tx.ExecContext(ctx,
			`UPDATE test_run SET status = 'CANCELLED', execution_outcome = 'CANCELLED', cancel_requested_at = UTC_TIMESTAMP(6), finished_at = UTC_TIMESTAMP(6) WHERE id = ?`, id); err != nil {
			return run.Run{}, fmt.Errorf("cancel Test Run: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE work_item SET status = 'DONE', lease_owner = NULL, lease_until = NULL
WHERE queue_name = ? AND aggregate_type = ? AND aggregate_id = ? AND status = 'READY'`, run.QueueName, run.AggregateType, id); err != nil {
			return run.Run{}, fmt.Errorf("retire queue item: %w", err)
		}
	case "RUNNING":
		if _, err := tx.ExecContext(ctx, `UPDATE test_run SET cancel_requested_at = COALESCE(cancel_requested_at, UTC_TIMESTAMP(6)) WHERE id = ?`, id); err != nil {
			return run.Run{}, fmt.Errorf("flag Test Run cancellation: %w", err)
		}
	case run.StatusEvaluating:
		if _, err := tx.ExecContext(ctx, `UPDATE test_run SET status = 'CANCELLED', cancel_requested_at = COALESCE(cancel_requested_at, UTC_TIMESTAMP(6)) WHERE id = ?`, id); err != nil {
			return run.Run{}, fmt.Errorf("cancel evaluating Test Run: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE evaluation_attempt SET status = 'CANCELLED', finished_at = UTC_TIMESTAMP(6) WHERE test_run_id = ? AND status IN ('PENDING','RUNNING')`, id); err != nil {
			return run.Run{}, fmt.Errorf("cancel evaluation attempt: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE work_item SET status = 'DONE', lease_owner = NULL, lease_until = NULL
WHERE queue_name = ? AND aggregate_type = ? AND aggregate_id = ? AND status = 'READY'`, run.QueueEvaluation, run.AggregateType, id); err != nil {
			return run.Run{}, fmt.Errorf("retire evaluation item: %w", err)
		}
	case "CANCELLED":
	default:
		return run.Run{}, run.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return run.Run{}, fmt.Errorf("commit cancel: %w", err)
	}
	return store.GetRun(ctx, id)
}

const evaluationColumns = `e.id, e.test_run_id, e.attempt_no, e.status, e.judge_model_alias, e.llm_score, e.final_score, e.pass, e.created_at`

func scanEvaluation(row rowScanner) (run.Evaluation, error) {
	var item run.Evaluation
	var llm, final sql.NullFloat64
	var pass sql.NullBool
	if err := row.Scan(&item.ID, &item.TestRunID, &item.AttemptNo, &item.Status, &item.JudgeModelAlias, &llm, &final, &pass, &item.CreatedAt); err != nil {
		return item, err
	}
	if llm.Valid {
		item.LLMScore = &llm.Float64
	}
	if final.Valid {
		item.FinalScore = &final.Float64
	}
	if pass.Valid {
		item.Pass = &pass.Bool
	}
	return item, nil
}

func (store *RunStore) ListEvaluations(ctx context.Context, runID string) ([]run.Evaluation, error) {
	rows, err := store.database.QueryContext(ctx, `SELECT `+evaluationColumns+` FROM evaluation_attempt e WHERE e.test_run_id = ? ORDER BY e.attempt_no`, runID)
	if err != nil {
		return nil, fmt.Errorf("list Evaluation Attempts: %w", err)
	}
	defer rows.Close()
	items := []run.Evaluation{}
	for rows.Next() {
		item, err := scanEvaluation(rows)
		if err != nil {
			return nil, fmt.Errorf("scan Evaluation Attempt: %w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (store *RunStore) ListAllEvaluations(ctx context.Context, filter run.EvalFilter, after *run.Cursor, limit int) ([]run.Evaluation, error) {
	query := `SELECT ` + evaluationColumns + ` FROM evaluation_attempt e WHERE 1 = 1`
	var args []any
	if filter.Status != "" {
		query += ` AND e.status = ?`
		args = append(args, filter.Status)
	}
	if filter.Pass != nil {
		query += ` AND e.pass = ?`
		args = append(args, *filter.Pass)
	}
	if after != nil {
		query += ` AND (e.created_at < ? OR (e.created_at = ? AND e.id < ?))`
		args = append(args, after.CreatedAt, after.CreatedAt, after.ID)
	}
	query += ` ORDER BY e.created_at DESC, e.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := store.database.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list Evaluation Attempts: %w", err)
	}
	defer rows.Close()
	var items []run.Evaluation
	for rows.Next() {
		item, err := scanEvaluation(rows)
		if err != nil {
			return nil, fmt.Errorf("scan Evaluation Attempt: %w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// Benchmark counts COMPLETED Runs that reached the Agent. The verdict is the latest COMPLETED Evaluation Attempt;
// a critical failure is a Run with a critical deterministic check that did not pass.
func (store *RunStore) Benchmark(ctx context.Context, caseIDs []string) ([]run.BenchmarkEntry, error) {
	caseArgs := make([]any, len(caseIDs))
	for i, id := range caseIDs {
		caseArgs[i] = id
	}
	rows, err := store.database.QueryContext(ctx,
		`SELECT r.agent_id, COUNT(*),
COALESCE(100 * AVG(CASE WHEN `+fmt.Sprintf(latestEvaluation, "pass")+` THEN 1 ELSE 0 END), 0),
COALESCE(AVG(`+fmt.Sprintf(latestEvaluation, "final_score")+`), 0),
COALESCE(SUM(CASE WHEN JSON_CONTAINS(r.deterministic_json, '{"critical": true, "pass": false}') THEN 1 ELSE 0 END), 0)
FROM test_run r
WHERE r.test_case_revision_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(caseIDs)), ",")+`) AND r.status = 'COMPLETED' AND r.execution_outcome IS NOT NULL AND r.execution_outcome <> 'INFRA_ERROR'
GROUP BY r.agent_id ORDER BY r.agent_id`, caseArgs...)
	if err != nil {
		return nil, fmt.Errorf("aggregate benchmark: %w", err)
	}
	defer rows.Close()
	items := []run.BenchmarkEntry{}
	for rows.Next() {
		var item run.BenchmarkEntry
		if err := rows.Scan(&item.AgentID, &item.RunCount, &item.PassRate, &item.AvgScore, &item.CriticalFailures); err != nil {
			return nil, fmt.Errorf("scan benchmark: %w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// AgentDatabases returns every distinct database server configured on an Agent.
func (store *RunStore) AgentDatabases(ctx context.Context) ([]run.AgentDatabase, error) {
	rows, err := store.database.QueryContext(ctx, `SELECT DISTINCT p.host, p.port, p.username, p.password_secret_ref, p.tls_mode, COALESCE(p.ca_certificate_pem, '')
FROM agent a JOIN database_profile p ON p.id = a.database_profile_id`)
	if err != nil {
		return nil, fmt.Errorf("list Agent databases: %w", err)
	}
	defer rows.Close()
	var servers []run.AgentDatabase
	for rows.Next() {
		var item run.AgentDatabase
		if err := rows.Scan(&item.Host, &item.Port, &item.Username, &item.PasswordSecretRef, &item.TLSMode, &item.CACertificatePEM); err != nil {
			return nil, fmt.Errorf("scan Agent database: %w", err)
		}
		servers = append(servers, item)
	}
	return servers, rows.Err()
}

// RunStatuses returns the status of each known Run ID.
func (store *RunStore) RunStatuses(ctx context.Context, runIDs []string) (map[string]string, error) {
	result := make(map[string]string, len(runIDs))
	if len(runIDs) == 0 {
		return result, nil
	}
	args := make([]any, len(runIDs))
	for i, id := range runIDs {
		args[i] = id
	}
	rows, err := store.database.QueryContext(ctx, `SELECT id, status FROM test_run WHERE id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(runIDs)), ",")+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("load Run statuses: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			return nil, fmt.Errorf("scan Run status: %w", err)
		}
		result[id] = status
	}
	return result, rows.Err()
}

// LoadAgentDatabase reads the connection settings of one Database Profile.
func (store *RunStore) LoadAgentDatabase(ctx context.Context, profileID string) (run.AgentDatabase, error) {
	var item run.AgentDatabase
	err := store.database.QueryRowContext(ctx, `SELECT host, port, username, password_secret_ref, tls_mode, COALESCE(ca_certificate_pem, '') FROM database_profile WHERE id = ?`, profileID).
		Scan(&item.Host, &item.Port, &item.Username, &item.PasswordSecretRef, &item.TLSMode, &item.CACertificatePEM)
	if errors.Is(err, sql.ErrNoRows) {
		return run.AgentDatabase{}, run.ErrNotFound
	}
	if err != nil {
		return run.AgentDatabase{}, fmt.Errorf("load Agent database: %w", err)
	}
	return item, nil
}
