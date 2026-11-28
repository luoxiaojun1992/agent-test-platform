package run

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

const (
	QueueEvaluation      = "evaluation"
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

// Turn statuses track one turn's progress through execute → judge → verdict.
const (
	TurnPending = "PENDING"
	TurnRunning = "RUNNING"
	TurnJudging = "JUDGING"
	TurnPassed  = "PASSED"
	TurnFailed  = "FAILED"
	TurnError   = "ERROR"
)

// StageKey returns the work_item stage_key for a pipeline node in a given iteration (e.g.
// "1:init", "1:turn-1", "1:judge-1", "1:finish"). The iteration prefix lets the same node id run
// again in a later iteration without colliding under uq_work_item_stage (technical/10 §7).
func StageKey(iteration int, nodeID string) string {
	return strconv.Itoa(iteration) + ":" + nodeID
}

// NodeRef identifies the pipeline node a work item executes, parsed from the work item payload.
type NodeRef struct {
	NodeID    string // "init", "turn-1", "judge-1", "finish"
	Iteration int
	TurnNo    int // 0 for init/finish
}

// NodeTurn returns the turn number encoded in a turn/judge node id, or 0 for init/finish.
func NodeTurn(nodeID string) int {
	for _, prefix := range []string{"turn-", "judge-"} {
		if strings.HasPrefix(nodeID, prefix) {
			n, err := strconv.Atoi(strings.TrimPrefix(nodeID, prefix))
			if err == nil && n >= 1 {
				return n
			}
		}
	}
	return 0
}

// NodeTypeOf returns the node type ("init"/"turn"/"judge"/"finish") encoded in a node id.
func NodeTypeOf(nodeID string) string {
	switch {
	case nodeID == "init":
		return "init"
	case nodeID == "finish":
		return "finish"
	case strings.HasPrefix(nodeID, "turn-"):
		return "turn"
	case strings.HasPrefix(nodeID, "judge-"):
		return "judge"
	}
	return ""
}

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

// Job is everything the execution worker needs to execute one node (init, turn, or finish) of a
// Test Run. NodeType/NodeID identify which node; TurnNo is set only for turn nodes.
type Job struct {
	RunID           string
	NodeID          string
	NodeType        string
	Iteration       int
	TurnNo          int
	Status          string
	CancelRequested bool
	CaseJSON        json.RawMessage
	PipelineJSON    json.RawMessage
	SessionID       string
	DatasetID       string
	SchemaSQL       string
	SeedSQL         string
	Agent           AgentSpec
	AgentDatabase   *AgentDatabase
}

// Settlement is the persisted result of one node execution attempt. KnowledgeDocumentIDs and
// DatabaseTables are the teardown handles produced by an init node (technical/10 §5).
type Settlement struct {
	NodeType             string
	Iteration            int
	TurnNo               int
	Status               string
	Outcome              string
	SessionID            string
	FinalOutput          *string
	OutputSHA256         *string
	ContextWatermark     int
	KnowledgeDocumentIDs []string
	DatabaseTables       []string
	Error                json.RawMessage
	StartedAt            *time.Time
	AgentFinishedAt      *time.Time
	CleanedAt            *time.Time
}

// Failure is a queue-level failure record.
type Failure struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}
