// Package runexec executes one Test Run: provisions the isolated Run database, calls the Target
// Agent, and gathers deterministic evidence for later LLM evaluation.
package runexec

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"agent-test-platform/contracts"
	"agent-test-platform/internal/agent"
	"agent-test-platform/internal/evaluation"
	"agent-test-platform/internal/evidence"
	"agent-test-platform/internal/generation"
	"agent-test-platform/internal/run"
	"agent-test-platform/internal/sandbox"
	"agent-test-platform/internal/toolmatch"
	"agent-test-platform/internal/validation"
)

const (
	CategoryState = "STATE"
	CategoryTool  = "TOOL"

	checkStateDiff = "state_diff"
	checkToolChain = "tool_chain"
	checkOutcome   = "run_outcome"

	cancelPollInterval = 2 * time.Second
	cleanupTimeout     = 30 * time.Second
)

// Check is the persisted shape of one deterministic check inside test_run.deterministic_json.
type Check struct {
	ID       string  `json:"id"`
	Category string  `json:"category"`
	Critical bool    `json:"critical"`
	Weight   float64 `json:"weight"`
	Pass     bool    `json:"pass"`
	Expected any     `json:"expected,omitempty"`
	Actual   any     `json:"actual,omitempty"`
	Reason   string  `json:"reason,omitempty"`
}

// StateRecord is the persisted shape of test_run.state_diff_json.
type StateRecord struct {
	Diff   sandbox.StateDiff      `json:"diff"`
	Report sandbox.MutationReport `json:"mutation_report"`
}

type Secrets interface {
	Resolve(ctx context.Context, name string) (string, error)
}

type ToolCalls interface {
	SaveToolCalls(ctx context.Context, runID string, records []evidence.Record) error
}

type CancelChecker interface {
	CancelRequested(ctx context.Context, runID string) (bool, error)
}

type Deps struct {
	Secrets      Secrets
	ToolCalls    ToolCalls
	Cancel       CancelChecker
	HTTP         agent.HTTPDoer
	Validator    *contracts.Validator
	AgentTimeout time.Duration
	Now          func() time.Time
}

type Executor struct{ deps Deps }

func New(deps Deps) (*Executor, error) {
	if deps.Secrets == nil || deps.ToolCalls == nil || deps.Cancel == nil || deps.HTTP == nil || deps.Validator == nil {
		return nil, errors.New("run executor dependencies are required")
	}
	if deps.AgentTimeout <= 0 {
		deps.AgentTimeout = 120 * time.Second
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &Executor{deps: deps}, nil
}

func scriptLimits() validation.ScriptLimits {
	return validation.ScriptLimits{MaxStatementBytes: 4096, MaxScriptBytes: 262144, MaxTables: 20, MaxRowsPerStatement: 1000, MaxTotalRows: 10000}
}

// retryableFailure marks an error the worker should retry.
type retryableFailure struct{ error }

func (err retryableFailure) Unwrap() error   { return err.error }
func (err retryableFailure) Retryable() bool { return true }

func retry(err error) error { return retryableFailure{err} }

// Execute provisions the environment and runs the Agent. Errors it returns are infrastructure
// failures; fixture and Agent failures are reported through the Settlement.
func (executor *Executor) Execute(ctx context.Context, job run.Job, markRunning func(context.Context) error) (run.Settlement, error) {
	var candidate generation.Candidate
	if err := json.Unmarshal(job.CaseJSON, &candidate); err != nil {
		return run.Settlement{}, fmt.Errorf("decode test case content: %w", err)
	}
	schemaInfo, err := validation.ValidateSQLScript(job.SchemaSQL, validation.SQLSchema, scriptLimits())
	if err != nil {
		return run.Settlement{}, fmt.Errorf("validate dataset schema: %w", err)
	}
	seedInfo, err := validation.ValidateSQLScript(job.SeedSQL, validation.SQLSeed, scriptLimits())
	if err != nil {
		return run.Settlement{}, fmt.Errorf("validate dataset seed: %w", err)
	}
	admin, closeAdmin, err := executor.adminConnection(ctx, job)
	if err != nil {
		return run.Settlement{}, err
	}
	defer closeAdmin()

	if job.AgentDatabase == nil || job.AgentDatabase.Database == "" {
		return run.Settlement{}, errors.New("the Agent has no fixed database configured")
	}
	database, err := admin.OpenDatabase(ctx, job.AgentDatabase.Database)
	if err != nil {
		return run.Settlement{}, retry(fmt.Errorf("open Agent database: %w", err))
	}
	defer database.Close()

	tableNames := make([]string, len(schemaInfo.Tables))
	for index, table := range schemaInfo.Tables {
		tableNames[index] = table.Name
	}
	startedAt := executor.deps.Now().UTC()
	if err := sandbox.ProvisionFixedDatabase(ctx, database, tableNames, schemaInfo.Statements, seedInfo.Statements); err != nil {
		if errors.Is(err, sandbox.ErrLeftoverTestData) {
			return run.Settlement{Status: run.StatusInvalidData, Error: errorJSON("LEFTOVER_TEST_DATA", err.Error())}, nil
		}
		return run.Settlement{}, retry(fmt.Errorf("provision Agent database: %w", err))
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		_ = sandbox.CleanupFixedDatabase(cleanupCtx, database, tableNames)
	}()

	settlement, err := executor.runCase(ctx, job, candidate, schemaInfo.Tables, job.AgentDatabase.Database, database, markRunning)
	if err != nil {
		return run.Settlement{}, err
	}
	settlement.StartedAt = &startedAt
	cleaned := executor.deps.Now().UTC()
	settlement.CleanedAt = &cleaned
	return settlement, nil
}

// adminConnection connects to the Agent's own database server, which receives the seed data. Its credentials stay here and
// are never sent to the Agent.
func (executor *Executor) adminConnection(ctx context.Context, job run.Job) (sandbox.AdminConnection, func(), error) {
	if job.AgentDatabase == nil {
		return sandbox.AdminConnection{}, nil, errors.New("the Agent has no database configured")
	}
	profile := job.AgentDatabase
	password, err := executor.deps.Secrets.Resolve(ctx, profile.PasswordSecretRef)
	if err != nil {
		return sandbox.AdminConnection{}, nil, retry(fmt.Errorf("resolve agent database password: %w", err))
	}
	connection, err := sandbox.OpenServer(sandbox.ServerEndpoint{Host: profile.Host, Port: profile.Port, Username: profile.Username,
		Password: password, TLSMode: profile.TLSMode, CACertificatePEM: profile.CACertificatePEM})
	if err != nil {
		return sandbox.AdminConnection{}, nil, err
	}
	return connection, func() { _ = connection.Admin.Close() }, nil
}

func (executor *Executor) dropDatabase(admin sandbox.AdminConnection, runID string) {
	identifiers, err := sandbox.DeriveRunIdentifiers(runID)
	if err != nil {
		return
	}
	quoted, err := sandbox.QuoteIdentifier(identifiers.Database)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	_, _ = admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+quoted)
}

func (executor *Executor) runCase(ctx context.Context, job run.Job, candidate generation.Candidate, tables []validation.TableInfo, databaseName string, database *sql.DB, markRunning func(context.Context) error) (run.Settlement, error) {
	specs, err := tableSpecs(tables)
	if err != nil {
		return run.Settlement{}, err
	}
	preconditions, preOK, err := runAssertions(ctx, database, candidate.Preconditions)
	if err != nil {
		return run.Settlement{}, err
	}
	preJSON, _ := json.Marshal(preconditions)
	if !preOK {
		return run.Settlement{Status: run.StatusInvalidData, Precondition: preJSON}, nil
	}
	before, err := sandbox.CaptureState(ctx, database, specs)
	if err != nil {
		return run.Settlement{}, fmt.Errorf("capture before state: %w", err)
	}
	if err := markRunning(ctx); err != nil {
		return run.Settlement{}, err
	}

	token, err := executor.resolveAgentToken(ctx, job)
	if err != nil {
		return run.Settlement{}, retry(fmt.Errorf("resolve agent token: %w", err))
	}
	client, err := agent.NewSSEClient(job.Agent.Endpoint, token, executor.deps.HTTP, executor.deps.Validator)
	if err != nil {
		return run.Settlement{}, fmt.Errorf("build agent client: %w", err)
	}
	knowledgeIDs, cleanupKnowledge, err := executor.provisionKnowledge(ctx, job.RunID, client, candidate.KnowledgeDocuments)
	if err != nil {
		return run.Settlement{}, retry(fmt.Errorf("provision knowledge: %w", err))
	}
	defer cleanupKnowledge()

	request, err := buildRequest(job, candidate, databaseName, executor.deps.Now().Add(executor.deps.AgentTimeout).UTC(), knowledgeIDs)
	if err != nil {
		return run.Settlement{}, err
	}

	agentCtx, cancelAgent := context.WithDeadline(ctx, request.Deadline)
	cancelled := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		executor.watchCancel(agentCtx, job.RunID, cancelAgent, cancelled)
	}()
	result, chatErr := client.Chat(agentCtx, request, func(agent.SessionEvent) error { return nil })
	agentTimedOut := errors.Is(agentCtx.Err(), context.DeadlineExceeded)
	cancelAgent()
	<-watcherDone
	if ctx.Err() != nil {
		return run.Settlement{}, ctx.Err()
	}
	finished := executor.deps.Now().UTC()

	wasCancelled := false
	select {
	case <-cancelled:
		wasCancelled = true
	default:
	}

	settlement := run.Settlement{Status: run.StatusEvaluating, Precondition: preJSON, AgentFinishedAt: &finished}
	switch {
	case wasCancelled:
		settlement.Outcome = run.OutcomeCancelled
	case agentTimedOut:
		settlement.Outcome = run.OutcomeTimeout
	case chatErr != nil:
		settlement.Outcome = run.OutcomeAgentError
		settlement.Error = errorJSON("AGENT_CALL_FAILED", chatErr.Error())
	case result.Status == "TIMEOUT":
		settlement.Outcome = run.OutcomeTimeout
	case result.Status == "AGENT_ERROR":
		settlement.Outcome = run.OutcomeAgentError
		if result.Error != nil {
			settlement.Error, _ = json.Marshal(result.Error)
		}
	default:
		settlement.Outcome = run.OutcomeCompleted
	}

	var calls []toolmatch.Call
	if chatErr == nil {
		calls = result.Normalized
		if len(result.Evidence) > 0 {
			if err := executor.deps.ToolCalls.SaveToolCalls(ctx, job.RunID, result.Evidence); err != nil {
				return run.Settlement{}, retry(fmt.Errorf("save tool calls: %w", err))
			}
		}
		if result.FinalOutput != nil {
			output, digest := capOutput(*result.FinalOutput)
			settlement.FinalOutput, settlement.OutputSHA256 = &output, &digest
		}
	}

	after, err := sandbox.CaptureState(ctx, database, specs)
	if err != nil {
		return run.Settlement{}, fmt.Errorf("capture after state: %w", err)
	}
	diff, err := sandbox.DiffStates(before, after)
	if err != nil {
		return run.Settlement{}, fmt.Errorf("diff state: %w", err)
	}
	report, err := sandbox.EvaluateMutations(diff, generation.RuntimeMutationRules(candidate.AllowedStateChanges))
	if err != nil {
		return run.Settlement{}, fmt.Errorf("evaluate mutations: %w", err)
	}
	postconditions, _, err := runAssertions(ctx, database, candidate.Postconditions)
	if err != nil {
		return run.Settlement{}, err
	}
	settlement.StateDiff, _ = json.Marshal(StateRecord{Diff: diff, Report: report})
	settlement.Postcondition, _ = json.Marshal(postconditions)

	checks, err := buildChecks(candidate, settlement.Outcome, postconditions, report, calls)
	if err != nil {
		return run.Settlement{}, err
	}
	settlement.Deterministic, _ = json.Marshal(checks)
	score := 0.0
	for _, check := range checks {
		if check.Pass {
			score += check.Weight * 100
		}
	}
	settlement.Score = &score
	return settlement, nil
}

func (executor *Executor) watchCancel(ctx context.Context, runID string, cancelAgent context.CancelFunc, cancelled chan struct{}) {
	ticker := time.NewTicker(cancelPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			requested, err := executor.deps.Cancel.CancelRequested(ctx, runID)
			if err == nil && requested {
				close(cancelled)
				cancelAgent()
				return
			}
		}
	}
}

// resolveAgentToken returns the bearer token for calling the Target: a client-credentials access
// token when OAuth is configured, otherwise the static bearer secret.
func (executor *Executor) resolveAgentToken(ctx context.Context, job run.Job) (string, error) {
	if job.Agent.TokenURL != "" {
		secret, err := executor.deps.Secrets.Resolve(ctx, job.Agent.ClientSecretRef)
		if err != nil {
			return "", fmt.Errorf("resolve agent client secret: %w", err)
		}
		return agent.FetchClientCredentialsToken(ctx, executor.deps.HTTP, job.Agent.TokenURL, job.Agent.ClientID, secret)
	}
	return executor.deps.Secrets.Resolve(ctx, job.Agent.TokenRef)
}

// provisionKnowledge uploads the Test Case's knowledge documents to the Target and waits for each
// to be READY (08 section 3). It returns the document IDs in order and a teardown func that deletes
// them. Knowledge is part of the test data and lives inside the Run lifecycle.
func (executor *Executor) provisionKnowledge(ctx context.Context, runID string, client *agent.SSEClient, documents []generation.KnowledgeDocument) ([]string, func(), error) {
	if len(documents) == 0 {
		return []string{}, func() {}, nil
	}
	now := executor.deps.Now().UTC()
	ids := make([]string, 0, len(documents))
	for index, document := range documents {
		request := agent.KnowledgeUploadRequest{
			SchemaVersion:    "1.0",
			RequestID:        fmt.Sprintf("%s-knowledge-%d", runID, index),
			Name:             document.Name,
			Text:             document.Text,
			ContentSHA256:    document.ContentSHA256,
			IndexingDeadline: now.Add(10 * time.Minute),
			ExpiresAt:        now.Add(24 * time.Hour),
			Metadata:         document.Metadata,
		}
		accepted, err := client.UploadKnowledge(ctx, request)
		if err != nil {
			return nil, func() {}, fmt.Errorf("upload knowledge %q: %w", document.Name, err)
		}
		if _, err := client.WaitKnowledgeReady(ctx, accepted.DocumentID, document.ContentSHA256); err != nil {
			return nil, func() {}, fmt.Errorf("wait knowledge %q ready: %w", document.Name, err)
		}
		ids = append(ids, accepted.DocumentID)
	}
	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		for _, id := range ids {
			_ = client.DeleteKnowledge(cleanupCtx, id)
		}
	}
	return ids, cleanup, nil
}

func buildRequest(job run.Job, candidate generation.Candidate, databaseName string, deadline time.Time, knowledgeDocumentIDs []string) (agent.ChatRequest, error) {
	mode := candidate.AgentMode
	if mode != "ANSWER" && mode != "TASK" {
		return agent.ChatRequest{}, fmt.Errorf("unsupported agent mode %q", mode)
	}
	var tools *[]agent.TargetTool
	if len(job.Agent.ToolsJSON) > 0 && string(job.Agent.ToolsJSON) != "null" {
		var parsed []agent.TargetTool
		if err := json.Unmarshal(job.Agent.ToolsJSON, &parsed); err != nil {
			return agent.ChatRequest{}, fmt.Errorf("decode agent tool catalog: %w", err)
		}
		tools = &parsed
	}
	content := job.Prompt + "\n\nUse the database named `" + databaseName + "` for all data access."
	if knowledgeDocumentIDs == nil {
		knowledgeDocumentIDs = []string{}
	}
	return agent.ChatRequest{
		SchemaVersion: "1.0", RunID: job.RunID, Model: job.Agent.Model, Mode: mode,
		Message:              agent.ChatMessage{ID: "msg-" + job.RunID, Role: "user", Content: content},
		Deadline:             deadline,
		Tools:                tools,
		KnowledgeDocumentIDs: knowledgeDocumentIDs,
	}, nil
}

func capOutput(output string) (string, string) {
	sum := sha256.Sum256([]byte(output))
	digest := hex.EncodeToString(sum[:])
	if utf8.RuneCountInString(output) <= run.MaxFinalOutputLength {
		return output, digest
	}
	runes := []rune(output)
	return string(runes[:run.MaxFinalOutputLength]), digest
}

func errorJSON(code, message string) json.RawMessage {
	data, _ := json.Marshal(map[string]string{"code": code, "message": message})
	return data
}

func runAssertions(ctx context.Context, database *sql.DB, assertions []generation.Assertion) ([]evaluation.AssertionResult, bool, error) {
	results := make([]evaluation.AssertionResult, 0, len(assertions))
	allPass := true
	for _, assertion := range assertions {
		result, err := evaluation.ExecuteSQLAssertion(ctx, database, evaluation.SQLAssertion{
			ID: assertion.ID, Query: assertion.Query, Expected: assertion.Expected, Critical: assertion.Critical,
			Weight: assertion.Weight, DecimalTolerance: assertion.DecimalTolerance,
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil, false, ctx.Err()
			}
			// A query that cannot run is a failed assertion, not an infrastructure error.
			result = evaluation.AssertionResult{ID: assertion.ID, Critical: assertion.Critical, Weight: assertion.Weight, Expected: assertion.Expected}
		}
		allPass = allPass && result.Pass
		results = append(results, result)
	}
	return results, allPass, nil
}

func buildChecks(candidate generation.Candidate, outcome string, postconditions []evaluation.AssertionResult, report sandbox.MutationReport, calls []toolmatch.Call) ([]Check, error) {
	type group struct {
		weight float64
		checks []Check
	}
	var groups []group
	if len(postconditions) > 0 {
		total := 0.0
		for _, result := range postconditions {
			total += assertionWeight(result)
		}
		var checks []Check
		for _, result := range postconditions {
			checks = append(checks, Check{
				ID: "postcondition:" + result.ID, Category: CategoryState, Critical: result.Critical,
				Weight: assertionWeight(result) / total, Pass: result.Pass, Expected: result.Expected, Actual: result.Actual,
			})
		}
		groups = append(groups, group{0.4, checks})
	}
	if candidate.AgentMode == "TASK" {
		reason := ""
		if !report.Pass {
			reason = violationSummary(report)
		}
		groups = append(groups, group{0.3, []Check{{ID: checkStateDiff, Category: CategoryState, Critical: true, Weight: 1, Pass: report.Pass, Actual: report.Violations, Reason: reason}}})
	}
	if candidate.ToolEvaluationMode == generation.ToolEvaluationDeterministic && candidate.ExpectedToolCallChain != nil {
		match, err := toolmatch.Match(generation.RuntimeToolChain(*candidate.ExpectedToolCallChain), calls, nil)
		if err != nil {
			return nil, fmt.Errorf("match tool chain: %w", err)
		}
		reason := ""
		if !match.Pass {
			reason = "tool call chain does not match the expectation"
		}
		groups = append(groups, group{0.3, []Check{{ID: checkToolChain, Category: CategoryTool, Critical: match.CriticalFailure, Weight: 1, Pass: match.Pass, Expected: candidate.ExpectedToolCallChain, Actual: match, Reason: reason}}})
	}
	if len(groups) == 0 {
		return []Check{{ID: checkOutcome, Category: CategoryState, Critical: true, Weight: 1, Pass: outcome == run.OutcomeCompleted, Actual: outcome}}, nil
	}
	sum := 0.0
	for _, item := range groups {
		sum += item.weight
	}
	var checks []Check
	for _, item := range groups {
		for _, check := range item.checks {
			check.Weight = check.Weight * item.weight / sum
			checks = append(checks, check)
		}
	}
	normalizeWeights(checks)
	return checks, nil
}

// normalizeWeights absorbs floating point drift so weights sum to exactly 1 on the last check.
func normalizeWeights(checks []Check) {
	total := 0.0
	for _, check := range checks[:len(checks)-1] {
		total += check.Weight
	}
	checks[len(checks)-1].Weight = 1 - total
}

func assertionWeight(result evaluation.AssertionResult) float64 {
	if result.Weight != nil && *result.Weight > 0 {
		return *result.Weight
	}
	return 1
}

func violationSummary(report sandbox.MutationReport) string {
	parts := make([]string, 0, len(report.Violations))
	for _, violation := range report.Violations {
		parts = append(parts, violation.Code)
	}
	return "state mutation violations: " + strings.Join(parts, ",")
}

func tableSpecs(tables []validation.TableInfo) ([]sandbox.TableSpec, error) {
	specs := make([]sandbox.TableSpec, 0, len(tables))
	for _, table := range tables {
		spec := sandbox.TableSpec{Name: table.Name, PrimaryKey: table.PrimaryKey}
		for _, column := range table.Columns {
			spec.Columns = append(spec.Columns, sandbox.ColumnSpec{Name: column.Name, Type: cellType(column.Type)})
		}
		specs = append(specs, spec)
	}
	if len(specs) == 0 {
		return nil, errors.New("dataset schema declares no tables")
	}
	return specs, nil
}

func cellType(sqlType string) sandbox.CellType {
	upper := strings.ToUpper(strings.TrimSpace(sqlType))
	if upper == "BOOL" || upper == "BOOLEAN" || strings.HasPrefix(upper, "TINYINT(1)") {
		return sandbox.CellBoolean
	}
	name := upper
	if index := strings.IndexAny(name, "( "); index >= 0 {
		name = name[:index]
	}
	switch name {
	case "TINYINT", "SMALLINT", "MEDIUMINT", "INT", "INTEGER", "BIGINT":
		return sandbox.CellInteger
	case "DECIMAL", "NUMERIC", "FLOAT", "DOUBLE", "REAL":
		return sandbox.CellDecimal
	case "DATE":
		return sandbox.CellDate
	case "DATETIME", "TIMESTAMP":
		return sandbox.CellDateTime
	case "BLOB", "TINYBLOB", "MEDIUMBLOB", "LONGBLOB", "BINARY", "VARBINARY":
		return sandbox.CellBinary
	case "JSON":
		return sandbox.CellJSON
	}
	return sandbox.CellString
}
