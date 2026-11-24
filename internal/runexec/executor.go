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
	"time"
	"unicode/utf8"

	"agent-test-platform/contracts"
	"agent-test-platform/internal/agent"
	"agent-test-platform/internal/evidence"
	"agent-test-platform/internal/generation"
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

// Execute injects test data into the Agent's fixed database, runs the Agent, and records the session
// for later judging. Errors it returns are infrastructure failures; Agent failures are reported
// through the Settlement.
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

	settlement, err := executor.runCase(ctx, job, candidate, job.AgentDatabase.Database, markRunning)
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

func (executor *Executor) runCase(ctx context.Context, job run.Job, candidate generation.Candidate, databaseName string, markRunning func(context.Context) error) (run.Settlement, error) {
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

	request, err := buildRequest(job, databaseName, executor.deps.Now().Add(executor.deps.AgentTimeout).UTC(), knowledgeIDs)
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

	settlement := run.Settlement{Status: run.StatusEvaluating, AgentFinishedAt: &finished}
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
	}
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

func buildRequest(job run.Job, databaseName string, deadline time.Time, knowledgeDocumentIDs []string) (agent.ChatRequest, error) {
	content := job.Prompt + "\n\nUse the database named `" + databaseName + "` for all data access."
	if knowledgeDocumentIDs == nil {
		knowledgeDocumentIDs = []string{}
	}
	return agent.ChatRequest{
		SchemaVersion: "1.0", RunID: job.RunID, Model: job.Agent.Model,
		Message:              agent.ChatMessage{ID: "msg-" + job.RunID, Role: "user", Content: content},
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
