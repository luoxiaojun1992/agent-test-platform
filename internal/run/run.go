// Package run creates and reads Test Runs (docs/technical/01 section 7). Creation only records QUEUED rows and
// their queue items; execution, evidence capture, and evaluation are performed by Workers.
package run

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

const (
	MaxPageSize = 200

	QueueName     = "run"
	AggregateType = "test_run"
	StageExecute  = "execute"

	maxRepetitions   = 10
	maxRunsPerCreate = 1000
)

var (
	ErrInvalidInput         = errors.New("run request is invalid")
	ErrNotFound             = errors.New("not found")
	ErrConflict             = errors.New("test run is in a terminal state and cannot be cancelled")
	ErrIdempotencyConflict  = errors.New("Idempotency-Key was already used for a different request")
	ErrDuplicate            = errors.New("run violates a unique key")
	ErrAgentDatabaseMissing = errors.New("the Agent has no database configured; configure one on the Agent first")
	ErrAgentBusy            = errors.New("the Agent already has a Test Run in progress")
	ErrInvalidCursor        = errors.New("invalid list cursor")
	ErrInvalidLimit         = errors.New("limit must be from 1 to 200")
)

// activeStatuses are the Test Run statuses that are not terminal; a Run in one of these still
// occupies its Agent, so a second concurrent Run for the same Agent must be refused.
var activeStatuses = map[string]bool{"QUEUED": true, "PREPARING": true, "RUNNING": true, "EVALUATING": true}

var (
	ulidPattern  = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)
	runStatuses  = map[string]bool{"QUEUED": true, "PREPARING": true, "RUNNING": true, "EVALUATING": true, "COMPLETED": true, "INVALID_TEST_DATA": true, "EVALUATION_FAILED": true, "CANCELLED": true}
	evalStatuses = map[string]bool{"PENDING": true, "RUNNING": true, "COMPLETED": true, "FAILED": true, "CANCELLED": true}
	terminal     = map[string]bool{"COMPLETED": true, "INVALID_TEST_DATA": true, "EVALUATION_FAILED": true}
)

// IsInvalidInput reports whether err is a request validation failure, which the API reports as 400.
func IsInvalidInput(err error) bool { return errors.Is(err, ErrInvalidInput) }

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidInput, fmt.Sprintf(format, args...))
}

// Request is the body of POST /test-runs: the Test Case Revisions to run on one Agent.
type Request struct {
	AgentID             string   `json:"agent_id"`
	TestCaseRevisionIDs []string `json:"test_case_revision_ids"`
	Repetitions         int      `json:"repetitions"`
}

// Run is the public Test Run view. JSON members are verbatim stored documents.
type Run struct {
	ID                 string          `json:"id"`
	Status             string          `json:"status"`
	ExecutionOutcome   *string         `json:"execution_outcome,omitempty"`
	TestCaseRevisionID string          `json:"test_case_revision_id"`
	DatasetRevisionID  string          `json:"dataset_revision_id"`
	AgentID            string          `json:"agent_id"`
	SessionID          string          `json:"session_id"`
	FinalOutput        *string         `json:"final_output"`
	Error              json.RawMessage `json:"error"`
	FinalScore         *float64        `json:"final_score"`
	Pass               *bool           `json:"pass"`
	CreatedAt          time.Time       `json:"created_at"`
}

// ToolCall is one stored Tool Call Record.
type ToolCall struct {
	Sequence    int             `json:"sequence"`
	CallID      string          `json:"call_id"`
	ToolName    string          `json:"tool_name"`
	Arguments   json.RawMessage `json:"arguments"`
	Result      json.RawMessage `json:"result"`
	Error       json.RawMessage `json:"error"`
	Status      string          `json:"status"`
	DurationMS  int64           `json:"duration_ms"`
	TraceSource string          `json:"trace_source"`
}

// Evaluation is one Evaluation Attempt.
type Evaluation struct {
	ID              string    `json:"id"`
	TestRunID       string    `json:"test_run_id"`
	AttemptNo       int       `json:"attempt_no"`
	Status          string    `json:"status"`
	JudgeModelAlias string    `json:"judge_model_alias"`
	LLMScore        *float64  `json:"llm_score"`
	FinalScore      *float64  `json:"final_score"`
	Pass            *bool     `json:"pass"`
	CreatedAt       time.Time `json:"created_at"`
}

// BenchmarkEntry aggregates the Runs of one Agent over a set of Test Case Revisions.
// Only COMPLETED Runs whose execution outcome reached the Agent count; INVALID_TEST_DATA, INFRA_ERROR and unfinished
// Runs are excluded from every figure (docs/architecture.md section 14).
type BenchmarkEntry struct {
	AgentID  string  `json:"agent_id"`
	RunCount int     `json:"run_count"`
	PassRate float64 `json:"pass_rate"`
	AvgScore float64 `json:"avg_score"`
}

// Case is one Test Case Revision to run.
type Case struct {
	RevisionID        string
	Lifecycle         string
	DatasetRevisionID string
	Prompt            string
}

// NewRun is one row to insert.
type NewRun struct {
	ID, RowKey, TestCaseRevisionID, DatasetRevisionID, Prompt string
	Repetition                                                int
}

// Creation is a stored batch of Runs.
type Creation struct {
	RunIDs        []string
	RequestSHA256 string
}

// Filter narrows GET /test-runs.
type Filter struct{ AgentID, TestCaseRevisionID, Status string }

// EvalFilter narrows GET /evaluations.
type EvalFilter struct {
	Status string
	Pass   *bool
}

// Cursor is the keyset position of a list.
type Cursor struct {
	CreatedAt time.Time
	ID        string
}

// Store persists Runs. Lookups of a missing row return ErrNotFound.
type Store interface {
	// TestCases returns the known Test Case Revisions among ids, in the order of ids.
	TestCases(ctx context.Context, ids []string) ([]Case, error)
	// AgentInfo reports whether the Agent exists and whether its Agent has a database configured for seed data.
	AgentInfo(ctx context.Context, id string) (exists, hasDatabase bool, err error)
	// AgentHasActiveRun reports whether the Agent already has a Run in a non-terminal state.
	AgentHasActiveRun(ctx context.Context, agentID string) (bool, error)
	FindCreation(ctx context.Context, requestKey string) (Creation, error)
	// InsertRuns inserts every Run and its queue item in one transaction.
	InsertRuns(ctx context.Context, agentID, requestKey, requestSHA256 string, runs []NewRun) error
	GetRun(ctx context.Context, id string) (Run, error)
	ListRuns(ctx context.Context, filter Filter, after *Cursor, limit int) ([]Run, error)
	ListToolCalls(ctx context.Context, runID string) ([]ToolCall, error)
	// Cancel applies the cancellation transition atomically and returns the Run.
	Cancel(ctx context.Context, id string) (Run, error)
	ListEvaluations(ctx context.Context, runID string) ([]Evaluation, error)
	ListAllEvaluations(ctx context.Context, filter EvalFilter, after *Cursor, limit int) ([]Evaluation, error)
	JudgeAliasExists(ctx context.Context, alias string) (bool, error)
	// Reevaluate copies the source Run's execution evidence into a new EVALUATING Run with a PENDING attempt for the alias.
	// It returns ErrNotFound for a missing source and ErrConflict when the source has no finished evidence.
	Reevaluate(ctx context.Context, sourceID, newID, requestKey, requestSHA256, judgeAlias string) error
	Benchmark(ctx context.Context, testCaseRevisionIDs []string) ([]BenchmarkEntry, error)
}

// AgentLocker serializes Run creation per Agent. It is the belt-and-suspenders complement to the
// AgentHasActiveRun status check, closing the race between the check and the insert (08 section 5).
type AgentLocker interface {
	LockAgent(ctx context.Context, agentID string) (unlock func(), err error)
}

// Service manages Test Runs.
type Service struct {
	store  Store
	locker AgentLocker
}

// NewService returns a Service.
func NewService(store Store) (*Service, error) {
	if store == nil {
		return nil, errors.New("run store is required")
	}
	return &Service{store: store}, nil
}

// SetAgentLocker installs an optional per-Agent distributed lock. When set, Create acquires the
// lock before the status check so concurrent creates for the same Agent serialize.
func (service *Service) SetAgentLocker(locker AgentLocker) { service.locker = locker }

// Create queues one Run per Test Case Revision and repetition. The Agent must have a database configured: the Dataset's
// seed data is written there before the Run.
func (service *Service) Create(ctx context.Context, key string, request Request) ([]string, error) {
	if !ulidPattern.MatchString(request.AgentID) {
		return nil, invalid("agent_id must be a ULID")
	}
	if len(request.TestCaseRevisionIDs) < 1 || len(request.TestCaseRevisionIDs) > maxRunsPerCreate {
		return nil, invalid("test_case_revision_ids must contain 1-%d items", maxRunsPerCreate)
	}
	seen := make(map[string]bool, len(request.TestCaseRevisionIDs))
	for _, id := range request.TestCaseRevisionIDs {
		if !ulidPattern.MatchString(id) {
			return nil, invalid("test_case_revision_ids must be ULIDs")
		}
		if seen[id] {
			return nil, invalid("test case revision %s is listed twice", id)
		}
		seen[id] = true
	}
	if request.Repetitions == 0 {
		request.Repetitions = 1
	}
	if request.Repetitions < 1 || request.Repetitions > maxRepetitions {
		return nil, invalid("repetitions must be from 1 to %d", maxRepetitions)
	}
	if len(request.TestCaseRevisionIDs)*request.Repetitions > maxRunsPerCreate {
		return nil, invalid("a request may create at most %d Runs", maxRunsPerCreate)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	sum := sha256.Sum256(encoded)
	digest := hex.EncodeToString(sum[:])
	if service.locker != nil {
		unlock, err := service.locker.LockAgent(ctx, request.AgentID)
		if err != nil {
			return nil, fmt.Errorf("lock Agent: %w", err)
		}
		defer unlock()
	}
	if replay, err := service.store.FindCreation(ctx, key); err == nil {
		if replay.RequestSHA256 != digest {
			return nil, ErrIdempotencyConflict
		}
		return replay.RunIDs, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	cases, err := service.store.TestCases(ctx, request.TestCaseRevisionIDs)
	if err != nil {
		return nil, err
	}
	if len(cases) != len(request.TestCaseRevisionIDs) {
		return nil, invalid("some test case revisions do not exist")
	}
	for _, item := range cases {
		if item.Lifecycle == "DEPRECATED" {
			return nil, invalid("test case revision %s is DEPRECATED", item.RevisionID)
		}
	}
	exists, hasDatabase, err := service.store.AgentInfo(ctx, request.AgentID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, invalid("agent %s does not exist", request.AgentID)
	}
	if !hasDatabase {
		return nil, ErrAgentDatabaseMissing
	}
	busy, err := service.store.AgentHasActiveRun(ctx, request.AgentID)
	if err != nil {
		return nil, err
	}
	if busy {
		return nil, ErrAgentBusy
	}
	runs := make([]NewRun, 0, len(cases)*request.Repetitions)
	ids := make([]string, 0, cap(runs))
	for _, item := range cases {
		for repetition := 1; repetition <= request.Repetitions; repetition++ {
			id := ulid.Make().String()
			rowSum := sha256.Sum256([]byte(key + "/" + id))
			runs = append(runs, NewRun{ID: id, RowKey: hex.EncodeToString(rowSum[:]), TestCaseRevisionID: item.RevisionID, DatasetRevisionID: item.DatasetRevisionID, Prompt: item.Prompt, Repetition: repetition})
			ids = append(ids, id)
		}
	}
	err = service.store.InsertRuns(ctx, request.AgentID, key, digest, runs)
	if err == nil {
		return ids, nil
	}
	if !errors.Is(err, ErrDuplicate) {
		return nil, err
	}
	// Either a concurrent replay of this key or a Run for the same Agent, case and repetition already exists.
	replay, findErr := service.store.FindCreation(ctx, key)
	if findErr == nil && replay.RequestSHA256 == digest {
		return replay.RunIDs, nil
	}
	return nil, ErrIdempotencyConflict
}

// Get returns one Run or ErrNotFound.
func (service *Service) Get(ctx context.Context, id string) (Run, error) {
	if !ulidPattern.MatchString(id) {
		return Run{}, ErrNotFound
	}
	return service.store.GetRun(ctx, id)
}

// List returns one page of Runs, newest first.
func (service *Service) List(ctx context.Context, filter Filter, cursor string, limit int) ([]Run, string, error) {
	if filter.AgentID != "" && !ulidPattern.MatchString(filter.AgentID) ||
		filter.TestCaseRevisionID != "" && !ulidPattern.MatchString(filter.TestCaseRevisionID) {
		return nil, "", invalid("agent_id and test_case_revision_id must be ULIDs")
	}
	if filter.Status != "" && !runStatuses[filter.Status] {
		return nil, "", invalid("unknown status %q", filter.Status)
	}
	after, err := pageArgs(cursor, limit)
	if err != nil {
		return nil, "", err
	}
	items, err := service.store.ListRuns(ctx, filter, after, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("list Test Runs: %w", err)
	}
	if len(items) <= limit {
		return items, "", nil
	}
	items = items[:limit]
	return items, encodeCursor(Cursor{CreatedAt: items[limit-1].CreatedAt, ID: items[limit-1].ID}), nil
}

// ToolCalls returns the Run's Tool Call Records in sequence order.
func (service *Service) ToolCalls(ctx context.Context, runID string) ([]ToolCall, error) {
	if _, err := service.Get(ctx, runID); err != nil {
		return nil, err
	}
	return service.store.ListToolCalls(ctx, runID)
}

// Cancel requests cancellation. QUEUED and PREPARING Runs end CANCELLED at once; a RUNNING Run is flagged and the
// Worker stops it and evaluates the cancelled execution. Terminal Runs return ErrConflict; cancelling a CANCELLED Run is a no-op.
func (service *Service) Cancel(ctx context.Context, runID string) (Run, error) {
	if _, err := service.Get(ctx, runID); err != nil {
		return Run{}, err
	}
	return service.store.Cancel(ctx, runID)
}

// Evaluations returns every Evaluation Attempt of a Run, oldest first.
func (service *Service) Evaluations(ctx context.Context, runID string) ([]Evaluation, error) {
	if _, err := service.Get(ctx, runID); err != nil {
		return nil, err
	}
	return service.store.ListEvaluations(ctx, runID)
}

// AllEvaluations searches Evaluation Attempts across Runs, newest first.
func (service *Service) AllEvaluations(ctx context.Context, filter EvalFilter, cursor string, limit int) ([]Evaluation, string, error) {
	if filter.Status != "" && !evalStatuses[filter.Status] {
		return nil, "", invalid("unknown status %q", filter.Status)
	}
	after, err := pageArgs(cursor, limit)
	if err != nil {
		return nil, "", err
	}
	items, err := service.store.ListAllEvaluations(ctx, filter, after, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("list Evaluation Attempts: %w", err)
	}
	if len(items) <= limit {
		return items, "", nil
	}
	items = items[:limit]
	return items, encodeCursor(Cursor{CreatedAt: items[limit-1].CreatedAt, ID: items[limit-1].ID}), nil
}

// Benchmark aggregates the Runs of the given Test Case Revisions by Agent.
func (service *Service) Benchmark(ctx context.Context, testCaseRevisionIDs []string) ([]BenchmarkEntry, error) {
	if len(testCaseRevisionIDs) < 1 || len(testCaseRevisionIDs) > 100 {
		return nil, invalid("test_case_revision_id must be given 1-100 times")
	}
	for _, id := range testCaseRevisionIDs {
		if !ulidPattern.MatchString(id) {
			return nil, invalid("test_case_revision_id must be a ULID")
		}
	}
	return service.store.Benchmark(ctx, testCaseRevisionIDs)
}

// IsTerminal reports whether a Run status is final and not cancellable.
func IsTerminal(status string) bool { return terminal[status] }

func pageArgs(cursor string, limit int) (*Cursor, error) {
	if limit < 1 || limit > MaxPageSize {
		return nil, ErrInvalidLimit
	}
	if cursor == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, ErrInvalidCursor
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return nil, ErrInvalidCursor
	}
	createdAt, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return nil, ErrInvalidCursor
	}
	if _, err := ulid.ParseStrict(parts[1]); err != nil {
		return nil, ErrInvalidCursor
	}
	return &Cursor{CreatedAt: createdAt, ID: parts[1]}, nil
}

func encodeCursor(cursor Cursor) string {
	return base64.RawURLEncoding.EncodeToString([]byte(cursor.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + cursor.ID))
}

// Reevaluate creates a new Test Run that re-judges the source Run's stored evidence with the chosen Judge alias.
// The source Run is never modified. The call is idempotent per key.
func (service *Service) Reevaluate(ctx context.Context, sourceID, key, judgeAlias string) (string, error) {
	if !ulidPattern.MatchString(sourceID) {
		return "", ErrNotFound
	}
	if judgeAlias == "" || len(judgeAlias) > 128 {
		return "", invalid("judge_model_alias must be 1 to 128 characters")
	}
	sum := sha256.Sum256([]byte(sourceID + "/" + judgeAlias))
	digest := hex.EncodeToString(sum[:])
	if replay, err := service.store.FindCreation(ctx, key); err == nil {
		if replay.RequestSHA256 != digest || len(replay.RunIDs) != 1 {
			return "", ErrIdempotencyConflict
		}
		return replay.RunIDs[0], nil
	} else if !errors.Is(err, ErrNotFound) {
		return "", err
	}
	if known, err := service.store.JudgeAliasExists(ctx, judgeAlias); err != nil {
		return "", err
	} else if !known {
		return "", invalid("judge_model_alias %q is not a registered Judge model", judgeAlias)
	}
	id := ulid.Make().String()
	err := service.store.Reevaluate(ctx, sourceID, id, key, digest, judgeAlias)
	if errors.Is(err, ErrDuplicate) {
		if replay, findErr := service.store.FindCreation(ctx, key); findErr == nil && replay.RequestSHA256 == digest && len(replay.RunIDs) == 1 {
			return replay.RunIDs[0], nil
		}
		return "", ErrIdempotencyConflict
	}
	if err != nil {
		return "", err
	}
	return id, nil
}
