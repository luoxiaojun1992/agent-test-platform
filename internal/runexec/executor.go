// Package runexec executes one Test Run: injects test data into the Agent's fixed database, calls the
// Target Agent, and records the session_id and final output for the later LLM Judge stage.
package runexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"

	"agent-test-platform/contracts"
	"agent-test-platform/internal/agent"
	"agent-test-platform/internal/evidence"
	"agent-test-platform/internal/generation"
	"agent-test-platform/internal/pipeline"
	"agent-test-platform/internal/run"
	"agent-test-platform/internal/sandbox"
	"agent-test-platform/internal/validation"
)

const (
	cancelPollInterval = 2 * time.Second
	cleanupTimeout     = 30 * time.Second
)

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

// Execute runs one pipeline node (init, turn, or finish). Errors it returns are infrastructure
// failures; Agent failures are reported through the Settlement.
func (executor *Executor) Execute(ctx context.Context, job run.Job, markRunning func(context.Context) error) (run.Settlement, error) {
	switch job.NodeType {
	case pipeline.NodeTypeInit:
		return executor.runInit(ctx, job, markRunning)
	case pipeline.NodeTypeTurn:
		return executor.runTurn(ctx, job, markRunning)
	case pipeline.NodeTypeFinish:
		return executor.runFinish(ctx, job)
	default:
		return run.Settlement{}, fmt.Errorf("unknown node type %q", job.NodeType)
	}
}

// runInit provisions the test environment — the fixed database and the knowledge upload — and
// returns the teardown handles (document IDs and table names) the finish node will tear down. If
// the previous iteration's teardown failed, the residual handles are still in the pipeline context,
// so init cleans them up first; when there are no handles it never wipes the database wholesale
// (it cannot locate the test data).
func (executor *Executor) runInit(ctx context.Context, job run.Job, markRunning func(context.Context) error) (run.Settlement, error) {
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
	definition, err := pipeline.Parse(executor.deps.Validator, job.PipelineJSON)
	if err != nil {
		return run.Settlement{}, err
	}
	residualTables := definition.Context.DatabaseTables
	residualKnowledge := definition.Context.KnowledgeDocumentIDs

	admin, closeAdmin, err := executor.adminConnection(ctx, job)
	if err != nil {
		return run.Settlement{}, err
	}
	defer closeAdmin()

	if job.AgentDatabase == nil || job.AgentDatabase.Database == "" {
		return run.Settlement{}, errors.New("the Agent has no fixed database configured")
	}
	quotedDB, err := sandbox.QuoteIdentifier(job.AgentDatabase.Database)
	if err != nil {
		return run.Settlement{}, fmt.Errorf("quote Agent database name: %w", err)
	}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS "+quotedDB+" CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		return run.Settlement{}, retry(fmt.Errorf("create Agent database: %w", err))
	}
	database, err := admin.OpenDatabase(ctx, job.AgentDatabase.Database)
	if err != nil {
		return run.Settlement{}, retry(fmt.Errorf("open Agent database: %w", err))
	}
	defer database.Close()

	// Clean up residual tables left by a failed previous teardown, if any handles remain.
	if len(residualTables) > 0 {
		if err := sandbox.CleanupFixedDatabase(ctx, database, residualTables); err != nil {
			return run.Settlement{}, retry(fmt.Errorf("cleanup residual tables: %w", err))
		}
	}

	tableNames := make([]string, len(schemaInfo.Tables))
	for index, table := range schemaInfo.Tables {
		tableNames[index] = table.Name
	}
	startedAt := executor.deps.Now().UTC()
	if err := sandbox.ProvisionFixedDatabase(ctx, database, tableNames, schemaInfo.Statements, seedInfo.Statements); err != nil {
		if errors.Is(err, sandbox.ErrLeftoverTestData) {
			return run.Settlement{NodeType: pipeline.NodeTypeInit, Iteration: job.Iteration, Status: run.StatusInvalidData, Error: errorJSON("LEFTOVER_TEST_DATA", err.Error())}, nil
		}
		return run.Settlement{}, retry(fmt.Errorf("provision Agent database: %w", err))
	}

	// Resolve the client once, delete any residual knowledge documents, then upload the new ones.
	token, err := executor.resolveAgentToken(ctx, job)
	if err != nil {
		return run.Settlement{}, retry(fmt.Errorf("resolve agent token: %w", err))
	}
	client, err := agent.NewSSEClient(job.Agent.Endpoint, token, executor.deps.HTTP, executor.deps.Validator)
	if err != nil {
		return run.Settlement{}, fmt.Errorf("build agent client: %w", err)
	}
	executor.deleteKnowledgeDocuments(ctx, client, residualKnowledge)
	knowledgeIDs, err := executor.uploadKnowledge(ctx, job.RunID, client, candidate.KnowledgeDocuments)
	if err != nil {
		return run.Settlement{}, retry(fmt.Errorf("provision knowledge: %w", err))
	}

	if err := markRunning(ctx); err != nil {
		return run.Settlement{}, err
	}
	finished := executor.deps.Now().UTC()
	return run.Settlement{
		NodeType: pipeline.NodeTypeInit, Iteration: job.Iteration, Status: run.StatusCompleted, Outcome: run.OutcomeCompleted,
		KnowledgeDocumentIDs: knowledgeIDs, DatabaseTables: tableNames, StartedAt: &startedAt, CleanedAt: &finished,
	}, nil
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

// runTurn executes one conversational turn, continuing the session and reading the knowledge IDs
// provisioned by init from the pipeline context.
func (executor *Executor) runTurn(ctx context.Context, job run.Job, markRunning func(context.Context) error) (run.Settlement, error) {
	if err := markRunning(ctx); err != nil {
		return run.Settlement{}, err
	}
	var candidate generation.Candidate
	if err := json.Unmarshal(job.CaseJSON, &candidate); err != nil {
		return run.Settlement{}, fmt.Errorf("decode test case content: %w", err)
	}
	if job.TurnNo < 1 || job.TurnNo > len(candidate.Turns) {
		return run.Settlement{}, fmt.Errorf("turn %d out of range (turns=%d)", job.TurnNo, len(candidate.Turns))
	}
	turn := candidate.Turns[job.TurnNo-1]

	definition, err := pipeline.Parse(executor.deps.Validator, job.PipelineJSON)
	if err != nil {
		return run.Settlement{}, err
	}
	knowledgeIDs := definition.Context.KnowledgeDocumentIDs
	if knowledgeIDs == nil {
		knowledgeIDs = []string{}
	}

	token, err := executor.resolveAgentToken(ctx, job)
	if err != nil {
		return run.Settlement{}, retry(fmt.Errorf("resolve agent token: %w", err))
	}
	client, err := agent.NewSSEClient(job.Agent.Endpoint, token, executor.deps.HTTP, executor.deps.Validator)
	if err != nil {
		return run.Settlement{}, fmt.Errorf("build agent client: %w", err)
	}

	databaseName := ""
	if job.AgentDatabase != nil {
		databaseName = job.AgentDatabase.Database
	}
	request, err := buildRequest(job, turn.Prompt, databaseName, executor.deps.Now().Add(executor.deps.AgentTimeout).UTC(), knowledgeIDs)
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

	settlement := run.Settlement{NodeType: pipeline.NodeTypeTurn, Iteration: job.Iteration, TurnNo: job.TurnNo, Status: run.StatusEvaluating, AgentFinishedAt: &finished}
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

	if chatErr == nil {
		settlement.SessionID = result.SessionID
		if len(result.Evidence) > 0 {
			if err := executor.deps.ToolCalls.SaveToolCalls(ctx, job.RunID, result.Evidence); err != nil {
				return run.Settlement{}, retry(fmt.Errorf("save tool calls: %w", err))
			}
		}
		if result.FinalOutput != nil {
			output, digest := capOutput(*result.FinalOutput)
			settlement.FinalOutput, settlement.OutputSHA256 = &output, &digest
		}
		if result.SessionID != "" {
			watermark, err := executor.contextWatermark(ctx, client, result.SessionID)
			if err != nil {
				return run.Settlement{}, retry(fmt.Errorf("read session context watermark: %w", err))
			}
			settlement.ContextWatermark = watermark
		}
	}
	return settlement, nil
}

// runFinish tears down the test environment using the teardown handles stored in the pipeline
// context (knowledge document IDs and database table names). Cleanup is best-effort; any error is
// recorded on the settlement but does not change the already-computed verdict.
func (executor *Executor) runFinish(ctx context.Context, job run.Job) (run.Settlement, error) {
	definition, err := pipeline.Parse(executor.deps.Validator, job.PipelineJSON)
	if err != nil {
		return run.Settlement{}, err
	}
	startedAt := executor.deps.Now().UTC()
	settlement := run.Settlement{NodeType: pipeline.NodeTypeFinish, Iteration: job.Iteration, Status: run.StatusCompleted, Outcome: run.OutcomeCompleted, StartedAt: &startedAt}

	cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	// Delete knowledge documents.
	if len(definition.Context.KnowledgeDocumentIDs) > 0 {
		token, err := executor.resolveAgentToken(ctx, job)
		if err != nil {
			settlement.Error = errorJSON("KNOWLEDGE_CLEANUP_FAILED", err.Error())
		} else {
			client, err := agent.NewSSEClient(job.Agent.Endpoint, token, executor.deps.HTTP, executor.deps.Validator)
			if err != nil {
				settlement.Error = errorJSON("KNOWLEDGE_CLEANUP_FAILED", err.Error())
			} else {
				executor.deleteKnowledgeDocuments(cleanupCtx, client, definition.Context.KnowledgeDocumentIDs)
			}
		}
	}

	// Drop the fixed-database tables.
	if len(definition.Context.DatabaseTables) > 0 && job.AgentDatabase != nil {
		admin, closeAdmin, err := executor.adminConnection(ctx, job)
		if err == nil {
			database, openErr := admin.OpenDatabase(ctx, job.AgentDatabase.Database)
			if openErr == nil {
				_ = sandbox.CleanupFixedDatabase(cleanupCtx, database, definition.Context.DatabaseTables)
				_ = database.Close()
			} else {
				settlement.Error = errorJSON("DATABASE_CLEANUP_FAILED", openErr.Error())
			}
			closeAdmin()
		} else {
			settlement.Error = errorJSON("DATABASE_CLEANUP_FAILED", err.Error())
		}
	}

	finished := executor.deps.Now().UTC()
	settlement.CleanedAt = &finished
	return settlement, nil
}

// contextWatermark returns the highest context sequence of a session, i.e. the boundary of the turn
// that just finished. The judge uses it as until_sequence.
func (executor *Executor) contextWatermark(ctx context.Context, client *agent.SSEClient, sessionID string) (int, error) {
	items, err := client.ListAllSessionContext(ctx, sessionID, 0, 0)
	if err != nil {
		return 0, err
	}
	if len(items) == 0 {
		return 0, nil
	}
	return items[len(items)-1].Sequence, nil
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

// uploadKnowledge uploads the Test Case's knowledge documents to the Target and waits for each to
// be READY (08 section 3). It returns the document IDs in order; the finish node deletes them using
// the IDs stored in the pipeline context.
func (executor *Executor) uploadKnowledge(ctx context.Context, runID string, client *agent.SSEClient, documents []generation.KnowledgeDocument) ([]string, error) {
	if len(documents) == 0 {
		return []string{}, nil
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
			return nil, fmt.Errorf("upload knowledge %q: %w", document.Name, err)
		}
		if _, err := client.WaitKnowledgeReady(ctx, accepted.DocumentID, document.ContentSHA256); err != nil {
			return nil, fmt.Errorf("wait knowledge %q ready: %w", document.Name, err)
		}
		ids = append(ids, accepted.DocumentID)
	}
	return ids, nil
}

// deleteKnowledgeDocuments removes knowledge documents by their IDs (best-effort, used both by
// finish teardown and by init residual cleanup).
func (executor *Executor) deleteKnowledgeDocuments(ctx context.Context, client *agent.SSEClient, ids []string) {
	if len(ids) == 0 {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, cleanupTimeout)
	defer cancel()
	for _, id := range ids {
		_ = client.DeleteKnowledge(cleanupCtx, id)
	}
}

func buildRequest(job run.Job, prompt, databaseName string, deadline time.Time, knowledgeDocumentIDs []string) (agent.ChatRequest, error) {
	content := prompt + "\n\nUse the database named `" + databaseName + "` for all data access."
	if knowledgeDocumentIDs == nil {
		knowledgeDocumentIDs = []string{}
	}
	return agent.ChatRequest{
		SchemaVersion: "1.0", RunID: job.RunID, Model: job.Agent.Model,
		SessionID:            job.SessionID,
		Message:              agent.ChatMessage{ID: "msg-" + job.RunID + "-" + strconv.Itoa(job.TurnNo), Role: "user", Content: content},
		Deadline:             deadline,
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
