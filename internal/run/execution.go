package run

import (
	"encoding/json"
	"time"
)

const (
	QueueEvaluation      = "evaluation"
	StageEvaluate        = "evaluate"
	StatusInvalidData    = "INVALID_TEST_DATA"
	OutcomeInfraError    = "INFRA_ERROR"
	OutcomeAgentError    = "AGENT_ERROR"
	OutcomeTimeout       = "TIMEOUT"
	OutcomeCompleted     = "COMPLETED"
	OutcomeCancelled     = "CANCELLED"
	StatusPreparing      = "PREPARING"
	StatusRunning        = "RUNNING"
	StatusEvaluating     = "EVALUATING"
	StatusCompleted      = "COMPLETED"
	StatusCancelled      = "CANCELLED"
	MaxFinalOutputLength = 16000
)

// Lease identifies a fenced work item claimed by one worker.
type Lease struct {
	WorkItemID   string
	Owner        string
	FencingToken uint64
	AttemptCount int
	MaxAttempts  int
}

// AgentSpec is the immutable Agent configuration needed to call the Target Agent.
type AgentSpec struct {
	Endpoint string
	// TokenRef names a static bearer token secret. It is used only when TokenURL is empty.
	TokenRef string
	// ClientID, ClientSecretRef and TokenURL configure OAuth 2.0 client credentials. When TokenURL
	// is set, the platform obtains a short-lived access token instead of using the static bearer.
	ClientID        string
	ClientSecretRef string
	TokenURL        string
	Model           string
}

// AgentDatabase is the database of the Agent under test. The platform uses it only to write test data; its
// credentials are never sent to the Agent. Database names the fixed database (schema) that receives the
// Run's test data; a Run no longer creates its own database.
type AgentDatabase struct {
	Host              string
	Port              int
	Database          string
	Username          string
	PasswordSecretRef string
	TLSMode           string
	CACertificatePEM  string
}

// Job is everything the execution worker needs for one Test Run.
type Job struct {
	RunID           string
	Status          string
	Prompt          string
	CancelRequested bool
	CaseJSON        json.RawMessage
	DatasetID       string
	SchemaSQL       string
	SeedSQL         string
	Agent           AgentSpec
	AgentDatabase   *AgentDatabase
}

// Settlement is the persisted result of one execution attempt.
type Settlement struct {
	Status          string
	Outcome         string
	SessionID       string
	FinalOutput     *string
	OutputSHA256    *string
	Error           json.RawMessage
	StartedAt       *time.Time
	AgentFinishedAt *time.Time
	CleanedAt       *time.Time
}

// Failure is a queue-level failure record.
type Failure struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}
